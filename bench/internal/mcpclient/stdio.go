package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// StdioClient runs an MCP server as a child process and speaks the
// stdio transport: newline-delimited JSON messages, one request at a
// time. Every message with an id gets a matching response.
type StdioClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	logs io.Writer // captures the server's stderr, never parsed
	mu   sync.Mutex
	// clock is injectable for tests.
	now func() time.Time
}

// NewStdioClient prepares a client for the given command. The server
// is started lazily on Initialize. Server stderr goes to logs.
func NewStdioClient(name string, args []string, logs io.Writer) *StdioClient {
	return &StdioClient{
		cmd:  exec.Command(name, args...),
		logs: logs,
		now:  time.Now,
	}
}

func (c *StdioClient) start() error {
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	c.cmd.Stderr = c.logs
	if err := c.cmd.Start(); err != nil {
		return err
	}
	c.in = stdin
	c.out = bufio.NewReader(stdout)
	return nil
}

func (c *StdioClient) request(ctx context.Context, method string, params any, id int64) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.out == nil {
		if err := c.start(); err != nil {
			return nil, fmt.Errorf("mcpclient: start stdio server: %w", err)
		}
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id > 0 {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if _, err := c.in.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	// Notifications have no response; return without reading.
	if id == 0 {
		return nil, nil
	}
	// Servers may emit notifications before the response; skip them.
	for {
		line, err := readLine(ctx, c.out)
		if err != nil {
			return nil, err
		}
		var probe struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, fmt.Errorf("mcpclient: bad server line: %w", err)
		}
		if probe.ID == nil {
			continue // notification
		}
		var num int64
		if err := json.Unmarshal(*probe.ID, &num); err != nil || num != id {
			continue // stale response
		}
		return line, nil
	}
}

func readLine(ctx context.Context, r *bufio.Reader) (json.RawMessage, error) {
	type result struct {
		line json.RawMessage
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return nil, fmt.Errorf("mcpclient: read stdio: %w", res.err)
		}
		return res.line, nil
	}
}

var nextStdioID = func() func() int64 {
	var mu sync.Mutex
	var id int64
	return func() int64 {
		mu.Lock()
		defer mu.Unlock()
		id++
		return id
	}
}()

// Initialize performs the MCP handshake and returns server metadata.
func (c *StdioClient) Initialize(ctx context.Context) (*InitResult, error) {
	raw, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "bench", "version": "0.1.0"},
	}, nextStdioID())
	if err != nil {
		return nil, err
	}
	var resp rpcResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcpclient: initialize error: %s", resp.Error.Message)
	}
	init := InitResult{}
	if err := json.Unmarshal(resp.Result, &init); err != nil {
		return nil, fmt.Errorf("mcpclient: bad initialize result: %w", err)
	}
	init.ProtocolVersion = gjsonStr(resp.Result, "protocolVersion")
	init.ServerInfo.Name = gjsonStr(resp.Result, "serverInfo.name")
	init.ServerInfo.Version = gjsonStr(resp.Result, "serverInfo.version")
	init.Instructions = gjsonStr(resp.Result, "instructions")
	if _, err := c.request(ctx, "notifications/initialized", nil, 0); err != nil {
		return nil, err
	}
	return &init, nil
}

// ListTools returns the server's tool definitions.
func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{}, nextStdioID())
	if err != nil {
		return nil, err
	}
	var resp rpcResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcpclient: tools/list error: %s", resp.Error.Message)
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool invokes a tool and waits for the result.
func (c *StdioClient) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, nextStdioID())
	if err != nil {
		return nil, err
	}
	var resp rpcResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcpclient: tools/call %s error: %s", name, resp.Error.Message)
	}
	var cr CallResult
	if err := json.Unmarshal(resp.Result, &cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

// Close stops the server process.
func (c *StdioClient) Close() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	_ = c.in.Close()
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
	return nil
}

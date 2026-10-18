package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPClient speaks the Streamable HTTP transport. It performs the
// initialize handshake, keeps the Mcp-Session-Id header, and parses
// both plain JSON and SSE-wrapped responses.
type HTTPClient struct {
	url    string
	hc     *http.Client
	mu     sync.Mutex // HTTP sessions are single-request-at-a-time
	sid    string
	closed bool
}

// NewHTTPClient creates a client for a Streamable HTTP endpoint
// (for example http://127.0.0.1:8080/mcp). token is optional.
func NewHTTPClient(url string, token string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &HTTPClient{
		url: url,
		hc:  &http.Client{Timeout: timeout},
	}
}

func (c *HTTPClient) do(ctx context.Context, body any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("mcpclient: client closed")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sid = sid
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("mcpclient: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	// Notifications carry no body.
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return parseSSE(raw), nil
	}
	return raw, nil
}

// parseSSE keeps the payload of every data: line; a single JSON
// message is returned as-is, multiple ones as a JSON array.
func parseSSE(raw []byte) json.RawMessage {
	var parts []json.RawMessage
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		parts = append(parts, json.RawMessage(data))
	}
	switch len(parts) {
	case 0:
		return nil
	case 1:
		return parts[0]
	default:
		out, _ := json.Marshal(parts)
		return out
	}
}

func (c *HTTPClient) request(ctx context.Context, method string, params any, id int64) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id > 0 {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	return c.do(ctx, msg)
}

var nextID = func() func() int64 {
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
func (c *HTTPClient) Initialize(ctx context.Context) (*InitResult, error) {
	raw, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "bench", "version": "0.1.0"},
	}, nextID())
	if err != nil {
		return nil, err
	}
	var resp rpcResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mcpclient: bad initialize response: %w", err)
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
func (c *HTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{}, nextID())
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
func (c *HTTPClient) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, nextID())
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

// Close releases the connection.
func (c *HTTPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.hc.CloseIdleConnections()
	return nil
}

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// gjsonStr digs a dotted path out of a JSON object and returns the
// string value, or "" when anything is missing.
func gjsonStr(raw json.RawMessage, path string) string {
	var cur any
	if err := json.Unmarshal(raw, &cur); err != nil {
		return ""
	}
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[key]
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

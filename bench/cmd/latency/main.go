// Command latency measures MCP tool-call round-trip times: p50 and
// p95 per tool, in both cold (fresh connection) and warm (repeated
// calls on one connection) states.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/E13Lau/vault-mcp/bench/internal/mcpclient"
	"github.com/E13Lau/vault-mcp/bench/internal/metrics"
)

type flags struct {
	outDir    string
	tool      string
	argsJSON  string
	mcpURL    string
	stdioCmd  string
	iterations int
	coldIters int
	timeout   time.Duration
}

func main() {
	var f flags
	flag.StringVar(&f.outDir, "out", "", "output directory")
	flag.StringVar(&f.tool, "tool", "", "tool name to call")
	flag.StringVar(&f.argsJSON, "args", "{}", "tool arguments as a JSON object")
	flag.StringVar(&f.mcpURL, "mcp-url", "", "Streamable HTTP endpoint URL")
	flag.StringVar(&f.stdioCmd, "stdio-cmd", "", "server command for the stdio transport")
	flag.IntVar(&f.iterations, "iterations", 20, "warm iterations per session")
	flag.IntVar(&f.coldIters, "cold", 3, "cold sessions (fresh connect each time)")
	flag.DurationVar(&f.timeout, "timeout", 120*time.Second, "per-call timeout")
	flag.Parse()
	if f.outDir == "" || f.tool == "" {
		fatal("usage: latency -tool <name> [-args '{\"k\":1}'] -out <dir> (-mcp-url <url> | -stdio-cmd <cmd>)")
	}
	if (f.mcpURL == "") == (f.stdioCmd == "") {
		fatal("exactly one of -mcp-url or -stdio-cmd is required")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(f.argsJSON), &args); err != nil {
		fatal("bad -args JSON: %v", err)
	}
	if err := run(f, args); err != nil {
		fatal("%v", err)
	}
}

type sample struct {
	Mode  string  `json:"mode"` // cold or warm
	Ms    float64 `json:"ms"`
	Error string  `json:"error,omitempty"`
}

func run(f flags, args map[string]any) error {
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		return err
	}
	var coldSamples, warmSamples []sample
	ctx := context.Background()

	// Cold: a fresh connection per sample, measuring initialize plus
	// the first tool call.
	for i := 0; i < f.coldIters; i++ {
		client, err := dial(f)
		if err != nil {
			return err
		}
		start := time.Now()
		if _, err := client.Initialize(ctx); err != nil {
			client.Close()
			coldSamples = append(coldSamples, sample{Mode: "cold", Error: err.Error()})
			continue
		}
		if _, err := client.CallTool(ctx, f.tool, args); err != nil {
			client.Close()
			coldSamples = append(coldSamples, sample{Mode: "cold", Error: err.Error()})
			continue
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		coldSamples = append(coldSamples, sample{Mode: "cold", Ms: ms})
		client.Close()
	}

	// Warm: one connection, repeated calls after the first.
	client, err := dial(f)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.Initialize(ctx); err != nil {
		return err
	}
	// The first call on the warm session is a warm-up, not a sample.
	if _, err := client.CallTool(ctx, f.tool, args); err != nil {
		return err
	}
	for i := 0; i < f.iterations; i++ {
		start := time.Now()
		if _, err := client.CallTool(ctx, f.tool, args); err != nil {
			warmSamples = append(warmSamples, sample{Mode: "warm", Error: err.Error()})
			continue
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		warmSamples = append(warmSamples, sample{Mode: "warm", Ms: ms})
	}

	out := struct {
		Tool     string                   `json:"tool"`
		Args     map[string]any           `json:"args"`
		Cold     metrics.Summary          `json:"cold_ms"`
		Warm     metrics.Summary          `json:"warm_ms"`
		Samples  []sample                 `json:"samples"`
		Generated time.Time               `json:"generated_at"`
	}{
		Tool:      f.tool,
		Args:      args,
		Cold:      metrics.Summarize(msOf(coldSamples)),
		Warm:      metrics.Summarize(msOf(warmSamples)),
		Samples:   append(coldSamples, warmSamples...),
		Generated: time.Now(),
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.outDir, "latency.json"), raw, 0o644); err != nil {
		return err
	}
	csv := "mode,ms\n"
	for _, s := range out.Samples {
		if s.Error != "" {
			continue
		}
		csv += fmt.Sprintf("%s,%.2f\n", s.Mode, s.Ms)
	}
	if err := os.WriteFile(filepath.Join(f.outDir, "latency.csv"), []byte(csv), 0o644); err != nil {
		return err
	}
	fmt.Printf("tool=%s cold n=%d p50=%.1f p95=%.1f | warm n=%d p50=%.1f p95=%.1f\n",
		f.tool, out.Cold.N, out.Cold.P50, out.Cold.P95, out.Warm.N, out.Warm.P50, out.Warm.P95)
	return nil
}

func msOf(samples []sample) []float64 {
	var out []float64
	for _, s := range samples {
		if s.Error == "" {
			out = append(out, s.Ms)
		}
	}
	return out
}

func dial(f flags) (mcpclient.Client, error) {
	if f.stdioCmd != "" {
		parts, err := shellSplit(f.stdioCmd)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty -stdio-cmd")
		}
		logs, err := os.Create(filepath.Join(f.outDir, "mcp-server.log"))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "server stderr: %s\n", filepath.Join(f.outDir, "mcp-server.log"))
		return mcpclient.NewStdioClient(parts[0], parts[1:], logs), nil
	}
	token := os.Getenv("MCP_TOKEN")
	return mcpclient.NewHTTPClient(f.mcpURL, token, f.timeout), nil
}

// shellSplit splits a command line on whitespace, honouring double
// quotes (same behaviour as the other tools).
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur []rune
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in -stdio-cmd")
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "latency: "+format+"\n", args...)
	os.Exit(1)
}

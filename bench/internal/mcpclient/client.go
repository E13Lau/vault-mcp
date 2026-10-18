// Package mcpclient is a minimal MCP client used by the bench tools.
// It supports Streamable HTTP and stdio transports, which is all the
// benchmark needs: it never runs a server itself.
package mcpclient

import (
	"context"
	"encoding/json"
)

// Tool is a subset of an MCP tool definition. InputSchema is the raw
// JSON Schema of the tool's arguments.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ContentItem is one entry of a tool result's content array.
type ContentItem struct {
	Type string `json:"type"` // "text" or "resource"
	Text string `json:"text"`
}

// CallResult is the outcome of a tools/call request.
type CallResult struct {
	IsError bool          `json:"isError"`
	Content []ContentItem `json:"content"`
}

// Text joins all text content items with newlines.
func (r *CallResult) Text() string {
	var out string
	for i, c := range r.Content {
		if c.Type != "text" {
			continue
		}
		if i > 0 {
			out += "\n"
		}
		out += c.Text
	}
	return out
}

// InitResult carries what initialize returned.
type InitResult struct {
	Instructions string
	ServerInfo   struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	ProtocolVersion string
}

// Client is the common interface over both transports.
type Client interface {
	// Initialize performs the MCP handshake and returns server
	// metadata. For stdio it also sends the initialized notification.
	Initialize(ctx context.Context) (*InitResult, error)
	// ListTools returns the server's tool definitions.
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool invokes a tool and waits for the result.
	CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error)
	// Close releases the connection and, for stdio, stops the process.
	Close() error
}

// Package openrouter is a minimal client for the OpenRouter
// OpenAI-compatible chat completions API, plus the per-call usage
// ledger the benchmark uses for its budget accounting.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Price is USD per one million tokens.
type Price struct {
	In  float64 // input (prompt) tokens
	Out float64 // output (completion) tokens
}

// DefaultPrices covers the models the benchmark plan names. Prices are
// in USD per million tokens and were fixed in the test plan.
var DefaultPrices = map[string]Price{
	"deepseek/deepseek-v4-flash": {In: 0.091, Out: 0.182},
	"z-ai/glm-5.3-flash":         {In: 0.15, Out: 0.50},
	"qwen/qwen3-embedding-8b":    {In: 0.01, Out: 0.01},
	"baai/bge-m3":                {In: 0.01, Out: 0.01},
}

// CostOf computes the USD cost of a usage entry.
func CostOf(model string, prompt, completion int64, prices map[string]Price) float64 {
	p, ok := prices[model]
	if !ok {
		p = Price{In: 0.1, Out: 0.2}
	}
	return float64(prompt)/1e6*p.In + float64(completion)/1e6*p.Out
}

// ChatMessage is one message of the conversation. ToolCallID is set on
// role "tool" messages; ToolCalls is set on role "assistant" messages
// that request tool calls.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireToolCall is the OpenAI wire format of a tool call inside a
// message (the flat ToolCall is the parsed form).
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// AssistantToolCallMessage builds the assistant message that requests
// the given tool calls.
func AssistantToolCallMessage(calls []ToolCall) ChatMessage {
	m := ChatMessage{Role: "assistant"}
	for _, c := range calls {
		var w wireToolCall
		w.ID = c.ID
		w.Type = "function"
		w.Function.Name = c.Name
		w.Function.Arguments = c.Arguments
		m.ToolCalls = append(m.ToolCalls, w)
	}
	return m
}

// ToolResultMessage builds the role "tool" message answering one tool
// call.
func ToolResultMessage(callID, content string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: callID, Content: content}
}

// ChatTool is an OpenAI function tool definition.
type ChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// Usage is the token accounting of one completion. CostUSD is the
// provider-reported cost when OpenRouter returns one (it does for
// every call); otherwise it stays zero and the caller uses the price
// table.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	CostUSD          float64
}

// UnmarshalJSON reads the standard OpenAI usage shape plus the
// OpenRouter cached-token and cost details.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var raw struct {
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
		Details          struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	u.PromptTokens = raw.PromptTokens
	u.CompletionTokens = raw.CompletionTokens
	u.CachedTokens = raw.Details.CachedTokens
	u.CostUSD = raw.Cost
	return nil
}

// Completion is the parts of a chat completion response the agent loop
// uses.
type Completion struct {
	ID        string      `json:"id"`
	Model     string      `json:"model"`
	Usage     Usage       `json:"usage"`
	Message   ChatMessage `json:"message"` // assistant message with content and tool calls
	ToolCalls []ToolCall  `json:"tool_calls"`
	Finish    string      `json:"finish_reason"`
}

// ToolCall is one function-call request from the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// Client talks to the OpenRouter chat completions endpoint.
type Client struct {
	baseURL      string
	apiKey       string
	model        string
	hc           *http.Client
	prices       map[string]Price
	reasoning    string // reasoning.effort value; "" = not sent
	temperature  float64
	seed         int64
	providerSort string // OpenRouter provider.sort, e.g. "throughput"
}

// NewClient builds a client. apiKey is the raw key string; the caller
// must source it from an environment variable, never a file.
func NewClient(model, baseURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	return &Client{
		baseURL:     baseURL,
		apiKey:      apiKey,
		model:       model,
		hc:          &http.Client{Timeout: 300 * time.Second},
		prices:      DefaultPrices,
		temperature: 0,
		seed:        42,
	}
}

// SetParams fixes temperature, seed and reasoning effort.
func (c *Client) SetParams(temperature float64, seed int64, reasoningEffort string) {
	c.temperature = temperature
	c.seed = seed
	c.reasoning = reasoningEffort
}

// SetPrices overrides the model price table.
func (c *Client) SetPrices(p map[string]Price) { c.prices = p }

// SetProviderSort selects an OpenRouter provider routing preference,
// for example "throughput". Empty means the API default.
func (c *Client) SetProviderSort(sort string) { c.providerSort = sort }

// SetTimeout sets the per-completion HTTP timeout.
func (c *Client) SetTimeout(d time.Duration) {
	if d > 0 {
		c.hc.Timeout = d
	}
}

// PriceOf returns the price entry for the configured model.
func (c *Client) PriceOf(model string) Price { return c.prices[model] }

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Tools       []ChatTool    `json:"tools,omitempty"`
	Temperature float64       `json:"temperature"`
	Seed        int64         `json:"seed"`
	Stream      bool          `json:"stream"`
	Reasoning   *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
	Provider *struct {
		Sort string `json:"sort"`
	} `json:"provider,omitempty"`
}

// Complete sends one chat completion request and returns the parsed
// response. When the API returns an error body, the error text is
// included in the returned error.
func (c *Client) Complete(ctx context.Context, messages []ChatMessage, tools []ChatTool) (*Completion, error) {
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Tools:       tools,
		Temperature: c.temperature,
		Seed:        c.seed,
		Stream:      false,
	}
	if c.reasoning != "" {
		reqBody.Reasoning = &struct {
			Effort string `json:"effort"`
		}{Effort: c.reasoning}
	}
	if c.providerSort != "" {
		reqBody.Provider = &struct {
			Sort string `json:"sort"`
		}{Sort: c.providerSort}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("openrouter: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var out struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Usage   Usage  `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("openrouter: bad response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openrouter: empty choices")
	}
	comp := &Completion{
		ID:      out.ID,
		Model:   out.Model,
		Usage:   out.Usage,
		Finish:  out.Choices[0].FinishReason,
		Message: ChatMessage{Role: out.Choices[0].Message.Role, Content: out.Choices[0].Message.Content},
	}
	for _, tc := range out.Choices[0].Message.ToolCalls {
		comp.ToolCalls = append(comp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return comp, nil
}

// Cost returns the USD cost of a completion. The provider-reported
// cost wins when present; the price table is the fallback.
func (c *Client) Cost(comp *Completion) float64 {
	if comp.Usage.CostUSD > 0 {
		return comp.Usage.CostUSD
	}
	return CostOf(comp.Model, comp.Usage.PromptTokens, comp.Usage.CompletionTokens, c.prices)
}

// KeyFromEnv reads the API key from an environment variable. The key
// value itself is never logged or written anywhere.
func KeyFromEnv(name string) (string, error) {
	if name == "" {
		name = "OPENROUTER_API_KEY"
	}
	key := os.Getenv(name)
	if key == "" {
		return "", fmt.Errorf("openrouter: %s is not set", name)
	}
	return key, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

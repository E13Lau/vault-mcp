package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/E13Lau/vault-mcp/internal/deepsearch"
	"github.com/E13Lau/vault-mcp/internal/vault"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func openVault(t *testing.T, readOnly bool) (*vault.Vault, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Rules: 保留原文。"), 0644); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(dir, nil, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, dir
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func connect(t *testing.T, ctx context.Context, transport mcp.Transport) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "vault-mcp-tests", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, ctx context.Context, s *mcp.ClientSession, name string, args any) json.RawMessage {
	t.Helper()
	r, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("%s failed: %+v", name, r.Content)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertTools(t *testing.T, ctx context.Context, s *mcp.ClientSession, readOnly bool) {
	t.Helper()
	r, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"get_vault_guide", "read_multiple_notes", "read_note", "get_frontmatter", "get_note_outline", "read_note_lines", "get_summary", "list_directory", "get_notes_info", "get_vault_stats", "resolve_wikilink", "list_all_tags", "search_notes"}
	if !readOnly {
		want = append(want, "write_note", "patch_note", "delete_note", "move_note", "move_file", "update_frontmatter", "manage_tags")
	}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools=%v want=%v", names, want)
	}
}

func TestHTTPMCP(t *testing.T) {
	v, dir := openVault(t, false)
	h := httptest.NewServer(Handler(v, "test-secret"))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	transport := func() mcp.Transport {
		return &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"test-secret"}}, DisableStandaloneSSE: true}
	}
	s := connect(t, ctx, transport())
	assertTools(t, ctx, s, false)
	if got := s.InitializeResult().Instructions; !strings.Contains(got, "Rules: 保留原文。") {
		t.Fatalf("initialize guide missing: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Updated rules: 中文。"), 0644); err != nil {
		t.Fatal(err)
	}
	fresh := connect(t, ctx, transport())
	if !strings.Contains(fresh.InitializeResult().Instructions, "Updated rules") {
		t.Fatal("new connection has stale instructions")
	}
	guide := call(t, ctx, s, "get_vault_guide", map[string]any{})
	if !bytes.Contains(guide, []byte("Updated rules")) {
		t.Fatal("guide tool stale")
	}
	resource, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "vault://guide"})
	if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != "Updated rules: 中文。" {
		t.Fatalf("resource: %v %v", resource, err)
	}
	call(t, ctx, s, "write_note", map[string]any{"path": "new/note.md", "content": "old"})
	call(t, ctx, s, "patch_note", map[string]any{"path": "new/note.md", "old_text": "old", "new_text": "新内容"})
	text := call(t, ctx, s, "read_note", map[string]any{"path": "new/note.md"})
	if !bytes.Contains(text, []byte("新内容")) {
		t.Fatal("note content mismatch")
	}
	call(t, ctx, s, "move_note", map[string]any{"source": "new/note.md", "destination": "moved.md"})
	batch := call(t, ctx, s, "read_multiple_notes", map[string]any{"paths": []string{"moved.md", "missing.md"}})
	if !bytes.Contains(batch, []byte("新内容")) || !bytes.Contains(batch, []byte("error")) {
		t.Fatal("batch must preserve successful notes alongside errors")
	}
	call(t, ctx, s, "delete_note", map[string]any{"path": "moved.md"})
	bad, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "read_note", Arguments: map[string]any{"path": "../secret.md"}})
	if err != nil || !bad.IsError {
		t.Fatalf("unsafe tool request not returned as tool error: %v %v", bad, err)
	}
	for _, paths := range []any{nil, []string{}, make([]string, 51)} {
		r, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "read_multiple_notes", Arguments: map[string]any{"paths": paths}})
		if err == nil && !r.IsError {
			t.Error("invalid batch input accepted")
		}
	}
	// Ten independently initialized clients share one vault write lock.
	sessions := make([]*mcp.ClientSession, 10)
	for i := range sessions {
		sessions[i] = connect(t, ctx, transport())
	}
	var wg sync.WaitGroup
	for i, session := range sessions {
		wg.Add(1)
		go func(i int, s *mcp.ClientSession) {
			defer wg.Done()
			chunk := fmt.Sprintf("<%d>%s</%d>\n", i, strings.Repeat("中", 1000), i)
			r, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "write_note", Arguments: map[string]any{"path": "log.md", "mode": "append", "content": chunk}})
			if err != nil {
				t.Error(err)
			} else if r.IsError {
				t.Errorf("append failed: %+v", r.Content)
			}
		}(i, session)
	}
	wg.Wait()
	log, err := v.ReadNote("log.md")
	if err != nil {
		t.Fatal(err)
	}
	for i := range sessions {
		if strings.Count(log, fmt.Sprintf("<%d>%s</%d>\n", i, strings.Repeat("中", 1000), i)) != 1 {
			t.Errorf("append %d was lost or interleaved", i)
		}
	}
}

func TestHTTPAuthAndOrigin(t *testing.T) {
	v, _ := openVault(t, false)
	h := Handler(v, "secret")
	for _, method := range []string{"POST", "GET", "DELETE"} {
		for _, auth := range []string{"", "Bearer wrong", "Basic secret", "Bearer secret ", "Bearersecret"} {
			r := httptest.NewRequest(method, "http://127.0.0.1/mcp", strings.NewReader("{}"))
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("method=%s auth=%q code=%d", method, auth, w.Code)
			}
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1/unknown", nil)
	r.Header.Set("Authorization", "bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("case-insensitive Bearer scheme rejected: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader("{}"))
	r.Header.Add("Authorization", "Bearer secret")
	r.Header.Add("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate auth accepted")
	}
	r = httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross origin request code=%d", w.Code)
	}
}

func TestReadOnlyMCP(t *testing.T) {
	v, _ := openVault(t, true)
	h := httptest.NewServer(Handler(v, ""))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	s := connect(t, ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", DisableStandaloneSSE: true})
	assertTools(t, ctx, s, true)
	for _, name := range []string{"write_note", "patch_note", "delete_note", "move_note", "move_file", "update_frontmatter", "manage_tags"} {
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil {
			t.Errorf("read-only server exposes %s", name)
		}
	}
}

func TestStdioMCP(t *testing.T) {
	_, dir := openVault(t, false)
	ctx := testContext(t)
	binary := filepath.Join(t.TempDir(), "vault-mcp")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/vault-mcp")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	for _, readOnly := range []bool{false, true} {
		cmd := exec.CommandContext(ctx, binary, "--vault", dir, "--transport=stdio", fmt.Sprintf("--read-only=%v", readOnly))
		cmd.Stderr = os.Stderr
		s := connect(t, ctx, &mcp.CommandTransport{Command: cmd})
		assertTools(t, ctx, s, readOnly)
		if !strings.Contains(s.InitializeResult().Instructions, "保留原文") {
			t.Fatal("stdio instructions missing")
		}
		call(t, ctx, s, "get_vault_guide", map[string]any{})
		if !readOnly {
			call(t, ctx, s, "write_note", map[string]any{"path": "stdio.md", "content": "stdio works"})
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The real vault remains outside the repository. This opt-in acceptance test
// only reads a disposable Brain checkout and never logs its contents.
func TestBrainGuide(t *testing.T) {
	dir := os.Getenv("VAULT_MCP_ACCEPTANCE_VAULT")
	if dir == "" {
		t.Skip("set VAULT_MCP_ACCEPTANCE_VAULT to a disposable Brain checkout")
	}
	v, err := vault.Open(dir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	full, err := v.ReadNote("CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Handler(v, ""))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	s := connect(t, ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", DisableStandaloneSSE: true})
	g, err := v.Guide()
	if err != nil {
		t.Fatal(err)
	}
	if s.InitializeResult().Instructions != g.Instructions() {
		t.Fatal("Brain initialization guide mismatch")
	}
	var out vault.Guide
	if err := json.Unmarshal(call(t, ctx, s, "get_vault_guide", map[string]any{}), &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != "CLAUDE.md" || out.Content != full {
		t.Fatal("Brain guide tool mismatch")
	}
	resource, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "vault://guide"})
	if err != nil || len(resource.Contents) != 1 || resource.Contents[0].Text != full {
		t.Fatal("Brain guide resource mismatch")
	}
	t.Logf("Brain guide verified: %d bytes; content not logged", len(full))
}

type stubScorer struct{}

func (stubScorer) Model() string { return "stub-judge" }

func (stubScorer) Score(context.Context, string, []deepsearch.Passage) ([]deepsearch.Score, deepsearch.Usage, error) {
	return nil, deepsearch.Usage{Model: "stub-judge"}, errors.New("judge offline")
}

func TestDeepSearchRegistration(t *testing.T) {
	v, _ := openVault(t, false)
	if v.DeepSearchEnabled() {
		t.Fatal("deep search must be off without a key")
	}
	v.EnableDeepSearch(stubScorer{})
	h := httptest.NewServer(Handler(v, "tok"))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	s := connect(t, ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"tok"}}, DisableStandaloneSSE: true})
	r, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if !slices.Contains(names, "deep_search") {
		t.Fatalf("tools = %v", names)
	}
	if got := s.InitializeResult().Instructions; !strings.Contains(got, "deep_search") {
		t.Fatalf("instructions do not mention deep_search: %q", got)
	}
}

func TestDeepSearchToolEndToEnd(t *testing.T) {
	v, dir := openVault(t, false)
	if err := os.WriteFile(filepath.Join(dir, "answer.md"), []byte("# 答案\n\nConnect 崩溃率是 0.6%。"), 0644); err != nil {
		t.Fatal(err)
	}
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer judge-key" {
			t.Errorf("judge auth = %q", r.Header.Get("Authorization"))
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("judge request: %v", err)
		}
		if req.Model != "stub/judge" || len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "0.6%") {
			t.Errorf("judge request model=%q messages=%d", req.Model, len(req.Messages))
		}
		io.WriteString(w, `{"model":"stub/judge","choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":0.99}]}"}}],"usage":{"prompt_tokens":500,"completion_tokens":12,"cost":0.00005}}`)
	}))
	t.Cleanup(judge.Close)
	v.EnableDeepSearch(deepsearch.NewOpenRouter(judge.URL, "stub/judge", "judge-key"))
	h := httptest.NewServer(Handler(v, "tok"))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	s := connect(t, ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"tok"}}, DisableStandaloneSSE: true})
	var out struct {
		Degraded bool `json:"degraded"`
		Usage    struct {
			Requests int     `json:"requests"`
			CostUSD  float64 `json:"cost_usd"`
		} `json:"usage"`
		Results []struct {
			Path  string  `json:"path"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(call(t, ctx, s, "deep_search", map[string]any{"query": "崩溃率", "limit": 5}), &out); err != nil {
		t.Fatal(err)
	}
	if out.Degraded || len(out.Results) != 1 || out.Results[0].Path != "answer.md" || out.Results[0].Score != 0.99 {
		t.Fatalf("deep_search output = %+v", out)
	}
	if out.Usage.Requests != 1 || out.Usage.CostUSD != 0.00005 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

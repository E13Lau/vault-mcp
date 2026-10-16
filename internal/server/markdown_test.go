package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/E13Lau/vault-mcp/internal/vault"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMarkdownMCP(t *testing.T) {
	v, _ := openVault(t, false)
	h := httptest.NewServer(Handler(v, ""))
	t.Cleanup(h.Close)
	ctx := testContext(t)
	s := connect(t, ctx, &mcp.StreamableClientTransport{Endpoint: h.URL + "/mcp", DisableStandaloneSSE: true})
	call(t, ctx, s, "write_note", map[string]any{"path": "wiki/concepts/note.md", "content": "---\ntitle:  'Original' # preserved\nsources:\n  - \"[[source]]\"\n---\n# Heading\n\nBody #inline\n"})
	call(t, ctx, s, "write_note", map[string]any{"path": "index.md", "content": "- [[note]] — indexed summary\n"})
	before, _ := v.ReadNote("wiki/concepts/note.md")
	for name, args := range map[string]any{
		"get_frontmatter":  map[string]any{"path": "wiki/concepts/note.md"},
		"get_note_outline": map[string]any{"path": "wiki/concepts/note.md"},
		"read_note_lines":  map[string]any{"path": "wiki/concepts/note.md", "start_line": 6, "end_line": 8},
		"get_summary":      map[string]any{"paths": []string{"wiki/concepts/note.md"}},
		"list_directory":   map[string]any{"path": "wiki/concepts", "limit": 1},
		"get_notes_info":   map[string]any{"paths": []string{"wiki/concepts/note.md"}},
		"get_vault_stats":  map[string]any{},
		"list_all_tags":    map[string]any{},
		"resolve_wikilink": map[string]any{"link": "[[note#Heading|Alias]]"},
	} {
		call(t, ctx, s, name, args)
	}
	call(t, ctx, s, "update_frontmatter", map[string]any{"path": "wiki/concepts/note.md", "fields": map[string]any{"summary": "metadata summary"}})
	call(t, ctx, s, "manage_tags", map[string]any{"path": "wiki/concepts/note.md", "action": "add", "tags": []string{"开发/iOS"}})
	after, _ := v.ReadNote("wiki/concepts/note.md")
	if !strings.Contains(after, "title:  'Original' # preserved\nsources:\n  - \"[[source]]\"\n") || !strings.HasSuffix(after, strings.SplitN(before, "---\n", 3)[2]) {
		t.Fatal("MCP update changed untouched frontmatter or body")
	}
	var summary struct {
		Summaries []vault.Summary `json:"summaries"`
	}
	if err := json.Unmarshal(call(t, ctx, s, "get_summary", map[string]any{"paths": []string{"wiki/concepts/note.md"}}), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Summaries) != 1 || summary.Summaries[0].Source != "frontmatter" || summary.Summaries[0].Summary != "metadata summary" {
		t.Fatalf("updated summary: %+v", summary)
	}
}

// The Brain copy stays read-only. Check every concept page that has a summary
// in the real root index, and compare frontmatter precedence independently.
func TestBrainMarkdown(t *testing.T) {
	dir := os.Getenv("VAULT_MCP_ACCEPTANCE_VAULT")
	if dir == "" {
		t.Skip("set VAULT_MCP_ACCEPTANCE_VAULT to a disposable Brain checkout")
	}
	if _, err := os.Stat(filepath.Join(dir, "wiki/concepts")); err != nil {
		t.Skip("Brain copy must include wiki/concepts")
	}
	v, err := vault.Open(dir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	ctx := context.Background()
	page, err := v.ListDirectory(ctx, vault.DirectoryOptions{Path: "wiki/concepts", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	indexed, metadata := 0, 0
	for {
		for _, row := range page.Entries {
			if row.Kind != "note" {
				continue
			}
			paths = append(paths, row.Path)
			summaries, err := v.Summaries(ctx, []string{row.Path})
			if err != nil || len(summaries) != 1 || summaries[0].Error != "" {
				t.Fatalf("concept summary error: %v", err)
			}
			got := summaries[0]
			if row.Summary != got.Summary || row.Source != got.Source || row.Title == "" {
				t.Fatal("directory and summary tools disagree")
			}
			fields, err := v.GetFrontmatter(row.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"summary", "description"} {
				if text, ok := fields[key].(string); ok && strings.TrimSpace(text) != "" {
					if got.Source != "frontmatter" || got.Summary != strings.Join(strings.Fields(text), " ") {
						t.Fatal("frontmatter precedence failed")
					}
					metadata++
					break
				}
			}
			if got.Source == "index" {
				indexed++
			}
		}
		if page.NextCursor == "" {
			break
		}
		page, err = v.ListDirectory(ctx, vault.DirectoryOptions{Path: "wiki/concepts", Limit: 3, Cursor: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(paths) == 0 || indexed == 0 {
		t.Fatal("Brain concepts did not produce any index-sourced summaries")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Fatal("duplicate paginated entry")
		}
		seen[p] = true
	}
	allPaths, err := v.NotePaths()
	if err != nil {
		t.Fatal(err)
	}
	expected := 0
	for _, p := range allPaths {
		if filepath.ToSlash(filepath.Dir(p)) == "wiki/concepts" {
			expected++
		}
	}
	if expected != len(paths) {
		t.Fatalf("pagination missed notes: got %d want %d", len(paths), expected)
	}
	// Add a summary only in an isolated temporary copy of a real Brain note.
	// The source checkout remains unchanged and no text enters the repository.
	noteText, err := v.ReadNote(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	indexText, err := v.ReadNote("index.md")
	if err != nil {
		t.Fatal(err)
	}
	copyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(copyDir, filepath.Dir(paths[0])), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, paths[0]), []byte(noteText), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "index.md"), []byte(indexText), 0600); err != nil {
		t.Fatal(err)
	}
	copyVault, err := vault.Open(copyDir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { copyVault.Close() })
	marker := "acceptance-only frontmatter priority marker"
	if err := copyVault.UpdateFrontmatter(ctx, paths[0], map[string]any{"summary": marker}, nil); err != nil {
		t.Fatal(err)
	}
	priority, err := copyVault.Summaries(ctx, []string{paths[0]})
	if err != nil || priority[0].Summary != marker || priority[0].Source != "frontmatter" {
		t.Fatal("real Brain note frontmatter priority failed")
	}
	t.Logf("Brain concepts verified: %d notes, %d index summaries, %d existing frontmatter summaries; temporary Brain note summary override passed; no contents logged", len(paths), indexed, metadata)
}

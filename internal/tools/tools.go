// Package tools adapts typed MCP calls to the shared vault instance.
package tools

import (
	"context"
	"errors"

	"github.com/E13Lau/vault-mcp/internal/vault"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type PathInput struct {
	Path string `json:"path" jsonschema:"Vault-relative Markdown path"`
}
type Note struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}
type BatchInput struct {
	Paths []string `json:"paths" jsonschema:"One to 50 vault-relative Markdown paths"`
}
type BatchOutput struct {
	Notes []Note `json:"notes"`
}
type Empty struct{}
type Status struct {
	OK bool `json:"ok"`
}
type WriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty" jsonschema:"overwrite (default), append, or prepend; no separator is inserted"`
}
type PatchInput struct {
	Path    string `json:"path"`
	OldText string `json:"old_text" jsonschema:"Nonempty text that must occur exactly once"`
	NewText string `json:"new_text"`
}
type MoveInput struct {
	Source      string `json:"source"`
	Destination string `json:"destination" jsonschema:"New vault-relative path; existing destinations are rejected"`
}
type MoveOutput struct {
	OK         bool     `json:"ok"`
	References []string `json:"references" jsonschema:"Files with wikilinks that may refer to the old note path; ambiguous matches included"`
}

func definition(name, description string, readOnly bool) *mcp.Tool {
	closed := false
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, OpenWorldHint: &closed}}
}

// These fields always carry arrays, never JSON null. The default Go inference
// permits null slices, which some MCP clients cannot map to their tool dialect.
func arraySchema[T any](field string) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	} // The types below are fixed at compile time.
	s.Properties[field].Types = nil
	s.Properties[field].Type = "array"
	return s
}

func Register(s *mcp.Server, v *vault.Vault) {
	mcp.AddTool(s, definition("read_note", "Read a UTF-8 Markdown note (L2).", true),
		func(ctx context.Context, _ *mcp.CallToolRequest, in PathInput) (*mcp.CallToolResult, Note, error) {
			text, err := v.ReadNote(in.Path)
			return nil, Note{Path: in.Path, Content: text}, err
		})
	batch := definition("read_multiple_notes", "Read up to 50 notes; individual failures are returned with each path.", true)
	batchInput := arraySchema[BatchInput]("paths")
	minimum, maximum := 1, 50
	batchInput.Properties["paths"].MinItems = &minimum
	batchInput.Properties["paths"].MaxItems = &maximum
	batch.InputSchema = batchInput
	batch.OutputSchema = arraySchema[BatchOutput]("notes")
	mcp.AddTool(s, batch,
		func(ctx context.Context, _ *mcp.CallToolRequest, in BatchInput) (*mcp.CallToolResult, BatchOutput, error) {
			if len(in.Paths) == 0 || len(in.Paths) > 50 {
				return nil, BatchOutput{}, errors.New("provide between 1 and 50 paths")
			}
			out := BatchOutput{Notes: make([]Note, 0, len(in.Paths))}
			for _, name := range in.Paths {
				if err := ctx.Err(); err != nil {
					return nil, BatchOutput{}, err
				}
				text, err := v.ReadNote(name)
				note := Note{Path: name, Content: text}
				if err != nil {
					note.Error = err.Error()
				}
				out.Notes = append(out.Notes, note)
			}
			return nil, out, nil
		})
	mcp.AddTool(s, definition("get_vault_guide", "Read the current full vault guide: CLAUDE.md, then AGENTS.md, then README.md.", true),
		func(context.Context, *mcp.CallToolRequest, Empty) (*mcp.CallToolResult, vault.Guide, error) {
			g, err := v.Guide()
			return nil, g, err
		})
	registerMarkdown(s, v)
	if v.DeepSearchEnabled() {
		mcp.AddTool(s, definition("deep_search", "Slow, paid second pass: rank BM25 candidates with a model judge and return the most relevant pages. Use only when search_notes finds nothing useful or the question uses different words.", true),
			func(ctx context.Context, _ *mcp.CallToolRequest, in vault.DeepSearchOptions) (*mcp.CallToolResult, vault.DeepSearchOutput, error) {
				out, err := v.DeepSearch(ctx, in)
				return nil, out, err
			})
	}
	if v.ReadOnly() {
		return
	}
	mcp.AddTool(s, definition("write_note", "Create or write a note atomically. Modes: overwrite, append, prepend. Missing parents are created.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in WriteInput) (*mcp.CallToolResult, Status, error) {
			err := v.WriteNote(ctx, in.Path, in.Content, in.Mode)
			return nil, Status{OK: err == nil}, err
		})
	mcp.AddTool(s, definition("patch_note", "Replace one unique text fragment atomically; absent or ambiguous matches fail without writing.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in PatchInput) (*mcp.CallToolResult, Status, error) {
			err := v.PatchNote(ctx, in.Path, in.OldText, in.NewText)
			return nil, Status{OK: err == nil}, err
		})
	mcp.AddTool(s, definition("delete_note", "Permanently delete one Markdown note. Directories are rejected.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in PathInput) (*mcp.CallToolResult, Status, error) {
			err := v.DeleteNote(ctx, in.Path)
			return nil, Status{OK: err == nil}, err
		})
	move := definition("move_note", "Move a Markdown note and return conservative wikilink references. Referring files are not changed.", false)
	move.OutputSchema = arraySchema[MoveOutput]("references")
	mcp.AddTool(s, move,
		func(ctx context.Context, _ *mcp.CallToolRequest, in MoveInput) (*mcp.CallToolResult, MoveOutput, error) {
			refs, err := v.MoveNote(ctx, in.Source, in.Destination)
			return nil, MoveOutput{OK: err == nil, References: refs}, err
		})
	mcp.AddTool(s, definition("move_file", "Move one file, including binary attachments. Directories and existing destinations are rejected.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in MoveInput) (*mcp.CallToolResult, Status, error) {
			err := v.MoveFile(ctx, in.Source, in.Destination)
			return nil, Status{OK: err == nil}, err
		})
}

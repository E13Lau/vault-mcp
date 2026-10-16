package tools

import (
	"context"

	"github.com/E13Lau/vault-mcp/internal/markdown"
	"github.com/E13Lau/vault-mcp/internal/vault"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type OutlineOutput struct {
	Headings []markdown.Heading `json:"headings"`
}
type LinesInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}
type FrontmatterOutput struct {
	Frontmatter map[string]any `json:"frontmatter"`
}
type FrontmatterInput struct {
	Path   string         `json:"path"`
	Fields map[string]any `json:"fields,omitempty" jsonschema:"Top-level fields to add or replace; null is a YAML null value"`
	Remove []string       `json:"remove,omitempty" jsonschema:"Top-level field names to delete"`
}
type SummaryOutput struct {
	Summaries []vault.Summary `json:"summaries"`
}
type InfoOutput struct {
	Notes []vault.NoteInfo `json:"notes"`
}
type ResolveInput struct {
	Link       string `json:"link"`
	SourcePath string `json:"source_path,omitempty" jsonschema:"Source note for explicit relative links or same-note headings"`
}
type TagsOutput struct {
	Tags []vault.TagInfo `json:"tags"`
}
type ManageTagsInput struct {
	Path   string   `json:"path"`
	Action string   `json:"action"`
	Tags   []string `json:"tags"`
}
type ManagedTagsOutput struct {
	Tags []string `json:"tags"`
}

// Tool arrays are always non-null; optional pointers/maps are omitted when
// absent. Arbitrary frontmatter values keep their unconstrained JSON schema.
func toolSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	var normalize func(*jsonschema.Schema)
	normalize = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		if len(s.Types) == 2 {
			for _, t := range s.Types {
				if t != "null" {
					s.Type = t
				}
			}
			s.Types = nil
		}
		for _, p := range s.Properties {
			normalize(p)
		}
		for _, p := range s.Defs {
			normalize(p)
		}
		normalize(s.Items)
		normalize(s.AdditionalProperties)
		for _, p := range s.AnyOf {
			normalize(p)
		}
	}
	normalize(s)
	return s
}

func add[In, Out any](s *mcp.Server, name, description string, readOnly bool, h mcp.ToolHandlerFor[In, Out]) {
	t := definition(name, description, readOnly)
	t.InputSchema = toolSchema[In]()
	t.OutputSchema = toolSchema[Out]()
	mcp.AddTool(s, t, h)
}

func batchDefinition[Out any](name, description string) *mcp.Tool {
	t := definition(name, description, true)
	in := toolSchema[BatchInput]()
	minimum, maximum := 1, 50
	in.Properties["paths"].MinItems = &minimum
	in.Properties["paths"].MaxItems = &maximum
	t.InputSchema = in
	t.OutputSchema = toolSchema[Out]()
	return t
}

func registerMarkdown(s *mcp.Server, v *vault.Vault) {
	add(s, "get_note_outline", "Read L1 headings with original 1-based line numbers; code blocks are ignored.", true,
		func(_ context.Context, _ *mcp.CallToolRequest, in PathInput) (*mcp.CallToolResult, OutlineOutput, error) {
			headings, err := v.Outline(in.Path)
			return nil, OutlineOutput{headings}, err
		})
	add(s, "read_note_lines", "Read an inclusive 1-based line range (at most 2000 lines); end_line is clamped at EOF. Original line endings are retained.", true,
		func(_ context.Context, _ *mcp.CallToolRequest, in LinesInput) (*mcp.CallToolResult, vault.NoteLines, error) {
			out, err := v.ReadLines(in.Path, in.StartLine, in.EndLine)
			return nil, out, err
		})
	add(s, "get_frontmatter", "Read YAML frontmatter. Invalid YAML is an error.", true,
		func(_ context.Context, _ *mcp.CallToolRequest, in PathInput) (*mcp.CallToolResult, FrontmatterOutput, error) {
			fields, err := v.GetFrontmatter(in.Path)
			return nil, FrontmatterOutput{fields}, err
		})
	mcp.AddTool(s, batchDefinition[SummaryOutput]("get_summary", "Read L0 summaries for 1–50 notes: frontmatter summary/description, configured index link suffix, then first body paragraph (120 Unicode characters)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in BatchInput) (*mcp.CallToolResult, SummaryOutput, error) {
			out, err := v.Summaries(ctx, in.Paths)
			return nil, SummaryOutput{out}, err
		})
	add(s, "list_directory", "Browse Markdown notes and directories with metadata and optional L0 summaries. Defaults: path root, depth 1, include_summary true, limit 100. Stale cursors require restarting pagination.", true,
		func(ctx context.Context, _ *mcp.CallToolRequest, in vault.DirectoryOptions) (*mcp.CallToolResult, vault.DirectoryPage, error) {
			out, err := v.ListDirectory(ctx, in)
			return nil, out, err
		})
	mcp.AddTool(s, batchDefinition[InfoOutput]("get_notes_info", "Read note metadata and indexed backlinks for 1–50 paths; individual failures are returned per note."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in BatchInput) (*mcp.CallToolResult, InfoOutput, error) {
			out, err := v.NotesInfo(ctx, in.Paths)
			return nil, InfoOutput{out}, err
		})
	add(s, "get_vault_stats", "Count accessible notes, directories, attachments, bytes, tags and wikilinks. Report files skipped by the text index.", true,
		func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, vault.Stats, error) {
			out, err := v.Stats(ctx)
			return nil, out, err
		})
	add(s, "resolve_wikilink", "Resolve a wikilink or shortest note path; return all ambiguous candidates. Supports aliases, headings, embeds and explicit relative paths.", true,
		func(ctx context.Context, _ *mcp.CallToolRequest, in ResolveInput) (*mcp.CallToolResult, vault.Resolution, error) {
			out, err := v.Resolve(ctx, in.Link, in.SourcePath)
			return nil, out, err
		})
	add(s, "list_all_tags", "List frontmatter and inline tags, including Chinese and nested tags, with note counts and paths. Code is ignored.", true,
		func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, TagsOutput, error) {
			out, err := v.AllTags(ctx)
			return nil, TagsOutput{out}, err
		})
	add(s, "search_notes", "Keyword search ranked by BM25. All space-separated keywords must match; Chinese is matched by characters and adjacent pairs, so no spaces are needed. Title, path and tag hits and whole-keyword matches rank higher. Filter by path_prefix, tag, or frontmatter field.", true,
		func(ctx context.Context, _ *mcp.CallToolRequest, in vault.SearchOptions) (*mcp.CallToolResult, vault.SearchOutput, error) {
			out, err := v.Search(ctx, in)
			return nil, out, err
		})
	if v.ReadOnly() {
		return
	}
	add(s, "update_frontmatter", "Add, replace, or remove top-level YAML fields atomically. Untouched fields and the body retain their source bytes. Invalid YAML is never overwritten.", false,
		func(ctx context.Context, _ *mcp.CallToolRequest, in FrontmatterInput) (*mcp.CallToolResult, Status, error) {
			err := v.UpdateFrontmatter(ctx, in.Path, in.Fields, in.Remove)
			return nil, Status{err == nil}, err
		})
	add(s, "manage_tags", "Add, remove, or set frontmatter tags atomically. Inline tags are not edited; list_all_tags includes them. Use action set with [] to clear frontmatter tags.", false,
		func(ctx context.Context, _ *mcp.CallToolRequest, in ManageTagsInput) (*mcp.CallToolResult, ManagedTagsOutput, error) {
			out, err := v.ManageTags(ctx, in.Path, in.Action, in.Tags)
			return nil, ManagedTagsOutput{out}, err
		})
}

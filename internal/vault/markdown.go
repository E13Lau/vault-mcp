package vault

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/E13Lau/vault-mcp/internal/index"
	"github.com/E13Lau/vault-mcp/internal/markdown"
)

// SetIndexFile is startup configuration; call it before serving any requests.
func (v *Vault) SetIndexFile(name string) error {
	if name == "" {
		name = "index.md"
	}
	n, err := v.note(name)
	if err != nil {
		return err
	}
	if stat, err := v.root.Stat(n); err == nil {
		if !stat.Mode().IsRegular() {
			return errors.New("index-file must be a regular Markdown file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return safeError("stat index-file", err)
	}
	v.indexFile = n
	return nil
}

func (v *Vault) Refresh(ctx context.Context) error { _, err := v.catalog.Snapshot(ctx); return err }

func (v *Vault) StatNote(name string) (fs.FileInfo, error) {
	n, err := v.note(name)
	if err != nil {
		return nil, err
	}
	info, err := v.root.Stat(n)
	if err != nil {
		return nil, safeError("stat note", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return info, nil
}

func (v *Vault) GetFrontmatter(name string) (map[string]any, error) {
	text, err := v.ReadNote(name)
	if err != nil {
		return nil, err
	}
	doc, err := markdown.Parse(text)
	if err != nil {
		return nil, err
	}
	return doc.Fields, nil
}

func (v *Vault) UpdateFrontmatter(ctx context.Context, name string, fields map[string]any, remove []string) error {
	return v.mutate(ctx, func() error {
		n, err := v.note(name)
		if err != nil {
			return err
		}
		text, err := v.read(n)
		if err != nil {
			return err
		}
		updated, err := markdown.Update(text, fields, remove)
		if err != nil {
			return err
		}
		if updated == text {
			return nil
		}
		return v.atomicWrite(n, updated)
	})
}

func (v *Vault) Outline(name string) ([]markdown.Heading, error) {
	text, err := v.ReadNote(name)
	if err != nil {
		return nil, err
	}
	doc, err := markdown.Parse(text)
	if err != nil {
		return nil, err
	}
	return markdown.Outline(doc), nil
}

type NoteLines struct {
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
	Content    string `json:"content"`
}

func (v *Vault) ReadLines(name string, start, end int) (NoteLines, error) {
	out := NoteLines{Path: name}
	if start < 1 || end < start {
		return out, errors.New("line range must be inclusive, 1-based, and end_line >= start_line")
	}
	if end-start >= 2000 {
		return out, errors.New("read at most 2000 lines per call")
	}
	text, err := v.ReadNote(name)
	if err != nil {
		return out, err
	}
	ls := strings.SplitAfter(text, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	out.TotalLines = len(ls)
	if len(ls) == 0 {
		return out, nil
	}
	if start > len(ls) {
		return out, errors.New("start_line exceeds the note's line count")
	}
	if end > len(ls) {
		end = len(ls)
	}
	out.StartLine, out.EndLine = start, end
	out.Content = strings.Join(ls[start-1:end], "")
	return out, nil
}

type Summary struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
	Source  string `json:"source,omitempty"`
	Error   string `json:"error,omitempty"`
}

func summary(e *index.Entry, fromIndex map[string]string) Summary {
	out := Summary{Path: e.Path}
	if e.MetadataError != "" {
		out.Error = e.MetadataError
		return out
	}
	for _, key := range []string{"summary", "description"} {
		if text, ok := e.Document.Fields[key].(string); ok && strings.TrimSpace(text) != "" {
			out.Summary = strings.Join(strings.Fields(text), " ")
			out.Source = "frontmatter"
			return out
		}
	}
	if text := fromIndex[e.Path]; text != "" {
		out.Summary = text
		out.Source = "index"
		return out
	}
	out.Summary, out.Source = e.BodySummary, "body"
	return out
}

func checkBatch(paths []string) error {
	if len(paths) < 1 || len(paths) > 50 {
		return errors.New("provide between 1 and 50 paths")
	}
	return nil
}

func (v *Vault) indexed(name string, s *index.Snapshot) (*index.Entry, error) {
	n, err := v.note(name)
	if err != nil {
		return nil, err
	}
	if e := s.ByPath[n]; e != nil {
		return e, nil
	}
	if _, err := v.ReadNote(n); err != nil {
		return nil, err
	}
	return nil, errors.New("note changed during indexing; retry the request")
}

func (v *Vault) Summaries(ctx context.Context, paths []string) ([]Summary, error) {
	if err := checkBatch(paths); err != nil {
		return nil, err
	}
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	fromIndex := s.Summaries(v.indexFile)
	out := make([]Summary, 0, len(paths))
	for _, p := range paths {
		e, err := v.indexed(p, s)
		if err != nil {
			out = append(out, Summary{Path: p, Error: err.Error()})
			continue
		}
		out = append(out, summary(e, fromIndex))
	}
	return out, nil
}

type NoteInfo struct {
	Path        string         `json:"path"`
	Title       string         `json:"title,omitempty"`
	Type        string         `json:"type,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Bytes       int64          `json:"bytes"`
	Modified    string         `json:"modified,omitempty"`
	Lines       int            `json:"lines"`
	Links       int            `json:"links"`
	Backlinks   []string       `json:"backlinks"`
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Error       string         `json:"error,omitempty"`
}

func info(e *index.Entry, s *index.Snapshot) NoteInfo {
	lineCount := strings.Count(e.Document.Body, "\n")
	if e.Document.Body != "" && !strings.HasSuffix(e.Document.Body, "\n") {
		lineCount++
	}
	lineCount += e.Document.BodyLine - 1
	return NoteInfo{Path: e.Path, Title: e.Title, Type: e.Type, Tags: e.Tags, Bytes: e.Bytes, Modified: e.Modified.UTC().Format(time.RFC3339Nano), Lines: lineCount, Links: len(e.Links), Backlinks: append([]string{}, s.Backlinks[e.Path]...), Frontmatter: e.Document.Fields, Error: e.MetadataError}
}

func (v *Vault) NotesInfo(ctx context.Context, paths []string) ([]NoteInfo, error) {
	if err := checkBatch(paths); err != nil {
		return nil, err
	}
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NoteInfo, 0, len(paths))
	for _, p := range paths {
		e, err := v.indexed(p, s)
		if err != nil {
			out = append(out, NoteInfo{Path: p, Error: err.Error(), Backlinks: []string{}})
			continue
		}
		out = append(out, info(e, s))
	}
	return out, nil
}

type Resolution struct {
	Target     string     `json:"target"`
	Heading    string     `json:"heading,omitempty"`
	Alias      string     `json:"alias,omitempty"`
	Embedded   bool       `json:"embedded"`
	Candidates []NoteInfo `json:"candidates"`
}

func (v *Vault) Resolve(ctx context.Context, input, source string) (Resolution, error) {
	l, err := markdown.ParseLink(input)
	if err != nil {
		return Resolution{}, err
	}
	if source != "" {
		n, err := v.note(source)
		if err != nil {
			return Resolution{}, err
		}
		source = n
	}
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return Resolution{}, err
	}
	out := Resolution{Target: l.Target, Heading: l.Heading, Alias: l.Alias, Embedded: l.Embedded, Candidates: []NoteInfo{}}
	for _, p := range s.Resolver.Resolve(l, source) {
		if e := s.ByPath[p]; e != nil {
			out.Candidates = append(out.Candidates, info(e, s))
		} else {
			_, err := v.indexed(p, s)
			message := "note could not be indexed"
			if err != nil {
				message = err.Error()
			}
			out.Candidates = append(out.Candidates, NoteInfo{Path: p, Backlinks: append([]string{}, s.Backlinks[p]...), Error: message})
		}
	}
	return out, nil
}

type TagInfo struct {
	Tag   string   `json:"tag"`
	Count int      `json:"count"`
	Paths []string `json:"paths"`
}

func (v *Vault) AllTags(ctx context.Context) ([]TagInfo, error) {
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	paths := map[string][]string{}
	for _, e := range s.Entries {
		for _, tag := range e.Tags {
			paths[tag] = append(paths[tag], e.Path)
		}
	}
	out := []TagInfo{}
	for tag, notes := range paths {
		out = append(out, TagInfo{tag, len(notes), notes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out, nil
}

// ManageTags edits only the frontmatter tags field. Inline tags remain source
// text and are still included in read-only aggregate tag queries.
func (v *Vault) ManageTags(ctx context.Context, name, action string, tags []string) ([]string, error) {
	out := []string{}
	err := v.mutate(ctx, func() error {
		n, err := v.note(name)
		if err != nil {
			return err
		}
		text, err := v.read(n)
		if err != nil {
			return err
		}
		doc, err := markdown.Parse(text)
		if err != nil {
			return err
		}
		set := map[string]bool{}
		for _, t := range markdown.FrontmatterTags(doc.Fields) {
			set[t] = true
		}
		if action == "set" {
			set = map[string]bool{}
		}
		if action != "add" && action != "remove" && action != "set" {
			return errors.New("action must be add, remove, or set")
		}
		for _, t := range tags {
			tag, err := markdown.NormalizeTag(t)
			if err != nil {
				return err
			}
			if action == "remove" {
				delete(set, tag)
			} else {
				set[tag] = true
			}
		}
		for tag := range set {
			out = append(out, tag)
		}
		out = markdown.UniqueTags(out)
		updated, err := markdown.Update(text, map[string]any{"tags": out}, nil)
		if err != nil {
			return err
		}
		if updated == text {
			return nil
		}
		return v.atomicWrite(n, updated)
	})
	return out, err
}

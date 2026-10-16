package vault

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/E13Lau/vault-mcp/internal/markdown"
)

type DirectoryOptions struct {
	Path           string `json:"path,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	IncludeSummary *bool  `json:"include_summary,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}

type DirectoryEntry struct {
	Path      string   `json:"path"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	Type      string   `json:"type,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Source    string   `json:"source,omitempty"`
	NoteCount *int     `json:"note_count,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type DirectoryPage struct {
	Entries    []DirectoryEntry `json:"entries"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Total      int              `json:"total"`
}

func (v *Vault) walk(start string, visit func(string, fs.DirEntry) error) error {
	return fs.WalkDir(v.root.FS(), start, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return safeError("walk vault", err)
		}
		if name != "." {
			if _, err := v.check(name, false); err != nil {
				if errors.Is(err, ErrDenied) {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				return err
			}
		}
		return visit(name, d)
	})
}

func (v *Vault) ListDirectory(ctx context.Context, opts DirectoryOptions) (DirectoryPage, error) {
	out := DirectoryPage{Entries: []DirectoryEntry{}}
	name, err := v.check(opts.Path, true)
	if err != nil {
		return out, err
	}
	stat, err := v.root.Stat(name)
	if err != nil {
		return out, safeError("stat directory", err)
	}
	if !stat.IsDir() {
		return out, errors.New("path is not a directory")
	}
	if opts.Depth == 0 {
		opts.Depth = 1
	}
	if opts.Depth < 1 || opts.Depth > 10 {
		return out, errors.New("depth must be between 1 and 10")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	if opts.Limit < 1 || opts.Limit > 500 {
		return out, errors.New("limit must be between 1 and 500")
	}
	includeSummary := opts.IncludeSummary == nil || *opts.IncludeSummary
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return out, err
	}
	fromIndex := s.Summaries(v.indexFile)
	counts := map[string]int{}
	counted := map[string]bool{}
	for _, p := range append(append([]string{}, s.Paths...), s.Skipped...) {
		if counted[p] {
			continue
		}
		counted[p] = true
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			counts[dir]++
		}
	}
	rows := []DirectoryEntry{}
	err = v.walk(name, func(p string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == name {
			return nil
		}
		rel := p
		if name != "." {
			rel = strings.TrimPrefix(p, name+"/")
		}
		depth := strings.Count(rel, "/") + 1
		if d.IsDir() {
			count := counts[p]
			rows = append(rows, DirectoryEntry{Path: p, Kind: "directory", Title: path.Base(p), NoteCount: &count})
			if depth >= opts.Depth {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(path.Ext(p), ".md") || !d.Type().IsRegular() {
			return nil
		}
		row := DirectoryEntry{Path: p, Kind: "note", Title: markdown.NoteTarget(path.Base(p))}
		if e := s.ByPath[p]; e != nil {
			row.Title, row.Type, row.Tags, row.Error = e.Title, e.Type, e.Tags, e.MetadataError
			if includeSummary {
				summary := summary(e, fromIndex)
				row.Summary, row.Source, row.Error = summary.Summary, summary.Source, summary.Error
			}
		} else {
			row.Error = "note could not be indexed; it may be unreadable, non-UTF-8, or over 16 MiB"
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return out, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	identity := struct {
		Path       string
		Depth      int
		Summary    bool
		Generation string
		Paths      []string
	}{name, opts.Depth, includeSummary, s.Generation, []string{}}
	for _, row := range rows {
		identity.Paths = append(identity.Paths, row.Path)
	}
	b, _ := json.Marshal(identity)
	stamp := fmt.Sprintf("%x", sha256.Sum256(b))
	type cursor struct {
		Stamp  string `json:"stamp"`
		Offset int    `json:"offset"`
	}
	offset := 0
	if opts.Cursor != "" {
		var cur cursor
		decoded, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if len(opts.Cursor) > 2048 || err != nil || json.Unmarshal(decoded, &cur) != nil || cur.Stamp != stamp || cur.Offset < 0 || cur.Offset > len(rows) {
			return out, errors.New("invalid or stale cursor; restart directory pagination")
		}
		offset = cur.Offset
	}
	end := min(offset+opts.Limit, len(rows))
	out.Entries = append(out.Entries, rows[offset:end]...)
	out.Total = len(rows)
	if end < len(rows) {
		encoded, _ := json.Marshal(cursor{stamp, end})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return out, nil
}

type Stats struct {
	Notes       int      `json:"notes"`
	Directories int      `json:"directories"`
	Attachments int      `json:"attachments"`
	Bytes       int64    `json:"bytes"`
	Tags        int      `json:"tags"`
	Wikilinks   int      `json:"wikilinks"`
	Skipped     []string `json:"skipped"`
}

func (v *Vault) Stats(ctx context.Context) (Stats, error) {
	out := Stats{Skipped: []string{}}
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return out, err
	}
	tags := map[string]bool{}
	for _, e := range s.Entries {
		out.Wikilinks += len(e.Links)
		for _, tag := range e.Tags {
			tags[tag] = true
		}
	}
	out.Tags = len(tags)
	out.Skipped = append(out.Skipped, s.Skipped...)
	err = v.walk(".", func(p string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			out.Directories++
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return safeError("stat file", err)
		}
		out.Bytes += info.Size()
		if strings.EqualFold(path.Ext(p), ".md") {
			out.Notes++
		} else {
			out.Attachments++
		}
		return nil
	})
	return out, err
}

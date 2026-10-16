package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/E13Lau/vault-mcp/internal/index"
	"github.com/E13Lau/vault-mcp/internal/markdown"
	"github.com/E13Lau/vault-mcp/internal/search"
)

type SearchOptions struct {
	Query       string         `json:"query" jsonschema:"Keywords separated by spaces; every keyword must match. Chinese needs no spaces."`
	PathPrefix  string         `json:"path_prefix,omitempty" jsonschema:"Only notes under this vault-relative directory or path prefix"`
	Tag         string         `json:"tag,omitempty" jsonschema:"Only notes with this tag or a nested child tag"`
	Frontmatter map[string]any `json:"frontmatter,omitempty" jsonschema:"Only notes whose frontmatter field equals the value, or whose list field contains it"`
	Limit       int            `json:"limit,omitempty" jsonschema:"Maximum results, 1 to 100; default 20"`
}

type SearchResult struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Score   float64  `json:"score"`
	Type    string   `json:"type,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Snippet string   `json:"snippet"`
}

type SearchOutput struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total" jsonschema:"Matching notes before the limit"`
}

func (v *Vault) searchDocFor(e *index.Entry) *search.Doc {
	if d, ok := v.searchDoc[e]; ok {
		return d
	}
	d := search.NewDoc(e.Title, e.Path, e.Tags, e.Document.Body)
	v.searchDoc[e] = d
	return d
}

func fieldMatches(have, want any) bool {
	if list, ok := have.([]any); ok {
		for _, item := range list {
			if fieldMatches(item, want) {
				return true
			}
		}
		return false
	}
	return fmt.Sprint(have) == fmt.Sprint(want)
}

// Search ranks notes by BM25. Term statistics are computed from the current
// catalog snapshot, so changed notes are reindexed lazily on the next query.
func (v *Vault) Search(ctx context.Context, o SearchOptions) (SearchOutput, error) {
	q := search.ParseQuery(o.Query)
	if q.Empty() {
		return SearchOutput{}, errors.New("query must contain at least one keyword")
	}
	limit := o.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return SearchOutput{}, errors.New("limit must be between 1 and 100")
	}
	tag := ""
	if o.Tag != "" {
		var err error
		if tag, err = markdown.NormalizeTag(o.Tag); err != nil {
			return SearchOutput{}, err
		}
	}
	prefix := strings.TrimPrefix(o.PathPrefix, "./")
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return SearchOutput{}, err
	}
	v.searchMu.Lock()
	defer v.searchMu.Unlock()
	if v.searchDoc == nil {
		v.searchDoc = map[*index.Entry]*search.Doc{}
	}
	live := make(map[*index.Entry]*search.Doc, len(s.Entries))
	var entries []*index.Entry
	var docs []*search.Doc
	for _, e := range s.Entries {
		d := v.searchDocFor(e)
		live[e] = d
		if prefix != "" && !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		if tag != "" && !hasTag(e.Tags, tag) {
			continue
		}
		matched := true
		for k, want := range o.Frontmatter {
			if have, ok := e.Document.Fields[k]; !ok || !fieldMatches(have, want) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		entries, docs = append(entries, e), append(docs, d)
	}
	v.searchDoc = live // drop documents of changed or deleted notes
	hits := search.Rank(docs, q)
	out := SearchOutput{Results: []SearchResult{}, Total: len(hits)}
	for _, h := range hits[:min(limit, len(hits))] {
		e := entries[h.Index]
		out.Results = append(out.Results, SearchResult{
			Path: e.Path, Title: e.Title, Score: float64(int(h.Score*1000)) / 1000, Type: e.Type, Tags: e.Tags,
			Snippet: search.Snippet(docs[h.Index], e.Document.Body, q, 80),
		})
	}
	return out, nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want || strings.HasPrefix(t, want+"/") {
			return true
		}
	}
	return false
}

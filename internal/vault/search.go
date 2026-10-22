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
	Query       string         `json:"query" jsonschema:"Search text or a natural-language question. Terms are OR-combined: a note that matches any term is a hit, and notes matching more terms rank higher. Chinese is word-segmented; no spaces are needed."`
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

// Candidate is one note for two-stage retrieval. Excerpt is the original text
// around the best match, so a later stage can score it without reading files.
type Candidate struct {
	Path    string  `json:"path"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
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

type rankedNote struct {
	entry *index.Entry
	doc   *search.Doc
	score float64
}

func roundScore(s float64) float64 { return float64(int(s*1000)) / 1000 }

// ranked applies the filters and returns every matching note, best first. Term
// statistics are computed from the current catalog snapshot, so changed notes
// are reindexed lazily on the next query.
func (v *Vault) ranked(ctx context.Context, o SearchOptions) ([]rankedNote, search.Query, error) {
	q := search.ParseQuery(o.Query)
	if q.Empty() {
		return nil, q, errors.New("query must contain at least one searchable term")
	}
	tag := ""
	if o.Tag != "" {
		var err error
		if tag, err = markdown.NormalizeTag(o.Tag); err != nil {
			return nil, q, err
		}
	}
	prefix := strings.TrimPrefix(o.PathPrefix, "./")
	s, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return nil, q, err
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
	out := make([]rankedNote, 0, len(hits))
	for _, h := range hits {
		out = append(out, rankedNote{entry: entries[h.Index], doc: docs[h.Index], score: h.Score})
	}
	return out, q, nil
}

// Search ranks notes by BM25 over OR-combined terms.
func (v *Vault) Search(ctx context.Context, o SearchOptions) (SearchOutput, error) {
	limit := o.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return SearchOutput{}, errors.New("limit must be between 1 and 100")
	}
	notes, q, err := v.ranked(ctx, o)
	if err != nil {
		return SearchOutput{}, err
	}
	out := SearchOutput{Results: []SearchResult{}, Total: len(notes)}
	for _, n := range notes[:min(limit, len(notes))] {
		out.Results = append(out.Results, SearchResult{
			Path: n.entry.Path, Title: n.entry.Title, Score: roundScore(n.score),
			Type: n.entry.Type, Tags: n.entry.Tags,
			Snippet: search.Snippet(n.doc, n.entry.Document.Body, q, 80),
		})
	}
	return out, nil
}

// SearchCandidates returns up to n notes with a wider excerpt around the match.
// It uses the same query parsing and filters as Search. deep_search uses it as
// its first stage: BM25 candidates, then a model scores the excerpts.
func (v *Vault) SearchCandidates(ctx context.Context, o SearchOptions, n int) ([]Candidate, error) {
	if n < 1 || n > 500 {
		return nil, errors.New("candidate count must be between 1 and 500")
	}
	notes, q, err := v.ranked(ctx, o)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, min(n, len(notes)))
	for _, note := range notes[:min(n, len(notes))] {
		out = append(out, Candidate{
			Path: note.entry.Path, Title: note.entry.Title, Score: roundScore(note.score),
			Excerpt: search.Snippet(note.doc, note.entry.Document.Body, q, 300),
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

// Package adapt holds the small, target-specific knowledge the bench
// tools need: which search tool to call and how to pull note paths out
// of its result. Keeping it in one place means the rest of the bench
// code stays target-agnostic.
package adapt

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/E13Lau/vault-mcp/bench/internal/mcpclient"
)

// Target identifies the system under test.
type Target string

const (
	Vault     Target = "vault"      // vault-mcp search_notes
	VaultDeep Target = "vault-deep" // vault-mcp deep_search (model judge)
	Sage      Target = "sage"       // sage-wiki
)

// SearchSpec describes one search-tool invocation.
type SearchSpec struct {
	Tool     string         // tool name
	Args     map[string]any // arguments; query and limit are filled in
	Channels string         // sage-wiki only: bm25, vector, graph (comma list, "" = default)
}

// SearchSpec returns the search tool and fixed arguments for a target.
func (t Target) SearchSpec() SearchSpec {
	switch t {
	case Sage:
		return SearchSpec{Tool: "wiki_search", Args: map[string]any{}}
	case VaultDeep:
		return SearchSpec{Tool: "deep_search", Args: map[string]any{"expansions": 0}}
	default:
		return SearchSpec{Tool: "search_notes", Args: map[string]any{}}
	}
}

// SearchArgs builds the final tool arguments for a query.
func (s SearchSpec) SearchArgs(query string, limit int) map[string]any {
	args := make(map[string]any, len(s.Args)+2)
	for k, v := range s.Args {
		args[k] = v
	}
	args["query"] = query
	args["limit"] = limit
	if s.Channels != "" {
		args["channels"] = s.Channels
	}
	return args
}

// Hit is one search result entry, reduced to what the metrics need.
// Aliases are other vault paths the result covers: sage-wiki returns
// compiled concept articles whose frontmatter lists the source pages
// they were built from, and those source pages are what a gold page
// refers to.
type Hit struct {
	Path    string   `json:"path"`
	Aliases []string `json:"aliases,omitempty"`
	Score   float64  `json:"score"`
}

// ExtractPaths pulls result paths out of a tool call result. Both
// vault-mcp and sage-wiki return their list as JSON inside a text
// content item; the exact shapes differ, so this walks the common
// keys instead of unmarshalling a fixed struct.
func ExtractPaths(cr *mcpclient.CallResult) ([]Hit, error) {
	var hits []Hit
	for _, c := range cr.Content {
		if c.Type != "text" {
			continue
		}
		parsed, err := parseAny(c.Text)
		if err != nil {
			continue
		}
		collect(parsed, &hits)
	}
	// An empty result list is a valid search outcome (no matches), not
	// an error; the caller records zero hits for the question.
	return hits, nil
}

func parseAny(text string) (any, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// collect walks a parsed JSON value and gathers result entries. It
// understands both shapes: vault-mcp results carry "path"+"score";
// sage-wiki results carry "ID" (src:<path> or concept:<title>),
// "ArticlePath", "FinalScore" and a markdown "Content" whose
// frontmatter lists the source pages.
func collect(v any, hits *[]Hit) {
	switch t := v.(type) {
	case map[string]any:
		if p, ok := t["path"].(string); ok && p != "" {
			score := 0.0
			switch s := t["score"].(type) {
			case json.Number:
				score, _ = s.Float64()
			case float64:
				score = s
			}
			*hits = append(*hits, Hit{Path: p, Score: score})
			return // a results entry; do not descend further
		}
		if id, ok := t["ID"].(string); ok && id != "" {
			h := Hit{}
			var aliases []string
			if strings.HasPrefix(id, "src:") {
				h.Path = strings.TrimPrefix(id, "src:")
			} else {
				h.Path = id
				if title := strings.TrimPrefix(id, "concept:"); title != id {
					aliases = append(aliases, title)
				}
			}
			if ap, ok := t["ArticlePath"].(string); ok && ap != "" {
				h.Path = ap
				aliases = append(aliases, id)
			}
			if content, ok := t["Content"].(string); ok {
				aliases = append(aliases, contentFrontmatterPaths(content)...)
			}
			score := 0.0
			switch s := t["FinalScore"].(type) {
			case json.Number:
				score, _ = s.Float64()
			case float64:
				score = s
			}
			h.Aliases = dedup(aliases)
			h.Score = score
			*hits = append(*hits, h)
			return
		}
		for _, child := range t {
			collect(child, hits)
		}
	case []any:
		for _, child := range t {
			collect(child, hits)
		}
	}
}

// Match checks whether a returned path is one of the gold pages. Both
// sides are normalized to forward slashes; a returned path matches a
// gold page exactly, or by suffix after the gold page's directory
// prefix (for example _wiki/concepts/x.md vs wiki/concepts/x.md).
// prefix maps a gold path prefix to its returned-path equivalent.
func Match(returned, gold string, prefix map[string]string) bool {
	r := normalize(returned)
	g := normalize(gold)
	if r == g {
		return true
	}
	for from, to := range prefix {
		if strings.HasPrefix(g, normalize(from)) {
			mapped := normalize(to) + strings.TrimPrefix(g, normalize(from))
			if r == mapped {
				return true
			}
		}
	}
	gbase := g
	if i := strings.LastIndex(gbase, "/"); i >= 0 {
		gbase = gbase[i+1:]
	}
	gbase = strings.TrimSuffix(gbase, ".md")
	// A bare name (concept title or wikilink) matches the basename.
	if !strings.Contains(r, "/") && r == gbase {
		return true
	}
	// Last resort: same basename under the same parent directory.
	i, j := strings.LastIndex(r, "/"), strings.LastIndex(g, "/")
	if i > 0 && j > 0 && r[i:] == g[j:] {
		return true
	}
	return false
}

// MatchHit reports whether a search hit, including its aliases (source
// paths and concept titles for sage-wiki), covers the target page.
func MatchHit(h Hit, target string, prefix map[string]string) bool {
	if Match(h.Path, target, prefix) {
		return true
	}
	for _, a := range h.Aliases {
		if Match(a, target, prefix) {
			return true
		}
	}
	return false
}

// GoldRecallAtK is the fraction of gold pages covered by one of the
// first k hits. A gold page covered by a hit's aliases counts.
func GoldRecallAtK(hits []Hit, gold []string, prefix map[string]string, k int) float64 {
	if len(gold) == 0 {
		return math.NaN()
	}
	limit := min(k, len(hits))
	found := 0
	for _, g := range gold {
		for _, h := range hits[:limit] {
			if MatchHit(h, g, prefix) {
				found++
				break
			}
		}
	}
	return float64(found) / float64(len(gold))
}

// MRROverHits is MRR@k where gold and related pages both count as hits.
func MRROverHits(hits []Hit, gold, related []string, prefix map[string]string, k int) float64 {
	if len(gold) == 0 {
		return math.NaN()
	}
	limit := min(k, len(hits))
	var sum float64
	for _, g := range gold {
		for i, h := range hits[:limit] {
			if MatchHit(h, g, prefix) {
				sum += 1.0 / float64(i+1)
				break
			}
			ok := false
			for _, r := range related {
				if MatchHit(h, r, prefix) {
					ok = true
					break
				}
			}
			if ok {
				sum += 1.0 / float64(i+1)
				break
			}
		}
	}
	return sum / float64(len(gold))
}

// Paths maps returned hits to normalized paths, dropping duplicates.
func Paths(hits []Hit, prefix map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range hits {
		p := normalize(h.Path)
		for from, to := range prefix {
			if strings.HasPrefix(p, normalize(to)) {
				p = normalize(from) + strings.TrimPrefix(p, normalize(to))
			}
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func normalize(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	return strings.TrimPrefix(p, "/")
}

// contentFrontmatterPaths reads the "sources: [\"path\", ...]" line of
// a compiled article's frontmatter and returns the paths.
func contentFrontmatterPaths(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "sources:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "sources:"))
		rest = strings.TrimPrefix(rest, "[")
		rest = strings.TrimSuffix(rest, "]")
		for _, part := range strings.Split(rest, ",") {
			p := strings.Trim(strings.TrimSpace(part), `"`)
			if strings.Contains(p, "/") {
				out = append(out, p)
			}
		}
		break
	}
	return out
}

func dedup(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// PrefixMap parses "from=to" pairs, where a gold path prefix `from`
// maps to a returned-path prefix `to`. The mapping is applied in both
// directions by Match and Paths.
func PrefixMap(pairs []string) map[string]string {
	m := map[string]string{}
	for _, p := range pairs {
		from, to, ok := strings.Cut(p, "=")
		if !ok || from == "" || to == "" {
			continue
		}
		m[normalize(from)] = normalize(to)
	}
	return m
}

// RankGold returns the 1-based rank of the first gold hit in hits.
func RankGold(hits []Hit, gold []string, prefix map[string]string) int {
	for i, h := range hits {
		for _, g := range gold {
			if Match(h.Path, g, prefix) {
				return i + 1
			}
		}
	}
	return 0
}

// TopHits sorts by score descending and returns at most k hits.
func TopHits(hits []Hit, k int) []Hit {
	sorted := make([]Hit, len(hits))
	copy(sorted, hits)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted
}

// ExtractField returns one top-level field of the JSON a tool returned, or nil.
// deep_search reports its model usage under "usage" so the retrieval bench can
// record judge requests and cost per question.
func ExtractField(cr *mcpclient.CallResult, field string) json.RawMessage {
	for _, c := range cr.Content {
		if c.Type != "text" {
			continue
		}
		parsed, err := parseAny(c.Text)
		if err != nil {
			continue
		}
		if obj, ok := parsed.(map[string]any); ok {
			if v, ok := obj[field]; ok {
				raw, err := json.Marshal(v)
				if err == nil {
					return raw
				}
			}
		}
	}
	return nil
}

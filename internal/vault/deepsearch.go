package vault

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/E13Lau/vault-mcp/internal/deepsearch"
	"github.com/E13Lau/vault-mcp/internal/search"
)

// Deep search limits. BM25 picks the candidates, one model request scores a
// batch of chunks, and pages are ranked by their best chunk.
const (
	deepCandidates     = 60 // BM25 pages used as candidates
	deepChunkChars     = 8000
	deepMaxRequests    = 2 // per scoring round when expansions is 0
	deepRequestChars   = 30000
	deepTimeout        = 15 * time.Second
	deepMaxLimit       = 20
	deepDefaultLimit   = 10
	deepMaxExpansions  = 2
	deepExpansionTerms = 6
)

// DeepSearchOptions is the deep_search tool input.
type DeepSearchOptions struct {
	Query      string `json:"query" jsonschema:"Free-form question or keywords, same syntax as search_notes"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum pages to return, 1 to 20; default 10"`
	Expansions int    `json:"expansions,omitempty" jsonschema:"0 (default): one scoring round; 1 or 2: add terms from the best passages and score again"`
}

// DeepSearchResult is one page. Score is the judge's best chunk score, or the
// BM25 score when Degraded is set.
type DeepSearchResult struct {
	Path    string  `json:"path"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Heading string  `json:"heading,omitempty"`
	Snippet string  `json:"snippet"`
}

// DeepSearchOutput is the deep_search tool output.
type DeepSearchOutput struct {
	Query      string             `json:"query"`
	Candidates int                `json:"candidates" jsonschema:"Page candidates from BM25"`
	Scored     int                `json:"scored" jsonschema:"Chunks sent to the judge"`
	Degraded   bool               `json:"degraded" jsonschema:"True when the judge failed or timed out; results are BM25 only"`
	Note       string             `json:"note,omitempty"`
	Usage      deepsearch.Usage   `json:"usage"`
	Results    []DeepSearchResult `json:"results"`
}

// EnableDeepSearch registers the optional tool. Call it before serving.
func (v *Vault) EnableDeepSearch(scorer deepsearch.Scorer) {
	v.deepMu.Lock()
	defer v.deepMu.Unlock()
	v.scorer = scorer
}

// DeepSearchEnabled reports whether a judge is configured.
func (v *Vault) DeepSearchEnabled() bool {
	v.deepMu.Lock()
	defer v.deepMu.Unlock()
	return v.scorer != nil
}

func (v *Vault) judge() deepsearch.Scorer {
	v.deepMu.Lock()
	defer v.deepMu.Unlock()
	return v.scorer
}

type chunk struct {
	Heading string
	Text    string
}

// chunkBody splits a note at blank lines and headings into pieces of at most
// maxChars runes. Headings are kept as chunk labels.
func chunkBody(body string, maxChars int) []chunk {
	var out []chunk
	var cur strings.Builder
	heading := ""
	flush := func() {
		if text := strings.TrimSpace(cur.String()); text != "" {
			out = append(out, chunk{Heading: heading, Text: text})
		}
		cur.Reset()
	}
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if h, ok := blockHeading(block); ok {
			flush()
			heading = h
		}
		for utf8.RuneCountInString(block) > maxChars {
			part, rest := splitRunes(block, maxChars)
			if cur.Len() > 0 {
				flush()
			}
			cur.WriteString(part)
			flush()
			block = rest
		}
		if cur.Len() > 0 && utf8.RuneCountInString(cur.String())+utf8.RuneCountInString(block) > maxChars {
			flush()
		}
		cur.WriteString(block)
		cur.WriteString("\n\n")
	}
	flush()
	return out
}

// blockHeading reports the text of a block that is a single Markdown heading.
func blockHeading(block string) (string, bool) {
	if strings.Contains(block, "\n") {
		return "", false
	}
	level := 0
	for level < len(block) && block[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level == len(block) || block[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(block[level:]), true
}

func splitRunes(s string, n int) (string, string) {
	runes := []rune(s)
	if len(runes) <= n {
		return s, ""
	}
	return string(runes[:n]), string(runes[n:])
}

// bestChunk returns the chunk with the most query-term hits. Every page
// contributes one chunk, so a long page cannot flood the candidate list.
func bestChunk(chunks []chunk, terms []string) (chunk, bool) {
	if len(chunks) == 0 {
		return chunk{}, false
	}
	best, bestHits := chunks[0], -1
	for _, c := range chunks {
		text := strings.ToLower(c.Heading + "\n" + c.Text)
		hits := 0
		for _, t := range terms {
			if t != "" && strings.Contains(text, t) {
				hits++
			}
		}
		if hits > bestHits {
			best, bestHits = c, hits
		}
	}
	return best, true
}

// splitBatches divides passages into as few groups as the character budget
// requires, at most maxBatches. Passages that do not fit are dropped; Scored
// reports how many were actually sent.
func splitBatches(passages []deepsearch.Passage, maxBatches, maxChars int) [][]deepsearch.Passage {
	var total int
	for _, p := range passages {
		total += utf8.RuneCountInString(p.Text)
	}
	batchesNeeded := (total + maxChars - 1) / maxChars
	if batchesNeeded < 1 {
		batchesNeeded = 1
	}
	if batchesNeeded > maxBatches {
		batchesNeeded = maxBatches
	}
	target := total/batchesNeeded + 1
	if target > maxChars {
		target = maxChars
	}
	var batches [][]deepsearch.Passage
	var cur []deepsearch.Passage
	var size int
	for _, p := range passages {
		n := utf8.RuneCountInString(p.Text)
		if len(cur) > 0 && (size+n > target || len(batches) == maxBatches-1 && size+n > maxChars) {
			batches = append(batches, cur)
			cur, size = nil, 0
			if len(batches) == maxBatches {
				return batches
			}
		}
		cur = append(cur, p)
		size += n
	}
	if len(cur) > 0 && len(batches) < maxBatches {
		batches = append(batches, cur)
	}
	return batches
}

func snippetOf(text string, terms []string, radius int) string {
	runes := []rune(text)
	start := 0
	lowered := strings.ToLower(text)
	for _, t := range terms {
		if i := strings.Index(lowered, t); i >= 0 {
			start = max(0, utf8.RuneCountInString(text[:i])-radius/2)
			break
		}
	}
	end := min(len(runes), start+radius)
	s := strings.Join(strings.Fields(string(runes[start:end])), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}

type scoredPage struct {
	path    string
	title   string
	heading string
	text    string
	score   float64
	rank    int
}

// DeepSearch runs BM25 candidate selection, one judge round (plus optional
// expansion rounds) and page-level ranking. On judge failure or timeout it
// returns the BM25 results and sets Degraded.
func (v *Vault) DeepSearch(ctx context.Context, o DeepSearchOptions) (DeepSearchOutput, error) {
	scorer := v.judge()
	if scorer == nil {
		return DeepSearchOutput{}, errors.New("deep search is not configured")
	}
	query := strings.TrimSpace(o.Query)
	if query == "" {
		return DeepSearchOutput{}, errors.New("query must contain at least one keyword")
	}
	limit := o.Limit
	if limit == 0 {
		limit = deepDefaultLimit
	}
	if limit < 1 || limit > deepMaxLimit {
		return DeepSearchOutput{}, errors.New("limit must be between 1 and 20")
	}
	if o.Expansions < 0 || o.Expansions > deepMaxExpansions {
		return DeepSearchOutput{}, errors.New("expansions must be 0, 1, or 2")
	}
	snap, err := v.catalog.Snapshot(ctx)
	if err != nil {
		return DeepSearchOutput{}, err
	}
	base, err := v.currentSearch(ctx, query)
	if err != nil {
		return DeepSearchOutput{}, err
	}
	out := DeepSearchOutput{Query: query, Results: []DeepSearchResult{}, Usage: deepsearch.Usage{Model: scorer.Model()}}
	pages := map[string]*scoredPage{}
	order := make([]string, 0, len(base.Results))
	var passages []deepsearch.Passage
	var owners []string
	terms := search.Terms(query, false)
	for rank, r := range base.Results {
		e := snap.ByPath[r.Path]
		if e == nil {
			continue
		}
		pages[r.Path] = &scoredPage{path: r.Path, title: e.Title, rank: rank}
		order = append(order, r.Path)
		if c, ok := bestChunk(chunkBody(e.Document.Body, deepChunkChars), terms); ok && len(passages) < deepCandidates {
			passages = append(passages, deepsearch.Passage{ID: len(passages) + 1, Title: e.Title, Heading: c.Heading, Text: c.Text})
			owners = append(owners, r.Path)
		}
	}
	out.Candidates = len(order)
	if len(passages) == 0 {
		out.Note = "BM25 found no candidate page"
		return out, nil
	}
	merge := func(scores []deepsearch.Score, usage deepsearch.Usage) {
		out.Usage.Add(usage)
		for _, s := range scores {
			if s.ID < 1 || s.ID > len(passages) {
				continue
			}
			p := pages[owners[s.ID-1]]
			if p == nil || s.Value <= p.score {
				continue
			}
			chunk := passages[s.ID-1]
			p.score, p.heading, p.text = s.Value, chunk.Heading, chunk.Text
		}
	}
	judgeCtx, cancel := context.WithTimeout(ctx, deepTimeout)
	defer cancel()
	runRound := func(round []deepsearch.Passage) error {
		batches := splitBatches(round, deepMaxRequests, deepRequestChars)
		type result struct {
			scores []deepsearch.Score
			usage  deepsearch.Usage
			err    error
		}
		results := make([]result, len(batches))
		var wg sync.WaitGroup
		for i, batch := range batches {
			wg.Add(1)
			go func(i int, batch []deepsearch.Passage) {
				defer wg.Done()
				scores, usage, err := scorer.Score(judgeCtx, query, batch)
				results[i] = result{scores: scores, usage: usage, err: err}
			}(i, batch)
		}
		wg.Wait()
		for _, r := range results {
			if r.err != nil {
				return r.err
			}
			merge(r.scores, r.usage)
		}
		// Count only the passages that reached a request.
		sent := 0
		for _, b := range batches {
			sent += len(b)
		}
		out.Scored += sent
		return nil
	}
	if err := runRound(passages); err != nil {
		return v.degraded(base, out, limit, err)
	}
	for round := 0; round < o.Expansions; round++ {
		if judgeCtx.Err() != nil {
			return v.degraded(base, out, limit, judgeCtx.Err())
		}
		extra := v.expandTerms(query, pages)
		if len(extra) == 0 {
			break
		}
		next, err := v.currentSearch(ctx, query+" "+strings.Join(extra, " "))
		if err != nil {
			break
		}
		var fresh []deepsearch.Passage
		for _, r := range next.Results {
			if _, ok := pages[r.Path]; ok || len(fresh) >= deepCandidates {
				continue
			}
			e := snap.ByPath[r.Path]
			if e == nil {
				continue
			}
			c, ok := bestChunk(chunkBody(e.Document.Body, deepChunkChars), terms)
			if !ok {
				continue
			}
			pages[r.Path] = &scoredPage{path: r.Path, title: e.Title, rank: len(order)}
			order = append(order, r.Path)
			passages = append(passages, deepsearch.Passage{ID: len(passages) + 1, Title: e.Title, Heading: c.Heading, Text: c.Text})
			owners = append(owners, r.Path)
			fresh = append(fresh, passages[len(passages)-1])
		}
		if len(fresh) == 0 {
			break
		}
		if err := runRound(fresh); err != nil {
			return v.degraded(base, out, limit, err)
		}
	}
	ranked := make([]*scoredPage, 0, len(order))
	for _, path := range order {
		if p := pages[path]; p != nil {
			ranked = append(ranked, p)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].rank < ranked[j].rank
	})
	for _, p := range ranked[:min(limit, len(ranked))] {
		out.Results = append(out.Results, DeepSearchResult{
			Path: p.path, Title: p.title, Score: round3(p.score), Heading: p.heading,
			Snippet: snippetOf(p.text, terms, 240),
		})
	}
	out.Usage.CostUSD = round6(out.Usage.CostUSD)
	return out, nil
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }
func round6(f float64) float64 { return float64(int(f*1e6+0.5)) / 1e6 }

// currentSearch runs BM25 in any-word mode.
func (v *Vault) currentSearch(ctx context.Context, query string) (SearchOutput, error) {
	return v.Search(ctx, SearchOptions{Query: query, Match: string(search.Any), Limit: deepCandidates})
}

// degraded returns the BM25 ranking, clearly marked as unscored.
func (v *Vault) degraded(base SearchOutput, out DeepSearchOutput, limit int, cause error) (DeepSearchOutput, error) {
	out.Degraded = true
	out.Results = []DeepSearchResult{}
	out.Note = "judge unavailable (" + cause.Error() + "); results are BM25 only and carry no model score"
	out.Usage.CostUSD = round6(out.Usage.CostUSD)
	for _, r := range base.Results[:min(limit, len(base.Results))] {
		out.Results = append(out.Results, DeepSearchResult{Path: r.Path, Title: r.Title, Score: r.Score, Snippet: r.Snippet})
	}
	return out, nil
}

// expandTerms picks frequent terms from the best pages that the query does not
// already contain. SAG2 does this from high-scoring passages; the intent is to
// bridge vocabulary between a question and the note that answers it.
func (v *Vault) expandTerms(query string, pages map[string]*scoredPage) []string {
	have := map[string]bool{}
	for _, t := range search.Terms(query, false) {
		have[t] = true
	}
	rank := make([]*scoredPage, 0, len(pages))
	for _, p := range pages {
		if p.score > 0 && p.text != "" {
			rank = append(rank, p)
		}
	}
	sort.SliceStable(rank, func(i, j int) bool { return rank[i].score > rank[j].score })
	counts := map[string]int{}
	for i, p := range rank {
		if i == 3 {
			break
		}
		for _, t := range search.Terms(p.text, false) {
			if have[t] || search.IsStopword(t) || utf8.RuneCountInString(t) < 2 {
				continue
			}
			counts[t]++
		}
	}
	terms := make([]string, 0, len(counts))
	for t := range counts {
		terms = append(terms, t)
	}
	sort.Slice(terms, func(i, j int) bool {
		if counts[terms[i]] != counts[terms[j]] {
			return counts[terms[i]] > counts[terms[j]]
		}
		return terms[i] < terms[j]
	})
	if len(terms) > deepExpansionTerms {
		terms = terms[:deepExpansionTerms]
	}
	return terms
}

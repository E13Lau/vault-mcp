// Command retrieval runs the search-only part of the benchmark: for
// every question of type A–D it calls the MCP search tool directly
// (no model involved), records the returned paths and computes
// Recall@5 and MRR@10 per question type.
//
// It connects to exactly one MCP server and never touches the vault
// filesystem itself.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/E13Lau/vault-mcp/bench/internal/adapt"
	"github.com/E13Lau/vault-mcp/bench/internal/mcpclient"
	"github.com/E13Lau/vault-mcp/bench/internal/metrics"
	"github.com/E13Lau/vault-mcp/bench/internal/questions"
)

type flags struct {
	questionsFile string
	outDir        string
	target        string
	mcpURL        string
	stdioCmd      string
	limit         int
	prefixPairs   []string
	channels      string
	searchText    string
	timeout       time.Duration
}

func main() {
	var f flags
	flag.StringVar(&f.questionsFile, "questions", "", "questions.jsonl file")
	flag.StringVar(&f.outDir, "out", "", "output directory (results JSON + CSV)")
	flag.StringVar(&f.target, "target", "vault", "system under test: vault, vault-deep, or sage")
	flag.StringVar(&f.mcpURL, "mcp-url", "", "Streamable HTTP endpoint URL (e.g. http://127.0.0.1:8080/mcp)")
	flag.StringVar(&f.stdioCmd, "stdio-cmd", "", "server command for the stdio transport, e.g. /path/server --arg1")
	flag.IntVar(&f.limit, "limit", 10, "number of search results requested per query")
	flag.Var(&stringList{&f.prefixPairs}, "prefix", "gold=returned path prefix mapping (repeatable)")
	flag.StringVar(&f.channels, "channels", "", "sage-wiki search channels (bm25,vector,graph); empty = server default")
	flag.StringVar(&f.searchText, "search-text", "query", "what to send to the search tool: query (the query field, falling back to question) or question (the natural question text)")
	flag.DurationVar(&f.timeout, "timeout", 120*time.Second, "per-call timeout")
	flag.Parse()
	if f.questionsFile == "" || f.outDir == "" {
		fatal("usage: retrieval -questions questions.jsonl -out <dir> (-mcp-url <url> | -stdio-cmd <cmd>) [-target vault|sage]")
	}
	if (f.mcpURL == "") == (f.stdioCmd == "") {
		fatal("exactly one of -mcp-url or -stdio-cmd is required")
	}
	if err := run(f); err != nil {
		fatal("%v", err)
	}
}

type perQuestion struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Question     string          `json:"question"`
	SearchText   string          `json:"search_text"`
	GoldPages    []string        `json:"gold_pages"`
	RelatedPages []string        `json:"related_pages,omitempty"`
	Results      []adapt.Hit     `json:"results"`
	HitRanks     map[string]int  `json:"hit_ranks"`     // gold page -> rank, 0 = not in top-k
	RelatedRanks map[string]int  `json:"related_ranks"` // related page -> rank, 0 = not in top-k
	FirstHit     int             `json:"first_hit_rank"`
	DurationMs   float64         `json:"duration_ms"`
	JudgeUsage   json.RawMessage `json:"judge_usage,omitempty"` // deep_search: requests, tokens, cost
}

type retrievalResult struct {
	Target     string                     `json:"target"`
	SearchTool string                     `json:"search_tool"`
	Limit      int                        `json:"limit"`
	Generated  time.Time                  `json:"generated_at"`
	ByType     map[string]metrics.Summary `json:"recall_mrr_by_type"`
	Questions  []perQuestion              `json:"questions"`
}

func run(f flags) error {
	target := adapt.Target(f.target)
	spec := target.SearchSpec()
	if f.channels != "" {
		spec.Channels = f.channels
	}
	prefix := adapt.PrefixMap(f.prefixPairs)
	qs, err := questions.LoadQuestions(f.questionsFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		return err
	}
	client, err := dial(f, target)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx := context.Background()
	if _, err := client.Initialize(ctx); err != nil {
		return err
	}
	out := retrievalResult{
		Target:     f.target,
		SearchTool: spec.Tool,
		Limit:      f.limit,
		Generated:  time.Now(),
		ByType:     map[string]metrics.Summary{},
	}
	// recall and mrr are reported per question type.
	recallByType := map[string][]float64{}
	mrrByType := map[string][]float64{}
	for _, q := range qs {
		if q.Type == "E" {
			continue // E questions have no gold pages; they are agent-only
		}
		start := time.Now()
		query := q.Query
		if f.searchText == "question" || query == "" {
			query = q.Question
		}
		cr, err := client.CallTool(ctx, spec.Tool, spec.SearchArgs(query, f.limit))
		if err != nil {
			return fmt.Errorf("question %s: %w", q.ID, err)
		}
		hits, err := adapt.ExtractPaths(cr)
		if err != nil {
			return fmt.Errorf("question %s: %w", q.ID, err)
		}
		judgeUsage := adapt.ExtractField(cr, "usage")
		hits = adapt.TopHits(hits, f.limit)
		// Scoring rule (same for both targets): drop the vault-wide
		// index.md from the result list before computing Recall/MRR,
		// because it contains every page name and would otherwise
		// match every keyword. Keep it only when a question uses it as
		// a gold page.
		hits = dropIndexPage(hits, q.GoldPages, prefix)
		pq := perQuestion{
			ID:           q.ID,
			Type:         q.Type,
			Question:     q.Question,
			SearchText:   query,
			GoldPages:    q.GoldPages,
			RelatedPages: q.RelatedPages,
			Results:      hits,
			HitRanks:     map[string]int{},
			RelatedRanks: map[string]int{},
			FirstHit:     rankOfHit(hits, q.GoldPages, prefix),
			DurationMs:   float64(time.Since(start).Microseconds()) / 1000,
			JudgeUsage:   judgeUsage,
		}
		for _, g := range q.GoldPages {
			pq.HitRanks[g] = rankOfHitOne(hits, g, prefix)
		}
		for _, r := range q.RelatedPages {
			pq.RelatedRanks[r] = rankOfHitOne(hits, r, prefix)
		}
		out.Questions = append(out.Questions, pq)
		recallByType[q.Type] = append(recallByType[q.Type], adapt.GoldRecallAtK(hits, q.GoldPages, prefix, 5))
		mrrByType[q.Type] = append(mrrByType[q.Type], adapt.MRROverHits(hits, q.GoldPages, q.RelatedPages, prefix, 10))
		fmt.Printf("%s type=%s first_hit=%d dur=%.1fms\n", q.ID, q.Type, pq.FirstHit, pq.DurationMs)
	}
	for _, t := range []string{"A", "B", "C", "D"} {
		if len(recallByType[t]) == 0 {
			continue
		}
		out.ByType[t] = metrics.Summary{
			N:    len(recallByType[t]),
			Mean: mean(recallByType[t]),
			P50:  metrics.Percentile(recallByType[t], 50),
			P95:  metrics.Percentile(recallByType[t], 95),
			Min:  metrics.Percentile(recallByType[t], 0),
			Max:  metrics.Percentile(recallByType[t], 100),
		}
		_ = mrrByType // mrr per type is written into the CSV below
	}
	// write JSON
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.outDir, "retrieval.json"), raw, 0o644); err != nil {
		return err
	}
	// write CSV: id,type,search_text,first_hit,recall@5,mrr@10,num_results
	csvFile, err := os.Create(filepath.Join(f.outDir, "retrieval.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(csvFile)
	_ = w.Write([]string{"id", "type", "search_text", "first_hit_rank", "recall_at_5", "mrr_at_10", "num_results"})
	for _, pq := range out.Questions {
		r := adapt.GoldRecallAtK(pq.Results, pq.GoldPages, prefix, 5)
		m := adapt.MRROverHits(pq.Results, pq.GoldPages, pq.RelatedPages, prefix, 10)
		_ = w.Write([]string{pq.ID, pq.Type, pq.SearchText,
			itoa(pq.FirstHit), ftoa(r), ftoa(m), itoa(len(pq.Results))})
	}
	// per-type summary rows
	for _, t := range []string{"A", "B", "C", "D"} {
		if len(mrrByType[t]) == 0 {
			continue
		}
		_ = w.Write([]string{"SUMMARY", t, "", "", ftoa(mean(recallByType[t])), ftoa(mean(mrrByType[t])), ""})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := csvFile.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s and retrieval.csv (search tool %s)\n", filepath.Join(f.outDir, "retrieval.json"), spec.Tool)
	return nil
}

// dial builds the MCP client for the configured transport.
func dial(f flags, target adapt.Target) (mcpclient.Client, error) {
	if f.stdioCmd != "" {
		parts, err := shellSplit(f.stdioCmd)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty -stdio-cmd")
		}
		// Server logs go to a file next to the output dir; they must
		// never be parsed or shipped.
		logFile := filepath.Join(f.outDir, "mcp-server.log")
		logs, err := os.Create(logFile)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "server stderr: %s\n", logFile)
		return mcpclient.NewStdioClient(parts[0], parts[1:], logs), nil
	}
	token := os.Getenv("MCP_TOKEN")
	return mcpclient.NewHTTPClient(f.mcpURL, token, f.timeout), nil
}

type stringList struct{ p *[]string }

func (s *stringList) String() string     { return fmt.Sprint(*s.p) }
func (s *stringList) Set(v string) error { *s.p = append(*s.p, v); return nil }

// shellSplit splits a command line on whitespace, honouring double
// quotes. It is deliberately small; server commands do not need
// escapes beyond quotes.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur []rune
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in -stdio-cmd")
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out, nil
}

// rankOfHitOne returns the 1-based rank of the first hit covering the
// page (directly or through an alias), or 0.
func rankOfHitOne(hits []adapt.Hit, page string, prefix map[string]string) int {
	for i, h := range hits {
		if adapt.MatchHit(h, page, prefix) {
			return i + 1
		}
	}
	return 0
}

// rankOfHit is the first rank among the gold pages, or 0.
func rankOfHit(hits []adapt.Hit, gold []string, prefix map[string]string) int {
	best := 0
	for _, g := range gold {
		if r := rankOfHitOne(hits, g, prefix); r > 0 && (best == 0 || r < best) {
			best = r
		}
	}
	return best
}

// dropIndexPage removes the vault-wide index page from the results.
// It is kept only when the question itself uses it as a gold page.
func dropIndexPage(hits []adapt.Hit, gold []string, prefix map[string]string) []adapt.Hit {
	for _, g := range gold {
		if isRootIndex(g) {
			return hits
		}
	}
	out := hits[:0]
	for _, h := range hits {
		if isRootIndex(h.Path) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func isRootIndex(path string) bool {
	p := strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "/")
	return p == "index.md" || p == "_wiki/index.md"
}

func itoa(v int) string { return strconv.Itoa(v) }

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "retrieval: "+format+"\n", args...)
	os.Exit(1)
}

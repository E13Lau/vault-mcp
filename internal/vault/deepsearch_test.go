package vault

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/E13Lau/vault-mcp/internal/deepsearch"
)

type fakeScorer struct {
	mu     sync.Mutex
	rounds int
	err    error
	delay  time.Duration
	scores map[string]float64 // marker found in passage text -> score
}

func (f *fakeScorer) Model() string { return "fake-judge" }

func (f *fakeScorer) Score(ctx context.Context, _ string, ps []deepsearch.Passage) ([]deepsearch.Score, deepsearch.Usage, error) {
	f.mu.Lock()
	f.rounds++
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, deepsearch.Usage{Requests: 1}, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, deepsearch.Usage{Requests: 1, InputTokens: 10}, f.err
	}
	out := make([]deepsearch.Score, 0, len(ps))
	for _, p := range ps {
		value := 0.1
		for marker, score := range f.scores {
			if strings.Contains(p.Text, marker) {
				value = score
			}
		}
		out = append(out, deepsearch.Score{ID: p.ID, Value: value})
	}
	return out, deepsearch.Usage{Requests: 1, InputTokens: 100, Model: "fake-judge"}, nil
}

func (f *fakeScorer) roundCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rounds
}

func TestDeepSearchRanksPagesByJudge(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "answer.md", "# 答案\n\nConnect 崩溃率是 0.6%。")
	put(t, dir, "noise.md", "# 无关\n\n崩溃率这个词只是提到。")
	ctx := context.Background()
	if err := v.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	sc := &fakeScorer{scores: map[string]float64{"0.6%": 0.95, "只是提到": 0.2}}
	v.EnableDeepSearch(sc)
	out, err := v.DeepSearch(ctx, DeepSearchOptions{Query: "崩溃率"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Degraded {
		t.Fatalf("unexpected degraded: %s", out.Note)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v", out.Results)
	}
	if out.Results[0].Path != "answer.md" || out.Results[0].Score != 0.95 {
		t.Fatalf("first result = %+v", out.Results[0])
	}
	if out.Results[0].Snippet == "" || out.Results[0].Heading != "答案" {
		t.Fatalf("snippet/heading = %+v", out.Results[0])
	}
	if out.Usage.Requests != 1 || out.Usage.InputTokens != 100 || out.Usage.Model != "fake-judge" {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if out.Scored != 2 || out.Candidates != 2 {
		t.Fatalf("counters = %+v", out)
	}
}

func TestDeepSearchFallsBackToBM25(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "answer.md", "# 答案\n\nConnect 崩溃率是 0.6%。")
	ctx := context.Background()
	if err := v.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	v.EnableDeepSearch(&fakeScorer{err: errors.New("HTTP 402: insufficient funds")})
	out, err := v.DeepSearch(ctx, DeepSearchOptions{Query: "崩溃率"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Degraded || !strings.Contains(out.Note, "BM25 only") {
		t.Fatalf("degraded=%v note=%q", out.Degraded, out.Note)
	}
	if len(out.Results) != 1 || out.Results[0].Path != "answer.md" {
		t.Fatalf("results = %+v", out.Results)
	}
}

func TestDeepSearchTimeoutFallsBack(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "answer.md", "# 答案\n\nConnect 崩溃率是 0.6%。")
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	v.EnableDeepSearch(&fakeScorer{delay: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	out, err := v.DeepSearch(ctx, DeepSearchOptions{Query: "崩溃率"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Degraded || len(out.Results) == 0 {
		t.Fatalf("timeout must degrade to BM25: %+v", out)
	}
}

func TestDeepSearchExpansionRound(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "a.md", "# A\n\nFoo 项目 使用 QuantumLeap 方案。")
	put(t, dir, "b.md", "# B\n\nQuantumLeap 方案 的细节。")
	ctx := context.Background()
	if err := v.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	sc := &fakeScorer{scores: map[string]float64{"Foo": 0.9, "细节": 0.7}}
	v.EnableDeepSearch(sc)
	base, err := v.DeepSearch(ctx, DeepSearchOptions{Query: "Foo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Results) != 1 || base.Results[0].Path != "a.md" {
		t.Fatalf("base results = %+v", base.Results)
	}
	expanded, err := v.DeepSearch(ctx, DeepSearchOptions{Query: "Foo", Expansions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if expanded.Results[0].Path != "a.md" || expanded.Results[0].Score != 0.9 {
		t.Fatalf("expanded first = %+v", expanded.Results[0])
	}
	found := false
	for _, r := range expanded.Results {
		if r.Path == "b.md" && r.Score == 0.7 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expansion did not score the new page: %+v", expanded.Results)
	}
	if sc.roundCount() < 3 {
		t.Fatalf("rounds = %d, want at least 3", sc.roundCount())
	}
}

func TestDeepSearchValidation(t *testing.T) {
	v, _ := fixture(t, nil, false)
	if _, err := v.DeepSearch(context.Background(), DeepSearchOptions{Query: "x"}); err == nil {
		t.Fatal("deep search must fail when no judge is configured")
	}
	v.EnableDeepSearch(&fakeScorer{})
	for _, tc := range []DeepSearchOptions{
		{Query: ""},
		{Query: "x", Limit: 21},
		{Query: "x", Expansions: 3},
	} {
		if _, err := v.DeepSearch(context.Background(), tc); err == nil {
			t.Fatalf("expected an error for %+v", tc)
		}
	}
}

func TestChunkBody(t *testing.T) {
	body := "# 标题一\n\n段落一\n\n# 标题二\n\n" + strings.Repeat("中", 25)
	chunks := chunkBody(body, 10)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	headings := map[string]bool{}
	for _, c := range chunks {
		if utf8.RuneCountInString(c.Text) > 10 {
			t.Fatalf("chunk too long (%d runes): %q", utf8.RuneCountInString(c.Text), c.Text)
		}
		headings[c.Heading] = true
	}
	if !headings["标题一"] || !headings["标题二"] {
		t.Fatalf("headings = %+v", headings)
	}
	best, ok := bestChunk(chunks, []string{"中中中"})
	if !ok || !strings.Contains(best.Text, "中中中") {
		t.Fatalf("best chunk = %+v", best)
	}
}

func TestSplitBatches(t *testing.T) {
	var passages []deepsearch.Passage
	for i := 1; i <= 10; i++ {
		passages = append(passages, deepsearch.Passage{ID: i, Text: strings.Repeat("x", 100)})
	}
	batches := splitBatches(passages, 2, 600)
	if len(batches) != 2 {
		t.Fatalf("batches = %d", len(batches))
	}
	total := 0
	for _, b := range batches {
		total += len(b)
	}
	if total != 10 {
		t.Fatalf("scored %d of 10", total)
	}
	if one := splitBatches(passages[:2], 2, 600); len(one) != 1 {
		t.Fatalf("small input split into %d batches", len(one))
	}
}

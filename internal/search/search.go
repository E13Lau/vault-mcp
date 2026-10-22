// Package search ranks notes with BM25 over dictionary words and CJK n-grams.
//
// A query is a bag of terms combined with OR: a note that matches any term is
// a hit, and notes that match more (or rarer) terms score higher. Chinese text
// has no spaces, so CJK runs are split with the embedded jieba-compatible
// dictionary, and each run also contributes every character and every adjacent
// pair. The extra n-grams keep recall when the dictionary splits a query and a
// note differently.
package search

import (
	_ "embed"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-ego/gse"
)

const (
	titleWeight = 3.0
	pathWeight  = 2.0
	tagWeight   = 2.0
	k1, b       = 1.2, 0.3

	// Bonuses for a query word that appears as a whole substring.
	titleSubstring = 3.0
	bodySubstring  = 1.0

	// Query term weights. A dictionary word is the strongest signal; an
	// adjacent pair only adds recall.
	wordWeight   = 1.0
	bigramWeight = 0.2
	maxTermScore = 2.5

	// Notes that match more distinct query terms rank higher than notes that
	// match one. The multiplier is capped at 1 + coverageBoost.
	coverageBoost = 0.25

	// lengthCapTerms bounds the document-length factor. Notes are multi-topic:
	// without a cap, a long entity page that mentions a query word once is
	// buried under short notes that mention it twice. Length still matters up
	// to 800 index terms; beyond that, extra length adds no further penalty.
	lengthCapTerms = 800
)

// dictText is the embedded Simplified Chinese dictionary: word, frequency and
// part-of-speech per line, in the jieba/gse format. See data/README.md.
//
//go:embed data/dict_zh.txt
var dictText string

var (
	segOnce sync.Once
	segMu   sync.Mutex
	seg     *gse.Segmenter
)

// segmenter loads the dictionary once, on the first tokenization, so server
// startup and sessions that never search do not pay for it.
func segmenter() *gse.Segmenter {
	segOnce.Do(func() {
		seg = &gse.Segmenter{}
		if err := seg.LoadDictStr(dictText); err != nil {
			panic("search: load embedded dictionary: " + err.Error())
		}
	})
	return seg
}

// cutCJK splits one CJK run into dictionary words. Unknown runs fall back to
// their characters. The segmenter is not safe for concurrent use, so calls are
// serialized.
func cutCJK(text string) []string {
	s := segmenter()
	segMu.Lock()
	words := s.Cut(text, true)
	segMu.Unlock()
	out := make([]string, 0, len(words))
	for _, w := range words {
		keep := true
		for _, r := range w {
			if !isCJK(r) {
				keep = false
				break
			}
		}
		if keep && w != "" {
			out = append(out, strings.ToLower(w))
		}
	}
	if len(out) == 0 {
		for _, r := range text {
			out = append(out, string(unicode.ToLower(r)))
		}
	}
	return out
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

type runKind uint8

const (
	runWord runKind = iota // Latin word, number, or mixed token such as 4.4
	runCJK
)

type textRun struct {
	kind runKind
	text string
}

// splitRuns cuts text into Latin/number tokens and CJK runs. Everything else
// (spaces, punctuation, symbols) separates runs.
func splitRuns(text string) []textRun {
	var out []textRun
	rs := []rune(text)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case isCJK(r):
			j := i + 1
			for j < len(rs) && isCJK(rs[j]) {
				j++
			}
			out = append(out, textRun{runCJK, string(rs[i:j])})
			i = j
		case isWord(r):
			j := i + 1
			for j < len(rs) {
				c := rs[j]
				if isWord(c) && !isCJK(c) {
					j++
					continue
				}
				// Keep decimal points and digit separators inside a number,
				// so 4.4 and 1,000 stay one term.
				if (c == '.' || c == ',') && j+1 < len(rs) && unicode.IsNumber(rs[j-1]) && unicode.IsNumber(rs[j+1]) {
					j++
					continue
				}
				break
			}
			out = append(out, textRun{runWord, strings.ToLower(string(rs[i:j]))})
			i = j
		default:
			i++
		}
	}
	return out
}

// Terms splits text into lowercase terms. With grams false a CJK run yields
// only its dictionary words; with grams true it also yields every character and
// every adjacent pair, which the document index needs.
func Terms(text string, grams bool) []string {
	var out []string
	for _, r := range splitRuns(text) {
		if r.kind == runWord {
			out = append(out, r.text)
			continue
		}
		out = append(out, cutCJK(r.text)...)
		if !grams {
			continue
		}
		rs := []rune(r.text)
		for _, c := range rs {
			out = append(out, string(c))
		}
		for i := 0; i+1 < len(rs); i++ {
			out = append(out, string(rs[i:i+2]))
		}
	}
	return out
}

// Doc is the searchable form of one note. It is immutable once built.
type Doc struct {
	tf     map[string]float64
	length float64
	title  string // lowercased
	body   string // lowercased, same rune count as the original
}

func lower(s string) string { return strings.Map(unicode.ToLower, s) }

func NewDoc(title, path string, tags []string, body string) *Doc {
	d := &Doc{tf: map[string]float64{}, title: lower(title), body: lower(body)}
	add := func(term string, weight float64) {
		d.tf[term] += weight
	}
	for _, t := range Terms(body, true) {
		add(t, 1)
		d.length++
	}
	for _, t := range Terms(title, true) {
		add(t, titleWeight)
	}
	for _, t := range Terms(path, true) {
		add(t, pathWeight)
	}
	for _, tag := range tags {
		for _, t := range Terms(tag, true) {
			add(t, tagWeight)
		}
	}
	return d
}

// Query is a parsed search request. Terms are OR-combined.
type Query struct {
	terms []weightedTerm
	keys  []string // lowercase words used for substring bonuses and snippets
}

type weightedTerm struct {
	text   string
	weight float64
}

// ParseQuery turns free text, including a natural-language question, into
// weighted terms. Stopwords are dropped so a question is not dominated by
// words that occur in almost every note.
func ParseQuery(q string) Query {
	scores := map[string]float64{}
	var keys []string
	seenKey := map[string]bool{}
	add := func(text string, weight float64) {
		if text == "" || weight <= 0 {
			return
		}
		if cur := scores[text]; cur+weight > maxTermScore {
			weight = maxTermScore - cur
		}
		if weight <= 0 {
			return
		}
		scores[text] += weight
	}
	addKey := func(text string) {
		if seenKey[text] || utf8.RuneCountInString(text) < 2 {
			return
		}
		seenKey[text] = true
		keys = append(keys, text)
	}
	for _, r := range splitRuns(q) {
		if r.kind == runWord {
			if stopWords[r.text] {
				continue
			}
			add(r.text, wordWeight)
			addKey(r.text)
			continue
		}
		var kept []string
		for _, w := range cutCJK(r.text) {
			if stopWords[w] {
				continue
			}
			kept = append(kept, w)
			add(w, wordWeight)
			addKey(w)
		}
		if len(kept) == 0 {
			continue
		}
		// Adjacent pairs of the kept words keep recall when the dictionary
		// segments the query and a note differently.
		joined := []rune(strings.Join(kept, ""))
		for i := 0; i+1 < len(joined); i++ {
			add(string(joined[i:i+2]), bigramWeight)
		}
	}
	if len(scores) == 0 {
		return Query{}
	}
	out := Query{keys: keys}
	out.terms = make([]weightedTerm, 0, len(scores))
	for text, weight := range scores {
		out.terms = append(out.terms, weightedTerm{text: text, weight: weight})
	}
	// Fixed order makes scores reproducible when several terms contribute.
	sort.Slice(out.terms, func(i, j int) bool { return out.terms[i].text < out.terms[j].text })
	// Longer words are more specific, so snippets centre on them first.
	sort.SliceStable(out.keys, func(i, j int) bool {
		li, lj := utf8.RuneCountInString(out.keys[i]), utf8.RuneCountInString(out.keys[j])
		if li != lj {
			return li > lj
		}
		return out.keys[i] < out.keys[j]
	})
	return out
}

func (q Query) Empty() bool { return len(q.terms) == 0 }

type Hit struct {
	Index   int
	Score   float64
	Snippet string
}

// Rank scores docs against q. Every document that matches at least one term is
// returned, best first, ties by index.
func Rank(docs []*Doc, q Query) []Hit {
	if q.Empty() || len(docs) == 0 {
		return nil
	}
	var total float64
	for _, d := range docs {
		total += d.length
	}
	df := make(map[string]int, len(q.terms))
	for _, t := range q.terms {
		n := 0
		for _, d := range docs {
			if d.tf[t.text] > 0 {
				n++
			}
		}
		df[t.text] = n
	}
	avg := total / math.Max(1, float64(len(docs)))
	if avg == 0 {
		avg = 1
	}
	n := float64(len(docs))
	var hits []Hit
	for i, d := range docs {
		dl := math.Min(d.length, lengthCapTerms)
		score := 0.0
		matched := 0
		for _, t := range q.terms {
			f := d.tf[t.text]
			if f == 0 {
				continue
			}
			matched++
			idf := math.Log(1 + (n-float64(df[t.text])+0.5)/(float64(df[t.text])+0.5))
			score += t.weight * idf * f * (k1 + 1) / (f + k1*(1-b+b*dl/avg))
		}
		if matched == 0 {
			continue
		}
		for _, key := range q.keys {
			if strings.Contains(d.title, key) {
				score += titleSubstring
			}
			if strings.Contains(d.body, key) {
				score += bodySubstring
			}
		}
		if matched > 1 {
			score *= 1 + coverageBoost*(1-1/float64(matched))
		}
		hits = append(hits, Hit{Index: i, Score: score})
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	return hits
}

// Snippet returns about 2*radius runes of original around the first query word
// found in the document, or the start of the body if none is present.
func Snippet(d *Doc, original string, q Query, radius int) string {
	at := -1
	for _, key := range q.keys {
		if i := strings.Index(d.body, key); i >= 0 {
			at = utf8.RuneCountInString(d.body[:i])
			break
		}
	}
	runes := []rune(original)
	start, end := 0, len(runes)
	if at >= 0 {
		start = max(0, at-radius)
	}
	end = min(len(runes), start+2*radius)
	s := strings.Join(strings.Fields(string(runes[start:end])), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}

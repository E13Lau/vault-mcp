// Package search ranks notes with BM25 over word and CJK n-gram terms. CJK text
// has no spaces, so each CJK character and each adjacent pair is a term.
package search

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	titleWeight = 3.0
	pathWeight  = 2.0
	tagWeight   = 2.0
	k1, b       = 1.2, 0.75
	// Bonuses for a keyword that appears as a whole substring.
	titleSubstring = 3.0
	bodySubstring  = 1.0
)

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// Terms splits text into lowercase terms. With grams false, a CJK run yields
// only its bigrams (or its single character when the run has length one);
// this is the form used for queries. With grams true it also yields every
// single character, which the document index needs.
func Terms(text string, grams bool) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		if len(cjk) == 1 || (grams && len(cjk) > 1) {
			for _, r := range cjk {
				out = append(out, string(r))
			}
		}
		for i := 0; i+1 < len(cjk); i++ {
			out = append(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, unicode.ToLower(r))
		case isWord(r):
			flushCJK()
			word = append(word, unicode.ToLower(r))
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
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
	for _, t := range Terms(body, true) {
		d.tf[t]++
		d.length++
	}
	for _, t := range Terms(title, true) {
		d.tf[t] += titleWeight
	}
	for _, t := range Terms(path, true) {
		d.tf[t] += pathWeight
	}
	for _, tag := range tags {
		for _, t := range Terms(tag, true) {
			d.tf[t] += tagWeight
		}
	}
	return d
}

// Query is a parsed search request: each keyword must match (AND).
type Query struct {
	keywords []keyword
}

type keyword struct {
	text  string   // lowercased, for substring checks
	terms []string // all must occur in the document
}

func ParseQuery(q string) Query {
	var out Query
	for _, f := range strings.Fields(q) {
		terms := Terms(f, false)
		if len(terms) == 0 {
			continue
		}
		out.keywords = append(out.keywords, keyword{lower(f), terms})
	}
	return out
}

func (q Query) Empty() bool { return len(q.keywords) == 0 }

type Hit struct {
	Index   int
	Score   float64
	Snippet string
}

// Rank scores docs against q; docs maps to the caller's own positions. Only
// documents matching every keyword are returned, best first, ties by index.
func Rank(docs []*Doc, q Query) []Hit {
	if q.Empty() {
		return nil
	}
	df := map[string]int{}
	var total float64
	for _, d := range docs {
		total += d.length
	}
	for _, kw := range q.keywords {
		for _, t := range kw.terms {
			if _, ok := df[t]; ok {
				continue
			}
			n := 0
			for _, d := range docs {
				if d.tf[t] > 0 {
					n++
				}
			}
			df[t] = n
		}
	}
	avg := total / math.Max(1, float64(len(docs)))
	if avg == 0 {
		avg = 1
	}
	n := float64(len(docs))
	var hits []Hit
	for i, d := range docs {
		score, ok := 0.0, true
		for _, kw := range q.keywords {
			for _, t := range kw.terms {
				f := d.tf[t]
				if f == 0 {
					ok = false
					break
				}
				idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
				score += idf * f * (k1 + 1) / (f + k1*(1-b+b*d.length/avg))
			}
			if !ok {
				break
			}
			if strings.Contains(d.title, kw.text) {
				score += titleSubstring
			}
			if strings.Contains(d.body, kw.text) {
				score += bodySubstring
			}
		}
		if ok {
			hits = append(hits, Hit{Index: i, Score: score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	return hits
}

// Snippet returns about 2*radius runes of original around the first keyword
// found in the document, or the start of the body if none is present.
func Snippet(d *Doc, original string, q Query, radius int) string {
	at := -1
	for _, kw := range q.keywords {
		if i := strings.Index(d.body, kw.text); i >= 0 {
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

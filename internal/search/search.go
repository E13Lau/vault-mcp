// Package search ranks notes with BM25 over word and CJK n-gram terms. CJK text
// has no spaces, so each CJK character and each adjacent pair is a term.
package search

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Field weights add to a term's frequency, so a hit in a title counts as
// several body hits. Aliases name the note, so they weigh as much as the title.
const (
	titleWeight = 3.0
	aliasWeight = 3.0
	tagWeight   = 2.0
	pathWeight  = 2.0
	k1, b       = 1.2, 0.75
	// coverageBoost scales a score by the share of query terms (weighted by
	// IDF) that the note contains: score *= 1 + coverageBoost*coverage.
	coverageBoost = 1.0
	// priorWeight scales a score by the note's backlink count:
	// score *= 1 + priorWeight*ln(1+backlinks). Linked-to pages (concepts,
	// entities) are better answers than the raw sources they summarize.
	priorWeight = 1.0
	// phraseTitleBonus is added when a quoted phrase occurs in the title.
	phraseTitleBonus = 3.0
	// Bonuses for a keyword that appears whole in the title or body.
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

func NewDoc(title, path string, tags, aliases []string, body string) *Doc {
	d := &Doc{tf: map[string]float64{}, title: lower(title), body: lower(body)}
	for _, t := range Terms(body, true) {
		d.tf[t]++
		d.length++
	}
	add := func(text string, w float64) {
		for _, t := range Terms(text, true) {
			d.tf[t] += w
		}
	}
	add(title, titleWeight)
	add(path, pathWeight)
	for _, a := range aliases {
		add(a, aliasWeight)
	}
	for _, tag := range tags {
		add(tag, tagWeight)
	}
	return d
}

// Mode selects how query terms combine.
type Mode string

const (
	// Any matches notes containing at least one query term; more terms rank higher.
	Any Mode = "any"
	// All matches only notes containing every space-separated keyword.
	All Mode = "all"
)

// Query is a parsed search request.
type Query struct {
	mode     Mode
	terms    []string  // distinct index terms; any mode scores these
	keywords []keyword // all mode: every keyword must match
	phrases  []string  // lowercased quoted phrases; each must occur in the note
	nonEmpty bool      // the text held something other than space
}

type keyword struct {
	text  string   // lowercased, for substring checks
	terms []string // all must occur in the document
}

var quoted = regexp.MustCompile(`"([^"]+)"|“([^”]+)”`)

// ParseQuery splits q into quoted phrases and free text. In Any mode the free
// text loses stopwords and is split into terms; in All mode every
// space-separated keyword is kept whole, as in v0.1.
func ParseQuery(q string, mode Mode) Query {
	out := Query{mode: mode}
	free := quoted.ReplaceAllStringFunc(q, func(m string) string {
		p := strings.TrimSpace(lower(strings.Trim(m, `"“”`)))
		if p != "" {
			out.phrases = append(out.phrases, p)
		}
		return " "
	})
	out.nonEmpty = strings.TrimSpace(q) != ""
	seen := map[string]bool{}
	addTerms := func(ts []string) {
		for _, t := range ts {
			if !seen[t] {
				seen[t] = true
				out.terms = append(out.terms, t)
			}
		}
	}
	for _, p := range out.phrases {
		addTerms(queryTerms(p, false))
	}
	if mode == All {
		for _, f := range strings.Fields(free) {
			terms := queryTerms(f, false)
			if len(terms) == 0 {
				continue
			}
			out.keywords = append(out.keywords, keyword{lower(f), terms})
			addTerms(terms)
		}
		return out
	}
	free = stripSingles(stripPhrases(lower(free)))
	for _, f := range strings.Fields(free) {
		if utf8.RuneCountInString(f) >= 2 && !IsStopword(f) {
			out.keywords = append(out.keywords, keyword{text: f})
		}
	}
	var terms []string
	for _, t := range queryTerms(free, false) {
		if !IsStopword(t) {
			terms = append(terms, t)
		}
	}
	addTerms(terms)
	return out
}

// Blank reports that the query text held nothing to search for.
func (q Query) Blank() bool { return !q.nonEmpty }

// Empty reports that nothing is left to match, for instance because every
// word was a stopword.
func (q Query) Empty() bool { return len(q.terms) == 0 && len(q.phrases) == 0 && len(q.keywords) == 0 }

type Hit struct {
	Index   int
	Score   float64
	Snippet string
}

// Rank scores docs against q; docs maps to the caller's own positions.
// Any mode returns every note that contains a query term; All mode only notes
// that contain every keyword. Quoted phrases are required in both. Best
// first, ties by index.
func Rank(docs []*Doc, q Query, prior []float64) []Hit {
	if q.Empty() {
		return nil
	}
	df := make(map[string]int, len(q.terms))
	var total float64
	for _, d := range docs {
		total += d.length
	}
	for _, t := range q.terms {
		n := 0
		for _, d := range docs {
			if d.tf[t] > 0 {
				n++
			}
		}
		df[t] = n
	}
	avg := total / math.Max(1, float64(len(docs)))
	if avg == 0 {
		avg = 1
	}
	n := float64(len(docs))
	idf := make(map[string]float64, len(q.terms))
	var idfSum float64
	for _, t := range q.terms {
		idf[t] = math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
		idfSum += idf[t]
	}
	var hits []Hit
	for i, d := range docs {
		if !d.hasPhrases(q.phrases) {
			continue
		}
		if q.mode == All && !d.hasKeywords(q.keywords) {
			continue
		}
		var score, matched float64
		for _, t := range q.terms {
			f := d.tf[t]
			if f == 0 {
				continue
			}
			score += idf[t] * f * (k1 + 1) / (f + k1*(1-b+b*d.length/avg))
			matched += idf[t]
		}
		if idfSum > 0 {
			score *= 1 + coverageBoost*matched/idfSum
		}
		if prior != nil && score > 0 {
			score *= 1 + priorWeight*math.Log1p(prior[i])
		}
		for _, kw := range q.keywords {
			if strings.Contains(d.title, kw.text) {
				score += titleSubstring
			}
			if strings.Contains(d.body, kw.text) {
				score += bodySubstring
			}
		}
		for _, p := range q.phrases {
			if strings.Contains(d.title, p) {
				score += phraseTitleBonus
			}
		}
		if score > 0 || len(q.terms) == 0 {
			hits = append(hits, Hit{Index: i, Score: score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	return hits
}

func (d *Doc) hasPhrases(phrases []string) bool {
	for _, p := range phrases {
		if !strings.Contains(d.title, p) && !strings.Contains(d.body, p) {
			return false
		}
	}
	return true
}

func (d *Doc) hasKeywords(kws []keyword) bool {
	for _, kw := range kws {
		for _, t := range kw.terms {
			if d.tf[t] == 0 {
				return false
			}
		}
	}
	return true
}

// Snippet returns about 2*radius runes of original around the longest query
// text found in the body, or the start of the body if none is present.
func Snippet(d *Doc, original string, q Query, radius int) string {
	cands := append(append([]string{}, q.phrases...), q.terms...)
	for _, kw := range q.keywords {
		cands = append(cands, kw.text)
	}
	sort.SliceStable(cands, func(a, b int) bool { return len(cands[a]) > len(cands[b]) })
	at := -1
	for _, c := range cands {
		if i := strings.Index(d.body, c); i >= 0 {
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

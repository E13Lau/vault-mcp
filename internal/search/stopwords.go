package search

import (
	"sort"
	"strings"
)

// queryStopwords are question filler words dropped from OR queries. Without
// them a whole-sentence question would match nearly every note.
var queryStopwords = []string{
	"哪些", "什么", "怎么", "如何", "是否", "有没有", "没有", "库里", "笔记", "页面", "相关", "记录",
	"哪个", "哪里", "为什么", "怎样", "多少", "请问", "吗", "呢", "的", "了",
	"what", "which", "how", "why", "when", "where", "who", "does", "did", "the", "are", "was", "were", "any", "about", "note", "notes", "page", "pages",
}

var (
	stopSet    = map[string]bool{}
	stopPhrase []string // CJK stopwords of two or more characters, longest first
)

func init() {
	for _, w := range queryStopwords {
		stopSet[w] = true
		if len([]rune(w)) >= 2 && isCJK([]rune(w)[0]) {
			stopPhrase = append(stopPhrase, w)
		}
	}
	sort.Slice(stopPhrase, func(i, j int) bool { return len(stopPhrase[i]) > len(stopPhrase[j]) })
}

// IsStopword reports whether w is a query filler word.
func IsStopword(w string) bool { return stopSet[w] }

// stripPhrases replaces multi-character CJK stopwords with a space.
func stripPhrases(text string) string {
	for _, w := range stopPhrase {
		text = strings.ReplaceAll(text, w, " ")
	}
	return text
}

// stripSingles removes one-character CJK stopwords.
func stripSingles(text string) string {
	out := make([]rune, 0, len(text))
	for _, r := range text {
		if isCJK(r) && stopSet[string(r)] {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

// queryTerms turns free text into query terms. With strip it first drops
// stopwords, so a whole question reduces to its content words.
func queryTerms(text string, strip bool) []string {
	if strip {
		text = stripSingles(stripPhrases(lower(text)))
	}
	return Terms(text, false)
}

package search

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTermsChineseWords(t *testing.T) {
	got := Terms("iOS崩溃率 Go", false)
	for _, want := range []string{"ios", "go", "崩溃", "率"} {
		if !contains(got, want) {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	// A dictionary word is indexed as itself, not only as pairs.
	if !contains(Terms("独立开发", false), "独立") {
		t.Fatalf("独立开发 was not segmented: %v", Terms("独立开发", false))
	}
}

func TestTermsDocumentGrams(t *testing.T) {
	got := Terms("闪念", true)
	for _, want := range []string{"闪念", "闪", "念"} {
		if !contains(got, want) {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	if got := Terms("cat 4.4%", false); !reflect.DeepEqual(got, []string{"cat", "4.4"}) {
		t.Fatalf("numbers keep their decimal point: %v", got)
	}
}

func TestParseQueryDropsStopwords(t *testing.T) {
	q := ParseQuery("「猫传腹」相关笔记有哪些？")
	if q.Empty() {
		t.Fatal("sentence query became empty")
	}
	if !contains(keys(q), "传腹") {
		t.Fatalf("content word missing: %v", keys(q))
	}
	for _, stop := range []string{"相关", "笔记", "哪些", "有"} {
		if contains(q.keys, stop) {
			t.Fatalf("stopword %q kept in %v", stop, q.keys)
		}
	}
	if !ParseQuery("什么 怎么 是否 的 吗 如何 the of").Empty() {
		t.Fatal("pure stopword query is not empty")
	}
	if !ParseQuery("   「」 - ").Empty() {
		t.Fatal("punctuation-only query is not empty")
	}
}

func TestRankOrSemantics(t *testing.T) {
	docs := []*Doc{
		NewDoc("闪念笔记", "inbox/a.md", nil, "记录一个想法"),
		NewDoc("无关", "b.md", nil, "独立开发者的 iOS崩溃率 统计"),
		NewDoc("独立开发", "c.md", []string{"开发"}, "独立开发 路线"),
		NewDoc("其它", "d.md", nil, "今天只谈零点"),
	}
	// Every document that matches any term is a hit.
	if got := rank(docs, "闪念 零点"); !reflect.DeepEqual(got, []int{0, 3}) {
		t.Fatalf("OR: %v", got)
	}
	// A note matching both terms ranks above notes matching one.
	got := rank(docs, "独立开发 闪念")
	if len(got) != 3 || got[0] != 2 {
		t.Fatalf("multi-term first: %v", got)
	}
	// Title and path hits count extra.
	if got := rank(docs, "独立开发"); got[0] != 2 {
		t.Fatalf("title match first: %v", got)
	}
	if got := rank(docs, "iOS崩溃率"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("mixed script: %v", got)
	}
	if got := rank(docs, "   "); got != nil {
		t.Fatalf("empty: %v", got)
	}
}

func TestRankSentenceQuestion(t *testing.T) {
	docs := []*Doc{
		NewDoc("持续集成", "wiki/concepts/持续集成.md", nil, "iOS 应用做完后，在一处归档并转给同事试用，用 TestFlight。"),
		NewDoc("买车调研", "wiki/concepts/买车调研.md", nil, "预算十万，怎么选车，怎么谈价。"),
		NewDoc("闪念", "wiki/sources/闪念.md", nil, "今天想到一个点子。"),
	}
	got := rank(docs, "iOS 应用做完后，怎么在一处归档并转给同事试用？")
	if len(got) == 0 || got[0] != 0 {
		t.Fatalf("whole question: %v", got)
	}
}

func TestRankMultiTermOutranksSingle(t *testing.T) {
	docs := []*Doc{
		NewDoc("", "a.md", nil, "崩溃率 4.4%"),
		NewDoc("", "b.md", nil, "崩溃率 统计"),
		NewDoc("", "c.md", nil, "4.4% 的记录"),
	}
	got := rank(docs, "崩溃率 4.4%")
	if !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("both terms first: %v", got)
	}
}

func TestSnippet(t *testing.T) {
	body := "前面很多字。这里提到了 iOS崩溃率 的统计。后面还有内容"
	d := NewDoc("t", "t.md", nil, body)
	s := Snippet(d, body, ParseQuery("ios崩溃率"), 8)
	if !strings.Contains(s, "iOS崩溃率") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || utf8.RuneCountInString(s) > 18 {
		t.Fatalf("%q", s)
	}
}

func rank(docs []*Doc, q string) []int {
	var out []int
	for _, h := range Rank(docs, ParseQuery(q)) {
		out = append(out, h.Index)
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func keys(q Query) []string { return q.keys }

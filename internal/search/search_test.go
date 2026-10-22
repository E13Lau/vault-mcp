package search

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTerms(t *testing.T) {
	if got := Terms("iOS崩溃率 Go", false); !reflect.DeepEqual(got, []string{"ios", "崩溃", "溃率", "go"}) {
		t.Fatalf("query terms: %v", got)
	}
	if got := Terms("闪", false); !reflect.DeepEqual(got, []string{"闪"}) {
		t.Fatalf("single: %v", got)
	}
	if got := Terms("闪念", true); !reflect.DeepEqual(got, []string{"闪", "念", "闪念"}) {
		t.Fatalf("doc terms: %v", got)
	}
}

func rankMode(docs []*Doc, q string, mode Mode) []int {
	var out []int
	for _, h := range Rank(docs, ParseQuery(q, mode), nil) {
		out = append(out, h.Index)
	}
	return out
}

func rank(docs []*Doc, q string) []int { return rankMode(docs, q, Any) }

func fixture() []*Doc {
	return []*Doc{
		NewDoc("闪念笔记", "inbox/a.md", nil, nil, "记录一个想法"),
		NewDoc("无关", "b.md", nil, nil, "独立开发者的 iOS崩溃率 统计"),
		NewDoc("独立开发", "c.md", []string{"开发"}, nil, "独立开发 路线"),
		NewDoc("其它", "d.md", nil, nil, "独立 开发 分开写"),
	}
}

func TestStopwords(t *testing.T) {
	for _, w := range []string{"哪些", "什么", "怎么", "如何", "是否", "吗", "呢", "的", "了", "有没有", "库里", "笔记", "页面", "相关", "记录"} {
		if !IsStopword(w) {
			t.Errorf("%q must be a stopword", w)
		}
	}
	if IsStopword("崩溃") {
		t.Error("content word must not be a stopword")
	}
	q := ParseQuery("库里有没有哪些笔记记录了崩溃率的相关页面吗", Any)
	if !reflect.DeepEqual(q.terms, []string{"崩溃", "溃率"}) {
		t.Fatalf("terms after stopwords: %v", q.terms)
	}
	if !ParseQuery("哪些笔记 相关记录吗", Any).Empty() {
		t.Fatal("all-stopword query must be empty")
	}
	if rank(fixture(), "哪些笔记 相关记录吗") != nil {
		t.Fatal("all-stopword query must return nothing")
	}
}

func TestRankAnyMatchesLongQuestion(t *testing.T) {
	got := rank(fixture(), "库里有没有哪些笔记提到了独立开发者的 iOS崩溃率 吗？")
	if len(got) == 0 || got[0] != 1 {
		t.Fatalf("question must find the matching note first: %v", got)
	}
	if got := rankMode(fixture(), "库里有没有哪些笔记提到了独立开发者的 iOS崩溃率 吗？", All); len(got) != 0 {
		t.Fatalf("all mode keeps the strict rule: %v", got)
	}
}

func TestRankMoreTermsRankHigher(t *testing.T) {
	docs := []*Doc{
		NewDoc("a", "a.md", nil, nil, "alpha only"),
		NewDoc("b", "b.md", nil, nil, "alpha beta gamma"),
		NewDoc("c", "c.md", nil, nil, "alpha beta"),
		NewDoc("d", "d.md", nil, nil, "unrelated"),
	}
	if got := rank(docs, "alpha beta gamma"); !reflect.DeepEqual(got, []int{1, 2, 0}) {
		t.Fatalf("order: %v", got)
	}
}

func TestRankChineseAndModes(t *testing.T) {
	docs := fixture()
	if got := rank(docs, "闪念"); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("闪念: %v", got)
	}
	if got := rank(docs, "iOS崩溃率"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("iOS崩溃率: %v", got)
	}
	got := rank(docs, "独立开发")
	if len(got) < 2 || got[0] != 2 {
		t.Fatalf("title match must rank first: %v", got)
	}
	if got := rankMode(docs, "ios 路线", All); len(got) != 0 {
		t.Fatalf("AND: %v", got)
	}
	if got := rankMode(docs, "ios 路线", Any); !reflect.DeepEqual(got, []int{1, 2}) && !reflect.DeepEqual(got, []int{2, 1}) {
		t.Fatalf("OR: %v", got)
	}
	if got := rankMode(docs, "独立开发 路线", All); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("AND both: %v", got)
	}
	if got := rank(docs, "崩"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("single char: %v", got)
	}
}

func TestEmptyQuery(t *testing.T) {
	for _, mode := range []Mode{Any, All} {
		q := ParseQuery("   ", mode)
		if !q.Blank() || !q.Empty() {
			t.Fatalf("%s: blank query", mode)
		}
		if got := Rank(fixture(), q, nil); got != nil {
			t.Fatalf("%s: %v", mode, got)
		}
	}
}

func TestPhrase(t *testing.T) {
	docs := []*Doc{
		NewDoc("a", "a.md", nil, nil, "red fox jumps"),
		NewDoc("b", "b.md", nil, nil, "fox is red"),
	}
	if got := rank(docs, `"red fox"`); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("phrase: %v", got)
	}
	if got := rank(docs, `"red fox" jumps`); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("phrase and term: %v", got)
	}
	if got := rank(docs, `"blue fox"`); len(got) != 0 {
		t.Fatalf("absent phrase: %v", got)
	}
}

func TestTitleAliasTagWeight(t *testing.T) {
	body := "mentions 彩虹桥 once in passing"
	docs := []*Doc{
		NewDoc("plain", "p.md", nil, nil, body),
		NewDoc("plain", "q.md", []string{"彩虹桥"}, nil, body),
		NewDoc("plain", "r.md", nil, []string{"彩虹桥"}, body),
		NewDoc("彩虹桥", "s.md", nil, nil, body),
	}
	got := rank(docs, "彩虹桥")
	if len(got) != 4 || got[3] != 0 {
		t.Fatalf("unboosted note must rank last: %v", got)
	}
	if got[0] != 3 && got[0] != 2 {
		t.Fatalf("title or alias must rank first: %v", got)
	}
}

func TestBacklinkPrior(t *testing.T) {
	docs := []*Doc{
		NewDoc("a", "a.md", nil, nil, "topic"),
		NewDoc("b", "b.md", nil, nil, "topic"),
	}
	hits := Rank(docs, ParseQuery("topic", Any), []float64{0, 5})
	if hits[0].Index != 1 {
		t.Fatalf("linked note must win a tie: %v", hits)
	}
}

func TestSnippet(t *testing.T) {
	body := "前面很多字。这里提到了 iOS崩溃率 的统计。后面还有内容"
	d := NewDoc("t", "t.md", nil, nil, body)
	s := Snippet(d, body, ParseQuery("ios崩溃率", Any), 8)
	if !strings.Contains(s, "崩溃") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || utf8.RuneCountInString(s) > 18 {
		t.Fatalf("%q", s)
	}
}

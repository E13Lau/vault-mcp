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

func rank(docs []*Doc, q string) []int {
	var out []int
	for _, h := range Rank(docs, ParseQuery(q)) {
		out = append(out, h.Index)
	}
	return out
}

func TestRankChineseAndAND(t *testing.T) {
	docs := []*Doc{
		NewDoc("闪念笔记", "inbox/a.md", nil, "记录一个想法"),
		NewDoc("无关", "b.md", nil, "独立开发者的 iOS崩溃率 统计"),
		NewDoc("独立开发", "c.md", []string{"开发"}, "独立开发 路线"),
		NewDoc("其它", "d.md", nil, "独立 开发 分开写"),
	}
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
	if got := rank(docs, "ios 路线"); len(got) != 0 {
		t.Fatalf("AND: %v", got)
	}
	if got := rank(docs, "崩"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("single char: %v", got)
	}
	if got := rank(docs, "   "); got != nil {
		t.Fatalf("empty: %v", got)
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

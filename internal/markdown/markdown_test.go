package markdown

import (
	"reflect"
	"strings"
	"testing"
)

func TestFrontmatterPreservesUntouchedBytes(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		text := strings.ReplaceAll("---\n# title comment\ntitle:    \"中文标题\" # keep spacing\nsources:\n  - \"[[来源一]]\" # source comment\n  - '[[来源二]]'\nsummary: 'old' # summary comment\n\n# following field\ntags: [开发,  iOS]\nnested: {a: 1, b: '二'}\nmultiline: |\n  keep this\n  and this\n---\n\n# 正文\n正文不变。\n", "\n", newline)
		got, err := Update(text, map[string]any{"summary": "新摘要"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(text, "summary: 'old'", "summary: '新摘要'", 1)
		if got != want {
			t.Fatalf("untouched bytes changed:\ngot %q\nwant %q", got, want)
		}
		doc, err := Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Fields["summary"] != "新摘要" {
			t.Fatal("summary not updated")
		}
	}
}

func TestFrontmatterEditsAndValidation(t *testing.T) {
	indented := "---\n  first: 'old'\n  second:    keep\n---\nbody"
	indentedResult, err := Update(indented, map[string]any{"first": "新值", "third": "new"}, nil)
	if err != nil || !strings.Contains(indentedResult, "  second:    keep\n") {
		t.Fatalf("indented mapping: %q %v", indentedResult, err)
	}
	text := "---\ntitle: \"keep\"\n# old field\nremove: 1\n# retained comment\nlast: '保留'\n---\nBODY\r\n"
	got, err := Update(text, map[string]any{"added": "new", "title": "changed"}, []string{"remove"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# retained comment\nlast: '保留'\n") || !strings.HasSuffix(got, "BODY\r\n") || strings.Contains(got, "remove:") {
		t.Fatalf("edit damaged source: %q", got)
	}
	if strings.Index(got, "last:") > strings.Index(got, "added:") {
		t.Fatal("new field changed existing order")
	}
	for _, input := range []string{"---\na: [oops\n---\nbody", "---\na: 1\na: 2\n---\nbody", "---\n- list\n---\nbody", "---\na: 1\n", "---\na: .nan\n---\n"} {
		if _, err := Update(input, map[string]any{"x": "y"}, nil); err == nil {
			t.Errorf("accepted invalid frontmatter %q", input)
		}
	}
	if _, err := Update(text, map[string]any{"title": "x"}, []string{"title"}); err == nil {
		t.Fatal("conflicting edits accepted")
	}
	for _, input := range []string{"body", "---\n---\nbody", "---\n# empty header\n---\nbody"} {
		updated, err := Update(input, map[string]any{"tags": []string{"中文"}}, nil)
		if err != nil {
			t.Fatalf("add to empty header: %v", err)
		}
		doc, err := Parse(updated)
		if err != nil || !reflect.DeepEqual(FrontmatterTags(doc.Fields), []string{"中文"}) {
			t.Fatalf("tags: %+v %v", doc, err)
		}
	}
	anchored := "---\nbase: &base {x: one}\nother: *base\n---\nBODY"
	if _, err := Update(anchored, nil, []string{"base"}); err == nil {
		t.Fatal("removal left a dangling alias")
	}
	if _, err := Update("---\n{a: 1, b: 2}\n---\nBODY", map[string]any{"a": 3}, nil); err == nil {
		t.Fatal("flow root must be rejected without rewriting untouched fields")
	}
}

func TestOutlineSummaryAndLinks(t *testing.T) {
	text := "---\ntitle: Test\n---\n# 主标题\n\n```md\n# fake\n[[假链接]] #假标签\n```\n\n## 第二节\n\n第一个 **正文** 段落。\n第二行。\n\nSetext\n------\n\n[[页面#标题|别名]] ![[目录/页面]] [[#本页]] `[[代码]] #代码` <!-- [[注释]] --> \\[[转义]]\n#开发/iOS #中文 #2026 #tag #tag [[页面#不是标签]]\n"
	doc, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	headings := Outline(doc)
	if len(headings) != 3 || headings[0].Text != "主标题" || headings[0].Line != 4 || headings[1].Level != 2 || headings[2].Text != "Setext" {
		t.Fatalf("outline: %+v", headings)
	}
	if got := BodySummary(doc); got != "第一个 正文 段落。 第二行。" {
		t.Fatalf("body summary: %q", got)
	}
	links := Links(doc.Body)
	if len(links) != 4 || links[0].Target != "页面" || links[0].Heading != "标题" || links[0].Alias != "别名" || !links[1].Embedded || links[2].Target != "" {
		t.Fatalf("links: %+v", links)
	}
	if got := Tags(doc); !reflect.DeepEqual(got, []string{"tag", "中文", "开发/iOS"}) {
		t.Fatalf("tags: %v", got)
	}
	doc.Body = strings.Repeat("中", 130)
	if len([]rune(BodySummary(doc))) != 120 {
		t.Fatal("body limit is not Unicode-aware")
	}
}

func TestWikilinkCandidates(t *testing.T) {
	paths := []string{"a/页面.md", "b/页面.md", "b/other.md", "folder/sub/unique.md"}
	for _, tc := range []struct {
		input, source string
		want          []string
	}{
		{"[[页面|alias]]", "", []string{"a/页面.md", "b/页面.md"}},
		{"![[b/页面#标题]]", "", []string{"b/页面.md"}},
		{"[[sub/unique]]", "", []string{"folder/sub/unique.md"}},
		{"[[./页面]]", "b/other.md", []string{"b/页面.md"}},
		{"[[../a/页面]]", "b/other.md", []string{"a/页面.md"}},
		{"[[#标题]]", "b/other.md", []string{"b/other.md"}},
		{"[[../../outside]]", "b/other.md", []string{}},
		{"[[missing]]", "", []string{}},
	} {
		link, err := ParseLink(tc.input)
		if err != nil {
			t.Fatal(err)
		}
		if got := Resolve(link, tc.source, paths); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %v want %v", tc.input, got, tc.want)
		}
	}
	for _, separator := range []string{"—", "-", ":", "："} {
		rows := IndexSummaries("- [[页面]] " + separator + " 摘要\n")
		if len(rows) != 1 || rows[0].Text != "摘要" {
			t.Fatalf("separator %q: %+v", separator, rows)
		}
	}
}

func TestLinkDestinationsDoNotCreateMetadata(t *testing.T) {
	doc, err := Parse("[section](#not-a-tag) [remote](https://example.com/[[not-a-note]]) ![#not-an-image-tag](image.png)\n\n#real-tag [[real-note]]\n\n[reference][id]\n\n[id]: #not-a-reference-tag\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := Tags(doc); !reflect.DeepEqual(got, []string{"real-tag"}) {
		t.Fatalf("destinations became tags: %v", got)
	}
	links := Links(doc.Body)
	if len(links) != 1 || links[0].Target != "real-note" {
		t.Fatalf("destinations became wikilinks: %+v", links)
	}
}

func TestFrontmatterDatesKeepOriginalFormat(t *testing.T) {
	text := "---\ncreated: 2026-09-25\nupdated: 2026-10-09\nname: x\n---\nbody\n"
	doc, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Fields["created"] != "2026-09-25" {
		t.Fatalf("created: %#v", doc.Fields["created"])
	}
	got, err := Update(text, map[string]any{"updated": "2026-10-16"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "---\ncreated: 2026-09-25\nupdated: 2026-10-16\nname: x\n---\nbody\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// A non-date string replacing a date stays a string.
	got, err = Update(text, map[string]any{"updated": "soon"}, nil)
	if err != nil || !strings.Contains(got, "updated: soon\n") {
		t.Fatalf("got %q %v", got, err)
	}
	// A quoted string value stays quoted.
	got, err = Update("---\nupdated: \"2026-10-09\"\n---\n", map[string]any{"updated": "2026-10-16"}, nil)
	if err != nil || !strings.Contains(got, "updated: \"2026-10-16\"\n") {
		t.Fatalf("got %q %v", got, err)
	}
}

package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSummariesDirectoryAndRefresh(t *testing.T) {
	v, dir := fixture(t, []string{"private/**"}, false)
	ctx := context.Background()
	put(t, dir, "index.md", "- [[alpha]] — index alpha\n- [[beta]]： index beta\n- [[nested]] - nested summary\n")
	put(t, dir, "wiki/concepts/alpha.md", "---\ntitle: Alpha title\nsummary: frontmatter wins\ntype: concept\ntags: [开发]\n---\n# Heading\nbody alpha\n")
	put(t, dir, "wiki/concepts/beta.md", "---\ndescription: ''\n---\n# B\nbody beta\n")
	put(t, dir, "wiki/concepts/body.md", "# Title\n\n"+strings.Repeat("中", 130)+"\n")
	put(t, dir, "wiki/concepts/sub/nested.md", "nested body\n")
	put(t, dir, "private/secret.md", "secret")
	if err := os.MkdirAll(filepath.Join(dir, "wiki/concepts/empty"), 0755); err != nil {
		t.Fatal(err)
	}
	paths := []string{"wiki/concepts/alpha.md", "wiki/concepts/beta.md", "wiki/concepts/body.md", "../outside.md"}
	got, err := v.Summaries(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Summary != "frontmatter wins" || got[0].Source != "frontmatter" || got[1].Summary != "index beta" || got[1].Source != "index" || len([]rune(got[2].Summary)) != 120 || got[2].Source != "body" || got[3].Error == "" {
		t.Fatalf("summaries: %+v", got)
	}
	all := []DirectoryEntry{}
	cursor := ""
	for {
		page, err := v.ListDirectory(ctx, DirectoryOptions{Path: "wiki/concepts", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Entries...)
		if page.Total != 5 {
			t.Fatalf("total=%d", page.Total)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(all) != 5 || all[0].Title != "Alpha title" || all[0].Summary != "frontmatter wins" || all[1].Source != "index" || all[3].NoteCount == nil || *all[3].NoteCount != 0 || *all[4].NoteCount != 1 {
		t.Fatalf("directory: %+v", all)
	}
	include := false
	page, err := v.ListDirectory(ctx, DirectoryOptions{Path: "wiki/concepts", Depth: 2, IncludeSummary: &include})
	if err != nil || page.Total != 6 {
		t.Fatalf("depth: %+v %v", page, err)
	}
	for _, row := range page.Entries {
		if row.Summary != "" || row.Source != "" {
			t.Fatal("summary false ignored")
		}
	}
	first, err := v.ListDirectory(ctx, DirectoryOptions{Path: "wiki/concepts", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "index.md", "- [[beta]] — refreshed\n")
	stamp := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "index.md"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ListDirectory(ctx, DirectoryOptions{Path: "wiki/concepts", Limit: 1, Cursor: first.NextCursor}); err == nil {
		t.Fatal("stale cursor accepted")
	}
	got, err = v.Summaries(ctx, []string{paths[1]})
	if err != nil || got[0].Summary != "refreshed" {
		t.Fatalf("mtime refresh: %+v %v", got, err)
	}
	for _, opts := range []DirectoryOptions{{Path: "../"}, {Path: ".git"}, {Path: "private"}, {Depth: -1}, {Limit: 501}, {Cursor: "broken"}} {
		if _, err := v.ListDirectory(ctx, opts); err == nil {
			t.Errorf("invalid listing accepted: %+v", opts)
		}
	}
	put(t, dir, "guide/custom.md", "- [[beta]] : custom summary\n")
	if err := v.SetIndexFile("guide/custom.md"); err != nil {
		t.Fatal(err)
	}
	got, err = v.Summaries(ctx, []string{paths[1]})
	if err != nil || got[0].Summary != "custom summary" {
		t.Fatalf("custom index: %+v %v", got, err)
	}
}

func TestFrontmatterTagsAndConcurrentEdits(t *testing.T) {
	v, dir := fixture(t, nil, false)
	ctx := context.Background()
	original := "---\ntitle:   '中文' # unchanged\nsources:\n  - \"[[来源]]\"\ntags: [旧标签]\n---\n# H\n正文 #inline #中文\n"
	put(t, dir, "note.md", original)
	if err := v.UpdateFrontmatter(ctx, "note.md", map[string]any{"summary": "摘要"}, nil); err != nil {
		t.Fatal(err)
	}
	text, err := v.ReadNote("note.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "title:   '中文' # unchanged\nsources:\n  - \"[[来源]]\"\n") {
		t.Fatalf("untouched format changed: %q", text)
	}
	tags, err := v.ManageTags(ctx, "note.md", "add", []string{"#开发/iOS", "新标签"})
	if err != nil || !reflect.DeepEqual(tags, []string{"开发/iOS", "新标签", "旧标签"}) {
		t.Fatalf("tags: %v %v", tags, err)
	}
	if _, err := v.ManageTags(ctx, "note.md", "remove", []string{"旧标签"}); err != nil {
		t.Fatal(err)
	}
	all, err := v.AllTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, tag := range all {
		found[tag.Tag] = tag.Count
	}
	if found["inline"] != 1 || found["中文"] != 1 || found["旧标签"] != 0 || found["开发/iOS"] != 1 {
		t.Fatalf("aggregate tags: %+v", all)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := v.UpdateFrontmatter(ctx, "note.md", map[string]any{fmt.Sprintf("field%d", i): i}, nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	fields, err := v.GetFrontmatter("note.md")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if fields[fmt.Sprintf("field%d", i)] != i {
			t.Errorf("field %d lost", i)
		}
	}
	put(t, dir, "bad.md", "---\ntags: [broken\n---\nBODY")
	before, _ := os.ReadFile(filepath.Join(dir, "bad.md"))
	if err := v.UpdateFrontmatter(ctx, "bad.md", map[string]any{"title": "new"}, nil); err == nil {
		t.Fatal("invalid YAML overwritten")
	}
	if _, err := v.ManageTags(ctx, "bad.md", "add", []string{"tag"}); err == nil {
		t.Fatal("invalid YAML tags overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "bad.md"))
	if string(before) != string(after) {
		t.Fatal("rejected update modified source")
	}
}

func TestIndexedBacklinksSkipUnreadableAndRefreshMoves(t *testing.T) {
	v, dir := fixture(t, nil, false)
	ctx := context.Background()
	put(t, dir, "a/Target.md", "target")
	put(t, dir, "b/Target.md", "other target")
	put(t, dir, "links.md", "[[Target|alias]] ![[a/Target#H]] `[[a/Target]]` [[bad]]\n")
	put(t, dir, "bad.md", string([]byte{255, 254}))
	f, err := os.Create(filepath.Join(dir, "large.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxNoteBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	resolution, err := v.Resolve(ctx, "[[Target#H|A]]", "")
	if err != nil || len(resolution.Candidates) != 2 || resolution.Alias != "A" || resolution.Heading != "H" {
		t.Fatalf("resolution: %+v %v", resolution, err)
	}
	info, err := v.NotesInfo(ctx, []string{"a/Target.md"})
	if err != nil || !reflect.DeepEqual(info[0].Backlinks, []string{"links.md"}) {
		t.Fatalf("backlinks: %+v %v", info, err)
	}
	refs, err := v.MoveNote(ctx, "a/Target.md", "archive/renamed.md")
	if err != nil || !reflect.DeepEqual(refs, []string{"links.md"}) {
		t.Fatalf("move blocked by unreadable note: %v %v", refs, err)
	}
	resolution, err = v.Resolve(ctx, "[[a/Target]]", "")
	if err != nil || len(resolution.Candidates) != 0 {
		t.Fatal("old target retained in index")
	}
	resolution, err = v.Resolve(ctx, "[[bad]]", "")
	if err != nil || len(resolution.Candidates) != 1 || resolution.Candidates[0].Error == "" {
		t.Fatal("unreadable link target was lost")
	}
	refs, err = v.MoveNote(ctx, "bad.md", "archive/bad.md")
	if err != nil || !reflect.DeepEqual(refs, []string{"links.md"}) {
		t.Fatalf("unreadable target backlinks: %v %v", refs, err)
	}
	stats, err := v.Stats(ctx)
	if err != nil || stats.Notes != 5 || len(stats.Skipped) != 2 || stats.Directories != 3 || stats.Wikilinks != 3 {
		t.Fatalf("stats: %+v %v", stats, err)
	}
}

func TestLinesAndReadOnlyMarkdown(t *testing.T) {
	v, dir := fixture(t, nil, true)
	put(t, dir, "note.md", "one\r\ntwo\r\n三")
	got, err := v.ReadLines("note.md", 2, 20)
	if err != nil || got.Content != "two\r\n三" || got.TotalLines != 3 || got.EndLine != 3 {
		t.Fatalf("lines: %+v %v", got, err)
	}
	for _, r := range [][2]int{{0, 1}, {3, 2}, {4, 5}, {1, 2001}} {
		if _, err := v.ReadLines("note.md", r[0], r[1]); err == nil {
			t.Errorf("invalid range accepted %v", r)
		}
	}
	if err := v.UpdateFrontmatter(context.Background(), "note.md", map[string]any{"title": "new"}, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatal("read-only update accepted")
	}
	if _, err := v.ManageTags(context.Background(), "note.md", "add", []string{"x"}); !errors.Is(err, ErrReadOnly) {
		t.Fatal("read-only tags accepted")
	}
}

func TestConcurrentCatalogReadsAndMetadataWrites(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "note.md", "---\ntitle: start\nsummary: start\n---\nbody")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("version-%d", i)
			if err := v.UpdateFrontmatter(ctx, "note.md", map[string]any{"title": marker, "summary": marker}, nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 5; n++ {
				notes, err := v.NotesInfo(ctx, []string{"note.md"})
				if err != nil {
					t.Error(err)
					return
				}
				if notes[0].Error != "" || notes[0].Title != notes[0].Frontmatter["summary"] {
					t.Error("reader observed mixed metadata versions")
				}
			}
		}()
	}
	wg.Wait()
}

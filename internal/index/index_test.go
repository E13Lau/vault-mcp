package index

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"testing"
	"time"
)

type fileInfo struct {
	name     string
	size     int64
	modified time.Time
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0644 }
func (i fileInfo) ModTime() time.Time { return i.modified }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

type fakeSource struct {
	text  map[string]string
	times map[string]time.Time
	reads map[string]int
	bad   map[string]bool
}

func (f *fakeSource) NotePaths() ([]string, error) {
	out := []string{}
	for p := range f.text {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
func (f *fakeSource) StatNote(p string) (fs.FileInfo, error) {
	return fileInfo{p, int64(len(f.text[p])), f.times[p]}, nil
}
func (f *fakeSource) ReadNote(p string) (string, error) {
	f.reads[p]++
	if f.bad[p] {
		return "", errors.New("invalid UTF-8")
	}
	return f.text[p], nil
}

func TestRefreshCachesChangedFilesAndSummaryParsing(t *testing.T) {
	f := &fakeSource{text: map[string]string{"index.md": "- [[a]] — old summary\n", "a.md": "# A\nbody", "bad.md": "bad"}, times: map[string]time.Time{}, reads: map[string]int{}, bad: map[string]bool{"bad.md": true}}
	c := New(f)
	ctx := context.Background()
	first, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Summaries("index.md")["a.md"]; got != "old summary" {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(first.Backlinks["a.md"], []string{"index.md"}) || !reflect.DeepEqual(first.Skipped, []string{"bad.md"}) {
		t.Fatalf("snapshot: %+v", first)
	}
	again, err := c.Snapshot(ctx)
	if err != nil || again != first {
		t.Fatal("unchanged snapshot was rebuilt")
	}
	for p, n := range f.reads {
		if n != 1 {
			t.Fatalf("%s reread %d times", p, n)
		}
	}
	f.text["index.md"] = "- [[a]] — new summary\n"
	f.times["index.md"] = time.Unix(1, 0)
	updated, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summaries("index.md")["a.md"] != "new summary" || first.Summaries("index.md")["a.md"] != "old summary" {
		t.Fatal("cache stale or previous snapshot mutated")
	}
	if f.reads["a.md"] != 1 || f.reads["bad.md"] != 1 || f.reads["index.md"] != 2 {
		t.Fatalf("unexpected reparses: %v", f.reads)
	}
	f.bad["bad.md"] = false
	f.times["bad.md"] = time.Unix(2, 0)
	recovered, err := c.Snapshot(ctx)
	if err != nil || len(recovered.Skipped) != 0 {
		t.Fatal("fixed file did not re-enter index")
	}
	delete(f.text, "a.md")
	deleted, err := c.Snapshot(ctx)
	if err != nil || deleted.ByPath["a.md"] != nil || len(deleted.Backlinks["a.md"]) != 0 {
		t.Fatal("deleted target retained")
	}
	// Service writes explicitly invalidate even when external metadata is equal.
	f.text["bad.md"] = "new"
	c.Forget("bad.md")
	forgotten, err := c.Snapshot(ctx)
	if err != nil || forgotten.ByPath["bad.md"].Document.Body != "new" {
		t.Fatal("write invalidation ignored")
	}
}

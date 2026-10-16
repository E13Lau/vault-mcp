package vault

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T, exclude []string, readOnly bool) (*Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v, err := Open(dir, exclude, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, dir
}

func put(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBoundaryAcrossOperations(t *testing.T) {
	v, dir := fixture(t, []string{"raw/assets/**", "private"}, false)
	outside := t.TempDir()
	put(t, outside, "secret.md", "outside")
	put(t, dir, "safe.md", "safe")
	put(t, dir, ".git/config.md", "hidden")
	put(t, dir, ".obsidian/config.md", "hidden")
	put(t, dir, "raw/assets/secret.md", "excluded")
	put(t, dir, "private/deep/secret.md", "excluded")
	for name, target := range map[string]string{"escape": outside, "alias": ".git", "excluded": "raw/assets", "loop": "loop", "safe-link.md": "safe.md", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for _, bad := range []string{"../", ".", ".git/config", ".obsidian/", "../secret.md", "a/../../secret.md", dir + "/safe.md", filepath.Join(outside, "secret.md"), "../" + filepath.Base(dir) + "-secret/secret.md", ".git/config.md", ".obsidian/config.md", "a/.secret.md", ".env.md", "raw/assets/secret.md", "private/deep/secret.md", "escape/secret.md", "alias/config.md", "excluded/secret.md", "safe-link.md", "loop/x.md", "dangling/new.md", "a\\..\\secret.md", "C:/secret.md", "bad\x00.md"} {
		t.Run(bad, func(t *testing.T) {
			operations := []func() error{
				func() error { _, err := v.ReadNote(bad); return err },
				func() error { return v.WriteNote(ctx, bad, "new", "overwrite") },
				func() error { return v.WriteNote(ctx, bad, "new", "append") },
				func() error { return v.PatchNote(ctx, bad, "safe", "new") },
				func() error { return v.DeleteNote(ctx, bad) },
				func() error { return v.MoveFile(ctx, "safe.md", bad) },
				func() error { return v.MoveFile(ctx, bad, "moved.md") },
				func() error { _, err := v.MoveNote(ctx, "safe.md", bad); return err },
				func() error { _, err := v.MoveNote(ctx, bad, "moved.md"); return err },
			}
			for i, op := range operations {
				err := op()
				if !errors.Is(err, ErrDenied) {
					t.Errorf("operation %d error = %v, want denied", i, err)
				}
				if err != nil && (strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), outside)) {
					t.Errorf("error leaks absolute path: %v", err)
				}
			}
		})
	}
	if text, err := v.ReadNote("safe.md"); err != nil || text != "safe" {
		t.Fatalf("safe file changed: %q %v", text, err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret.md")); err != nil || string(b) != "outside" {
		t.Fatal("outside file changed")
	}
	files, err := v.NotePaths()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{"safe.md"}) {
		t.Fatalf("walk exposed denied entries: %v", files)
	}
}

func TestWritesAreAtomicAndSerialized(t *testing.T) {
	v, dir := fixture(t, nil, false)
	ctx := context.Background()
	if err := v.WriteNote(ctx, "new/deep/log.md", "start\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteNote(ctx, "new/deep/log.md", "header\n", "prepend"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := v.WriteNote(ctx, "new/deep/log.md", fmt.Sprintf("<%d>%s</%d>\n", i, strings.Repeat("闪念", 1024), i), "append"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	text, err := v.ReadNote("./new/deep/log.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "header\nstart\n") {
		t.Fatalf("wrong prefix")
	}
	for i := 0; i < 10; i++ {
		chunk := fmt.Sprintf("<%d>%s</%d>\n", i, strings.Repeat("闪念", 1024), i)
		if strings.Count(text, chunk) != 1 {
			t.Errorf("chunk %d lost or interleaved", i)
		}
	}
	for _, op := range []func() error{
		func() error { return v.PatchNote(ctx, "new/deep/log.md", "闪念", "bad") },
		func() error { return v.PatchNote(ctx, "new/deep/log.md", "missing", "bad") },
		func() error { return v.PatchNote(ctx, "new/deep/log.md", "", "bad") },
		func() error { return v.WriteNote(ctx, "new/deep/log.md", "bad", "invalid") },
		func() error { return v.WriteNote(ctx, "new/deep/log.md", strings.Repeat("x", MaxNoteBytes+1), "") },
	} {
		if err := op(); err == nil {
			t.Error("expected rejected write")
		}
	}
	after, _ := v.ReadNote("new/deep/log.md")
	if after != text {
		t.Fatal("rejected write changed original")
	}
	if err := os.Chmod(filepath.Join(dir, "new/deep/log.md"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := v.PatchNote(ctx, "new/deep/log.md", "header", "revised"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "new/deep/log.md"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode not preserved: %v %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "new/deep"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}

func TestMoveAndDelete(t *testing.T) {
	v, dir := fixture(t, nil, false)
	put(t, dir, "wiki/概念.md", "# 概念")
	put(t, dir, "index.md", "[[概念|别名]] [[wiki/概念#标题]] ![[概念]]")
	put(t, dir, "wiki/relative.md", "[[概念]]")
	put(t, dir, "other.md", "[[不相关]]")
	refs, err := v.MoveNote(context.Background(), "wiki/概念.md", "archive/概念.md")
	if err != nil || !reflect.DeepEqual(refs, []string{"index.md", "wiki/relative.md"}) {
		t.Fatalf("refs=%v err=%v", refs, err)
	}
	if _, err := v.ReadNote("wiki/概念.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("source still exists")
	}
	put(t, dir, "asset.bin", string([]byte{0, 1, 2, 255}))
	if err := v.MoveFile(context.Background(), "asset.bin", "assets/file.bin"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "assets/file.bin"))
	if err != nil || string(b) != string([]byte{0, 1, 2, 255}) {
		t.Fatal("binary corrupted")
	}
	if _, err := v.MoveNote(context.Background(), "other.md", "archive/概念.md"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("overwrite not rejected: %v", err)
	}
	if err := v.DeleteNote(context.Background(), "archive/概念.md"); err != nil {
		t.Fatal(err)
	}
	if err := v.DeleteNote(context.Background(), "archive/概念.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing delete should fail")
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := v.DeleteNote(context.Background(), "directory.md"); err == nil {
		t.Fatal("deleted a directory")
	}
}

func TestReadOnlyAndCancellation(t *testing.T) {
	v, dir := fixture(t, nil, true)
	put(t, dir, "note.md", "old")
	ctx := context.Background()
	for _, op := range []func() error{
		func() error { return v.WriteNote(ctx, "note.md", "new", "") },
		func() error { return v.PatchNote(ctx, "note.md", "old", "new") },
		func() error { return v.DeleteNote(ctx, "note.md") },
		func() error { return v.MoveFile(ctx, "note.md", "new.md") },
		func() error { _, err := v.MoveNote(ctx, "note.md", "new.md"); return err },
	} {
		if err := op(); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("write in read-only mode: %v", err)
		}
	}
	writable, _ := fixture(t, nil, false)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := writable.WriteNote(canceled, "x.md", "new", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestGuidePriorityRefreshAndTruncation(t *testing.T) {
	v, dir := fixture(t, nil, false)
	for _, name := range []string{"README.md", "AGENTS.md", "CLAUDE.md"} {
		put(t, dir, name, name)
		g, err := v.Guide()
		if err != nil || g.Path != name {
			t.Fatalf("guide priority: %+v %v", g, err)
		}
	}
	full := strings.Repeat("中文", InstructionRunes)
	put(t, dir, "CLAUDE.md", full)
	g, err := v.Guide()
	if err != nil || g.Content != full {
		t.Fatal("guide did not refresh or was truncated")
	}
	if !strings.Contains(g.Instructions(), "get_vault_guide") || len([]rune(g.Instructions())) > InstructionRunes+300 {
		t.Fatal("instructions not bounded or missing full-text hint")
	}
	if err := os.Remove(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	g, err = v.Guide()
	if err != nil || g.Path != "AGENTS.md" {
		t.Fatal("guide deletion did not refresh")
	}
}

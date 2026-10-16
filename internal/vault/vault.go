// Package vault owns the filesystem boundary and serializes all mutations.
package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/E13Lau/vault-mcp/internal/index"
	"github.com/E13Lau/vault-mcp/internal/search"
	"github.com/bmatcuk/doublestar/v4"
)

const MaxNoteBytes = 16 << 20

var (
	ErrDenied   = errors.New("path is not allowed")
	ErrReadOnly = errors.New("vault is read-only")
)

type Vault struct {
	root      *os.Root
	exclude   []string
	readOnly  bool
	writes    sync.Mutex
	catalog   *index.Cache
	indexFile string
	searchMu  sync.Mutex
	searchDoc map[*index.Entry]*search.Doc
}

func Open(dir string, exclude []string, readOnly bool) (*Vault, error) {
	for _, p := range exclude {
		if !doublestar.ValidatePattern(p) {
			return nil, errors.New("invalid exclusion pattern")
		}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, safeError("open vault", err)
	}
	v := &Vault{root: r, exclude: append([]string(nil), exclude...), readOnly: readOnly, indexFile: "index.md"}
	v.catalog = index.New(v)
	return v, nil
}

func (v *Vault) Close() error   { return v.root.Close() }
func (v *Vault) ReadOnly() bool { return v.readOnly }

// check rejects raw traversal before cleaning and checks every ancestor against
// policy. os.Root provides the OS-enforced boundary for every actual operation.
// Symlinks are deliberately unsupported, including aliases to hidden paths.
func (v *Vault) check(name string, allowRoot bool) (string, error) {
	if allowRoot && (name == "" || name == ".") {
		return ".", nil
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00") || !utf8.ValidString(name) {
		return "", ErrDenied
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == ".." || (strings.HasPrefix(part, ".") && part != ".") {
			return "", ErrDenied
		}
	}
	name = path.Clean(name)
	if name == "." {
		return "", ErrDenied
	}
	parts = strings.Split(name, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		for _, pattern := range v.exclude {
			if matched, _ := doublestar.Match(pattern, prefix); matched {
				return "", ErrDenied
			}
		}
		info, err := v.root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", safeError("check path", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", ErrDenied
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return "", ErrDenied
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("parent is not a directory")
		}
	}
	return name, nil
}

func (v *Vault) note(name string) (string, error) {
	n, err := v.check(name, false)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(path.Ext(n), ".md") {
		return "", errors.New("note path must end in .md")
	}
	return n, nil
}

func safeError(op string, err error) error {
	for _, kind := range []error{fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, ErrDenied} {
		if errors.Is(err, kind) {
			return fmt.Errorf("%s: %w", op, kind)
		}
	}
	return fmt.Errorf("%s: filesystem operation failed", op)
}

func (v *Vault) read(name string) (string, error) {
	f, err := v.root.Open(name)
	if err != nil {
		return "", safeError("read file", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", safeError("stat file", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxNoteBytes+1))
	if err != nil {
		return "", safeError("read file", err)
	}
	if len(b) > MaxNoteBytes {
		return "", errors.New("file exceeds 16 MiB limit")
	}
	if !utf8.Valid(b) {
		return "", errors.New("file is not valid UTF-8")
	}
	return string(b), nil
}

func (v *Vault) ReadNote(name string) (string, error) {
	n, err := v.note(name)
	if err != nil {
		return "", err
	}
	return v.read(n)
}

func (v *Vault) mutate(ctx context.Context, fn func() error) error {
	if v.readOnly {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	v.writes.Lock()
	defer v.writes.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// atomicWrite pins the parent directory, stages a complete file, then renames
// it within that directory. A failed write never truncates the original file.
func (v *Vault) atomicWrite(name, content string) error {
	if len(content) > MaxNoteBytes {
		return errors.New("note exceeds 16 MiB limit")
	}
	if !utf8.ValidString(content) {
		return errors.New("content must be valid UTF-8")
	}
	if err := v.root.MkdirAll(path.Dir(name), 0755); err != nil {
		return safeError("create parent", err)
	}
	parent, err := v.root.OpenRoot(path.Dir(name))
	if err != nil {
		return safeError("open parent", err)
	}
	defer parent.Close()
	mode := fs.FileMode(0644)
	if info, err := parent.Stat(path.Base(name)); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("destination is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return safeError("stat destination", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("cannot create temporary name")
	}
	tmp := fmt.Sprintf(".vault-mcp-%x.tmp", nonce)
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return safeError("stage note", err)
	}
	defer parent.Remove(tmp)
	_, writeErr := io.WriteString(f, content)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return safeError("write note", writeErr)
	}
	if closeErr != nil {
		return safeError("close note", closeErr)
	}
	if err := parent.Rename(tmp, path.Base(name)); err != nil {
		return safeError("replace note", err)
	}
	v.catalog.Forget(name)
	return nil
}

func (v *Vault) WriteNote(ctx context.Context, name, content, mode string) error {
	return v.mutate(ctx, func() error {
		n, err := v.note(name)
		if err != nil {
			return err
		}
		if mode == "" {
			mode = "overwrite"
		}
		if mode != "overwrite" && mode != "append" && mode != "prepend" {
			return errors.New("mode must be overwrite, append, or prepend")
		}
		if mode != "overwrite" {
			old, err := v.read(n)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if mode == "append" {
				content = old + content
			} else {
				content += old
			}
		}
		return v.atomicWrite(n, content)
	})
}

func (v *Vault) PatchNote(ctx context.Context, name, old, replacement string) error {
	return v.mutate(ctx, func() error {
		n, err := v.note(name)
		if err != nil {
			return err
		}
		if old == "" {
			return errors.New("old_text must not be empty")
		}
		text, err := v.read(n)
		if err != nil {
			return err
		}
		if strings.Count(text, old) != 1 {
			return errors.New("old_text must match exactly once; supply more context")
		}
		return v.atomicWrite(n, strings.Replace(text, old, replacement, 1))
	})
}

func (v *Vault) DeleteNote(ctx context.Context, name string) error {
	return v.mutate(ctx, func() error {
		n, err := v.note(name)
		if err != nil {
			return err
		}
		info, err := v.root.Stat(n)
		if err != nil {
			return safeError("stat note", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("path is not a regular file")
		}
		if err := v.root.Remove(n); err != nil {
			return safeError("delete note", err)
		}
		v.catalog.Forget(n)
		return nil
	})
}

func (v *Vault) move(from, to string) error {
	if from == to {
		return errors.New("source and destination are identical")
	}
	info, err := v.root.Stat(from)
	if err != nil {
		return safeError("stat source", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	if _, err := v.root.Lstat(to); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return safeError("stat destination", err)
	}
	if err := v.root.MkdirAll(path.Dir(to), 0755); err != nil {
		return safeError("create parent", err)
	}
	if err := v.root.Rename(from, to); err != nil {
		return safeError("move file", err)
	}
	v.catalog.Forget(from)
	v.catalog.Forget(to)
	return nil
}

func (v *Vault) MoveFile(ctx context.Context, from, to string) error {
	return v.mutate(ctx, func() error {
		a, err := v.check(from, false)
		if err != nil {
			return err
		}
		b, err := v.check(to, false)
		if err != nil {
			return err
		}
		return v.move(a, b)
	})
}

// MoveNote uses the shared wikilink index, including ambiguous candidates.
// Unreadable notes are skipped by the index. Referring files are unchanged.
func (v *Vault) MoveNote(ctx context.Context, from, to string) ([]string, error) {
	refs := []string{}
	err := v.mutate(ctx, func() error {
		a, err := v.note(from)
		if err != nil {
			return err
		}
		b, err := v.note(to)
		if err != nil {
			return err
		}
		snapshot, err := v.catalog.Snapshot(ctx)
		if err != nil {
			return err
		}
		refs = append(refs, snapshot.Backlinks[a]...)
		return v.move(a, b)
	})
	return refs, err
}

// NotePaths returns accessible Markdown files in lexical order. Hidden,
// excluded, symlink and non-regular entries are never exposed.
func (v *Vault) NotePaths() ([]string, error) {
	files := []string{}
	err := v.walk(".", func(name string, d fs.DirEntry) error {
		if !d.IsDir() && d.Type().IsRegular() && strings.EqualFold(path.Ext(name), ".md") {
			files = append(files, name)
		}
		return nil
	})
	return files, err
}

// Package index maintains an immutable, mtime-refreshed Markdown catalog. Its
// source interface keeps filesystem policy and serialized writes in vault.
package index

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/E13Lau/vault-mcp/internal/markdown"
)

type Source interface {
	NotePaths() ([]string, error)
	ReadNote(string) (string, error)
	StatNote(string) (fs.FileInfo, error)
}

// Entries and snapshots are immutable after construction. Callers must not
// mutate Documents or their fields; write tools always parse a fresh document.
type Entry struct {
	Path, Title, Type string
	Bytes             int64
	Modified          time.Time
	Document          markdown.Document
	MetadataError     string
	Tags              []string
	Links             []markdown.Link
	IndexSummaries    []markdown.SummaryLink
	BodySummary       string
}

type Snapshot struct {
	Entries    []*Entry
	ByPath     map[string]*Entry
	Paths      []string
	Backlinks  map[string][]string
	Skipped    []string
	Generation string
	Resolver   *markdown.Resolver
	summaryMu  sync.Mutex
	summaries  map[string]map[string]string
}

type cached struct {
	mtime time.Time
	size  int64
	entry *Entry
}

type Cache struct {
	mu       sync.Mutex
	source   Source
	items    map[string]cached
	snapshot *Snapshot
	dirty    bool
}

func New(source Source) *Cache {
	return &Cache{source: source, items: map[string]cached{}, dirty: true}
}

func (c *Cache) Forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, name)
	c.dirty = true
}

func (c *Cache) Snapshot(ctx context.Context) (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	paths, err := c.source.NotePaths()
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	changed := c.dirty || len(paths) != len(c.items)
	next := make(map[string]cached, len(paths))
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := c.source.StatNote(name)
		if err != nil {
			changed = true
			continue
		}
		old, ok := c.items[name]
		if ok && old.mtime.Equal(info.ModTime()) && old.size == info.Size() {
			next[name] = old
			continue
		}
		changed = true
		item := cached{mtime: info.ModTime(), size: info.Size()}
		text, err := c.source.ReadNote(name)
		if err == nil {
			doc, parseErr := markdown.Parse(text)
			if parseErr != nil {
				doc.Fields = map[string]any{}
			}
			e := &Entry{Path: name, Title: markdown.NoteTarget(path.Base(name)), Bytes: info.Size(), Modified: info.ModTime(), Document: doc}
			if parseErr != nil {
				e.MetadataError = parseErr.Error()
			}
			if title, ok := doc.Fields["title"].(string); ok && title != "" {
				e.Title = title
			}
			e.Type, _ = doc.Fields["type"].(string)
			e.Tags = markdown.Tags(doc)
			e.Links = markdown.Links(doc.Body)
			e.IndexSummaries = markdown.IndexSummaries(doc.Body)
			e.BodySummary = markdown.BodySummary(doc)
			item.entry = e
		}
		// Unreadable files are cached as skipped entries. They never stop a
		// backlink query or move, and are retried when their mtime changes.
		next[name] = item
	}
	if !changed && c.snapshot != nil {
		return c.snapshot, nil
	}
	s := &Snapshot{Entries: []*Entry{}, ByPath: map[string]*Entry{}, Paths: []string{}, Backlinks: map[string][]string{}, Skipped: []string{}, summaries: map[string]map[string]string{}}
	hash := sha256.New()
	for _, name := range paths {
		item, ok := next[name]
		if !ok {
			s.Skipped = append(s.Skipped, name)
			continue
		}
		fmt.Fprintf(hash, "%d:%s:%d:%d;", len(name), name, item.mtime.UnixNano(), item.size)
		s.Paths = append(s.Paths, name)
		if item.entry == nil {
			s.Skipped = append(s.Skipped, name)
			continue
		}
		s.Entries = append(s.Entries, item.entry)
		s.ByPath[name] = item.entry
	}
	s.Resolver = markdown.NewResolver(s.Paths)
	for _, e := range s.Entries {
		seen := map[string]bool{}
		for _, link := range e.Links {
			for _, target := range s.Resolver.Resolve(link, e.Path) {
				if !seen[target] {
					s.Backlinks[target] = append(s.Backlinks[target], e.Path)
					seen[target] = true
				}
			}
		}
	}
	s.Generation = fmt.Sprintf("%x", hash.Sum(nil))
	c.items, c.snapshot, c.dirty = next, s, false
	return s, nil
}

func (s *Snapshot) Summaries(indexFile string) map[string]string {
	s.summaryMu.Lock()
	defer s.summaryMu.Unlock()
	if cached, ok := s.summaries[indexFile]; ok {
		return cached
	}
	out := map[string]string{}
	if e := s.ByPath[indexFile]; e != nil {
		for _, summary := range e.IndexSummaries {
			for _, target := range s.Resolver.Resolve(summary.Link, indexFile) {
				if out[target] == "" {
					out[target] = summary.Text
				}
			}
		}
	}
	s.summaries[indexFile] = out
	return out
}

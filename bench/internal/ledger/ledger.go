// Package ledger appends one JSON line per OpenRouter generation to a
// cost file and reports phase totals, so the benchmark can stop a
// phase when its allocation is exhausted.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one recorded generation. No API keys or request bodies are
// ever recorded.
type Entry struct {
	Phase            string    `json:"phase"`
	GenerationID     string    `json:"generation_id"`
	Model            string    `json:"model"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Time             time.Time `json:"time"`
}

// File is a thread-safe append-only ledger.
type File struct {
	mu   sync.Mutex
	path string
}

// New opens (creating if needed) a ledger file.
func New(path string) (*File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &File{path: path}, f.Close()
}

// Add appends one entry.
func (l *File) Add(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc.Encode(e)
}

// Total sums the cost of all entries in the file.
func Total(path string) (float64, error) {
	entries, err := Read(path)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, e := range entries {
		sum += e.CostUSD
	}
	return sum, nil
}

// PhaseTotal sums the cost of one phase's entries.
func PhaseTotal(path, phase string) (float64, error) {
	entries, err := Read(path)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, e := range entries {
		if e.Phase == phase {
			sum += e.CostUSD
		}
	}
	return sum, nil
}

// Read loads all entries from a ledger file. A missing file is not an
// error.
func Read(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		line := raw[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("ledger: bad line: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

// Package questions loads the benchmark question and write-task files.
// The format is documented in SCHEMA.md: one JSON object per line.
package questions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Question is one test question. GoldPages are vault-relative note
// paths that a correct answer must be grounded in. For type E
// (nonexistent content) GoldPages is empty.
type Question struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // A, B, C, D or E
	Question string `json:"question"`
	// Query is the keyword query the retrieval tool sends to the
	// search tool (natural question text would not be a fair keyword
	// query). The answering agent never sees Query. Optional.
	Query     string   `json:"query,omitempty"`
	Answer    string   `json:"answer"`
	GoldPages []string `json:"gold_pages"`
	// RelatedPages are other pages that genuinely answer the question
	// (for example pages that contain the same keyword). They count as
	// hits for MRR but not for the Recall denominator.
	RelatedPages []string `json:"related_pages,omitempty"`
	Notes        string   `json:"notes"`
}

// WriteTask is one write-operation task for the agent to perform.
type WriteTask struct {
	ID              string   `json:"id"`
	Task            string   `json:"task"`
	ExpectedChanges []string `json:"expected_changes"`
	Checks          []string `json:"checks"`
	Notes           string   `json:"notes"`
}

// LoadQuestions reads a questions.jsonl file.
func LoadQuestions(path string) ([]Question, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Question
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var q Question
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if q.ID == "" || q.Type == "" || q.Question == "" {
			return nil, fmt.Errorf("%s:%d: id, type and question are required", path, line)
		}
		out = append(out, q)
	}
	return out, sc.Err()
}

// LoadWriteTasks reads a write_tasks.jsonl file.
func LoadWriteTasks(path string) ([]WriteTask, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []WriteTask
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var t WriteTask
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if t.ID == "" || t.Task == "" {
			return nil, fmt.Errorf("%s:%d: id and task are required", path, line)
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

// Command report aggregates the JSON/JSONL result files produced by
// the other tools into one summary JSON and CSV, ready for the
// report. It never interprets the answers themselves; scoring is
// done by the human reviewer.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/E13Lau/vault-mcp/bench/internal/ledger"
)

type resultSet struct {
	Path string `json:"path"`
	// Retrieval
	Retrieval *retrievalFile `json:"retrieval,omitempty"`
	// Latency
	Latency *latencyFile `json:"latency,omitempty"`
	// Agent lines
	AgentLines int `json:"agent_lines"`
	// Costs
	Costs []ledger.Entry `json:"costs"`
}

type retrievalFile struct {
	Target    string `json:"target"`
	SearchTool string `json:"search_tool"`
	Limit     int    `json:"limit"`
	Questions int    `json:"questions"`
	FirstHitP50 float64 `json:"first_hit_p50"`
}

type latencyFile struct {
	Tool string `json:"tool"`
}

func main() {
	outDir := flag.String("out", "", "output file (summary.json; CSV is derived)")
	resultsDir := flag.String("results", "", "directory with the result files of one configuration")
	flag.Parse()
	if *outDir == "" || *resultsDir == "" {
		fatal("usage: report -results <dir> -out <summary.json>")
	}
	var rs resultSet
	rs.Path = *resultsDir

	if b, err := os.ReadFile(filepath.Join(*resultsDir, "retrieval.json")); err == nil {
		var rf retrievalFile
		if err := json.Unmarshal(b, &rf); err == nil {
			var raw struct {
				Questions []struct {
					FirstHit int `json:"first_hit_rank"`
				} `json:"questions"`
			}
			_ = json.Unmarshal(b, &raw)
			rf.Questions = len(raw.Questions)
			rs.Retrieval = &rf
			// p50 of first-hit rank over all questions.
			ranks := make([]float64, 0, len(raw.Questions))
			for _, q := range raw.Questions {
				if q.FirstHit > 0 {
					ranks = append(ranks, float64(q.FirstHit))
				}
			}
			if len(ranks) > 0 {
				rf.FirstHitP50 = percentile(ranks, 50)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(*resultsDir, "latency.json")); err == nil {
		var lf latencyFile
		if err := json.Unmarshal(b, &lf); err == nil {
			rs.Latency = &lf
		}
	}
	entries, _ := ledger.Read(filepath.Join(*resultsDir, "cost.jsonl"))
	rs.Costs = entries
	// Count agent lines.
	if b, err := os.ReadFile(filepath.Join(*resultsDir, "agent.jsonl")); err == nil {
		for _, c := range b {
			if c == '\n' {
				rs.AgentLines++
			}
		}
	}

	raw, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		fatal("%v", err)
	}
	if err := os.WriteFile(*outDir, raw, 0o644); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("summary written to %s\n", *outDir)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	// Values may arrive unsorted; sort a copy.
	vals := make([]float64, len(sorted))
	copy(vals, sorted)
	for i := 1; i < len(vals); i++ {
		for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
			vals[j], vals[j-1] = vals[j-1], vals[j]
		}
	}
	rank := int(float64(len(vals))*p/100 + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(vals) {
		rank = len(vals)
	}
	return vals[rank-1]
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "report: "+format+"\n", args...)
	os.Exit(1)
}

// Package metrics computes the retrieval and latency statistics used
// by the benchmark report: recall@k, MRR@k and percentiles.
package metrics

import (
	"math"
	"sort"
)

// RecallAtK is the fraction of gold pages that appear in the first k
// returned paths. It returns NaN when there are no gold pages.
func RecallAtK(returned []string, gold []string, k int) float64 {
	if len(gold) == 0 {
		return math.NaN()
	}
	limit := min(k, len(returned))
	hits := 0
	for _, g := range gold {
		for _, r := range returned[:limit] {
			if r == g {
				hits++
				break
			}
		}
	}
	return float64(hits) / float64(len(gold))
}

// MRRAtK is the mean reciprocal rank across gold pages: for each gold
// page the reciprocal of its rank in the returned list (1-based), or 0
// when it is not in the first k results. It returns NaN when there are
// no gold pages.
func MRRAtK(returned []string, gold []string, k int) float64 {
	if len(gold) == 0 {
		return math.NaN()
	}
	limit := min(k, len(returned))
	var sum float64
	for _, g := range gold {
		found := 0
		for i, r := range returned[:limit] {
			if r == g {
				found = i + 1
				break
			}
		}
		if found > 0 {
			sum += 1.0 / float64(found)
		}
	}
	return sum / float64(len(gold))
}

// MRRAtKWithRelated is MRR@k where related pages also count as hits.
// They never enter the Recall denominator, so they cannot inflate
// Recall, but a related page ranked first does not penalize MRR.
func MRRAtKWithRelated(returned, gold, related []string, k int) float64 {
	if len(gold) == 0 {
		return math.NaN()
	}
	limit := min(k, len(returned))
	var sum float64
	for _, g := range gold {
		found := 0
		for i, r := range returned[:limit] {
			if r == g || contains(related, r) {
				found = i + 1
				break
			}
		}
		if found > 0 {
			sum += 1.0 / float64(found)
		}
	}
	return sum / float64(len(gold))
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// FirstHitRank is the 1-based rank of the first gold page in the
// returned list, or 0 when no gold page appears in the first k.
func FirstHitRank(returned []string, gold []string, k int) int {
	limit := min(k, len(returned))
	for i, r := range returned[:limit] {
		for _, g := range gold {
			if r == g {
				return i + 1
			}
		}
	}
	return 0
}

// Percentile returns the p-th percentile (0-100) of sorted values.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	// Nearest-rank method.
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// Summary holds the common latency summary numbers.
type Summary struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean_ms"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	Min  float64 `json:"min_ms"`
	Max  float64 `json:"max_ms"`
}

// Summarize computes the summary over durations in milliseconds.
func Summarize(ms []float64) Summary {
	if len(ms) == 0 {
		return Summary{}
	}
	var sum float64
	for _, v := range ms {
		sum += v
	}
	return Summary{
		N:    len(ms),
		Mean: round2(sum / float64(len(ms))),
		P50:  round2(Percentile(ms, 50)),
		P95:  round2(Percentile(ms, 95)),
		Min:  round2(Percentile(ms, 0)),
		Max:  round2(Percentile(ms, 100)),
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

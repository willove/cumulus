// Package facts implements LENS per-fact evidence targets (B1): decompose a
// query into atomic requirements D_req = {f1..fK}, track per-fact coverage
// from a shared sample pool, and report the weakest-requirement stop signal
// (B2). Offline decomposition is a deterministic heuristic so gates stay
// stable; production may replace Build with an aigate analyzer that returns
// the same []Fact shape.
package facts

import (
	"strings"

	"github.com/cumubase/ask/internal/mcs"
)

// Fact is one atomic evidence requirement (LENS Definition 2).
type Fact struct {
	ID       string      `json:"id"`
	Query    string      `json:"query"`
	Covered  bool        `json:"covered"`
	Score    float64     `json:"score"`
	SourceID string      `json:"source_id,omitempty"`
	Window   *mcs.Sample `json:"window,omitempty"`
}

// Report is the multi-hop coverage state (weakest-requirement stop signal).
type Report struct {
	Facts    []Fact   `json:"facts"`
	Complete bool     `json:"complete"`
	Missing  []string `json:"missing"`
	Weakest  float64  `json:"weakest"`
	K        int      `json:"k"`
}

// CoverScore is the per-fact hit line on the offline 0-10 sample scale.
const CoverScore = 4.0

// CoverHit is the minimum keyword-overlap share for a window to support a fact.
const CoverHit = 0.5

// Build decomposes query into atomic facts (LENS D_req).
// Heuristic: conjunction / comparison marks spawn extra facts; otherwise K=1.
func Build(query string) []Fact {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	parts := splitQuery(q)
	out := make([]Fact, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, Fact{ID: "f" + itoa(i+1), Query: p})
	}
	if len(out) == 0 {
		out = []Fact{{ID: "f1", Query: q}}
	}
	return out
}

// Evaluate marks per-fact coverage from a shared sample pool and returns the
// weakest-requirement report.
func Evaluate(facts []Fact, samples []mcs.Sample) Report {
	rep := Report{Facts: facts, K: len(facts), Weakest: 1}
	if len(facts) == 0 {
		rep.Complete = false
		rep.Weakest = 0
		return rep
	}
	weakest := 1.0
	for i := range facts {
		f := &facts[i]
		kws := mcs.Fields(f.Query)
		var best *mcs.Sample
		bestHit := 0.0
		for j := range samples {
			sm := samples[j]
			if sm.Score < CoverScore {
				continue
			}
			hit := hitRatio(kws, sm.Content)
			if hit < CoverHit {
				continue
			}
			if best == nil || hit > bestHit || (hit == bestHit && sm.Score > best.Score) {
				cp := sm
				best = &cp
				bestHit = hit
			}
		}
		if best != nil {
			f.Covered = true
			f.Score = best.Score
			f.SourceID = best.Source
			f.Window = best
		} else {
			f.Covered = false
			f.Score = 0
			f.SourceID = ""
			f.Window = nil
		}
		if f.Score/10 < weakest {
			weakest = f.Score / 10
		}
		if !f.Covered {
			weakest = 0
			rep.Missing = append(rep.Missing, f.ID)
		}
	}
	rep.Weakest = weakest
	rep.Complete = len(rep.Missing) == 0
	rep.Facts = facts
	return rep
}

// NeedContinue is the budget-aware continue predicate (LENS §4.3): keep
// exploring while some requirement is open and the loop budget remains.
func NeedContinue(rep Report, loops, maxLoops int) bool {
	if loops >= maxLoops {
		return false
	}
	return !rep.Complete
}

// MissingQueries returns the fact queries still open (self-correction input).
func MissingQueries(facts []Fact, rep Report) []string {
	miss := map[string]bool{}
	for _, id := range rep.Missing {
		miss[id] = true
	}
	var out []string
	for _, f := range facts {
		if miss[f.ID] {
			out = append(out, f.Query)
		}
	}
	return out
}

func hitRatio(kws []string, content string) float64 {
	if len(kws) == 0 {
		return 0
	}
	low := strings.ToLower(content)
	hits := 0
	for _, w := range kws {
		if w != "" && strings.Contains(low, strings.ToLower(w)) {
			hits++
		}
	}
	return float64(hits) / float64(len(kws))
}

func splitQuery(q string) []string {
	remaining := q
	var parts []string
	for {
		cutAt, cutLen := findMark(remaining)
		if cutAt < 0 {
			break
		}
		left := strings.TrimSpace(remaining[:cutAt])
		if left != "" {
			parts = append(parts, left)
		}
		remaining = remaining[cutAt+cutLen:]
	}
	remaining = strings.TrimSpace(remaining)
	if remaining != "" {
		parts = append(parts, remaining)
	}
	if len(parts) == 0 {
		return []string{q}
	}
	if len(parts) > 4 {
		parts = parts[:4]
	}
	return parts
}

func findMark(s string) (idx, length int) {
	bestIdx, bestLen := -1, 0
	for _, m := range splitMarks {
		i := strings.Index(strings.ToLower(s), strings.ToLower(m))
		if i < 0 {
			continue
		}
		if m == "和" || m == "与" || m == "及" {
			if !boundary(s, i, len(m)) {
				continue
			}
		}
		if bestIdx < 0 || i < bestIdx {
			bestIdx, bestLen = i, len(m)
		}
	}
	return bestIdx, bestLen
}

func boundary(s string, i, n int) bool {
	beforeOK := i == 0 || !isCJKByte(s[i-1])
	after := i + n
	afterOK := after >= len(s) || !isCJKByte(s[after])
	return beforeOK || afterOK
}

func isCJKByte(b byte) bool {
	return b >= 0xE4 && b <= 0xE9
}

var splitMarks = []string{
	"以及", "并且", "同时", "另外", "此外",
	"相比", "对比", "还是", "或者",
	"和", "与", "及",
	" and ", " and/or ", " or ", " vs ", " versus ", " compared ",
	"；", ";",
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Package belief is the candidate-region posterior of the DEEP
// propose–observe–update loop.
//
// The loop's "observe" half was already in place (per-fact covers, per-arm
// weights); the "update" half was not: which FILES an observation proved
// productive was written nowhere, so every proposal round used the same
// static admission ranking — propose–propose–propose with a coverage check,
// not a belief-evolving loop. This package holds that state: b_f ∈ [0,1],
// "file f holds covering evidence", initialized from the admission order
// and folded forward by each file's evidence yield.
//
// It is deterministic and LLM-free by construction: the update is a
// naive-Bayes posterior over the observation, and the observation comes
// only from numbers the loop already computes (windows kept, best window
// score, newly covered facts). Default-off at the wiring site
// (CLUS_DEEP_BELIEF=1); the mechanism has to earn its default with paired
// A/B runs like every other knob in this suite.
package belief

import "sort"

// Belief is the per-file posterior. Not safe for concurrent use; the DEEP
// loop is single-goroutine per query.
type Belief struct {
	b     map[string]float64
	tried map[string]bool
	kappa float64 // observation learning rate
}

// New builds a belief from a prior map (missing ids start at 0.5 — maximum
// uncertainty, so the first observation moves them fully). kappa scales the
// evidence strength of each observation: 0 never updates, 1 treats the
// observation as certain.
func New(prior map[string]float64, kappa float64) *Belief {
	b := &Belief{b: make(map[string]float64, len(prior)), tried: map[string]bool{}, kappa: kappa}
	for id, p := range prior {
		b.b[id] = clamp01(p)
	}
	return b
}

// PriorFromRank turns an admission order into a prior: position decays
// geometrically. The ranker scores stay inside the ranker (the DEEP layer
// only sees the order), so rank position IS the prior signal; the decay
// floor keeps late-admitted files from starting at zero.
func PriorFromRank(ids []string) map[string]float64 {
	out := make(map[string]float64, len(ids))
	for i, id := range ids {
		p := 1.0
		for j := 0; j < i && p > 0.05; j++ {
			p *= 0.9
		}
		out[id] = p
	}
	return out
}

// Observe folds one file's evidence yield into its posterior. o ∈ [0,1] is
// the observation strength (0 = nothing usable, 1 = strong covering
// evidence). The update is an EMA,
//
//	b' = b + κ·(o − b)
//
// NOT the naive-Bayes posterior the design draft specified: that form
// (b' = κo·b/(κo·b+(1−κo)·(1−b))) sharpens even when the observation merely
// AGREES with the prior (o=b=0.8 → 0.94), piling all belief mass onto one
// good file's neighbours — unstable for a loop whose exploration budget is
// three widen slots. The EMA is bounded, stays put when o=b, and matches
// the suite's existing λ_t idiom (B5 arm weights). κ=0 is a no-op.
func (b *Belief) Observe(id string, o float64) {
	o = clamp01(o)
	p, ok := b.b[id]
	if !ok {
		p = 0.5
	}
	b.b[id] = clamp01(p + b.kappa*(o-p))
	b.tried[id] = true
}

// Order sorts candidate ids by proposal value: untried files by posterior
// descending, tried files sink to the bottom (their evidence is already in
// the kept set; re-proposing them buys nothing). Ties break by original
// order to keep the ordering deterministic.
func (b *Belief) Order(ids []string) []string {
	out := append([]string(nil), ids...)
	pos := make(map[string]int, len(ids))
	for i, id := range ids {
		pos[id] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := b.tried[out[i]], b.tried[out[j]]
		if ti != tj {
			return !ti // untried first
		}
		if bi, bj := b.b[out[i]], b.b[out[j]]; bi != bj {
			return bi > bj
		}
		return pos[out[i]] < pos[out[j]]
	})
	return out
}

// Get exposes a posterior (0.5 when unknown) — for telemetry and tests.
func (b *Belief) Get(id string) float64 {
	if p, ok := b.b[id]; ok {
		return p
	}
	return 0.5
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

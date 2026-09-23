// Package abstain is a zero-LLM retrieval-failure head (ir-rag 3.1 / RCS
// idea): a handful of retrieval-time features → logistic p_fail. Borrow the
// *shape*, never RCS published weights (no code, single-author paper).
//
// Fit is offline on operator-owned samples; eval golden sets are never
// training data (红线：不为刷评测题拟合阈值).
package abstain

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Features are available immediately after a search attempt — no extra LLM.
type Features struct {
	QueryLen     int     // rune length of the query
	Candidates   int     // sources offered to the scorer (admission universe)
	Kept         int     // windows that cleared the score bar
	TopScore     float64 // best window score (0–10 scale)
	MissingFacts int     // open atomic facts (0 = fully covered)
	Confidence   float64 // answer confidence in [0,1]
	Skipped      bool    // synthesis produced nothing usable
	Refused      bool    // synthesizer refused for insufficient evidence
}

// Head is a linear-logistic fail predictor. Weights are operator-owned;
// Default() is a conservative heuristic, not a fitted RCS clone.
type Head struct {
	// Weights align with featureVec order (bias is separate).
	Weights []float64
	Bias    float64
	// DeepAbove: recommend forced DEEP when p_fail ≥ this (before answer).
	DeepAbove float64
	// RefuseAbove: recommend refuse when p_fail ≥ this (after search).
	RefuseAbove float64
	// EarlyAbove: when > 0 and the caller reports a hopeless pre-search
	// state (no samples at all), p_fail ≥ this refuses BEFORE DEEP burns
	// budget (ir-rag 3.1 / RCS "少烧钱"). It forfeits DEEP's recovery
	// chance (真机有 DEEP 救回拒答的先例), so callers keep it OFF unless
	// the operator opts in — see ASK_EARLY_ABSTAIN.
	EarlyAbove float64
}

// Default returns the built-in heuristic head. It only reacts to structural
// signals (no samples / refused / many missing facts) so offline gates that
// already escalate or refuse keep their behavior; it does not encode any
// evaluation item's wording or gold label.
func Default() *Head {
	// feature order: queryLenN, candidates, kept, topScore, missing, conf, skipped, refused
	return &Head{
		Weights: []float64{
			-0.15, // longer query → slightly less likely "empty retrieval fail"
			-0.05, // more candidates → less admission-starved
			-0.40, // kept windows are the strongest success signal
			-0.25, // top score
			0.55,  // missing facts
			-1.20, // confidence
			1.80,  // skipped
			1.70,  // refused — explicit insufficient-evidence signal
		},
		Bias:        -0.20,
		DeepAbove:   0.35,
		RefuseAbove: 0.80,
		EarlyAbove:  0.80,
	}
}

func featureVec(f Features) []float64 {
	ql := float64(f.QueryLen)
	if ql > 64 {
		ql = 64
	}
	ql /= 64
	cand := float64(f.Candidates)
	if cand > 64 {
		cand = 64
	}
	cand /= 64
	kept := float64(f.Kept)
	if kept > 8 {
		kept = 8
	}
	kept /= 8
	top := f.TopScore / 10
	if top < 0 {
		top = 0
	}
	if top > 1 {
		top = 1
	}
	miss := float64(f.MissingFacts)
	if miss > 4 {
		miss = 4
	}
	miss /= 4
	conf := f.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	sk, rf := 0.0, 0.0
	if f.Skipped {
		sk = 1
	}
	if f.Refused {
		rf = 1
	}
	return []float64{ql, cand, kept, top, miss, conf, sk, rf}
}

// PFail returns the logistic fail probability in (0,1).
func (h *Head) PFail(f Features) float64 {
	if h == nil {
		return 0
	}
	x := featureVec(f)
	w := h.Weights
	if len(w) != len(x) {
		return 0.5
	}
	z := h.Bias
	for i := range x {
		z += w[i] * x[i]
	}
	// numerically stable logistic
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// Decide maps p_fail onto an action: "" (do nothing), "deep", or "refuse".
// Refuse wins when both thresholds clear — refuse is the safer product call
// after a completed search.
func (h *Head) Decide(f Features) (p float64, action string) {
	if h == nil {
		return 0, ""
	}
	p = h.PFail(f)
	if p >= h.RefuseAbove {
		return p, "refuse"
	}
	if p >= h.DeepAbove {
		return p, "deep"
	}
	return p, ""
}

// FromAnswer builds features from a FAST/DEEP-shaped result without importing
// fast/deep (keeps the package free of cycles).
func FromAnswer(query string, candidates, kept int, topScore float64, missingFacts int, confidence float64, skipped, refused bool) Features {
	return Features{
		QueryLen:     utf8.RuneCountInString(strings.TrimSpace(query)),
		Candidates:   candidates,
		Kept:         kept,
		TopScore:     topScore,
		MissingFacts: missingFacts,
		Confidence:   confidence,
		Skipped:      skipped,
		Refused:      refused,
	}
}

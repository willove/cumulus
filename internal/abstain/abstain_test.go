package abstain

import "testing"

func TestPFailMonotoneInSkipped(t *testing.T) {
	h := Default()
	base := Features{QueryLen: 20, Candidates: 8, Kept: 3, TopScore: 7, MissingFacts: 0, Confidence: 0.8}
	pOK := h.PFail(base)
	base.Skipped = true
	base.Kept = 0
	base.Confidence = 0
	base.TopScore = 0
	base.MissingFacts = 2
	pBad := h.PFail(base)
	if !(pBad > pOK) {
		t.Fatalf("skipped/empty must raise p_fail: ok=%.3f bad=%.3f", pOK, pBad)
	}
	if pBad < 0.7 {
		t.Fatalf("empty+skipped should sit high, got %.3f", pBad)
	}
}

func TestDecideActions(t *testing.T) {
	h := Default()
	// Healthy search → no action.
	p, act := h.Decide(Features{QueryLen: 16, Candidates: 10, Kept: 4, TopScore: 8, MissingFacts: 0, Confidence: 0.9})
	if act != "" {
		t.Fatalf("healthy must be silent: p=%.3f act=%q", p, act)
	}
	// Refused answer → refuse recommendation.
	p, act = h.Decide(Features{QueryLen: 16, Candidates: 2, Kept: 0, TopScore: 0, MissingFacts: 3, Confidence: 0.1, Refused: true})
	if act != "refuse" {
		t.Fatalf("refused must recommend refuse: p=%.3f act=%q", p, act)
	}
	// Mid confidence + missing → deep, not refuse.
	p, act = h.Decide(Features{QueryLen: 16, Candidates: 6, Kept: 1, TopScore: 4, MissingFacts: 2, Confidence: 0.3})
	if act != "deep" {
		t.Fatalf("weak mid state should force deep: p=%.3f act=%q", p, act)
	}
}

func TestNilHeadSafe(t *testing.T) {
	var h *Head
	if p, act := h.Decide(Features{}); p != 0 || act != "" {
		t.Fatalf("nil head must no-op: %v %q", p, act)
	}
}

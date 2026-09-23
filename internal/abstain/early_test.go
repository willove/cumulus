package abstain

import "testing"

// EarlyAbove must sit at or above RefuseAbove: an early refuse forfeits
// DEEP's recovery chance, so it may not be easier to trigger than a
// post-search refuse.
func TestEarlyAboveNotLooserThanRefuse(t *testing.T) {
	h := Default()
	if h.EarlyAbove < h.RefuseAbove {
		t.Fatalf("EarlyAbove %.2f < RefuseAbove %.2f", h.EarlyAbove, h.RefuseAbove)
	}
	if h.EarlyAbove <= 0 {
		t.Fatal("Default head ships the early gate available (callers opt in)")
	}
}

// The hopeless pre-search state (no samples, skipped) must clear the early
// bar; a healthy one must not.
func TestHopelessStateClearsEarlyBar(t *testing.T) {
	h := Default()
	hopeless := Features{QueryLen: 20, Candidates: 10, Kept: 0, TopScore: 0, MissingFacts: 2, Confidence: 0.1, Skipped: true}
	p := h.PFail(hopeless)
	if p < h.EarlyAbove {
		t.Fatalf("hopeless p_fail %.3f must clear EarlyAbove %.2f", p, h.EarlyAbove)
	}
	healthy := Features{QueryLen: 20, Candidates: 10, Kept: 3, TopScore: 7, MissingFacts: 0, Confidence: 0.8}
	if h.PFail(healthy) >= h.EarlyAbove {
		t.Fatalf("healthy p_fail must stay under the early bar")
	}
}

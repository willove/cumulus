package mcs

import "testing"

// 1.7: while coverage gaps remain, the global (blind-spot) arm must receive
// a larger share of the round budget — anchor arms already failed the
// original wording, so self-correction explores by scatter.
func TestExploreBoostShiftsBudgetToGlobal(t *testing.T) {
	cfg := DefaultConfig()
	base := New(cfg, KeywordScorer{})
	boosted := New(cfg, KeywordScorer{})
	boosted.ExploreBoost = 3.0

	λ := equalWeights()
	b0 := base.armBudget(λ)
	b1 := boosted.armBudget(λ)

	if b1["global"] <= b0["global"] {
		t.Fatalf("boost must raise global slots: %+v → %+v", b0, b1)
	}
	// Floors: no arm is silenced.
	for _, arm := range Arms {
		if b1[arm] < 1 {
			t.Fatalf("arm %s floor violated: %+v", arm, b1)
		}
	}
	// Round budget never grows.
	sum := 0
	for _, n := range b1 {
		sum += n
	}
	if sum != cfg.SamplesPerRound {
		t.Fatalf("round budget changed: %d (%+v)", sum, b1)
	}
}

// Boost never exceeds the movable slots, and a boost of ≤1 is a no-op.
func TestExploreBoostBoundedAndNoopWhenDisabled(t *testing.T) {
	cfg := DefaultConfig()
	s := New(cfg, KeywordScorer{})
	λ := equalWeights()
	s.ExploreBoost = 100
	got := s.armBudget(λ)
	if got["global"] > cfg.SamplesPerRound-2 {
		t.Fatalf("global cannot monopolize (floors hold): %+v", got)
	}
	s.ExploreBoost = 1
	plain := s.armBudget(λ)
	base := New(cfg, KeywordScorer{}).armBudget(λ)
	if plain["global"] != base["global"] || plain["lex"] != base["lex"] {
		t.Fatalf("boost=1 must equal no boost: %+v vs %+v", plain, base)
	}
}

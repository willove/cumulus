package mcs

import "testing"

// window() used to reset an out-of-range span to the WHOLE body ([0,n)), so a
// single jittered gaussian probe could silently become an entire document —
// blowing the MaxEvidence budget and dominating the score ranking. Clamping the
// span alone would instead produce an empty window and lose the sample. Both
// halves are pinned here.
func TestWindowClampsCenterNotWholeBody(t *testing.T) {
	body := []rune("0123456789")
	cases := []struct {
		center, half int
		wantS, wantE int
	}{
		{-100, 5, 0, 5}, // far-left overshoot: half-width sliver at the head
		{-8, 5, 0, 5},   // the regression case: used to be [0,10) = whole body
		{0, 5, 0, 5},    // exactly at the head
		{5, 5, 0, 10},   // in range, unchanged
		{10, 5, 5, 10},  // exactly at the tail
		{15, 5, 5, 10},  // center past the tail
		{100, 5, 5, 10}, // far-right overshoot
	}
	for _, c := range cases {
		sm := window(body, c.center, c.half, "gaussian")
		if sm.Start != c.wantS || sm.End != c.wantE {
			t.Errorf("window(center=%d,half=%d) = [%d,%d), want [%d,%d)",
				c.center, c.half, sm.Start, sm.End, c.wantS, c.wantE)
		}
		if sm.End < sm.Start {
			t.Errorf("window(center=%d,half=%d): end < start", c.center, c.half)
		}
		if sm.Content != string(body[sm.Start:sm.End]) {
			t.Errorf("window(center=%d,half=%d): content does not match the span", c.center, c.half)
		}
	}
}

// EnvConfig must be a no-op with nothing set, honor valid overrides, and ignore
// unparseable/non-positive ones (SSOT §3.3 配置驱动).
func TestEnvConfigOverrides(t *testing.T) {
	if got := EnvConfig(); got != DefaultConfig() {
		t.Fatalf("no env set: EnvConfig=%+v, want %+v", got, DefaultConfig())
	}
	t.Setenv("CLUS_MCS_WINDOW", "120")
	t.Setenv("CLUS_MCS_SAMPLES_PER_ROUND", "7")
	t.Setenv("CLUS_MCS_SIGMA", "90.5")
	t.Setenv("CLUS_MCS_ROUNDS", "not-a-number")
	t.Setenv("CLUS_MCS_TOP_SEEDS", "-3")
	got := EnvConfig()
	if got.Window != 120 || got.SamplesPerRound != 7 || got.Sigma != 90.5 {
		t.Fatalf("valid overrides not applied: %+v", got)
	}
	if got.Rounds != DefaultConfig().Rounds || got.TopSeeds != DefaultConfig().TopSeeds {
		t.Fatalf("invalid overrides must be ignored: %+v", got)
	}
}

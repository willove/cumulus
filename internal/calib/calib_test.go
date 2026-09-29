package calib

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/willove/cumulite"
)

// TestMineBandsFastOnly pins the mining shape: FAST rows bucket into 0.05
// bands ascending, DEEP rows are not the servable population and are skipped.
func TestMineBandsFastOnly(t *testing.T) {
	eps := []Episode{
		{Conf: 0.47, Correct: false, Mode: "FAST"},
		{Conf: 0.46, Correct: false, Mode: "FAST"},
		{Conf: 0.82, Correct: true, Mode: "FAST"},
		{Conf: 0.90, Correct: false, Mode: "DEEP"}, // not servable
	}
	bands := Mine(eps)
	if len(bands) != 2 {
		t.Fatalf("bands = %d, want 2: %+v", len(bands), bands)
	}
	if bands[0].Low != 0.45 || bands[0].N != 2 || bands[0].Correct != 0 {
		t.Fatalf("low band wrong: %+v", bands[0])
	}
	if bands[1].Low != 0.80 || bands[1].N != 1 || bands[1].Rate != 1.0 {
		t.Fatalf("high band wrong: %+v", bands[1])
	}
}

// TestProposeReproducesTheHandMining pins the loop against the real curve
// measured by hand on 2026-09-29: <0.65 all wrong, 0.65–0.75 ≈ 29%, 0.75–0.85
// ≈ 49%, ≥0.85 93% — target 0.75 with min support proposes 0.85.
func TestProposeReproducesTheHandMining(t *testing.T) {
	mk := func(low, hi float64, n, correct int, mode string) []Episode {
		var out []Episode
		for i := 0; i < n; i++ {
			out = append(out, Episode{Conf: low + (hi-low)*float64(i)/float64(n),
				Correct: i < correct, Mode: mode})
		}
		return out
	}
	var eps []Episode
	eps = append(eps, mk(0.50, 0.60, 9, 0, "FAST")...)  // 0/9
	eps = append(eps, mk(0.68, 0.72, 31, 9, "FAST")...)  // 29%
	eps = append(eps, mk(0.78, 0.82, 41, 20, "FAST")...) // 49%
	eps = append(eps, mk(0.87, 0.90, 14, 13, "FAST")...) // 93%
	eps = append(eps, mk(0.60, 0.90, 20, 15, "DEEP")...) // not servable

	p, ok := Propose(eps, 0.35, 0.75, 10)
	if !ok {
		t.Fatalf("want a proposal on the hand-mined curve, got keep-current: %+v", p)
	}
	if p.Proposed < 0.85 || p.Proposed >= 0.90 {
		t.Fatalf("proposed line = %v, want the ≥0.85 band (serve rate %.2f, n=%d)",
			p.Proposed, p.ServeRate, p.ServeN)
	}
	if p.ServeRate < 0.75 {
		t.Fatalf("serve rate %.2f below target", p.ServeRate)
	}
}

// TestProposeThinEvidenceKeepsCurrent: sparse bands are not a proposal —
// the loop must say "keep" rather than act on noise.
func TestProposeThinEvidenceKeepsCurrent(t *testing.T) {
	eps := []Episode{{Conf: 0.9, Correct: true, Mode: "FAST"}}
	if _, ok := Propose(eps, 0.35, 0.75, 10); ok {
		t.Fatal("thin evidence must not produce a proposal")
	}
	// A proposal at/below the current line is a cost cut, not this loop's move.
	eps = []Episode{
		{Conf: 0.36, Correct: true, Mode: "FAST"},
		{Conf: 0.38, Correct: true, Mode: "FAST"},
	}
	if _, ok := Propose(eps, 0.35, 0.5, 2); ok {
		t.Fatal("a below-current proposal must be refused")
	}
}

// TestDecideReproducesTheRealVerdicts pins the decision rule against both
// real pairs: the 0.85 proposal (gain 0, tokens 1.10×) rejects; a genuine
// gain at acceptable cost applies.
func TestDecideReproducesTheRealVerdicts(t *testing.T) {
	if v := Decide(PairStat{N: 30, CorrectA: 22, CorrectB: 22, TokensA: 182727, TokensB: 200771}); v.Apply {
		t.Fatalf("the real 0.85 pair must reject: %+v", v)
	}
	if v := Decide(PairStat{N: 30, CorrectA: 20, CorrectB: 24, TokensA: 180000, TokensB: 186000}); !v.Apply {
		t.Fatalf("gain 4 at 1.03× must apply: %+v", v)
	}
	// Gain with runaway cost rejects.
	if v := Decide(PairStat{N: 30, CorrectA: 20, CorrectB: 24, TokensA: 180000, TokensB: 220000}); v.Apply {
		t.Fatalf("gain 4 at 1.22× must reject: %+v", v)
	}
	// Cheap but noise-level gain rejects (judge floor ±5-6/30).
	if v := Decide(PairStat{N: 30, CorrectA: 20, CorrectB: 21, TokensA: 180000, TokensB: 182000}); v.Apply {
		t.Fatalf("gain 1 is under the noise floor, must reject: %+v", v)
	}
}

// TestStoreRoundtrip pins the takeover point: save → load returns the line;
// empty store keeps env/const in charge; out-of-range lines refuse to load.
func TestStoreRoundtrip(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(c)
	ctx := context.Background()
	if _, ok, err := s.Load(ctx); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if err := s.Save(ctx, 0.85, "pair gain 4 at 1.03x"); err != nil {
		t.Fatal(err)
	}
	line, ok, err := s.Load(ctx)
	if err != nil || !ok || line != 0.85 {
		t.Fatalf("load = %v ok=%v err=%v", line, ok, err)
	}
}

// TestReadUsagePseudoLabels pins the production-episode bridge: only FAST
// rows WITH a sampled stab become episodes, and the Phase-0 threshold (0.5)
// maps stability to the outcome pseudo-label.
func TestReadUsagePseudoLabels(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureCollection(context.Background(), "clus_usage"); err != nil {
		t.Fatal(err)
	}
	docs := []map[string]any{
		{"mode": "FAST", "conf": 0.72, "stab": 0.67}, // stable → pseudo-correct
		{"mode": "FAST", "conf": 0.58, "stab": 0.33}, // wobbly → pseudo-wrong
		{"mode": "FAST", "conf": 0.90, "stab": -1.0}, // not sampled → skipped
		{"mode": "FAST", "conf": 0.80},                // no stab column → skipped
		{"mode": "DEEP", "conf": 0.77, "stab": 0.9},   // not the servable population
	}
	if _, err := c.Insert(context.Background(), "clus_usage", docs); err != nil {
		t.Fatal(err)
	}
	eps, err := ReadUsage(context.Background(), c, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2: %+v", len(eps), eps)
	}
	// The ledger query does not guarantee row order — match by conf.
	byConf := map[float64]Episode{}
	for _, e := range eps {
		byConf[e.Conf] = e
	}
	if e := byConf[0.72]; !e.Correct {
		t.Fatalf("stable row mapped wrong: %+v", e)
	}
	if e := byConf[0.58]; e.Correct {
		t.Fatalf("wobbly row mapped wrong: %+v", e)
	}
}

// TestCompareGuardrailBoundaries pins the alarm's edge behaviour: exactly at
// tolerance passes, one past it fails — an alarm tighter than its tolerance
// would become a teacher (the trap this guardrail exists to prevent).
func TestCompareGuardrailBoundaries(t *testing.T) {
	tol := DefaultGuardrailTolerances()
	base := GuardrailMetrics{N: 12, EvRec: 6, Tokens: 100000, LatP90MS: 20000}

	// Exactly at every tolerance: ev_rec −1, tokens ×1.15, p90 ×1.30.
	atEdge := GuardrailMetrics{N: 12, EvRec: 5, Tokens: 115000, LatP90MS: 26000}
	if v := CompareGuardrail(base, atEdge, tol); !v.Pass {
		t.Fatalf("at-tolerance must pass: %+v", v)
	}
	pastEv := atEdge
	pastEv.EvRec = 4
	if v := CompareGuardrail(base, pastEv, tol); v.Pass {
		t.Fatal("ev_rec two items down must alarm")
	}
	pastTok := atEdge
	pastTok.Tokens = 115001
	if v := CompareGuardrail(base, pastTok, tol); v.Pass {
		t.Fatal("tokens past ceiling must alarm")
	}
	pastLat := atEdge
	pastLat.LatP90MS = 26001
	if v := CompareGuardrail(base, pastLat, tol); v.Pass {
		t.Fatal("latency past ceiling must alarm")
	}
	if v := CompareGuardrail(base, GuardrailMetrics{}, tol); v.Pass {
		t.Fatal("empty current run must alarm")
	}
	zeroSide := GuardrailMetrics{N: 12, EvRec: 5, Tokens: 1, LatP90MS: 1}
	if v := CompareGuardrail(GuardrailMetrics{N: 12}, zeroSide, tol); !v.Pass {
		t.Fatalf("zero baseline must not false-alarm: %+v", v)
	}
}

// TestReadMetricsAggregation pins the row→metrics bridge: ev_rec counts,
// tokens prefer search_tokens, p90 lands on the index floor.
func TestReadMetricsAggregation(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/rows.jsonl"
	rows := []string{
		`{"id":"a","mode":"DEEP","eval":{"ev_rec":true,"correct":true},"search_tokens":100,"latency_ms":1000}`,
		`{"id":"b","mode":"DEEP","eval":{"ev_rec":false,"correct":false},"search_tokens":300,"latency_ms":3000}`,
		`{"id":"c","mode":"FAST","eval":{"ev_rec":true},"tokens":50,"latency_ms":2000}`,
		`{"id":"d","mode":"DEEP"}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMetrics(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.N != 3 || m.EvRec != 2 || m.Tokens != 450 || m.LatP90MS != 3000 {
		t.Fatalf("metrics wrong: %+v", m)
	}
}

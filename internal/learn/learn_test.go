package learn

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulite"
)

// TestParseHypothesisIronRules pins the degraded-trust contract: only
// whitelisted knobs with sane directions ever become a "tune" — everything
// else (prose, off-list knob, nonsense direction, unparseable) is a
// no-action, never an error.
func TestParseHypothesisIronRules(t *testing.T) {
	good := `{"action":"tune","knob":"CLUS_ESCALATE_BELOW","direction":"up","anomaly":"x","rationale":"y"}`
	if h := ParseHypothesis(good); h.Action != "tune" || h.Knob != "CLUS_ESCALATE_BELOW" || h.Direction != "up" {
		t.Fatalf("good hypothesis degraded: %+v", h)
	}
	// Off-whitelist knob → no-action (the R1 iron rule).
	offList := `{"action":"tune","knob":"CLUS_DEEP_LOOPS","direction":"up"}`
	if h := ParseHypothesis(offList); h.Action != "no-action" {
		t.Fatalf("off-list knob must degrade: %+v", h)
	}
	// Nonsense direction → no-action.
	badDir := `{"action":"tune","knob":"CLUS_COVER_SCORE","direction":"sideways"}`
	if h := ParseHypothesis(badDir); h.Action != "no-action" {
		t.Fatalf("bad direction must degrade: %+v", h)
	}
	// Prose / unparseable → no-action.
	if h := ParseHypothesis("我认为应该调一下升级线。"); h.Action != "no-action" {
		t.Fatalf("prose must degrade: %+v", h)
	}
	// Explicit no-action passes through.
	if h := ParseHypothesis(`{"action":"no-action"}`); h.Action != "no-action" {
		t.Fatalf("no-action mangled: %+v", h)
	}
}

// TestWhitelistIsExactlyThree pins the whitelist itself: three takeover
// points, no drift — an agent that can reach more knobs than the calib loop
// has earned is an agent that bypassed the discipline.
func TestWhitelistIsExactlyThree(t *testing.T) {
	if len(Whitelist) != 3 {
		t.Fatalf("whitelist = %d knobs, want exactly 3: %+v", len(Whitelist), Whitelist)
	}
	for _, k := range Whitelist {
		if !Whitelisted(k.Name) {
			t.Fatalf("whitelist member %q not whitelisted?", k.Name)
		}
	}
	if Whitelisted("CLUS_DEEP_LOOPS") || Whitelisted("") {
		t.Fatal("non-members must not pass")
	}
}

// TestBudgetGate pins the ceiling: an experiment the cycle cannot afford is
// a no-action, and the default budget admits one measured 30-item pair.
func TestBudgetGate(t *testing.T) {
	if !BudgetOK(360_000, 0) {
		t.Fatal("measured pair cost must fit the default budget")
	}
	if BudgetOK(360_001, 360_000) {
		t.Fatal("one token over the ceiling must gate")
	}
}

// TestMineAnomaliesAndJournal mines a synthetic ledger: the wobble anomaly
// ranks, the journal round-trips, and an empty ledger is idle (no episodes,
// no anomalies — the cycle must not act on nothing).
func TestMineAnomaliesAndJournal(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.EnsureCollection(ctx, "clus_usage"); err != nil {
		t.Fatal(err)
	}
	if a, err := MineAnomalies(ctx, c, 6); err != nil || len(a) != 0 {
		t.Fatalf("empty ledger must be idle: %+v err=%v", a, err)
	}
	docs := []map[string]any{}
	for i := 0; i < 30; i++ {
		mode, stab := "DEEP", -1.0
		if i%2 == 0 {
			mode = "FAST"
		}
		if i%4 == 0 {
			stab = 0.3 // half the FAST rows wobble
		} else if i%4 == 2 {
			stab = 0.8
		}
		docs = append(docs, map[string]any{"mode": mode, "stab": stab, "conf": 0.7,
			"tokens": 5000 + i*100, "latency_ms": 15000 + i*500})
	}
	if _, err := c.Insert(ctx, "clus_usage", docs); err != nil {
		t.Fatal(err)
	}
	as, err := MineAnomalies(ctx, c, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) == 0 {
		t.Fatal("a 30-row ledger with wobble and tails must yield anomalies")
	}
	found := map[string]bool{}
	for _, a := range as {
		found[a.Name] = true
	}
	if !found["selfplay-wobble"] || !found["latency-tail"] || !found["token-tail"] {
		t.Fatalf("expected wobble+tails among anomalies: %+v", as)
	}

	j := NewJournal(c)
	if err := j.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.Record(ctx, map[string]any{"outcome": "dry"}); err != nil {
		t.Fatal(err)
	}
	rows, err := j.Latest(ctx, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("journal roundtrip: %+v err=%v", rows, err)
	}
	if rows[0]["outcome"] != "dry" {
		t.Fatalf("journal entry mangled: %+v", rows[0])
	}
	if !strings.Contains(RenderKnobs(), "CLUS_ESCALATE_BELOW") {
		t.Fatal("knob rendering lost a whitelist entry")
	}
}

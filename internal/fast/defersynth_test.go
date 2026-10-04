package fast

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// Deferred FAST synthesis (CLUS_FAST_DEFER_SYNTH): a thin-confidence answer
// skips its render — the DEEP escalation re-synthesizes anyway, and the
// stages_tokens breakdown showed that render is ~half of a fast bucket that
// is itself ~half of a DEEP query. The four states pinned here: skip, no
// defer when confident, backfill pays the debt, and the default (DeferBelow
// unset) synthesizes exactly as before.

type countingSynth struct{ calls *int }

func (c countingSynth) Synthesize(context.Context, string, []mcs.Sample) (string, error) {
	*c.calls++
	return "合成答案", nil
}

func deferFixture(t *testing.T, score float64) (*Engine, *int, []source.Source) {
	t.Helper()
	calls := 0
	// scoreScorer returns a fixed score so the answer's confidence is
	// scriptable: 9 → confident, 4 → thin.
	sc := fixedScoreScorer{score: score}
	e := New(sc)
	e.Synth = countingSynth{calls: &calls}
	body := strings.Repeat("正文内容。", 40) + " 连接池最大 128，超时 30 秒。"
	srcs := []source.Source{source.New("cfg", "md", "file://cfg", "cfg", "zh", body, nil)}
	return e, &calls, srcs
}

type fixedScoreScorer struct{ score float64 }

func (f fixedScoreScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	return f.score, "covers", nil
}

func TestDeferThinSynthSkipsRender(t *testing.T) {
	e, calls, srcs := deferFixture(t, 4) // thin
	e.DeferBelow = 0.6
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !ans.SynthDeferred {
		t.Fatal("a thin answer under the line must defer its synthesis")
	}
	if *calls != 0 {
		t.Fatalf("the render ran anyway (%d calls) — the whole saving is this call", *calls)
	}
	if ans.Summary != "" {
		t.Fatalf("a deferred answer carries no summary yet, got %q", ans.Summary)
	}
}

func TestConfidentAnswerStillSynthesizes(t *testing.T) {
	e, calls, srcs := deferFixture(t, 9) // confident
	e.DeferBelow = 0.6
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if ans.SynthDeferred {
		t.Fatal("a confident answer must not defer")
	}
	if *calls != 1 || ans.Summary != "合成答案" {
		t.Fatalf("the standing answer must be complete: calls=%d summary=%q", *calls, ans.Summary)
	}
}

func TestBackfillPaysTheDebt(t *testing.T) {
	e, calls, srcs := deferFixture(t, 4)
	e.DeferBelow = 0.6
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	e.BackfillSynth(context.Background(), &ans, srcs[0])
	if ans.SynthDeferred {
		t.Fatal("backfill must clear the debt flag")
	}
	if ans.Summary != "合成答案" || *calls != 1 {
		t.Fatalf("backfill must render exactly once: calls=%d summary=%q", *calls, ans.Summary)
	}
	if ans.LLMCalls != 2 { // analyze + the backfilled render
		t.Fatalf("LLMCalls = %d, want 2 (the render is billed when it runs)", ans.LLMCalls)
	}
	// A second backfill is a no-op — the debt is paid once.
	e.BackfillSynth(context.Background(), &ans, srcs[0])
	if *calls != 1 {
		t.Fatalf("double backfill double-billed: calls=%d", *calls)
	}
}

func TestNoDeferLineSynthesizesAsBefore(t *testing.T) {
	// DeferBelow unset: every gate's configuration. Even a thin answer
	// synthesizes immediately, byte-for-byte the historical behaviour.
	e, calls, srcs := deferFixture(t, 4)
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if ans.SynthDeferred || *calls != 1 || ans.Summary != "合成答案" {
		t.Fatalf("default path changed: deferred=%v calls=%d summary=%q", ans.SynthDeferred, *calls, ans.Summary)
	}
}

// The ruler drift (live 2026-10-04, 「闯红灯有什么处罚」 142s run): the cover
// arm measured whole-query lexical coverage of the (possibly rewritten)
// sampleQuery, while the escalation measures per-fact coverage of the SAME
// samples (facts.ReportFor). A query the expander rewrote to corpus vocabulary
// covers the sentence lexically (cov→1, conf high) yet leaves an uncoverable
// fact open — the render ran (64.8s) and was discarded by the very escalation
// that could see the missing fact. With DeferFacts both sides are the same
// ruler on the same samples, so the drift is structurally impossible.
func TestDeferCoverUsesTheEscalationRuler(t *testing.T) {
	e, calls, _ := deferFixture(t, 9) // confident scorer
	e.DeferThinCover = true
	// The body carries the whole query VERBATIM → lexical cov is exactly 1;
	// the old ruler saw full coverage and let the render run.
	full := []source.Source{source.New("cfg", "md", "file://cfg", "cfg", "zh",
		strings.Repeat("正文内容。", 40)+" 连接池最大连接数是多少？答：128。", nil)}
	// …but the decomposition asks a second fact whose keywords the body never
	// carries — the escalation's ruler sees an open requirement.
	e.DeferFacts = []facts.Fact{
		{ID: "f1", Query: "连接池最大连接数是多少"},
		{ID: "f2", Query: "许可证过期未续怎么办"},
	}
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", full)
	if err != nil {
		t.Fatal(err)
	}
	if !ans.SynthDeferred {
		t.Fatal("a fact-uncovered answer must defer even when whole-query coverage is full")
	}
	if *calls != 0 {
		t.Fatalf("render ran anyway: %d calls — this is the discarded-render waste", *calls)
	}
	// The same body fully satisfies a decomposition without the open fact →
	// the answer stands and synthesizes exactly once.
	e2, calls2, _ := deferFixture(t, 9)
	e2.DeferThinCover = true
	e2.DeferFacts = []facts.Fact{{ID: "f1", Query: "连接池最大连接数是多少"}}
	ans2, err := e2.Search(context.Background(), "连接池最大连接数是多少", full)
	if err != nil {
		t.Fatal(err)
	}
	if ans2.SynthDeferred || *calls2 != 1 {
		t.Fatalf("a fully fact-covered answer must synthesize: deferred=%v calls=%d", ans2.SynthDeferred, *calls2)
	}
}

// The cover arm: armed by the DEEP tier for K>1 decompositions, it defers a
// HIGH-confidence answer whose whole-query coverage is thin — the measured
// two-question case escalates on cover-incompleteness, not on confidence.
// (No DeferFacts set: this pins the legacy lexical fallback ruler.)
func TestDeferThinCoverArmsIndependentlyOfConfidence(t *testing.T) {
	e, calls, srcs := deferFixture(t, 9) // confident scorer
	e.DeferThinCover = true
	// Half the query's tokens in the body → cov < 1 even with 9-score windows.
	half := []source.Source{source.New("cfg", "md", "file://cfg", "cfg", "zh",
		strings.Repeat("正文内容。", 40)+" 连接池最大 128。", nil)}
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少，超时多少秒", half)
	if err != nil {
		t.Fatal(err)
	}
	_ = srcs
	if !ans.SynthDeferred {
		t.Fatal("a cover-thin answer must defer even at high confidence")
	}
	if *calls != 0 {
		t.Fatalf("render ran anyway: %d", *calls)
	}
	// Full coverage disarms it, conf or not. The body carries the query
	// VERBATIM so the token coverage is exactly 1.
	full := []source.Source{source.New("cfg", "md", "file://cfg", "cfg", "zh",
		strings.Repeat("正文内容。", 40)+" 连接池最大连接数是多少？答：128。", nil)}
	e2, calls2, _ := deferFixture(t, 9)
	e2.DeferThinCover = true
	ans2, err := e2.Search(context.Background(), "连接池最大连接数是多少", full)
	if err != nil {
		t.Fatal(err)
	}
	if ans2.SynthDeferred || *calls2 != 1 {
		t.Fatalf("a fully covered answer must synthesize: deferred=%v calls=%d", ans2.SynthDeferred, *calls2)
	}
}

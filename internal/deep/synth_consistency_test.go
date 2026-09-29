package deep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// stubConsistency records the windows it saw and answers programmatically.
type stubConsistency struct {
	calls int
	saw   int
	agree bool
	why   string
	fail  error
}

func (s *stubConsistency) CheckConsistency(_ context.Context, _ string, windows []mcs.Sample) (bool, string, error) {
	s.calls++
	s.saw = len(windows)
	if s.fail != nil {
		return true, "", s.fail
	}
	return s.agree, s.why, nil
}

func TestConsistencyGateWindows(t *testing.T) {
	mk := func(src string, score, start int) mcs.Sample {
		return mcs.Sample{Source: src, Score: float64(score), Start: start, Content: "窗口内容。"}
	}
	corpus := []source.Source{
		{ID: "a", Body: "《题》 作者：甲。全文：正文甲正文甲。"},
		{ID: "b", Body: "《题》 作者：乙。全文：正文乙正文乙。"},
	}
	// Single source (even many windows) → no check possible.
	if got := consistencyGateWindows([]mcs.Sample{mk("a", 9, 500), mk("a", 8, 700), mk("a", 7, 900)}, corpus); got != nil {
		t.Fatalf("single source must not gate, got %d windows", len(got))
	}
	// Sub-cover windows never count as disagreeing evidence.
	if got := consistencyGateWindows([]mcs.Sample{mk("a", 9, 500), mk("b", 3, 500)}, corpus); got != nil {
		t.Fatalf("sub-cover second source must not gate, got %d", len(got))
	}
	// Two sources, top windows mid-body → EACH gains its head sample (the
	// agreeprobe discrimination: the divergence signal lives at the head).
	got := consistencyGateWindows([]mcs.Sample{mk("a", 9, 500), mk("b", 8, 500), mk("a", 5, 900), mk("b", 8, 600)}, corpus)
	if len(got) != 4 {
		t.Fatalf("want top+head per source (4 windows), got %d: %+v", len(got), got)
	}
	heads := 0
	for _, w := range got {
		if w.Start == 0 && strings.Contains(w.Content, "作者：") {
			heads++
		}
	}
	if heads != 2 {
		t.Fatalf("want one head window per source, got %d: %+v", heads, got)
	}
	// A top window already covering the head must not duplicate it.
	got = consistencyGateWindows([]mcs.Sample{mk("a", 9, 0), mk("b", 8, 0)}, corpus)
	if len(got) != 2 {
		t.Fatalf("head-covering tops stay single, got %d: %+v", len(got), got)
	}
	// Cap at three sources.
	in := make([]mcs.Sample, 0, 6)
	corpus6 := []source.Source{}
	for _, s := range []string{"a", "b", "c", "d", "e", "f"} {
		in = append(in, mk(s, 9, 500))
		corpus6 = append(corpus6, source.Source{ID: s, Body: "头。" + strings.Repeat("正文。", 200)})
	}
	if got := consistencyGateWindows(in, corpus6); len(got) > 6 {
		t.Fatalf("cap = %d windows, want <= 6 (3 sources x top+head)", len(got))
	}
}

// twoDocScorer makes every window cover-grade so kept spans the two docs the
// fixtures provide — the gate precondition.
type twoDocScorer struct{}

func (twoDocScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	return 8, "covers f1", nil
}

func consistencyEngineWith(sc mcs.Scorer, cons ConsistencyChecker) *Engine {
	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	e.Consistency = cons
	return e
}

// TestConsistencyGateMarksContestedAnswer drives afterBase directly with a
// hand-built base (the TestEscalationLoadsCorpusDespiteNarrowAnchors
// pattern): a served answer whose kept windows span two sources is exactly
// the gate's precondition, without coupling the test to the DEEP loop's own
// stop mechanics. Controls: flag off; checker agreeing; failing checker.
func TestConsistencyGateMarksContestedAnswer(t *testing.T) {
	q := "《值雨》這首詩的作者是誰？"
	gold := source.New("gold", "md", "file://gold", "gold", "zh",
		"《值雨》 作者：郭印。全文：慘慘雲頭暗，繩繩雨腳垂。", nil)
	dist := source.New("dist", "md", "file://dist", "dist", "zh",
		"《值雨》 作者：宋伯仁。全文：吳陵兩月雨留連，來問瓊花。", nil)
	base := kb.Result{Answer: fast.Answer{
		Query: q, Mode: fast.ModeFAST, Confidence: 0.99, SourceID: "gold",
		Samples: []mcs.Sample{
			{Source: "gold", Score: 8, Content: "《值雨》 作者：郭印。全文：慘慘雲頭暗。", Start: 0, End: 18},
			{Source: "dist", Score: 8, Content: "《值雨》 作者：宋伯仁。全文：吳陵兩月雨。", Start: 0, End: 18},
		},
	}}

	run := func(env string, stub *stubConsistency) Result {
		t.Setenv("CLUS_SYNTH_CONSISTENCY", env)
		e := consistencyEngineWith(twoDocScorer{}, stub)
		res, err := e.afterBase(context.Background(), time.Now(), q, base,
			[]source.Source{gold, dist}, e.thresholdFor(facts.Build(q)), facts.Build(q), nil)
		if err != nil {
			t.Fatalf("afterBase (env=%q): %v", env, err)
		}
		return res
	}

	// Control 1: flag off — checker never runs, answer plain.
	if res := run("", &stubConsistency{agree: false}); res.Answer.Contested {
		t.Fatal("flag off: answer marked contested")
	}
	// Control 2: flag on, checker agrees — plain answer.
	if res := run("1", &stubConsistency{agree: true}); res.Answer.Contested {
		t.Fatal("agreeing checker marked contested")
	}
	// Treatment: disagreement → Contested + BOTH claims in the prefix.
	res := run("1", &stubConsistency{agree: false, why: "《值雨》的作者，一段记为郭印，另一段记为宋伯仁。"})
	if !res.Answer.Contested {
		t.Fatal("disagreeing checker did not mark Contested")
	}
	for _, want := range []string{"语料证据存在分歧", "郭印", "宋伯仁"} {
		if !strings.Contains(res.Answer.Summary, want) {
			t.Fatalf("contested summary lost %q:\n%s", want, res.Answer.Summary)
		}
	}
	// A failing checker degrades to unmarked, never breaks the answer.
	if res := run("1", &stubConsistency{fail: errors.New("endpoint 500")}); res.Answer.Contested {
		t.Fatal("failing checker marked the answer contested")
	}
}

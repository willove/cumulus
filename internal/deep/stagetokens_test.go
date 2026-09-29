package deep

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// Stage-token attribution: every paying stage's meter delta must land in its
// own bucket, and with no Meter wired the Result must carry nothing (the
// offline gates stay byte-for-byte). The spends are counter bumps, not real
// tokens — the plumbing under test is begin/end bookkeeping, not the API.

// spendScorer bumps the fake meter by 10 per Score call, like a real scoring
// batch costs the chat client.
type spendScorer struct{ n *int }

func (s spendScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	*s.n += 10
	return 6, "covers f1", nil
}

type decomposeFunc func(context.Context, string) ([]string, error)

func (f decomposeFunc) Decompose(ctx context.Context, q string) ([]string, error) {
	return f(ctx, q)
}

var _ facts.Decomposer = decomposeFunc(nil)

type synthFunc func(context.Context, string, []mcs.Sample) (string, error)

func (f synthFunc) Synthesize(ctx context.Context, q string, s []mcs.Sample) (string, error) {
	return f(ctx, q, s)
}

var _ fast.Synthesizer = synthFunc(nil)

// The Ask level must attach the buckets and attribute the FAST tier plus the
// decompose call. Escalation is NOT forced here (thresholdFor caps the line
// at 0.6 and a confident FAST answer legitimately stands) — the DEEP-only
// stages are pinned by the runDeep test below.
func TestStageTokensAttachedAtAskLevel(t *testing.T) {
	ctx := context.Background()
	spend := 0
	sc := spendScorer{n: &spend}

	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	e.Meter = func() int64 { return int64(spend) }

	e.Decompose = decomposeFunc(func(context.Context, string) ([]string, error) {
		spend += 8
		return []string{"孙悟空的师父是谁", "学成了哪些本领"}, nil
	})
	e.DecomposeBudget = 1

	q := "孙悟空的师父是谁？学成了哪些本领？" // licenses K=2
	srcs := []source.Source{
		source.New("blk/a", "md", "file://a", "a", "zh", q+" 相关记载。正文内容。", nil),
		source.New("blk/b", "md", "file://b", "b", "zh", q+" 相关记载。正文内容。", nil),
	}
	res, err := e.Ask(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	st := res.StagesTokens
	if st == nil {
		t.Fatal("with a Meter wired the result must carry stage tokens")
	}
	if st.Decompose != 8 {
		t.Errorf("decompose bucket = %d, want 8", st.Decompose)
	}
	if st.Fast == 0 || st.Fast%10 != 0 {
		t.Errorf("fast bucket = %d, want a multiple of 10 (the FAST tier scored through the same spendScorer)", st.Fast)
	}
	if st.Rewrite != 0 {
		t.Errorf("rewrite bucket = %d, want 0 (no HistoryRewriter wired)", st.Rewrite)
	}
	if total := st.Fast + st.Decompose + st.Rank + st.Score + st.Synth + st.Widen; total != int64(spend) {
		t.Errorf("buckets sum %d but meter moved %d — a paying call escaped attribution", total, spend)
	}
}

// The DEEP-only stages (rank / score / synth) are pinned at the runDeep
// level, the same way earlystop_test.go drives the loop directly — Ask-level
// forcing would mean fighting thresholdFor's 0.6 cap.
func TestStageTokensAttributeDeepStages(t *testing.T) {
	ctx := context.Background()
	spend := 0
	sc := spendScorer{n: &spend}

	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	e.Meter = func() int64 { return int64(spend) }
	e.stageTok = &StageTokens{} // runDeep is below the Ask-level reset

	e.RankAdmission = func(context.Context, string, []source.Source, map[string]bool) ([]source.Source, error) {
		spend += 5
		return nil, nil
	}
	e.Synth = synthFunc(func(context.Context, string, []mcs.Sample) (string, error) {
		spend += 7
		return "合成答案", nil
	})

	q := "孙悟空的师父是谁？学成了哪些本领？"
	srcs := []source.Source{
		source.New("blk/a", "md", "file://a", "a", "zh", q+" 相关记载。正文内容。", nil),
		source.New("blk/b", "md", "file://b", "b", "zh", q+" 相关记载。正文内容。", nil),
		source.New("blk/c", "md", "file://c", "c", "zh", q+" 相关记载。正文内容。", nil),
	}
	if _, _, _, _, _, _, _, _, err := e.runDeep(ctx, q, srcs, nil); err != nil {
		t.Fatal(err)
	}
	st := e.stageTok
	if st.Rank != 5 {
		t.Errorf("rank bucket = %d, want 5", st.Rank)
	}
	if st.Score == 0 || st.Score%10 != 0 {
		t.Errorf("score bucket = %d, want a multiple of 10 (10 per scoring call)", st.Score)
	}
	if st.Synth == 0 || st.Synth%7 != 0 {
		t.Errorf("synth bucket = %d, want a multiple of 7 (7 per synthesis)", st.Synth)
	}
	if total := st.Fast + st.Decompose + st.Rank + st.Score + st.Synth + st.Widen; total != int64(spend) {
		t.Errorf("buckets sum %d but meter moved %d — a paying call escaped attribution", total, spend)
	}
}

// The no-Meter default is the offline contract: nothing is attached, nothing
// changes. Every gate runs this configuration.
func TestStageTokensAbsentWithoutMeter(t *testing.T) {
	ctx := context.Background()
	sc := &fixedScorer{scores: []float64{7, 1, 1}, pass: true}
	e := engineWith(sc)
	e.MaxLoops = 3
	q := "孙悟空的兵器是什么"
	srcs := docsWith(q, "blk/a", "blk/b", "blk/c")
	res, err := e.Ask(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.StagesTokens != nil {
		t.Fatalf("no Meter wired but stages attached: %+v", res.StagesTokens)
	}
}

// Deferred FAST synthesis end-to-end under the real flag: the FAST tier
// skips its render, escalation still fires on the same line, and the DEEP
// tier is the only synthesis that runs.
func TestDeferredFastSynthEscalatesWithoutFastRender(t *testing.T) {
	ctx := context.Background()
	spend := 0
	sc := spendScorer{n: &spend}

	thin := &thinScorer{n: &spend} // 5/10 windows: conf sits under the 0.6 cap
	fastCalls := 0
	fe := fast.New(thin)
	fe.Synth = synthFunc(func(context.Context, string, []mcs.Sample) (string, error) {
		fastCalls++
		return "FAST 合成", nil
	})
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	e.Synth = synthFunc(func(context.Context, string, []mcs.Sample) (string, error) {
		spend += 7
		return "DEEP 合成", nil
	})

	t.Setenv("CLUS_FAST_DEFER_SYNTH", "1")
	e.EscalateBelow = 0.5
	q := "孙悟空的师父是谁？学成了哪些本领？"
	// The body carries only the FIRST half of the question, so the FAST
	// coverage (and with it the confidence) sits under the line.
	srcs := []source.Source{
		source.New("blk/a", "md", "file://a", "a", "zh", "孙悟空的师父是谁 相关记载。正文内容。", nil),
	}
	res, err := e.Ask(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if fastCalls != 0 {
		t.Fatalf("the FAST tier rendered %d times — the whole saving is skipping it", fastCalls)
	}
	if res.Answer.Summary != "DEEP 合成" {
		t.Fatalf("the DEEP tier must be the only synthesizer, got %q", res.Answer.Summary)
	}
}

// thinScorer spends 10 and scores 4 — the admission floor. With a body that
// carries only half the query's tokens (cov≈0.5), the FAST confidence lands
// ≈0.45, under the 0.5 line the test wires.
type thinScorer struct{ n *int }

func (s *thinScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	*s.n += 10
	return 4, "covers f1", nil
}

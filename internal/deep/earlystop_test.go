package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// A scorer that hands out a fixed score per document, so the loop's stop
// decision is fully determined and the test costs nothing.
type fixedScorer struct {
	// scores[i] is the score for the i-th Sample the loop scores.
	scores []float64
	n      int
	// pass makes the window cover the query, which is what makes rep.Complete
	// reachable at all.
	pass bool
}

func (f *fixedScorer) Score(_ context.Context, _ string, s mcs.Sample) (float64, string, error) {
	i := f.n
	f.n++
	if i >= len(f.scores) {
		i = len(f.scores) - 1
	}
	if f.pass {
		return f.scores[i], "covers f1", nil
	}
	return f.scores[i], "no cover", nil
}

// engineWith builds a DEEP engine whose scorer is fixed, going through kb the
// way every real DEEP path does (askEffective delegates to the kb engine).
func engineWith(sc mcs.Scorer) *Engine {
	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	return e
}

// docsWith builds documents that actually CONTAIN the query. rep.Complete is a
// keyword-overlap judgement (facts.CoverHit), so a fixture whose bodies share
// no token with the question can never complete and the stop path is
// unreachable — the first version of this file had that bug.
func docsWith(query string, keys ...string) []source.Source {
	out := make([]source.Source, 0, len(keys))
	for _, k := range keys {
		body := query + " 相关记载。" + strings.Repeat("正文内容。", 200)
		out = append(out, source.New(k, "md", "file://"+k, k, "zh", body, nil))
	}
	return out
}

// The measured production case (2026-07-28, 727k-rune novel): the block
// holding the answer scored 5.0, the wrong ones 2.0/1.0/1.0. Separation is
// clean, but 5.0 is permanently under the fixed >= 8 line, so the loop could
// not stop and went on to score more documents for nothing.
func TestEarlyStopBudgetArmNeedsRisk(t *testing.T) {
	ctx := context.Background()
	q := "唐僧的前世是谁"
	srcs := docsWith(q, "blk/000084", "blk/000026", "blk/000027", "blk/000061", "blk/000099", "blk/000123")

	// No budget: behaviour must be exactly what it was — keep exploring, because
	// 5.0 < 8. This is the regression guard for the default path.
	noBudget := engineWith(&fixedScorer{scores: []float64{2, 1, 1, 5, 1, 1}, pass: true})
	noBudget.MaxLoops = 4
	_, _, _, _, _, _, _, reason1, err := noBudget.runDeep(ctx, q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason1 == "sufficient" {
		t.Fatalf("without a budget the loop must not stop on a 5.0 window (regression): %q", reason1)
	}
	if noBudget.Scorer.(*fixedScorer).n < 4 {
		t.Fatalf("the answering document was never scored (scored %d)", noBudget.Scorer.(*fixedScorer).n)
	}

	// With a budget already at risk, the same scores must stop at the covering
	// window instead of spending the rest of the query.
	atRisk := engineWith(&fixedScorer{scores: []float64{2, 1, 1, 5, 1, 1}, pass: true})
	atRisk.MaxLoops = 4
	atRisk.TokenBudget = 100
	atRisk.TokensUsed = func() int64 { return 80 } // 80% ≥ 60%
	_, _, _, _, _, _, _, reason2, err := atRisk.runDeep(ctx, q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason2 != "sufficient" {
		t.Fatalf("at risk, a covering window must end the loop, got %q", reason2)
	}
	// It must have stopped BEFORE scoring the trailing junk documents.
	if got := atRisk.Scorer.(*fixedScorer).n; got > 4 {
		t.Fatalf("scored %d documents after a sufficient answer at #4", got)
	}
}

// A strong window stops regardless of budget — the new arm must not weaken the
// existing stop.
func TestEarlyStopStrongWindowStopsWithoutBudget(t *testing.T) {
	ctx := context.Background()
	q := "孙悟空的兵器是什么"
	srcs := docsWith(q, "blk/a", "blk/b", "blk/c", "blk/d")
	e := engineWith(&fixedScorer{scores: []float64{1, 9, 1, 1}, pass: true})
	e.MaxLoops = 4
	_, _, _, _, _, _, _, reason, err := e.runDeep(ctx, q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "sufficient" {
		t.Fatalf("a 9.0 covering window must stop the loop, got %q", reason)
	}
}

// A low-scoring window must NOT stop even under budget pressure: the second
// arm is gated on the cover line, so a loop that has found nothing still works.
func TestEarlyStopWeakWindowNeverStops(t *testing.T) {
	ctx := context.Background()
	q := "孙悟空的兵器是什么"
	srcs := docsWith(q, "blk/a", "blk/b", "blk/c", "blk/d")
	e := engineWith(&fixedScorer{scores: []float64{1, 2, 1, 1}, pass: true})
	e.MaxLoops = 2
	e.TokenBudget = 100
	e.TokensUsed = func() int64 { return 99 }
	_, _, _, _, _, _, _, reason, err := e.runDeep(ctx, q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason == "sufficient" {
		t.Fatal("a window below the cover line must not stop the loop, even at risk")
	}
}

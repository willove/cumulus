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

// conflictScorer scores every window 8 (cover grade), and — on the conflict
// batch path — marks every window as contradicting the digest, so the stop
// gate has a live veto to exercise. markConflicts lets one stub serve both
// the treatment and the no-mark arm.
type conflictScorer struct {
	confCalls     int
	markConflicts bool
}

func (c *conflictScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	return 8, "covers f1", nil
}

func (c *conflictScorer) ScoreBatch(_ context.Context, _ string, _ []string, samples []mcs.Sample) ([]mcs.BatchResult, error) {
	out := make([]mcs.BatchResult, len(samples))
	for i := range samples {
		out[i] = mcs.BatchResult{Score: 8, Reasoning: "covers f1"}
	}
	return out, nil
}

func (c *conflictScorer) ScoreBatchConflict(_ context.Context, _ string, _ []string, samples, _ []mcs.Sample) ([]mcs.BatchResult, error) {
	c.confCalls++
	out := make([]mcs.BatchResult, len(samples))
	for i := range samples {
		r := mcs.BatchResult{Score: 8, Reasoning: "covers f1"}
		if c.markConflicts {
			r.Conflicts = []string{"K1"}
		}
		out[i] = r
	}
	return out, nil
}

// bigConflictDocs makes bodies above SmallFileRunes (100k) so the sampler
// takes the windowed path and proposes multi-window batches — the conflict
// branch needs len(in) > 1 — while the query sits at the head so a kept
// anchor window can complete coverage.
func bigConflictDocs(query string, keys ...string) []source.Source {
	out := make([]source.Source, 0, len(keys))
	for _, k := range keys {
		body := query + " 相关记载。" + strings.Repeat("正文内容。", 20000)
		out = append(out, source.New(k, "md", "file://"+k, k, "zh", body, nil))
	}
	return out
}

func conflictEngineWith(sc mcs.Scorer) *Engine {
	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	return e
}

// TestConflictVetoBlocksSufficientStop is the v3b core property: with the
// flag on and the kept set carrying contradiction marks, the sufficient exit
// is vetoed — the loop keeps hunting instead of stopping on contested
// evidence. Two controls: flag off (identical marks available, branch never
// runs) and flag on with no marks both stop sufficient exactly as before.
func TestConflictVetoBlocksSufficientStop(t *testing.T) {
	q := "连接池最大连接数是多少"
	run := func(flag string, mark bool) (string, int, int) {
		t.Setenv("CLUS_SCORER_CONFLICT", flag)
		t.Setenv("CLUS_MCS_SCORER_BATCH", "") // the conflict branch must not need it
		sc := &conflictScorer{markConflicts: mark}
		e := conflictEngineWith(sc)
		_, _, loops, _, _, _, _, reason, err := e.runDeep(context.Background(), q, bigConflictDocs(q, "blk/a", "blk/b", "blk/c"), nil)
		if err != nil {
			t.Fatalf("runDeep (flag=%q): %v", flag, err)
		}
		return reason, loops, sc.confCalls
	}

	// Control 1: flag off — the branch never runs, the strong covering
	// window stops the loop exactly as today.
	reason0, loops0, conf0 := run("", true)
	if conf0 != 0 {
		t.Fatalf("flag off but conflict branch ran (%d calls)", conf0)
	}
	if reason0 != "sufficient" {
		t.Fatalf("control arm should stop sufficient, got %q — fixture no longer reaches a covering window", reason0)
	}

	// Treatment: flag on + marks — sufficient is vetoed, the loop continues
	// under the same caps and ends by another exit.
	reason1, loops1, conf1 := run("1", true)
	if conf1 == 0 {
		t.Fatal("flag on but conflict branch never ran — batches never exceeded one window")
	}
	if reason1 == "sufficient" {
		t.Fatal("sufficient exit fired on contested evidence — the c_d veto is dead")
	}
	if loops1 <= loops0 {
		t.Fatalf("vetoed stop should keep exploring: loops %d <= control %d", loops1, loops0)
	}

	// Control 2: flag on, model reports NO conflicts — identical to control 1.
	reason2, loops2, _ := run("1", false)
	if reason2 != "sufficient" || loops2 != loops0 {
		t.Fatalf("no-mark arm should match control exactly: reason=%q loops=%d (control %q/%d)", reason2, loops2, reason0, loops0)
	}
}

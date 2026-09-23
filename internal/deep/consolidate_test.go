package deep

import (
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

// A1: overlapping windows on one source merge into one continuous block.
func TestConsolidateWindowsMergesOverlap(t *testing.T) {
	kept := []mcs.Sample{
		{Source: "a", Start: 0, End: 10, Score: 5, Content: "0123456789", Covers: []string{"f1"}},
		{Source: "a", Start: 5, End: 15, Score: 8, Content: "56789ABCDE", Covers: []string{"f2"}},
		{Source: "a", Start: 40, End: 50, Score: 7, Content: "far-away!!"},
		{Source: "b", Start: 0, End: 4, Score: 6, Content: "other"},
	}
	out := consolidateWindows(kept)
	if len(out) != 3 {
		t.Fatalf("want 3 blocks (a:merged, a:far, b), got %d: %+v", len(out), out)
	}
	var merged mcs.Sample
	for _, sm := range out {
		if sm.Source == "a" && sm.Start == 0 {
			merged = sm
		}
	}
	if merged.Start != 0 || merged.End != 15 {
		t.Fatalf("merged span want [0,15), got [%d,%d)", merged.Start, merged.End)
	}
	if merged.Score != 8 {
		t.Fatalf("merged score want max(5,8)=8, got %v", merged.Score)
	}
	if len(merged.Covers) != 2 {
		t.Fatalf("covers must union, got %v", merged.Covers)
	}
}

// A1+A3: topKeeps drops failures, merges overlaps, then cuts to budget.
func TestTopKeepsMergesAndDropsFailed(t *testing.T) {
	var kept []mcs.Sample
	for i := 0; i < maxKeepWindows; i++ {
		kept = append(kept, mcs.Sample{
			Source: "s", Start: i * 2, End: i*2 + 3, Score: 9, Content: "xxxxx",
		})
	}
	kept = append(kept, mcs.Sample{Source: "s", Start: 100, End: 110, Score: mcs.ScoreFailed, Content: "bad"})
	kept = append(kept, mcs.Sample{Source: "s", Start: 50, End: 60, Score: 4, Content: "tail"})
	out := topKeeps(kept)
	for _, sm := range out {
		if sm.Failed() {
			t.Fatal("failed sample must not survive topKeeps")
		}
	}
	if len(out) > maxKeepWindows {
		t.Fatalf("budget exceeded: %d", len(out))
	}
	// Adjacent chain 0..(2*maxKeep) should collapse into far fewer blocks than input.
	if len(out) >= len(kept)-1 {
		t.Fatalf("expected consolidation to shrink windows, got %d from %d", len(out), len(kept))
	}
}

// A3: failed score is not "irrelevant" (0).
func TestScoreFailedDistinctFromZero(t *testing.T) {
	if !(mcs.Sample{Score: mcs.ScoreFailed}).Failed() {
		t.Fatal("ScoreFailed must be Failed()")
	}
	if (mcs.Sample{Score: 0}).Failed() {
		t.Fatal("score 0 is judged irrelevant, not Failed")
	}
}

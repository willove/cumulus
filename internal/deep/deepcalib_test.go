package deep

import (
	"testing"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/mcs"
)

// The DEEP synthesis bonus lands directly on fast.SkipBelow (0.35): with the
// default 0.1 an answer at 0.25 crosses into "answered"; with the bonus off
// the same evidence stays "skipped". That boundary effect is exactly why the
// constant is env-declared — this test pins both sides so the coupling is
// visible instead of folklore (audit: the +0.1 had no provenance and could
// decide persistence/escalation on its own).
func TestDeepSynthBonusSitsOnTheSkipLine(t *testing.T) {
	// Fixture: one window scoring 5, sharing no bigram with the query →
	// mcs.Confidence(5, 0) = 0.25, exactly one bonus-step below SkipBelow.
	kept := []mcs.Sample{{Content: "听证程序的步骤与时限。", Score: 5}}
	rep := facts.Report{Complete: true, K: 1, Facts: []facts.Fact{{ID: "f1", Query: "闯红灯", Covered: true, Score: 5}}}

	t.Run("bonus off is the raw confidence", func(t *testing.T) {
		t.Setenv("CLUS_DEEP_SYNTH_BONUS", "0")
		_, conf, _ := deepMetrics("闯红灯", "某条例", kept, rep)
		if conf != 0.25 {
			t.Fatalf("raw conf = %.3f, want 0.25", conf)
		}
		if !(conf < 0.35) { // the engine's own answered/skipped expression
			t.Fatal("raw answer must read as skipped")
		}
	})
	t.Run("default bonus crosses into answered", func(t *testing.T) {
		t.Setenv("CLUS_DEEP_SYNTH_BONUS", "0.1")
		_, conf, _ := deepMetrics("闯红灯", "某条例", kept, rep)
		if conf != 0.35 {
			t.Fatalf("bonused conf = %.3f, want 0.35 (raw+0.1)", conf)
		}
		if conf < 0.35 {
			t.Fatal("bonused answer must not read as skipped")
		}
	})
}

package main

import (
	"testing"

	"github.com/willove/cumulus/internal/eval"
)

// 1.5: not_retrieved items split into corpus-missing vs admission-miss vs
// post-admission citation failure — the Remark 1 ceiling is measurable.
func TestNRRetrivalAdmissionBreakdown(t *testing.T) {
	lines := []evalResult{
		{
			ID: "miss-corpus", Eval: eval.ItemScore{ID: "miss-corpus"},
			GoldInCorpus: false, GoldInAdmitted: false,
		},
		{
			ID: "miss-admit", Eval: eval.ItemScore{ID: "miss-admit"},
			GoldInCorpus: true, GoldInAdmitted: false,
		},
		{
			ID: "admit-no-cite", Eval: eval.ItemScore{ID: "admit-no-cite"},
			GoldInCorpus: true, GoldInAdmitted: true,
		},
		{
			// correct — must not enter NR breakdown
			ID: "ok", Eval: eval.ItemScore{ID: "ok", Correct: true},
			GoldInCorpus: true, GoldInAdmitted: true,
		},
	}
	rep := aggregateResults(lines, false, 0)
	nr := rep.NRBreakdown
	if nr.NotRetrieved != 3 {
		t.Fatalf("not_retrieved=%d want 3", nr.NotRetrieved)
	}
	if nr.GoldMissingFromCorpus != 1 || nr.GoldNotAdmitted != 1 || nr.GoldAdmittedNoCite != 1 {
		t.Fatalf("breakdown=%+v", nr)
	}
	if nr.NotRetrieved != nr.GoldMissingFromCorpus+nr.GoldNotAdmitted+nr.GoldAdmittedNoCite {
		t.Fatal("breakdown must partition not_retrieved")
	}
}

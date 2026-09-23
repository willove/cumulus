package main

import (
	"testing"

	"github.com/cumubase/ask/internal/eval"
)

// 3.2: aggregate report carries the three cost lines separately.
func TestAggregateSplitsTokenAccounting(t *testing.T) {
	lines := []evalResult{
		{ID: "a", Mode: "FAST", SearchTokens: 100, JudgeTokens: 40, RejectedProposals: 1,
			Eval: eval.ItemScore{Correct: true, Answered: true}},
		{ID: "b", Mode: "DEEP", SearchTokens: 200, JudgeTokens: 60, RejectedProposals: 2,
			Eval: eval.ItemScore{EvRec: true, Answered: true}},
	}
	rep := aggregateResults(lines, true, 0)
	if rep.SearchTokens != 300 {
		t.Fatalf("search_tokens=%d, want 300", rep.SearchTokens)
	}
	if rep.JudgeTokens != 100 {
		t.Fatalf("judge_tokens=%d, want 100", rep.JudgeTokens)
	}
	if rep.RejectedProposals != 3 {
		t.Fatalf("rejected_proposals=%d, want 3", rep.RejectedProposals)
	}
	// Judge must not be folded into search.
	if rep.SearchTokens == rep.SearchTokens+rep.JudgeTokens {
		t.Fatal("sanity")
	}
}

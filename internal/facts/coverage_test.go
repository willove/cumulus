package facts

import (
	"testing"

	"github.com/willove/cumulus/internal/mcs"
)

// The keyword coverage path counted each CJK bigram as independent evidence,
// so a window holding HALF a word covered a whole fact: "闯红灯" was covered
// by a window containing only "红灯" (1 of 2 bigrams = the old CoverHit),
// "试用期" by "试用". That is how the red-light refusal looked "covered" on
// the general-punishment-law window. Coverage now also requires a contiguous
// 3-character core of the fact in the window.
func TestEvaluateCoverageNeedsFactCoreSpan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fact    string
		content string
		covered bool
	}{
		{"fragment is not coverage", "闯红灯", "红灯亮时，禁止车辆通行。", false},
		{"full term covers", "闯红灯", "闯红灯的，处二百元罚款。", true},
		{"prefix fragment is not coverage", "试用期", "试用期内双方可以约定。", true},
		{"half word is not coverage", "试用期", "员工试用表现合格后转正。", false},
		{"latin word needs the word", "probation", "the proba period rules", false},
		{"latin word present covers", "probation", "the probation period rules", true},
		{"core inside longer window", "闯红灯会有什么处罚", "闯红灯怎么处罚，处二百元罚款。", true},
		{"only generic punishment words", "闯红灯会有什么处罚", "行政处罚法规定了处罚的种类与程序。", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := []Fact{{ID: "f1", Query: tc.fact}}
			samples := []mcs.Sample{{Content: tc.content, Score: 6}}
			rep := Evaluate(facts, samples)
			if rep.Facts[0].Covered != tc.covered {
				t.Fatalf("fact %q vs %q: covered = %v, want %v (nearMiss %.2f)",
					tc.fact, tc.content, rep.Facts[0].Covered, tc.covered, rep.Facts[0].NearMiss)
			}
		})
	}
}

// A single-character CJK conjunction at the very edge of a query is not a
// conjunction — and the dead byte-range boundary() guard used to let a LEADING
// mark split the query and keep only the tail ("参与违法怎么办" → "法").
// Mid-query marks still split (best-effort: no word list distinguishes
// "总和与净利润" from "及时与否", and that limit is documented, not guarded
// by decoration).
func TestSplitQuerySkipsEdgeConjunctionMarks(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"参与违法怎么办", "参与违法怎么办"},
		{"迟到和", "迟到和"},
		{"总和与净利润分别是多少", "总和"},
		{"治安管理处罚法和刑法", "治安管理处罚法"},
	} {
		parts := splitQuery(tc.in)
		if len(parts) == 0 || parts[0] != tc.want {
			t.Fatalf("splitQuery(%q) = %v, want first part %q", tc.in, parts, tc.want)
		}
	}
	// The positive control: a mid-query mark still decomposes.
	parts := splitQuery("试用期被辞退有赔偿吗和经济补偿金怎么算")
	if len(parts) != 2 {
		t.Fatalf("mid-query conjunction must still split: %v", parts)
	}
}

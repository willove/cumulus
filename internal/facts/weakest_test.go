package facts

import (
	"reflect"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
)

// "Weakest requirement first" must actually order by weakness. Sorting on
// Fact.Score is a no-op — every UNCOVERED fact has Score 0 by construction —
// so MissingQueries sorts on the NearMiss signal Evaluate records.
func TestMissingQueriesOrdersByNearMiss(t *testing.T) {
	facts := []Fact{
		{ID: "f1", Query: "甲", NearMiss: 0.4}, // closest to covered
		{ID: "f2", Query: "乙", NearMiss: 0.0}, // weakest: no support at all
		{ID: "f3", Query: "丙", NearMiss: 0.2},
	}
	rep := Report{Facts: facts, Missing: []string{"f1", "f2", "f3"}}
	want := []string{"乙", "丙", "甲"} // weakest first, not declaration order
	if got := MissingQueries(facts, rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("weakest-first order wrong:\n got  %v\n want %v", got, want)
	}
	// Declaring them in a different order must not change the outcome.
	facts2 := []Fact{
		{ID: "f3", Query: "丙", NearMiss: 0.2},
		{ID: "f2", Query: "乙", NearMiss: 0.0},
		{ID: "f1", Query: "甲", NearMiss: 0.4},
	}
	if got := MissingQueries(facts2, rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("order must follow weakness, not declaration: got %v want %v", got, want)
	}
}

// Evaluate must RECORD the weakness signal — otherwise MissingQueries has
// nothing to sort on and silently degenerates to declaration order.
func TestEvaluateRecordsNearMissForUncovered(t *testing.T) {
	near := mcs.Sample{Start: 0, End: 12, Content: "连接池说明文字", Score: 8}
	far := mcs.Sample{Start: 0, End: 12, Content: "完全不相干的一段文字", Score: 8}
	rep := Evaluate([]Fact{{ID: "f1", Query: "连接池上限配置"}}, []mcs.Sample{near, far})
	if len(rep.Missing) != 1 {
		t.Fatalf("precondition: the fact must stay uncovered, got %+v", rep)
	}
	if rep.Facts[0].Covered {
		t.Fatal("precondition: fact must be uncovered")
	}
	if rep.Facts[0].Score != 0 {
		t.Fatalf("an uncovered fact must keep Score 0 (got %v) — the sort must use NearMiss, not Score",
			rep.Facts[0].Score)
	}
	if rep.Facts[0].NearMiss <= 0 {
		t.Fatalf("near miss must be recorded for a partially-matching window: %+v", rep.Facts[0])
	}
	if rep.Facts[0].NearMiss >= CoverHit {
		t.Fatalf("near miss must stay below the cover line: %v", rep.Facts[0].NearMiss)
	}
}

// A covered fact keeps its evidence score and does not need a near miss.
func TestEvaluateCoveredFactUnchanged(t *testing.T) {
	hit := mcs.Sample{Start: 0, End: 20, Content: "连接池上限配置说明如下", Score: 9}
	rep := Evaluate([]Fact{{ID: "f1", Query: "连接池上限配置"}}, []mcs.Sample{hit})
	if !rep.Facts[0].Covered || rep.Facts[0].Score != 9 {
		t.Fatalf("covered fact must keep its evidence: %+v", rep.Facts[0])
	}
	if rep.Facts[0].NearMiss != 0 {
		t.Fatalf("covered facts carry no near miss: %+v", rep.Facts[0])
	}
}

// Equal weakness must fall back to declaration order (determinism).
func TestMissingQueriesStableOnTies(t *testing.T) {
	facts := []Fact{{ID: "f1", Query: "甲"}, {ID: "f2", Query: "乙"}, {ID: "f3", Query: "丙"}}
	rep := Report{Facts: facts, Missing: []string{"f1", "f2", "f3"}}
	if got, want := MissingQueries(facts, rep), []string{"甲", "乙", "丙"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ties must keep declaration order: got %v want %v", got, want)
	}
}

// An all-covered report yields nothing to re-sample.
func TestMissingQueriesEmptyWhenComplete(t *testing.T) {
	facts := []Fact{{ID: "f1", Query: "甲", Covered: true}}
	if got := MissingQueries(facts, Report{Facts: facts, Complete: true}); len(got) != 0 {
		t.Fatalf("complete report must have no missing queries: %v", got)
	}
}

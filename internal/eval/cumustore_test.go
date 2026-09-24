package eval

import (
	"context"
	"testing"

	"github.com/willove/cumulite"
)

// The scoreboard reads runs from the store: save → list (newest first) →
// get, with the aggregate fields round-tripping.
func TestRunStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer eng.Close()
	coll := "clus_evals"
	if err := eng.EnsureCollection(ctx, coll); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	st := NewCumuStore(eng, coll)

	older := RunDoc{
		ID: "run:1000", Tag: "baseline", N: 39, Judged: false,
		System:       Report{N: 39, EM: 0.433, EvRec: 0.30, Ground: 0.90, Taxonomy: Taxonomy{Correct: 13, AnsweredWrong: 14, NotRetrieved: 12}},
		ClosedBook:   Report{N: 39, EM: 0.70},
		McNemar:      McNemar{BOnly: 4, COnly: 12, P: 0.077, N: 39},
		Modes:        map[string]int{"DEEP": 30, "FAST": 9},
		SearchTokens: 164672, JudgeTokens: 5349, RejectedProposals: 2,
	}
	newer := older
	newer.ID = "run:2000"
	newer.Tag = "l1-ab"
	newer.System.EM = 0.667
	newer.Extra = map[string]any{"frozen": map[string]any{"items_sha": "abc123"}}

	for _, d := range []RunDoc{older, newer} {
		if err := st.SaveRun(ctx, d); err != nil {
			t.Fatalf("save %s: %v", d.ID, err)
		}
	}
	runs, err := st.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != "run:2000" {
		t.Fatalf("list order: %+v", runs)
	}
	got, err := st.GetRun(ctx, "run:1000")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.System.EM != 0.433 || got.System.EvRec != 0.30 || got.System.Ground != 0.90 {
		t.Fatalf("system round-trip: %+v", got.System)
	}
	if got.System.Taxonomy.Correct != 13 || got.System.Taxonomy.AnsweredWrong != 14 || got.System.Taxonomy.NotRetrieved != 12 {
		t.Fatalf("taxonomy round-trip: %+v", got.System.Taxonomy)
	}
	if got.McNemar.BOnly != 4 || got.McNemar.COnly != 12 || got.McNemar.P != 0.077 {
		t.Fatalf("mcnemar round-trip: %+v", got.McNemar)
	}
	if got.Modes["DEEP"] != 30 || got.SearchTokens != 164672 || got.RejectedProposals != 2 {
		t.Fatalf("cost round-trip: %+v", got)
	}
	// An unknown id is nil, not an error.
	miss, err := st.GetRun(ctx, "run:9999")
	if err != nil || miss != nil {
		t.Fatalf("unknown id: %v %v", miss, err)
	}
	// Upsert by id (a re-saved run replaces, not duplicates).
	if err := st.SaveRun(ctx, older); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	runs, _ = st.ListRuns(ctx, 10)
	if len(runs) != 2 {
		t.Fatalf("upsert duplicated: %d runs", len(runs))
	}
}

// Two namespaces are two libraries: the scoreboard never mixes them.
func TestRunStoreNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer eng.Close()
	a := NewCumuStore(eng, "t1:clus_evals")
	b := NewCumuStore(eng, "t2:clus_evals")
	if err := eng.EnsureCollection(ctx, "t1:clus_evals"); err != nil {
		t.Fatal(err)
	}
	if err := eng.EnsureCollection(ctx, "t2:clus_evals"); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveRun(ctx, RunDoc{ID: "run:1", Tag: "t1-run", N: 5}); err != nil {
		t.Fatal(err)
	}
	runs, err := b.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("t2 list: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("t2 must not see t1 runs: %+v", runs)
	}
	if _, err := b.GetRun(ctx, "run:1"); err != nil {
		t.Fatalf("cross-ns get: %v", err)
	}
}

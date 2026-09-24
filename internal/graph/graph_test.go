package graph

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
)

func mk(t *testing.T, id, topic string, embed []float64, hot float64) cluster.Cluster {
	t.Helper()
	c := cluster.New(topic, id, "content-"+id, "query-"+id, "src", nil, embed, 0.8)
	c.ID = id
	c.Hotness = hot
	return c
}

func setupGraph(t *testing.T) (*Expander, cluster.Store, Store) {
	t.Helper()
	ctx := context.Background()
	cs := cluster.NewMemory()
	es := NewMemory()
	// Line: A → B → C → D ; A → X (low weight)
	a := mk(t, "Ca", "ta", []float64{1, 0, 0}, 0.9)
	b := mk(t, "Cb", "tb", []float64{0.9, 0.1, 0}, 0.8)
	c := mk(t, "Cc", "tc", []float64{0.8, 0.2, 0}, 0.7)
	d := mk(t, "Cd", "td", []float64{0.2, 0, 0.8}, 0.6)
	x := mk(t, "Cx", "tx", []float64{0, 1, 0}, 0.9)
	for _, cl := range []cluster.Cluster{a, b, c, d, x} {
		if err := cs.Save(ctx, cl); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []Edge{
		{From: "Ca", To: "Cb", Weight: 0.9, Source: SourceCoOcur},
		{From: "Cb", To: "Cc", Weight: 0.8, Source: SourceQuerySeq},
		{From: "Cc", To: "Cd", Weight: 0.7, Source: SourceCoOcur},
		{From: "Ca", To: "Cx", Weight: 0.2, Source: SourceEmbedSim},
	} {
		if err := es.Save(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return NewExpander(es, cs), cs, es
}

// Hand-written neighborhood at depth 1..2.
func TestMultiHopNeighborhood(t *testing.T) {
	ctx := context.Background()
	ex, _, _ := setupGraph(t)
	got, err := ex.Expand(ctx, ExpandRequest{StartID: "Ca", MaxDepth: 2, MinWeight: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for _, r := range got {
		ids[r.Cluster.ID] = r.Depth
	}
	if ids["Cb"] != 1 {
		t.Fatalf("Cb must be depth 1, got %v", ids)
	}
	if ids["Cc"] != 2 {
		t.Fatalf("Cc must be depth 2, got %v", ids)
	}
	if _, ok := ids["Cd"]; ok {
		t.Fatal("Cd is depth 3 — must be outside MaxDepth=2")
	}
	if _, ok := ids["Cx"]; ok {
		t.Fatal("Cx weight 0.2 must be filtered by MinWeight=0.5")
	}
	if ids["Ca"] != 0 && len(got) > 0 {
		// start itself never appears as a result
		for _, r := range got {
			if r.Cluster.ID == "Ca" {
				t.Fatal("start must not appear in expansion results")
			}
		}
	}
}

// hopKNN keeps the probe-closest neighbors and does not drop a high-sim one.
func TestHopKNNPruneKeepsClosest(t *testing.T) {
	ctx := context.Background()
	ex, _, _ := setupGraph(t)
	probe := []float64{0.9, 0.1, 0} // closest to B then A-ish
	got, err := ex.Expand(ctx, ExpandRequest{
		StartID: "Ca", MaxDepth: 1, MinWeight: 0.5,
		HopKNN: 1, Probe: probe,
	})
	if err != nil {
		t.Fatal(err)
	}
	// At depth 1 from Ca only Cb (and filtered Cx) — hopKNN must keep Cb.
	if len(got) != 1 || got[0].Cluster.ID != "Cb" {
		t.Fatalf("hopKNN must keep Cb, got %+v", got)
	}

	// Fan-out case: Ca → B and Ca → E(embed near probe); HopKNN=1 keeps E.
	e := mk(t, "Ce", "te", []float64{0.95, 0.05, 0}, 0.9)
	_ = ex.Clusters.Save(ctx, e)
	_ = ex.Edges.Save(ctx, Edge{From: "Ca", To: "Ce", Weight: 0.9, Source: SourceEmbedSim})
	got, err = ex.Expand(ctx, ExpandRequest{
		StartID: "Ca", MaxDepth: 1, MinWeight: 0.5,
		HopKNN: 1, Probe: []float64{0.95, 0.05, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got {
		if r.Cluster.ID == "Ce" {
			found = true
		}
	}
	if !found {
		t.Fatalf("high-sim neighbor Ce must survive hopKNN, got %+v", got)
	}
	// With K=1 only one hop-1 result.
	n := 0
	for _, r := range got {
		if r.Depth == 1 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("HopKNN=1 must yield 1 depth-1 hit, got %d", n)
	}
}

// empty graph returns nil — caller falls back to L0.
func TestEmptyGraphFallsBack(t *testing.T) {
	ctx := context.Background()
	ex := NewExpander(NewMemory(), cluster.NewMemory())
	got, err := ex.Expand(ctx, ExpandRequest{StartID: "Nope", MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty graph must return no neighbors, got %+v", got)
	}
}

func TestLinkHelpers(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	a := mk(t, "A", "ta", []float64{1, 0}, 1)
	b := mk(t, "B", "tb", []float64{0.99, 0.1}, 1)
	if err := LinkEmbedSim(ctx, st, a, b); err != nil {
		t.Fatal(err)
	}
	es, _ := st.All(ctx)
	if len(es) != 1 || es[0].Source != SourceEmbedSim {
		t.Fatalf("embed_sim edge: %+v", es)
	}
	if err := LinkQuerySeq(ctx, st, "A", "B"); err != nil {
		t.Fatal(err)
	}
	if err := LinkCoOcur(ctx, st, "B", "A"); err != nil {
		t.Fatal(err)
	}
	es, _ = st.All(ctx)
	if len(es) != 3 {
		t.Fatalf("want 3 edges, got %d", len(es))
	}
}

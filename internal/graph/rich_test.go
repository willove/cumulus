package graph

import (
	"context"
	"math"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
)

// richFixture: A ↔ {B, C, D} over plain weak edges, plus the Stores/Expander
// the rich-kind semantics act on.
func richFixture(t *testing.T) (*Expander, cluster.Store, Store) {
	t.Helper()
	ctx := context.Background()
	cs := cluster.NewMemory()
	es := NewMemory()
	for _, id := range []string{"Ca", "Cb", "Cc", "Cd"} {
		if err := cs.Save(ctx, mk(t, id, "t"+id, []float64{1, 0, 0}, 0.8)); err != nil {
			t.Fatal(err)
		}
	}
	for _, to := range []string{"Cb", "Cc", "Cd"} {
		if err := es.Save(ctx, Edge{From: "Ca", To: to, Weight: 0.6, Source: SourceCoOcur}); err != nil {
			t.Fatal(err)
		}
	}
	return NewExpander(es, cs), cs, es
}

func TestLinkQuerySeqUpgradesToPathway(t *testing.T) {
	ctx := context.Background()
	es := NewMemory()

	// First traversal: a plain weak query_seq edge.
	if err := LinkQuerySeq(ctx, es, "Ca", "Cb"); err != nil {
		t.Fatalf("link1: %v", err)
	}
	edges := fromTo(t, es, "Ca", "Cb")
	if len(edges) != 1 || edges[0].Kind != "" || edges[0].Hits != 1 {
		t.Fatalf("after one traversal: %+v", edges)
	}

	// Second: the link has been walked — pathway.
	if err := LinkQuerySeq(ctx, es, "Ca", "Cb"); err != nil {
		t.Fatalf("link2: %v", err)
	}
	edges = fromTo(t, es, "Ca", "Cb")
	if len(edges) != 1 || edges[0].Kind != KindPathway || edges[0].Hits != 2 {
		t.Fatalf("after two traversals: %+v", edges)
	}
	if edges[0].Reason != "query_seq×2" {
		t.Fatalf("reason: %q", edges[0].Reason)
	}

	// Third: stays pathway, hits accumulate (idempotent edge identity).
	if err := LinkQuerySeq(ctx, es, "Ca", "Cb"); err != nil {
		t.Fatalf("link3: %v", err)
	}
	edges = fromTo(t, es, "Ca", "Cb")
	if len(edges) != 1 || edges[0].Kind != KindPathway || edges[0].Hits != 3 {
		t.Fatalf("after three traversals: %+v", edges)
	}
}

func TestLinkBarrierBidirectionalIdempotent(t *testing.T) {
	ctx := context.Background()
	es := NewMemory()
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "divergent claims: 200 vs 128"); err != nil {
		t.Fatalf("link: %v", err)
	}
	ab := fromTo(t, es, "Ca", "Cb")
	ba := fromTo(t, es, "Cb", "Ca")
	if len(ab) != 1 || ab[0].Kind != KindBarrier || ab[0].Reason != "divergent claims: 200 vs 128" {
		t.Fatalf("a->b: %+v", ab)
	}
	if len(ba) != 1 || ba[0].Kind != KindBarrier {
		t.Fatalf("b->a: %+v", ba)
	}
	// Second call is a no-op (same edge identity), not a duplicate.
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "divergent claims: 200 vs 128"); err != nil {
		t.Fatalf("relink: %v", err)
	}
	if got := fromTo(t, es, "Ca", "Cb"); len(got) != 1 {
		t.Fatalf("idempotency: %+v", got)
	}
	// Self-links and empty endpoints are ignored.
	if err := LinkBarrier(ctx, es, "Ca", "Ca", "x"); err != nil {
		t.Fatalf("self: %v", err)
	}
	if err := LinkBarrier(ctx, es, "", "Cb", "x"); err != nil {
		t.Fatalf("empty: %v", err)
	}
	// A nil store is ignored (barriers are optional wiring).
	if err := LinkBarrier(ctx, nil, "Ca", "Cb", "x"); err != nil {
		t.Fatalf("nil store: %v", err)
	}
}

func TestExpandBarrierPrunesDirectNeighbor(t *testing.T) {
	ctx := context.Background()
	exp, _, es := richFixture(t)
	// Baseline: all three weak neighbors.
	got := mustExpand(t, exp, "Ca")
	if len(got) != 3 {
		t.Fatalf("baseline neighbors: %d", len(got))
	}
	// Bar Ca→Cb: Cb must leave the neighborhood, Cc/Cd untouched.
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "contested"); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	got = mustExpand(t, exp, "Ca")
	if len(got) != 2 {
		t.Fatalf("after barrier: %d neighbors", len(got))
	}
	for _, r := range got {
		if r.Cluster.ID == "Cb" {
			t.Fatalf("barred neighbor leaked into expansion")
		}
	}
	// The other direction prunes too (bidirectional barrier).
	got = mustExpand(t, exp, "Cb")
	if len(got) != 0 {
		t.Fatalf("Cb must not expand into Ca: %+v", got)
	}
}

func TestExpandBarrierIsGlobalToStart(t *testing.T) {
	ctx := context.Background()
	exp, cs, es := richFixture(t)
	// Bar only Ca↔Cb; B keeps a live path to Cc.
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "contested"); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	if err := es.Save(ctx, Edge{From: "Cb", To: "Cc", Weight: 0.6, Source: SourceCoOcur}); err != nil {
		t.Fatalf("link b->c: %v", err)
	}
	_ = cs
	got := mustExpand(t, exp, "Ca")
	for _, r := range got {
		if r.Cluster.ID == "Cb" {
			t.Fatalf("barred cluster leaked at some depth: %+v", got)
		}
	}
	// The bar does not poison the rest of the neighborhood.
	if len(got) != 2 {
		t.Fatalf("neighbors after bar: %d (%+v)", len(got), got)
	}
}

func TestExpandPathwayRanksAboveWeak(t *testing.T) {
	ctx := context.Background()
	exp, _, es := richFixture(t)
	// Cb stays a one-off weak edge; Cd becomes a walked pathway of the SAME
	// weight — only the kind differs.
	if err := LinkQuerySeq(ctx, es, "Ca", "Cd"); err != nil {
		t.Fatalf("qs1: %v", err)
	}
	if err := LinkQuerySeq(ctx, es, "Ca", "Cd"); err != nil {
		t.Fatalf("qs2: %v", err)
	}
	got := mustExpand(t, exp, "Ca")
	var weak, path float64 = -1, -1
	for _, r := range got {
		switch r.Cluster.ID {
		case "Cb":
			weak = r.Score
		case "Cd":
			path = r.Score
		}
	}
	if weak != 0.6 {
		t.Fatalf("weak edge score = %v, want 0.6 (no bonus)", weak)
	}
	if math.Abs(path-0.6*PathwayBonus) > 1e-9 {
		t.Fatalf("pathway score = %v, want %v", path, 0.6*PathwayBonus)
	}
}

// --- helpers -----------------------------------------------------------

func fromTo(t *testing.T, st Store, from, to string) []Edge {
	t.Helper()
	es, err := st.From(context.Background(), from)
	if err != nil {
		t.Fatalf("from %s: %v", from, err)
	}
	out := es[:0]
	for _, e := range es {
		if e.To == to {
			out = append(out, e)
		}
	}
	return out
}

func mustExpand(t *testing.T, exp *Expander, start string) []ExpandResult {
	t.Helper()
	got, err := exp.Expand(context.Background(), ExpandRequest{StartID: start})
	if err != nil {
		t.Fatalf("expand %s: %v", start, err)
	}
	return got
}

package graph

import (
	"context"
	"fmt"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
)

// H4: a barrier edge and a co_occur edge between the same pair used to share
// one document id, so LinkCoOcur overwrote the barrier — the conflict bar
// vanished and Expand served the contested cluster as evidence for the other
// side (P4 / G-pollute violated). Rich kinds now own their id namespace.
func TestCoOcurCannotClobberBarrier(t *testing.T) {
	ctx := context.Background()
	es := NewMemory()
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "claims 128 vs 256"); err != nil {
		t.Fatal(err)
	}
	// A later shared-evidence co-mention between the same pair.
	if err := LinkCoOcur(ctx, es, "Ca", "Cb"); err != nil {
		t.Fatal(err)
	}
	if err := LinkCoOcur(ctx, es, "Ca", "Cb"); err != nil {
		t.Fatal(err)
	}
	barriers := fromToKind(t, es, "Ca", "Cb", KindBarrier)
	if len(barriers) != 1 {
		t.Fatalf("barrier edge lost after co_occur saves: %+v", fromTo(t, es, "Ca", "Cb"))
	}
	if barriers[0].Reason != "claims 128 vs 256" {
		t.Fatalf("barrier reason lost: %q", barriers[0].Reason)
	}
	// Expansion must still refuse the contested neighbour.
	cs := cluster.NewMemory()
	for _, id := range []string{"Ca", "Cb"} {
		if err := cs.Save(ctx, mk(t, id, "t"+id, []float64{1, 0, 0}, 0.8)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewExpander(es, cs).Expand(ctx, ExpandRequest{StartID: "Ca", Direction: "out"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Cluster.ID == "Cb" {
			t.Fatalf("barred neighbour served as evidence: %+v", r)
		}
	}
}

// The barrier must not be counted as co-retrieval signal: its weight is 1 by
// construction, which would make a contested pair look like the strongest
// co-occurrence partner (tidy's co signal is derived from this).
func TestCoOccurSignalsIgnoreBarriers(t *testing.T) {
	ctx := context.Background()
	es := NewMemory()
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "contested"); err != nil {
		t.Fatal(err)
	}
	if w := CoOccurWeight(ctx, es, "Ca", "Cb"); w != 0 {
		t.Fatalf("barrier counted as co_occur weight: %v", w)
	}
	if ps := CoOccurPartners(ctx, es, "Ca"); len(ps) != 0 {
		t.Fatalf("barrier listed as a co-retrieval partner: %+v", ps)
	}
	// A genuine co_occur edge still registers.
	if err := LinkCoOcur(ctx, es, "Ca", "Cc"); err != nil {
		t.Fatal(err)
	}
	if w := CoOccurWeight(ctx, es, "Ca", "Cc"); w <= 0 {
		t.Fatalf("plain co_occur weight lost: %v", w)
	}
	if ps := CoOccurPartners(ctx, es, "Ca"); len(ps) != 1 || ps[0].To != "Cc" {
		t.Fatalf("co-retrieval partner wrong: %+v", ps)
	}
}

// The pathway upgrade must REPLACE the plain query_seq edge, not sit beside
// it as a duplicate (rich kinds own their id namespace, so the upgrade carries
// the previous id).
func TestPathwayUpgradeReplacesPlainEdge(t *testing.T) {
	ctx := context.Background()
	es := NewMemory()
	for i := 0; i < 3; i++ {
		if err := LinkQuerySeq(ctx, es, "Ca", "Cb"); err != nil {
			t.Fatal(err)
		}
	}
	edges := fromTo(t, es, "Ca", "Cb")
	if len(edges) != 1 {
		t.Fatalf("upgrade left %d edges, want exactly 1: %+v", len(edges), edges)
	}
	if edges[0].Kind != KindPathway || edges[0].Hits != 3 {
		t.Fatalf("upgraded edge wrong: %+v", edges[0])
	}
}

// A plain edge keeps its historical id, so pre-P4 documents stay readable.
func TestEdgeIDBackwardCompatible(t *testing.T) {
	if got := edgeID("A", "B", SourceCoOcur); got != "e:A-B-co_occur" {
		t.Fatalf("plain edge id drifted: %q", got)
	}
	if got := edgeIDWithKind("A", "B", SourceCoOcur, ""); got != edgeID("A", "B", SourceCoOcur) {
		t.Fatalf("empty kind must reuse the plain id: %q", got)
	}
	for _, kind := range []string{KindPathway, KindBarrier} {
		if got := edgeIDWithKind("A", "B", SourceCoOcur, kind); got == edgeID("A", "B", SourceCoOcur) {
			t.Fatalf("kind %q must own its id namespace", kind)
		}
	}
}

func fromToKind(t *testing.T, st Store, from, to, kind string) []Edge {
	t.Helper()
	var out []Edge
	for _, e := range fromTo(t, st, from, to) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// A hub cluster's adjacency must page too. queryBy used to cap one page at 500,
// so a barrier edge past the cap was invisible to startBarriers — and the
// contested neighbour came back as evidence (the H4 hole, at scale), even
// though the unit test above passes on a small fixture.
func TestBarrierSurvivesPastOneAdjacencyPage(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const coll = "clus_weak_edges"
	if err := engine.EnsureCollection(ctx, coll); err != nil {
		t.Fatal(err)
	}
	es := NewCumuStore(engine, coll)
	cs := cluster.NewMemory()
	for _, id := range []string{"Ca", "Cb"} {
		if err := cs.Save(ctx, mk(t, id, "t"+id, []float64{1, 0, 0}, 0.8)); err != nil {
			t.Fatal(err)
		}
	}
	// The contested pair, plus enough other edges to push it past one page.
	if err := LinkBarrier(ctx, es, "Ca", "Cb", "claims 128 vs 256"); err != nil {
		t.Fatal(err)
	}
	const pad = 520
	for i := 0; i < pad; i++ {
		if err := es.Save(ctx, Edge{From: "Ca", To: fmt.Sprintf("Cpad%d", i),
			Weight: 0.6, Source: SourceCoOcur}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cs.Save(ctx, mk(t, "Cpad0", "pad", []float64{0, 1, 0}, 0.8)); err != nil {
		t.Fatal(err)
	}
	from, err := es.From(ctx, "Ca")
	if err != nil {
		t.Fatal(err)
	}
	if len(from) <= 500 {
		t.Fatalf("precondition: adjacency must exceed one page, got %d", len(from))
	}
	barriers := 0
	for _, e := range from {
		if e.Kind == KindBarrier {
			barriers++
		}
	}
	if barriers == 0 {
		t.Fatal("barrier edge lost past the first adjacency page")
	}
	got, err := NewExpander(es, cs).Expand(ctx, ExpandRequest{StartID: "Ca", Direction: "out"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Cluster.ID == "Cb" {
			t.Fatalf("barred neighbour served as evidence: %+v", r)
		}
	}
}

package graph

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// countingPort counts Query calls so a cache hit is observable, not assumed.
type countingPort struct {
	cumulite.Port
	queries atomic.Int64
}

func (p *countingPort) Query(ctx context.Context, coll string, q contract.Query) (*contract.QueryResult, error) {
	p.queries.Add(1)
	return p.Port.Query(ctx, coll, q)
}

func openEdgeStore(t *testing.T) (*countingPort, *CumuStore, context.Context) {
	t.Helper()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	cp := &countingPort{Port: eng}
	ctx := context.Background()
	if err := cp.EnsureCollection(ctx, "clus_weak_edges"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return cp, NewCumuStore(cp, "clus_weak_edges"), ctx
}

func TestCumuStoreCachesAdjacency(t *testing.T) {
	cp, st, ctx := openEdgeStore(t)
	if err := st.Save(ctx, Edge{From: "A", To: "B", Weight: 0.5, Source: SourceQuerySeq}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.Save(ctx, Edge{From: "A", To: "C", Weight: 0.4, Source: SourceEmbedSim}); err != nil {
		t.Fatalf("save: %v", err)
	}

	first, err := st.From(ctx, "A")
	if err != nil || len(first) != 2 {
		t.Fatalf("from: %v %+v", err, first)
	}
	if cp.queries.Load() != 1 {
		t.Fatalf("first From must hit the engine once, got %d", cp.queries.Load())
	}
	second, err := st.From(ctx, "A")
	if err != nil || len(second) != 2 {
		t.Fatalf("cached from: %v %+v", err, second)
	}
	if cp.queries.Load() != 1 {
		t.Fatalf("second From must be served from cache, queries=%d", cp.queries.Load())
	}
	// Inbound view is cached independently.
	if _, err := st.To(ctx, "B"); err != nil || len(second) == 0 {
		t.Fatalf("to: %v", err)
	}
	if cp.queries.Load() != 2 {
		t.Fatalf("first To must hit the engine once, queries=%d", cp.queries.Load())
	}
	if _, err := st.To(ctx, "B"); err != nil {
		t.Fatalf("cached to: %v", err)
	}
	if cp.queries.Load() != 2 {
		t.Fatalf("second To must be cached, queries=%d", cp.queries.Load())
	}
	// An unknown node stays a miss (empty result also caches nothing new).
	if got, err := st.From(ctx, "Z"); err != nil || len(got) != 0 {
		t.Fatalf("unknown node: %v %+v", err, got)
	}
}

func TestCumuStoreSaveInvalidates(t *testing.T) {
	cp, st, ctx := openEdgeStore(t)
	if err := st.Save(ctx, Edge{From: "A", To: "B", Weight: 0.5, Source: SourceQuerySeq}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := st.From(ctx, "A"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	before := cp.queries.Load()

	// A new edge invalidates A's outbound view.
	if err := st.Save(ctx, Edge{From: "A", To: "D", Weight: 0.3, Source: SourceCoOcur}); err != nil {
		t.Fatalf("save2: %v", err)
	}
	got, err := st.From(ctx, "A")
	if err != nil || len(got) != 2 {
		t.Fatalf("after save: %v %+v", err, got)
	}
	if cp.queries.Load() != before+1 {
		t.Fatalf("save must invalidate: queries %d -> %d", before, cp.queries.Load())
	}

	// Re-saving the same edge id updates the weight through the cache.
	if err := st.Save(ctx, Edge{From: "A", To: "B", Weight: 0.9, Source: SourceQuerySeq}); err != nil {
		t.Fatalf("save3: %v", err)
	}
	got, err = st.From(ctx, "A")
	if err != nil {
		t.Fatalf("after weight save: %v", err)
	}
	for _, e := range got {
		if e.To == "B" && e.Weight != 0.9 {
			t.Fatalf("stale weight after re-save: %+v", e)
		}
	}
	// The inbound view of the new target sees it too.
	in, err := st.To(ctx, "D")
	if err != nil || len(in) != 1 || in[0].From != "A" {
		t.Fatalf("inbound after save: %v %+v", err, in)
	}
}

func TestCumuStoreDeleteInvalidates(t *testing.T) {
	_, st, ctx := openEdgeStore(t)
	e := Edge{From: "A", To: "B", Weight: 0.5, Source: SourceQuerySeq}
	if err := st.Save(ctx, e); err != nil {
		t.Fatalf("save: %v", err)
	}
	id := edgeID("A", "B", SourceQuerySeq)
	if _, err := st.From(ctx, "A"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if err := st.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := st.From(ctx, "A")
	if err != nil || len(got) != 0 {
		t.Fatalf("after delete: %v %+v", err, got)
	}
	in, err := st.To(ctx, "B")
	if err != nil || len(in) != 0 {
		t.Fatalf("inbound after delete: %v %+v", err, in)
	}
}

func TestCumuStoreInstancesAreShared(t *testing.T) {
	cp, st, _ := openEdgeStore(t)
	again := NewCumuStore(cp, "clus_weak_edges")
	if again != st {
		t.Fatalf("NewCumuStore must memoize per (engine, collection)")
	}
	// A different collection on the same engine is a different store.
	other := NewCumuStore(cp, "t1:clus_weak_edges")
	if other == st {
		t.Fatalf("different collections must not share an instance")
	}
	// A different engine is isolated (another temp store in-process).
	eng2, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine2: %v", err)
	}
	defer eng2.Close()
	if NewCumuStore(eng2, "clus_weak_edges") == st {
		t.Fatalf("different engines must not share an instance")
	}
}

func TestCumuStoreConcurrentReadWrite(t *testing.T) {
	_, st, ctx := openEdgeStore(t)
	if err := st.Save(ctx, Edge{From: "A", To: "B", Weight: 0.5, Source: SourceQuerySeq}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	stop := time.Now().Add(200 * time.Millisecond)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for time.Now().Before(stop) {
				from, _ := st.From(ctx, "A")
				_, _ = st.To(ctx, "B")
				if i%2 == 0 {
					_ = st.Save(ctx, Edge{From: "A", To: "B", Weight: 0.5, Source: SourceQuerySeq})
				} else {
					_ = st.Delete(ctx, edgeID("A", "C", SourceCoOcur)) // missing: exercises the delete path
				}
				_ = from
			}
		}(i)
	}
	wg.Wait()
	// Final state is consistent: exactly the seeded edge.
	got, err := st.From(ctx, "A")
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if len(got) != 1 || got[0].To != "B" {
		t.Fatalf("final state: %+v", got)
	}
}

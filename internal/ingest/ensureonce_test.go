package ingest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/source"
)

// countingPort wraps a real engine and counts the collection declarations, so
// "declared once, never per ingest" (D7) is assertable rather than assumed.
type countingPort struct {
	cumulite.Port
	mu    sync.Mutex
	calls []string
}

func (c *countingPort) EnsureCollection(ctx context.Context, name string) error {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
	return c.Port.EnsureCollection(ctx, name)
}

func (c *countingPort) declared() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// undeclaredStore is the L9 shape: a real engine with NO suite collections
// declared — exactly what an operator gets who points the CLI at a fresh
// store and runs `put` without reading the docs first.
func undeclaredStore(t *testing.T) (*Store, *countingPort) {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	cp := &countingPort{Port: engine}
	return New(cp, "clus_sources", "clus_evidence", "clus_clusters", ""), cp
}

// L9: the CLI put face was the one write path that never declared its
// collections, so the first put on a fresh store died on a raw
// "collection not found" and the operator had to know to run `ensure` first.
// The Job path and the serve face both declared; this pins the third.
func TestPutOnAFreshStoreDeclaresAndSucceeds(t *testing.T) {
	st, cp := undeclaredStore(t)
	if cp.declared() != 0 {
		t.Fatalf("precondition: a fresh store must have nothing declared, got %d", cp.declared())
	}

	res, err := st.Put(context.Background(), source.Source{
		Title: "部署手册", BusinessKey: "handbook", SourceType: "md",
		Body: "连接池最大 128，超时 30 秒。",
	})
	if err != nil {
		t.Fatalf("put on an undeclared store must succeed, got: %v", err)
	}
	if res.ID == "" {
		t.Fatalf("put returned no document id: %+v", res)
	}
	if cp.declared() == 0 {
		t.Fatal("put did not declare the suite collections")
	}
}

// D7: collection shape is declared once, never per ingest — Ensure writes a
// changelog record, so a per-document Ensure would add a write per document.
func TestEnsureOnceDeclaresOnlyOnTheFirstWrite(t *testing.T) {
	st, cp := undeclaredStore(t)
	for i := 0; i < 5; i++ {
		if _, err := st.Put(context.Background(), source.Source{
			Title: "手册", BusinessKey: "k", SourceType: "md", Body: "内容。",
		}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// sources + evidence + clusters = 3, and that must be all we ever paid.
	if got := cp.declared(); got != 3 {
		t.Fatalf("declared %d collections over 5 puts, want 3 (declared once, then memoized)", got)
	}
}

// Only success is remembered. A transient failure (cancelled context, locked
// store) must not poison the Store for its lifetime.
func TestEnsureOnceRetriesAfterAFailure(t *testing.T) {
	st, cp := undeclaredStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a context that is already dead

	if err := st.EnsureOnce(ctx); err == nil {
		t.Skip("engine accepted a cancelled context; nothing to assert about retry")
	}
	if st.ensured {
		t.Fatal("a failed EnsureOnce must not mark the store as ensured")
	}
	// A later call with a healthy context must still declare and succeed.
	if err := st.EnsureOnce(context.Background()); err != nil {
		t.Fatalf("EnsureOnce after a failure must retry, got: %v", err)
	}
	if !st.ensured {
		t.Fatal("EnsureOnce did not record success")
	}
	if cp.declared() == 0 {
		t.Fatal("retry did not declare anything")
	}
}

// The Job path calls put() directly and declares for itself, so it must not
// be affected by Put's memo — and must still accept over-cap bodies that the
// synchronous path rejects.
func TestJobPathStillDeclaresForItself(t *testing.T) {
	st, cp := undeclaredStore(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("连接池最大 128。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := st.IngestFiles(context.Background(), dir, false, "job1")
	if err != nil {
		t.Fatalf("job path on an undeclared store must succeed, got: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested=%d, want 1", n)
	}
	if cp.declared() == 0 {
		t.Fatal("job path did not declare the suite collections")
	}
}

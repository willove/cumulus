package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/source"
)

// newTestStore opens a hermetic in-memory engine with the suite's collections
// declared (clus_sources carries the changelog Reconcile consumes).
func newTestStore(t *testing.T) (*Store, cumulite.Port) {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	st := New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(context.Background(), "clus_weak_edges", "clus_cites", "clus_conflicts"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return st, engine
}

func putKey(t *testing.T, st *Store, key, body string) Result {
	t.Helper()
	res, err := st.Put(context.Background(), source.Source{
		Title: key, BusinessKey: key, SourceType: "md", Body: body,
	})
	if err != nil {
		t.Fatalf("put %q under %q: %v", body, key, err)
	}
	return res
}

// liveOf returns the ACTIVE revisions under one business key. Every revision
// test asserts the same invariant on it: exactly one.
func liveOf(t *testing.T, engine cumulite.Port, key string) []map[string]any {
	t.Helper()
	res, err := engine.Query(context.Background(), "clus_sources", contract.Query{
		Filter: map[string]any{"business_key": key, "status": source.StatusActive},
		Limit:  1000,
	})
	if err != nil {
		t.Fatalf("query live revisions: %v", err)
	}
	return res.Documents
}

// A→B→A: restoring earlier content must be a new revision, not a collision with
// the stale one that still carries those bytes.
func TestPutRestoresEarlierContent(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()
	bodyA, bodyB := "连接池最大 128。", "连接池最大 256。"

	r1 := putKey(t, st, "handbook", bodyA)
	r2 := putKey(t, st, "handbook", bodyB)
	r3 := putKey(t, st, "handbook", bodyA)

	if r1.ID == r2.ID || r2.ID == r3.ID || r1.ID == r3.ID {
		t.Fatalf("revisions must have distinct ids: %q %q %q", r1.ID, r2.ID, r3.ID)
	}
	if r3.Version != 3 || r2.Version != 2 {
		t.Fatalf("versions = %d %d %d, want 1 2 3", r1.Version, r2.Version, r3.Version)
	}
	if r3.Status != "updated" || r3.StaleID != r2.ID {
		t.Fatalf("restore result = %+v, want updated with stale_id %s", r3, r2.ID)
	}
	got, err := st.Get(ctx, r3.ID)
	if err != nil || got == nil || got.Status != source.StatusActive || got.Body != bodyA {
		t.Fatalf("restored revision = %+v, err = %v; want live body %q", got, err, bodyA)
	}
	if live := liveOf(t, engine, "handbook"); len(live) != 1 || live[0]["_id"] != r3.ID {
		t.Fatalf("live revisions = %+v, want exactly %s", live, r3.ID)
	}
}

// A delete is a tombstone, not a ban on the bytes: re-importing them must
// produce a live document instead of reporting "unchanged" against the corpse.
func TestPutAfterDeleteRestoresContent(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()
	body := "连接池最大 128。"

	r1 := putKey(t, st, "handbook", body)
	if err := st.Delete(ctx, r1.ID); err != nil {
		t.Fatal(err)
	}
	r2 := putKey(t, st, "handbook", body)

	if r2.ID == r1.ID {
		t.Fatalf("re-import reused the tombstone id %s", r2.ID)
	}
	if r2.Status != "created" {
		t.Fatalf("re-import status = %q, want created (no live revision existed)", r2.Status)
	}
	got, err := st.Get(ctx, r2.ID)
	if err != nil || got == nil || got.Status != source.StatusActive || got.Body != body {
		t.Fatalf("re-imported revision = %+v, err = %v; want live", got, err)
	}
	if old, err := st.Get(ctx, r1.ID); err != nil || old == nil || old.Status != source.StatusDeleted {
		t.Fatalf("tombstone = %+v, err = %v; want it still deleted", old, err)
	}
	if live := liveOf(t, engine, "handbook"); len(live) != 1 || live[0]["_id"] != r2.ID {
		t.Fatalf("live revisions = %+v, want exactly %s", live, r2.ID)
	}
}

// An update that failed halfway (new revision written, previous one never
// retired) leaves two live revisions. Re-putting the same bytes must converge
// them, not report "unchanged" and walk away.
func TestPutHealsStrandedLiveRevision(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()

	r1 := putKey(t, st, "handbook", "连接池最大 128。")
	r2 := putKey(t, st, "handbook", "连接池最大 256。")
	// Simulate the interrupted update: the previous revision is live again.
	if _, err := engine.PatchDocument(ctx, "clus_sources", r1.ID, map[string]any{
		"$set": map[string]any{"status": source.StatusActive},
	}); err != nil {
		t.Fatal(err)
	}
	if live := liveOf(t, engine, "handbook"); len(live) != 2 {
		t.Fatalf("precondition: %d live revisions, want 2", len(live))
	}

	r3 := putKey(t, st, "handbook", "连接池最大 256。")
	if r3.Status != "unchanged" || r3.ID != r2.ID {
		t.Fatalf("re-put = %+v, want unchanged on %s", r3, r2.ID)
	}
	if live := liveOf(t, engine, "handbook"); len(live) != 1 || live[0]["_id"] != r2.ID {
		t.Fatalf("live revisions = %+v, want the stranded one retired", live)
	}
}

// The current revision must be found by identity, not by whatever the store
// happens to page first: ten revisions must not make the version regress.
func TestPutManyRevisionsKeepsOneLive(t *testing.T) {
	st, engine := newTestStore(t)
	for i := 1; i <= 12; i++ {
		res := putKey(t, st, "handbook", fmt.Sprintf("连接池最大 %d。", i*128))
		if res.Version != i {
			t.Fatalf("revision %d reported version %d", i, res.Version)
		}
	}
	live := liveOf(t, engine, "handbook")
	if len(live) != 1 {
		t.Fatalf("%d live revisions after 12 updates, want 1", len(live))
	}
	if v, _ := live[0]["version"].(float64); int(v) != 12 {
		t.Fatalf("live revision version = %v, want 12", live[0]["version"])
	}
}

// Identical text under two business keys is two documents. Content addressing
// alone would collide them and swallow the second write as "unchanged".
func TestPutSameBodyDifferentKeys(t *testing.T) {
	st, _ := newTestStore(t)
	body := "当事人应当按照约定全面履行自己的义务。"

	a := putKey(t, st, "k1", body)
	b := putKey(t, st, "k2", body)
	if a.ID == b.ID {
		t.Fatalf("identical text under different keys shared the id %s", a.ID)
	}
	if b.Status != "created" {
		t.Fatalf("second key reported %q, want created — its write must not be swallowed", b.Status)
	}
	for _, res := range []Result{a, b} {
		got, err := st.Get(context.Background(), res.ID)
		if err != nil || got == nil || got.Status != source.StatusActive || got.Body != body {
			t.Fatalf("revision %s = %+v, err = %v; want live", res.ID, got, err)
		}
	}
}

// A finished job must resume at its cursor instead of re-walking the corpus,
// and the job document must still report real cumulative progress.
func TestIngestFilesResumeSurvivesJobDoc(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# "+name+"\n路由器配置说明。\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.IngestFiles(ctx, dir, false, "fg")
	if err != nil || first != 2 {
		t.Fatalf("first run processed %d, err = %v; want 2", first, err)
	}
	second, err := st.IngestFiles(ctx, dir, false, "fg")
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("second run processed %d files, want 0 — the job state write clobbered the cursor", second)
	}
	d, err := st.GetJobDoc(ctx, "fg")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "done" || d.Done != d.Total || d.Total != 2 {
		t.Fatalf("job doc = %+v, want state done with 2/2 progress", d)
	}
}

// A failed change must not be consumed: the cursor stays where the last fully
// successful page ended, so the next run retries the work instead of skipping
// it forever.
func TestReconcileDoesNotConsumeFailedChange(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	// Fail the cluster patch once, then let it through.
	p := &patchedOncePort{Port: engine, failID: "c:handbook"}
	st := New(p, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(ctx, "clus_weak_edges", "clus_cites", "clus_conflicts"); err != nil {
		t.Fatal(err)
	}
	r := putKey(t, st, "handbook", "连接池最大 128。")
	if _, err := engine.Insert(ctx, "clus_clusters", []map[string]any{
		{"_id": "c:handbook", "source_id": r.ID, "lifecycle": "established"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, r.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Reconcile(ctx); err == nil {
		t.Fatal("an injected patch failure must surface, not be swallowed")
	}
	if raw, _ := engine.KVGet(ctx, "clus:reconcile:clus_sources"); len(raw) != 0 {
		t.Fatalf("cursor = %q after a failed change; the page must not be consumed", raw)
	}
	rep, err := st.Reconcile(ctx) // fault cleared
	if err != nil {
		t.Fatal(err)
	}
	if rep.ClustersMarked != 1 {
		t.Fatalf("retry marked %d clusters, want 1 — the failed change was skipped", rep.ClustersMarked)
	}
}

// patchedOncePort delegates everything to the real engine but fails the first
// PatchDocument for one id, so a test can interrupt a reconcile midway.
type patchedOncePort struct {
	cumulite.Port
	failID string
	failed bool
}

func (p *patchedOncePort) PatchDocument(ctx context.Context, coll, id string, update map[string]any) (map[string]any, error) {
	if id == p.failID && !p.failed {
		p.failed = true
		return nil, fmt.Errorf("injected patch failure on %s", id)
	}
	return p.Port.PatchDocument(ctx, coll, id, update)
}

// A source purged out of band produces a delete change with nothing left to
// read. The downstream cleanup keys off that id and must still run.
func TestReconcileCleansUpAfterPhysicalRemoval(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()
	r := putKey(t, st, "handbook", "连接池最大 128。")
	if _, err := engine.Insert(ctx, "clus_evidence", []map[string]any{
		{"_id": "ev:handbook", "doc_id": r.ID, "status": "live"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Insert(ctx, "clus_clusters", []map[string]any{
		{"_id": "c:handbook", "source_id": r.ID, "lifecycle": "established"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.DeleteDocument(ctx, "clus_sources", r.ID); err != nil {
		t.Fatal(err)
	}

	rep, err := st.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.EvInvalidated != 1 || rep.ClustersMarked != 1 {
		t.Fatalf("report = %+v, want 1 evidence invalidated and 1 cluster marked", rep)
	}
	ev, err := engine.GetDocument(ctx, "clus_evidence", "ev:handbook")
	if err != nil || ev["status"] != "stale" {
		t.Fatalf("evidence = %+v, err = %v; want status stale", ev, err)
	}
	cl, err := engine.GetDocument(ctx, "clus_clusters", "c:handbook")
	if err != nil || cl["lifecycle"] != "emerging" {
		t.Fatalf("cluster = %+v, err = %v; want lifecycle emerging", cl, err)
	}
}

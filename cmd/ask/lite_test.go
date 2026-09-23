package main

import (
	"context"
	"testing"
	"time"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
	"github.com/cumubase/cumulite"
)

// The portability claim, executed: the whole suite — put, ensure, ensure-embed,
// reconcile, sessions, cluster/graph reuse and a real FAST search — runs
// against the embedded cumulite engine with no cumudb server anywhere in the
// process. Before -lite this was only an architecture intention; now it is a
// test that fails the moment a store path grows a dependency outside Port.
func TestAskRunsOnCumuliteWithoutServer(t *testing.T) {
	t.Setenv("AIGATE_BASE_URL", "") // offline gates: no network in tests
	ctx := context.Background()

	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer engine.Close()

	st := ingest.New(engine, "ask_sources", "ask_evidence", "ask_clusters", "")
	if _, err := st.Ensure(ctx, "ask_weak_edges", "ask_cites", "ask_conflicts"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	bodies := []string{
		"当事人应当按照约定全面履行自己的义务",
		"民事主体从事民事活动应当遵循诚信原则",
	}
	var srcIDs []string
	for _, body := range bodies {
		res, err := st.Put(ctx, source.Source{
			Title: body, Body: body,
			SourceType: "md", Status: source.StatusActive,
			IngestedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("put %q: %v", body, err)
		}
		srcIDs = append(srcIDs, res.ID)
	}

	active, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatalf("active sources: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active sources = %d, want 2", len(active))
	}

	// The two puts happened after ensure turned the changelog on, so a
	// downstream reconcile must see exactly them.
	rep, err := st.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Scanned != 2 || rep.Cursor == 0 {
		t.Fatalf("reconcile = %+v, want 2 scanned past cursor 0", rep)
	}
	// idempotent re-run: nothing new
	rep2, err := st.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile again: %v", err)
	}
	if rep2.Scanned != 0 || rep2.Cursor != rep.Cursor {
		t.Fatalf("second reconcile = %+v, want 0 scanned at cursor %d", rep2, rep.Cursor)
	}

	// ensure-embed: vector index declaration + backfill through the same
	// EmbedderFn the CLI passes.
	dim := 4
	embed := func(_ context.Context, texts []string) ([][]float64, error) {
		out := make([][]float64, len(texts))
		for i, text := range texts {
			out[i] = []float64{float64(len(text) % 7), 1, 0, 0}
		}
		return out, nil
	}
	n, err := st.EnsureEmbed(ctx, embed, dim, "test-hash", 64)
	if err != nil {
		t.Fatalf("ensure embed: %v", err)
	}
	if n != 2 {
		t.Fatalf("embedded = %d, want 2", n)
	}
	res, err := engine.KNN(ctx, "ask_sources", client.KNNRequest{
		Field: "body_embed", Vector: []float64{3, 1, 0, 0}, K: 2, Metric: "cosine",
	})
	if err != nil {
		t.Fatalf("knn: %v", err)
	}
	if len(res.Documents) != 2 || len(res.Distances) != 2 {
		t.Fatalf("knn docs = %d distances = %d, want 2", len(res.Documents), len(res.Distances))
	}
	if res.Distances[0] > res.Distances[1] {
		t.Fatalf("distances not ascending: %v", res.Distances)
	}

	// Reuse stores: cluster and weak-edge round-trips through the engine.
	cs := cluster.NewCumuStore(engine, "ask_clusters")
	now := time.Now().UTC()
	if err := cs.Save(ctx, cluster.Cluster{
		ID: "Ctest1", TopicKey: "tk1", Name: "履行义务", Content: "全面履行",
		Queries: []string{"全面履行义务"}, LevelKeys: []cluster.LevelKey{{Level: "scenario", Text: "全面履行义务"}},
		Lifecycle: "stable", Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("cluster save: %v", err)
	}
	found, err := cs.FindByTopic(ctx, "tk1")
	if err != nil {
		t.Fatalf("cluster find: %v", err)
	}
	if len(found) != 1 || found[0].Name != "履行义务" {
		t.Fatalf("cluster find = %+v", found)
	}

	gs := graph.NewCumuStore(engine, "ask_weak_edges")
	if err := gs.Save(ctx, graph.Edge{ID: "e1", From: srcIDs[0], To: srcIDs[1], Weight: 0.5, Source: "test"}); err != nil {
		t.Fatalf("edge save: %v", err)
	}
	edges, err := gs.From(ctx, srcIDs[0])
	if err != nil {
		t.Fatalf("edge from: %v", err)
	}
	if len(edges) != 1 || edges[0].To != srcIDs[1] {
		t.Fatalf("edge from = %+v", edges)
	}

	// Sessions ride the KV path.
	sess := sessionStore{c: engine}
	doc, err := sess.appendTurn(ctx, "sess-1", "lite", "全面履行义务", "当事人应当全面履行")
	if err != nil {
		t.Fatalf("session appendTurn: %v", err)
	}
	list, err := sess.list(ctx)
	if err != nil {
		t.Fatalf("session list: %v", err)
	}
	if len(list) != 1 || list[0].ID != doc.ID {
		t.Fatalf("session list = %+v", list)
	}

	// The claim that carries the test: a real FAST search answers from the
	// embedded store and cites the right source.
	ss, err := newSearchStack(ctx, engine, st, "ask_sources", SearchOptions{})
	if err != nil {
		t.Fatalf("search stack: %v", err)
	}
	out, err := runSearch(ctx, ss, "全面履行义务")
	if err != nil {
		t.Fatalf("runSearch: %v", err)
	}
	if out.Answer.Skipped || len(out.Answer.Samples) == 0 {
		t.Fatalf("search returned no sample: %+v", out.Answer)
	}
	if out.Answer.Samples[0].Source != srcIDs[0] {
		t.Fatalf("search cited %q, want %q", out.Answer.Samples[0].Source, srcIDs[0])
	}
}

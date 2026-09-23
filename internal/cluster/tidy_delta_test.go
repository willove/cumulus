package cluster

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

func mustVec(t *testing.T, e Embedder, s string) []float64 {
	t.Helper()
	vs, err := e.Embed(context.Background(), []string{s})
	if err != nil || len(vs) != 1 {
		t.Fatalf("embed %q: %v %d", s, err, len(vs))
	}
	return vs[0]
}

// 2.6: a sweep's report must carry the before/after population snapshot so
// "fewer clusters" is never mistaken for "better clusters".
func TestTidyReportCarriesDelta(t *testing.T) {
	ctx := context.Background()
	emb := Local{N: 32}
	// Three clusters: two same-domain near-duplicates (foldable), one other.
	a := Cluster{ID: "Ca", TopicKey: "k1", Name: "a", Content: "连接池最大 128。",
		Queries: []string{"连接池最大是多少", "池上限"}, Embed: mustVec(t, emb, "连接池最大 128"),
		Confidence: 0.8, Hotness: 0.6, Lifecycle: LifecycleStable,
		Evidence: []mcs.Sample{
			{Source: "src:a", Start: 0, End: 8, Content: "连接池最大 128"},
			{Source: "src:a", Start: 20, End: 30, Content: "超时 30 秒"},
		}}
	b := Cluster{ID: "Cb", TopicKey: "k2", Name: "b", Content: "连接池上限 128。",
		Queries: []string{"连接池上限是多少"}, Embed: mustVec(t, emb, "连接池上限 128"),
		Confidence: 0.6, Hotness: 0.5, Lifecycle: LifecycleStable,
		Evidence: []mcs.Sample{{Source: "src:b", Start: 0, End: 10, Content: "连接池上限 128"}}}
	other := Cluster{ID: "Cc", TopicKey: "k3", Name: "c", Content: "部署机房在广州。",
		Queries: []string{"机房在哪"}, Embed: mustVec(t, emb, "部署机房在广州"),
		Confidence: 0.9, Hotness: 0.4, Lifecycle: LifecycleStable}

	st := NewMemory()
	for _, c := range []Cluster{a, b, other} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := Tidy(ctx, st, emb, 0.2, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged == 0 {
		t.Skip("fixture produced no fold — cosine too low under the offline embedder")
	}
	d := rep.Delta
	if d == nil {
		t.Fatal("report must carry a delta")
	}
	if d.ClustersBefore != 3 || d.ClustersAfter >= d.ClustersBefore {
		t.Fatalf("delta must show the fold: %+v", d)
	}
	if d.EvidenceAfter < d.EvidenceBefore {
		t.Fatalf("a fold must not lose evidence windows: %+v", d)
	}
	if d.QueriesAfter < d.QueriesBefore {
		t.Fatalf("a fold must not lose queries: %+v", d)
	}
}

// 2.6: a dry-run must project the exemplar fold, not report a no-op.
func TestTidyDryRunDeltaIsProjected(t *testing.T) {
	ctx := context.Background()
	emb := Local{N: 32}
	a := Cluster{ID: "Ca", TopicKey: "k1", Name: "a", Content: "连接池最大 128。",
		Queries: []string{"连接池最大是多少"}, Embed: mustVec(t, emb, "连接池最大 128"),
		Confidence: 0.8, Hotness: 0.6, Lifecycle: LifecycleStable,
		Evidence: []mcs.Sample{{Source: "src:a", Start: 0, End: 8, Content: "连接池最大 128"}}}
	b := Cluster{ID: "Cb", TopicKey: "k2", Name: "b", Content: "连接池上限 128。",
		Queries: []string{"连接池上限是多少"}, Embed: mustVec(t, emb, "连接池上限 128"),
		Confidence: 0.6, Hotness: 0.5, Lifecycle: LifecycleStable,
		Evidence: []mcs.Sample{{Source: "src:b", Start: 0, End: 10, Content: "连接池上限 128"}}}
	st := NewMemory()
	for _, c := range []Cluster{a, b} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := Tidy(ctx, st, emb, 0.2, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Merged != 0 {
		t.Fatalf("dry-run must change nothing: %+v", rep)
	}
	d := rep.Delta
	if d == nil || !d.Projected {
		t.Fatalf("dry-run delta must be marked projected: %+v", d)
	}
	if d.ClustersAfter != d.ClustersBefore-1 {
		t.Fatalf("projection must show the would-be fold: %+v", d)
	}
	// The store itself must be untouched.
	all, _ := st.All(ctx)
	if len(all) != 2 {
		t.Fatalf("dry-run wrote to the store: %d clusters", len(all))
	}
}

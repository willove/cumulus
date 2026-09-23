package kb

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
)

// a cross-topic cluster with one strong shared level key must NOT be
// returned as a reused answer; L0 still runs and the write path may merge.
func TestCrossTopicNearIsMergeNotReuse(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()

	// Seed a cross-topic cluster whose scenario key matches the query family.
	other := cluster.Cluster{
		ID: "Ccross", TopicKey: cluster.TopicKey("数据库连接池上限配置"),
		Name: "上限配置", Content: "连接池上限 128。",
		Queries: []string{"数据库连接池上限配置"},
		Embed:   mustEmbed(t, e, "数据库连接池上限配置"),
		LevelKeys: []cluster.LevelKey{
			{Level: cluster.KeyScenario, Text: "连接池最大连接数"},
		},
		Lifecycle: cluster.LifecycleEmerging,
		Evidence: []mcs.Sample{{
			Source: "src:cfg", Start: 0, End: 10, Content: "连接池最大 128", Score: 9,
		}},
	}
	if err := st.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	// Ensure the write path sees this cluster as a merge candidate.
	e.MergeTheta = 0.3
	e.ReuseTheta = 0.99 // block same-topic reuse so we exercise the L0 path

	r, err := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reused {
		t.Fatalf("cross-topic near must not be returned as reuse: %+v", r)
	}
	// Either merged into the cross cluster or created/merged a same-topic one —
	// never silently answered from the foreign cluster alone without L0.
	if r.ClusterID == "" && !r.Persisted {
		t.Fatalf("expected persist/merge after L0: %+v", r)
	}
	if r.Sampled == 0 && !r.Reused {
		t.Fatalf("L0 must sample when not reusing: %+v", r)
	}
}

// Same-topic reuse still works under max-over-keys (regression).
func TestSameTopicReuseStillWorks(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()
	r1, err := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if err != nil || !r1.Persisted {
		t.Fatalf("seed: %+v %v", r1, err)
	}
	r2, err := e.Ask(ctx, "连接池最大连接数是多大", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused || r2.ClusterID != r1.ClusterID {
		t.Fatalf("same-topic reuse broken: %+v", r2)
	}
}

func mustEmbed(t *testing.T, e *Engine, s string) []float64 {
	t.Helper()
	vs, err := e.Embedder.Embed(context.Background(), []string{s})
	if err != nil || len(vs) != 1 {
		t.Fatalf("embed %q: %v %d", s, err, len(vs))
	}
	return vs[0]
}

package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

func fixtureSources() []source.Source {
	body := strings.Repeat("填充无关内容 padding padding padding。\n", 20) +
		"关键配置：连接池最大 128，超时 30 秒。\n" +
		strings.Repeat("填充无关内容 padding padding padding。\n", 20)
	return []source.Source{
		source.New("配置手册", "md", "file://cfg", "cfg", "zh", body, nil),
	}
}

// second synonymous query reuses the cluster with 0 samples.
func TestGateBReuseZeroSampling(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()

	r1, err := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Persisted || r1.ClusterID == "" {
		t.Fatalf("first ask must persist a cluster: %+v", r1)
	}
	if r1.Reused {
		t.Fatal("first ask is not reuse")
	}

	r2, err := e.Ask(ctx, "连接池最大连接数是多大", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused {
		t.Fatalf("synonym must reuse (G-id/门B), got %+v", r2)
	}
	if r2.Sampled != 0 {
		t.Fatalf("reuse must sample 0 windows, got %d", r2.Sampled)
	}
	if r2.ClusterID != r1.ClusterID {
		t.Fatalf("reuse must hit the same cluster %s vs %s", r2.ClusterID, r1.ClusterID)
	}
	// Evolution: version bumps, embed recomputed from query set.
	if r2.ClusterVer <= r1.ClusterVer {
		t.Fatalf("evolve must bump version %d -> %d", r1.ClusterVer, r2.ClusterVer)
	}
}

// many paraphrases do not fracture (split_cap).
func TestGateBNoFractureUnderParaphrase(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()
	queries := []string{
		"连接池最大连接数是多少",
		"连接池最大连接数是多大",
		"最大连接数 连接池",
		"连接池最大连接数",
	}
	var id string
	for _, q := range queries {
		r, err := e.Ask(ctx, q, srcs)
		if err != nil {
			t.Fatal(err)
		}
		if r.ClusterID == "" {
			t.Fatalf("no cluster for %q", q)
		}
		if id == "" {
			id = r.ClusterID
		} else if r.ClusterID != id && !r.Merged {
			t.Fatalf("paraphrase fractured: %s vs %s for %q", id, r.ClusterID, q)
		}
	}
	all, _ := st.All(ctx)
	n := 0
	for _, c := range all {
		if c.TopicKey == cluster.TopicKey("连接池最大连接数是多少") {
			n++
		}
	}
	if n > cluster.DefaultSplitCap {
		t.Fatalf("split_cap=%d exceeded: %d clusters", cluster.DefaultSplitCap, n)
	}
}

// drop all clusters and rebuild yields the same stable id.
func TestGateBDropRebuild(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()
	r1, _ := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	_ = st.Delete(ctx, r1.ClusterID)
	if got, _ := st.Get(ctx, r1.ClusterID); got != nil {
		t.Fatal("delete failed")
	}
	r2, _ := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if r2.ClusterID != r1.ClusterID {
		t.Fatalf("rebuild id drift: %s vs %s", r1.ClusterID, r2.ClusterID)
	}
}

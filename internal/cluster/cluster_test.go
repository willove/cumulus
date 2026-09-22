package cluster

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

func embedOf(t *testing.T, e Embedder, s string) []float64 {
	t.Helper()
	vs, err := e.Embed(context.Background(), []string{s})
	if err != nil || len(vs) != 1 {
		t.Fatalf("embed: %v %d", err, len(vs))
	}
	return vs[0]
}

// G-id: paraphrases sharing vocabulary land on one topic key (no fracture).
func TestGIDStableIdentity(t *testing.T) {
	k1 := TopicKey("连接池最大连接数是多少")
	k2 := TopicKey("连接池最大连接数是多大")
	k3 := TopicKey("最大连接数 连接池")
	if k1 != k2 {
		t.Fatalf("paraphrase fractured topic: %s vs %s", k1, k2)
	}
	if k1 != k3 {
		t.Fatalf("reordered query fractured topic: %s vs %s", k1, k3)
	}
	kOther := TopicKey("部署机房在一个么城市")
	if k1 == kOther {
		t.Fatal("distinct topics must not collide")
	}
}

func TestGIDSplitCap(t *testing.T) {
	key := TopicKey("连接池最大连接数")
	a := New(key, "a", "连接池 128", "q1", "s1", nil, []float64{1, 0}, 0.8)
	b := New(key, "b", "连接池 256", "q2", "s2", nil, []float64{0.9, 0.1}, 0.7)
	got := SplitCap([]Cluster{a, b}, key, 1)
	if len(got) != 1 {
		t.Fatalf("split_cap=1 want 1 cluster, got %d", len(got))
	}
}

// G-merge: a near hit folds into the existing cluster instead of spawning.
func TestGMergeFoldsIntoExisting(t *testing.T) {
	e := Local{N: 32}
	key := TopicKey("连接池最大连接数")
	base := New(key, "pool", "连接池默认 128", "连接池最大连接数是多少", "s1", nil,
		embedOf(t, e, "连接池最大连接数是多少"), 0.8)
	paraphrase := "连接池最大连接数是多大"
	qe := embedOf(t, e, paraphrase)
	if !CanMerge(&base, qe, 0.5) {
		t.Fatalf("paraphrase must be mergeable; cos=%v", Cosine(base.Embed, qe))
	}
	base.Evolve(paraphrase, embedOf(t, e, stringsJoin(base.Queries)))
	if base.Version < 2 {
		t.Fatalf("merge must bump version, got %d", base.Version)
	}
	if len(base.Queries) != 2 {
		t.Fatalf("merge must retain queries, got %v", base.Queries)
	}
}

// G-idem: replaying the same query does not inflate the query list; hotness caps.
func TestGIdempotentEvolve(t *testing.T) {
	e := Local{N: 32}
	key := TopicKey("连接池")
	c := New(key, "p", "x", "连接池参数", "s", nil, embedOf(t, e, "连接池参数"), 0.5)
	for i := 0; i < 12; i++ {
		c.Evolve("连接池参数", embedOf(t, e, "连接池参数"))
	}
	if len(c.Queries) != 1 {
		t.Fatalf("same query must not append, got %v", c.Queries)
	}
	if c.Hotness != 1.0 {
		t.Fatalf("hotness must cap at 1.0, got %v", c.Hotness)
	}
}

// G-pollute: a cluster that does not overlap the question must not reuse.
func TestGPolluteRejectsUnrelated(t *testing.T) {
	c := New(TopicKey("缓存"), "cache", "缓存穿透与雪崩", "缓存穿透怎么处理", "s",
		nil, []float64{1}, 0.9)
	if RelevanceGate("连接池最大连接数", c, 0.3) {
		t.Fatal("unrelated cluster must fail the relevance gate")
	}
	if !RelevanceGate("缓存穿透怎么处理", c, 0.3) {
		t.Fatal("on-topic cluster must pass the relevance gate")
	}
	// Deprecated never reuses.
	c.Lifecycle = LifecycleDeprecated
	e := Local{N: 8}
	qe := embedOf(t, e, "缓存穿透")
	if ShouldReuse(&c, qe, 0.1) {
		t.Fatal("deprecated cluster must not reuse")
	}
}

// G-drop: clusters are accelerators — delete them and identity still rebuilds.
func TestGDropRebuilds(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	e := Local{N: 32}
	key := TopicKey("连接池最大连接数")
	c := New(key, "pool", "128", "连接池最大连接数是多少", "s1", nil, embedOf(t, e, "连接池最大连接数是多少"), 0.8)
	if err := st.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, c.ID); got != nil {
		t.Fatal("cluster must be gone after delete")
	}
	// Rebuild from the same query: same stable ID.
	again := New(key, "pool", "128", "连接池最大连接数是多少", "s1", nil, embedOf(t, e, "连接池最大连接数是多少"), 0.8)
	if again.ID != c.ID {
		t.Fatalf("rebuild must keep stable id %s vs %s", again.ID, c.ID)
	}
}

func TestConfidenceIsComputedNotConstant(t *testing.T) {
	// Sirchmunk writes confidence=0.5 always — we must not.
	ev := []mcs.Sample{{Score: 9}, {Score: 8}}
	mean := (ev[0].Score + ev[1].Score) / 2
	c := New(TopicKey("x"), "x", "x", "q", "s", ev, []float64{1}, mcs.Confidence(mean, 1))
	if c.Confidence == 0.5 && mean != 5 {
		t.Fatalf("confidence looks hardcoded: %v", c.Confidence)
	}
	if c.Confidence < 0.8 {
		t.Fatalf("high-score evidence must give high confidence, got %v", c.Confidence)
	}
}

func stringsJoin(qs []string) string {
	out := ""
	for i, q := range qs {
		if i > 0 {
			out += " "
		}
		out += q
	}
	return out
}

package cluster

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

// D.6 / 2.3: one cluster carries keys at several abstraction levels; reuse
// scores max-over-keys so a rephrased query can hit any single key.
func TestDeriveLevelKeysMultiLevel(t *testing.T) {
	c := New(TopicKey("连接池最大连接数是多少"), "连接池配置",
		"连接池最大 128。超时 30 秒。", "连接池最大连接数是多少", "s1",
		[]mcs.Sample{{Content: "连接池默认最大 128，超时 30 秒。"}}, []float64{1, 0}, 0.9)

	levels := map[string]bool{}
	for _, k := range c.LevelKeys {
		levels[k.Level] = true
		if k.Text == "" {
			t.Fatalf("empty key text: %+v", k)
		}
	}
	for _, want := range []string{KeyScenario, KeyOp, KeyAnchor, KeyPrinciple} {
		if !levels[want] {
			t.Fatalf("missing level %q in %+v", want, c.LevelKeys)
		}
	}
}

// max-over-keys: a weak aggregate embed still reuses when ONE level key
// strongly matches the query (no fusion weights).
func TestMaxOverKeysReuseOnSingleKey(t *testing.T) {
	// Embed is deliberately orthogonal to the query (hash of unrelated text).
	c := New(TopicKey("无关身份键"), "unused", "连接池最大 128。",
		"完全不同的原始问法", "s1", nil, []float64{0, 1}, 0.5)
	// Force one strong scenario key (as Evolve would after a hit).
	c.LevelKeys = []LevelKey{{Level: KeyScenario, Text: "连接池最大连接数"}}

	q := "连接池最大连接数"
	// Local hash of q vs embed {0,1} will not clear 0.85 alone.
	emb := Local{N: 8}
	qe, err := emb.Embed(context.Background(), []string{q})
	if err != nil || len(qe) != 1 {
		t.Fatal(err)
	}
	if Cosine(c.Embed, qe[0]) >= 0.85 {
		t.Fatalf("fixture embed must be weak, cos=%v", Cosine(c.Embed, qe[0]))
	}
	if rel := MaxKeyRel(q, &c); rel < 0.99 {
		t.Fatalf("max-over-keys rel=%v, want >=0.99", rel)
	}
	if !ShouldReuse(&c, q, qe[0], 0.85) {
		t.Fatalf("single strong key must clear reuse; score=%v", ReuseScore(&c, q, qe[0]))
	}
	// G-pollute: unrelated query must still fail.
	if ShouldReuse(&c, "部署机房在哪", qe[0], 0.85) {
		t.Fatal("unrelated query must not reuse via max-over-keys")
	}
}

// G-drop: level keys are accelerators — deleting them leaves identity and
// L0 rebuild intact (TopicKey + body still enough to re-cluster).
func TestLevelKeysAreOptionalAccelerators(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	key := TopicKey("连接池最大连接数")
	c := New(key, "pool", "128", "连接池最大连接数是多少", "s1", nil, []float64{1, 0}, 0.8)
	if len(c.LevelKeys) == 0 {
		t.Fatal("New must derive level keys")
	}
	if err := st.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Drop derived keys (simulates wipe of the derived layer only).
	got, err := st.Get(ctx, c.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	got.LevelKeys = nil
	if err := st.Save(ctx, *got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Get(ctx, c.ID)
	if again == nil || again.TopicKey != key || again.LevelKeys != nil {
		t.Fatalf("after drop: %+v", again)
	}
	// Same topic still finds the cluster (identity does not depend on keys).
	found, err := st.FindByTopic(ctx, key)
	if err != nil || len(found) != 1 {
		t.Fatalf("FindByTopic after key drop: %v %v", found, err)
	}
}

// G-cross: different TopicKey, one shared strong key → ShouldReuse true
// (kb then treats it as a merge candidate, not an answer).
func TestCrossTopicHitViaLevelKey(t *testing.T) {
	a := New(TopicKey("连接池最大连接数"), "pool", "128", "连接池最大连接数是多少",
		"s1", nil, []float64{1, 0}, 0.8)
	b := Cluster{
		ID: "Ccross", TopicKey: TopicKey("数据库连接池上限配置"),
		Name: "上限", Content: "上限 128",
		Queries: []string{"数据库连接池上限配置"},
		Embed:   []float64{0, 1},
		LevelKeys: []LevelKey{
			{Level: KeyScenario, Text: "连接池最大连接数"},
		},
		Lifecycle: LifecycleEmerging,
	}
	q := "连接池最大连接数是多少"
	emb := Local{N: 8}
	qe, _ := emb.Embed(context.Background(), []string{q})
	if b.TopicKey == a.TopicKey {
		t.Fatal("fixture must be cross-topic")
	}
	if !ShouldReuse(&b, q, qe[0], 0.5) {
		t.Fatalf("cross-topic key hit must clear a moderate theta; score=%v",
			ReuseScore(&b, q, qe[0]))
	}
	// Strict reuse theta may or may not clear depending on embed — the
	// important contract is max-over-keys ≥ lexical key match.
	if MaxKeyRel(q, &b) < 0.5 {
		t.Fatalf("key rel too low: %v", MaxKeyRel(q, &b))
	}
}

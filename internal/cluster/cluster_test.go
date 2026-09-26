package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/mcs"
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
	if !CanMerge(&base, paraphrase, qe, 0.5) {
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
	if ShouldReuse(&c, "缓存穿透", qe, 0.1) {
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

func TestNormalizeEvidence(t *testing.T) {
	for _, label := range []string{"", "full", "stratified", "fuzz", "gaussian", "global", "doc:other", "full-document"} {
		t.Run(label, func(t *testing.T) {
			sample := mcs.Sample{
				Source: label, Start: 3, End: 12, Content: "evidence",
				Score: 8, Reasoning: "supports answer", Arm: "local", Covers: []string{"f1"},
			}
			input := []mcs.Sample{sample}
			want := sample
			if label != "doc:other" && label != "full-document" {
				want.Source = "doc:current"
			}
			normalized := NormalizeEvidence("doc:current", input)
			c := New("topic", "name", "answer", "query", "doc:current", input, nil, 0.8)
			for _, got := range [][]mcs.Sample{normalized, c.Evidence} {
				if !reflect.DeepEqual(got, []mcs.Sample{want}) {
					t.Fatalf("evidence = %+v, want %+v", got, want)
				}
				got[0].Source = "changed"
				if !reflect.DeepEqual(input, []mcs.Sample{sample}) {
					t.Fatalf("input evidence mutated: %+v", input)
				}
			}
		})
	}
	if got := NormalizeEvidence("doc:current", nil); got != nil {
		t.Fatalf("nil evidence = %v, want nil", got)
	}
}

func TestSplitCapIncludesTopicAliases(t *testing.T) {
	a := Cluster{ID: "a", TopicKey: "own", TopicKeys: []string{"alias"}}
	b := Cluster{ID: "b", TopicKey: "alias"}
	clusters := []Cluster{{ID: "unrelated", TopicKey: "other"}, a, b}
	for _, tc := range []struct {
		key string
		cap int
		ids []string
	}{
		{"own", 2, []string{"a"}},
		{"alias", 2, []string{"a", "b"}},
		{"alias", 1, []string{"a"}},
		{"alias", 0, []string{"a"}},
		{"missing", 2, nil},
	} {
		var ids []string
		for _, c := range SplitCap(clusters, tc.key, tc.cap) {
			ids = append(ids, c.ID)
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Errorf("SplitCap(%q, %d) = %v, want %v", tc.key, tc.cap, ids, tc.ids)
		}
	}
}

func TestTopicAliasesJSONRoundTrip(t *testing.T) {
	for _, aliases := range [][]string{nil, {"folded", "inherited"}} {
		c := Cluster{ID: "survivor", TopicKey: "own", TopicKeys: aliases}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc["topic_keys"]; ok != (len(aliases) > 0) {
			t.Fatalf("optional topic_keys field: %s", raw)
		}
		got, err := fromDoc(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*got, c) {
			t.Fatalf("round trip = %+v, want %+v", got, c)
		}
	}
}

// Store-boundary discipline: Save is insert-then-replace, aliases survive the
// round trip, and FindByTopic resolves all three alias shapes through the
// engine's own filter semantics.
func TestCumuStoreTopicAliases(t *testing.T) {
	for _, key := range []string{"own", "folded", "inherited"} {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			engine, err := cumulite.Open(t.TempDir())
			if err != nil {
				t.Fatalf("open engine: %v", err)
			}
			defer engine.Close()
			if err := engine.EnsureCollection(ctx, "clus_clusters"); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			st := NewCumuStore(engine, "")

			c := New("own", "name", "answer", "original query", "doc", nil, nil, 0.8)
			c.TopicKeys = []string{"folded", "inherited"}
			if err := st.Save(ctx, c); err != nil {
				t.Fatal(err)
			}
			got, err := st.Get(ctx, c.ID)
			if err != nil || got == nil {
				t.Fatalf("Get = %+v, err = %v", got, err)
			}
			if !reflect.DeepEqual(got.TopicKeys, c.TopicKeys) {
				t.Fatalf("inserted aliases = %v, want %v", got.TopicKeys, c.TopicKeys)
			}
			for i := 0; i < MaxQueriesPerCluster+1; i++ {
				got.Evolve(fmt.Sprintf("new query %d", i), nil)
			}
			if err := st.Save(ctx, *got); err != nil {
				t.Fatal(err)
			}
			found, err := st.FindByTopic(ctx, key)
			if err != nil || len(found) != 1 {
				t.Fatalf("FindByTopic(%q) = %+v, err = %v", key, found, err)
			}
			if !reflect.DeepEqual(found[0], *got) || found[0].TopicKey != c.TopicKey || found[0].ID != c.ID {
				t.Fatalf("persisted survivor = %+v, want %+v", found[0], got)
			}
			// Two saves, one document: a second insert would fail loudly on the
			// duplicate identity, and a replace that missed would leave two.
			all, err := st.All(ctx)
			if err != nil || len(all) != 1 {
				t.Fatalf("save must be insert-or-replace, got %d documents (err %v)", len(all), err)
			}
		})
	}
}

// contested finally has a writer: MarkContested is what DetectConflict's
// "// Lifecycle: contested." comment always intended — without it, tidy's
// contested protection was dead code.
func TestMarkContestedWritesLifecycle(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	a := New("aaaa1111aaaa2222", "a", "c", "q", "s", nil, nil, 0.5)
	b := New("bbbb2222bbbb3333", "b", "c", "q", "s", nil, nil, 0.5)
	if err := st.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(ctx, b); err != nil {
		t.Fatal(err)
	}
	n, err := MarkContested(ctx, st, a.ID, b.ID, "missing-id")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("marked %d, want 2", n)
	}
	for _, id := range []string{a.ID, b.ID} {
		c, _ := st.Get(ctx, id)
		if c.Lifecycle != LifecycleContested {
			t.Fatalf("cluster %s lifecycle = %s", id, c.Lifecycle)
		}
	}
	// Idempotent: an already-contested cluster is not re-stamped.
	if n, _ := MarkContested(ctx, st, a.ID); n != 0 {
		t.Fatalf("re-mark counted %d, want 0", n)
	}
}

// New cluster ids carry the full 64-bit topic key, and Save refuses to
// overwrite an id held by a DIFFERENT topic_key — the old 32-bit truncation
// made that a silent cluster-eats-cluster.
func TestClusterIDWidthAndSaveCollisionGuard(t *testing.T) {
	tk := "0123456789abcdef"
	c := New(tk, "n", "c", "q", "s", nil, nil, 0.5)
	if c.ID != "C"+tk {
		t.Fatalf("id = %s, want C+full topic key", c.ID)
	}
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx := context.Background()
	if err := engine.EnsureCollection(ctx, "clus_clusters"); err != nil {
		t.Fatal(err)
	}
	st := NewCumuStore(engine, "clus_clusters")
	if err := st.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	other := c
	other.TopicKey = "ffffffffffffffff"
	if err := st.Save(ctx, other); err == nil {
		t.Fatal("save under an id held by another topic_key must refuse")
	}
	// Same topic_key (a normal update) still replaces.
	other.TopicKey = tk
	other.Name = "updated"
	if err := st.Save(ctx, other); err != nil {
		t.Fatalf("same-identity update refused: %v", err)
	}
}

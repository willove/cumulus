package cluster

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cumubase/ask/internal/mcs"
)

// fixture builds a cluster whose embed is the query's own vector (one query),
// so pairwise sims are directly computable from the unigram overlap.
func fixture(id, topic, query, content string, emb Embedder, created time.Time) Cluster {
	vs, err := emb.Embed(context.Background(), []string{query})
	if err != nil {
		panic(err)
	}
	return Cluster{
		ID: id, TopicKey: topic, Name: query, Content: content,
		Queries: []string{query}, Embed: vs[0], Confidence: 0.6,
		Hotness: 0.5, Lifecycle: LifecycleStable, Version: 1,
		SourceID: "src:" + id, CreatedAt: created, UpdatedAt: created,
	}
}

func TestTidyFoldsCrossTopicNearDuplicate(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	// Same 9 unigrams + 2 extra: cos = 9/sqrt(9*11) ≈ 0.904 ≥ θ, different
	// topic keys — exactly the pair the write path (FindByTopic-scoped merge)
	// cannot see.
	a := fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "连接池最大 128。", emb, base)
	b := fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "连接池上限 128。", emb, base.Add(time.Minute))
	far := fixture("Cccc", TopicKey("照明功率是多少"), "照明功率是多少", "照明 200 瓦。", emb, base.Add(2*time.Minute))
	for _, c := range []Cluster{a, b, far} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Tidy(ctx, st, emb, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 || rep.Merged != 1 || len(rep.Pairs) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if p := rep.Pairs[0]; p.Winner != "Caaa" || p.Loser != "Cbbb" || p.Sim < 0.55 {
		t.Fatalf("pair = %+v (older must win)", p)
	}
	if got, _ := st.Get(ctx, "Cbbb"); got != nil {
		t.Fatal("loser must be deleted")
	}
	w, _ := st.Get(ctx, "Caaa")
	if w.Version != 2 || w.Hotness != 0.5 {
		t.Fatalf("winner state: version=%d hotness=%.2f", w.Version, w.Hotness)
	}
	if len(w.Queries) != 2 {
		t.Fatalf("queries merged: %v", w.Queries)
	}
	if w.Confidence != 0.6 {
		t.Fatalf("confidence averaged: %.3f", w.Confidence)
	}
	assertTopics := func() {
		t.Helper()
		for _, key := range []string{a.TopicKey, b.TopicKey} {
			found, err := st.FindByTopic(ctx, key)
			if err != nil || len(found) != 1 {
				t.Fatalf("FindByTopic(%q) = %+v, err = %v", key, found, err)
			}
			if found[0].ID != a.ID || found[0].TopicKey != a.TopicKey {
				t.Fatalf("survivor identity changed: %+v", found[0])
			}
		}
	}
	assertTopics()
	for i := 0; i < MaxQueriesPerCluster+1; i++ {
		w.Evolve(fmt.Sprintf("unrelated query %d", i), nil)
	}
	if containsString(w.Queries, a.Queries[0]) || containsString(w.Queries, b.Queries[0]) {
		t.Fatalf("original queries must be evicted: %v", w.Queries)
	}
	if err := st.Save(ctx, *w); err != nil {
		t.Fatal(err)
	}
	assertTopics()
	if found, err := st.FindByTopic(ctx, "missing"); err != nil || len(found) != 0 {
		t.Fatalf("unrelated topic lookup = %+v, err = %v", found, err)
	}
}

func TestTidyLeavesDissimilarClustersAlone(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	_ = st.Save(ctx, fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "a", emb, base))
	_ = st.Save(ctx, fixture("Cbbb", TopicKey("照明功率是多少"), "照明功率是多少", "b", emb, base.Add(time.Minute)))
	rep, err := Tidy(ctx, st, emb, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 0 || len(rep.Pairs) != 0 {
		t.Fatalf("nothing should fold: %+v", rep)
	}
	if all, _ := st.All(ctx); len(all) != 2 {
		t.Fatalf("both clusters survive: %d", len(all))
	}
}

func TestTidySkipsContestedAndDeprecated(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	a := fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "a", emb, base)
	contested := fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "b", emb, base.Add(time.Minute))
	contested.Lifecycle = LifecycleContested
	deprecated := fixture("Cccc", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "c", emb, base.Add(2*time.Minute))
	deprecated.Lifecycle = LifecycleDeprecated
	for _, c := range []Cluster{a, contested, deprecated} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Tidy(ctx, st, emb, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 0 {
		t.Fatalf("contested/deprecated must not fold: %+v", rep)
	}
	if rep.SkippedContested != 1 || rep.SkippedDeprecated != 1 {
		t.Fatalf("skip counts = %+v", rep)
	}
	if rep.Lifecycle[LifecycleContested] != 1 || rep.Lifecycle[LifecycleDeprecated] != 1 {
		t.Fatalf("lifecycle population = %+v", rep.Lifecycle)
	}
}

func TestTidyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	_ = st.Save(ctx, fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "a", emb, base))
	_ = st.Save(ctx, fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "b", emb, base.Add(time.Minute)))
	if _, err := Tidy(ctx, st, emb, 0, false, 0); err != nil {
		t.Fatal(err)
	}
	rep2, err := Tidy(ctx, st, emb, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Merged != 0 {
		t.Fatalf("second sweep must be empty: %+v", rep2)
	}
}

func TestTidyDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	_ = st.Save(ctx, fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "a", emb, base))
	_ = st.Save(ctx, fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "b", emb, base.Add(time.Minute)))
	rep, err := Tidy(ctx, st, emb, 0, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || len(rep.Pairs) != 1 || rep.Merged != 0 {
		t.Fatalf("dry-run report = %+v", rep)
	}
	if all, _ := st.All(ctx); len(all) != 2 {
		t.Fatalf("dry-run must not mutate: %d clusters", len(all))
	}
}

func TestTidyEvidenceUnionAndHotnessTransfer(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	w := fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "a", emb, base)
	w.Evidence = []mcs.Sample{{Source: "src:a", Start: 0, End: 9, Content: "连接池最大"}}
	w.Hotness = 0.5
	l := fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "b", emb, base.Add(time.Minute))
	l.Evidence = []mcs.Sample{
		{Source: "src:b", Start: 0, End: 9, Content: "连接池上限"},
		{Source: "src:a", Start: 0, End: 9, Content: "连接池最大"}, // same window as winner's
	}
	l.Hotness = 0.9
	for _, c := range []Cluster{w, l} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Tidy(ctx, st, emb, 0, false, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "Caaa")
	if len(got.Evidence) != 2 {
		t.Fatalf("evidence union by window identity: %d windows", len(got.Evidence))
	}
	if got.Hotness != 0.9 {
		t.Fatalf("hotness must transfer (max), got %.2f", got.Hotness)
	}
}

func TestTidyRespectsMaxMerges(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	// Three mutually-near clusters: sweep folds at most -max 1.
	for i, q := range []string{"连接池最大连接数是多少", "连接池最大连接数上限是多少", "连接池最大连接数上限是多少呢"} {
		id := "C" + string(rune('a'+i)) + string(rune('a'+i)) + string(rune('a'+i))
		_ = st.Save(ctx, fixture(id, TopicKey(q), q, q, emb, base.Add(time.Duration(i)*time.Minute)))
	}
	rep, err := Tidy(ctx, st, emb, 0, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 1 {
		t.Fatalf("max cap not respected: %+v", rep)
	}
}

func TestQuerySetEmbedFallsBackOnEmbedderError(t *testing.T) {
	_, err := QuerySetEmbed(context.Background(), Local{N: 64}, nil)
	if err == nil {
		t.Fatal("empty query set must error (caller keeps the stored embed)")
	}
	vs, err := QuerySetEmbed(context.Background(), Local{N: 32}, []string{"甲", "乙"})
	if err != nil || len(vs) != 32 {
		t.Fatalf("mean-pool dims: len=%d err=%v", len(vs), err)
	}
}

func TestTidyNormalizesLegacyEvidenceBeforeDedup(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	emb := Local{N: 64}
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	w := fixture("Caaa", "topic-a", "pool limit", "document a", emb, base)
	l := fixture("Cbbb", "topic-b", "pool limit", "document b", emb, base.Add(time.Minute))
	// Deliberately bypass New: persisted legacy clusters still use method labels.
	w.Evidence = []mcs.Sample{
		{Source: "full", Start: 0, End: 9, Content: w.Content},
		{Source: "doc:explicit", Start: 0, End: 9, Content: "earlier document"},
	}
	l.Evidence = []mcs.Sample{
		{Source: "full", Start: 0, End: 9, Content: l.Content},
		{Source: w.SourceID, Start: 0, End: 9, Content: w.Content, Score: 9},
		{Source: "doc:explicit", Start: 0, End: 9, Content: "earlier document"},
	}
	beforeW := append([]mcs.Sample(nil), w.Evidence...)
	beforeL := append([]mcs.Sample(nil), l.Evidence...)
	for _, c := range []Cluster{w, l} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Tidy(ctx, st, emb, 0, false, 0)
	if err != nil || rep.Merged != 1 {
		t.Fatalf("Tidy = %+v, err = %v", rep, err)
	}
	got, err := st.Get(ctx, w.ID)
	if err != nil || got == nil {
		t.Fatalf("Get winner = %+v, err = %v", got, err)
	}
	want := []mcs.Sample{beforeW[0], beforeW[1], beforeL[0]}
	want[0].Source = w.SourceID
	want[2].Source = l.SourceID
	if !reflect.DeepEqual(got.Evidence, want) {
		t.Fatalf("per-document evidence = %+v, want %+v", got.Evidence, want)
	}
	if !reflect.DeepEqual(w.Evidence, beforeW) || !reflect.DeepEqual(l.Evidence, beforeL) {
		t.Fatalf("input evidence mutated: winner=%+v loser=%+v", w.Evidence, l.Evidence)
	}
}

func TestFoldTopicAliasesAreUniqueAndTransitive(t *testing.T) {
	w := Cluster{ID: "winner", TopicKey: "own", TopicKeys: []string{"kept", "shared"}}
	l := Cluster{ID: "loser", TopicKey: "folded", TopicKeys: []string{"shared", "inherited", "own", "folded", ""}}
	beforeW := w.TopicKeys
	beforeL := append([]string(nil), l.TopicKeys...)
	foldInto(&w, &l)
	if w.ID != "winner" || w.TopicKey != "own" {
		t.Fatalf("survivor identity changed: %+v", w)
	}
	if want := []string{"kept", "shared", "folded", "inherited"}; !reflect.DeepEqual(w.TopicKeys, want) {
		t.Fatalf("aliases = %v, want %v", w.TopicKeys, want)
	}
	if !reflect.DeepEqual(l.TopicKeys, beforeL) || !reflect.DeepEqual(beforeW, []string{"kept", "shared"}) {
		t.Fatal("input aliases mutated")
	}
	older := Cluster{ID: "oldest", TopicKey: "oldest-topic"}
	foldInto(&older, &w)
	if want := []string{"own", "kept", "shared", "folded", "inherited"}; !reflect.DeepEqual(older.TopicKeys, want) {
		t.Fatalf("transitive aliases = %v, want %v", older.TopicKeys, want)
	}
	if older.ID != "oldest" || older.TopicKey != "oldest-topic" {
		t.Fatalf("older survivor identity changed: %+v", older)
	}
}

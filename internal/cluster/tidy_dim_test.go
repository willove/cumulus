package cluster

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// A tidy run must not re-measure a cluster with a DIFFERENT embedder than the
// one that built it. The recompute used to overwrite the stored vectors with
// whatever the tidy process had in its environment (a cron without
// CLUS_EMBED=minilm → 64-d hash vectors over a 384-d cluster); Cosine across
// mismatched dims returns 0, so semantic reuse silently collapsed — and the
// damage was persisted by Save.
func TestTidyKeepsVectorIdentityUnderForeignEmbedder(t *testing.T) {
	ctx := context.Background()
	emb384 := Local{N: 384}
	emb64 := Local{N: 64}
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	// Same 9-unigram overlap as TestTidyFoldsCrossTopicNearDuplicate: a fold
	// IS on the cards, so the recompute path actually runs.
	a := fixture("Caaa", TopicKey("连接池最大连接数是多少"), "连接池最大连接数是多少", "连接池最大 128。", emb384, base)
	b := fixture("Cbbb", TopicKey("连接池最大连接数上限是多少"), "连接池最大连接数上限是多少", "连接池上限 128。", emb384, base.Add(time.Minute))
	original := append([]float64(nil), a.Embed...)

	st := NewMemory()
	for _, c := range []Cluster{a, b} {
		if err := st.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Tidy(ctx, st, emb64, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 1 {
		t.Fatalf("fold must still happen under a foreign embedder: %+v", rep)
	}
	w, err := st.Get(ctx, "Caaa")
	if err != nil || w == nil {
		t.Fatalf("winner missing: %v", err)
	}
	if len(w.Embed) != len(original) {
		t.Fatalf("winner embed was re-measured by the foreign embedder: %d-d, want %d-d",
			len(w.Embed), len(original))
	}
	if !reflect.DeepEqual(w.Embed, original) {
		t.Fatal("winner embed must be byte-identical to what the cluster was built with")
	}

	// Positive control: with the embedder the cluster was built under, the
	// recompute DOES run — the winner embed is the merged query set's vector.
	st2 := NewMemory()
	for _, c := range []Cluster{a, b} {
		if err := st2.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Tidy(ctx, st2, emb384, 0, false, 0); err != nil {
		t.Fatal(err)
	}
	w2, err := st2.Get(ctx, "Caaa")
	if err != nil || w2 == nil {
		t.Fatalf("winner missing: %v", err)
	}
	want, err := QuerySetEmbed(ctx, emb384, w2.Queries)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w2.Embed, want) {
		t.Fatal("under the matching embedder the winner embed must be refreshed to the merged query set")
	}
}

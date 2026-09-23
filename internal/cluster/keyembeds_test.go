package cluster

import (
	"context"
	"testing"
)

// 2.5 (MVR-cache 分段 MaxSim): a weak query-set embed must still reuse when
// ONE segment strongly matches the query.
func TestMaxKeySimReusesOnSingleSegment(t *testing.T) {
	emb := Local{N: 8}
	c := New(TopicKey("连接池最大连接数"), "pool", "连接池最大 128。",
		"连接池最大连接数是多少", "s1", nil, []float64{0, 1}, 0.5)
	if len(c.LevelKeys) == 0 {
		t.Fatal("fixture must derive level keys")
	}
	qe, _ := emb.Embed(context.Background(), []string{"连接池最大连接数是多少"})
	// Without segment embeds, MaxKeySim is silent.
	if c.MaxKeySim(qe[0]) != 0 {
		t.Fatal("no aligned key embeds → MaxKeySim must be 0")
	}
	// Compute per-key embeds (what kb does on the write path).
	vs, err := emb.Embed(context.Background(), c.LevelKeyTexts())
	if err != nil {
		t.Fatal(err)
	}
	c.AttachKeyEmbeds(vs)
	sim := c.MaxKeySim(qe[0])
	if sim < 0.85 {
		t.Fatalf("segment MaxSim must clear the reuse line: %.3f", sim)
	}
	// ReuseScore takes the max, so a single strong segment carries the hit
	// even if the aggregate Embed cosine is weak.
	if !ShouldReuse(&c, "连接池最大连接数是多少", qe[0], 0.85) {
		t.Fatalf("single strong segment must clear reuse; score=%.3f",
			ReuseScore(&c, "连接池最大连接数是多少", qe[0]))
	}
}

// Misaligned/empty segment maps must never score (MVR-cache alignment guard).
func TestKeyEmbedsMisalignedIgnored(t *testing.T) {
	c := New(TopicKey("k"), "n", "连接池最大 128。", "q", "s", nil, []float64{1, 0}, 0.5)
	c.AttachKeyEmbeds([][]float64{{1, 0}}) // shorter than LevelKeys
	if len(c.KeyEmbeds) != 0 {
		t.Fatal("misaligned map must be dropped")
	}
	c.AttachKeyEmbeds([][]float64{}) // empty
	if len(c.KeyEmbeds) != 0 {
		t.Fatal("empty map must be dropped")
	}
	if c.MaxKeySim([]float64{1, 0}) != 0 {
		t.Fatal("misaligned map must not score")
	}
}

// addLevelKey invalidates segment embeds until recomputed.
func TestAddLevelKeyInvalidatesKeyEmbeds(t *testing.T) {
	c := New(TopicKey("k"), "n", "连接池最大 128。", "q", "s", nil, []float64{1, 0}, 0.5)
	embeds := make([][]float64, len(c.LevelKeys))
	for i := range embeds {
		embeds[i] = []float64{1, 0}
	}
	c.AttachKeyEmbeds(embeds)
	if len(c.KeyEmbeds) == 0 {
		t.Fatal("fixture must have aligned embeds")
	}
	c.addLevelKey(KeyScenario, "新问法")
	if len(c.KeyEmbeds) != 0 {
		t.Fatal("new key must invalidate the aligned map")
	}
}

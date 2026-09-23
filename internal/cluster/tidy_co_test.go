package cluster

import (
	"context"
	"testing"
	"time"
)

// A4: co-retrieval profile breaks cosine ties so comparative diagnosis
// outranks a bare near-duplicate that never co-occurred with anything.
func TestTidyWithCoPrefersProfileBackedPair(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Three clusters, pairwise cosine equal via identical embeds.
	embed := []float64{1, 0}
	a := Cluster{ID: "Aa", TopicKey: "连接池", Content: "连接池 128", Embed: embed, CreatedAt: base}
	b := Cluster{ID: "Bb", TopicKey: "连接池上限", Content: "连接池 上限 128", Embed: embed, CreatedAt: base.Add(time.Hour)}
	c := Cluster{ID: "Cc", TopicKey: "无关", Content: "无关内容", Embed: embed, CreatedAt: base.Add(2 * time.Hour)}
	for _, x := range []Cluster{a, b, c} {
		if err := st.Save(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	// Only A–B has a co_occur profile.
	co := func(x, y string) float64 {
		if (x == a.ID && y == b.ID) || (x == b.ID && y == a.ID) {
			return 0.9
		}
		return 0
	}
	rep, err := TidyWithCo(ctx, st, nil, 0.5, true /* dry-run */, 1, co)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 1 {
		t.Fatalf("want one exemplar pair, got %+v", rep.Pairs)
	}
	p := rep.Pairs[0]
	got := p.Winner + p.Loser
	if !(got == a.ID+b.ID || got == b.ID+a.ID) {
		t.Fatalf("profile-backed A–B must win the dry-run exemplar, got %s–%s", p.Winner, p.Loser)
	}
	if p.Co != 0.9 {
		t.Fatalf("report must carry co profile weight, got %v", p.Co)
	}
}

// betterPair: same cosine, higher Co wins.
func TestBetterPairCoBreaksTie(t *testing.T) {
	low := TidyPair{Winner: "A", Loser: "B", Sim: 0.8, Co: 0}
	high := TidyPair{Winner: "C", Loser: "D", Sim: 0.8, Co: 0.9}
	if !betterPair(high, low) {
		t.Fatal("higher co must win equal cosine")
	}
}

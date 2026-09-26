package main

import (
	"context"
	"testing"

	"github.com/willove/cumulite"
)

// The reset's session prefix once drifted from the writer's: sessions were
// stored under session.go's "clus:session:" while the reset deleted under a
// re-spelled "sess:" — so every "clean start" reset left the previous run's
// sessions in the store AND the learning probe reported Clean=true with
// them still there. Sessions are derived state (they steer sampleContextText
// and the DEEP rewriter on later queries), so both halves are pinned here:
// the counter must see a live session, and the reset must remove it.
func TestResetLearnedSeesAndClearsSessions(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	st := sessionStore{c: c, ns: "alpha"}
	if _, err := st.appendTurn(ctx, "s1", "标题", "问题", "答案"); err != nil {
		t.Fatalf("appendTurn: %v", err)
	}

	before, err := LearningState(ctx, c, "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if before.KVKeys == 0 {
		t.Fatal("learning probe counts 0 kv keys with a live session — the probe prefix drifted from the writer's")
	}
	if before.Clean {
		t.Fatal("Clean=true with a session present — the starting point is not the clean slate it reports")
	}

	rs, err := ResetLearned(ctx, c, "alpha", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if rs.KVKeys == 0 {
		t.Fatal("reset removed 0 kv keys — the session survived the reset")
	}

	after, err := LearningState(ctx, c, "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Clean || after.Total != 0 {
		t.Fatalf("after reset: clean=%v total=%d — want a clean slate", after.Clean, after.Total)
	}
}

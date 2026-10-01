package deep

import (
	"context"
	"fmt"
	"testing"

	"github.com/willove/cumulite"
)

// The conflict store is a P4 deliverable and this file is its first test.
// It matters because All() paginates in a loop rather than trusting one
// capped query — that shape changed once (a hard Limit:1000 that silently
// truncated the population) and nothing here would have caught a regression.

func conflictStore(t *testing.T) *CumuStore {
	t.Helper()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.EnsureCollection(context.Background(), "clus_conflicts"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return NewCumuStore(eng, "clus_conflicts")
}

func TestConflictStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := conflictStore(t)

	in := Conflict{A: "C1", B: "C2", Group: "金额", Reason: "主张 30 万 vs 50 万"}
	if err := st.Save(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(all))
	}
	got := all[0]
	if got.A != in.A || got.B != in.B || got.Group != in.Group || got.Reason != in.Reason {
		t.Fatalf("round-trip lost fields: got %+v, want %+v", got, in)
	}
	if got.Saved.IsZero() {
		t.Fatal("Saved was not stamped — the read faces surface it, so it must persist")
	}
}

func TestConflictStoreSynthesisesIDWhenAbsent(t *testing.T) {
	ctx := context.Background()
	st := conflictStore(t)

	if err := st.Save(ctx, Conflict{A: "C1", B: "C2", Group: "g"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d, want 1", len(all))
	}
	if all[0].ID == "" {
		t.Fatal("an empty ID must be synthesised, not left blank — Between/All key off it")
	}
}

func TestConflictStoreSaveOverwritesRatherThanDuplicates(t *testing.T) {
	ctx := context.Background()
	st := conflictStore(t)

	// Same explicit ID twice: this is "the same conflict, re-detected", so it
	// must update in place. A duplicate here would double-count every
	// re-run of conflict detection.
	if err := st.Save(ctx, Conflict{ID: "x:fixed", A: "C1", B: "C2", Reason: "第一次"}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := st.Save(ctx, Conflict{ID: "x:fixed", A: "C1", B: "C2", Reason: "第二次"}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("re-saving the same ID produced %d rows, want 1", len(all))
	}
	if all[0].Reason != "第二次" {
		t.Fatalf("re-save did not update: reason = %q", all[0].Reason)
	}
}

func TestConflictStoreBetweenIsSymmetric(t *testing.T) {
	ctx := context.Background()
	st := conflictStore(t)

	if err := st.Save(ctx, Conflict{A: "C1", B: "C2", Group: "g1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.Save(ctx, Conflict{A: "C3", B: "C4", Group: "g2"}); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	// Direction must not matter: a conflict between two clusters is the same
	// relation whichever way round it was detected.
	for _, pair := range [][2]string{{"C1", "C2"}, {"C2", "C1"}} {
		got, err := st.Between(ctx, pair[0], pair[1])
		if err != nil {
			t.Fatalf("between %v: %v", pair, err)
		}
		if len(got) != 1 {
			t.Fatalf("Between(%s,%s) = %d rows, want 1", pair[0], pair[1], len(got))
		}
	}

	got, err := st.Between(ctx, "C1", "C9")
	if err != nil {
		t.Fatalf("between miss: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Between(C1,C9) matched %d rows — unrelated pairs must not collide", len(got))
	}
}

// All walks pages rather than trusting one capped query. The population here
// is well under a page, so this pins the LOOP's shape (no skipped page, no
// duplicate) rather than the page boundary itself; crossing 1000 rows in a
// unit test would cost more than the regression it guards is likely to cause.
func TestConflictStoreAllReturnsEveryRowOnce(t *testing.T) {
	ctx := context.Background()
	st := conflictStore(t)

	const n = 37
	for i := 0; i < n; i++ {
		if err := st.Save(ctx, Conflict{
			ID: fmt.Sprintf("x:%02d", i), A: "C1", B: fmt.Sprintf("C%d", i+2), Group: "g",
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != n {
		t.Fatalf("All returned %d rows, want %d", len(all), n)
	}
	seen := map[string]bool{}
	for _, c := range all {
		if seen[c.ID] {
			t.Fatalf("duplicate row %q in All — the paging loop re-read a page", c.ID)
		}
		seen[c.ID] = true
	}
}

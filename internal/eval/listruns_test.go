package eval

import (
	"context"
	"fmt"
	"testing"

	"github.com/willove/cumulite"
)

// ListRuns used to collapse every limit > 200 — and every non-positive limit —
// to 50, so `ListRuns(500)` returned 50 rows with no error and no way for the
// caller to tell it had been shortchanged. These pin the two distinct cases:
// "caller said nothing" keeps the 50 default; "caller asked for more than the
// documented 200 budget" gets the budget, not a quarter of the request.

func runStoreWith(t *testing.T, n int) (*CumuStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	coll := "clus_evals"
	if err := eng.EnsureCollection(ctx, coll); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	st := NewCumuStore(eng, coll)
	for i := 0; i < n; i++ {
		d := RunDoc{ID: fmt.Sprintf("run:%06d", i), Tag: "t", System: Report{N: 1}}
		if err := st.SaveRun(ctx, d); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	return st, ctx
}

func TestListRunsClampsToTheBudgetNotToTheDefault(t *testing.T) {
	// 60 rows: enough that the old code's 50 would visibly truncate, small
	// enough to stay fast.
	st, ctx := runStoreWith(t, 60)

	for _, tc := range []struct {
		limit int
		want  int
		why   string
	}{
		{10, 10, "an explicit in-budget limit is honoured exactly"},
		{60, 60, "asking for everything returns everything"},
		{500, 60, "over-budget asks are clamped to the budget, not to the 50 default"},
		{0, 50, "no usable limit still means the 50 default"},
		{-5, 50, "a negative limit is 'unspecified', not 'give me nothing'"},
	} {
		got, err := st.ListRuns(ctx, tc.limit)
		if err != nil {
			t.Fatalf("ListRuns(%d): %v", tc.limit, err)
		}
		if len(got) != tc.want {
			t.Fatalf("ListRuns(%d) = %d rows, want %d — %s", tc.limit, len(got), tc.want, tc.why)
		}
	}
}

// The clamp must not become a way to bypass the budget by asking for a
// smaller-but-still-huge number: 200 is the ceiling whatever the input.
func TestListRunsNeverExceedsTheBudget(t *testing.T) {
	st, ctx := runStoreWith(t, 30)
	for _, limit := range []int{200, 201, 10_000, 1 << 30} {
		got, err := st.ListRuns(ctx, limit)
		if err != nil {
			t.Fatalf("ListRuns(%d): %v", limit, err)
		}
		if len(got) > 30 {
			t.Fatalf("ListRuns(%d) returned %d rows, more than exist", limit, len(got))
		}
	}
}

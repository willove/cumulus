package ingest

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// PutBatch must keep Put's semantics: same digest → unchanged, changed digest →
// next revision with the previous live revision retired. It only changes the
// COST (one index scan per batch instead of one filtered query per document),
// so a behavioural difference here is a correctness bug.
func TestPutBatchKeepsRevisionSemantics(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	bi, err := st.NewBatchIngester(ctx)
	if err != nil {
		t.Fatal(err)
	}
	batch := func(key, body string) []source.Source {
		return []source.Source{source.New("T", "md", "", key, "zh", body, nil)}
	}
	// First batch: two distinct identities.
	n, err := bi.PutBatch(ctx, batch("k1", "第一版"))
	if err != nil || n != 1 {
		t.Fatalf("batch1: n=%d err=%v", n, err)
	}
	// Identical content → unchanged, nothing stored.
	n, err = bi.PutBatch(ctx, batch("k1", "第一版"))
	if err != nil || n != 0 {
		t.Fatalf("identical re-ingest must be a no-op: n=%d err=%v", n, err)
	}
	// Changed content → new revision, old one retired.
	n, err = bi.PutBatch(ctx, batch("k1", "第二版"))
	if err != nil || n != 1 {
		t.Fatalf("update: n=%d err=%v", n, err)
	}
	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Body != "第二版" || live[0].Version != 2 {
		t.Fatalf("expected one live v2 doc: %+v", live)
	}
	// Re-ingest inside the SAME batch: the second copy must be recognised as
	// unchanged, which is what the in-memory index is for.
	n, err = bi.PutBatch(ctx, append(batch("k2", "B"), batch("k2", "B")...))
	if err != nil || n != 1 {
		t.Fatalf("in-batch duplicate must dedupe: n=%d err=%v", n, err)
	}
}

// A batch of many distinct keys must all land, and a second batch on a larger
// store must not lose any (the regression that motivated PutBatch).
func TestPutBatchManyDistinctKeys(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	bi, err := st.NewBatchIngester(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first []source.Source
	for i := 0; i < 600; i++ {
		first = append(first, source.New("T", "md", "", itoa(i), "zh", "body "+itoa(i), nil))
	}
	if n, err := bi.PutBatch(ctx, first); err != nil || n != 600 {
		t.Fatalf("first batch: n=%d err=%v", n, err)
	}
	var second []source.Source
	for i := 600; i < 1500; i++ {
		second = append(second, source.New("T", "md", "", itoa(i), "zh", "body "+itoa(i), nil))
	}
	if n, err := bi.PutBatch(ctx, second); err != nil || n != 900 {
		t.Fatalf("second batch: n=%d err=%v", n, err)
	}
	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1500 {
		t.Fatalf("all 1500 must be live, got %d", len(live))
	}
}

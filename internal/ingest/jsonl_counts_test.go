package ingest

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// textOnlyMapFn is the CLI's default ingest-jsonl mapping: it reads `text`
// and ignores every other spelling of the body field.
func textOnlyMapFn(m map[string]any) (source.Source, error) {
	key, _ := m["key"].(string)
	title, _ := m["title"].(string)
	text, _ := m["text"].(string)
	return source.New(title, "jsonl", "", key, "zh", text, m), nil
}

// L9 family, third member: the batch path read clus_sources through
// NewBatchIngester before anything declared it, so ingest-jsonl against a
// fresh -data died on a raw `collection "clus_sources": not found`. The put
// face and the job face each grew their own EnsureOnce; this pins the shared
// batch entry point so a fourth write face cannot forget it again.
func TestIngestJSONLOnAFreshStoreDeclaresAndSucceeds(t *testing.T) {
	st, cp := undeclaredStore(t)
	if cp.declared() != 0 {
		t.Fatalf("precondition: a fresh store must have nothing declared, got %d", cp.declared())
	}
	recs := []map[string]any{{"key": "k1", "title": "条目一", "text": "连接池最大 128。"}}

	got, err := st.IngestJSONL(context.Background(), "fresh", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("ingest-jsonl on an undeclared store must succeed, got: %v", err)
	}
	if got.Written != 1 {
		t.Fatalf("written = %d, want 1 (%+v)", got.Written, got)
	}
}

// The count the operator reads must be the count the store received. Counting
// chunk slots instead let a corpus whose body field was misnamed report
// `processed: 9600` while PutBatch skipped every record (measured 2026-10-02:
// 9,600 in, 0 stored, exit 0).
func TestIngestJSONLCountsDroppedEmptyBodies(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	recs := make([]map[string]any, 0, 9)
	for i := 0; i < 9; i++ {
		recs = append(recs, map[string]any{"id": i, "title": "T", "body": "正文在 body 里"})
	}

	got, err := st.IngestJSONL(ctx, "mismatch", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got.Written != 0 || got.DroppedEmpty != 9 {
		t.Fatalf("a misnamed body field must be visible in the counts: %+v", got)
	}
	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("nothing was stored, so nothing may be live: got %d", len(live))
	}
}

// Written + Unchanged + DroppedEmpty must account for every record the run
// touched. PutBatch skips for exactly two reasons — an empty body and an
// unchanged digest — so deriving Unchanged by subtraction is exact, and this
// is the invariant that would break if a third skip reason were added.
func TestIngestJSONLCountsPartitionEveryRecord(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	rec := func(k, body string) map[string]any {
		return map[string]any{"key": k, "title": "T", "text": body}
	}
	// k3 has no body; the trailing k1 duplicates an identity already live.
	recs := []map[string]any{rec("k1", "一"), rec("k2", "二"), rec("k3", ""), rec("k1", "一")}

	got, err := st.IngestJSONL(ctx, "partition", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got.Written != 2 || got.Unchanged != 1 || got.DroppedEmpty != 1 {
		t.Fatalf("first run = %+v, want written 2 / unchanged 1 / dropped 1", got)
	}
	if got.Total() != len(recs) {
		t.Fatalf("counts %v do not partition %d records", got, len(recs))
	}

	// New fingerprint → the cursor resets and every record is re-examined:
	// k2 changes (written), k1 and its duplicate stay (unchanged), k3 is still
	// empty (dropped).
	recs2 := []map[string]any{rec("k1", "一"), rec("k2", "二改"), rec("k3", ""), rec("k1", "一")}
	got, err = st.IngestJSONL(ctx, "partition", recs2, textOnlyMapFn)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got.Written != 1 || got.Unchanged != 2 || got.DroppedEmpty != 1 {
		t.Fatalf("re-run = %+v, want written 1 / unchanged 2 / dropped 1", got)
	}
	if got.Total() != len(recs2) {
		t.Fatalf("counts %v do not partition %d records", got, len(recs2))
	}

	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("k1 and k2 must be live, got %d", len(live))
	}
	for _, s := range live {
		if s.BusinessKey == "k2" && (s.Body != "二改" || s.Version != 2) {
			t.Fatalf("k2 must be v2 of 二改, got v%d %q", s.Version, s.Body)
		}
	}
}

// A resumed job that has nothing left to do must report zeros, not the size
// of the records it skipped over.
func TestIngestJSONLResumeReportsZeroCounts(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	recs := []map[string]any{
		{"key": "k1", "title": "T", "text": "一"},
		{"key": "k2", "title": "T", "text": "二"},
	}
	if _, err := st.IngestJSONL(ctx, "resume", recs, textOnlyMapFn); err != nil {
		t.Fatalf("first run: %v", err)
	}

	got, err := st.IngestJSONL(ctx, "resume", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.Total() != 0 {
		t.Fatalf("an unchanged fingerprint resumes past every record: %+v", got)
	}
}

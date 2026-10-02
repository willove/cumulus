package ingest

import (
	"context"
	"testing"
)

// IngestJSONL now runs through BatchIngester (one revision index per job,
// O(n) instead of O(n²)). The migration must not change observable job
// semantics: revision arithmetic, unchanged-skip and the resumable cursor all
// behave as the per-record path did. These pin exactly that — the O(n²) form
// this replaced made a 20k-record corpus crawl (measured on the sibling bulk
// path: 6.4k records added 207s).
//
// The counts are the one place the migration DID change meaning, on purpose:
// they report what the store received, not how many chunk slots the run
// walked. See jsonl_counts_test.go.
func TestIngestJSONLBatchPathKeepsJobSemantics(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	rec := func(k, body string) map[string]any {
		return map[string]any{"key": k, "title": "T", "text": body}
	}

	// One job: three distinct identities plus an in-job duplicate.
	recs := []map[string]any{rec("k1", "一"), rec("k2", "二"), rec("k3", "三"), rec("k1", "一")}
	got, err := st.IngestJSONL(ctx, "j", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got.Written != 3 || got.Unchanged != 1 {
		t.Fatalf("3 distinct identities plus 1 in-job duplicate: %+v", got)
	}
	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("3 distinct identities expected, got %d", len(live))
	}

	// Same fingerprint → the cursor says done, nothing re-processed.
	got, err = st.IngestJSONL(ctx, "j", recs, textOnlyMapFn)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.Total() != 0 {
		t.Fatalf("resume with unchanged fingerprint must examine 0 records: %+v", got)
	}

	// New fingerprint → the job re-runs from the start; unchanged bodies are
	// skipped by digest, the changed one bumps to v2 and retires its old
	// revision (same arithmetic the single-doc path performed).
	recs2 := []map[string]any{rec("k1", "一"), rec("k2", "二改"), rec("k3", "三")}
	got, err = st.IngestJSONL(ctx, "j", recs2, textOnlyMapFn)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got.Written != 1 || got.Unchanged != 2 {
		t.Fatalf("only k2 changed: %+v", got)
	}
	live, err = st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("still 3 live docs expected, got %d", len(live))
	}
	for _, s := range live {
		if s.BusinessKey == "k2" {
			if s.Body != "二改" || s.Version != 2 {
				t.Fatalf("k2 must be v2 of 二改, got v%d %q", s.Version, s.Body)
			}
		} else if s.Version != 1 {
			t.Fatalf("%s must stay at v1, got v%d", s.BusinessKey, s.Version)
		}
	}
}

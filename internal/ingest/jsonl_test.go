package ingest

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// IngestJSONL now runs through BatchIngester (one revision index per job,
// O(n) instead of O(n²)). The migration must not change observable job
// semantics: counts, revision arithmetic, unchanged-skip and the resumable
// cursor all behave as the per-record path did. These pin exactly that —
// the O(n²) form this replaced made a 20k-record corpus crawl (measured on
// the sibling bulk path: 6.4k records added 207s).
func TestIngestJSONLBatchPathKeepsJobSemantics(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	rec := func(k, body string) map[string]any {
		return map[string]any{"key": k, "title": "T", "text": body}
	}
	mapFn := func(m map[string]any) (source.Source, error) {
		key, _ := m["key"].(string)
		title, _ := m["title"].(string)
		text, _ := m["text"].(string)
		return source.New(title, "jsonl", "", key, "zh", text, m), nil
	}

	// One job: three distinct identities plus an in-job duplicate.
	recs := []map[string]any{rec("k1", "一"), rec("k2", "二"), rec("k3", "三"), rec("k1", "一")}
	n, err := st.IngestJSONL(ctx, "j", recs, mapFn)
	if err != nil || n != 4 {
		t.Fatalf("ingest: n=%d err=%v", n, err)
	}
	live, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("3 distinct identities expected, got %d", len(live))
	}

	// Same fingerprint → the cursor says done, nothing re-processed.
	n, err = st.IngestJSONL(ctx, "j", recs, mapFn)
	if err != nil || n != 0 {
		t.Fatalf("resume with unchanged fingerprint must process 0: n=%d err=%v", n, err)
	}

	// New fingerprint → the job re-runs from the start; unchanged bodies are
	// skipped by digest, the changed one bumps to v2 and retires its old
	// revision (same arithmetic the single-doc path performed).
	recs2 := []map[string]any{rec("k1", "一"), rec("k2", "二改"), rec("k3", "三")}
	n, err = st.IngestJSONL(ctx, "j", recs2, mapFn)
	if err != nil || n != 3 {
		t.Fatalf("re-run: n=%d err=%v", n, err)
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

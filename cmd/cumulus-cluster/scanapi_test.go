package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
)

// POST /v1/scan is the workbench's discovery step: rules over a server-local
// directory, no store touched.
func TestScanFace(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct{ name, body string }{
		{"pool.md", "连接池最大 200。"},
		{"notes.txt", "runbook"},
		{"data.csv", "x,y"},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	registerScanFace(mux, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/scan", strings.NewReader(body)))
		return w
	}

	// Happy path: two candidates, the csv skipped.
	w := post(`{"dir":"` + dir + `","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var rep ingest.ScanReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Candidates) != 2 || rep.Skipped["ext"] != 1 {
		t.Fatalf("scan report: %+v", rep)
	}
	for _, c := range rep.Candidates {
		if c.Size == 0 || c.Headline == "" {
			t.Fatalf("candidate metadata: %+v", c)
		}
	}

	// Freshness + limit parse through the duration string.
	w = post(`{"dir":"` + dir + `","newer_than":"not-a-duration"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad newer_than must 400: %d", w.Code)
	}
	w = post(`{"dir":"","recursive":false}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing dir must 400: %d", w.Code)
	}
	w = post(`{"dir":"` + dir + `","ns":"bad:ns"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("illegal ns must 400: %d", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/scan", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must 405: %d", w.Code)
	}

	// A nonexistent dir is a clean 400, not a panic.
	w = post(`{"dir":"/no/such/dir"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing dir must 400: %d", w.Code)
	}
}

// The candidate list then feeds the SAME job state machine — the e2e Gate AA
// asserts the loop; here the store side is pinned directly.
func TestIngestCandidatesThroughJobMachine(t *testing.T) {
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer eng.Close()
	st := ingest.New(eng, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(ctx, "clus_weak_edges", "clus_cites", "clus_conflicts"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.md"), filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("连接池最大 200。"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("runbook"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := st.IngestCandidates(ctx, []string{a, b}, "cand1")
	if err != nil {
		t.Fatalf("ingest candidates: %v", err)
	}
	if n.Stored() != 2 {
		t.Fatalf("processed = %+v, want 2 stored", n)
	}
	d, err := st.GetJobDoc(ctx, "cand1")
	if err != nil {
		t.Fatalf("job doc: %v", err)
	}
	if d.State != "done" || d.Total != 2 || d.Done != 2 {
		t.Fatalf("job state: %+v", d)
	}
	// A fresh job key over the same files: the digest-addressed upserts are
	// idempotent, so no duplicate sources appear. Re-running the SAME job key
	// is a resume (cursor past the end) — zero processed, not a re-ingest.
	if c, err := st.IngestCandidates(ctx, []string{a, b}, "cand2"); err != nil || c.Stored() != 2 {
		t.Fatalf("rerun under a new job key: %+v err=%v", c, err)
	}
	if c, err := st.IngestCandidates(ctx, []string{a, b}, "cand1"); err != nil || c.Stored() != 0 {
		t.Fatalf("same-key rerun must resume to zero: %+v err=%v", c, err)
	}
	list, err := st.ActiveSources(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("active sources = %d, want 2 (no duplicates)", len(list))
	}
	// An empty list is refused (a silent no-op job would lie about progress).
	if _, err := st.IngestCandidates(ctx, nil, "empty"); err == nil {
		t.Fatalf("empty candidate list must error")
	}
}

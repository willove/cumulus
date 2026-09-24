package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/eval"
)

// GET /v1/evals lists scoreboard rows; /v1/evals/{id} reads one. Read-only:
// runs are written by eval-run (CLI).
func TestEvalFace(t *testing.T) {
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer eng.Close()
	if err := eng.EnsureCollection(ctx, "clus_evals"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	st := eval.NewCumuStore(eng, "clus_evals")
	if err := st.SaveRun(ctx, eval.RunDoc{
		ID: "run:1", Tag: "chinalaw39", N: 39, Judged: true,
		System: eval.Report{N: 39, EM: 0.667}, ClosedBook: eval.Report{N: 39, EM: 0.70},
		Modes: map[string]int{"DEEP": 30},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	mux := http.NewServeMux()
	registerEvalFace(mux, eng, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/evals?limit=10")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct {
		Namespace string        `json:"namespace"`
		Runs      []eval.RunDoc `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(list.Runs) != 1 || list.Runs[0].Tag != "chinalaw39" || list.Runs[0].System.EM != 0.667 {
		t.Fatalf("list payload: %+v", list)
	}

	resp, err = http.Get(srv.URL + "/v1/evals/run:1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	var one eval.RunDoc
	if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	resp.Body.Close()
	if one.ID != "run:1" || one.Modes["DEEP"] != 30 || one.Judged != true {
		t.Fatalf("detail payload: %+v", one)
	}

	// Unknown run: 404, not 500.
	r404, err := http.Get(srv.URL + "/v1/evals/run:missing")
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	r404.Body.Close()
	if r404.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run status = %d", r404.StatusCode)
	}
	// Illegal namespace is refused before any read.
	rns := httptest.NewRecorder()
	mux.ServeHTTP(rns, httptest.NewRequest(http.MethodGet, "/v1/evals?ns=bad:ns", nil))
	if rns.Code != http.StatusBadRequest {
		t.Fatalf("bad ns status = %d", rns.Code)
	}
	// POST is refused (runs come from the CLI).
	rp := httptest.NewRecorder()
	mux.ServeHTTP(rp, httptest.NewRequest(http.MethodPost, "/v1/evals", nil))
	if rp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rp.Code)
	}
}

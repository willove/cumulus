package main

// Eval scoreboard REST face (B3): the workbench 评测 pane lists past eval
// runs and reads one report. Runs are written by `eval-run` (CLI) into
// clus_evals; this face is read-only — triggering a run needs an items file
// and an LLM budget, which stays on the CLI (see evalrun.go).

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/ns"
)

func registerEvalFace(mux *http.ServeMux, c cumulite.Port, serveNS string) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		if err := ns.Validate(r.URL.Query().Get("ns")); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		reqNS := firstNonEmpty(r.URL.Query().Get("ns"), serveNS)
		store := eval.NewCumuStore(c, ns.Coll(reqNS, "clus_evals"))
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/evals"), "/")
		if id == "" {
			limit := 50
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
					limit = n
				}
			}
			runs, err := store.ListRuns(r.Context(), limit)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"namespace": reqNS, "runs": runs})
			return
		}
		run, err := store.GetRun(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if run == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		writeJSON(w, http.StatusOK, run)
	}
	mux.HandleFunc("/v1/evals", handler)
	mux.HandleFunc("/v1/evals/", handler)
}

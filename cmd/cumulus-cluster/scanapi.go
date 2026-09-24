package main

// Scan REST face (P9/B2): the workbench 摄取 panel's candidate-discovery
// step. POST /v1/scan walks a server-local directory under the rule set
// (extension / size / freshness, stratified cap) and returns the report;
// an optional query topic ranks the survivors through the LLM (opt-in, the
// same asset as the CLI). Scanning touches no store — like the CLI scan it
// is a pure pre-ingest step, and the trimmed list then goes to the existing
// POST /v1/ingest/jobs {candidates:[...]}.

import (
	"net/http"
	"time"

	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/ns"
)

// scanIn is the POST /v1/scan body.
type scanIn struct {
	Dir       string `json:"dir"`
	Recursive bool   `json:"recursive"`
	Limit     int    `json:"limit"`
	MaxSize   int64  `json:"max_size"`
	NewerThan string `json:"newer_than"` // duration string, e.g. "168h"; "" = off
	Query     string `json:"query"`      // optional LLM topic rank
	NS        string `json:"ns"`         // per-request namespace (ranking only)
}

func registerScanFace(mux *http.ServeMux, serveNS string) {
	mux.HandleFunc("/v1/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		var in scanIn
		if err := decode(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if in.Dir == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dir required"})
			return
		}
		if err := ns.Validate(in.NS); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		var newer time.Duration
		if in.NewerThan != "" {
			d, derr := time.ParseDuration(in.NewerThan)
			if derr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "newer_than: " + derr.Error()})
				return
			}
			newer = d
		}
		rep, err := ingest.ScanDir(in.Dir, ingest.ScanOptions{
			Recursive: in.Recursive, Limit: in.Limit, MaxSize: in.MaxSize, NewerThan: newer,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if in.Query != "" {
			// LLM topic rank over the survivors; a failure keeps the rule
			// order (discovery must never fail because the model did).
			if rerr := ingest.ApplyRank(r.Context(), &rep, in.Query, scanRankFunc(newProdStack().chat)); rerr != nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"dir": rep.Dir, "walked": rep.Walked, "candidates": rep.Candidates,
					"skipped": rep.Skipped, "rank_error": rerr.Error(),
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, rep)
	})
}

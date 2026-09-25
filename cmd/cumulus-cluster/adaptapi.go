package main

// Adapt REST face: the workbench 摄取 panel's heterogeneous-corpus path.
//
// POST /v1/adapt/probe   — what is this file (container + available fields)?
// POST /v1/adapt/ingest  — ingest selected files with an explicit field map,
//                          through the same resumable job state machine.
//
// The probe exists so a mapping is chosen from the file's ACTUAL columns rather
// than guessed: a fixed id/text map works for mmarco and silently produces empty
// documents for baidu_baike, whose columns are title/content.

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/adapt"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
)

// adaptProbeIn is the POST /v1/adapt/probe body.
type adaptProbeIn struct {
	Paths []string `json:"paths"` // files to probe (server-local paths)
}

// adaptIngestIn is the POST /v1/adapt/ingest body.
type adaptIngestIn struct {
	Paths []string `json:"paths"` // explicit file list (wins over Dir)
	Dir   string   `json:"dir"`   // or a directory's immediate files
	Job   string   `json:"job"`   // job key (resumable cursor)
	NS    string   `json:"ns"`    // bucket to ingest into (required)
	ID    string   `json:"id"`    // field holding the business key
	Title string   `json:"title"` // field holding the title
	Body  string   `json:"body"`  // field holding the text
	Extra []string `json:"extra"` // fields copied into meta
}

// itoaMS / nowMS / contextWithTimeout keep this file free of ad-hoc formatting.
func itoaMS(ms int64) string { return strconv.FormatInt(ms, 10) }
func nowMS() int64           { return time.Now().UnixMilli() }
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func registerAdaptFace(mux *http.ServeMux, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, ensure *nsEnsurer, buckets *bucket.Store) {
	mux.HandleFunc("/v1/adapt/probe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		var in adaptProbeIn
		if err := decode(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if len(in.Paths) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "paths required"})
			return
		}
		out := make([]adapt.Probe, 0, len(in.Paths))
		var failed []string
		for _, p := range in.Paths {
			pr, err := adapt.ProbeFile(r.Context(), p)
			if err != nil {
				// One unreadable file must not fail the whole probe: report it
				// and carry on, exactly like the scan face's skip accounting.
				failed = append(failed, p+": "+err.Error())
				continue
			}
			out = append(out, pr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"probes": out, "failed": failed})
	})

	mux.HandleFunc("/v1/adapt/ingest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		var in adaptIngestIn
		if err := decode(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if !requireHTTPBucket(w, r, buckets, in.NS) {
			return
		}
		files := append([]string(nil), in.Paths...)
		if len(files) == 0 && in.Dir != "" {
			ents, err := os.ReadDir(in.Dir)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			for _, e := range ents {
				if !e.IsDir() {
					files = append(files, filepath.Join(in.Dir, e.Name()))
				}
			}
		}
		if len(files) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "paths or dir required"})
			return
		}
		sort.Strings(files)
		if err := ensure.declare(r.Context(), in.NS); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		stForReq, _, err := storeForNS(c, st, serveNS, in.NS, sourcesColl)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		job := in.Job
		if job == "" {
			job = "adapt-" + itoaMS(nowMS())
		}
		fields := adapt.Fields{ID: in.ID, Title: in.Title, Body: in.Body, Extra: in.Extra}
		// Queue first (the panel polls this), then run in the background: a
		// 100k-record corpus must not hold the request open.
		if err := stForReq.PutJobDoc(r.Context(), job, ingest.JobDoc{
			State: "queued", Phase: "extracting", Total: len(files),
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		go func() {
			bg, cancel := contextWithTimeout(10 * time.Minute)
			defer cancel()
			runAdaptJob(bg, stForReq, files, fields, job)
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"job": job, "state": "queued", "total": len(files), "ns": in.NS,
		})
	})
}

func runAdaptJob(ctx context.Context, st *ingest.Store, files []string, fields adapt.Fields, job string) {
	if _, err := st.IngestAdapted(ctx, files, fields, job); err != nil {
		// A timeout must not prevent its own terminal write. Read the last
		// checkpoint on the fresh context too, preserving file/record counts,
		// phase and skip diagnostics instead of replacing them with zeroes.
		fw, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()
		d, readErr := st.GetJobDoc(fw, job)
		if readErr != nil {
			log.Printf("adapt job %s: read progress: %v", job, readErr)
			d = ingest.JobDoc{Total: len(files)}
		}
		d.State, d.Error = "failed", err.Error()
		if writeErr := st.PutJobDoc(fw, job, d); writeErr != nil {
			log.Printf("adapt job %s: write terminal state: %v (ingest: %v)", job, writeErr, err)
		}
	}
}

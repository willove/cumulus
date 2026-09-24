package main

// Bucket REST face: the registry the workbench reads and the gate the search
// face enforces. GET lists, POST creates (idempotent), DELETE unregisters.
//
// Buckets ARE namespaces: every collection and KV key the suite owns for a
// bucket is the same composite identity -ns builds, so isolation is the
// engine's, not ours. What this adds is enumeration plus the required-selection
// rule on the retrieval faces.

import (
	"net/http"

	"github.com/willove/cumulus/internal/bucket"
)

func registerBucketFace(mux *http.ServeMux, buckets *bucket.Store, serveNS string) {
	mux.HandleFunc("/v1/buckets", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			all, err := buckets.List(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"buckets": all, "default_ns": serveNS})
		case http.MethodPost:
			var in struct {
				Name  string `json:"name"`
				Label string `json:"label"`
				Note  string `json:"note"`
			}
			if err := decode(r, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if in.Name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name required"})
				return
			}
			b, err := buckets.Create(r.Context(), in.Name, in.Label, in.Note)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, b)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST only"})
		}
	})
	mux.HandleFunc("/v1/buckets/", func(w http.ResponseWriter, r *http.Request) {
		name := trimSlash(r.URL.Path)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bucket name required"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			b, err := buckets.Get(r.Context(), name)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if b == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "bucket not registered: " + name})
				return
			}
			writeJSON(w, http.StatusOK, b)
		case http.MethodDelete:
			if err := buckets.Remove(r.Context(), name); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"removed": name})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/DELETE only"})
		}
	})
}

func trimSlash(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

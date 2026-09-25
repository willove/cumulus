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
	"strings"

	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ns"
)

// requireHTTPBucket gates HTTP writes and searches, not legacy CLI ingest or
// read-only listings. Never substitute the serve namespace for an omitted ns.
func requireHTTPBucket(w http.ResponseWriter, r *http.Request, buckets *bucket.Store, name string) bool {
	err := ns.Validate(name)
	if err == nil {
		err = buckets.Require(r.Context(), name)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
			"hint":  `Select a bucket from GET /v1/buckets (create one with POST /v1/buckets), then send {"ns":"<bucket>"}`,
		})
		return false
	}
	return true
}

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
		// The name is the path MINUS the mount point. It used to be the whole
		// path ("/v1/buckets/law"), so ns validation saw the slashes, failed,
		// and both GET and DELETE returned 500 unconditionally — no test or UI
		// walked this route, which is how it shipped broken.
		name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/buckets"), "/")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bucket name required"})
			return
		}
		if strings.Contains(name, "/") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid bucket name: " + name})
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

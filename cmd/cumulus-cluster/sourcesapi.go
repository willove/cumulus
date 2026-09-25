package main

// Sources REST face: read-only listing and active document detail in the
// selected library. Only detail includes body text; neither route mutates
// sources. storeFor resolves the namespace exactly like the ingest faces do.

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/source"
)

func registerSourcesFace(mux *http.ServeMux, storeFor func(context.Context, string) (*ingest.Store, error)) {
	mux.HandleFunc("/v1/sources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		st, err := storeFor(r.Context(), r.URL.Query().Get("ns"))
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		srcs, err := st.ActiveSources(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Newest first: the document column reads top-down as "latest additions".
		sort.Slice(srcs, func(i, j int) bool { return srcs[i].IngestedAt.After(srcs[j].IngestedAt) })
		docs := make([]map[string]any, 0, len(srcs))
		for _, s := range srcs {
			docs = append(docs, map[string]any{
				"id": s.ID, "title": s.Title, "type": s.SourceType,
				"uri": s.SourceURI, "bytes": len(s.Body),
				"ingested_at": s.IngestedAt, "version": s.Version,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": docs})
	})
	mux.HandleFunc("/v1/sources/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
			return
		}
		// IDs are opaque revision identities and may contain #, Unicode or
		// slashes in a business key. Accept one URL-escaped path segment,
		// decoding once; never interpret an ID as a namespace or file path.
		segment := strings.TrimPrefix(r.URL.EscapedPath(), "/v1/sources/")
		id, err := url.PathUnescape(segment)
		if err != nil || strings.TrimSpace(id) == "" || strings.Contains(segment, "/") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "one URL-escaped source id required"})
			return
		}
		st, err := storeFor(r.Context(), r.URL.Query().Get("ns"))
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		s, err := st.Get(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if s == nil || s.Status != source.StatusActive {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "source not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": s.ID, "title": s.Title, "type": s.SourceType,
			"uri": s.SourceURI, "body": s.Body, "version": s.Version,
			"ingested_at": s.IngestedAt,
		})
	})
}

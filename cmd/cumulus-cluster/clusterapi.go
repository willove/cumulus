package main

// Cluster browse REST face: the web workbench's cluster page reads these.
// List is namespace-scoped like every other face; detail carries the
// cluster's cite edges so the UI can render the cluster → source evidence
// links without a second round trip.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/ns"
)

// registerClusterFace mounts GET /v1/clusters and GET /v1/clusters/{id}.
func registerClusterFace(mux *http.ServeMux, c cumulite.Port, serveNS string) {
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
		store := cluster.NewCumuStore(c, ns.Coll(reqNS, "clus_clusters"))
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/clusters"), "/")
		if id == "" {
			limit := 100
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
					limit = n
				}
			}
			all, err := store.All(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			// Freshest first — the UI shows evolving knowledge, not archaeology.
			sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
			if want := r.URL.Query().Get("lifecycle"); want != "" {
				filtered := all[:0]
				for _, cl := range all {
					if cl.Lifecycle == want {
						filtered = append(filtered, cl)
					}
				}
				all = filtered
			}
			if len(all) > limit {
				all = all[:limit]
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"namespace": reqNS, "clusters": all,
			})
			return
		}
		cl, err := store.Get(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if cl == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "cluster not found"})
			return
		}
		// Cite edges anchored on this cluster (same namespace), so the detail
		// view can render evidence links in one response.
		cites := []map[string]any{}
		if all, cerr := deep.NewCumuCiteStore(c, ns.Coll(reqNS, "clus_cites")).List(r.Context()); cerr == nil {
			for _, e := range all {
				if from, _ := e["_from"].(string); from == id {
					cites = append(cites, e)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"namespace": reqNS, "cluster": cl, "cites": cites,
		})
	}
	mux.HandleFunc("/v1/clusters", handler)
	mux.HandleFunc("/v1/clusters/", handler)
}

package main

// Monitor REST face: GET /v1/monitor (composite), plus the per-block routes a
// dashboard polls independently. Read-only — nothing here mutates state.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/monitor"
	"github.com/willove/cumulus/internal/ns"
)

func registerMonitorFace(mux *http.ServeMux, tr *monitor.Tracker, dataDir, serveNS string, know func(context.Context, string) *monitor.Knowledge) {
	block := func(name string, pick func(monitor.Snapshot) any) {
		mux.HandleFunc("/v1/monitor/"+name, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
				return
			}
			reqNS := r.URL.Query().Get("ns")
			if err := ns.Validate(reqNS); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			s := snapshot(r.Context(), tr, dataDir)
			// Knowledge belongs to this request's namespace, never to the
			// shared tracker: interleaved A/B reads must not swap populations.
			s.Knowledge = nil
			if know != nil {
				s.Knowledge = know(r.Context(), firstNonEmpty(reqNS, serveNS))
			}
			writeJSON(w, http.StatusOK, pick(s))
		})
	}
	block("overview", func(s monitor.Snapshot) any { return s })
	block("system", func(s monitor.Snapshot) any { return s.System })
	block("llm", func(s monitor.Snapshot) any { return s.LLM })
	block("retrieval", func(s monitor.Snapshot) any { return s.Retrieval })
	block("knowledge", func(s monitor.Snapshot) any { return s.Knowledge })
	block("namespaces", func(s monitor.Snapshot) any { return s.Namespaces })
}

// clusterKnowledge reads the L2 population for one namespace. It is deliberately
// best-effort: the monitor must not fail a request because a collection is
// absent (a fresh store has none), so an unreadable collection reports an empty
// block rather than an error.
func clusterKnowledge(ctx context.Context, c cumulite.Port, namespace string) *monitor.Knowledge {
	store := cluster.NewCumuStore(c, ns.Coll(namespace, "clus_clusters"))
	all, err := store.All(ctx)
	if err != nil {
		return nil
	}
	k := &monitor.Knowledge{ByLifecycle: map[string]int{}}
	for _, cl := range all {
		k.Clusters++
		k.ByLifecycle[cl.Lifecycle]++
		k.AvgConfidence += cl.Confidence
		k.AvgHotness += cl.Hotness
		k.EvidenceWindows += len(cl.Evidence)
		switch cl.Lifecycle {
		case cluster.LifecycleContested:
			k.Contested++
		case cluster.LifecycleEmerging:
			k.NeedingReview++
		}
	}
	if k.Clusters > 0 {
		k.AvgConfidence /= float64(k.Clusters)
		k.AvgHotness /= float64(k.Clusters)
	}
	return k
}

// snapshot builds the monitor read model. Store size is read here because the
// engine port is what knows it; a failure is reported as 0 rather than failing
// the whole monitor response.
func snapshot(ctx context.Context, tr *monitor.Tracker, dataDir string) monitor.Snapshot {
	_ = ctx
	s := tr.Snapshot(storeSize(dataDir), dataDir)
	return s
}

// storeSize sums the store directory's size. It walks the directory, so it is
// bounded by what the OS reports and never blocks on the engine.
func storeSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

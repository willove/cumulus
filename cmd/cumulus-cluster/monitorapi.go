package main

// Monitor REST face: GET /v1/monitor (composite), plus the per-block routes a
// dashboard polls independently. Read-only — nothing here mutates state.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/willove/cumulus/internal/monitor"
)

func registerMonitorFace(mux *http.ServeMux, tr *monitor.Tracker, dataDir, serveNS string) {
	block := func(name string, pick func(monitor.Snapshot) any) {
		mux.HandleFunc("/v1/monitor/"+name, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
				return
			}
			writeJSON(w, http.StatusOK, pick(snapshot(r.Context(), tr, dataDir)))
		})
	}
	block("overview", func(s monitor.Snapshot) any { return s })
	block("system", func(s monitor.Snapshot) any { return s.System })
	block("llm", func(s monitor.Snapshot) any { return s.LLM })
	block("retrieval", func(s monitor.Snapshot) any { return s.Retrieval })
	block("namespaces", func(s monitor.Snapshot) any { return s.Namespaces })
}

// snapshot builds the monitor read model. Store size is read here because the
// engine port is what knows it; a failure is reported as 0 rather than failing
// the whole monitor response.
func snapshot(ctx context.Context, tr *monitor.Tracker, dataDir string) monitor.Snapshot {
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

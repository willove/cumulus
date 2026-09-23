package main

// Web UI: a single self-contained page embedded into the binary —
// the appliance serves its own workbench with zero Node runtime. The page
// talks to /v1/search/stream (SSE) and /v1/sessions (KV) directly.

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:web/dist
var webDist embed.FS

// registerWebFace serves the embedded UI at /ui/ and redirects / there.
func registerWebFace(mux *http.ServeMux) {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		panic(err) // pinned layout; unreachable for the embedded build
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/ui/", http.StripPrefix("/ui/", fileServer))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// suiteExtra composes the suite collections one namespace needs beyond the
// store's own sources/evidence/clusters (weak edges, cites, conflicts).
func suiteExtra(namespace string) []string {
	return []string{
		ns.Coll(namespace, "clus_weak_edges"),
		ns.Coll(namespace, "clus_cites"),
		ns.Coll(namespace, "clus_conflicts"),
		ns.Coll(namespace, "clus_evals"),
	}
}

// nsEnsurer memoizes the per-namespace suite declaration and hands out the
// right store for a request. It exists because the engine fail-closes writes
// to collections it has never seen: any face that may write (ingest upserts,
// the search path's cluster persist, the MCP tools) must declare the target
// namespace's collections before writing, or the first write dies on a raw
// "collection not found" — including on a namespace the operator only ever
// searched in.
type nsEnsurer struct {
	c           cumulite.Port
	st          *ingest.Store
	serveNS     string
	sourcesColl string
	mu          sync.Mutex
	ensured     map[string]bool
}

func newNSEnsurer(c cumulite.Port, st *ingest.Store, serveNS, sourcesColl string) *nsEnsurer {
	return &nsEnsurer{c: c, st: st, serveNS: serveNS, sourcesColl: sourcesColl, ensured: map[string]bool{}}
}

// declare ensures the suite collections for one namespace, once per process.
func (n *nsEnsurer) declare(ctx context.Context, nsForReq string) error {
	n.mu.Lock()
	first := !n.ensured[nsForReq]
	n.ensured[nsForReq] = true
	n.mu.Unlock()
	if !first {
		return nil
	}
	stForReq, _, err := storeForNS(n.c, n.st, n.serveNS, nsForReq, n.sourcesColl)
	if err != nil {
		return err
	}
	if _, err := stForReq.Ensure(ctx, suiteExtra(nsForReq)...); err != nil {
		return err
	}
	return nil
}

// store returns the store for a request namespace with its collections
// declared. reqNS "" falls back to the serve-level namespace.
func (n *nsEnsurer) store(ctx context.Context, reqNS string) (*ingest.Store, error) {
	if err := n.declare(ctx, firstNonEmpty(reqNS, n.serveNS)); err != nil {
		return nil, err
	}
	stForReq, _, err := storeForNS(n.c, n.st, n.serveNS, reqNS, n.sourcesColl)
	return stForReq, err
}

// sourceIn / jobIn are the HTTP ingest face payloads.
type sourceIn struct {
	Title string         `json:"title"`
	Type  string         `json:"type"`
	URI   string         `json:"uri"`
	Key   string         `json:"key"`
	Lang  string         `json:"lang"`
	Body  string         `json:"body"`
	Meta  map[string]any `json:"meta"`
	NS    string         `json:"ns"` // per-request namespace; empty = serve's -ns
}

type jobIn struct {
	Dir        string   `json:"dir"`
	Job        string   `json:"job"`
	Recursive  bool     `json:"recursive"`
	Candidates []string `json:"candidates"` // explicit file list (P9 scan output); wins over Dir
	NS         string   `json:"ns"`         // per-request namespace; empty = serve's -ns
}

// runServe exposes the HTTP faces: ingest (/health, POST /v1/ingest/sources,
// POST /v1/ingest/jobs, GET /v1/ingest/jobs/{id}), search (POST
// /v1/search, POST /v1/search/stream), sessions, clusters, MCP
// (POST /mcp) and the workbench (/ui/). serveNS is the default namespace for
// every face; request bodies may override it per call.
func runServe(ctx context.Context, c cumulite.Port, st *ingest.Store, listen, sourcesColl, serveNS string, verbose bool) {
	mux := http.NewServeMux()

	// Boot: declare the suite collections for the default namespace. The
	// engine fail-closes writes to collections it has never seen, so an
	// ingest on a fresh store (the workbench 摄取 panel starts here) would
	// otherwise die on a raw "not found" instead of ingesting.
	if _, err := st.Ensure(ctx, suiteExtra(serveNS)...); err != nil {
		fatal(err)
	}

	// nsEnsurer declares a namespace's suite collections the first time a
	// request reaches it. The engine fail-closes writes to collections it has
	// never seen, so EVERY face that may write (ingest, search's cluster
	// persist) must pass through here — not just ingest. serveNS is declared
	// at boot below.
	ensure := newNSEnsurer(c, st, serveNS, sourcesColl)

	registerSearchFace(mux, c, st, sourcesColl, serveNS, verbose, ensure)
	registerSessionFace(mux, c, serveNS)
	registerClusterFace(mux, c, serveNS)
	registerMCPFace(mux, c, st, sourcesColl, serveNS, verbose, ensure)
	registerScanFace(mux, serveNS)
	registerEvalFace(mux, c, serveNS)
	registerModelFace(mux)
	logModelReminder()
	registerWebFace(mux)

	// scopedStore returns the default store, or a per-request store when the
	// body asks for a different namespace than the server's own, with that
	// namespace's collections declared on first sighting (idempotent).
	scopedStore := ensure.store

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		h, err := c.Health(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"status": "upstream", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "store": h})
	})

	mux.HandleFunc("/v1/ingest/sources", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		var in sourceIn
		if err := decode(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		rst, err := scopedStore(r.Context(), in.NS)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		typ := in.Type
		if typ == "" {
			typ = "jsonl"
		}
		lang := in.Lang
		if lang == "" {
			lang = "zh"
		}
		body := in.Body
		switch typ {
		case "html":
			body = ingest.ExtractHTML(body)
		case "docx":
			docxText, derr := ingest.ExtractDOCX([]byte(body))
			if derr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": derr.Error()})
				return
			}
			body = docxText
		case "pdf":
			body = ingest.ExtractPDF([]byte(body))
		}
		res, err := rst.Put(r.Context(), source.New(in.Title, typ, in.URI, in.Key, lang, body, in.Meta))
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		code := http.StatusOK
		if res.Status == "created" {
			code = http.StatusCreated
		}
		writeJSON(w, code, res)
	})

	jobsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var in jobIn
			if err := decode(r, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if in.Job == "" {
				in.Job = "http-" + fmt.Sprint(time.Now().UnixMilli())
			}
			rst, err := scopedStore(r.Context(), in.NS)
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			// P9: an explicit candidate list replaces the walk — the
			// pipeline (state machine, cursor, upserts) is the same.
			if len(in.Candidates) > 0 {
				if err := rst.PutJobDoc(r.Context(), in.Job, ingest.JobDoc{
					State: "queued", Phase: "extracting", Total: len(in.Candidates),
				}); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				go func() {
					bg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer cancel()
					if _, err := rst.IngestCandidates(bg, in.Candidates, in.Job); err != nil {
						_ = rst.PutJobDoc(bg, in.Job, ingest.JobDoc{
							State: "failed", Phase: "upserting", Error: err.Error(),
						})
					}
				}()
				writeJSON(w, http.StatusAccepted, map[string]any{
					"job": in.Job, "state": "queued", "total": len(in.Candidates),
				})
				return
			}
			if in.Dir == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dir or candidates required"})
				return
			}
			if !dirExists(in.Dir) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dir not found"})
				return
			}
			total, err := countIngestable(in.Dir, in.Recursive)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if err := rst.PutJobDoc(r.Context(), in.Job, ingest.JobDoc{
				State: "queued", Phase: "extracting", Total: total,
			}); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			// Async run: the goroutine owns the state machine from here.
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if _, err := rst.IngestFiles(bg, in.Dir, in.Recursive, in.Job); err != nil {
					_ = rst.PutJobDoc(bg, in.Job, ingest.JobDoc{
						State: "failed", Phase: "upserting", Error: err.Error(),
					})
				}
			}()
			writeJSON(w, http.StatusAccepted, map[string]any{
				"job": in.Job, "state": "queued", "total": total,
			})
		case http.MethodGet:
			// GET /v1/ingest/jobs/{id}: routed here via the trailing segment.
			id := strings.TrimPrefix(r.URL.Path, "/v1/ingest/jobs/")
			id = strings.Trim(id, "/")
			if id == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "job id required"})
				return
			}
			rst, err := scopedStore(r.Context(), r.URL.Query().Get("ns"))
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			d, err := rst.GetJobDoc(r.Context(), id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if d.State == "" {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
				return
			}
			writeJSON(w, http.StatusOK, d)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST/GET only"})
		}
	})
	mux.HandleFunc("/v1/ingest/jobs", jobsHandler)
	mux.HandleFunc("/v1/ingest/jobs/", jobsHandler) // GET /v1/ingest/jobs/{id}

	// Timeouts: ReadHeaderTimeout guards slowloris headers. ReadTimeout and
	// WriteTimeout are deliberately NOT set — a DEEP search legitimately runs
	// for minutes and an SSE stream is open-ended, so a blanket write deadline
	// would kill the very requests this face exists to serve. IdleTimeout
	// still reaps dead keep-alive connections.
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("cumulus-cluster serve on %s", listen)
	// Graceful shutdown on ctx cancellation: in-flight SSE searches finish,
	// the port is released instead of being killed mid-write.
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil && err != context.DeadlineExceeded {
			log.Printf("serve shutdown: %v", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	return nil
}

// writeStoreErr maps a store/namespace failure to the right status: an invalid
// namespace segment is the caller's fault (400); everything else is the engine
// refusing the operation (500). Both used to be 400, so an operator debugging a
// store problem saw "bad request".
func writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ns.ErrInvalidNamespace) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func dirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// countIngestable mirrors the walk in ingest.walkIngestable so the queued job
// reports the real Total. It drifted once before (four extensions here, six in
// the walk) and the UI progress lied.
func countIngestable(dir string, recursive bool) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".md", ".txt", ".html", ".htm", ".docx", ".pdf":
			n++
		}
		return nil
	})
	return n, err
}

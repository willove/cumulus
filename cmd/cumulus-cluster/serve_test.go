package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/adapt"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/monitor"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

// Namespace declaration is still available to legacy CLI callers without a
// registered bucket. The HTTP write gate is tested separately below; changing
// the HTTP contract must not restrict the internal store/CLI ingest rules.
func TestLegacyStoreEnsuresNamespacesBeforeFirstWrite(t *testing.T) {
	t.Setenv("AIGATE_BASE_URL", "") // offline: no network in tests
	ctx := context.Background()

	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer engine.Close()

	// Boot path: the default library's suite, fully declared.
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatalf("boot ensure (default library): %v", err)
	}

	// Scoped path: first sighting of a namespace declares its suite, then
	// accepts a write on the fresh store.
	scoped, scopedSources, err := storeForNS(engine, base, "", "t1", "clus_sources")
	if err != nil {
		t.Fatalf("storeForNS: %v", err)
	}
	if scopedSources != "t1:clus_sources" {
		t.Fatalf("scoped sources identity = %q", scopedSources)
	}
	if _, err := scoped.Ensure(ctx, suiteExtra("t1")...); err != nil {
		t.Fatalf("scoped ensure: %v", err)
	}
	res, err := scoped.Put(ctx, source.New("t1 手册", "md", "", "h1", "zh", "连接池上限 200", nil))
	if err != nil {
		t.Fatalf("scoped put after ensure: %v", err)
	}
	if res.Status != "created" {
		t.Fatalf("scoped put status = %+v", res)
	}

	// Isolation holds: the default library never sees the tenant's doc.
	if src, err := base.Get(ctx, res.ID); err != nil || src != nil {
		t.Fatalf("default library leaked tenant source: src=%v err=%v", src, err)
	}
}

// M10 + bucket gate: a search must NAME a registered bucket. The gate refuses
// an empty one (no silent fallback to the default library) and an unregistered
// one, and a registered bucket in a namespace whose collections were never
// declared must still persist its cluster.
func TestSearchRequiresRegisteredBucket(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AIGATE_BASE_URL", "") // offline stubs: no network in tests
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const nsName = "searchonly"
	srcColl := ns.Coll(nsName, "clus_sources")
	if err := engine.EnsureCollection(ctx, srcColl); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Insert(ctx, srcColl, []map[string]any{{
		"_id": "src:d", "title": "手册", "source_type": "md",
		"body": "连接池最大 128，超时 30 秒。", "business_key": "d",
		"lang": "zh", "digest": "x", "status": "active", "version": 1,
	}}); err != nil {
		t.Fatal(err)
	}
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatal(err)
	}
	buckets := bucket.New(engine)
	mux := http.NewServeMux()
	ens := newNSEnsurer(engine, base, "", "clus_sources")
	registerSearchFace(mux, engine, base, "clus_sources", "", false, ens, buckets, monitor.New())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]any) {
		resp, err := http.Post(srv.URL+"/v1/search", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	// 1) No bucket named at all → refused, not defaulted.
	resp, out := post(`{"query":"连接池最大是多少"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unnamed bucket must be refused: %d %v", resp.StatusCode, out)
	}
	if _, ok := out["hint"]; !ok {
		t.Fatalf("the refusal must point at the bucket registry: %v", out)
	}
	// 2) Named but unregistered → refused.
	resp, _ = post(`{"query":"连接池最大是多少","ns":"` + nsName + `"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unregistered bucket must be refused: %d", resp.StatusCode)
	}
	// 3) Registered → serves, and persists the cluster into that namespace.
	if _, err := buckets.Create(ctx, nsName, "测试桶", ""); err != nil {
		t.Fatal(err)
	}
	resp, out = post(`{"query":"连接池最大是多少","ns":"` + nsName + `"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a registered bucket must serve: %d %v", resp.StatusCode, out)
	}
	if out["cluster_id"] == "" || out["cluster_id"] == nil {
		t.Fatalf("the answer must persist a cluster in the bucket: %+v", out)
	}
	// 4) The query is recorded against the bucket (registry telemetry).
	b, err := buckets.Get(ctx, nsName)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Queries == 0 {
		t.Fatalf("the bucket must record the query: %+v", b)
	}
	// 5) Re-listing the bucket shows the corpus/cluster counters.
	if b.Sources <= 0 || b.Clusters <= 0 {
		t.Fatalf("bucket counters must be refreshed: %+v", b)
	}
}

func serveJSON(t *testing.T, h http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(string(raw))))
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%s %s: expected JSON, got %d %s", method, path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: %v; %s", method, path, err, w.Body.String())
	}
	return w, out
}

func waitHTTPJob(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		w, out := serveJSON(t, h, http.MethodGet, path, nil)
		if w.Code != http.StatusOK || out["state"] == "failed" {
			t.Fatalf("job polling: %d %v", w.Code, out)
		}
		if out["state"] == "done" {
			return out
		}
		select {
		case <-timer.C:
			t.Fatalf("job did not complete: %v", out)
		case <-tick.C:
		}
	}
}

// Exercise production handlers on a fresh store, including both job input
// forms. Merely ensuring a namespace must never register it implicitly.
func TestHTTPIngestRequiresRegisteredBucket(t *testing.T) {
	t.Setenv("CLUS_OFFLINE", "1")
	t.Setenv("CLUS_EMBED", "")
	t.Setenv("AIGATE_BASE_URL", "")
	for _, serveNS := range []string{"", "serve"} {
		for _, kind := range []string{"source", "directory", "candidates", "adapt"} {
			t.Run(serveNS+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				engine, err := cumulite.Open("", cumulite.WithInMemory())
				if err != nil {
					t.Fatal(err)
				}
				defer engine.Close()
				sourcesColl := ns.Coll(serveNS, "clus_sources")
				base := ingest.New(engine, sourcesColl, ns.Coll(serveNS, "clus_evidence"), ns.Coll(serveNS, "clus_clusters"), serveNS)
				ens := newNSEnsurer(engine, base, serveNS, sourcesColl)
				buckets := bucket.New(engine)
				mux := http.NewServeMux()
				registerIngestFace(mux, ens, buckets)
				registerAdaptFace(mux, engine, base, sourcesColl, serveNS, ens, buckets)
				registerSearchFace(mux, engine, base, sourcesColl, serveNS, false, ens, buckets, monitor.New())
				registerBucketFace(mux, buckets, serveNS)
				registerSourcesFace(mux, ens.store)

				w, _ := serveJSON(t, mux, http.MethodGet, "/v1/buckets", nil)
				if w.Code != http.StatusOK {
					t.Fatal(w.Body.String())
				}
				if serveNS != "" {
					// Even a registered serve default is not an explicit selection.
					if _, err := buckets.Create(ctx, serveNS, "", ""); err != nil {
						t.Fatal(err)
					}
				}
				dir := t.TempDir()
				file := filepath.Join(dir, "manual.txt")
				if err := os.WriteFile(file, []byte("连接池最大 128，超时 30 秒。"), 0600); err != nil {
					t.Fatal(err)
				}
				path := "/v1/ingest/jobs"
				in := map[string]any{"job": "first"}
				switch kind {
				case "source":
					path = "/v1/ingest/sources"
					in["title"], in["body"] = "手册", "连接池最大 128，超时 30 秒。"
				case "directory":
					in["dir"] = dir
				case "candidates":
					in["candidates"] = []string{file}
				case "adapt":
					path = "/v1/adapt/ingest"
					in["paths"] = []string{file}
				}
				for _, name := range []string{"(omitted)", "", "newbucket", "bad:ns"} {
					in["ns"] = name
					if name == "(omitted)" {
						delete(in, "ns")
					}
					w, out := serveJSON(t, mux, http.MethodPost, path, in)
					if w.Code != http.StatusBadRequest || out["error"] == nil || out["hint"] == nil {
						t.Fatalf("ingest ns=%q: %d %v", name, w.Code, out)
					}
					for _, searchPath := range []string{"/v1/search", "/v1/search/stream"} {
						w, out = serveJSON(t, mux, http.MethodPost, searchPath, map[string]any{"ns": name, "query": "连接池最大是多少"})
						if w.Code != http.StatusBadRequest || out["error"] == nil || out["hint"] == nil {
							t.Fatalf("search ns=%q: %d %v", name, w.Code, out)
						}
					}
				}
				if len(ens.ensured) != 0 {
					t.Fatalf("rejected write declared namespaces: %v", ens.ensured)
				}
				if _, err := engine.KVGet(ctx, ns.KV("newbucket", "clus:job:first")); !contract.IsNotFound(err) {
					t.Fatalf("rejected request created a job: %v", err)
				}
				// Read-only lists keep the legacy empty/unregistered behavior.
				for _, suffix := range []string{"", "?ns=legacy"} {
					w, out := serveJSON(t, mux, http.MethodGet, "/v1/sources"+suffix, nil)
					if w.Code != http.StatusOK || len(out["sources"].([]any)) != 0 {
						t.Fatalf("empty list: %d %v", w.Code, out)
					}
				}
				w, out := serveJSON(t, mux, http.MethodPost, "/v1/buckets", map[string]any{"name": "newbucket"})
				if w.Code != http.StatusCreated {
					t.Fatalf("register: %d %v", w.Code, out)
				}
				in["ns"] = "newbucket"
				w, out = serveJSON(t, mux, http.MethodPost, path, in)
				if kind == "source" {
					if w.Code != http.StatusCreated {
						t.Fatalf("source: %d %v", w.Code, out)
					}
				} else {
					if w.Code != http.StatusAccepted {
						t.Fatalf("queue: %d %v", w.Code, out)
					}
					job := waitHTTPJob(t, mux, "/v1/ingest/jobs/first?ns=newbucket")
					if job["done"] != float64(1) || job["total"] != float64(1) {
						t.Fatalf("progress: %v", job)
					}
				}
				w, out = serveJSON(t, mux, http.MethodGet, "/v1/sources?ns=newbucket", nil)
				if w.Code != http.StatusOK || len(out["sources"].([]any)) != 1 {
					t.Fatalf("ingested sources: %d %v", w.Code, out)
				}
				w, out = serveJSON(t, mux, http.MethodPost, "/v1/search", map[string]any{"ns": "newbucket", "query": "连接池最大是多少"})
				if w.Code != http.StatusOK || out["cluster_id"] == nil || out["cluster_id"] == "" {
					t.Fatalf("registered search: %d %v", w.Code, out)
				}
				w, out = serveJSON(t, mux, http.MethodGet, "/v1/sources", nil)
				if w.Code != http.StatusOK || len(out["sources"].([]any)) != 0 {
					t.Fatalf("default namespace contaminated: %d %v", w.Code, out)
				}
			})
		}
	}
}

func TestJobReadErrorsAndLegacyNamespaces(t *testing.T) {
	storeErr := errors.New("storage unavailable")
	for _, namespace := range []string{"", "unregistered"} {
		for _, tc := range []struct {
			name string
			raw  string
			err  error
			code int
		}{
			{"missing", "", contract.ErrNotFound, http.StatusNotFound},
			{"wrapped missing", "", fmt.Errorf("kv: %w", contract.ErrNotFound), http.StatusNotFound},
			{"storage error", "", storeErr, http.StatusInternalServerError},
			{"cancelled", "", context.Canceled, http.StatusInternalServerError},
			{"empty value", "", nil, http.StatusInternalServerError},
			{"corrupt", "not json", nil, http.StatusInternalServerError},
			{"legacy job", `{"state":"done","done":2}`, nil, http.StatusOK},
		} {
			t.Run(namespace+"/"+tc.name, func(t *testing.T) {
				p := newTestPort()
				p.seed(ns.KV(namespace, "clus:job:test"), tc.raw)
				p.OnKVGet = func(string) error { return tc.err }
				st := ingest.New(p, "", "", "", namespace)
				ens := newNSEnsurer(p, st, namespace, ns.Coll(namespace, "clus_sources"))
				ens.ensured[namespace] = true
				mux := http.NewServeMux()
				registerIngestFace(mux, ens, bucket.New(p))
				w, out := serveJSON(t, mux, http.MethodGet, "/v1/ingest/jobs/test?ns="+namespace, nil)
				if w.Code != tc.code {
					t.Fatalf("job: %d %v, want %d", w.Code, out, tc.code)
				}
				if tc.code == http.StatusOK && out["job"] != "test" {
					t.Fatalf("legacy job id not filled: %v", out)
				}
				if tc.err != nil && !contract.IsNotFound(tc.err) {
					if _, err := st.GetJobDoc(context.Background(), "test"); !errors.Is(err, tc.err) {
						t.Fatalf("store error lost: %v", err)
					}
				}
			})
		}
	}
}

// Unlike testPort, this port rejects operations on a cancelled context, just
// like production storage. No file needs opening to hit the timeout path.
type cancelledAdaptPort struct{ *testPort }

func (p cancelledAdaptPort) KVGet(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.testPort.KVGet(ctx, key)
}

func (p cancelledAdaptPort) KVPut(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.testPort.KVPut(ctx, key, value, ttl)
}

func (p cancelledAdaptPort) EnsureCollection(ctx context.Context, _ string) error {
	return ctx.Err()
}

func TestAdaptCancelledJobPreservesTerminalProgress(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", expired), func(t *testing.T) {
			p := cancelledAdaptPort{newTestPort()}
			st := ingest.New(p, "", "", "", "tenant")
			progress := ingest.JobDoc{
				State: "running", Phase: "upserting", Total: 5, Done: 3,
				Failed: 1, Records: 42, Skipped: 1,
				SkipReasons: map[string]int{"unreadable": 1},
				SkipErrors:  map[string]string{"missing.txt": "not found"},
			}
			if err := st.PutJobDoc(context.Background(), "timeout", progress); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if expired {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
			}
			runAdaptJob(ctx, st, []string{"unused.txt"}, adapt.Fields{}, "timeout")
			got, err := st.GetJobDoc(context.Background(), "timeout")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Error, ctx.Err().Error()) {
				t.Fatalf("terminal error lost cancellation cause: %q", got.Error)
			}
			progress.Job, progress.State, progress.Error = "timeout", "failed", got.Error
			progress.Updated = got.Updated
			if !reflect.DeepEqual(got, progress) || got.Updated == "" {
				t.Fatalf("terminal progress: got %+v, want %+v", got, progress)
			}
		})
	}
}

// bgJobs exists so shutdown can cancel and drain background jobs BEFORE the
// engine closes: stop() must cancel a job mid-budget and not return while the
// goroutine (or its terminal write) is still running.
func TestBgJobsStopCancelsAndDrains(t *testing.T) {
	b := newBgJobs()
	entered := make(chan struct{})
	released := make(chan struct{})
	b.run(time.Hour, func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		<-released // simulate the terminal-state write outliving the cancel
	})
	<-entered
	stopped := make(chan struct{})
	go func() {
		b.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while the job goroutine was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(released)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not drain the job goroutine")
	}
	// A second stop (shutdown idempotence) must not hang or panic.
	b.stop()
}

// The grace window must cover a full DEEP run (minutes), never regress below
// the old 30s, and honour an explicit override.
func TestShutdownGraceBounds(t *testing.T) {
	t.Setenv("CLUS_SHUTDOWN_GRACE", "")
	if d := shutdownGrace(); d < 5*time.Minute {
		t.Fatalf("default grace %s < 5m", d)
	}
	t.Setenv("CLUS_SHUTDOWN_GRACE", "45")
	if d := shutdownGrace(); d != 45*time.Second {
		t.Fatalf("override grace = %s, want 45s", d)
	}
	t.Setenv("CLUS_SHUTDOWN_GRACE", "5")
	if d := shutdownGrace(); d < 30*time.Second {
		t.Fatalf("sub-30s override must floor at 30s, got %s", d)
	}
}

// /v1/learning must count ONLY the request namespace's learning state. The
// handler used to pass a bare "clus_evidence", which never matched a tenant's
// ns-prefixed list entry — so the DEFAULT library's evidence was counted into
// the tenant's report and a fresh namespace read as not-clean.
func TestLearningFaceCountsOnlyTenantEvidence(t *testing.T) {
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx := context.Background()
	for _, coll := range []string{"clus_evidence", "tenant:clus_evidence"} {
		if err := engine.EnsureCollection(ctx, coll); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Insert(ctx, coll, []map[string]any{{"_id": "e1"}}); err != nil {
			t.Fatal(err)
		}
	}
	st := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	mux := http.NewServeMux()
	registerClusterFace(mux, engine, st, "", "clus_evidence")
	w, out := serveJSON(t, mux, http.MethodGet, "/v1/learning?ns=tenant", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("learning: %d %v", w.Code, out)
	}
	docs, _ := out["docs"].(map[string]any)
	if n, _ := docs["tenant:clus_evidence"].(float64); n != 1 {
		t.Fatalf("tenant evidence count = %v, want 1 (docs: %v)", docs["tenant:clus_evidence"], docs)
	}
	if n, ok := docs["clus_evidence"]; ok && n != float64(0) {
		t.Fatalf("default library leaked into the tenant report: clus_evidence=%v", n)
	}
}

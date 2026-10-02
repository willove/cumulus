package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/monitor"
)

func uploadFixture(t *testing.T) (*http.ServeMux, *nsEnsurer, string) {
	t.Helper()
	t.Setenv("CLUS_OFFLINE", "1")
	t.Setenv("CLUS_EMBED", "")
	t.Setenv("LLM_BASE_URL", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	ens := newNSEnsurer(engine, base, "", "clus_sources")
	buckets := bucket.New(engine)
	if _, err := buckets.Create(context.Background(), "uploads", "", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerIngestFace(mux, ens, buckets)
	registerSourcesFace(mux, ens.store, engine, "")
	registerSearchFace(mux, engine, base, "clus_sources", "", false, ens, buckets, monitor.New())
	return mux, ens, tmp
}

type uploadTestPart struct {
	name, text string
	size       int64 // when nonzero, stream this many bytes without allocating them
}

type uploadRepeatReader struct{}

func (uploadRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func postUpload(t *testing.T, h http.Handler, parts []uploadTestPart) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rd, wr := io.Pipe()
	mw := multipart.NewWriter(wr)
	contentType := mw.FormDataContentType()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer wr.Close()
		for _, p := range parts {
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "files", "filename": p.name}))
			part, err := mw.CreatePart(header)
			if err != nil {
				return
			}
			if p.size > 0 {
				_, err = io.CopyN(part, uploadRepeatReader{}, p.size)
			} else {
				_, err = io.WriteString(part, p.text)
			}
			if err != nil {
				return
			}
		}
		_ = mw.Close()
	}()
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/upload?ns=uploads", rd)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	_ = rd.Close()
	<-finished
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("upload: %d %s: %v", w.Code, w.Body.String(), err)
	}
	return w, out
}

func assertUploadClean(t *testing.T, root string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		entries, err := filepath.Glob(filepath.Join(root, "cumulus-upload-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("staging directories leaked: %v", entries)
		case <-tick.C:
		}
	}
}

func TestUploadNestedStableReadableSearchable(t *testing.T) {
	mux, ens, tmp := uploadFixture(t)
	parts := []uploadTestPart{
		{name: "selected/a/manual.txt", text: "连接池最大 128，超时 30 秒。"},
		{name: "selected/b/manual.txt", text: "连接池最大 128，超时 30 秒。"},
		{name: "selected/说明.HTML", text: "<p>备份每天凌晨执行。</p>"},
	}
	var previous map[string]string
	var previousJob string
	for attempt := 0; attempt < 2; attempt++ {
		w, out := postUpload(t, mux, parts)
		if w.Code != http.StatusAccepted || out["state"] != "queued" || out["total"] != float64(len(parts)) {
			t.Fatalf("queue: %d %v", w.Code, out)
		}
		job := out["job"].(string)
		if job == previousJob || !strings.HasPrefix(job, "upload-") || len(job) != 39 {
			t.Fatalf("job identity: %q, previous %q", job, previousJob)
		}
		previousJob = job
		doc := waitHTTPJob(t, mux, "/v1/ingest/jobs/"+job+"?ns=uploads")
		if doc["done"] != float64(len(parts)) || doc["records"] != float64(len(parts)) {
			t.Fatalf("job counters: %v", doc)
		}
		assertUploadClean(t, tmp)
		st, err := ens.store(context.Background(), "uploads")
		if err != nil {
			t.Fatal(err)
		}
		sources, err := st.ActiveSources(context.Background())
		if err != nil || len(sources) != len(parts) {
			t.Fatalf("sources: %v %v", sources, err)
		}
		ids := map[string]string{}
		for _, src := range sources {
			ids[src.BusinessKey] = src.ID
			if src.SourceURI != "upload://"+src.BusinessKey || !strings.HasPrefix(src.BusinessKey, "selected/") || src.Version != 1 {
				t.Fatalf("temporary path or unstable identity: %+v", src)
			}
			w, detail := serveJSON(t, mux, http.MethodGet, "/v1/sources/"+url.PathEscape(src.ID)+"?ns=uploads", nil)
			if w.Code != http.StatusOK || detail["body"] != src.Body || detail["uri"] != src.SourceURI {
				t.Fatalf("detail: %d %v", w.Code, detail)
			}
		}
		if attempt > 0 && !reflect.DeepEqual(previous, ids) {
			t.Fatalf("repeated upload changed identities: %v -> %v", previous, ids)
		}
		previous = ids
	}
	w, out := serveJSON(t, mux, http.MethodPost, "/v1/search", map[string]any{"ns": "uploads", "query": "连接池最大是多少"})
	if w.Code != http.StatusOK || out["cluster_id"] == nil || out["cluster_id"] == "" {
		t.Fatalf("search: %d %v", w.Code, out)
	}
	citations, _ := out["citations"].(map[string]any)
	refs, _ := citations["refs"].([]any)
	if len(refs) == 0 {
		t.Fatalf("uploaded sources not retrieved: %v", out)
	}
	for _, raw := range refs {
		ref := raw.(map[string]any)
		found := false
		for _, id := range previous {
			found = found || ref["source_id"] == id
		}
		if !found || ref["quote"] == "" {
			t.Fatalf("unreadable upload citation: %v", ref)
		}
	}
	for _, namespace := range []string{"", "other"} {
		w, out = serveJSON(t, mux, http.MethodGet, "/v1/sources?ns="+namespace, nil)
		if w.Code != http.StatusOK || len(out["sources"].([]any)) != 0 {
			t.Fatalf("namespace leaked: %d %v", w.Code, out)
		}
	}
}

type unreadUploadBody struct{ read bool }

func (b *unreadUploadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, fmt.Errorf("body must not be consumed")
}
func (*unreadUploadBody) Close() error { return nil }

func TestUploadNamespaceGateBeforeBody(t *testing.T) {
	mux, ens, tmp := uploadFixture(t)
	for _, namespace := range []string{"", "missing", "bad:ns"} {
		body := &unreadUploadBody{}
		r := httptest.NewRequest(http.MethodPost, "/v1/ingest/upload?ns="+namespace, body)
		r.Header.Set("Content-Type", "multipart/form-data; boundary=unused")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || body.read || !strings.Contains(w.Body.String(), "hint") {
			t.Fatalf("gate: %d %s read=%v", w.Code, w.Body.String(), body.read)
		}
	}
	if len(ens.ensured) != 0 {
		t.Fatalf("gate declared collections: %v", ens.ensured)
	}
	assertUploadClean(t, tmp)
}

func TestUploadRejectsInvalidBatchAtomically(t *testing.T) {
	mux, ens, tmp := uploadFixture(t)
	for _, name := range []string{
		"/absolute.txt", "../escape.txt", "dir/../escape.txt", "dir/./file.txt", "dir//file.txt",
		`C:\windows.txt`, "C:/windows.txt", `dir\file.txt`, ".hidden.txt", "dir/.hidden/file.txt",
		"dir/.file.txt", "file.json", "file.jsonl", "file.csv", "file.parquet", "file.exe", "", "good.txt",
	} {
		t.Run(name, func(t *testing.T) {
			w, out := postUpload(t, mux, []uploadTestPart{{name: "good.txt", text: "must not ingest"}, {name: name, text: "bad"}})
			if w.Code != http.StatusBadRequest || out["error"] == nil || out["job"] != nil {
				t.Fatalf("invalid batch: %d %v", w.Code, out)
			}
			assertUploadClean(t, tmp)
		})
	}
	for _, parts := range [][]uploadTestPart{nil, {{name: "empty.txt"}}} {
		w, out := postUpload(t, mux, parts)
		if w.Code != http.StatusBadRequest || out["error"] == nil {
			t.Fatalf("empty batch: %d %v", w.Code, out)
		}
		assertUploadClean(t, tmp)
	}
	if len(ens.ensured) != 0 {
		t.Fatalf("invalid batch reached store: %v", ens.ensured)
	}
}

func TestUploadMalformedMultipartAndBodyLimit(t *testing.T) {
	mux, _, tmp := uploadFixture(t)
	for _, tc := range []struct {
		name, contentType, body string
		length                  int64
		code                    int
	}{
		{"not multipart", "application/json", "{}", 0, 400},
		{"missing boundary", "multipart/form-data", "bad", 0, 400},
		{"empty body", "multipart/form-data; boundary=x", "", 0, 400},
		{"broken headers", "multipart/form-data; boundary=x", "--x\r\nbad header\r\n\r\n", 0, 400},
		{"truncated", "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=files; filename=a.txt\r\n\r\ntext", 0, 400},
		{"field", "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=other\r\n\r\nvalue\r\n--x--\r\n", 0, 400},
		{"known oversized body", "multipart/form-data; boundary=x", "", uploadBodyLimit + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/ingest/upload?ns=uploads", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			if tc.length > 0 {
				r.ContentLength = tc.length
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), "error") {
				t.Fatalf("malformed: %d %s", w.Code, w.Body.String())
			}
			assertUploadClean(t, tmp)
		})
	}
	// A multipart closing boundary must not bypass the overall HTTP body cap.
	prefix := "--x\r\nContent-Disposition: form-data; name=files; filename=a.txt\r\n\r\ntext\r\n--x--\r\n"
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/upload?ns=uploads", io.MultiReader(strings.NewReader(prefix), io.LimitReader(uploadRepeatReader{}, uploadBodyLimit)))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body cap: %d %s", w.Code, w.Body.String())
	}
	assertUploadClean(t, tmp)
}

func TestUploadSizeAndCountLimits(t *testing.T) {
	mux, ens, tmp := uploadFixture(t)
	many := make([]uploadTestPart, uploadFileCount+1)
	for i := range many {
		many[i] = uploadTestPart{name: fmt.Sprintf("dir/%d.txt", i), text: "x"}
	}
	batch := []uploadTestPart{}
	for i := 0; i < 4; i++ {
		batch = append(batch, uploadTestPart{name: fmt.Sprintf("%d.txt", i), size: uploadFileLimit})
	}
	batch = append(batch, uploadTestPart{name: "overflow.txt", text: "x"})
	for _, tc := range []struct {
		name  string
		parts []uploadTestPart
		match string
	}{
		{"file", []uploadTestPart{{name: "large.txt", size: uploadFileLimit + 1}}, "16 MiB"},
		{"batch", batch, "64 MiB"},
		{"count", many, "500 files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, out := postUpload(t, mux, tc.parts)
			if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(fmt.Sprint(out["error"]), tc.match) || out["job"] != nil {
				t.Fatalf("limit: %d %v", w.Code, out)
			}
			assertUploadClean(t, tmp)
		})
	}
	if len(ens.ensured) != 0 {
		t.Fatalf("oversize batch reached store: %v", ens.ensured)
	}
}

func TestUploadExtractionSkipsHaveRelativeErrorsAndCleanup(t *testing.T) {
	mux, _, tmp := uploadFixture(t)
	w, out := postUpload(t, mux, []uploadTestPart{
		{name: "selected/a/bad.docx", text: "not a zip"},
		{name: "selected/b/bad.docx", text: "also not a zip"},
		{name: "selected/blank.txt", text: " \x00\n"},
		{name: "selected/good.md", text: "valid document"},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("queue: %d %v", w.Code, out)
	}
	doc := waitHTTPJob(t, mux, "/v1/ingest/jobs/"+out["job"].(string)+"?ns=uploads")
	if doc["done"] != float64(4) || doc["records"] != float64(1) || doc["skipped"] != float64(3) {
		t.Fatalf("skip counters: %v", doc)
	}
	errors, _ := doc["skip_errors"].(map[string]any)
	for _, p := range []string{"selected/a/bad.docx", "selected/b/bad.docx", "selected/blank.txt"} {
		if errors[p] == nil || errors[p] == "" {
			t.Fatalf("relative error missing for %s: %v", p, doc)
		}
	}
	assertUploadClean(t, tmp)
}

type uploadFailurePort struct {
	cumulite.Port
	queue bool
}

func (p uploadFailurePort) KVPut(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if strings.Contains(key, "clus:job:upload-") && (p.queue || strings.HasSuffix(key, ":cursor")) {
		return fmt.Errorf("injected upload storage failure")
	}
	return p.Port.KVPut(ctx, key, value, ttl)
}

func TestUploadFailureCleanupAndCounters(t *testing.T) {
	for _, queueFailure := range []bool{true, false} {
		t.Run(fmt.Sprint("queue=", queueFailure), func(t *testing.T) {
			mux, ens, tmp := uploadFixture(t)
			ens.c = uploadFailurePort{Port: ens.c, queue: queueFailure}
			w, out := postUpload(t, mux, []uploadTestPart{{name: "a.txt", text: "first document"}, {name: "b.txt", text: "second document"}})
			if queueFailure {
				if w.Code != http.StatusInternalServerError || out["job"] != nil {
					t.Fatalf("queue failure: %d %v", w.Code, out)
				}
			} else {
				if w.Code != http.StatusAccepted {
					t.Fatalf("queue: %d %v", w.Code, out)
				}
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					_, doc := serveJSON(t, mux, http.MethodGet, "/v1/ingest/jobs/"+out["job"].(string)+"?ns=uploads", nil)
					if doc["state"] == "failed" {
						if doc["total"] != float64(2) || doc["done"] != float64(1) || doc["records"] != float64(1) || !strings.Contains(fmt.Sprint(doc["error"]), "injected") {
							t.Fatalf("failed counters lost: %v", doc)
						}
						break
					}
					select {
					case <-deadline.C:
						t.Fatalf("failure not terminal: %v", doc)
					case <-tick.C:
					}
				}
			}
			assertUploadClean(t, tmp)
		})
	}
}

func TestPlainCancelledJobPreservesProgress(t *testing.T) {
	p := cancelledAdaptPort{newTestPort()}
	st := ingest.New(p, "", "", "", "tenant")
	want := ingest.JobDoc{State: "running", Phase: "normalizing", Total: 5, Done: 3, Records: 2, Skipped: 1,
		SkipReasons: map[string]int{"empty": 1}, SkipErrors: map[string]string{"blank.txt": "empty"}}
	if err := st.PutJobDoc(context.Background(), "cancelled", want); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runFileJob(ctx, st, "cancelled", func(ctx context.Context) (int, error) {
		c, err := st.IngestCandidates(ctx, []string{"unused.txt"}, "cancelled")
		return c.Stored(), err
	})
	got, err := st.GetJobDoc(context.Background(), "cancelled")
	if err != nil {
		t.Fatal(err)
	}
	want.Job, want.State, want.Error, want.Updated = "cancelled", "failed", context.Canceled.Error(), got.Updated
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal counters: %+v, want %+v", got, want)
	}
}

func TestDirectoryQueueUsesSharedDiscovery(t *testing.T) {
	mux, _, _ := uploadFixture(t)
	dir := t.TempDir()
	for _, name := range []string{"selected/a.txt", "selected/b.md", ".hidden/ignore.txt", "selected/.ignore.txt", "ignore.json"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("document "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "selected/a.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	total, err := countIngestable(dir, true)
	if err != nil || total != 2 {
		t.Fatalf("count: %d %v", total, err)
	}
	w, out := serveJSON(t, mux, http.MethodPost, "/v1/ingest/jobs", map[string]any{"ns": "uploads", "dir": dir, "recursive": true, "job": "walk"})
	if w.Code != http.StatusAccepted || out["total"] != float64(total) {
		t.Fatalf("queue: %d %v", w.Code, out)
	}
	doc := waitHTTPJob(t, mux, "/v1/ingest/jobs/walk?ns=uploads")
	if doc["done"] != float64(total) || doc["records"] != float64(total) {
		t.Fatalf("walk counters: %v", doc)
	}
	w, out = serveJSON(t, mux, http.MethodGet, "/v1/sources?ns=uploads", nil)
	if w.Code != http.StatusOK || len(out["sources"].([]any)) != total {
		t.Fatalf("sources: %d %v", w.Code, out)
	}
	for _, raw := range out["sources"].([]any) {
		if !strings.Contains(raw.(map[string]any)["id"].(string), "selected/") {
			t.Fatalf("walk root lost in identity: %v", raw)
		}
	}
}

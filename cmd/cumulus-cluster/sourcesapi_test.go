package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/source"
)

// GET /v1/sources feeds the workbench's document column. The panel is
// read-only over the same clus_sources collection ingest writes — this test
// pins the three things the UI depends on: newest-first ordering, the absence
// of body text in the payload, and 405 off GET.
func TestSourcesFaceListsActiveDocuments(t *testing.T) {
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	buckets := bucket.New(engine)
	st := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBucketFace(mux, buckets, "")
	// storeFor mirrors runServe's scopedStore: empty ns answers the default store.
	registerSourcesFace(mux, func(ctx context.Context, nsName string) (*ingest.Store, error) {
		return st, nil
	}, engine, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	call := func(method, path, body string) (*http.Response, map[string]any) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	// Empty library first: the panel's empty state, not an error.
	resp, out := call(http.MethodGet, "/v1/sources", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET on empty library = %d %v", resp.StatusCode, out)
	}
	if n := len(out["sources"].([]any)); n != 0 {
		t.Fatalf("empty library listed %d sources, want 0", n)
	}

	put := func(title, body string, at time.Time) {
		t.Helper()
		if _, err := st.Put(context.Background(), source.Source{Title: title, Body: body, Lang: "zh",
			SourceType: "md", Status: source.StatusActive, IngestedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	put("值班 runbook", "queue overflow 先查 capacity 上限", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))
	put("网关改造设计", "连接池上限调整为 128", time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC))

	resp, out = call(http.MethodGet, "/v1/sources", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %v", resp.StatusCode, out)
	}
	docs, _ := out["sources"].([]any)
	if len(docs) != 2 {
		t.Fatalf("listed %d sources, want 2: %v", len(docs), out)
	}
	// Newest first: the panel reads top-down as "latest additions".
	first, _ := docs[0].(map[string]any)
	if first["title"] != "网关改造设计" {
		t.Fatalf("newest-first broken, got %v", first["title"])
	}
	// The document column must not leak body text to the browser.
	if _, leaked := first["body"]; leaked {
		t.Fatal("sources face leaked the document body")
	}

	resp, _ = call(http.MethodPost, "/v1/sources", `{}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/sources = %d, want 405 (read-only face)", resp.StatusCode)
	}
}

func TestSourceDetailActiveAndScoped(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	base := ingest.New(engine, "serve:clus_sources", "serve:clus_evidence", "serve:clus_clusters", "serve")
	ens := newNSEnsurer(engine, base, "serve", "serve:clus_sources")
	mux := http.NewServeMux()
	registerSourcesFace(mux, ens.store, engine, "serve")
	put := func(namespace, key, body string) (*ingest.Store, ingest.Result) {
		t.Helper()
		st, err := ens.store(ctx, namespace)
		if err != nil {
			t.Fatal(err)
		}
		res, err := st.Put(ctx, source.New("原文标题", "md", "fixture://manual", key, "zh", body, map[string]any{"private_meta": "not part of detail"}))
		if err != nil {
			t.Fatal(err)
		}
		return st, res
	}
	// Opaque IDs include characters that MUST be escaped by clients. The same
	// revision ID legitimately exists in A/B, but its body must stay scoped.
	_, alpha := put("alpha", "shared/文档?#%", "alpha 原文\n第二行")
	_, beta := put("beta", "shared/文档?#%", "beta 原文")
	if alpha.ID != beta.ID {
		t.Fatalf("fixture IDs differ: %s / %s", alpha.ID, beta.ID)
	}
	_, onlyA := put("alpha", "only-alpha", "only alpha sees this")
	_, legacy := put("", "legacy", "serve default remains readable")
	_, stale := put("alpha", "revision", "old")
	_, current := put("alpha", "revision", "new")
	st, deleted := put("alpha", "deleted", "deleted text")
	if err := st.Delete(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, query, body string
		version         int
	}{
		{alpha.ID, "?ns=alpha", "alpha 原文\n第二行", 1},
		{beta.ID, "?ns=beta", "beta 原文", 1},
		{legacy.ID, "", "serve default remains readable", 1},
		{current.ID, "?ns=alpha", "new", 2},
	} {
		w, out := serveJSON(t, mux, http.MethodGet, "/v1/sources/"+url.PathEscape(tc.id)+tc.query, nil)
		if w.Code != http.StatusOK || len(out) != 7 || out["id"] != tc.id || out["body"] != tc.body ||
			out["title"] != "原文标题" || out["type"] != "md" || out["uri"] != "fixture://manual" || out["version"] != float64(tc.version) {
			t.Fatalf("detail %s%s: %d %v", tc.id, tc.query, w.Code, out)
		}
		at, _ := out["ingested_at"].(string)
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			t.Fatalf("ingested_at: %q %v", at, err)
		}
	}
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/v1/sources/", http.StatusBadRequest},
		{"/v1/sources/%20", http.StatusBadRequest},
		{"/v1/sources/extra/segment?ns=alpha", http.StatusBadRequest},
		{"/v1/sources/" + url.PathEscape(alpha.ID) + "?ns=bad:ns", http.StatusBadRequest},
		{"/v1/sources/missing?ns=alpha", http.StatusNotFound},
		{"/v1/sources/" + url.PathEscape(onlyA.ID) + "?ns=beta", http.StatusNotFound},
		{"/v1/sources/" + url.PathEscape(alpha.ID), http.StatusNotFound},
		{"/v1/sources/" + url.PathEscape(alpha.ID) + "?ns=unregistered", http.StatusNotFound},
		{"/v1/sources/" + url.PathEscape(stale.ID) + "?ns=alpha", http.StatusNotFound},
		{"/v1/sources/" + url.PathEscape(deleted.ID) + "?ns=alpha", http.StatusNotFound},
	} {
		w, out := serveJSON(t, mux, http.MethodGet, tc.path, nil)
		if w.Code != tc.code || out["error"] == nil || out["body"] != nil {
			t.Fatalf("%s: %d %v, want %d", tc.path, w.Code, out, tc.code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		w, out := serveJSON(t, mux, method, "/v1/sources/"+url.PathEscape(alpha.ID)+"?ns=alpha", nil)
		if w.Code != http.StatusMethodNotAllowed || out["error"] == nil {
			t.Fatalf("%s detail: %d %v", method, w.Code, out)
		}
	}
	w, list := serveJSON(t, mux, http.MethodGet, "/v1/sources?ns=alpha", nil)
	if w.Code != http.StatusOK || len(list["sources"].([]any)) != 3 {
		t.Fatalf("active list: %d %v", w.Code, list)
	}
	for _, d := range list["sources"].([]any) {
		if _, ok := d.(map[string]any)["body"]; ok {
			t.Fatal("list must still omit bodies")
		}
	}
}

// DELETE /v1/sources/{id} is the UI's per-document delete. It must reuse the
// store's sanctioned soft delete, answer 404 for foreign-namespace / already
// deleted / missing ids, and drop the document from the active list.
func TestSourceDeleteSoftDeletesAndScopes(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	base := ingest.New(engine, "serve:clus_sources", "serve:clus_evidence", "serve:clus_clusters", "serve")
	ens := newNSEnsurer(engine, base, "serve", "serve:clus_sources")
	mux := http.NewServeMux()
	registerSourcesFace(mux, ens.store, engine, "serve")
	st, err := ens.store(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.Put(ctx, source.New("靶文档", "md", "fixture://del", "delete-me", "zh", "要删的正文", nil))
	if err != nil {
		t.Fatal(err)
	}
	id := res.ID

	del := func(path string) (*httptest.ResponseRecorder, map[string]any) {
		return serveJSON(t, mux, http.MethodDelete, path, nil)
	}
	// Foreign namespace must not see — or delete — the document.
	if w, out := del("/v1/sources/"+url.PathEscape(id)+"?ns=beta"); w.Code != http.StatusNotFound {
		t.Fatalf("delete via ns=beta: %d %v", w.Code, out)
	}
	if w, out := del("/v1/sources/missing?ns=alpha"); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d %v", w.Code, out)
	}
	w, out := del("/v1/sources/"+url.PathEscape(id)+"?ns=alpha")
	if w.Code != http.StatusOK || out["deleted"] != id {
		t.Fatalf("delete: %d %v", w.Code, out)
	}
	// Gone from detail and the active list; second delete is 404 (already gone).
	if w, _ := serveJSON(t, mux, http.MethodGet, "/v1/sources/"+url.PathEscape(id)+"?ns=alpha", nil); w.Code != http.StatusNotFound {
		t.Fatalf("detail after delete: %d", w.Code)
	}
	if w, out := del("/v1/sources/"+url.PathEscape(id)+"?ns=alpha"); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d %v", w.Code, out)
	}
	w, list := serveJSON(t, mux, http.MethodGet, "/v1/sources?ns=alpha", nil)
	if w.Code != http.StatusOK || len(list["sources"].([]any)) != 0 {
		t.Fatalf("list after delete: %d %v", w.Code, list)
	}
}

type sourceReadErrorPort struct{ cumulite.Port }

func (sourceReadErrorPort) GetDocument(context.Context, string, string) (map[string]any, error) {
	return nil, errors.New("source storage unavailable")
}

func TestSourceDetailStorageError(t *testing.T) {
	mux := http.NewServeMux()
	registerSourcesFace(mux, func(context.Context, string) (*ingest.Store, error) {
		return ingest.New(sourceReadErrorPort{}, "", "", "", ""), nil
	}, nil, "")
	w, out := serveJSON(t, mux, http.MethodGet, "/v1/sources/missing", nil)
	if w.Code != http.StatusInternalServerError || out["error"] != "source storage unavailable" {
		t.Fatalf("storage failure must not become 404: %d %v", w.Code, out)
	}
}

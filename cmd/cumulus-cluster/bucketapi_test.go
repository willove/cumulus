package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
)

// The detail routes used to hand the WHOLE path ("/v1/buckets/law") to the
// bucket store as the name: ns validation saw the slashes, failed, and both
// GET and DELETE returned 500 unconditionally. No test or UI walked these
// routes — this test is that walk.
func TestBucketDetailRoutes(t *testing.T) {
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	buckets := bucket.New(engine)
	mux := http.NewServeMux()
	registerBucketFace(mux, buckets, "")
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

	resp, out := call(http.MethodPost, "/v1/buckets", `{"name":"law","label":"法律"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %v", resp.StatusCode, out)
	}

	// The route that used to 500 must now resolve the name and serve it.
	resp, out = call(http.MethodGet, "/v1/buckets/law", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET detail = %d %v (the TrimPrefix fix must make this 200)", resp.StatusCode, out)
	}
	if out["name"] != "law" {
		t.Fatalf("GET detail body = %v", out)
	}

	resp, out = call(http.MethodGet, "/v1/buckets/nope", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown bucket = %d %v, want 404", resp.StatusCode, out)
	}

	resp, _ = call(http.MethodGet, "/v1/buckets/", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET with empty name = %d, want 400", resp.StatusCode)
	}
	resp, _ = call(http.MethodGet, "/v1/buckets/a/b", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET with a slash in the name = %d, want 400", resp.StatusCode)
	}

	resp, out = call(http.MethodDelete, "/v1/buckets/law", "")
	if resp.StatusCode != http.StatusOK || out["removed"] != "law" {
		t.Fatalf("DELETE detail = %d %v", resp.StatusCode, out)
	}
	resp, _ = call(http.MethodGet, "/v1/buckets/law", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d, want 404", resp.StatusCode)
	}
}

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/monitor"
)

// The corpus-vector seat must be the embedder the operator asked for: with
// CLUS_MINILM_REQUIRE=1 a weights-absent minilm is a hard error, not a
// silent hash backfill.
func TestEmbedderForStrictMinilm(t *testing.T) {
	t.Setenv("CLUS_EMBED", "minilm")
	t.Setenv("CLUS_MINILM_DIR", t.TempDir()) // present dir, no weights
	t.Setenv("CLUS_MINILM_REQUIRE", "")

	// Not required: silent hash fallback (unchanged behavior).
	fn, dims, model, err := embedderFor()
	if err != nil || fn == nil {
		t.Fatalf("non-strict must degrade silently: err=%v", err)
	}
	if model != "local-hash-64" || dims != 64 {
		t.Fatalf("fallback model: %s/%d", model, dims)
	}

	// Required: hard failure naming the cause.
	t.Setenv("CLUS_MINILM_REQUIRE", "1")
	if _, _, _, err := embedderFor(); err == nil || !strings.Contains(err.Error(), "CLUS_MINILM_REQUIRE") {
		t.Fatalf("strict minilm must fail: %v", err)
	}

	// No minilm requested: the flag alone changes nothing.
	t.Setenv("CLUS_EMBED", "")
	if _, _, model, err := embedderFor(); err != nil || model != "local-hash-64" {
		t.Fatalf("flag without CLUS_EMBED must be inert: %s %v", model, err)
	}
}

func TestSearchStackFailureIsTracked(t *testing.T) {
	t.Setenv("CLUS_OFFLINE", "1")
	t.Setenv("CLUS_EMBED", "minilm")
	t.Setenv("CLUS_MODEL_DIR", t.TempDir())
	t.Setenv("CLUS_MINILM_REQUIRE", "1")
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	st := ingest.New(engine, "", "", "", "")
	buckets := bucket.New(engine)
	if _, err := buckets.Create(context.Background(), "tenant", "", ""); err != nil {
		t.Fatal(err)
	}
	tr := monitor.New()
	mux := http.NewServeMux()
	registerSearchFace(mux, engine, st, "clus_sources", "", false,
		newNSEnsurer(engine, st, "", "clus_sources"), buckets, tr)
	for _, path := range []string{"/v1/search", "/v1/search/stream"} {
		for _, l1pre := range []bool{false, true} {
			w, out := serveJSON(t, mux, http.MethodPost, path, map[string]any{
				"query": "test", "ns": "tenant", "l1pre": l1pre,
			})
			if w.Code != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "CLUS_MINILM_REQUIRE") {
				t.Fatalf("stack failure: %d %v", w.Code, out)
			}
		}
	}
	s := tr.Snapshot(0, "")
	if s.Queries != 4 || s.Retrieval.Errors != 4 || s.Retrieval.ColdCount != 0 || s.Retrieval.Embedder != "" {
		t.Fatalf("initialization errors not tracked accurately: %+v", s)
	}
	if len(s.Namespaces) != 1 || s.Namespaces[0].Namespace != "tenant" || s.Namespaces[0].Queries != 4 {
		t.Fatalf("failure namespace lost: %+v", s.Namespaces)
	}
}

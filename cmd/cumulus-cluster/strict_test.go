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

// The corpus-vector seat must be the embedder the operator asked for. Asking
// for minilm without having the weights is an error, not a silent hash
// backfill: the backfill produces meaning-free vectors, and Rerank then uses
// them to scramble BM25 order at random — which is how serve ran for days on a
// degraded seat before anyone noticed (measured 2026-10-01). The opt-out is
// explicit (CLUS_EMBED=hash), so an accident can never look like a choice.
func TestEmbedderForFailsWhenMinilmWeightsAbsent(t *testing.T) {
	t.Setenv("CLUS_EMBED", "minilm")
	t.Setenv("CLUS_MINILM_DIR", t.TempDir()) // present dir, no weights
	t.Setenv("CLUS_MINILM_REQUIRE", "")
	// Keep the seat under test: an ambient CLUS_MODEL_DIR would shadow
	// CLUS_MINILM_DIR, and a configured remote embedder would win outright.
	t.Setenv("CLUS_MODEL_DIR", "")
	t.Setenv("LLM_EMBED_MODEL", "")

	if _, _, _, err := embedderFor(); err == nil || !strings.Contains(err.Error(), "weights absent") {
		t.Fatalf("an unhonored minilm request must fail: %v", err)
	}

	// The legacy flag is an alias now, not the switch: same failure either way.
	t.Setenv("CLUS_MINILM_REQUIRE", "1")
	if _, _, _, err := embedderFor(); err == nil || !strings.Contains(err.Error(), "weights absent") {
		t.Fatalf("CLUS_MINILM_REQUIRE=1 must still fail: %v", err)
	}

	// Nothing requested: hash-64 is the honest default, not a degradation.
	t.Setenv("CLUS_EMBED", "")
	if _, _, model, err := embedderFor(); err != nil || model != "local-hash-64" {
		t.Fatalf("no embed seat requested must stay inert: %s %v", model, err)
	}

	// The explicit opt-out keeps working.
	t.Setenv("CLUS_EMBED", "hash")
	fn, dims, model, err := embedderFor()
	if err != nil || fn == nil || model != "local-hash-64" || dims != 64 {
		t.Fatalf("CLUS_EMBED=hash must opt out cleanly: %s/%d %v", model, dims, err)
	}
}

func TestSearchStackFailureIsTracked(t *testing.T) {
	t.Setenv("CLUS_OFFLINE", "1")
	t.Setenv("CLUS_EMBED", "minilm")
	t.Setenv("CLUS_MODEL_DIR", t.TempDir())
	// No CLUS_MINILM_REQUIRE: failing is the default posture now, so this
	// exercises what an operator gets without knowing the legacy flag exists.
	t.Setenv("CLUS_MINILM_REQUIRE", "")
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
			if w.Code != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "weights absent") {
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

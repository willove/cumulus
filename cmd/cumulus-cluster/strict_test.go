package main

import (
	"context"
	"errors"
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
	// l1pre is refused at the request boundary (see TestSearchFaceRejectsL1Pre)
	// and never reaches the stack, so this stays on the plain path and counts
	// only the embedder-initialization failures.
	for _, path := range []string{"/v1/search", "/v1/search/stream"} {
		w, out := serveJSON(t, mux, http.MethodPost, path, map[string]any{
			"query": "test", "ns": "tenant",
		})
		if w.Code != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(out["error"]), "weights absent") {
			t.Fatalf("stack failure: %d %v", w.Code, out)
		}
	}
	s := tr.Snapshot(0, "")
	if s.Queries != 2 || s.Retrieval.Errors != 2 || s.Retrieval.ColdCount != 0 || s.Retrieval.Embedder != "" {
		t.Fatalf("initialization errors not tracked accurately: %+v", s)
	}
	if len(s.Namespaces) != 1 || s.Namespaces[0].Namespace != "tenant" || s.Namespaces[0].Queries != 2 {
		t.Fatalf("failure namespace lost: %+v", s.Namespaces)
	}
}

// `search -l1pre` / `{"l1pre":true}` used to be accepted and narrow nothing
// (see errSearchL1Pre); it is refused now, and the message names the arm that
// does exist. Rejection rather than rename, because the eval faces really do
// narrow — see the NOTE on narrowByKNN in evalrun.go.
func TestSearchFaceRejectsL1Pre(t *testing.T) {
	t.Setenv("CLUS_OFFLINE", "1")
	t.Setenv("CLUS_EMBED", "")
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	st := ingest.New(engine, "", "", "", "")
	buckets := bucket.New(engine)
	ctx := context.Background()
	if _, err := buckets.Create(ctx, "tenant", "", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerSearchFace(mux, engine, st, "clus_sources", "", false,
		newNSEnsurer(engine, st, "", "clus_sources"), buckets, monitor.New())

	for _, path := range []string{"/v1/search", "/v1/search/stream"} {
		w, out := serveJSON(t, mux, http.MethodPost, path, map[string]any{
			"query": "test", "ns": "tenant", "l1pre": true,
		})
		// 400, not 500: this is a caller mistake, and a 5xx would tell a
		// retrying client to keep retrying a request that can never succeed.
		if w.Code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "eval-run -l1pre") {
			t.Fatalf("l1pre must be refused as a client error on %s: %d %v", path, w.Code, out)
		}
	}

	// The same face must still serve the plain request — same store, same
	// query, the knob is the only difference. Probed: 200 without it, 400 with.
	if w, out := serveJSON(t, mux, http.MethodPost, "/v1/search", map[string]any{"query": "test", "ns": "tenant"}); w.Code != http.StatusOK {
		t.Fatalf("a request without l1pre must still be served: %d %v", w.Code, out)
	}

	// The choke point catches any caller that builds options directly (CLI and
	// MCP route through here too).
	if _, err := newSearchStackWith(ctx, engine, st, "clus_sources", SearchOptions{L1Pre: true}, newProdStack()); !errors.Is(err, errSearchL1Pre) {
		t.Fatalf("newSearchStackWith must return errSearchL1Pre, got %v", err)
	}
	if _, err := newSearchStackWith(ctx, engine, st, "clus_sources", SearchOptions{}, newProdStack()); errors.Is(err, errSearchL1Pre) {
		t.Fatal("the plain path must not hit the l1pre refusal")
	}
}

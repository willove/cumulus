package main

import (
	"strings"
	"testing"
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

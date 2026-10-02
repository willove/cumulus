package minilm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Required parses the legacy opt-in flag. It no longer changes behavior —
// Resolve fails on absent weights unconditionally — but gate harnesses still
// set it and /v1/config still reports it, so the parsing stays pinned.
func TestRequiredParses(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "": false, "0": false, "yes": false, " 1 ": true} {
		t.Setenv("CLUS_MINILM_REQUIRE", v)
		if got := Required(); got != want {
			t.Fatalf("CLUS_MINILM_REQUIRE=%q: %v, want %v", v, got, want)
		}
	}
}

// Absent weights are an error whether or not the legacy flag is set: an
// operator who asked for minilm asked for semantic vectors, and the silent
// hash-64 backfill is what let serve run for days reranking with meaning-free
// vectors. The opt-out is CLUS_EMBED=hash, which never reaches Resolve.
func TestResolveFailsWhenWeightsAbsent(t *testing.T) {
	present := t.TempDir()
	for _, f := range []string{"model.safetensors", "unigram.json"} {
		if err := os.WriteFile(filepath.Join(present, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	absent := t.TempDir()

	t.Setenv("CLUS_MINILM_DIR", present)
	if emb, err := Resolve(); err != nil || emb == nil {
		t.Fatalf("weights present: emb=%v err=%v", emb, err)
	}

	t.Setenv("CLUS_MINILM_DIR", absent)
	for _, required := range []string{"", "1"} {
		t.Setenv("CLUS_MINILM_REQUIRE", required)
		emb, err := Resolve()
		if err == nil || emb != nil {
			t.Fatalf("CLUS_MINILM_REQUIRE=%q: absent weights must fail hard, got emb=%v err=%v", required, emb, err)
		}
		if !strings.Contains(err.Error(), absent) {
			t.Fatalf("error must name the directory it looked in: %v", err)
		}
		if !strings.Contains(err.Error(), "CLUS_EMBED=hash") {
			t.Fatalf("error must name the opt-out: %v", err)
		}
	}
}

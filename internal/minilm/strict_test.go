package minilm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The strict gate: CLUS_MINILM_REQUIRE=1 turns "weights absent" from a
// silent hash fallback into an error, so CI precision paths cannot read
// green on a degraded embedder.
func TestRequiredParses(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "": false, "0": false, "yes": false, " 1 ": true} {
		t.Setenv("CLUS_MINILM_REQUIRE", v)
		if got := Required(); got != want {
			t.Fatalf("CLUS_MINILM_REQUIRE=%q: %v, want %v", v, got, want)
		}
	}
}

func TestResolveThreeCases(t *testing.T) {
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
	t.Setenv("CLUS_MINILM_REQUIRE", "")
	if emb, err := Resolve(); err != nil || emb != nil {
		t.Fatalf("absent + not required must degrade silently: emb=%v err=%v", emb, err)
	}

	t.Setenv("CLUS_MINILM_REQUIRE", "1")
	emb, err := Resolve()
	if err == nil || emb != nil {
		t.Fatalf("absent + required must fail hard: emb=%v err=%v", emb, err)
	}
	if !strings.Contains(err.Error(), "CLUS_MINILM_REQUIRE") || !strings.Contains(err.Error(), absent) {
		t.Fatalf("error must name the flag and the dir: %v", err)
	}
}

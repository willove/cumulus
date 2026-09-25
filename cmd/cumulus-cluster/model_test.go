package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmDownload(t *testing.T) {
	cases := map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, "": false, "n\n": false, "\n": false,
	}
	for in, want := range cases {
		var out bytes.Buffer
		if got := confirmDownload(strings.NewReader(in), &out, "download?"); got != want {
			t.Fatalf("confirmDownload(%q) = %v, want %v", in, got, want)
		}
		if !strings.Contains(out.String(), "download?") {
			t.Fatalf("prompt not shown: %q", out.String())
		}
	}
}

func TestVerifyModelWithoutWeights(t *testing.T) {
	if _, err := verifyModel(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "model install") {
		t.Fatalf("absent weights must point at the installer: %v", err)
	}
	// A directory with a bogus safetensors must surface as an error (not a panic).
	dir := t.TempDir()
	if err := writeFile(dir, "model.safetensors", "not-a-safetensors"); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyModel(context.Background(), dir); err == nil {
		t.Fatalf("corrupt weights must fail verify")
	}
}

func TestCollectModelStatus(t *testing.T) {
	dir := t.TempDir()
	st := collectModelStatus(dir)
	if st.Installed || st.Dims != 384 || len(st.Files) != 0 {
		t.Fatalf("empty dir status: %+v", st)
	}
	if err := writeFile(dir, "model.safetensors", "x"); err != nil {
		t.Fatal(err)
	}
	st = collectModelStatus(dir)
	if !st.Installed || len(st.Files) != 1 || st.Files[0].Size != 1 {
		t.Fatalf("status after drop: %+v", st)
	}
}

func writeFile(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

func TestConfigReportsEffectiveStack(t *testing.T) {
	// Configuration resolution is local only. Even the online cases below use
	// an inert endpoint and never invoke a model or install weights.
	t.Setenv("CLUS_MODEL_DIR", t.TempDir())
	t.Setenv("CLUS_VERBOSE", "")
	t.Setenv("AIGATE_API_KEY", "test-key-never-returned")
	for _, tc := range []struct {
		name, base, forcedOffline, split, embed, remoteEmbed, required string
		wantOffline, wantSplit                                         bool
		wantEmbed                                                      string
	}{
		{name: "no endpoint", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "auto minimax", base: "http://minimaxi.com.invalid", wantSplit: true, wantEmbed: "local-hash-64"},
		{name: "explicit disable", base: "http://minimaxi.com.invalid", split: "0", wantEmbed: "local-hash-64"},
		{name: "explicit false", base: "http://minimaxi.com.invalid", split: "false", wantEmbed: "local-hash-64"},
		{name: "explicit enable", base: "http://model.invalid", split: "1", remoteEmbed: "configured-model", wantSplit: true, wantEmbed: "aigate-64"},
		{name: "explicit true", base: "http://model.invalid", split: "TRUE", wantSplit: true, wantEmbed: "local-hash-64"},
		{name: "forced offline", base: "http://minimaxi.com.invalid", forcedOffline: "1", split: "1", remoteEmbed: "ignored", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "missing minilm fallback", embed: "minilm", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "missing minilm required", embed: "minilm", required: "1", wantOffline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIGATE_BASE_URL", tc.base)
			t.Setenv("CLUS_OFFLINE", tc.forcedOffline)
			t.Setenv("AIGATE_REASONING_SPLIT", tc.split)
			t.Setenv("CLUS_EMBED", tc.embed)
			t.Setenv("AIGATE_EMBED_MODEL", tc.remoteEmbed)
			t.Setenv("CLUS_MINILM_REQUIRE", tc.required)
			mux := http.NewServeMux()
			registerModelFace(mux)
			w, out := serveJSON(t, mux, http.MethodGet, "/v1/config", nil)
			if w.Code != http.StatusOK || out["offline"] != tc.wantOffline || out["reasoning_split"] != tc.wantSplit || out["effective_embedder"] != tc.wantEmbed {
				t.Fatalf("effective config: %d %v", w.Code, out)
			}
			if tc.required == "1" && !strings.Contains(out["embedder_error"].(string), "CLUS_MINILM_REQUIRE") {
				t.Fatalf("strict failure hidden: %v", out)
			}
			if out["base_url"] != tc.base || out["embed_model"] != tc.remoteEmbed || out["api_key_set"] != true || strings.Contains(w.Body.String(), "test-key-never-returned") {
				t.Fatalf("configured fields or key masking changed: %v", out)
			}
		})
	}
}

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/deep"
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
	t.Setenv("LLM_API_KEY", "test-key-never-returned")
	for _, tc := range []struct {
		name, base, forcedOffline, split, embed, remoteEmbed, required string
		wantOffline, wantSplit                                         bool
		wantEmbed                                                      string
	}{
		{name: "no endpoint", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "auto minimax", base: "http://minimaxi.com.invalid", wantSplit: true, wantEmbed: "local-hash-64"},
		{name: "explicit disable", base: "http://minimaxi.com.invalid", split: "0", wantEmbed: "local-hash-64"},
		{name: "explicit false", base: "http://minimaxi.com.invalid", split: "false", wantEmbed: "local-hash-64"},
		{name: "explicit enable", base: "http://model.invalid", split: "1", remoteEmbed: "configured-model", wantSplit: true, wantEmbed: "remote-64"},
		{name: "explicit true", base: "http://model.invalid", split: "TRUE", wantSplit: true, wantEmbed: "local-hash-64"},
		{name: "forced offline", base: "http://minimaxi.com.invalid", forcedOffline: "1", split: "1", remoteEmbed: "ignored", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "missing minilm fallback", embed: "minilm", wantOffline: true, wantEmbed: "local-hash-64"},
		{name: "missing minilm required", embed: "minilm", required: "1", wantOffline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LLM_BASE_URL", tc.base)
			t.Setenv("CLUS_OFFLINE", tc.forcedOffline)
			t.Setenv("LLM_REASONING_SPLIT", tc.split)
			t.Setenv("CLUS_EMBED", tc.embed)
			t.Setenv("LLM_EMBED_MODEL", tc.remoteEmbed)
			t.Setenv("CLUS_MINILM_REQUIRE", tc.required)
			engine, err := cumulite.Open("", cumulite.WithInMemory())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Close() })
			mux := http.NewServeMux()
			registerModelFace(mux, engine)
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

// The model-profile face: CRUD masking, activation materializing to .env, and
// the usage ledger round-trip. CLUS_ENV points the .env write at a temp file so
// the test never touches the developer's real config.
func TestModelProfileFaceAndUsageLedger(t *testing.T) {
	for _, key := range []string{"LLM_BASE_URL", "LLM_CHAT_MODEL", "LLM_EMBED_MODEL", "LLM_API_KEY", "LLM_REASONING_SPLIT", "LLM_BASE_URL", "LLM_MODEL_NAME", "LLM_API_KEY"} {
		t.Setenv(key, os.Getenv(key)) // register restore; materialize mutates these
	}
	envFile := filepath.Join(t.TempDir(), ".env")
	t.Setenv("CLUS_ENV", envFile)
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	mux := http.NewServeMux()
	registerModelFace(mux, engine)

	// 创建两个 profile；key 不得回显。
	w, out := serveJSON(t, mux, http.MethodPost, "/v1/models", map[string]any{
		"id": "mm", "label": "MiniMax", "base_url": "https://api.minimaxi.com/v1", "chat_model": "MiniMax-M3", "api_key": "sk-secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %v", w.Code, out)
	}
	if out["api_key_set"] != true || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("key must be masked: %v", out)
	}
	if w, _ := serveJSON(t, mux, http.MethodPost, "/v1/models", map[string]any{"id": "alt", "base_url": "http://model.invalid/v1", "chat_model": "other"}); w.Code != http.StatusOK {
		t.Fatal("second create failed")
	}
	w, out = serveJSON(t, mux, http.MethodGet, "/v1/models", nil)
	if w.Code != http.StatusOK || out["active"] != "" {
		t.Fatalf("list: %d %v", w.Code, out)
	}
	if profiles, _ := out["profiles"].([]any); len(profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(profiles))
	}

	// 激活：物化 .env + 热生效 + 指针落位。
	w, out = serveJSON(t, mux, http.MethodPost, "/v1/models/mm/activate", nil)
	if w.Code != http.StatusOK || out["hot_applied"] != true {
		t.Fatalf("activate: %d %v", w.Code, out)
	}
	if os.Getenv("LLM_CHAT_MODEL") != "MiniMax-M3" {
		t.Fatalf("activation not hot-applied: %q", os.Getenv("LLM_CHAT_MODEL"))
	}
	raw, rerr := os.ReadFile(envFile)
	if rerr != nil || !strings.Contains(string(raw), "LLM_BASE_URL=https://api.minimaxi.com/v1") {
		t.Fatalf("activation must materialize .env: %v %s", rerr, raw)
	}
	w, out = serveJSON(t, mux, http.MethodGet, "/v1/models", nil)
	if out["active"] != "mm" {
		t.Fatalf("active pointer: %v", out["active"])
	}
	// 激活中的 profile 不可删除。
	if w, _ := serveJSON(t, mux, http.MethodDelete, "/v1/models/mm", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("deleting the active profile must be refused, got %d", w.Code)
	}

	// 消费台账：一条记录写入后按模型过滤可读回。
	if err := engine.EnsureCollection(context.Background(), "clus_usage"); err != nil {
		t.Fatal(err)
	}
	recordConsumption(context.Background(), engine, "law", "MiniMax-M3", 100, 50, 150, deep.Result{Mode: "FAST", Tokens: 150}, 0.67)
	w, out = serveJSON(t, mux, http.MethodGet, "/v1/usage?model=MiniMax-M3", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("usage: %d %v", w.Code, out)
	}
	records, _ := out["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want 1", len(records))
	}
	rec, _ := records[0].(map[string]any)
	if rec["model"] != "MiniMax-M3" || rec["prompt_tokens"] != float64(100) || rec["completion_tokens"] != float64(50) {
		t.Fatalf("usage record fields: %v", rec)
	}
	// 按不存在的模型过滤 → 空列表而非报错。
	w, out = serveJSON(t, mux, http.MethodGet, "/v1/usage?model=nope", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("filtered usage failed: %d", w.Code)
	}
	if empty, _ := out["records"].([]any); len(empty) != 0 {
		t.Fatalf("filtered usage must be empty: %v", empty)
	}
}

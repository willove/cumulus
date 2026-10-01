package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	body := "# comment\n" +
		"LLM_BASE_URL=https://api.minimaxi.com/v1\n" +
		"export LLM_API_KEY=\"sk-quoted-123456\"\n" +
		"LLM_MODEL_NAME='MiniMax-M3'\n" +
		"BROKEN LINE WITHOUT EQUALS\n" +
		"\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUS_ENV", p)
	t.Setenv("LLM_API_KEY", "") // clear; loader must not see a pre-set value
	os.Unsetenv("LLM_API_KEY")
	if err := loadDotEnv(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LLM_BASE_URL"); got != "https://api.minimaxi.com/v1" {
		t.Fatalf("LLM_BASE_URL=%q", got)
	}
	if got := os.Getenv("LLM_API_KEY"); got != "sk-quoted-123456" {
		t.Fatalf("quotes must strip: %q", got)
	}
	if got := os.Getenv("LLM_MODEL_NAME"); got != "MiniMax-M3" {
		t.Fatalf("single quotes must strip: %q", got)
	}
}

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("LLM_BASE_URL=https://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLUS_ENV", p)
	t.Setenv("LLM_BASE_URL", "https://from-env")
	if err := loadDotEnv(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LLM_BASE_URL"); got != "https://from-env" {
		t.Fatalf("explicit env must win over .env: %q", got)
	}
}

func TestLoadDotEnvMissingOK(t *testing.T) {
	t.Setenv("CLUS_ENV", filepath.Join(t.TempDir(), "nope"))
	if err := loadDotEnv(); err != nil {
		t.Fatalf("missing .env must be a no-op: %v", err)
	}
}

func TestLoadDotEnvRejectsTraversal(t *testing.T) {
	for _, p := range []string{"../secrets.env", "a/../../etc/passwd", "../../.env"} {
		t.Setenv("CLUS_ENV", p)
		if err := loadDotEnv(); err == nil {
			t.Fatalf("traversal path %q must be rejected", p)
		}
	}
}

func TestCheckEnvPathAllowsAbsAndDevice(t *testing.T) {
	for _, p := range []string{"/dev/null", "/abs/operators.env", ".env", "conf/base.env"} {
		if err := checkEnvPath(p); err != nil {
			t.Fatalf("%q must stay allowed: %v", p, err)
		}
	}
}

func TestApplyAliases(t *testing.T) {
	// Canonical LLM_* keys stay untouched; legacy AIGATE_* / LLM_MODEL_NAME
	// only fill canonical keys that are unset.
	t.Setenv("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("AIGATE_CHAT_MODEL", "legacy-value")
	t.Setenv("LLM_MODEL_NAME", "operator-value")
	applyLLMAliases()
	if got := os.Getenv("LLM_BASE_URL"); got != "https://api.minimaxi.com/v1" {
		t.Fatalf("canonical key must stay: %q", got)
	}
	if got := os.Getenv("LLM_CHAT_MODEL"); got != "legacy-value" {
		t.Fatalf("legacy AIGATE_CHAT_MODEL must fill unset LLM_CHAT_MODEL: %q", got)
	}
	// Canonical beats legacy when both are present.
	t.Setenv("LLM_EMBED_MODEL", "canonical-embed")
	t.Setenv("AIGATE_EMBED_MODEL", "legacy-embed")
	applyLLMAliases()
	if got := os.Getenv("LLM_EMBED_MODEL"); got != "canonical-embed" {
		t.Fatalf("canonical LLM_EMBED_MODEL must win over AIGATE_*: %q", got)
	}
}

func TestOfflineForced(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " True "} {
		t.Setenv("CLUS_OFFLINE", v)
		if !offlineForced() {
			t.Fatalf("CLUS_OFFLINE=%q must pin offline", v)
		}
	}
	for _, v := range []string{"", "0", "false", "yes", "off"} {
		t.Setenv("CLUS_OFFLINE", v)
		if offlineForced() {
			t.Fatalf("CLUS_OFFLINE=%q must not pin offline", v)
		}
	}
}

// The regression this guards: a developer exports the documented operator
// convention (LLM_BASE_URL/LLM_MODEL_NAME) and runs the offline gates. Before
// CLUS_OFFLINE existed, applyLLMAliases promoted those into the engine's
// endpoint variables and every search hit the live endpoint — 153 assertions
// collapsed to 72 ok / 81 fail.
func TestOfflinePinBeatsConfiguredEndpoint(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("LLM_API_KEY", "sk-should-never-be-sent")
	t.Setenv("LLM_MODEL_NAME", "MiniMax-M3")
	applyLLMAliases()
	if os.Getenv("LLM_BASE_URL") == "" {
		t.Fatal("precondition: alias fill must keep the endpoint configured")
	}
	t.Setenv("CLUS_OFFLINE", "1")
	ps := newProdStack()
	if ps.chat != nil {
		t.Fatal("CLUS_OFFLINE=1 must leave the chat client nil even with LLM_BASE_URL set")
	}
	if ps.embErr != nil {
		t.Fatalf("offline stack must build clean: %v", ps.embErr)
	}
	// The offline stub seats stay wired, so the gate path is unchanged.
	if ps.scorer == nil || ps.emb == nil {
		t.Fatal("offline stubs (scorer/embedder) must remain wired")
	}
}

// Without the pin, a configured endpoint still wires the live client — the
// opt-in must not silently disable production.
func TestEndpointStillWiresWithoutOfflinePin(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("CLUS_OFFLINE", "")
	if ps := newProdStack(); ps.chat == nil {
		t.Fatal("without CLUS_OFFLINE a configured endpoint must wire the chat client")
	}
}

// Configuration written from the workbench must survive as a normal .env: the
// operator's comments and ordering are theirs, unknown keys append, and a device
// path is refused outright (offline gates run with CLUS_ENV=/dev/null and a save
// must not turn that into a regular file).
func TestWriteEnvValuesPreservesTheOperatorFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	original := "# 端点配置（注释必须保留）\nLLM_BASE_URL=https://old.example/v1\n\n# 下面是模型\nLLM_MODEL_NAME=old-model\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvValues(path, map[string]string{"LLM_MODEL_NAME": "new-model", "LLM_EMBED_MODEL": "emb-1"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"# 端点配置（注释必须保留）", "# 下面是模型", "LLM_BASE_URL=https://old.example/v1", "LLM_MODEL_NAME=new-model", "LLM_EMBED_MODEL=emb-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old-model") {
		t.Fatalf("old value survived:\n%s", got)
	}
	// 权限：这份文件装密钥
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file must stay 0600: %v %v", info.Mode(), err)
	}
	// 设备路径必须被拒绝，而不是被写成普通文件
	if err := writeEnvValues("/dev/null", map[string]string{"LLM_MODEL_NAME": "x"}); err == nil {
		t.Fatal("writing configuration to a device must be refused")
	}
	// 注意用字面量的 ..：filepath.Join 会先把 .. 规整掉，那样测不到守卫。
	if err := writeEnvValues("../"+filepath.Base(dir)+"/.env", nil); err == nil {
		t.Fatal("traversal must be refused")
	}
}

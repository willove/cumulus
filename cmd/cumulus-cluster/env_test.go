package main

import (
	"os"
	"path/filepath"
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

func TestApplyLLMAliases(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("LLM_MODEL_NAME", "MiniMax-M3")
	t.Setenv("AIGATE_CHAT_MODEL", "already-set")
	applyLLMAliases()
	if got := os.Getenv("AIGATE_BASE_URL"); got != "https://api.minimaxi.com/v1" {
		t.Fatalf("alias not applied: %q", got)
	}
	if got := os.Getenv("AIGATE_CHAT_MODEL"); got != "already-set" {
		t.Fatalf("AIGATE_* must win over LLM_*: %q", got)
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
// CLUS_OFFLINE existed, applyLLMAliases promoted those into AIGATE_* and every
// search hit the live endpoint — 153 assertions collapsed to 72 ok / 81 fail.
func TestOfflinePinBeatsConfiguredEndpoint(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("LLM_API_KEY", "sk-should-never-be-sent")
	t.Setenv("LLM_MODEL_NAME", "MiniMax-M3")
	applyLLMAliases()
	if os.Getenv("AIGATE_BASE_URL") == "" {
		t.Fatal("precondition: aliases must promote LLM_* to AIGATE_*")
	}
	t.Setenv("CLUS_OFFLINE", "1")
	ps := newProdStack()
	if ps.chat != nil {
		t.Fatal("CLUS_OFFLINE=1 must leave the chat client nil even with AIGATE_BASE_URL set")
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
	t.Setenv("AIGATE_BASE_URL", "https://api.minimaxi.com/v1")
	t.Setenv("AIGATE_API_KEY", "sk-test")
	t.Setenv("CLUS_OFFLINE", "")
	if ps := newProdStack(); ps.chat == nil {
		t.Fatal("without CLUS_OFFLINE a configured endpoint must wire the chat client")
	}
}

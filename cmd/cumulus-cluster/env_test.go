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

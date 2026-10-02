package envcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Coverage note: Load / CheckPath / FilePath / OfflineForced / ApplyAliases are
// already exercised through cmd/cumulus-cluster/env_test.go's ten tests, which
// reach them via the thin wrappers (loadDotEnv, checkEnvPath, applyLLMAliases…).
// Those wrappers are how the suite calls this package, so that coverage counts.
// What it does NOT cover is Resolve — the composed two-step the standalone
// probes (cmd/scoreprobe, cmd/agreeprobe) call instead — and the precedence
// that only shows up once Load and ApplyAliases run together. This file pins
// exactly that, and nothing else.

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Resolve is the single entry point the probes share with the search stack.
// A probe that resolved differently would publish a reliability number that is
// not a number about this suite — which is the only reason this package exists.
func TestResolveComposesLoadThenAliases(t *testing.T) {
	t.Setenv("CLUS_ENV", writeEnv(t, strings.Join([]string{
		"# comment is skipped",
		"",
		"LLM_BASE_URL=https://env.example/v1",
		`LLM_API_KEY="quoted-key"`,
		"export LLM_CHAT_MODEL=env-model",
		"AIGATE_EMBED_MODEL=legacy-embed",
		"garbage-line-without-equals",
	}, "\n")))
	for _, k := range []string{"LLM_BASE_URL", "LLM_API_KEY", "LLM_CHAT_MODEL", "LLM_EMBED_MODEL", "AIGATE_EMBED_MODEL"} {
		t.Setenv(k, "")
	}

	if err := Resolve(); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"LLM_BASE_URL":    "https://env.example/v1",
		"LLM_API_KEY":     "quoted-key", // quotes stripped
		"LLM_CHAT_MODEL":  "env-model",  // `export ` prefix stripped
		"LLM_EMBED_MODEL": "legacy-embed",
	} {
		if got := os.Getenv(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// The precedence that only exists once both steps run: an .env canonical value
// beats an AMBIENT legacy name, because Load fills the canonical key first and
// ApplyAliases then refuses to overwrite a set canonical.
//
// Worth naming explicitly: the package doc lists the order as
// "env already set > .env > legacy alias", which is ambiguous for exactly this
// case — the ambient value IS "already set", but only under its legacy name.
// This test pins what the code actually does; it does not claim the choice was
// deliberate.
func TestEnvFileCanonicalBeatsAmbientLegacyName(t *testing.T) {
	t.Setenv("CLUS_ENV", writeEnv(t, "LLM_BASE_URL=https://from-env-file/v1\n"))
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("AIGATE_BASE_URL", "https://from-ambient-legacy/v1")

	if err := Resolve(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LLM_BASE_URL"); got != "https://from-env-file/v1" {
		t.Fatalf("an .env canonical key must win over an ambient legacy alias, got %q", got)
	}
}

// The other side of the same seam: with no canonical value anywhere, an ambient
// legacy name must still reach the canonical key — that is what keeps an old
// operator shell working after the gateway rename.
func TestAmbientLegacyFillsUnsetCanonical(t *testing.T) {
	t.Setenv("CLUS_ENV", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("AIGATE_BASE_URL", "https://legacy-only/v1")

	if err := Resolve(); err != nil {
		t.Fatalf("a missing .env must not be an error: %v", err)
	}
	if got := os.Getenv("LLM_BASE_URL"); got != "https://legacy-only/v1" {
		t.Fatalf("legacy name must fill an unset canonical key, got %q", got)
	}
}

// A mispointed CLUS_ENV must not pull an arbitrary file into memory whole.
// Documenting the consequence, not just the cap: past 1 MiB the tail is
// dropped SILENTLY — Resolve returns no error, so a truncated config reads as
// a complete one. If that ever needs to be loud, this is the seam.
func TestOversizedEnvFileIsTruncatedNotLoaded(t *testing.T) {
	pad := strings.Repeat("x", 1<<10)
	var b strings.Builder
	for i := 0; i < 2048; i++ { // > maxEnvBytes (1 MiB) of filler lines
		b.WriteString("# " + pad + "\n")
	}
	b.WriteString("LLM_CHAT_MODEL=past-the-cap\n")
	t.Setenv("CLUS_ENV", writeEnv(t, b.String()))
	t.Setenv("LLM_CHAT_MODEL", "")

	if err := Resolve(); err != nil {
		t.Fatalf("truncation is not an error today: %v", err)
	}
	if got := os.Getenv("LLM_CHAT_MODEL"); got == "past-the-cap" {
		t.Fatal("a value past maxEnvBytes must NOT load — the cap has no observable effect otherwise")
	}
}

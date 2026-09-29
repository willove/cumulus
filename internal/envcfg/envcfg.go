// Package envcfg resolves the suite's endpoint configuration the same way for
// every face that talks to a live model.
//
// It was extracted from cmd/cumulus-cluster so that a probe (cmd/scoreprobe)
// measures the SAME endpoint, model and key the search stack uses, rather than
// a differently-configured second code path. A reliability number gathered
// against a different model is not a number about this suite.
//
// Resolution order (explicit beats implicit):
//  1. environment variables already set
//  2. KEY=VALUE lines from $CLUS_ENV (default ./.env)
//  3. the LLM_* operator convention aliased onto AIGATE_*
package envcfg

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxEnvBytes caps the read so a mispointed CLUS_ENV cannot pull an arbitrary
// file into memory at full size.
const maxEnvBytes = 1 << 20

// aliasPairs maps the operator's LLM_* convention onto the suite's AIGATE_*
// variables. AIGATE_* wins when both are present.
var aliasPairs = [][2]string{
	{"LLM_BASE_URL", "AIGATE_BASE_URL"},
	{"LLM_API_KEY", "AIGATE_API_KEY"},
	{"LLM_MODEL_NAME", "AIGATE_CHAT_MODEL"},
}

// CheckPath rejects traversal elements in an operator-supplied env-file path.
// Absolute paths and devices such as /dev/null stay allowed (offline gates set
// CLUS_ENV=/dev/null). The file is the operator's own local config on their own
// machine: the only content ever interpreted is KEY=VALUE lines, so the worst
// case is a wrong config value, never code execution.
func CheckPath(p string) error {
	for _, part := range strings.Split(filepath.Clean(p), string(os.PathSeparator)) {
		if part == ".." {
			return fmt.Errorf("env file path must not contain ..: %s", p)
		}
	}
	return nil
}

// FilePath resolves the suite .env location: $CLUS_ENV or ./.env.
func FilePath() string {
	if p := os.Getenv("CLUS_ENV"); p != "" {
		return p
	}
	return ".env"
}

// Load reads KEY=VALUE lines from the suite .env into the process environment
// without overriding anything already set. A missing file is not an error —
// offline gates run with no endpoint config at all.
func Load() error {
	p := FilePath()
	if err := CheckPath(p); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", p, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxEnvBytes))
	if err != nil {
		return fmt.Errorf("reading %s: %w", p, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" || os.Getenv(k) != "" {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return nil
}

// OfflineForced reports whether this process is pinned to the offline stubs
// (CLUS_OFFLINE=1). Gate harnesses set it so an ambient LLM_BASE_URL / AIGATE_*
// in the developer's shell — the documented operator convention — cannot route
// deterministic gates at a live endpoint: each search would spend real tokens
// and the assertions would flake (D6: mechanism gates must be reproducible).
// It only ever removes collaborators; it never invents an endpoint.
func OfflineForced() bool {
	v := strings.TrimSpace(os.Getenv("CLUS_OFFLINE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// ApplyLLMAliases maps the operator's LLM_* convention onto AIGATE_*.
func ApplyLLMAliases() {
	for _, p := range aliasPairs {
		if os.Getenv(p[1]) == "" && os.Getenv(p[0]) != "" {
			_ = os.Setenv(p[1], os.Getenv(p[0]))
		}
	}
}

// Resolve is the standard two-step every face should call before reading
// AIGATE_* : Load the suite .env, then apply the LLM_* aliases.
func Resolve() error {
	if err := Load(); err != nil {
		return err
	}
	ApplyLLMAliases()
	return nil
}

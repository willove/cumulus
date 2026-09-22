// Per-suite endpoint configuration: the suite reads its own .env (same
// LLM_* convention as the operator's other tools) and maps it onto the
// AIGATE_* variables the CLI consumes. Environment variables already set
// always win over the file — explicit beats implicit.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// checkEnvPath rejects traversal elements in an operator-supplied env-file
// path. Absolute paths and devices such as /dev/null stay allowed (offline
// gates set ASK_ENV=/dev/null). The file is the operator's own local config
// on their own machine: the only content ever interpreted is KEY=VALUE lines,
// so the worst case is a wrong config value, never code execution.
func checkEnvPath(p string) error {
	for _, part := range strings.Split(filepath.Clean(p), string(os.PathSeparator)) {
		if part == ".." {
			return fmt.Errorf("env file path must not contain ..: %s", p)
		}
	}
	return nil
}

// envFilePath resolves the suite .env location: $ASK_ENV or ./.env.
func envFilePath() string {
	if p := os.Getenv("ASK_ENV"); p != "" {
		return p
	}
	return ".env"
}

// loadDotEnv reads KEY=VALUE lines from the suite .env selected by ASK_ENV
// (default ./.env) into the process environment without overriding anything
// that is already set. Missing file is not an error (offline gates run with
// no endpoint config at all).
func loadDotEnv() error {
	p := envFilePath()
	if err := checkEnvPath(p); err != nil {
		return err
	}
	// Reads are capped so a mispointed variable cannot pull in an arbitrary
	// file at full size.
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", p, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
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

// applyLLMAliases maps the operator's LLM_* convention onto the suite's
// AIGATE_* variables (AIGATE_* wins if both are present).
func applyLLMAliases() {
	pairs := [][2]string{
		{"LLM_BASE_URL", "AIGATE_BASE_URL"},
		{"LLM_API_KEY", "AIGATE_API_KEY"},
		{"LLM_MODEL_NAME", "AIGATE_CHAT_MODEL"},
	}
	for _, p := range pairs {
		if os.Getenv(p[1]) == "" && os.Getenv(p[0]) != "" {
			_ = os.Setenv(p[1], os.Getenv(p[0]))
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

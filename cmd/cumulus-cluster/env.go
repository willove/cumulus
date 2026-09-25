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
	"sort"
	"strings"
)

// checkEnvPath rejects traversal elements in an operator-supplied env-file
// path. Absolute paths and devices such as /dev/null stay allowed (offline
// gates set CLUS_ENV=/dev/null). The file is the operator's own local config
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

// envFilePath resolves the suite .env location: $CLUS_ENV or ./.env.
func envFilePath() string {
	if p := os.Getenv("CLUS_ENV"); p != "" {
		return p
	}
	return ".env"
}

// loadDotEnv reads KEY=VALUE lines from the suite .env selected by CLUS_ENV
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

// offlineForced reports whether this process is pinned to the offline stubs
// (CLUS_OFFLINE=1). Gate harnesses set it so an ambient LLM_BASE_URL / AIGATE_*
// in the developer's shell — the documented operator convention — cannot route
// deterministic gates at a live endpoint: each search would spend real tokens
// and the assertions would flake (D6: mechanism gates must be reproducible).
// It only ever removes collaborators; it never invents an endpoint.
func offlineForced() bool {
	v := strings.TrimSpace(os.Getenv("CLUS_OFFLINE"))
	return v == "1" || strings.EqualFold(v, "true")
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

// writeEnvValues updates KEY=VALUE lines in a suite .env, preserving comments,
// blank lines and the original order; keys that are absent are appended. The same
// skeleton Sirchmunk uses (settings.py _update_env_file), with one deliberate
// difference: this writer refuses a non-regular file, because offline gates run
// with CLUS_ENV=/dev/null and a save must never turn that device into a config
// file — or worse, silently rewrite whatever the variable points at.
func writeEnvValues(path string, values map[string]string) error {
	if err := checkEnvPath(path); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; refusing to write configuration there", path)
	}
	var lines []string
	if raw, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	// Iterate the file once, remembering which keys were replaced, so an existing
	// key keeps its position and its inline comment shape.
	written := map[string]bool{}
	for i, line := range lines {
		for key, value := range values {
			if written[key] {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, key+"=") {
				continue
			}
			lines[i] = key + "=" + value
			written[key] = true
		}
	}
	// Deterministic append order keeps the diff readable across saves.
	appended := make([]string, 0, len(values))
	for key := range values {
		if !written[key] {
			appended = append(appended, key)
		}
	}
	sort.Strings(appended)
	for _, key := range appended {
		lines = append(lines, key+"="+values[key])
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	// This file holds API keys: force 0600 even when it already existed, because
	// os.WriteFile's perm only applies at creation and an operator's 0644 .env
	// would otherwise keep a freshly written secret world-readable.
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

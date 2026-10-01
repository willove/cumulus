// Per-suite endpoint configuration: the suite reads its own .env (LLM_*
// convention, same as the operator's other tools) and aliases legacy names
// (AIGATE_* from the retired gateway plan, LLM_MODEL_NAME) onto the
// canonical LLM_* keys. Environment variables already set always win over
// the file — explicit beats implicit.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/envcfg"
)

// checkEnvPath rejects traversal elements in an operator-supplied env-file
// path. Absolute paths and devices such as /dev/null stay allowed (offline
// gates set CLUS_ENV=/dev/null). The file is the operator's own local config
// on their own machine: the only content ever interpreted is KEY=VALUE lines,
// so the worst case is a wrong config value, never code execution.
//
// The implementation is shared with cmd/scoreprobe via internal/envcfg, so a
// probe measures the same endpoint the search stack uses.
func checkEnvPath(p string) error { return envcfg.CheckPath(p) }

// envFilePath resolves the suite .env location: $CLUS_ENV or ./.env.
func envFilePath() string { return envcfg.FilePath() }

// envFlag reads an opt-in boolean. Absent, empty, "0" and "false" are off;
// "1" and "true" are on, case-insensitively. Anything else is off, so a typo
// degrades to the cheaper/default path instead of silently enabling a paying
// or behaviour-changing mechanism.
func envFlag(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true")
}

// loadDotEnv reads KEY=VALUE lines from the suite .env selected by CLUS_ENV
// (default ./.env) into the process environment without overriding anything
// that is already set. Missing file is not an error (offline gates run with
// no endpoint config at all). Implementation: internal/envcfg, shared with
// cmd/scoreprobe so both faces resolve the endpoint identically.
func loadDotEnv() error { return envcfg.Load() }

// offlineForced reports whether this process is pinned to the offline stubs
// (CLUS_OFFLINE=1). Gate harnesses set it so an ambient LLM_* endpoint config
// in the developer's shell — the documented operator convention — cannot route
// deterministic gates at a live endpoint: each search would spend real tokens
// and the assertions would flake (D6: mechanism gates must be reproducible).
// It only ever removes collaborators; it never invents an endpoint.
func offlineForced() bool { return envcfg.OfflineForced() }

// applyLLMAliases maps legacy names (AIGATE_* from the retired gateway plan,
// LLM_MODEL_NAME) onto the canonical LLM_* keys, filling only unset ones.
func applyLLMAliases() { envcfg.ApplyAliases() }

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

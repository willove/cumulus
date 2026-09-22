// Per-suite endpoint configuration: the suite reads its own .env (same
// LLM_* convention as the operator's other tools) and maps it onto the
// AIGATE_* variables the CLI consumes. Environment variables already set
// always win over the file — explicit beats implicit.
package main

import (
	"fmt"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE lines into the process environment without
// overriding anything that is already set. Missing file is not an error
// (offline gates run with no endpoint config at all).
func loadDotEnv(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
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

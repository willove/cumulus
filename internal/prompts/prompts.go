// Package prompts is the SSOT for LLM prompt assets (S5 plan §6.3 / R1).
// Bodies live in sibling .md files so wording can be reviewed and frozen
// without touching Go; templates inject {{param}} placeholders at runtime.
package prompts

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed *.md
var fs embed.FS

// Asset names (file stems).
const (
	EvaluateSample     = "evaluate_sample"
	FastAnalyze        = "fast_analyze"
	KeywordsMultilevel = "keywords_multilevel"
	HistoryRewrite     = "history_rewrite"
	SynthesizeROI      = "synthesize_roi"
	JudgeCorrect       = "judge_correct"
)

// Load returns the raw markdown body of a prompt asset.
func Load(name string) (string, error) {
	b, err := fs.ReadFile(name + ".md")
	if err != nil {
		return "", fmt.Errorf("prompts: load %s: %w", name, err)
	}
	return string(b), nil
}

// Render replaces {{key}} placeholders. Unknown keys are left intact so a
// frozen regression can detect missing injections rather than silent blanks.
func Render(tmpl string, vars map[string]string) string {
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// MustRender is Render for known-good templates in tests/CLI.
func MustRender(name string, vars map[string]string) string {
	t, err := Load(name)
	if err != nil {
		panic(err)
	}
	return Render(t, vars)
}

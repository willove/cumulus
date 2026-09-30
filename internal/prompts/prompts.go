// Package prompts holds the LLM prompt assets.
// Bodies live in sibling .md files so wording can be reviewed and frozen
// without touching Go; templates inject {{param}} placeholders at runtime.
package prompts

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed *.md
var fs embed.FS

// Asset names (file stems).
const (
	EvaluateSample     = "evaluate_sample"
	EvidenceAgree      = "evidence_agree"
	Paraphrase         = "paraphrase"
	ExpandTerms        = "expand_terms"
	SelectTerms        = "select_terms"
	LearnHypothesis    = "learn_hypothesis"
	FastAnalyze        = "fast_analyze"
	KeywordsMultilevel = "keywords_multilevel"
	HistoryRewrite     = "history_rewrite"
	SynthesizeROI      = "synthesize_roi"
	JudgeCorrect       = "judge_correct"
	KeywordsRefine     = "keywords_refine"
	QueryAbstract      = "query_abstract"
	QueryFromAbstract  = "query_from_abstract"
	ScanRank           = "scan_rank"
	ClosedBook         = "closed_book"
	JudgeAnswer        = "judge_answer"
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
//
// Keys are substituted in sorted order, not map order: if a value itself
// contains "{{other}}", map iteration order (randomized in Go) decided whether
// the nested placeholder got expanded — the same inputs could produce different
// prompts, which defeats the frozen-regression contract.
func Render(tmpl string, vars map[string]string) string {
	out := tmpl
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = strings.ReplaceAll(out, "{{"+k+"}}", vars[k])
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

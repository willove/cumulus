package prompts

import (
	"sort"
	"strings"
	"testing"
)

// Render leaves an unknown {{key}} intact, on purpose, so a frozen regression
// can notice a missing injection instead of getting a silent blank. That is
// only useful if something CHECKS for residue — and nothing did. The failure
// mode is quiet: rename a variable at a call site, the prompt still renders,
// and the model receives a literal "{{query}}" as if it were the question.
//
// This file pins the property that closes it: every placeholder in every
// asset must be injectable, and a full injection of the asset's own key set
// must leave no residue. Adding a placeholder to an asset without teaching a
// call site to fill it now fails here instead of in production.

// assetVars is the injection contract per asset. It is the seam: when a call
// site starts passing a new key, add it here; when an asset gains a
// placeholder, filling it here is what makes this test pass.
var assetVars = map[string][]string{
	"closed_book":         {"query"},
	"evaluate_sample":     {"facts", "query", "sample_content", "sample_source"},
	"evidence_agree":      {"count", "query", "windows"},
	"expand_terms":        {"query"},
	"fast_analyze":        {"query"},
	"history_rewrite":     {"history", "query"},
	"judge_answer":        {"answer", "query"},
	"judge_correct":       {"answer", "query", "reference"},
	"keywords_multilevel": {"levels", "query"},
	"keywords_refine":     {"domain", "failed", "query"},
	"learn_hypothesis":    {"anomalies", "knobs"},
	"paraphrase":          {"query"},
	"query_abstract":      {"query"},
	"query_from_abstract": {"domain", "need", "tried"},
	"rewrite_query":       {"query"},
	"scan_rank":           {"candidates", "query"},
	"select_terms":        {"query", "terms"},
	"synthesize_roi":      {"evidences", "query"},
}

func placeholders(tmpl string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range strings.Split(tmpl, "{{") {
		i := strings.Index(m, "}}")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(m[:i])
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// assetNames lists every embedded .md asset, so a NEW asset is covered here
// the day it lands rather than the day someone remembers to add it.
func assetNames(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(".")
	if err != nil {
		t.Fatalf("read prompt dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".md") {
			out = append(out, strings.TrimSuffix(n, ".md"))
		}
	}
	if len(out) == 0 {
		t.Fatal("no prompt assets found — the embed is broken, not the test")
	}
	sort.Strings(out)
	return out
}

// TestEveryAssetPlaceholderIsDeclaredInTheContract: an asset's placeholders
// and the declared injection contract must agree exactly. A placeholder with
// no declared key is a placeholder no call site is expected to fill.
func TestEveryAssetPlaceholderIsDeclaredInTheContract(t *testing.T) {
	for _, name := range assetNames(t) {
		tmpl, err := Load(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		got := placeholders(tmpl)
		declared := append([]string(nil), assetVars[name]...)
		sort.Strings(declared)

		if len(got) == 0 {
			if len(declared) > 0 {
				t.Errorf("%s declares vars %v but its template has no placeholders", name, declared)
			}
			continue
		}
		if strings.Join(got, ",") != strings.Join(declared, ",") {
			t.Errorf("%s\n  template placeholders: %v\n  declared contract:   %v\n"+
				"  → every template key must be declared here, and every declared key must appear",
				name, got, declared)
		}
	}
}

// TestFullInjectionLeavesNoResidue is the property the whole mechanism rests
// on: filling the contract must actually consume the placeholders. A leftover
// "{{...}}" here means a call site that renders this asset without the full
// key set will ship a literal placeholder to the model.
func TestFullInjectionLeavesNoResidue(t *testing.T) {
	for _, name := range assetNames(t) {
		tmpl, err := Load(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		vars := map[string]string{}
		for _, k := range assetVars[name] {
			vars[k] = "«" + k + "»"
		}
		out := Render(tmpl, vars)
		if left := placeholders(out); len(left) > 0 {
			t.Errorf("%s still contains %v after a full injection — a call site rendering it "+
				"with a partial key set would send literals to the model", name, left)
		}
	}
}

// TestDeclaredContractCoversEveryAsset keeps the seam honest in the other
// direction: an entry for an asset that does not exist, or a typo in a name,
// is a silently-dead contract.
func TestDeclaredContractCoversEveryAsset(t *testing.T) {
	assets := map[string]bool{}
	for _, n := range assetNames(t) {
		assets[n] = true
	}
	for name := range assetVars {
		if !assets[name] {
			t.Errorf("assetVars declares %q but internal/prompts has no such asset", name)
		}
	}
}

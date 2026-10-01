package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/fast"
)

func TestPredictionOfCitations(t *testing.T) {
	res := deep.Result{
		Answer: fast.Answer{
			Query:    "q",
			Summary:  "答",
			SourceID: "src:law000",
		},
		Citations: deep.CitationSet{Refs: []deep.Ref{
			{Index: 1, SourceID: "src:law000", Resolved: true},
			{Index: 2, SourceID: "src:neg001", Resolved: false},
			{Index: 3, SourceID: "src:law000", Resolved: true}, // dup must dedupe
		}},
	}
	keys := map[string]string{"src:law000": "law000", "src:neg001": "neg001"}
	p := predictionOf(res, keys)
	// Business keys must ride along so gold_sources (keys) can match.
	want := map[string]bool{"src:law000": true, "law000": true, "src:neg001": true, "neg001": true}
	if len(p.SourceIDs) != len(want) {
		t.Fatalf("source ids: %v", p.SourceIDs)
	}
	for _, id := range p.SourceIDs {
		if !want[id] {
			t.Fatalf("unexpected id %q in %v", id, p.SourceIDs)
		}
	}
	if p.Refs != 3 || p.Resolved != 2 {
		t.Fatalf("refs=%d resolved=%d", p.Refs, p.Resolved)
	}
	if p.Answer != "答" {
		t.Fatalf("answer=%q", p.Answer)
	}
}

// The aggregate's judged flag must come from the stored rows, not from the
// invocation's -judge flag: realeval.sh's report stage is resume-only
// (-limit 0 recomputes nothing), and it once aggregated judge-flipped rows
// under a no-judge invocation — the scorecard read judged:false while the
// judge had contributed 12 of the 13 correct answers.
func TestRowsJudgedFromStoredRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []evalResult
		want bool
	}{
		{"no rows", nil, false},
		{"rule-only run", []evalResult{{ID: "a"}, {ID: "b", Eval: eval.ItemScore{Correct: true}}}, false},
		{"system-arm judge present", []evalResult{{ID: "a", Judge: "10 完全正确"}}, true},
		{"closed-book judge only", []evalResult{{ID: "a", CBJudge: "8 基本正确"}}, true},
		{"empty judge string is not a verdict", []evalResult{{ID: "a", Judge: ""}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowsJudged(tc.rows); got != tc.want {
				t.Fatalf("rowsJudged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAggregateResultsReport(t *testing.T) {
	lines := []evalResult{
		{ID: "a", Mode: "FAST", Eval: eval.ItemScore{Correct: true, Answered: true}, CB: eval.ItemScore{}},
		{ID: "b", Mode: "DEEP", Eval: eval.ItemScore{EvRec: true, Answered: true}, CB: eval.ItemScore{Correct: true}},
		{ID: "c", Mode: "FAST", Eval: eval.ItemScore{}, CB: eval.ItemScore{}},
	}
	rep := aggregateResults(lines, true, 1)
	if rep.N != 3 || rep.Resumed != 1 || !rep.Judged {
		t.Fatalf("rep=%+v", rep)
	}
	tx := rep.System.Taxonomy
	if tx.Correct != 1 || tx.RetrievedOnly != 1 || tx.NotRetrieved != 1 || tx.AnsweredWrong != 0 {
		t.Fatalf("taxonomy=%+v", tx)
	}
	if rep.Modes["FAST"] != 2 || rep.Modes["DEEP"] != 1 {
		t.Fatalf("modes=%v", rep.Modes)
	}
	if rep.McNemar.N != 3 {
		t.Fatalf("mcnemar=%+v", rep.McNemar)
	}
}

func TestReadEvalItems(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "items.jsonl")
	body := "{\"id\":\"q1\",\"query\":\"造谣处罚\",\"answer\":\"治安管理处罚法条文\",\"gold_sources\":[\"law000\"]}\n" +
		"\n{\"id\":\"q2\",\"query\":\"x\",\"answer\":\"y\"}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := readEvalItems(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "q1" || len(items[0].Gold) != 1 {
		t.Fatalf("items=%+v", items)
	}
	if _, err := readEvalItems(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestReadDoneIDsResume(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "results.jsonl")
	// Missing file → empty, no error.
	done, err := readDoneIDs(filepath.Join(dir, "nope.jsonl"))
	if err != nil || len(done) != 0 {
		t.Fatalf("done=%v err=%v", done, err)
	}
	body := "{\"id\":\"q1\",\"eval\":{\"correct\":true}}\n{\"id\":\"q2\",\"eval\":{}}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	done, err = readDoneIDs(p)
	if err != nil {
		t.Fatal(err)
	}
	if !done["q1"] || !done["q2"] || len(done) != 2 {
		t.Fatalf("done=%v", done)
	}
}

func TestJudgePassThreshold(t *testing.T) {
	if !strings.Contains(judgeCorrectDoc(), "score") {
		t.Fatal("judge asset must carry the score contract")
	}
}

// judgeCorrectDoc guards that the judge prompt asset stays loadable from the
// CLI side (rendering contract is frozen in the prompts package tests).
func judgeCorrectDoc() string {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "prompts", "judge_correct.md"))
	if err != nil {
		return ""
	}
	return string(b)
}

// The A.6 config binding must name every knob that moves a number. It used to
// carry only prior/l1pre/judge/ns, so two ablations on DIFFERENT models
// produced byte-identical ConfigSHA — the scoreboard showed them as the same
// configuration and a model swap looked like a no-op.
func TestEvalConfigFingerprintCoversModelAndBudgets(t *testing.T) {
	st := prodStack{} // offline stubs
	base := evalConfig(st, false, false, false, "ev")

	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"chat model", func() { t.Setenv("LLM_CHAT_MODEL", "model-B") }, "model-B"},
		{"embed model", func() { t.Setenv("LLM_EMBED_MODEL", "text-embedding-3-small") }, "text-embedding-3-small"},
		{"minilm seat", func() { t.Setenv("CLUS_EMBED", "minilm") }, "embed_seat=minilm"},
		{"abstain on", func() { t.Setenv("CLUS_ABSTAIN", "1") }, "abstain=1"},
		{"query sim on", func() { t.Setenv("CLUS_QUERY_SIM", "1") }, "query_sim=1"},
		{"token budget", func() { t.Setenv("CLUS_SEARCH_TOKEN_BUDGET", "5000") }, "token_budget=5000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			got := evalConfig(st, false, false, false, "ev")
			if !strings.Contains(got, c.want) {
				t.Fatalf("config must name %q: %s", c.want, got)
			}
			if got == base {
				t.Fatalf("changing %s must change the fingerprint", c.name)
			}
		})
	}

	// A different budget/model must produce a different HASH, not just text.
	var prev string
	for _, alt := range []func(){
		func() { t.Setenv("LLM_CHAT_MODEL", "m1") },
		func() { t.Setenv("LLM_CHAT_MODEL", "m2") },
		func() { t.Setenv("LLM_CHAT_MODEL", "") },
	} {
		alt()
		sha := eval.Freeze(nil, nil, []byte(evalConfig(st, false, false, false, "ev")), 0).ConfigSHA
		if sha == prev && prev != "" {
			t.Fatal("distinct configs must hash differently")
		}
		prev = sha
	}
}

// maskHost must keep the endpoint identifiable without exposing credentials.
func TestMaskHost(t *testing.T) {
	if got := maskHost(""); got != "<unset>" {
		t.Fatalf("empty: %q", got)
	}
	if got := maskHost("https://api.minimaxi.com/v1"); got != "https://api.minimaxi.com" {
		t.Fatalf("host only: %q", got)
	}
	if got := maskHost("not a url"); got != "<unparseable>" {
		t.Fatalf("garbage: %q", got)
	}
}

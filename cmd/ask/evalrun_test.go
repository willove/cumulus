package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/eval"
	"github.com/cumubase/ask/internal/fast"
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

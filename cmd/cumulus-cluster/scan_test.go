package main

import (
	"testing"
)

// parseScanRank is the defensive boundary between a model answer and the
// rule-ordered candidate list: unknown paths dropped, fenced JSON unwrapped,
// garbage rejected (ApplyRank then keeps the rule order).
func TestParseScanRank(t *testing.T) {
	known := map[string]bool{"/d/a.md": true, "/d/b.txt": true, "/d/c.pdf": true}

	got, err := parseScanRank(`{"ranking":["/d/c.pdf","/d/a.md"]}`, known)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "/d/c.pdf" || got[1] != "/d/a.md" {
		t.Fatalf("ranked: %v", got)
	}

	// Fenced JSON + unknown paths + duplicates.
	got, err = parseScanRank("```json\n{\"ranking\":[\"/d/b.txt\",\"/d/nope.md\",\"/d/b.txt\",\"/d/a.md\"]}\n```", known)
	if err != nil {
		t.Fatalf("fenced parse: %v", err)
	}
	if len(got) != 2 || got[0] != "/d/b.txt" || got[1] != "/d/a.md" {
		t.Fatalf("fenced ranked: %v", got)
	}

	// Prose around the object.
	got, err = parseScanRank("排名如下：\n{\"ranking\":[\"/d/a.md\"]}\n完毕", known)
	if err != nil || len(got) != 1 || got[0] != "/d/a.md" {
		t.Fatalf("prose parse: %v %v", got, err)
	}

	// Garbage is an error, not a panic.
	if _, err := parseScanRank("我不知道", known); err == nil {
		t.Fatalf("garbage must error")
	}
	// Empty ranking is an error — ApplyRank keeps rules.
	if _, err := parseScanRank(`{"ranking":[]}`, known); err == nil {
		t.Fatalf("empty ranking must error")
	}
	// Only-unknown paths is an error too.
	if _, err := parseScanRank(`{"ranking":["/d/ghost.md"]}`, known); err == nil {
		t.Fatalf("all-unknown must error")
	}
}

func TestScanRankFuncNilClient(t *testing.T) {
	if scanRankFunc(nil) != nil {
		t.Fatalf("nil chat client must yield no ranker")
	}
}

package eval

import "testing"

func TestScoreEvRecAndGround(t *testing.T) {
	it := Item{ID: "q1", Query: "连接池", Answer: "128", Gold: []string{"handbook", "pool"}}
	p := Prediction{
		Query: "连接池", Answer: "连接池最大 128",
		SourceIDs: []string{"src:handbook", "src:other"},
		Resolved:  2, Refs: 2,
	}
	s := Score(it, p)
	if !s.EvRec {
		t.Fatal("gold handbook should hit src:handbook substring")
	}
	if !s.Grounded {
		t.Fatal("all refs resolved and non-empty answer → grounded")
	}
	if !s.Correct {
		t.Fatal("gold 128 in answer → correct")
	}
}

func TestScoreClosedBookByConstruction(t *testing.T) {
	it := Item{ID: "q2", Query: "无检索", Answer: "42"}
	s := ClosedBook(it, "答案是 42")
	if !s.Correct {
		t.Fatal("model-memory answer can still be EM-correct")
	}
	if s.EvRec || s.Grounded {
		t.Fatal("closed-book must have EvRec=Ground=false")
	}
}

func TestScoreUnresolvedCitesNotGrounded(t *testing.T) {
	it := Item{ID: "q3", Query: "x", Answer: "y", Gold: []string{"s"}}
	p := Prediction{Answer: "y", SourceIDs: []string{"s"}, Resolved: 1, Refs: 2}
	s := Score(it, p)
	if s.Grounded {
		t.Fatal("unresolved citation → not grounded")
	}
}

func TestAggregateAndMcNemar(t *testing.T) {
	a := []ItemScore{{Correct: true}, {Correct: true}, {Correct: false}}
	b := []ItemScore{{Correct: true}, {Correct: false}, {Correct: false}}
	r := Aggregate(a)
	if r.N != 3 || r.EM <= 0.6 {
		t.Fatalf("report=%+v", r)
	}
	m := Compare(a, b)
	if m.BOnly != 1 || m.COnly != 0 {
		t.Fatalf("mcnemar=%+v", m)
	}
	if m.P <= 0 || m.P > 1 {
		t.Fatalf("p=%v", m.P)
	}
}

func TestMcNemarEmptyDiscordant(t *testing.T) {
	a := []ItemScore{{Correct: true}, {Correct: false}}
	b := []ItemScore{{Correct: true}, {Correct: false}}
	m := Compare(a, b)
	if m.P != 1 {
		t.Fatalf("p=%v want 1 when no discordant pairs", m.P)
	}
}

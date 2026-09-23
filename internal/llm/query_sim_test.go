package llm

import "testing"

func TestParseQueryAbstractJSON(t *testing.T) {
	need, err := ParseQueryAbstractJSON(`{"need": "用户想了解退货期限"}`)
	if err != nil || need != "用户想了解退货期限" {
		t.Fatalf("need=%q err=%v", need, err)
	}
	if _, err := ParseQueryAbstractJSON("nope"); err == nil {
		t.Fatal("must reject non-JSON")
	}
}

func TestParseQueryListJSON(t *testing.T) {
	qs, err := ParseQueryListJSON(`{"queries": ["a", " b ", ""]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 3 || qs[1] != " b " {
		t.Fatalf("qs=%v", qs)
	}
}

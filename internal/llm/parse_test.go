package llm

import "testing"

// Frozen shape regressions for the prompt JSON contracts (§6.3).
func TestParseAnalyzeJSON(t *testing.T) {
	raw := `{"intent":"search","primary":{"连接池":0.9},"fallback":{"最大":0.4},"keywords_alt":{"pool":0.5}}`
	a, err := ParseAnalyzeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Intent != "search" || a.Primary["连接池"] != 0.9 {
		t.Fatalf("analyze parse: %+v", a)
	}
	if a.Fallback["pool"] != 0.5 {
		t.Fatalf("keywords_alt must merge into fallback: %+v", a.Fallback)
	}
	if _, err := ParseAnalyzeJSON("not json"); err == nil {
		t.Fatal("must reject non-JSON")
	}
}

func TestParseSynthesizeJSON(t *testing.T) {
	raw := `{"summary":"最大 128 [1]","citations":[{"index":1,"quote":"连接池最大 128"}],"confidence_note":"证据充分","refuse":false}`
	s, err := ParseSynthesizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Refuse || s.Summary == "" || len(s.Citations) != 1 {
		t.Fatalf("synthesize parse: %+v", s)
	}
}

func TestParseMultilevelJSON(t *testing.T) {
	raw := `{"level_1":["连接池"],"level_2":["最大连接数"],"level_3":["128"]}`
	levels, err := ParseMultilevelJSON(raw, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 || levels[2][0] != "128" {
		t.Fatalf("multilevel parse: %v", levels)
	}
}

func TestParseHistoryRewriteJSON(t *testing.T) {
	raw := `{"history_relevant": true, "standalone_query": "连接池最大连接数是多少", "changed": true}`
	r, err := ParseHistoryRewriteJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HistoryRelevant || !r.Changed || r.StandaloneQuery == "" {
		t.Fatalf("history parse: %+v", r)
	}
	if _, err := ParseHistoryRewriteJSON("nope"); err == nil {
		t.Fatal("must reject non-JSON")
	}
}

package llm

import "testing"

// Frozen shape regressions for the prompt JSON contracts.
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

// MiniMax-M3 inlines chain-of-thought without reasoning_split; the answer
// must survive extraction, and think text with braces must not poison the
// downstream JSON parse.
func TestSplitThink(t *testing.T) {
	raw := "<think>先分析 {\"score\": 9} 是不是陷阱。</think>\n\n{\"score\": 8, \"reasoning\": \"直接答案\"}"
	clean, reasoning := SplitThink(raw)
	if clean != `{"score": 8, "reasoning": "直接答案"}` {
		t.Fatalf("clean wrong: %q", clean)
	}
	if reasoning == "" || !containsAll(reasoning, "陷阱") {
		t.Fatalf("reasoning wrong: %q", reasoning)
	}
	plain := `{"score": 7, "reasoning": "无思考块"}`
	c2, r2 := SplitThink(plain)
	if c2 != plain || r2 != "" {
		t.Fatalf("plain content must pass through: %q %q", c2, r2)
	}
}

func containsAll(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Models decorate structured answers: trailing prose, a duplicate object,
// or — as baike-058 emitted — garbage after a complete object. The
// balanced-object extraction must recover the payload instead of failing
// the whole analysis.
func TestParseAnalyzeJSONExtractsFirstBalancedObject(t *testing.T) {
	// Trailing garbage after a complete object (the baike-058 failure shape).
	raw := `{"intent":"search","primary":{"国王":0.9}}  "junk after the object"`
	an, err := ParseAnalyzeJSON(raw)
	if err != nil {
		t.Fatalf("trailing garbage must not fail the parse: %v", err)
	}
	if an.Primary["国王"] != 0.9 {
		t.Fatalf("payload lost: %v", an.Primary)
	}
	// A brace inside a string value must not confuse the depth counting.
	raw2 := `{"intent":"search","primary":{"a}b":0.5},"tail":"x"}`
	an2, err := ParseAnalyzeJSON(raw2)
	if err != nil {
		t.Fatalf("string braces must not break extraction: %v", err)
	}
	if an2.Primary["a}b"] != 0.5 {
		t.Fatalf("string-brace key lost: %v", an2.Primary)
	}
	// Reasoning preamble before the object.
	raw3 := "Let me think. {\"intent\":\"search\",\"primary\":{\"王\":0.3}}"
	an3, err := ParseAnalyzeJSON(raw3)
	if err != nil {
		t.Fatalf("preamble must not fail the parse: %v", err)
	}
	if an3.Primary["王"] != 0.3 {
		t.Fatalf("preamble case lost: %v", an3.Primary)
	}
	// Genuinely broken (no balanced object) still errors — the caller
	// degrades to the rule analyzer rather than crashing the query.
	if _, err := ParseAnalyzeJSON(`{"intent": "search"`); err == nil {
		t.Fatal("unbalanced input must still error")
	}
}

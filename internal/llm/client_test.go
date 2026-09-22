package llm

import (
	"strings"
	"testing"
)

func TestParseScoreJSONRubricBands(t *testing.T) {
	cases := []struct {
		raw   string
		score float64
	}{
		{`{"score": 1, "reasoning": "无关"}`, 1},
		{"Here you go:\n{\"score\": 5, \"reasoning\": \"有关键词\"}\n", 5},
		{`{"score": 9, "reasoning": "直接答案"}`, 9},
	}
	for _, c := range cases {
		sc, why, err := ParseScoreJSON(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if sc != c.score {
			t.Fatalf("score %v want %v", sc, c.score)
		}
		if why == "" {
			t.Fatal("reasoning required")
		}
	}
	if _, _, err := ParseScoreJSON("not json"); err == nil {
		t.Fatal("must reject non-JSON")
	}
}

func TestTruncate(t *testing.T) {
	s := truncate(strings.Repeat("a", 50), 10)
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("truncate: %q", s)
	}
}

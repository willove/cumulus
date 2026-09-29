package llm

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/prompts"
)

// TestParseEvaluateBatchJSONHappy pins the happy path: a clean array maps
// id→position, so a model that answers out of order still lands correctly.
func TestParseEvaluateBatchJSONHappy(t *testing.T) {
	raw := `[{"id":"S2","score":5,"reasoning":"b","covers":[]},
	         {"id":"S1","score":8,"reasoning":"a","covers":["f1","f9"]}]`
	got, err := ParseEvaluateBatchJSON(raw, 2)
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if got[0].ID != "S1" || got[0].Score != 8 || got[1].ID != "S2" || got[1].Score != 5 {
		t.Fatalf("id mapping lost: %+v", got)
	}
}

// TestParseEvaluateBatchJSONDecorated: the same decorations that broke single
// objects (cf. firstJSONObject / baike-058) break arrays — leading prose,
// trailing garbage, a fenced block.
func TestParseEvaluateBatchJSONDecorated(t *testing.T) {
	raw := "好的，以下是打分结果：\n```json\n[{\"id\":\"S1\",\"score\":3,\"reasoning\":\"x\",\"covers\":[]}]\n```\n以上共 1 段。"
	got, err := ParseEvaluateBatchJSON(raw, 1)
	if err != nil {
		t.Fatalf("decorated array should parse: %v", err)
	}
	if got[0].Score != 3 {
		t.Fatalf("score = %v, want 3", got[0].Score)
	}
}

// TestParseEvaluateBatchJSONBracketsInStrings: brackets inside JSON strings
// must not participate in depth counting, or a reasoning like "见[注1]" ends
// the array early and the parse silently truncates.
func TestParseEvaluateBatchJSONBracketsInStrings(t *testing.T) {
	raw := `[{"id":"S1","score":2,"reasoning":"见[注1]的说明","covers":[]},{"id":"S2","score":9,"reasoning":"ok","covers":["f2"]}]`
	got, err := ParseEvaluateBatchJSON(raw, 2)
	if err != nil {
		t.Fatalf("string-bracket array: %v", err)
	}
	if got[1].Score != 9 {
		t.Fatalf("truncation: %+v", got)
	}
}

// TestParseEvaluateBatchJSONCountMismatch: a short or padded array is an
// error, never a silent truncation — downstream gates read scores
// positionally and a missing window would read as a refusal.
func TestParseEvaluateBatchJSONCountMismatch(t *testing.T) {
	if _, err := ParseEvaluateBatchJSON(`[{"id":"S1","score":1,"reasoning":"","covers":[]}]`, 2); err == nil {
		t.Fatal("short array parsed without error")
	}
	if _, err := ParseEvaluateBatchJSON(`[{"id":"S1","score":1,"reasoning":"","covers":[]},{"id":"S2","score":2,"reasoning":"","covers":[]}]`, 1); err == nil {
		t.Fatal("padded array parsed without error")
	}
}

// TestParseEvaluateBatchJSONMissingID: every mandated id must be present —
// an id the model renamed or skipped is a hard error, not a zero score.
func TestParseEvaluateBatchJSONMissingID(t *testing.T) {
	raw := `[{"id":"S1","score":1,"reasoning":"","covers":[]},{"id":"S3","score":7,"reasoning":"","covers":[]}]`
	if _, err := ParseEvaluateBatchJSON(raw, 2); err == nil {
		t.Fatal("missing S2 parsed without error")
	}
}

// TestFirstJSONArrayStringAwareness pins the extractor directly against
// prose that itself contains brackets (the "[S1]..[Sn]" labelling leaks into
// decorations easily).
func TestFirstJSONArrayStringAwareness(t *testing.T) {
	if got := firstJSONArray(`前缀 [引用] 文字 [{"id":"S1"}] 尾巴`); got != `[{"id":"S1"}]` {
		t.Fatalf("extractor picked %q", got)
	}
	if got := firstJSONArray(`no array here {"id":"S1"}`); got != "" {
		t.Fatalf("object-only input should yield no array, got %q", got)
	}
	if got := firstJSONArray(""); got != "" {
		t.Fatalf("empty input, got %q", got)
	}
}

// TestEvaluateBatchPromptShape is the frozen-prompt regression: the asset
// exists, renders every window with the mandated [Sn] labelling, and the
// count placeholders agree with the window list (a drifting template must
// fail here, not in a live A/B).
func TestEvaluateBatchPromptShape(t *testing.T) {
	tmpl := prompts.MustRender(prompts.EvaluateBatch, map[string]string{
		"query":   "专利法的目的",
		"count":   "2",
		"facts":   "（none）",
		"windows": "[S1] (Source: src:a [0,10))\n...第一段...\n\n[S2] (Source: src:b [20,30))\n...第二段...",
	})
	for _, want := range []string{
		"共 2 段，编号 [S1]..[S2]",
		"恰好 2 个对象",
		"[S1] (Source: src:a [0,10))",
		"[S2] (Source: src:b [20,30))",
		"0-3: 完全无关",
		"8-10: 含精确数据、事实或直接答案",
	} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("rendered batch prompt lost %q:\n%s", want, tmpl)
		}
	}
}

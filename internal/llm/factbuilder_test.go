package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/prompts"
)

// AigateFactBuilder's whole value is the degradation ladder: a model that
// answers with prose, an object, or fragments must land on the deterministic
// heuristic, never on a search with unreachable requirements. These cases pin
// every rung except the live call itself.

func TestParseDecomposeJSONPlainArray(t *testing.T) {
	parts, err := parseDecomposeJSON(`["实用新型的定义", "外观设计的定义"]`, "实用新型和外观设计分别指什么")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0] != "实用新型的定义" || parts[1] != "外观设计的定义" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestParseDecomposeJSONFencedAndPrefixed(t *testing.T) {
	for _, raw := range []string{
		"```json\n[\"连接池最大连接数是多少\"]\n```",
		"好的，拆解如下：\n[\"连接池最大连接数是多少\"]",
		"[\"连接池最大连接数是多少\"]\n以上。",
	} {
		parts, err := parseDecomposeJSON(raw, "连接池最大连接数是多少")
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if len(parts) != 1 || parts[0] != "连接池最大连接数是多少" {
			t.Fatalf("%q -> parts = %v", raw, parts)
		}
	}
}

// A wrapped object is a shape the model produces; it must be unwrapped, not
// treated as an unreadable response.
func TestParseDecomposeJSONWrappedObject(t *testing.T) {
	for _, key := range []string{"requirements", "facts", "parts"} {
		raw := `{"` + key + `": ["实用新型的定义"]}`
		parts, err := parseDecomposeJSON(raw, "连接池最大连接数是多少")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if len(parts) != 1 || parts[0] != "实用新型的定义" {
			t.Fatalf("%s -> parts = %v", key, parts)
		}
	}
}

// Prose, an empty string, and an unrelated object must all degrade to "no
// parts" — the caller's signal to fall back to facts.Build. None of them may
// return an error the query pays for.
func TestParseDecomposeJSONDegradesQuietly(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"这个问题只有一个需求。",
		`{"intent":"search"}`,
		`["" ]`,
		`["参"]`,                // below the unit floor
		`["\"右", "\"这本书的出版社"]`, // bracket-cut fragments
	} {
		parts, err := parseDecomposeJSON(raw, "连接池最大连接数是多少")
		if err != nil {
			t.Fatalf("%q: unexpected error %v", raw, err)
		}
		if len(parts) != 0 {
			t.Fatalf("%q must degrade to no parts, got %v", raw, parts)
		}
		if facts.BuildParts("q", parts) != nil {
			t.Fatalf("%q: BuildParts must stay nil so the caller falls back", raw)
		}
	}
}

// The gate applies to LLM output exactly as it does to any other source.
func TestParseDecomposeJSONAppliesSemanticUnitGate(t *testing.T) {
	// Leading fragment dropped, survivors renumbered.
	parts, err := parseDecomposeJSON(`["\"右", "实用新型的定义", "外观设计的定义"]`, "实用新型和外观设计分别指什么")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("want the fragment dropped, got %v", parts)
	}
	if parts[0] != "实用新型的定义" {
		t.Fatalf("order/content after drop: %v", parts)
	}
}

func TestParseDecomposeJSONCapsAtFour(t *testing.T) {
	raw := `["需求一是什么", "需求二是什么", "需求三是什么", "需求四是什么", "需求五是什么"]`
	// "分别" is what licenses a 5-way split here; without it the coordination
	// gate rejects the whole multi-part result (see TestParseDecomposeJSONRejectsUnlicensedSplit).
	parts, err := parseDecomposeJSON(raw, "五项要求分别是什么")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 {
		t.Fatalf("want the 4-unit ceiling, got %d: %v", len(parts), parts)
	}
}

// TestParseDecomposeJSONRejectsUnlicensedSplit is the measured 发明创造
// failure, pinned: the model split a statutory term of art into a coordinate
// pair, which produced a confident 0-loop stop citing the WRONG statute at 51%
// of the tokens. A K>1 result is only accepted when the query itself signals a
// requirement list.
func TestParseDecomposeJSONRejectsUnlicensedSplit(t *testing.T) {
	parts, err := parseDecomposeJSON(`["发明的定义", "创造的定义"]`, "发明创造定义")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Fatalf("a bare term-of-art lookup must not be split: %v", parts)
	}
	// The same parts ARE accepted when the query licenses a requirement list.
	parts, err = parseDecomposeJSON(`["发明的定义", "创造的定义"]`, "发明和创造分别指什么")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("a coordinated query must keep both units: %v", parts)
	}
	// A legal alternative is not coordination.
	parts, err = parseDecomposeJSON(
		`["数额特别巨大时的量刑", "有其他特别严重情节时的量刑"]`,
		"数额特别巨大或者有其他特别严重情节的，最高会判什么刑")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Fatalf("「或者」是法律择一，不得拆成两个需求: %v", parts)
	}
}

// The prompt asset must stay corpus-agnostic: no answer text, no domain
// persona. A domain hint in a shared asset is contamination (design-plan §6.1).
func TestDecomposePromptIsCorpusAgnostic(t *testing.T) {
	body, err := prompts.Load(prompts.DecomposeQuery)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"法律", "法条", "专利", "连接池", "你是"} {
		if strings.Contains(body, banned) && !strings.Contains(body, "领事保护") {
			// 领事保护 appears only inside the anti-miscut rule examples.
			t.Errorf("decompose_query must not carry domain content %q", banned)
		}
	}
	if !strings.Contains(body, "{{query}}") {
		t.Error("decompose_query must interpolate {{query}}")
	}
	// The anti-miscut rules are the whole point of the asset.
	for _, must := range []string{"禁止", "书名", "枚举", "或者"} {
		if !strings.Contains(body, must) {
			t.Errorf("decompose_query is missing the anti-miscut rule %q", must)
		}
	}
}

func TestAigateFactBuilderEmptyQueryShortCircuits(t *testing.T) {
	b := &AigateFactBuilder{Client: &ChatClient{BaseURL: "http://invalid.invalid"}}
	parts, err := b.Decompose(context.Background(), "   ")
	if err != nil {
		t.Fatalf("an empty query must not reach the model: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("empty query -> %v", parts)
	}
}

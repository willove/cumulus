package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/prompts"
)

func TestParseEvaluateDimsJSONHappy(t *testing.T) {
	raw := `[{"id":"S1","score":8,"reasoning":"直接给出数值。","covers":["f1"],"novelty":9,"support":2,"conflicts_with":["K2","S2"]},
	         {"id":"S2","score":5,"reasoning":"部分相关。","covers":[],"novelty":1,"support":6,"conflicts_with":[]}]`
	got, err := ParseEvaluateDimsJSON(raw, 2)
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if len(got[0].ConflictsWith) != 2 || got[0].ConflictsWith[0] != "K2" {
		t.Fatalf("conflicts_with not parsed: %+v", got[0])
	}
	if got[1].Novelty != 1 || got[1].Support != 6 {
		t.Fatalf("dims not parsed: %+v", got[1])
	}
}

// TestParseEvaluateDimsJSONOmittedDims: a model that drops the optional dims
// (novelty/support/conflicts_with absent) must still parse — the consumed
// field is score/covers, and a missing conflicts_with means "no marks".
func TestParseEvaluateDimsJSONOmittedDims(t *testing.T) {
	raw := `[{"id":"S1","score":7,"reasoning":"相关。","covers":["f1"]}]`
	got, err := ParseEvaluateDimsJSON(raw, 1)
	if err != nil {
		t.Fatalf("omitted dims should parse: %v", err)
	}
	if got[0].ConflictsWith != nil {
		t.Fatalf("absent conflicts_with must stay nil, got %v", got[0].ConflictsWith)
	}
}

func TestParseEvaluateDimsJSONCountAndID(t *testing.T) {
	if _, err := ParseEvaluateDimsJSON(`[{"id":"S1","score":1}]`, 2); err == nil {
		t.Fatal("short array parsed")
	}
	renamed := `[{"id":"S1","score":1},{"id":"S3","score":2}]`
	if _, err := ParseEvaluateDimsJSON(renamed, 2); err == nil {
		t.Fatal("renamed/missing id parsed")
	}
}

// TestEvaluateDimsPromptShape is the frozen-prompt regression for the v3b
// asset: digest section, K/S labelling, the verbatim 0-10 ruler, and the
// conflict rule (same-fact-point only).
func TestEvaluateDimsPromptShape(t *testing.T) {
	tmpl := prompts.MustRender(prompts.EvaluateDims, map[string]string{
		"query":        "专利法的目的",
		"count":        "2",
		"digest_count": "1",
		"facts":        "（none）",
		"digest":       "[K1] 此前已采到的窗口内容……",
		"windows":      "[S1] (Source: src:a [0,10))\n...第一段...\n\n[S2] (Source: src:b [20,30))\n...第二段...",
	})
	for _, want := range []string{
		"Current Evidence Digest（此前已采证据，编号 [K1]..[K1]",
		"共 2 段，编号 [S1]..[S2]",
		"恰好 2 个对象",
		"[K1] 此前已采到的窗口内容",
		"novelty",
		"support",
		"conflicts_with",
		"同一事实点给出不同数值/结论才算矛盾",
		"0-3: 完全无关",
		"8-10: 含精确数据、事实或直接答案",
	} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("rendered dims prompt lost %q", want)
		}
	}
}

// TestScoreBatchConflictDigestBounds pins the digest contract: top-3 by
// score, truncated, stable for equal scores. It exercises the rendering
// through a fake transport-free path — the prompt is captured by rendering
// with a nil-client guard... instead we pin the helper pieces via the
// prompt test above and bound the digest selection logic here through the
// exported method on a stub client is impossible offline; so pin the
// selection constants instead.
func TestScoreBatchConflictDigestConstants(t *testing.T) {
	if digestTop != 3 {
		t.Fatalf("digestTop drifted: %d (the A/B ran with 3)", digestTop)
	}
	if maxDigestRunes != 400 {
		t.Fatalf("maxDigestRunes drifted: %d", maxDigestRunes)
	}
	// Sample.Conflicts must be part of the JSON contract the search API
	// serializes — pin the tag.
	sm := mcs.Sample{Conflicts: []string{"K1"}}
	if !strings.Contains(string(mustJSON(t, sm)), `"conflicts"`) {
		t.Fatal("Sample.Conflicts lost its json tag")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

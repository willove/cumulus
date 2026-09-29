package llm

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/prompts"
)

func TestParseAgreeJSON(t *testing.T) {
	// Happy disagree with a summary.
	got, err := ParseAgreeJSON(`{"agree": false, "conflict_summary": "《值雨》的作者，一段记为郭印，另一段记为宋伯仁。"}`)
	if err != nil || got.Agree || got.ConflictSummary == "" {
		t.Fatalf("disagree parse: %+v err=%v", got, err)
	}
	// Agree must blank the summary (no half-marked states on the wire).
	got, err = ParseAgreeJSON(`{"agree": true, "conflict_summary": "模型可能残留"}`)
	if err != nil || !got.Agree || got.ConflictSummary != "" {
		t.Fatalf("agree must blank summary: %+v err=%v", got, err)
	}
	// Decorated prose around the object (the firstJSONObject contract).
	raw := "判读如下：\n```json\n{\"agree\": false, \"conflict_summary\": \"数字不一致。\"}\n```\n完毕。"
	if got, err = ParseAgreeJSON(raw); err != nil || got.Agree {
		t.Fatalf("decorated parse: %+v err=%v", got, err)
	}
}

// TestEvidenceAgreePromptShape is the frozen-prompt regression: labelled
// windows, the same-fact-point rule, and the no-external-knowledge rule.
func TestEvidenceAgreePromptShape(t *testing.T) {
	tmpl := prompts.MustRender(prompts.EvidenceAgree, map[string]string{
		"query":   "《值雨》的作者是谁",
		"count":   "2",
		"windows": "[E1] (Source: gold [0,18))\n...《值雨》 作者：郭印。...\n\n[E2] (Source: dist [0,18))\n...《值雨》 作者：宋伯仁。...",
	})
	for _, want := range []string{
		"共 2 段，编号 [E1]..[E2]",
		"[E1] (Source: gold [0,18))",
		"同一事实点",
		"不同数值、不同人名、不同结论",
		"不要凭外部知识裁决",
		"agree",
		"conflict_summary",
	} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("evidence_agree prompt lost %q", want)
		}
	}
}

// Thinking depth control for reasoning-capable models (MiniMax M3.1+,
// OpenAI o1/o3+, Anthropic Claude 3.5+). Different pipeline stages need
// different depths: keyword extraction is mechanical (low), answer synthesis
// is the quality moment (high). The old binary Complete/CompleteStructured
// (thinking on/off) is the degenerate case of this spectrum.
//
// Provider mapping (both fields sent; the endpoint uses what it knows):
//   MiniMax Anthropic API: output_config.effort = "low"|"medium"|"high"|"xhigh"|"max"
//   OpenAI o1/o3:         reasoning_effort = "low"|"medium"|"high"
//   Anthropic native:     thinking.budget_tokens (not mapped here — the
//                          Anthropic-compatible gateways translate effort)
package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ThinkingLevel is the reasoning depth for one LLM call.
type ThinkingLevel string

const (
	// ThinkingLow: mechanical passes (keyword extraction, term expansion,
	// structured JSON extraction) — fastest, cheapest.
	ThinkingLow ThinkingLevel = "low"
	// ThinkingMedium: scoring passes that benefit from brief reasoning
	// but don't need chains (evidence 0-10 relevance, decomposition).
	ThinkingMedium ThinkingLevel = "medium"
	// ThinkingHigh: the quality moments (answer synthesis, judging,
	// multi-hop reasoning) — the default for Complete().
	ThinkingHigh ThinkingLevel = "high"
	// ThinkingXHigh / ThinkingMax: provider-specific extremes, opt-in.
	ThinkingXHigh ThinkingLevel = "xhigh"
	ThinkingMax   ThinkingLevel = "max"
)

// CompleteWithEffort sends one chat turn with an explicit thinking depth.
// The effort parameter is mapped to both known provider formats (both are
// sent in the body; the endpoint picks the one it recognizes and ignores
// the other — same convention as reasoning_split and thinking{type}).
func (c *ChatClient) CompleteWithEffort(ctx context.Context, user string, effort ThinkingLevel) (string, error) {
	if c.BaseURL == "" {
		return "", fmt.Errorf("llm: BaseURL required")
	}
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
		"temperature": 0,
	}
	if c.ReasoningSplit {
		body["reasoning_split"] = true
	}
	if effort != "" {
		// MiniMax Anthropic-compatible (M3.1+): the canonical depth control.
		body["output_config"] = map[string]string{"effort": string(effort)}
		// OpenAI-compatible (o1/o3+): the equivalent parameter, different name.
		// Unknown-field-tolerant endpoints ignore whichever they don't use.
		body["reasoning_effort"] = string(effort)
	}
	return c.doChat(ctx, body)
}

// StageEffort reads the per-stage thinking depth from the environment.
// Format: CLUS_THINK_<STAGE>=low|medium|high|xhigh|max
// Stages: ANALYZE, SCORE, SYNTH, JUDGE, EXPAND.
// An empty/invalid value returns the fallback.
func StageEffort(stage string, fallback ThinkingLevel) ThinkingLevel {
	v := os.Getenv("CLUS_THINK_" + strings.ToUpper(stage))
	switch ThinkingLevel(strings.TrimSpace(v)) {
	case ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax:
		return ThinkingLevel(strings.TrimSpace(v))
	}
	return fallback
}

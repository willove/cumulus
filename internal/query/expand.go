package query

import (
	"encoding/json"
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/llm"
)

// Expander 是词汇鸿沟的桥：把口语问句翻成语料语言的检索词。
//
// cumulus 的 keywords_multilevel 契约 + fast.SampleContext 的“文档是
// 对的、用词对不上”案例（“养狗叫得太吵” vs 法条“饲养动物”）。本项目
// 的落点：主关键词级稀薄（query.Analysis.Thin）时调用一次，产出替换
// 词表，用加权检索再取一轮。
type Expander interface {
	Expand(ctx gocontext.Context, question string) ([]string, error)
}

// LLM 用模型做关键词扩展。严格 JSON 契约：
//
//	{"keywords": ["词一", "词二"]}
//
// 解析不放松（同 synth 的口径）：畸形输出 = 错误，调用方按“扩展失败”
// 处理（退化到原查询重试，不许把模型废话当检索词）。
type LLM struct {
	Client llm.Completer
}

const expandSystem = `你是检索词翻译器。用户用口语提问，检索库用书面语。
给出适合在法规/文档库里检索的替换词（3-8 个），覆盖问句的实体与动作，
不要整句。输出严格 JSON：{"keywords": ["...", "..."]}。`

type expandAnswer struct {
	Keywords []string `json:"keywords"`
}

func (l *LLM) Expand(ctx gocontext.Context, question string) ([]string, error) {
	if l.Client == nil {
		return nil, llm.ErrNotConfigured
	}
	resp, err := l.Client.Complete(ctx, llm.Request{System: expandSystem, Prompt: question, MaxTokens: 200})
	if err != nil {
		return nil, fmt.Errorf("query expand: complete: %w", err)
	}
	raw := strings.TrimSpace(resp.Text)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var parsed expandAnswer
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("query expand: not the agreed JSON contract: %w; raw=%q", err, truncate(raw, 200))
	}
	out := make([]string, 0, len(parsed.Keywords))
	for _, k := range parsed.Keywords {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("query expand: empty keywords; raw=%q", truncate(raw, 200))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

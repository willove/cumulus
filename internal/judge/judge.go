// Package judge 是判官臂：判“答案与金标是否等价”。
//
// 判官不覆盖规则分（cumulus 的口径）：两套分数并列呈现，谁不替谁
// 说话。未判分是 N/A，不是 0。
package judge

import (
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/llm"
)

// Judge 判一次等价性。裁决带 token 账单（判官不是免费劳动力）。
type Judge interface {
	Judge(question, answer, gold string) (evalfcore.Verdict, error)
}

// LLM 用模型判等价。问法只有一句：是否等价，只许答 YES/NO。
// 模型不守规矩（既不说 YES 也不说 NO）时返回错误——判不了就是判不了，
// 不许猜一个 true/false 混进统计。
type LLM struct {
	Client llm.Completer
}

const judgeSystem = `你是裁判。判断候选答案与标准答案在事实层面是否等价（数字、实体、
结论一致即算等价，措辞不同不算不等价）。只回答 YES 或 NO，不要解释。`

func (l *LLM) Judge(question, answer, gold string) (evalfcore.Verdict, error) {
	if l.Client == nil {
		return evalfcore.Verdict{}, llm.ErrNotConfigured
	}
	prompt := fmt.Sprintf("问题：%s\n标准答案：%s\n候选答案：%s\n是否等价？", question, gold, answer)
	resp, err := l.Client.Complete(gocontext.Background(), llm.Request{System: judgeSystem, Prompt: prompt, MaxTokens: 8})
	if err != nil {
		return evalfcore.Verdict{}, fmt.Errorf("judge: complete: %w", err)
	}
	v := evalfcore.Verdict{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown:        resp.Usage.CostKnown,
	}
	switch upper := strings.ToUpper(strings.TrimSpace(resp.Text)); {
	case strings.HasPrefix(upper, "YES"):
		v.OK = true
		return v, nil
	case strings.HasPrefix(upper, "NO"):
		v.OK = false
		return v, nil
	default:
		return evalfcore.Verdict{}, fmt.Errorf("judge: unparseable verdict %q", truncate(resp.Text, 40))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = llm.ErrNotConfigured

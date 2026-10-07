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

// 判官口径（校准后）：问“支持”不问“等价”。
//
// 校准数据（25 题人工核对）发现原口径（“事实等价”）在**金标是段落**
// 的基准上系统性过严：答案逐字重述金标（q16）、答案第一句即金标原文
// 后面带解释（q13）都被判 NO——拿答案和整段法条要求等价，答案当然
// “不等价”。正确口径：答案的每个事实点是否被金标支持；答案可以只覆盖
// 金标一部分、可以带解释，只要不与金标矛盾、不引入金标外的内容。
const judgeSystem = `你是裁判。判断候选答案是否被标准答案（金标材料）支持。
判 YES：答案的事实点都能在金标里找到依据；答案可以只覆盖金标的一部分，
可以换措辞、可以带解释。
判 NO：答案与金标矛盾，或答案包含金标中没有依据的内容。
标准答案是一段参考材料，不是唯一正确表述——不要因为它比答案长就判 NO。
只回答 YES 或 NO，不要解释。`

func (l *LLM) Judge(question, answer, gold string) (evalfcore.Verdict, error) {
	if l.Client == nil {
		return evalfcore.Verdict{}, llm.ErrNotConfigured
	}
	prompt := fmt.Sprintf("问题：%s\n金标材料：%s\n候选答案：%s\n候选答案是否被金标材料支持？", question, gold, answer)
	resp, err := l.Client.Complete(gocontext.Background(), llm.Request{System: judgeSystem, Prompt: prompt, MaxTokens: 64})
	if err != nil {
		return evalfcore.Verdict{}, fmt.Errorf("judge: complete: %w", err)
	}
	v := evalfcore.Verdict{
		Raw:              resp.Text,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown:        resp.Usage.CostKnown,
	}
	// 判定词双语：提示词是中文，模型答中文（"是/否/等价/不等价"）远多于
	// 英文。只认 YES/NO 的判官对中文模型等于没有判官（真实运行：50 题
	// 只有 3 题判上分）。先判强信号（不等价为否），再判等价类为是。
	verdict := strings.TrimSpace(resp.Text)
	switch {
	case strings.HasPrefix(verdict, "不等价"), strings.HasPrefix(verdict, "否"), strings.HasPrefix(verdict, "不对"), strings.HasPrefix(verdict, "NO"), strings.HasPrefix(verdict, "no"):
		v.OK = false
		return v, nil
	case strings.HasPrefix(verdict, "等价"), strings.HasPrefix(verdict, "是"), strings.HasPrefix(verdict, "对"), strings.HasPrefix(verdict, "YES"), strings.HasPrefix(verdict, "yes"):
		v.OK = true
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

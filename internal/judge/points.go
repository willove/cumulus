package judge

import (
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/marks"
)

// 分点覆盖判官：把金标拆成**要点**，逐条问"候选答案答没答到"，命中比例过阈
// 才算对。
//
// 为什么要有（真跑踩出来的）：等义判官在**多跳综合**任务上系统性判否——
// DomainRAG multidoc 的金标是人工综合段（"共同目标是…在独特性方面…"），模型
// 只要措辞或分点不同就被判否：48 题里 evidence 70.8%、judge 只有 6.8%，逐题看
// 有一半的答案与金标几乎逐字一致仍判 NO。**那是口径不匹配，不是能力缺口**。
// 分点判据把"整段等义"换成"逐点命中"，量的是**答案有没有覆盖金标的每个要点**。
//
// 口径：
//   - 拆点按中文标点（。！？；\n）+ 句长上限，确定性、零成本；
//   - **一次调用**逐点判定（不是每点一次）——判官账单是实打实的；
//   - 阈值**默认 0.6 且可配**，但真上线前要按数据校准（别再拍一个数上去）。
type Points struct {
	Client llm.Completer
	// Threshold 是命中比例下限（默认 0.6）。0.6 的来历：多跳任务里"答到大部分
	// 要点"就应当算对，逐字对齐对综合答案不现实。
	Threshold float64
	// MaxPoints 限制送进提示词的点数（默认 12）——金标超长时先截断并在
	// Raw 里注明（"不许静默截断"）。
	MaxPoints int
}

const pointsSystem = `你是分点覆盖裁判。给定标准答案拆出的若干要点和候选答案，逐条判断候选答案**是否覆盖了这条要点**。
对每条输出一行，严格格式：编号:Y 或 编号:N。
- 候选答案答出了该要点（措辞不同没关系，可以只覆盖要点的一部分）→ Y。
- 候选答案没有该要点的内容 → N。
只回答要点，不要写解释。`

// Judge 实现 evalfcore.Judge。
func (p *Points) Judge(question, answer, gold string) (evalfcore.Verdict, error) {
	if p == nil || p.Client == nil {
		return evalfcore.Verdict{}, llm.ErrNotConfigured
	}
	points := SplitPoints(gold)
	maxP := p.MaxPoints
	if maxP <= 0 {
		maxP = 12
	}
	truncated := false
	if len(points) > maxP {
		points = points[:maxP]
		truncated = true
	}
	if len(points) == 0 {
		// 拆不出要点（单行无标点）→ 退回等义判官，不假装分点判过了
		return (&LLM{Client: p.Client}).Judge(question, answer, gold)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "标准答案要点：\n")
	for i, pt := range points {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, truncate(pt, 120))
	}
	fmt.Fprintf(&b, "\n候选答案：%s\n", truncate(answer, 800))
	// MaxTokens 要给**推理 token 留够**：本项目的 provider 是推理模型
	// （.env 里 LLM_REASONING_SPLIT），先写一段思考再给答案——真跑踩过
	// MaxTokens=4+3×点数 → 推理把预算吃光 → "only reasoning content
	// returned, no answer"，48 题里 37 题整题报错（读数只剩 10 题，不可信）。
	resp, err := p.Client.Complete(gocontext.Background(), llm.Request{
		System:    pointsSystem,
		Prompt:    b.String(),
		MaxTokens: 96 + 6*len(points),
	})
	if err != nil {
		return evalfcore.Verdict{}, fmt.Errorf("judge: points complete: %w", err)
	}
	parsed := marks.Parse(resp.Text)
	covered := 0
	for i := range points {
		if parsed.Covers(i + 1) {
			covered++
		}
	}
	ratio := float64(covered) / float64(len(points))
	v := evalfcore.Verdict{
		OK:               ratio >= p.threshold(),
		Coverage:         ratio,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown:        resp.Usage.CostKnown,
	}
	v.Raw = fmt.Sprintf("points=%d covered=%d ratio=%.2f judged=%d raw=%q%s",
		len(points), covered, ratio, parsed.Judged, resp.Text,
		map[bool]string{true: " [truncated]"}[truncated])
	return v, nil
}

func (p *Points) threshold() float64 {
	if p.Threshold <= 0 || p.Threshold > 1 {
		return 0.6
	}
	return p.Threshold
}

// SplitPoints 把金标拆成要点：按中文标点断句（。！？；\n），滤掉太短的碎片，
// 单条上限 200 字（再长按逗号断）。确定性别名：同样输入同样要点。
func SplitPoints(gold string) []string {
	text := strings.TrimSpace(gold)
	if text == "" {
		return nil
	}
	var raw []string
	cur := strings.Builder{}
	for _, ch := range text {
		cur.WriteRune(ch)
		if strings.ContainsRune("。！？；!?;\n", ch) {
			raw = append(raw, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		raw = append(raw, cur.String())
	}
	var out []string
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if len([]rune(s)) < 4 {
			continue // 太碎的（"嗯"、"。"）不是要点
		}
		if len([]rune(s)) <= 200 {
			out = append(out, s)
			continue
		}
		// 超长：按逗号断成几段，仍超长就原样保留（截断在提示词侧有记录）
		for _, part := range strings.Split(s, "，") {
			part = strings.TrimSpace(part)
			if len([]rune(part)) >= 8 {
				out = append(out, part)
			}
		}
		if len(out) > 0 && len([]rune(out[len(out)-1])) == 0 {
			out = out[:len(out)-1]
		}
	}
	if len(out) == 0 {
		out = []string{text} // 拆不出就当一个要点
	}
	return out
}

// Package rerank is the window selector: given a limited budget, which
// windows from the pool do we keep.
//
// 为什么要有它：深循环把池子取大（实测池子 recall 85–92%），但**兑现**靠
// 选择——词法分数、词面覆盖、MiniLM 余弦三种便宜选择器都只兑现了个位数
// 百分点（DuReader hard：池 92.2% → 兑现 70.8%；DomainRAG 合并集：池 85.2%
// → α=0.20 时答满但风险 15%）。剩下的路只有一条：**真的做相关性判断**。
//
// 形态：**逐条 Y/N 判定的清单式交叉编码器**——一次调用把整池编号交给模型，
// 它逐条回答"这段里能不能找到答案"，我们取判 Y 的。两种踩过的坑：
//
//   - 清单式"挑最相关的 N 条"会被模型偷懒（实测每题只给 1/9）；改成逐条判
//     Y/N 才榨得出信息，而**一次调用**仍然够用（逐条打分是 27 倍调用）。
//   - **半信半疑地混用最伤**：模型只给 1 条时，"1 条模型 + 8 条按序"会把
//     top-9 的好窗口挤掉，evidence 从 85% 掉到 40%。所以判得太少就**整份
//     回退到分数序**，并把这件事记进 Reason（degraded 必须可见）。
package rerank

import (
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/marks"
)

// LLM 是清单式交叉编码器（逐条 Y/N 判定）。
type LLM struct {
	Client llm.Completer
	// RunesPerPassage 是每条送进提示词的字数上限（法条/网页窗口可以很长，
	// 截断是为了让"一次调用装得下整池"——提示词越长越贵也越容易看漏）。
	RunesPerPassage int
	// MaxCandidates 是送进模型的候选上限（池子比它大时先按分数截断）。
	MaxCandidates int
	// Reason 记进遥测（KeyRerank）：判定了几条、退回过没有、为什么。
	Reason string
}

// rerankSystem 只做一件事：逐条判断"这段里能不能直接找到答案"。**不许复述、
// 不许评价、逐条都要答**——它是重排器，不是判官。
const rerankSystem = `你是检索重排器。给定一个问题和若干编号片段，对**每一个**片段判断：它的原文里能不能直接找到该问题的答案。
对每个片段输出一行，严格格式：编号:Y 或编号:N（Y=能找到答案依据，N=不能）。编号从 1 开始，逐条都要有，不许省略、不许只挑几条。
只要片段里能找到答案依据就判 Y，哪怕答案不完整；多个片段合起来能答就都判 Y。
不要输出解释。`

// Select 实现 deepcore.Selector。
func (l *LLM) Select(ctx gocontext.Context, query string, pool []deepcore.Window, budget int) ([]deepcore.Window, error) {
	if l == nil || l.Client == nil {
		return nil, llm.ErrNotConfigured
	}
	if budget <= 0 || len(pool) <= budget {
		return pool, nil
	}
	runes := l.RunesPerPassage
	if runes <= 0 {
		runes = 200
	}
	maxCand := l.MaxCandidates
	if maxCand <= 0 || maxCand > len(pool) {
		maxCand = len(pool)
	}
	cand := topByScore(pool, maxCand) // 确定序：分数降序（同分保原序）

	var sb strings.Builder
	fmt.Fprintf(&sb, "问题：%s\n逐条判断下列片段能不能找到该问题的答案（最多保留 %d 条）：\n", query, budget)
	for i, w := range cand {
		text := w.Text
		if r := []rune(text); len(r) > runes {
			text = string(r[:runes]) + "…"
		}
		fmt.Fprintf(&sb, "[%d] %s\n", i+1, strings.TrimSpace(text))
	}
	resp, err := l.complete(ctx, sb.String(), len(cand))
	if err != nil {
		return nil, fmt.Errorf("rerank: complete: %w", err)
	}
	parsed := marks.Parse(resp.Text)
	ys, judged := parsed.Yes, parsed.Judged
	// 回退判据是"**没干活**"（judged 太少），不是"判 Y 少"。
	// 真跑踩过：模型逐条判完 12 条、只给 1 条 Y，而那条恰好就是含答案的
	// ——这是**精确**而非偷懒；用 yes<budget/2 当懒会把它误杀成回退（40/40
	// 题全部白白回退，重排等于没跑）。偷懒的形状是"只判了一条就交差"。
	if judged < 2 {
		l.note(fmt.Sprintf("llm-not-working(judged=%d)-fallback-score-order", judged))
		return topByScore(pool, budget), nil
	}
	if len(ys) == 0 {
		l.note("llm-all-no-fallback-score-order")
		return topByScore(pool, budget), nil
	}
	chosen := make([]deepcore.Window, 0, budget)
	used := make(map[int]bool, len(ys))
	for _, id := range ys {
		if id < 1 || id > len(cand) || used[id] {
			continue
		}
		used[id] = true
		chosen = append(chosen, cand[id-1])
	}
	// 判 Y 不足预算：按分数序补齐（补的是**原本就在 top 里**的窗口，不是替换）。
	if len(chosen) < budget {
		for i, w := range cand {
			if len(chosen) >= budget {
				break
			}
			if used[i+1] {
				continue
			}
			used[i+1] = true
			chosen = append(chosen, w)
		}
		l.note(fmt.Sprintf("llm-yes=%d/%d-filled-by-order(judged=%d)", len(ys), budget, judged))
		return chosen, nil
	}
	l.note(fmt.Sprintf("llm-verdicts(judged=%d,yes=%d)", judged, len(ys)))
	return chosen, nil
}

func (l *LLM) note(s string) {
	if l.Reason == "" {
		l.Reason = s
	}
}

// complete 带一次重试：提供方偶发 5xx/限流直接让整题变成 eval-error 是最亏的
// 失败方式（一题的钱已经花了，答案只差一次重试）。真跑踩过：20 题里 11 题
// 因为一次失败整题报错。
func (l *LLM) complete(ctx gocontext.Context, prompt string, cands int) (llm.Response, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := l.Client.Complete(ctx, llm.Request{
			System:    rerankSystem,
			Prompt:    prompt,
			MaxTokens: 6 + 3*cands, // "12:Y\n" ≈ 5 字符/条
		})
		if err == nil {
			return resp, nil
		}
		last = err
	}
	return llm.Response{}, last
}

// topByScore 取分数前 k 条（回退口径，确定序：同分保原序）。
func topByScore(pool []deepcore.Window, k int) []deepcore.Window {
	sorted := append([]deepcore.Window(nil), pool...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Score > sorted[j-1].Score; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted
}

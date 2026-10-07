package qaflow

import (
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
)

// FactsStage 是证据侧的事实覆盖判定（cumulus facts.Evaluate 的接线）：
// 逐条事实看有没有窗真盖到（3 字核心在场），产出 Report（Complete/
// Missing/Weakest）与**证据一致性门**的冲突列表（同事实两窗给不同的数）。
// 挂在 evidence 之后、evict 之前——判定要看的是驱逐前还是驱逐后的窗？
// 驱逐前：事实缺口在驱逐前就报出来，驱逐不能把"未盖"洗成"已盖"。
type FactsStage struct {
	// Scorer 模型判官（词面判据的兜底升级）。nil = 只有词面判据。
	// 判官只升级不降级：词面已盖的事实不过问；失败/违约时保持词面原判
	// ——判官缺席流程照跑。
	Scorer facts.Scorer
}

func (FactsStage) Name() string { return "fact-coverage" }
func (FactsStage) Reads() []string {
	return []string{KeyWindows.String(), KeyFacts.String()}
}
func (FactsStage) Writes() []string {
	return []string{KeyFactReport.String(), KeyConflicts.String()}
}

func (s FactsStage) Run(c *context.Context) error {
	fx, ok := context.Get(c, KeyFacts)
	if !ok {
		// 直驱路径（测试/单独调深路）没走 intent 阶段：门静默跳过，不
		// 失败——生产流程 RewriteStage 恒在先，门在那里生效
		return context.Set(c, KeyFactReport, facts.Report{})
	}
	ws, _ := context.Get(c, KeyWindows)
	views := make([]facts.Window, 0, len(ws))
	for _, w := range ws {
		views = append(views, facts.Window{SourceID: w.SourceID, Span: w.Span, Text: w.Text, Score: w.Score})
	}
	rep := facts.Evaluate(fx, views)
	// 判官兜底：词面判没盖的事实（认不出改写的那类——"专利期"三字在
	// "专利权的期限"里就不连续）问一次模型。词面判据的盲区正是模型
	// 的读长项（Noesis：瓶颈是上下文利用，不是检索质量）。
	_ = facts.Rescue(&rep, views, s.Scorer) // 判官失败 = 没有判官，原判不变
	conflicts := facts.Conflicts(fx, views)
	if err := context.Set(c, KeyFactReport, rep); err != nil {
		return err
	}
	return context.Set(c, KeyConflicts, conflicts)
}

func (FactsStage) Verify(c *context.Context) error { return nil }

// AbstainStage 是零 LLM 失败预测头（cumulus internal/abstain 移植）：
// 检索期结构特征 → logistic p_fail → 早弃权（refuse）/ 强升级（deep）。
// 放在 route 之后、escalate 之前：头看见的是证据+事实+置信（全是前置于
// 合成的信号），它的 refuse 意思是"别升级了，直接拒"——DEEP 的扩征在
// 这类题上是纯燃烧（cumulus 实测 ≈96s/19k tokens 换必然的拒答）。
type AbstainStage struct {
	Head *abstain.Head // nil = 门缺席（可选组件，不启用就不注册）
}

func (AbstainStage) Name() string { return "abstain-gate" }
func (AbstainStage) Reads() []string {
	return []string{KeyWindows.String(), KeyRoute.String(), KeyFactReport.String()}
}
func (AbstainStage) Writes() []string { return []string{KeyAbstain.String()} }

func (s AbstainStage) Run(c *context.Context) error {
	if s.Head == nil {
		return nil
	}
	ws, _ := context.Get(c, KeyWindows)
	route, _ := context.Get(c, KeyRoute)
	rep, _ := context.Get(c, KeyFactReport)
	query, _ := context.Get(c, KeyRewrite)
	top := 0.0
	for _, w := range ws {
		if w.Score > top {
			top = w.Score
		}
	}
	f := abstain.Features{
		QueryLen:     len([]rune(query.Original)),
		Candidates:   len(ws),
		Kept:         len(ws),
		TopScore:     top,
		MissingFacts: len(rep.Missing),
		Confidence:   route.Signals.Confidence,
		Skipped:      len(ws) == 0,
		Refused:      route.Action == "refuse",
	}
	p, act := s.Head.Decide(f)
	reason := ""
	switch act {
	case "refuse":
		reason = "abstain: retrieval features say unanswerable (p_fail)"
	case "deep":
		reason = "abstain: escalation forced by fail predictor"
	}
	return context.Set(c, KeyAbstain, abstain.Verdict{PFail: p, Action: act, Reason: reason})
}

func (AbstainStage) Verify(c *context.Context) error { return nil }

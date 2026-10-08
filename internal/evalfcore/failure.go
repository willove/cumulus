package evalfcore

import (
	"github.com/willove/cumulus/internal/failure"
)

// Verdict 是判官裁决。Tokens 是判官自己的花费——判官不是免费劳动力，
// 它的 token 要进账单（计费诚实：花过钱的调用都记账）。
type Verdict struct {
	OK bool
	// Coverage 是**答案级验证信号**：分点判官量到的"要点命中比例"
	// （0..1）。它不是判词，是可校准的数——检索侧信号（覆盖/边际/死路）
	// 答的是"窗够不够"，这一项答的是"**这句答案有没有证据**"。能不能
	// 撑起拒答阈值，正是研究线 post-answer verification 要验的事。
	// 等义判官不产出它（留 0 = 无此信号，不假装有）。
	Coverage         float64
	Raw              string // 判词原文（校准要看得见模型说了什么，只留解析结果等于盲调）
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
}

// Judge 是判官。离线 stub 与真 LLM 判官都实现它；判官结论不覆盖规则分
// （cumulus 的口径：两套分数并列呈现，谁不替谁说话）。
type Judge interface {
	Judge(question, answer, gold string) (Verdict, error)
}

// ClassifyInput 是失败归类的输入。字段全部来自一次问答的可观测事实，
// 不靠猜。
type ClassifyInput struct {
	Windows             int    // 取到的证据窗口数
	GoldHit             bool   // 金标 id 是否命中
	UnresolvedCitations int    // 不可解析的引用数
	Refused             bool   // 系统是否拒答
	RouteAction         string // fast / escalate / refuse
	RuleScore           float64
	BudgetExhausted     bool
	// 底物信号（BioHarness 的"证据类型错配"，可观测化）：
	AsksNumeric        bool // 问题要的是数值型答案
	EvidenceHasNumeric bool // 窗里有可读的数值（failure.HasNumeric 口径）
	QueryOutOfCorpus   bool // 库与问句词面零共享（内容词一个都不在语料里）：需要外部实体接地
}

// Classify 按固定规则归类。规则只有这几条，判据写死；
// 没有规则匹配时返回未分类（-1）——不硬塞一个类别让人误判。
//
// 归因口径（每一条都对着一个可观测事实，且六类都可达）：
//   - 超限：轮次/窗口打满；
//   - 接地失败：引用核不掉；
//   - 底物错配：要数值却全是叙述文本，或查询的实体语料里根本没有——
//     BioHarness 的判据：这时再加检索是治错病；
//   - 路由误判：有证据（金标在窗内）却拒答 = 过度拒答；或快路硬答而
//     规则分为零 = 该升级没升级；
//   - 召回不足：空手拒答或金标没进候选——拒答本身是诚实出口，但归因
//     要落到"检索没够到"，不是笼统贴给腐烂；
//   - 腐烂：手上有窗、预算没用完就提前给不确定答案（abstain 早弃权那
//     类）。**拒答不再一律算腐烂**——那是把系统的正确出口当失败，会
//     同时污染诊断与学习周期（文法的"拒答是诚实结局"与归因口径别混）。
func Classify(in ClassifyInput) failure.Category {
	switch {
	case in.BudgetExhausted:
		return failure.BudgetExceeded
	case in.UnresolvedCitations > 0:
		return failure.GroundingFail
	case in.AsksNumeric && in.Windows > 0 && !in.EvidenceHasNumeric:
		// 要数值、有窗、窗里没有数：证据类型不对（在叙述文本里找测量值）
		return failure.SubstrateMismatch
	case in.QueryOutOfCorpus:
		// 库与问句**词面零共享**：语料没命名这个问题里的任何内容词，
		// 需要的是实体接地/换证据源，不是再翻文本。**有没有窗都算**。
		// 判据之所以要"零共享"这么窄：松一档（用 OOV 占比）就会把
		// 普通口语问句标成错配——那是词表桥的触发器，不是失败标签。
		return failure.SubstrateMismatch
	case in.Refused && in.GoldHit:
		// 金标就在窗里却拒答 = 过度拒答（路由漏了该走的那条路）
		return failure.RouteError
	case in.Refused && in.Windows == 0:
		// 空手拒答：出口是对的，账要记在召回上
		return failure.RecallMiss
	case in.Refused:
		// 有窗、没命中、预算没用完就放弃 = 提前给不确定答案
		return failure.Rot
	case in.Windows == 0:
		return failure.RecallMiss
	case !in.GoldHit:
		return failure.RecallMiss
	case in.RouteAction == "fast" && in.RuleScore == 0:
		return failure.RouteError
	default:
		return failure.Category(-1)
	}
}

// FormatCategory 渲染类别；未分类显式写出来，不许静默吞掉。
func FormatCategory(c failure.Category) string {
	if c == failure.Category(-1) {
		return "unclassified"
	}
	return c.String()
}

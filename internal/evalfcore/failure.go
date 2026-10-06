package evalfcore

import (
	"github.com/willove/cumulus/internal/failure"
)

// Verdict 是判官裁决。Tokens 是判官自己的花费——判官不是免费劳动力，
// 它的 token 要进账单（计费诚实：花过钱的调用都记账）。
type Verdict struct {
	OK               bool
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
}

// Classify 按固定规则归类。规则只有这几条，判据写死；
// 没有规则匹配时返回未分类（-1）——不硬塞一个类别让人误判。
// 拒答归类为 Rot：文法里拒答是诚实出口，但在评测归因里它仍是
// “没答上”的一种，单列出来由人复核这次拒答应不应该。
func Classify(in ClassifyInput) failure.Category {
	switch {
	case in.BudgetExhausted:
		return failure.BudgetExceeded
	case in.UnresolvedCitations > 0:
		return failure.GroundingFail
	case in.Refused:
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

// Package failure 是失败模式六分类。评测 capture 时按此归因，
// 学习周期的诊断阶段只允许用这六类说话（流程文法 §三）。
package failure

// Category 是一类失败。判据与修复方向见 docs/flow-grammar.md §三。
type Category int

const (
	// SubstrateMismatch 底物错配：要的证据类型和用的检索类型不是一回事。
	SubstrateMismatch Category = iota
	// RecallMiss 召回不足：正确证据没进候选。
	RecallMiss
	// GroundingFail 接地失败：答案词在证据窗口里对不上。
	GroundingFail
	// RouteError 路由误判：该升级没升级，或不必要地升级了。
	RouteError
	// Rot 腐烂：长任务中直接放弃或提前给不确定答案。
	Rot
	// BudgetExceeded 超限：窗口或轮次打满。
	BudgetExceeded
)

var names = map[Category]string{
	SubstrateMismatch: "substrate-mismatch",
	RecallMiss:        "recall-miss",
	GroundingFail:     "grounding-fail",
	RouteError:        "route-error",
	Rot:               "rot",
	BudgetExceeded:    "budget-exceeded",
}

func (c Category) String() string {
	if s, ok := names[c]; ok {
		return s
	}
	return "unknown"
}

// All 返回全部类别，供评测面板枚举。
func All() []Category {
	return []Category{SubstrateMismatch, RecallMiss, GroundingFail, RouteError, Rot, BudgetExceeded}
}

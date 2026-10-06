package learncore

import "fmt"

// Baseline 是护栏基线：一组率 + 它对应的冻结指纹。
// 没有基线的周期不许跑（cumulus 的“无护栏基线拒跑”，照搬）。
type Baseline struct {
	EvidenceHitRate float64
	CitationsOKRate float64
}

// Guardrail 是护栏：候选的率不得低于地板。
//
// 率地板，不是逐题 slack：cumulus 的教训——“1 题 slack”会随切片大小
// 改含义（10 题掉 1 题是 10%，1000 题掉 1 题是 0.1%），地板不会。
type Guardrail struct {
	EvidenceHitFloor float64
	CitationsOKFloor float64
}

// Check 对比候选与基线。全过返回 ok；不过返回逐条理由（不许只说“失败”）。
func (g Guardrail) Check(cand Baseline) (bool, []string) {
	var reasons []string
	if cand.EvidenceHitRate < g.EvidenceHitFloor {
		reasons = append(reasons, fmt.Sprintf("evidence_hit rate %.3f below floor %.3f", cand.EvidenceHitRate, g.EvidenceHitFloor))
	}
	if cand.CitationsOKRate < g.CitationsOKFloor {
		reasons = append(reasons, fmt.Sprintf("citations_ok rate %.3f below floor %.3f", cand.CitationsOKRate, g.CitationsOKFloor))
	}
	return len(reasons) == 0, reasons
}

// FloorsFromBaseline 用基线设地板：候选不得比基线差（率口径，零容差）。
// 需要容差时由调用方显式下调，不默认给——宽容必须是决定，不是默认值。
func FloorsFromBaseline(b Baseline) Guardrail {
	return Guardrail{EvidenceHitFloor: b.EvidenceHitRate, CitationsOKFloor: b.CitationsOKRate}
}

// IsZero 空基线判定：空基线的周期拒跑。
func (b Baseline) IsZero() bool { return b.EvidenceHitRate == 0 && b.CitationsOKRate == 0 }

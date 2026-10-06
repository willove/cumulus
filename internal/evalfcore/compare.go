package evalfcore

import "fmt"

// Compare 只在三指纹全同时给差值；否则只给理由，不给差值
// （cumulus 的口径：不可比时列原因，绝不摆数字）。
func Compare(a, b RunState) (map[string]float64, []string) {
	reasons := a.Fingerprints.DiffReasons(b.Fingerprints)
	if len(reasons) > 0 {
		return nil, reasons
	}
	return map[string]float64{
		"rule_avg":       Summarize(a).RuleAvg - Summarize(b).RuleAvg,
		"evidence_hit":   Summarize(a).EvidenceHitRate - Summarize(b).EvidenceHitRate,
		"citations_ok":   Summarize(a).CitationsOKRate - Summarize(b).CitationsOKRate,
		"judge_acc":      judgeAccDelta(Summarize(a), Summarize(b)),
		"avg_latency_ms": float64(Summarize(a).AvgLatencyMS - Summarize(b).AvgLatencyMS),
	}, nil
}

// judgeAccDelta 未判分（N/A）时的差值约定为 0 并在调用方以 JudgeN 区分——
// N/A 不是 0，差值只在两侧都判了分时才有意义。
func judgeAccDelta(a, b Summary) float64 {
	if a.JudgeN == 0 || b.JudgeN == 0 {
		return 0
	}
	return a.JudgeAcc - b.JudgeAcc
}

// Summary 是聚合指标。JudgeN/JudgeAcc 分开：N/A 与 0 是两件事。
type Summary struct {
	ItemsDone             int
	RuleAvg               float64
	EvidenceHitRate       float64
	CitationsOKRate       float64
	JudgeN                int // 判了分的题数
	JudgeAcc              float64
	AvgLatencyMS          int64
	TotalPromptTokens     int
	TotalCompletionTokens int
	CostUnknownItems      int // 上游不报 usage 的题数（计费诚实）
}

// Summarize 聚合一次运行。
func Summarize(s RunState) Summary {
	out := Summary{ItemsDone: len(s.Results)}
	if len(s.Results) == 0 {
		return out
	}
	var ruleSum float64
	var citeOK int
	var latencySum int64
	for _, r := range s.Results {
		ruleSum += r.RuleScore
		if r.EvidenceHit {
			out.EvidenceHitRate += 1
		}
		if r.CitationsTotal > 0 && r.CitationsResolved == r.CitationsTotal {
			citeOK++
		}
		if r.JudgeOK != nil {
			out.JudgeN++
			if *r.JudgeOK {
				out.JudgeAcc += 1
			}
		}
		latencySum += r.LatencyMS
		out.TotalPromptTokens += r.PromptTokens
		out.TotalCompletionTokens += r.CompletionTokens
		if !r.CostKnown {
			out.CostUnknownItems++
		}
	}
	n := float64(len(s.Results))
	out.RuleAvg = ruleSum / n
	out.EvidenceHitRate /= n
	out.CitationsOKRate = float64(citeOK) / n
	out.AvgLatencyMS = latencySum / int64(len(s.Results))
	if out.JudgeN > 0 {
		out.JudgeAcc /= float64(out.JudgeN)
	}
	return out
}

// String 渲染一行摘要（ CLI 与 tests 用）。JudgeN=0 时判官列显示 N/A。
func (s Summary) String() string {
	judge := "N/A"
	if s.JudgeN > 0 {
		judge = fmt.Sprintf("%.1f%% (n=%d)", s.JudgeAcc*100, s.JudgeN)
	}
	return fmt.Sprintf(
		"items=%d rule=%.1f%% evidence=%.1f%% citations=%.1f%% judge=%s latency=%dms tokens(p/c)=%d/%d cost_unknown=%d",
		s.ItemsDone, s.RuleAvg*100, s.EvidenceHitRate*100, s.CitationsOKRate*100,
		judge, s.AvgLatencyMS, s.TotalPromptTokens, s.TotalCompletionTokens, s.CostUnknownItems,
	)
}

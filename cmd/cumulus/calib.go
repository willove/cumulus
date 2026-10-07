package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/calib"
	"github.com/willove/cumulus/internal/evalfcore"
)

// printCalib 打校准层的判决：PAV 压平后的曲线 + 带有限样本修正的拒答
// 阈值（internal/calib）。α 由 CUMULUS_ALPHA 给（默认 0.10）。
//
// 为什么要单独打：分桶表看的是"可靠性"，这里是"能不能下注"——阈值、
// 已答规模、上界、以及被跳过的题数（拒答/判官出错不参与校准）。
func printCalib(state evalfcore.RunState) {
	alpha := 0.10
	if v := os.Getenv("CUMULUS_ALPHA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			alpha = f
		}
	}
	samples, skipped := calib.SampleFromResults(state.Results)
	rep := calib.Build(samples, skipped, alpha, 4)
	oracles := "none"
	if len(rep.Oracles) > 0 {
		parts := make([]string, 0, len(rep.Oracles))
		for _, k := range []string{calib.OracleJudge, calib.OracleEvidence} {
			if rep.Oracles[k] > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", k, rep.Oracles[k]))
			}
		}
		oracles = strings.Join(parts, " ")
	}
	fmt.Printf("calib[%s] samples=%d skipped=%d oracles(%s) raw-monotone=%v \n",
		state.Arm, rep.Samples, rep.Skipped, oracles, rep.BucketMono)
	if !rep.Satisfiable {
		fmt.Printf("  α=%.2f 无可行阈值 → 全拒答（置信代理在这批数据上不承载承诺；先把特征改好）\n", alpha)
		return
	}
	fmt.Printf("  α=%.2f → 拒答阈值 τ=%.3f：答 %d/%d 题、风险 %.1f%%、修正上界 %.1f%%（单调包络）\n",
		alpha, rep.Threshold, rep.Answered, rep.Samples, rep.AnsweredRisk*100, rep.Bound*100)
	// 曲线只打**有样本量支撑**的段：置信度是连续量时会出现大量 n=1 的段
	// （一条题一个块），全打出来等于刷屏，而 n=1 的段本来就不该被当作
	// 证据。被折叠的段数与样本数一起报，避免"看起来只有 6 段"的错觉。
	if len(rep.Curve.Knots) > 0 {
		parts, collapsed, shown := make([]string, 0, len(rep.Curve.Knots)), 0, 0
		for _, k := range rep.Curve.Knots {
			if k.N < 5 {
				collapsed += k.N
				continue
			}
			parts = append(parts, fmt.Sprintf("[%.2f,%.2f)=%.2f(n=%d)", k.Lo, k.Hi, k.Value, k.N))
			shown += k.N
		}
		fmt.Printf("  PAV 曲线（n≥5 的段，覆盖 %d/%d 样本，折叠 %d 条）：%s\n",
			shown, rep.Samples, collapsed, strings.Join(parts, " "))
	}
}

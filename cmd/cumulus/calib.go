package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/calib"
	"github.com/willove/cumulus/internal/evalfcore"
)

// alphaFromEnv 读目标错误率上限（CUMULUS_ALPHA，默认 0.10）。
func alphaFromEnv() float64 {
	if v := os.Getenv("CUMULUS_ALPHA"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			return f
		}
	}
	return 0.10
}

// signalCandidate 是一个候选置信信号：名字 + 取值函数。
type signalCandidate struct {
	name  string
	value func(evalfcore.ItemResult) float64
}

// printCalibContest 在**同一批结果**上比较候选信号：单调性、α 可行性、
// 阈值与已答覆盖率。校准的下一步是"换信号"而不是"调阈值"——只有复合
// 置信度一个值时，"阈值不合适"没有下一步可走。
//
// 共同口径：所有候选共用同一批标签（拒答/判官出错/评测出错都不进），
// 换信号不换事实。
func printCalibContest(state evalfcore.RunState, alpha float64) {
	cands := []signalCandidate{
		{"confidence", func(r evalfcore.ItemResult) float64 { return r.Confidence }},
		{"coverage", func(r evalfcore.ItemResult) float64 { return r.Coverage }},
		{"margin", func(r evalfcore.ItemResult) float64 { return r.Margin }},
		{"support", func(r evalfcore.ItemResult) float64 { return r.Support }},
		{"margin*support", func(r evalfcore.ItemResult) float64 { return r.Margin * r.Support }},
	}
	alphas := []float64{alpha, 0.20, 0.30}
	if v := os.Getenv("CUMULUS_ALPHAS"); v != "" {
		alphas = alphas[:0]
		for _, part := range strings.Split(v, ",") {
			if f, err := strconv.ParseFloat(strings.TrimSpace(part), 64); err == nil && f > 0 && f < 1 {
				alphas = append(alphas, f)
			}
		}
		if len(alphas) == 0 {
			alphas = []float64{alpha}
		}
	}
	fmt.Printf("signal contest[%s]（同一批标签，换信号不换事实；格 = 敢答题数/该集合风险）\n", state.Arm)
	head := fmt.Sprintf("  %-16s %6s %9s", "signal", "n", "monotone")
	for _, a := range alphas {
		head += fmt.Sprintf(" %14s", fmt.Sprintf("α=%.2f", a))
	}
	fmt.Println(head)
	for _, c := range cands {
		var samples []calib.Sample
		skipped := 0
		for _, r := range state.Results {
			s, ok := calib.SampleOf(r, c.value(r))
			if !ok {
				skipped++
				continue
			}
			samples = append(samples, s)
		}
		base := calib.Build(samples, skipped, alphas[0], 4)
		row := fmt.Sprintf("  %-16s %6d %9v", c.name, base.Samples, base.BucketMono)
		for _, a := range alphas {
			rep := base
			if a != alphas[0] {
				rep = calib.Build(samples, skipped, a, 4)
			}
			if !rep.Satisfiable {
				row += fmt.Sprintf(" %14s", "—")
				continue
			}
			row += fmt.Sprintf(" %14s", fmt.Sprintf("%d/%.0f%%", rep.Answered, rep.AnsweredRisk*100))
		}
		fmt.Println(row)
	}
}

// printLockbox 是锁箱复验：用**校准集定下的阈值**（CUMULUS_LOCKBOX_TAU）
// 评估本次运行（锁箱集），如实报它兑现没兑现。锁箱不接受重新挑阈值——
// 在锁箱上重挑等于用锁箱调参，锁箱就废了。
func printLockbox(state evalfcore.RunState) {
	raw := os.Getenv("CUMULUS_LOCKBOX_TAU")
	if raw == "" {
		return
	}
	tau, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fmt.Printf("lockbox: CUMULUS_LOCKBOX_TAU 不是数：%q\n", raw)
		return
	}
	alpha := alphaFromEnv()
	var samples []calib.Sample
	skipped := 0
	for _, r := range state.Results {
		s, ok := calib.SampleOf(r, r.Confidence)
		if !ok {
			skipped++
			continue
		}
		samples = append(samples, s)
	}
	rep := calib.Evaluate(tau, samples, alpha)
	verdict := "**通过**"
	if !rep.Satisfiable {
		verdict = "**未兑现**"
	}
	fmt.Printf("lockbox[%s] τ=%.3f（来自校准集）α=%.2f → 答 %d/%d 题、错误率 %.1f%%、修正上界 %.1f%% %s（跳过 %d）\n",
		state.Arm, tau, alpha, rep.Answered, rep.Samples, rep.AnsweredRisk*100, rep.Bound*100, verdict, skipped)
}

// printCalib 打校准层的判决：PAV 压平后的曲线 + 带有限样本修正的拒答
// 阈值（internal/calib）。α 由 CUMULUS_ALPHA 给（默认 0.10）。
//
// 为什么要单独打：分桶表看的是"可靠性"，这里是"能不能下注"——阈值、
// 已答规模、上界、以及被跳过的题数（拒答/判官出错不参与校准）。
func printCalib(state evalfcore.RunState) {
	alpha := alphaFromEnv()
	// 双神谕并列：检索侧承诺（证据命中，确定性）与答案侧承诺（判官，受
	// 金标形态影响）读数可以差几十个百分点——只报一个就是把口径当事实。
	oracles := []string{calib.OracleJudgeOpt, calib.OracleEvidenceOpt}
	if v := os.Getenv("CUMULUS_ORACLE"); v != "" {
		oracles = []string{v}
	}
	for _, oracle := range oracles {
		printCalibFor(state, oracle, alpha)
	}
}

func printCalibFor(state evalfcore.RunState, oracle string, alpha float64) {
	var samples []calib.Sample
	skipped := 0
	for _, r := range state.Results {
		s, ok := calib.SampleOfOracle(r, r.Confidence, oracle)
		if !ok {
			skipped++
			continue
		}
		samples = append(samples, s)
	}
	rep := calib.Build(samples, skipped, alpha, 4)
	fmt.Printf("calib[%s] oracle=%s samples=%d skipped=%d raw-monotone=%v\n",
		state.Arm, oracle, rep.Samples, rep.Skipped, rep.BucketMono)
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

package calib

import (
	"math"
	"testing"

	"github.com/willove/cumulus/internal/evalfcore"
)

// PAV 只压违例：升序看 0.7(F)=0 / 0.8(T)=1 / 0.9(F)=0——最后一个块
// (0) 低于前一均值 (1) 要合并成 0.5，而 0.5 ≥ 0 合法，于是两段：
// [0.7,0.8) → 0，[0.8,∞) → 0.5。完全合并是错的（那会把合法的低段
// 也拉高，凭空抬高没把握那一段的准确率）。
func TestFitPoolsViolators(t *testing.T) {
	samples := []Sample{
		{Confidence: 0.9, Correct: false},
		{Confidence: 0.8, Correct: true},
		{Confidence: 0.7, Correct: false},
	}
	c := Fit(samples)
	if len(c.Knots) != 2 {
		t.Fatalf("want two blocks after pooling the violation, got %d: %+v", len(c.Knots), c.Knots)
	}
	if math.Abs(c.Knots[0].Value-0) > 1e-9 || c.Knots[0].N != 1 {
		t.Fatalf("low block wrong: %+v", c.Knots[0])
	}
	if math.Abs(c.Knots[1].Value-0.5) > 1e-9 || c.Knots[1].N != 2 {
		t.Fatalf("pooled block wrong: %+v", c.Knots[1])
	}
}

func TestFitIsMonotoneRegardlessOfInputOrder(t *testing.T) {
	samples := []Sample{
		{Confidence: 0.2, Correct: false}, {Confidence: 0.3, Correct: true},
		{Confidence: 0.4, Correct: false}, {Confidence: 0.5, Correct: true},
		{Confidence: 0.6, Correct: true}, {Confidence: 0.7, Correct: false},
		{Confidence: 0.8, Correct: true}, {Confidence: 0.9, Correct: true},
	}
	shuffled := []Sample{samples[7], samples[0], samples[3], samples[5], samples[1], samples[6], samples[2], samples[4]}
	a, b := Fit(samples), Fit(shuffled)
	if len(a.Knots) != len(b.Knots) {
		t.Fatalf("fit must not depend on input order: %d vs %d", len(a.Knots), len(b.Knots))
	}
	prev := math.Inf(-1)
	for i, k := range a.Knots {
		if k.Value < prev-1e-12 {
			t.Fatalf("curve not monotone at %d: %+v", i, a.Knots)
		}
		prev = k.Value
		if math.Abs(k.Value-b.Knots[i].Value) > 1e-12 {
			t.Fatalf("knot %d differs by order: %v vs %v", i, k.Value, b.Knots[i].Value)
		}
	}
}

// 边界外取端值：没把握不等于一定错（Apply 不会返回 0）。
func TestApplyClampsToEdgeValues(t *testing.T) {
	samples := []Sample{
		{Confidence: 0.4, Correct: false},
		{Confidence: 0.6, Correct: true},
		{Confidence: 0.6, Correct: false},
		{Confidence: 0.9, Correct: true},
	}
	c := Fit(samples)
	if got := c.Apply(0.0); math.Abs(got-0.0) > 1e-9 {
		t.Fatalf("low edge must take the leftmost block value, got %v", got)
	}
	if got := c.Apply(1.0); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("high edge must take the rightmost block value, got %v", got)
	}
	if got := c.Apply(0.7); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("0.7 falls in the pooled 0.6 block (1/2), got %v", got)
	}
}

// 阈值要能被 α 认账：置信度 ≥0.5 全对、<0.5 全错时，τ 应落在 0.5 附近，
// 已答集合错误率为 0，并且满足有限样本修正。
func TestBuildFindsSafeThreshold(t *testing.T) {
	var samples []Sample
	for i := 0; i < 100; i++ {
		conf := float64(i) / 100
		samples = append(samples, Sample{Confidence: conf, Correct: conf >= 0.5, Oracle: OracleEvidence})
	}
	rep := Build(samples, 0, 0.10, 4)
	if !rep.Satisfiable {
		t.Fatalf("threshold must exist: %+v", rep)
	}
	// α=0.10 允许少量错误，所以阈值会**低于**准确率悬崖（0.5）：0.46 处
	// 答 54 题、错 4 条、风险 0.074、修正后上界 0.091 ≤ 0.10；再低一档
	// (0.45) 上界 0.107 就越界了。这正是 α 的含义——不是"零错误"。
	if rep.Threshold > 0.5 || rep.Threshold < 0.45 {
		t.Fatalf("threshold must be the smallest feasible one, got %v", rep.Threshold)
	}
	if rep.AnsweredRisk > 0.10 || rep.Bound > 0.10 {
		t.Fatalf("answered risk must respect alpha: risk=%v bound=%v", rep.AnsweredRisk, rep.Bound)
	}
	if rep.Answered < 51 {
		t.Fatalf("alpha=0.10 must buy more than the zero-error threshold: answered=%d", rep.Answered)
	}
}

// 置信度与对错无关（纯噪声）时不允许许愿：或者不可满足，或者阈值很高
// （已答集合小到修正项还兜得住）。
func TestBuildRefusesToPromiseOnNoise(t *testing.T) {
	var samples []Sample
	for i := 0; i < 200; i++ {
		conf := float64(i%20) / 20
		samples = append(samples, Sample{Confidence: conf, Correct: i%2 == 0})
	}
	rep := Build(samples, 0, 0.05, 4)
	if rep.Satisfiable {
		// 若声称可满足，就必须真的满足（包络 + 修正都算上）
		if rep.Bound > 0.05 {
			t.Fatalf("claimed satisfiable but bound violates alpha: %+v", rep)
		}
		if rep.Answered > 60 {
			t.Fatalf("noise must not allow answering most of the set: answered=%d", rep.Answered)
		}
		return
	}
	if rep.Threshold != 1 {
		t.Fatalf("unsatisfiable must mean refuse-all, got threshold %v", rep.Threshold)
	}
}

// 校准只用有效样本：拒答 / 判官出错 / 评测出错 / 无置信记录都要跳过，并在
// 神谕计数里如实分类。
func TestSampleFromResultsSkipsInvalidAndCountsOracles(t *testing.T) {
	ok := true
	bad := false
	results := []evalfcore.ItemResult{
		{ItemID: "judged-ok", Confidence: 0.8, JudgeOK: &ok, EvidenceHit: false},
		{ItemID: "judged-bad", Confidence: 0.6, JudgeOK: &bad},
		{ItemID: "judge-err", Confidence: 0.9, JudgeErr: "timeout", EvidenceHit: true},
		{ItemID: "refused", Confidence: 0.4, Refused: true, EvidenceHit: true},
		{ItemID: "eval-error", Confidence: 0.7, Failure: "eval-error"},
		{ItemID: "no-conf", Confidence: 0, EvidenceHit: true},
		{ItemID: "evidence-only", Confidence: 0.5, EvidenceHit: true},
	}
	samples, skipped := SampleFromResults(results)
	if len(samples) != 3 || skipped != 4 {
		t.Fatalf("want 3 valid / 4 skipped, got %d / %d", len(samples), skipped)
	}
	rep := Build(samples, skipped, 0.10, 4)
	if rep.Oracles[OracleJudge] != 2 || rep.Oracles[OracleEvidence] != 1 {
		t.Fatalf("oracles must be counted separately: %+v", rep.Oracles)
	}
	if rep.Skipped != 4 {
		t.Fatalf("skipped must be visible in the report, got %d", rep.Skipped)
	}
}

// 锁箱复验：校准集的阈值搬到锁箱上，风险还兜得住就不算漂移；锁箱上
// 置信度与对错脱钩时必须报漂移（阈值不能带着校准集的记忆上线）。
func TestCompareSamplesDetectsDrift(t *testing.T) {
	var fit []Sample
	for i := 0; i < 100; i++ {
		conf := float64(i) / 100
		fit = append(fit, Sample{Confidence: conf, Correct: conf >= 0.5, Oracle: OracleEvidence})
	}
	rep := Build(fit, 0, 0.10, 4)

	var same []Sample
	for i := 0; i < 40; i++ {
		conf := 0.5 + float64(i)/80
		same = append(same, Sample{Confidence: conf, Correct: true})
	}
	d := CompareSamples(rep, same)
	if d.Drifted {
		t.Fatalf("a more conservative lockbox is not drift: %+v", d)
	}
	if !d.Improved {
		t.Fatalf("lockbox much better than calibration must be flagged Improved: %+v", d)
	}

	var broken []Sample
	for i := 0; i < 40; i++ {
		conf := 0.5 + float64(i)/80
		broken = append(broken, Sample{Confidence: conf, Correct: i%3 != 0})
	}
	db := CompareSamples(rep, broken)
	if !db.Drifted || db.LockboxRisk <= rep.Alpha {
		t.Fatalf("lockbox violating alpha must be reported: %+v", db)
	}
}

func TestBuildEmptySamplesRefusesAll(t *testing.T) {
	rep := Build(nil, 7, 0.10, 4)
	if rep.Threshold != 1 || !rep.Satisfiable || rep.Samples != 0 || rep.Skipped != 7 {
		t.Fatalf("empty calibration must refuse all and say so: %+v", rep)
	}
	if len(rep.Curve.Knots) != 0 {
		t.Fatalf("empty curve must have no knots: %+v", rep.Curve)
	}
}

// 分桶单调性检查看的是**原始**桶（不压平）——压平后的曲线必然单调，
// 用它检查等于自证。
func TestBucketMonotoneFlagReflectsRawNoise(t *testing.T) {
	var samples []Sample
	// 低桶准确率高、高桶准确率低：明显非单调
	for i := 0; i < 40; i++ {
		conf := float64(i%4)/4 + 0.05
		correct := i%4 < 2
		samples = append(samples, Sample{Confidence: conf, Correct: correct})
	}
	rep := Build(samples, 0, 0.10, 4)
	if rep.BucketMono {
		t.Fatalf("raw buckets are not monotone here: %+v", rep.Buckets)
	}
	prev := math.Inf(-1)
	for _, k := range rep.Curve.Knots {
		if k.Value < prev-1e-12 {
			t.Fatalf("fitted curve must still be monotone: %+v", rep.Curve.Knots)
		}
		prev = k.Value
	}
}

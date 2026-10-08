// Package calib 是置信度的校准层：把"置信代理"变成"可下注的准确率"，
// 并给出带有限样本修正的拒答阈值。
//
// 三件东西，各有出处：
//
//   - **PAV 保序回归（isotonic）**：把分桶的非单调噪声压成单调曲线。
//     v0.2 §2.1 的可靠性要求；DuReader hard 上复合置信度实测非单调
//     （0.56 / 0.61 / 0.57 / 0.73），不压平就谈不上阈值——非单调的
//     阈值只是把噪声写成了数字。
//   - **前缀风险包络（A-CRC-QA）**：按阈值从高到低扫，已答集合的经验
//     错误率取**单调包络**（对更大阈值也成立），再加 (1−α)/(n+1) 的
//     有限样本修正。取"包络上界仍 ≤ α"的最小阈值。修正项不是装饰：
//     样本小时经验错误率天生乐观，没有它就是在小样本上许大愿。
//   - **锁箱对照**：校准集拟合、锁箱集复验（同一语料指纹、不同题集
//     指纹）。纪律：锁箱只看一次，看完就换一份——反复看等于没有锁箱。
//
// 校准只用**有效样本**：拒答、判官没判上分、评测自身出错的题都不进
// 曲线（它们不是"答错了"，是"没有可校准的答案"）。神谕优先判官、其次
// 证据命中，并且**如实分类计数**——判官不是唯一神谕，"多少个结论来自
// 判官、多少个来自证据命中"必须写在报告里。
//
// 包不碰存储、不碰流程：输入是逐题结果，输出是曲线与判决。持久化与
// 版本引用是调用方的事（阈值版本进提交视图）。
package calib

import (
	"fmt"
	"math"
	"sort"

	"github.com/willove/cumulus/internal/evalfcore"
)

// Sample 是一条可校准样本：置信代理 + 事实上的对错 + 神谕来源。
type Sample struct {
	Confidence float64
	Correct    bool
	Oracle     string // "judge" / "evidence"：结论是谁给的
}

// 神谕常量（报告里按它计数）。
const (
	OracleJudge    = "judge"
	OracleEvidence = "evidence"
)

// Label 是一条样本的**事实**（对错 + 神谕来源），与用哪个信号无关。
// 信号竞赛必须共用同一批标签：换信号不许换答案口径，否则比的是题目不是信号。
type Label struct {
	Correct bool
	Oracle  string
}

// LabelOf 给一道题定标签。第二个返回值为假 = 这道题**不进任何校准**
// （拒答 / 判官出错 / 评测出错）：它不是"答错"，是没有可校准的答案。
func LabelOf(r evalfcore.ItemResult) (Label, bool) {
	if r.Refused || r.Failure == "eval-error" || r.JudgeErr != "" {
		return Label{}, false
	}
	if r.JudgeOK != nil {
		return Label{Correct: *r.JudgeOK, Oracle: OracleJudge}, true
	}
	return Label{Correct: r.EvidenceHit, Oracle: OracleEvidence}, true
}

// SampleOf 用给定的信号值 + 共同标签造样本（信号竞赛用）。
func SampleOf(r evalfcore.ItemResult, signal float64) (Sample, bool) {
	lab, ok := LabelOf(r)
	if !ok {
		return Sample{}, false
	}
	return Sample{Confidence: signal, Correct: lab.Correct, Oracle: lab.Oracle}, true
}

// 神谕选择（校准用哪个事实口径）。判官不是唯一神谕：检索侧信号要承诺的
// 是"找到没找到"（证据命中，确定性），答案侧信号要承诺的是"答对没答对"
// （判官，受金标形态影响）。两个口径的读数可以差几十个百分点——并列报，
// 不许拿一个冒充另一个。
const (
	OracleAny         = "any"      // 有判官用判官，否则证据命中
	OracleJudgeOpt    = "judge"    // 只认判官判过的题
	OracleEvidenceOpt = "evidence" // 只认证据命中（确定性，不看判官）
)

// SampleOfOracle 按指定神谕造样本。valid=false 表示这道题在该口径下没有
// 可用事实（拒答、判官没判上分、评测出错），必须排除而不是当成"答错"。
func SampleOfOracle(r evalfcore.ItemResult, signal float64, oracle string) (Sample, bool) {
	if r.Refused || r.Failure == "eval-error" {
		return Sample{}, false
	}
	switch oracle {
	case OracleEvidenceOpt:
		return Sample{Confidence: signal, Correct: r.EvidenceHit, Oracle: OracleEvidence}, true
	case OracleJudgeOpt:
		if r.JudgeOK == nil || r.JudgeErr != "" {
			return Sample{}, false
		}
		return Sample{Confidence: signal, Correct: *r.JudgeOK, Oracle: OracleJudge}, true
	default:
		return SampleOf(r, signal)
	}
}

// SampleFromResults 把逐题结果转成可校准样本。第二个返回值是**被跳过**
// 的题数（拒答 / 判官出错 / 评测出错 / 无置信记录）——跳过多少必须可见，
// 否则"校准集只有 40 条"这种事实会消失在平均值里。
func SampleFromResults(results []evalfcore.ItemResult) ([]Sample, int) {
	samples := make([]Sample, 0, len(results))
	skipped := 0
	for _, r := range results {
		if r.Refused || r.Failure == "eval-error" || r.JudgeErr != "" || r.Confidence <= 0 {
			skipped++
			continue
		}
		if r.JudgeOK != nil {
			samples = append(samples, Sample{Confidence: r.Confidence, Correct: *r.JudgeOK, Oracle: OracleJudge})
			continue
		}
		samples = append(samples, Sample{Confidence: r.Confidence, Correct: r.EvidenceHit, Oracle: OracleEvidence})
	}
	return samples, skipped
}

// Knot 是曲线的一段：置信区间 [Lo,Hi) 上取常值 Value（PAV 的块均值）。
type Knot struct {
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
	N     int     `json:"n"`
	Value float64 `json:"value"`
}

// Curve 是保序校准曲线：置信度 → 经验准确率（单调不降）。
type Curve struct {
	Knots []Knot `json:"knots"`
}

// Apply 查曲线。边界外取端值（低于最小置信度 → 最左块的准确率，不是 0
// ——"没有把握"不等于"一定错"，这一点会直接影响阈值扫动的结果）。
func (c Curve) Apply(conf float64) float64 {
	if len(c.Knots) == 0 {
		return 0
	}
	if conf <= c.Knots[0].Lo {
		return c.Knots[0].Value
	}
	for _, k := range c.Knots {
		if conf < k.Hi {
			return k.Value
		}
	}
	return c.Knots[len(c.Knots)-1].Value
}

// Fit 用 PAV（池相邻违例）拟合保序曲线：按置信度升序分块，前块均值大于
// 后块均值就合并，直到整条序列单调不降。同样本同结果 → 同曲线（排序 +
// 稳定合并），与输入顺序无关。
func Fit(samples []Sample) Curve {
	if len(samples) == 0 {
		return Curve{}
	}
	sorted := append([]Sample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Confidence < sorted[j].Confidence })

	type block struct {
		lo, hi float64
		n      int
		ok     int
	}
	blocks := make([]block, 0, len(sorted))
	for _, s := range sorted {
		b := block{lo: s.Confidence, hi: s.Confidence, n: 1}
		if s.Correct {
			b.ok = 1
		}
		blocks = append(blocks, b)
		// 合并所有违例（均值下降）的相邻块
		for len(blocks) >= 2 {
			a, c := blocks[len(blocks)-2], blocks[len(blocks)-1]
			if float64(a.ok)/float64(a.n) <= float64(c.ok)/float64(c.n) {
				break
			}
			merged := block{lo: a.lo, hi: c.hi, n: a.n + c.n, ok: a.ok + c.ok}
			blocks = append(blocks[:len(blocks)-2], merged)
		}
	}
	knots := make([]Knot, 0, len(blocks))
	for i, b := range blocks {
		hi := b.hi
		if i+1 < len(blocks) {
			hi = blocks[i+1].lo // 段上界取下一段起点：查表区间无缝
		} else {
			hi = math.Inf(1)
		}
		knots = append(knots, Knot{Lo: b.lo, Hi: hi, N: b.n, Value: float64(b.ok) / float64(b.n)})
	}
	return Curve{Knots: knots}
}

// Point 是包络上的一点：阈值为 τ 时（答 conf ≥ τ）的已答规模与风险。
type Point struct {
	Threshold float64 `json:"threshold"`
	Answered  int     `json:"answered"`
	Errors    int     `json:"errors"`
	Risk      float64 `json:"risk"`     // 已答集合的经验错误率
	Envelope  float64 `json:"envelope"` // 对更大阈值也成立的单调包络
	Bound     float64 `json:"bound"`    // 包络 + (1−α)/(n+1)
	Feasible  bool    `json:"feasible"` // Bound ≤ α
}

// Report 是一次校准的完整判决面。
type Report struct {
	Samples      int            `json:"samples"`
	Skipped      int            `json:"skipped"`
	Oracles      map[string]int `json:"oracles"`
	Buckets      []Knot         `json:"buckets"` // 原始分桶（未压平）
	BucketMono   bool           `json:"bucket_monotone"`
	Curve        Curve          `json:"curve"`     // PAV 压平后的曲线
	Alpha        float64        `json:"alpha"`     // 目标错误率上界
	Threshold    float64        `json:"threshold"` // 拒答阈值（answer iff conf ≥ τ）
	Answered     int            `json:"answered"`
	AnsweredRisk float64        `json:"answered_risk"`
	Bound        float64        `json:"bound"`
	Satisfiable  bool           `json:"satisfiable"` // 是否存在满足 α 的阈值
	Envelope     []Point        `json:"envelope,omitempty"`
}

// Build 拟合曲线并扫出拒答阈值。buckets 是分桶数（<=0 用 4）。
//
// 判决规则：从高阈值往低扫，维护风险包络（前缀最大），取"包络 + (1−α)/
// (n+1) ≤ α"的**最小**阈值——比它更高的阈值因此也都满足（包络保证）。
// 一个都不满足时 Satisfiable=false、Threshold=1（全拒答：宁可不答，
// 不假装能答）。
func Build(samples []Sample, skipped int, alpha float64, buckets int) Report {
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.10
	}
	if buckets <= 0 {
		buckets = 4
	}
	rep := Report{Samples: len(samples), Skipped: skipped, Alpha: alpha, Oracles: map[string]int{}}
	for _, s := range samples {
		rep.Oracles[s.Oracle]++
	}
	rep.Curve = Fit(samples)
	rep.Buckets = bucketize(samples, buckets)
	rep.BucketMono = monotone(rep.Buckets)
	if len(samples) == 0 {
		rep.Threshold = 1
		rep.Satisfiable = true // 没有样本就没有可下注的准确率：全拒答
		return rep
	}

	// 阈值候选：所有出现过的置信度（降序扫）
	conf := make([]float64, 0, len(samples))
	for _, s := range samples {
		conf = append(conf, s.Confidence)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(conf)))
	uniq := conf[:0]
	for i, v := range conf {
		if i == 0 || v != conf[i-1] {
			uniq = append(uniq, v)
		}
	}

	// 候选阈值只认"已答 ≥ minAnswers"的档：在 3 题上算风险是噪声，不是
	// 承诺。真跑踩过——easy 档整体 93.9%，最高档只有 1 题且答错，风险包络
	// 被钉在 1.0，于是**所有 α 都判不可行**；而答满 214 题的实际风险只有
	// 6%。包络本身要保留（它保证"置信度更高的人不会更差"），但必须建立在
	// 有样本量的档上。
	minAnswers := 20
	if len(samples) < 2*minAnswers {
		minAnswers = 1
	}
	envelope, best := 0.0, math.Inf(1)
	for _, tau := range uniq {
		answered, errors := 0, 0
		for _, s := range samples {
			if s.Confidence < tau {
				continue
			}
			answered++
			if !s.Correct {
				errors++
			}
		}
		if answered < minAnswers {
			continue // 档太小：不算风险，也不进包络
		}
		risk := float64(errors) / float64(answered)
		if risk > envelope {
			envelope = risk
		}
		bound := envelope + (1-alpha)/float64(answered+1)
		feasible := bound <= alpha
		rep.Envelope = append(rep.Envelope, Point{
			Threshold: tau, Answered: answered, Errors: errors,
			Risk: risk, Envelope: envelope, Bound: bound, Feasible: feasible,
		})
		if feasible && tau < best {
			best = tau
			rep.Answered, rep.AnsweredRisk, rep.Bound = answered, risk, bound
		}
	}
	if math.IsInf(best, 1) {
		rep.Threshold, rep.Satisfiable = 1, false
		return rep
	}
	rep.Threshold, rep.Satisfiable = best, true
	return rep
}

// Drift 是校准集与锁箱集的对照结果。
type Drift struct {
	CalibThreshold   float64 `json:"calib_threshold"`
	LockboxThreshold float64 `json:"lockbox_threshold"`
	ThresholdGap     float64 `json:"threshold_gap"`
	CalibRisk        float64 `json:"calib_risk"`
	LockboxRisk      float64 `json:"lockbox_risk"`
	RiskGap          float64 `json:"risk_gap"`
	Drifted          bool    `json:"drifted"`  // 变差：承诺可能破了
	Improved         bool    `json:"improved"` // 变好：校准偏保守（不是漂移）
	Note             string  `json:"note"`
}

// Evaluate 用**给定的阈值**评估一批样本：答 conf ≥ τ 的题数与错误率。
//
// 这是锁箱工作流的入口：阈值来自校准集（Build），样本来自锁箱集。
// 不复用 Build 是因为 Build 会**重新挑**阈值——在锁箱上重挑阈值等于用
// 锁箱调参，锁箱就废了。锁箱只接受一个外来阈值，然后如实回答它是否兑现。
func Evaluate(threshold float64, samples []Sample, alpha float64) Report {
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.10
	}
	rep := Report{Samples: len(samples), Alpha: alpha, Threshold: threshold, Oracles: map[string]int{}}
	for _, s := range samples {
		rep.Oracles[s.Oracle]++
	}
	errors := 0
	for _, s := range samples {
		if s.Confidence < threshold {
			continue
		}
		rep.Answered++
		if !s.Correct {
			errors++
		}
	}
	if rep.Answered > 0 {
		rep.AnsweredRisk = float64(errors) / float64(rep.Answered)
	}
	rep.Bound = rep.AnsweredRisk + (1-alpha)/float64(rep.Answered+1)
	rep.Satisfiable = rep.Answered > 0 && rep.Bound <= alpha
	return rep
}

// CompareSamples 是锁箱复验：拿校准集拟合出的阈值，直接在锁箱样本上算
// "答 conf ≥ τ"的错误率，看它是否仍然 ≤ α。锁箱只看一次，看完换一份。
func CompareSamples(calibReport Report, lockboxSamples []Sample) Drift {
	d := Drift{CalibThreshold: calibReport.Threshold, LockboxThreshold: calibReport.Threshold,
		CalibRisk: calibReport.AnsweredRisk}
	answered, errors := 0, 0
	for _, s := range lockboxSamples {
		if s.Confidence < calibReport.Threshold {
			continue
		}
		answered++
		if !s.Correct {
			errors++
		}
	}
	if answered > 0 {
		d.LockboxRisk = float64(errors) / float64(answered)
	}
	d.RiskGap = d.LockboxRisk - d.CalibRisk
	// 只有"变差"才算漂移：锁箱风险超过 α（承诺破了），或比校准集高出
	// α/2 以上（阈值带着校准集的记忆，换一份题就不成立）。锁箱更保守
	// （风险更低）不是漂移，是校准偏保守——单独标 Improved，别混进告警。
	d.Drifted = d.LockboxRisk > calibReport.Alpha || d.RiskGap > calibReport.Alpha/2
	d.Improved = d.RiskGap < -calibReport.Alpha/2
	d.Note = fmt.Sprintf("锁箱精确复验：τ=%.3f 上答 %d 题、错误率 %.3f（校准 %.3f，α=%.2f）",
		calibReport.Threshold, answered, d.LockboxRisk, calibReport.AnsweredRisk, calibReport.Alpha)
	return d
}

// bucketize 等宽分桶（原始桶，不压平——单调性检查要看的是原始噪声）。
func bucketize(samples []Sample, n int) []Knot {
	out := make([]Knot, 0, n)
	for i := 0; i < n; i++ {
		lo := float64(i) / float64(n)
		hi := float64(i+1) / float64(n)
		k := Knot{Lo: lo, Hi: hi}
		ok := 0
		for _, s := range samples {
			if s.Confidence < lo || (i < n-1 && s.Confidence >= hi) {
				continue
			}
			k.N++
			if s.Correct {
				ok++
			}
		}
		if k.N > 0 {
			k.Value = float64(ok) / float64(k.N)
		}
		out = append(out, k)
	}
	return out
}

func monotone(knots []Knot) bool {
	prev := math.Inf(-1)
	for _, k := range knots {
		if k.N == 0 {
			continue
		}
		if k.Value < prev {
			return false
		}
		prev = k.Value
	}
	return true
}

package learncore

import (
	"errors"
	"sort"
)

// ErrNothingToLearn 没有可学的方向。这是正常结论，不是错误调用：
// 学不到东西就直说，不许为了“有产出”而硬提一个变更。
var ErrNothingToLearn = errors.New("learncore: nothing to learn")

// Observation 是观察阶段的产出：失败按六分类的计数、当前旋钮值、题数。
type Observation struct {
	FailureCounts map[string]int
	Knobs         map[string]float64 // 当前值（提议 = 当前值 ± 步长）
	ItemsDone     int
}

// Proposal 是提议：一组白名单内的新值 + 理由。
type Proposal struct {
	Knobs  map[string]float64
	Reason string
}

// Hypothesizer 提建议。离线版按失败归类给规则建议；LLM 版（未接）一次
// 假设调用生成。无论哪版，白名单都由 Registry 把守——模型也越界不了。
type Hypothesizer interface {
	Propose(obs Observation) (Proposal, error)
}

// OfflineHypothesizer 规则版：失败归类驱动旋钮方向。
//
// 规则就两条，写死在这里，改规则要走评审：
//   - recall-miss 主导 → topk 加一步（多取证）；
//   - grounding-fail 出现 → width 加一步（窗口宽一点，锚点更稳）。
//
// 其余类别不提议——特别是 route-error：动路由阈值要先看代价账，
// 规则版不碰。
type OfflineHypothesizer struct {
	TopKKnob  string
	WidthKnob string
	TopKStep  float64
	WidthStep float64
}

func (h OfflineHypothesizer) Propose(obs Observation) (Proposal, error) {
	if obs.ItemsDone == 0 {
		return Proposal{}, ErrNothingToLearn
	}
	dominant, count := dominantCategory(obs.FailureCounts)
	if count == 0 {
		return Proposal{}, ErrNothingToLearn
	}
	p := Proposal{Knobs: map[string]float64{}}
	switch dominant {
	case "recall-miss":
		p.Knobs[h.TopKKnob] = obs.Knobs[h.TopKKnob] + h.TopKStep
		p.Reason = "recall-miss dominant: widen candidate set by one step"
	case "grounding-fail":
		p.Knobs[h.WidthKnob] = obs.Knobs[h.WidthKnob] + h.WidthStep
		p.Reason = "grounding-fail present: widen evidence window by one step"
	default:
		return Proposal{}, ErrNothingToLearn
	}
	return p, nil
}

// dominantCategory 返回计数最高的类别（并列按类别名字典序——确定性）。
func dominantCategory(counts map[string]int) (string, int) {
	var names []string
	for n, c := range counts {
		if c > 0 {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", 0
	}
	sort.Strings(names)
	best := names[0]
	for _, n := range names[1:] {
		if counts[n] > counts[best] {
			best = n
		}
	}
	return best, counts[best]
}

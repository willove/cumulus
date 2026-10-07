package qaflow

import (
	"fmt"

	gocontext "context"

	"github.com/willove/cumulus/internal/query"

	"github.com/willove/cumulus/internal/context"
)

// KeyEscalation 是升级执行的账：这一题有没有真去升级、升级后路由变成
// 了什么。升级是死标签还是有执行处，这个 key 回答得了。
var KeyEscalation = context.NewKey[EscalationRecord]("route.escalation")

// EscalationRecord 记录一次升级执行。
type EscalationRecord struct {
	Triggered bool   `json:"triggered"` // 路由是否判了 escalate
	Executed  bool   `json:"executed"`  // 是否真跑了贵路（有执行处且判了 escalate）
	Before    string `json:"before"`    // 升级前动作
	After     string `json:"after"`     // 升级后重判的动作（未执行则同 Before）
	Windows   int    `json:"windows"`   // 升级后的窗口数
	Reason    string `json:"reason,omitempty"`
}

// EscalateStage 是升级的执行处（BioHarness 级联：便宜快路 → 充足性
// 判定 → 贵路升级 → 再判定）。只有路由判了 escalate 才跑贵路；没判
// 就空过（留痕“未触发”）。升级后再判一次路由：
//   - 够了 → fast，带着新窗口进合成；
//   - 还不够 → refuse（"escalation exhausted"）——升级是补救机会，
//     不是绕过充足性硬答的旁门。
//
// 贵路是注入的取数函数（BM25DeepEvidence 等）——升级路径和首程同样是
// 注册件，换底物不动流程。
type EscalateStage struct {
	Retrieve func(*context.Context, Rewrite) ([]EvidenceWindow, error) // 贵路：nil = 无执行处
	// 词汇鸿沟桥（cumulus 的 keywords_multilevel）：升级且关键词级稀薄
	// 时，先把口语问句翻成语料语言的检索词，再重取。缺一个字段就只走
	// 普通贵路——桥是可选件，不是必经路。
	Expand   query.Expander
	Analyze  func(q string) query.Analysis
	Weighted func(weights map[string]float64) ([]EvidenceWindow, error)
}

func (EscalateStage) Name() string { return "escalate" }
func (EscalateStage) Reads() []string {
	return []string{KeyRoute.String(), KeyRewrite.String()}
}
func (EscalateStage) Writes() []string {
	return []string{KeyWindows.String(), KeyRoute.String(), KeyEscalation.String(), KeyDeep.String(), KeyRerank.String(), KeyCoverage.String()}
}

func (s EscalateStage) Run(c *context.Context) error {
	route, _ := context.Get(c, KeyRoute)
	rec := EscalationRecord{Triggered: route.Action == "escalate", Before: route.Action, After: route.Action}
	if !rec.Triggered {
		rec.Reason = "route did not escalate"
		return context.Set(c, KeyEscalation, rec)
	}
	if s.Retrieve == nil {
		rec.Reason = "no escalation backend wired; escalate stays terminal"
		return context.Set(c, KeyEscalation, rec)
	}
	rw, ok := context.Get(c, KeyRewrite)
	if !ok {
		return fmt.Errorf("escalate: rewrite missing")
	}
	ws, err := s.escalateRetrieve(c, rw)
	if err != nil {
		return fmt.Errorf("escalate: retrieve: %w", err)
	}
	// **合并**而不是替换：首程窗（再问时已加宽）与贵路窗合起来才够判。
	// 替换是自相矛盾的——首程刚逐事实取回的证据，一替换就把事实的支撑
	// 窗丢了（真跑教训：再问后 f1 的覆盖反而从"盖"变"缺"，模型答"该事
	// 实无支撑证据"）。合并后按 (源,span) 去重、按分降序、封顶 12——
	// 再多对合成面就是噪声（提示词预算）。
	prev, _ := context.Get(c, KeyWindows)
	merged := mergeWindows(prev, ws)
	if err := context.Set(c, KeyWindows, merged); err != nil {
		return err
	}
	rec.Executed = true
	rec.Windows = len(ws)
	// 重判：升级后的窗口重新算信号（升级前后的路由判据同一把尺）
	next := routeWith(c, ws)
	rec.After = next.Action
	if err := context.Set(c, KeyRoute, next); err != nil {
		return err
	}
	return context.Set(c, KeyEscalation, rec)
}

func (EscalateStage) Verify(c *context.Context) error {
	// 升级后仍然拒答的话，答案不许出现（Verify 在合成 stage 兜同一道）
	return nil
}

// routeWith 用当前 context 里的信号对给定窗口重算路由（升级后重判用）。
//
// 升级后判据和首程不同，这是有意的：**升级的钱已经花了**。首程 escalate
// 的门槛可以低（不确定就多看一眼），升级后还以同一个低门槛拒答，等于把
// 刚花的钱扔掉（真跑实测：65 题升级后仍以覆盖不足拒答，但金标就在窗内
// ——79.3% 的证据命中被 21.7% 的拒答抵消）。所以升级后只有“贵路什么都没
// 取到”才拒答；取到了就作答，但把残余低置信度写进 reason（诚实标注：
// 这题是带疑作答，不是干净命中）。
func routeWith(c *context.Context, ws []EvidenceWindow) RouteDecision {
	sig := gatherSignals(c, ws)
	d := RouteDecision{Grounded: len(ws) > 0, Signals: sig}
	switch {
	case len(ws) == 0:
		d.Action = "refuse"
		d.Reason = "no evidence window after escalation; refuse by grammar"
	case sig.Confidence < 0.5:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("answered after escalation with residual low confidence %.3f (coverage=%.3f margin=%.3f) — best-effort, not a clean hit", sig.Confidence, sig.Coverage, sig.Margin)
	default:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("resolved by escalation: confidence %.3f (coverage=%.3f margin=%.3f)", sig.Confidence, sig.Coverage, sig.Margin)
	}
	return d
}

// escalateRetrieve 升级时的取数：关键词级稀薄（词汇鸿沟）且有桥时，先
// 扩展再按权取；否则走普通贵路。扩展失败不阻塞——按原查询走贵路，
// 原因由调用方日志承担，不许把流程搞死。
func (s EscalateStage) escalateRetrieve(c *context.Context, rw Rewrite) ([]EvidenceWindow, error) {
	a, hasAnalysis := context.Get(c, KeyAnalysis)
	thin := !hasAnalysis || a.Thin(2, 2.0)
	if s.Expand == nil || s.Weighted == nil || !thin {
		return s.Retrieve(c, rw)
	}
	expanded, err := s.Expand.Expand(gocontext.Background(), rw.Original)
	if err != nil || len(expanded) == 0 {
		return s.Retrieve(c, rw) // 桥失败：退化贵路
	}
	// 合并权重：**扩展词为主，原主级降为 0.2 的边注**。桥的语义是替换不
	// 是并列——原问词在鸿沟场景下正是失败的那批（垃圾二元组在库里稀
	// 有、idf 高，并列着会把桥带歪：真跑教训"养狗叫得太吵"扩出噪声，
	// 得太/谁管俩垃圾词权重 2.0/1.0 把窗口拉去太湖流域管理条例）。
	// 原词不清零：非纯鸿沟时它们仍可能带对信号。
	weights := map[string]float64{}
	for k, v := range a.Primary {
		weights[k] = v * 0.2
	}
	for _, k := range expanded {
		// 扩展词拆成索引口径（二元组）才进得了倒排
		for _, bg := range query.SplitTerms(k) {
			// 扩展词覆盖同形原词（桥比原问词懂行——重叠时听桥的）
			weights[bg] = 2.0
		}
	}
	return s.Weighted(weights)
}

// mergeWindows 合并两个窗集：按 (SourceID,Span) 去重，按分降序，封顶
// 12（合成提示词的预算上限）。
func mergeWindows(a, b []EvidenceWindow) []EvidenceWindow {
	seen := map[string]bool{}
	out := make([]EvidenceWindow, 0, len(a)+len(b))
	for _, ws := range [][]EvidenceWindow{a, b} {
		for _, w := range ws {
			key := w.SourceID + "#" + w.Span
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, w)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

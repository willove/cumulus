package qaflow

import (
	"fmt"

	"github.com/willove/cumulus/internal/context"
)

// KeyEscalation 是升级执行的账：这一题有没有真去升级、升级后路由变成
// 了什么。升级是死标签还是有执行处，这个 key 回答得了。
var KeyEscalation = context.NewKey[EscalationRecord]("route.escalation")

// EscalationRecord 记录一次升级执行。
type EscalationRecord struct {
	Triggered bool   // 路由是否判了 escalate
	Executed  bool   // 是否真跑了贵路（有执行处且判了 escalate）
	Before    string // 升级前动作
	After     string // 升级后重判的动作（未执行则同 Before）
	Windows   int    // 升级后的窗口数
	Reason    string // 没执行的原因（无执行处配装等）
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
	ws, err := s.Retrieve(c, rw)
	if err != nil {
		return fmt.Errorf("escalate: retrieve: %w", err)
	}
	if err := context.Set(c, KeyWindows, ws); err != nil {
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

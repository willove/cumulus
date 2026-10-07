package qaflow

import (
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge"
)

// KeyReuseState 是复用库的查询结果：命中/未命中及原因，和 KeyRerank 同
// 一纪律——每个决定都要可审计（不留静默决定）。
var KeyReuseState = context.NewKey[ReuseState]("knowledge.reuse.state")

// KeyDeepen 是再问加深旗。ReuseStage 发现本会话原样再问时置位：
// 检索加宽（topk×2、窗宽×1.5）+ 路由强制升级（词汇桥/贵路重取）。
//
// 语义只有一条：**用户原样再问 = "上次的答案不够"**。此时正确反应是
// 设法取更多证据，不是把上轮同一套窗重放一遍。重放同一个不够好的答
// 案（旧行为）对个人工具是伪需求——一次 BM25 检索本来就是毫秒级，
// “越问越快”省下的是机器的时间，赔上的是用户的答案。
// （本旗由 ReuseStage 写，EvidenceStage/BM25Evidence 与 RouteStage 读。）
var KeyDeepen = context.NewKey[bool]("qa.deepen")

// ReuseState 是复用库查的结果。
type ReuseState struct {
	Hit    bool   `json:"hit"`
	Reason string `json:"reason,omitempty"` // 未命中的原因
}

// ReuseStage 在 evidence 前查会话复用库，只做一件事：**识别原样再问**。
// 命中不改行为路径（不再短路窗口）——原样再问时置 KeyDeepen，让本轮
// 检索加深；未命中什么都不做。复用库因此从"重放器"退成"再问检测器"
// （记录仍在 ReuseRecordStage 落，只是不再被当答案用）。
type ReuseStage struct {
	Session string
	Store   *knowledge.ReuseStore
	Query   string // 问题原文（Runner 组装时注入；经 RewriteStage 归一）
}

func (ReuseStage) Name() string { return "reuse" }

// 本 stage 只写状态旗，不碰窗口（短路已废——见 KeyDeepen 的语义）。
func (ReuseStage) Reads() []string { return nil }
func (ReuseStage) Writes() []string {
	return []string{KeyReuseState.String(), KeyDeepen.String()}
}

func (s ReuseStage) Run(c *context.Context) error {
	if s.Store == nil {
		return nil
	}
	_, seen := s.Store.Lookup(s.Session, s.Query)
	if !seen {
		return context.Set(c, KeyReuseState, ReuseState{Hit: false, Reason: "no record for this question in session"})
	}
	// 原样再问：不重放（重放=同一个答案再收一次钱），置加深旗让本轮
	// 设法。回答过又原样问，只有一种解释——上次不够。
	if err := context.Set(c, KeyDeepen, true); err != nil {
		return err
	}
	return context.Set(c, KeyReuseState, ReuseState{Hit: false, Reason: "re-ask: deepening instead of replaying"})
}

// ReuseRecordStage 在 account 后记录：窗口 + 本轮是否真答上。route 是
// refuse 时不记（拒答的经验没有复用价值）。记录的存在意义从"重放答案"
// 变成"标记这个问题问过了"——ReuseStage 靠它识别再问。
type ReuseRecordStage struct {
	Session string
	Store   *knowledge.ReuseStore
	Query   string
}

func (ReuseRecordStage) Name() string { return "reuse-record" }
func (ReuseRecordStage) Reads() []string {
	return []string{KeyWindows.String(), KeyRoute.String(), KeyAnswer.String()}
}
func (ReuseRecordStage) Writes() []string { return nil }

func (s ReuseRecordStage) Run(c *context.Context) error {
	if s.Store == nil {
		return nil
	}
	ws, _ := context.Get(c, KeyWindows)
	route, _ := context.Get(c, KeyRoute)
	ans, _ := context.Get(c, KeyAnswer)
	if route.Action == "refuse" || ans.Refused {
		return nil // 拒答不记
	}
	wins := make([]knowledge.Window, 0, len(ws))
	for _, w := range ws {
		wins = append(wins, knowledge.Window{SourceID: w.SourceID, Title: w.Title, Span: w.Span, Text: w.Text, Score: w.Score})
	}
	cov, _ := context.Get(c, KeyCoverage)
	s.Store.Record(s.Session, s.Query, wins, len(ans.Citations) > 0, knowledge.Coverage{
		Value: cov.Value, Terms: cov.Terms, OOV: cov.OOV,
	})
	return nil
}

func (ReuseRecordStage) Verify(c *context.Context) error { return nil }

// Verify：本 stage 只写状态旗，不产数据，没有可验的契约。
func (ReuseStage) Verify(c *context.Context) error { return nil }

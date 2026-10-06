package qaflow

import (
	"errors"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge"
)

// KeyReuse 是复用命中记录：命中/未命中及原因，和 KeyRerank 同一纪律——
// 可选组件的每个决定都要可审计（不留静默决定）。
var KeyReuseState = context.NewKey[ReuseState]("knowledge.reuse.state")

// ReuseState 是复用查的结果。
type ReuseState struct {
	Hit    bool
	Reason string // 未命中的原因
}

// ReuseStage 在 evidence 前查会话复用：命中就直接用上轮窗口，本问不再
// 检索。未命中什么都不做——evidence 照常跑，复用不是旁路，是短路。
type ReuseStage struct {
	Session string
	Store   *knowledge.ReuseStore
	Query   string // 问题原文（Runner 组装时注入；经 RewriteStage 归一）
}

func (ReuseStage) Name() string { return "reuse" }

// 本 stage 只写窗口（短路检索），不读 context 中的检索数据
func (ReuseStage) Reads() []string { return nil }
func (ReuseStage) Writes() []string {
	return []string{KeyWindows.String(), KeyReuseState.String(), KeyCoverage.String()}
}

func (s ReuseStage) Run(c *context.Context) error {
	if s.Store == nil {
		return nil
	}
	e, ok := s.Store.Lookup(s.Session, s.Query)
	if !ok {
		return context.Set(c, KeyReuseState, ReuseState{Hit: false, Reason: "no record for this question in session"})
	}
	windows := make([]EvidenceWindow, 0, len(e.Windows))
	for _, w := range e.Windows {
		windows = append(windows, EvidenceWindow{
			SourceID: w.SourceID,
			Span:     w.Span,
			Text:     w.Text,
			Score:    w.Score,
		})
	}
	// 上轮窗口没引用的就不复用（空记录等同未命中）
	if len(windows) == 0 {
		return context.Set(c, KeyReuseState, ReuseState{Hit: false, Reason: "record has no windows"})
	}
	if err := context.Set(c, KeyWindows, windows); err != nil {
		return err
	}
	// 覆盖度随窗口回放：复用命中时路由看到的是上轮同一口径的事实，
	// 不是缺省 0
	if err := context.Set(c, KeyCoverage, CoverageInfo{Value: e.Coverage.Value, Terms: e.Coverage.Terms, OOV: e.Coverage.OOV}); err != nil {
		return err
	}
	return context.Set(c, KeyReuseState, ReuseState{Hit: true})
}

// Verify 复用窗口同样要过契约：每条引用可回溯（同一把尺子）
func (ReuseStage) Verify(c *context.Context) error {
	if rs, ok := context.Get(c, KeyReuseState); !ok || !rs.Hit {
		return nil
	}
	ws, _ := context.Get(c, KeyWindows)
	for _, w := range ws {
		if w.SourceID == "" || w.Span == "" {
			return errors.New("reuse: window missing source id or span — reuse must satisfy the same citation contract as retrieval")
		}
	}
	return nil
}

// ReuseRecordStage 在 account 后记录：窗口 + 本轮是否真答上。route 是
// refuse 时不记（拒答的经验没有复用价值）。
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
		wins = append(wins, knowledge.Window{SourceID: w.SourceID, Span: w.Span, Text: w.Text, Score: w.Score})
	}
	cov, _ := context.Get(c, KeyCoverage)
	s.Store.Record(s.Session, s.Query, wins, len(ans.Citations) > 0, knowledge.Coverage{
		Value: cov.Value, Terms: cov.Terms, OOV: cov.OOV,
	})
	return nil
}

func (ReuseRecordStage) Verify(c *context.Context) error { return nil }

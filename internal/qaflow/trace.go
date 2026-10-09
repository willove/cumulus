package qaflow

import (
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/harness"
)

// Trace 把**流程内部观测**翻译成**对外事件**（harness 事件流）。
//
// 分层上它站在 pipeline → capabilities 这条合法边上：qaflow 认识 harness 的事件
// 词表，harness 不认识 qaflow（那边是反向的，门禁会拦）。所以事件载荷用
// harness 的朴素结构体，窗口/答案的读取在这里做一次转换。
//
// **缺席即无事件**：Emitter 为 nil 时本类型什么都不发，流程逐字段不变
// （harness 契约 1：可选增强不许变成依赖）。
type Trace struct {
	em         *harness.Emitter
	runID      string
	classifier *WindowClassifier // nil = 不分级（事件里没有 class 字段）
	files      map[string]bool   // 已发过的 docid（去重：升级/重取数会重发同一篇）
	done       int               // 已收尾的阶段数（算进度用）
	total      int               // 本次流程的标准阶段数（分母；0 = 未知）
	synth      bool              // 已经过合成阶段（详情文案要防"用未来数据解释过去"）
}

// NewTrace 装一个事件发射器（em 为 nil = 这一层没接）。total 是本次流程的
// 标准阶段数——**进度要有分母**，否则百分比就是猜的。
func NewTrace(em *harness.Emitter, runID string, total int) *Trace {
	return &Trace{em: em, runID: runID, files: map[string]bool{}, total: total}
}

// withClassifier 装窗口分级器（可选能力；nil = 不分级）。
func (t *Trace) withClassifier(c *WindowClassifier) *Trace {
	t.classifier = c
	return t
}

// Emitter 返回底层发射器（供上层补发终态事件）。
func (t *Trace) Emitter() *harness.Emitter {
	if t == nil {
		return nil
	}
	return t.em
}

// Stage 满足 flow.TraceFunc：相位 → 事件；证据阶段结束时**顺手把窗口发出去**。
//
// 为什么窗口挂在 evidence 上：检索是"取到一个就能讲一个"的动作，玩家想看的就是
// 检索日志（旧 cumulus 的 stage=file 同形）。但 BM25 是一次性返回有序列表，所以
// 事件是**有序批量**发出，而不是逐个检索瞬间——这是接口决定的，**不许假装成流式**。
func (t *Trace) Stage(c *context.Context, name string, phase flow.TracePhase, durMS int64) {
	if t == nil {
		return
	}
	if t.em == nil && t.classifier == nil {
		return // 没有出口也没有分级器：这一层没接（合法状态）
	}
	if t.em == nil {
		// **只有分级器、没有事件出口**（`/v1/qa` 一次性 JSON 就是这个形态）：
		// 分级结果仍要落到 context 的挂点上，否则读数里"未分级"与"没跑"分不开。
		if phase == flow.TraceDone && name == (EvidenceStage{}).Name() {
			t.classifyOnly(c)
		}
		return
	}
	p := harness.PhaseStart
	switch phase {
	case flow.TraceDone:
		p = harness.PhaseDone
		t.done++
	case flow.TraceFail:
		p = harness.PhaseDone // 失败也收尾（耗时可见），错误由上层发 error 事件
		t.done++
	}
	// 详情按"这一步**刚做完时**可读到的东西"写——不许用后面才产生的数据
	// 解释这一步（真跑教训式的诱惑：合成之后再回头给检索阶段写"找到了答案"）。
	ev, err := harness.StageDetailed(t.runID, name, p, durMS,
		t.detail(c, name, phase), harness.StageProgress(t.done, t.total))
	if err != nil {
		return // 半截事件不许进流；构造错由 harness 侧计数
	}
	_ = t.em.Emit(ev)
	if phase == flow.TraceDone && name == (EvidenceStage{}).Name() {
		t.emitWindows(c)
	}
}

// emitWindows 把最终窗集发成 file 事件（rank/标题/分数/坐标/预览 + 类别）。
//
// 分级在**这里**做而不是更早：发事件时我们手上就是最终窗集（升级/驱逐都过了），
// 早做会分到一批已经作废的窗口。
func (t *Trace) emitWindows(c *context.Context) {
	ws, ok := context.Get(c, KeyWindows)
	if !ok {
		return
	}
	// 分级失败**不发空类别**：宁可这一批窗口没有类别（消费方显示"未分级"），
	// 也不要把半张表写进事件流。
	var classes map[int]string
	if t.classifier != nil {
		rw, ok2 := context.Get(c, KeyRewrite)
		switch {
		case !ok2:
			t.noteClassify(c, "no-rewrite")
		default:
			got, err := t.classifier.Classify(c, rw.Original, ws)
			if err != nil {
				// 分级失败要**看得见**（否则读数里"没有类别"和"没分级"分不开，
				// 而我第一版就是这么把失败静默掉的）
				t.noteClassify(c, "failed: "+err.Error())
			} else {
				classes = got
			}
		}
	}
	for i, w := range ws {
		if t.files[w.SourceID] {
			continue // 同一次跑动里同一篇只发一次（升级重取数不算新窗口）
		}
		t.files[w.SourceID] = true
		ev, err := harness.File(t.runID, harness.FileInfo{
			Rank: i + 1, DocID: w.SourceID, Title: w.Title, Score: w.Score,
			Span: w.Span, Preview: truncateRunes(w.Text, 160),
			Class: classes[i+1], // 没分级就是空（消费方显示"未分级"）
		})
		if err != nil {
			continue
		}
		_ = t.em.Emit(ev)
	}
}

// RelatedFrom 是**关联文档**的默认口径：取数里**没被引用**的窗口。
//
// 为什么这样定义："关联文档"回答的是"系统还看到了什么"——检索面比答案面宽，这是
// RAG 与搜索最大的差别，也是用户最常追问的（"还有别的相关文档吗"）。口径必须
// **窄而真**：宁可少列，不可把无关的东西塞进"相关"。why 字段说明它为什么算，
// 消费方不必猜。
func RelatedFrom(ws []EvidenceWindow, cited []string, limit int) []harness.RelatedInfo {
	used := make(map[string]bool, len(cited))
	for _, id := range cited {
		// 引用有两种形状在流通："docid" 与 "docid#span"（答案里就是后者）。
		// 只认一种会让"关联文档"把已引用的文档又列一遍——**宁可多解析一次
		// 格式，也不要让读数骗人**。
		used[id] = true
		if i := strings.IndexByte(id, '#'); i > 0 {
			used[id[:i]] = true
		}
	}
	out := make([]harness.RelatedInfo, 0, limit)
	seen := map[string]bool{}
	for _, w := range ws {
		if used[w.SourceID] || seen[w.SourceID] || len(out) >= limit {
			continue
		}
		seen[w.SourceID] = true
		out = append(out, harness.RelatedInfo{
			DocID: w.SourceID, Title: w.Title, Score: w.Score, Span: w.Span,
			Preview: truncateRunes(w.Text, 160), Why: "retrieved-uncited",
		})
	}
	return out
}

// FinalFrom 把问答的最终状态映射成 done 事件载荷（由上层 apps 调用——它才知道
// 自己的响应形状；这里只做语义翻译）。
//
// 为什么参数这么碎：done 一帧是流式消费者的**唯一收尾信息**——很多客户端不回查
// /v1/qa，所以答案、路由、计量、提交视图、决策留痕必须**一次说全**。
func FinalFrom(resp FinalInput) harness.DoneInfo {
	return harness.DoneInfo{
		Answer:           resp.Answer,
		Refused:          resp.Refused,
		RefusalReason:    resp.RefusalReason,
		RouteAction:      resp.RouteAction,
		RouteTier:        resp.RouteTier,
		Threshold:        resp.Threshold,
		Coverage:         resp.Coverage,
		LatencyMS:        resp.LatencyMS,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		CostKnown:        resp.CostKnown,
		DecisionKind:     resp.DecisionKind,
		DecisionApplied:  resp.DecisionApplied,
		DecisionReason:   resp.DecisionReason,
		Committed:        resp.Committed,
		Counts:           resp.Counts,
	}
}

// FinalInput 是 FinalFrom 的输入（**中立结构体**，不绑任何一层的类型）。
type FinalInput struct {
	Answer           string
	Refused          bool
	RefusalReason    string
	RouteAction      string
	RouteTier        string
	Threshold        float64
	Coverage         float64
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
	DecisionKind     string
	DecisionApplied  bool
	DecisionReason   string
	Committed        string
	Counts           map[string]int
}

// CommittedString 把提交视图压成**一串 k=v**（外部展示与对账够用，且不把
// kernel 的结构体形状泄漏成公开契约）。顺序固定 → 可直接比对。
func CommittedString(v context.CommittedView) string {
	parts := []string{
		"corpus=" + strings.TrimSpace(v.CorpusVersion),
		"config=" + strings.TrimSpace(v.ConfigVersion),
		"strategy=" + strings.TrimSpace(v.StrategyVersion),
		"belief=" + strings.TrimSpace(v.BeliefVersion),
	}
	cal := v.Calibration
	if cal.Tier != "" {
		parts = append(parts, "tier="+cal.Tier)
	}
	if cal.Program != "" {
		parts = append(parts, "calibration="+cal.Program)
	}
	if cal.Threshold != 0 {
		parts = append(parts, fmt.Sprintf("threshold=%.3f", cal.Threshold))
	}
	if cal.ThresholdVersion != "" {
		parts = append(parts, "threshold_version="+cal.ThresholdVersion)
	}
	return strings.Join(parts, " ")
}

func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// detail 是"这一步干了什么"的一行摘要。**只写这一步当时可读到的**；读不到
// 就留空（消费方退回阶段标签）——不留空而编一句，读数就开始骗人。
func (t *Trace) detail(c *context.Context, name string, phase flow.TracePhase) string {
	if phase == flow.TraceStart {
		return "" // 开始时通常还什么都没产生
	}
	switch name {
	case (EvidenceStage{}).Name():
		if ws, ok := context.Get(c, KeyWindows); ok {
			return fmt.Sprintf("取回 %d 条窗口", len(ws))
		}
	case (FactsStage{}).Name():
		if fx, ok := context.Get(c, KeyFactReport); ok {
			covered := 0
			for _, f := range fx.Facts {
				if f.Covered {
					covered++
				}
			}
			return fmt.Sprintf("%d/%d 条事实已覆盖", covered, len(fx.Facts))
		}
	case (RouteStage{}).Name():
		if r, ok := context.Get(c, KeyRoute); ok {
			return fmt.Sprintf("路由 %s · 覆盖 %.2f · 边际 %.2f", r.Action, r.Signals.Coverage, r.Signals.Margin)
		}
	case (EvictStage{}).Name():
		if ws, ok := context.Get(c, KeyWindows); ok {
			return fmt.Sprintf("上下文内保留 %d 条窗口", len(ws))
		}
	case (SynthesizeStage{}).Name():
		if a, ok := context.Get(c, KeyAnswer); ok {
			if a.Refused {
				return "证据不足，已拒答（" + oneLine(a.RefusalReason) + "）"
			}
			return fmt.Sprintf("答案 %d 字", len([]rune(a.Text)))
		}
	case (AccountStage{}).Name():
		if u, ok := context.Get(c, KeyUsage); ok {
			return fmt.Sprintf("计量 %d/%d token", u.PromptTokens, u.CompletionTokens)
		}
	case (EscalateStage{}).Name():
		if e, ok := context.Get(c, KeyEscalation); ok {
			if e.Executed {
				if r := oneLine(e.Reason); r != "" {
					return "已升级取数并重判（" + r + "）"
				}
				return "已升级取数并重判"
			}
			if e.Triggered {
				return "路由判了升级，但没有贵路执行处"
			}
		}
		return "未升级"
	case (GateStage{}).Name():
		if rec, ok := DecisionRecordOf(c); ok {
			if !rec.Applied {
				return "决策面缺席，按原路径放行"
			}
			return "决策闸门分 " + fmt.Sprintf("%.2f", rec.Noul)
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) > 60 {
		return string(rs[:60]) + "…"
	}
	return s
}

// noteClassify 把分级失败的结局记进升级记录旁的分类字段（可观测）。
func (t *Trace) noteClassify(c *context.Context, outcome string) {
	rec, ok := context.Get(c, KeyEscalation)
	if !ok {
		return // 还没升级（正常时序）：此时结局已由 KeyWindowClass 承载
	}
	if rec.Classification == "" {
		rec.Classification = outcome
	} else {
		rec.Classification += "+" + outcome
	}
	_ = context.Set(c, KeyEscalation, rec)
}

// classifyOnly 只做分级并写挂点（没有事件出口时的形态：`/v1/qa` 一次性 JSON）。
// 窗口事件仍发不出去，但**分级读数要看得见**——否则那条路径上分级等于没接。
func (t *Trace) classifyOnly(c *context.Context) {
	ws, ok := context.Get(c, KeyWindows)
	if !ok || len(ws) == 0 {
		_ = SetWindowClass(c, nil)
		return
	}
	rw, ok := context.Get(c, KeyRewrite)
	if !ok {
		_ = SetWindowClass(c, nil)
		return
	}
	got, err := t.classifier.Classify(c, rw.Original, ws)
	if err != nil {
		t.noteClassify(c, "failed: "+err.Error())
	}
	_ = SetWindowClass(c, got)
}

// Package qaflow 是流程一：一次问答的六个 stage（docs/flow-grammar.md §二）。
//
// v0.1 是骨架：stage 划分、契约、验证已经是真的；检索与合成是桩（TODO 标出），
// 接口先于实现定死，之后往里填零件不改 runner。
package qaflow

import (
	"errors"
	"fmt"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/knowledge/belief"
)

// 本包登记的 key。命名规则 "<域>.<名>"，全进程唯一。
var (
	KeyRewrite = context.NewKey[Rewrite]("qa.rewrite")
	KeyWindows = context.NewKey[[]EvidenceWindow]("qa.evidence.windows")
	KeyRoute   = context.NewKey[RouteDecision]("qa.route")
	KeyAnswer  = context.NewKey[Answer]("qa.answer")
	KeyUsage   = context.NewKey[Usage]("qa.usage")
	// KeySynthUsage 是合成步记下的用量。记账步只读它——账目和发生额
	// 是同一个来源，不许两处各写一份。
	KeySynthUsage = context.NewKey[Usage]("qa.synthesis.usage")
	// KeyBelief 是候选区信念的挂点。绑在 context 上，可用性因此可见：
	// 没绑就是没绑，status 面看得到——可选组件不许静默失效（不变量 3）。
	KeyBelief = context.NewKey[*belief.Belief]("knowledge.belief")
)

// Rewrite 是 stage 1 的产物：原问 + 假设文档双视图。
type Rewrite struct {
	Original     string
	Hypothetical string // TODO: 接 llm 生成假设摘要（HyDE 式）
}

// EvidenceWindow 是一个证据窗口。SourceID + Span 非空是引用可回溯的
// 最低契约，Verify 强制；Text 是窗口原文——合成面读它，模型看不到
// 原文就等于没有证据（第一次真调用学到的：坐标不是内容）。
type EvidenceWindow struct {
	SourceID  string
	Span      string
	Text      string
	Score     float64
	Substrate string // text / structured / cross-doc，来自底物注册
}

// RouteDecision 是 stage 3 的路由结论。信号与阈值都留痕，供回放
// “当时为什么升级/拒答”。
type RouteDecision struct {
	Action     string // fast / escalate / refuse
	Confidence float64
	Grounded   bool
	Reason     string
}

// Answer 是 stage 4 的产物。Citations 必须能映射回窗口 id。
type Answer struct {
	Text      string
	Citations []string // window 标识（SourceID#Span）
	Refused   bool
}

// Usage 是 stage 5 的记账。上游不报 usage 时 CostKnown=false，
// 按流程文法不许继续发起付费调用。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
}

// ---------- stage 1: 意图澄清 ----------

// RewriteStage 读会话、写改写。Verify 强制“改写可逆回原问”：
// 防假设漂移把用户的意图带走。
type RewriteStage struct{ Query string }

func (RewriteStage) Name() string     { return "intent-clarify" }
func (RewriteStage) Reads() []string  { return []string{"session"} }
func (RewriteStage) Writes() []string { return []string{KeyRewrite.String()} }
func (s RewriteStage) Run(c *context.Context) error {
	return context.Set(c, KeyRewrite, Rewrite{
		Original:     s.Query,
		Hypothetical: "", // TODO: llm
	})
}
func (s RewriteStage) Verify(c *context.Context) error {
	r, ok := context.Get(c, KeyRewrite)
	if !ok {
		return errors.New("rewrite missing")
	}
	if r.Original != s.Query {
		return fmt.Errorf("rewrite drifted from original question: %q != %q", r.Original, s.Query)
	}
	return nil
}

// ---------- stage 2: 证据供给 ----------

// EvidenceStage 从检索后端取证据窗口。Retrieve 收 context：候选区信念
// 这类按查询变化的输入从 context 读，不靠闭包捕获——命中过什么、
// 当前信什么，因此都进得了提交视图。
type EvidenceStage struct {
	Retrieve func(c *context.Context, rewrite Rewrite) ([]EvidenceWindow, error)
}

func (EvidenceStage) Name() string     { return "evidence-supply" }
func (EvidenceStage) Reads() []string  { return []string{KeyRewrite.String()} }
func (EvidenceStage) Writes() []string { return []string{KeyWindows.String()} }
func (s EvidenceStage) Run(c *context.Context) error {
	r, ok := context.Get(c, KeyRewrite)
	if !ok {
		return errors.New("rewrite missing; stage 1 must run first")
	}
	if s.Retrieve == nil {
		return errors.New("no retrieval backend wired")
	}
	ws, err := s.Retrieve(c, r)
	if err != nil {
		return err
	}
	return context.Set(c, KeyWindows, ws)
}

// Verify 强制每条窗口可回溯：SourceID 与 Span 都必须有。
// 这是“引用可核”在流程里的第一道闸，缺一个都不行。
func (EvidenceStage) Verify(c *context.Context) error {
	ws, ok := context.Get(c, KeyWindows)
	if !ok {
		return errors.New("evidence windows missing")
	}
	for i, w := range ws {
		if w.SourceID == "" || w.Span == "" {
			return fmt.Errorf("window %d: source id and span are both required (citation must resolve)", i)
		}
	}
	return nil
}

// ---------- stage 3: 充足性路由 ----------

// RouteStage 用两类便宜信号决定 fast / escalate / refuse：
// 置信度（路由代理，非裁判）与接地检查。决策留痕。
type RouteStage struct {
	Confidence float64
}

func (RouteStage) Name() string     { return "sufficiency-route" }
func (RouteStage) Reads() []string  { return []string{KeyWindows.String()} }
func (RouteStage) Writes() []string { return []string{KeyRoute.String()} }
func (s RouteStage) Run(c *context.Context) error {
	ws, _ := context.Get(c, KeyWindows)
	grounded := len(ws) > 0

	d := RouteDecision{Grounded: grounded, Confidence: s.Confidence}
	switch {
	case !grounded:
		d.Action = "refuse" // 没有证据：诚实的不知道，不许硬答
		d.Reason = "no evidence window; refuse by grammar"
	case s.Confidence < 0.5:
		d.Action = "escalate"
		d.Reason = "draft confidence below threshold; escalate to DEEP"
	default:
		d.Action = "fast"
		d.Reason = "grounded and confident enough"
	}
	return context.Set(c, KeyRoute, d)
}
func (RouteStage) Verify(c *context.Context) error {
	d, ok := context.Get(c, KeyRoute)
	if !ok {
		return errors.New("route decision missing")
	}
	switch d.Action {
	case "fast", "escalate", "refuse":
		return nil
	default:
		return fmt.Errorf("unknown route action %q", d.Action)
	}
}

// ---------- stage 4: 合成 ----------

// SynthFunc 是合成面契约：问题 + 证据窗口 → 带引用的答案 + 用量。
// 离线确定性合成与 LLM 合成都实现它（internal/synth）；区别只在文本
// 从哪来，契约不变：每个断言挂窗口，挂不上的不许存在。
type SynthFunc func(question string, windows []EvidenceWindow) (Answer, Usage, error)

// SynthesizeStage 只读窗口与改写。路由是 refuse 时标记拒答、不调合成面；
// 非拒答而没有合成面是配置错误，直接失败——不许悄悄产无证据文本。
type SynthesizeStage struct {
	Query string
	Synth SynthFunc
}

func (SynthesizeStage) Name() string     { return "synthesize" }
func (SynthesizeStage) Reads() []string  { return []string{KeyWindows.String(), KeyRoute.String()} }
func (SynthesizeStage) Writes() []string { return []string{KeyAnswer.String(), KeySynthUsage.String()} }
func (s SynthesizeStage) Run(c *context.Context) error {
	d, ok := context.Get(c, KeyRoute)
	if !ok {
		return errors.New("route missing; stage 3 must run first")
	}
	if d.Action == "refuse" {
		return context.Set(c, KeyAnswer, Answer{Refused: true, Text: ""})
	}
	if s.Synth == nil {
		return errors.New("no synthesizer wired: refusing to emit an uncited answer (misconfiguration fails loud)")
	}
	ws, _ := context.Get(c, KeyWindows)
	ans, usage, err := s.Synth(s.Query, ws)
	if err != nil {
		return err
	}
	if err := context.Set(c, KeyAnswer, ans); err != nil {
		return err
	}
	return context.Set(c, KeySynthUsage, usage)
}

// Verify 强制：非拒答答案的每条引用都能映射回窗口；无引用的断言不允许存在。
func (SynthesizeStage) Verify(c *context.Context) error {
	a, ok := context.Get(c, KeyAnswer)
	if !ok {
		return errors.New("answer missing")
	}
	if a.Refused {
		if a.Text != "" {
			return errors.New("refused answer must not carry text")
		}
		return nil
	}
	ws, _ := context.Get(c, KeyWindows)
	index := make(map[string]bool, len(ws))
	for _, w := range ws {
		index[w.SourceID+"#"+w.Span] = true
	}
	for _, cit := range a.Citations {
		if !index[cit] {
			return fmt.Errorf("citation %q does not resolve to any evidence window", cit)
		}
	}
	if len(a.Citations) == 0 {
		return errors.New("non-refused answer has no citations; every claim must map to a window")
	}
	return nil
}

// ---------- stage 5: 记账 ----------

// AccountStage 把合成步的发生额过到台账。账目和发生额同一个来源
// （KeySynthUsage）——不许两处各写一份。上游不报 usage 时记
// CostKnown=false：成本未知不是 0，且调用方据此停止后续付费调用。
type AccountStage struct{}

func (AccountStage) Name() string     { return "account" }
func (AccountStage) Reads() []string  { return []string{KeySynthUsage.String()} }
func (AccountStage) Writes() []string { return []string{KeyUsage.String()} }
func (AccountStage) Run(c *context.Context) error {
	u, ok := context.Get(c, KeySynthUsage)
	if !ok {
		// 没有合成发生额（例如拒答）：零发生额，但仍要记一笔
		return context.Set(c, KeyUsage, Usage{})
	}
	return context.Set(c, KeyUsage, u)
}
func (AccountStage) Verify(c *context.Context) error {
	u, ok := context.Get(c, KeyUsage)
	if !ok {
		return errors.New("usage missing")
	}
	if !u.CostKnown {
		// 不是错误：显式记录“成本未知”
		return nil
	}
	if u.PromptTokens < 0 || u.CompletionTokens < 0 {
		return errors.New("negative token counts")
	}
	return nil
}

// ---------- stage 6: 学习（可选） ----------

// LearnStage 默认不进流程。只有开启自进化的部署才追加它，
// 且必须由护栏保护（learncore 的职责，v0.1 未接线）。
type LearnStage struct{ Enabled bool }

func (LearnStage) Name() string     { return "learn" }
func (LearnStage) Reads() []string  { return []string{KeyUsage.String()} }
func (LearnStage) Writes() []string { return []string{"knowledge.belief"} }
func (s LearnStage) Run(c *context.Context) error {
	if !s.Enabled {
		return nil // 不写任何 key：不启用就不留痕迹
	}
	// TODO: 受管变更五阶段（learncore）
	return nil
}
func (LearnStage) Verify(c *context.Context) error { return nil }

// Runner 组装问答流程的 stage。学习步按开关取舍：不启用就整个不进流程，
// 不留空壳注册（可选组件的启用必须是显式的）。
func Runner(query string, retrieve func(*context.Context, Rewrite) ([]EvidenceWindow, error), synth SynthFunc, opts Options) *flow.Runner {
	stages := []flow.Stage{
		RewriteStage{Query: query},
		EvidenceStage{Retrieve: retrieve},
		RouteStage{},
		SynthesizeStage{Query: query, Synth: synth},
		AccountStage{},
	}
	if opts.LearnEnabled {
		stages = append(stages, LearnStage{Enabled: true})
	}
	return &flow.Runner{
		Flow:   "qa",
		Stages: stages,
		View: context.CommittedView{
			CorpusVersion:   opts.CorpusVersion,
			ConfigVersion:   opts.ConfigVersion,
			StrategyVersion: opts.StrategyVersion,
			BeliefVersion:   opts.BeliefVersion,
		},
	}
}

// Options 是问答流程的环境版本与开关。四版本进提交视图，
// 缺一项就不可比（流程文法 §五.4）。
type Options struct {
	CorpusVersion   string
	ConfigVersion   string
	StrategyVersion string
	BeliefVersion   string
	LearnEnabled    bool
}

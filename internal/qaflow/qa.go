// Package qaflow 是流程一：一次问答的六个 stage（docs/flow-grammar.md §二）。
//
// stage 划分、契约、验证都是执行处：检索（BM25 + 可选语义重排）、路由
// （便宜信号）、合成（SynthFunc 契约，离线/LLM 两实现）、记账（发生额
// 同一来源）、学习（可选步）。lexical 与语义两把接地尺都在 Verify。
package qaflow

import (
	"errors"
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/ctxmgmt"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/retrieval"
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
)

// Rewrite 是 stage 1 的产物：原问 + 假设文档双视图。
type Rewrite struct {
	Original      string
	Hypothetical  string   // 改写文本（HyDE 式假设摘要）；空 = 用原问检索
	DriftRejected bool     // 改写被漂移闸拦下（内容词丢了原问的）
	DroppedTerms  []string // 被拦下的改写丢了哪些内容词
}

// Effective 是检索实际使用的文本：改写通过漂移闸才用，否则原问。
// 覆盖度始终按 Original 算——改写管召回，原问管接地。
func (r Rewrite) Effective() string {
	if r.Hypothetical != "" && !r.DriftRejected {
		return r.Hypothetical
	}
	return r.Original
}

// EvidenceWindow 是一个证据窗口。SourceID + Span 非空是引用可回溯的
// 最低契约，Verify 强制；Text 是窗口原文——合成面读它，模型看不到
// 原文就等于没有证据（第一次真调用学到的：坐标不是内容）。
type EvidenceWindow struct {
	SourceID  string
	Title     string // 文档身份（法律名/文档题）——合成提示词用它标注窗口
	Span      string
	Text      string
	Score     float64
	Substrate string // text / structured / cross-doc，来自底物注册
}

// RouteDecision 是 stage 3 的路由结论。信号与阈值都留痕，供回放
// “当时为什么升级/拒答”。
type RouteDecision struct {
	Action   string // fast / escalate / refuse
	Signals  RouteSignals
	Grounded bool
	Reason   string
}

// Answer 是 stage 4 的产物。Citations 必须能映射回窗口 id。
type Answer struct {
	Text          string
	Citations     []string // window 标识（SourceID#Span）
	Refused       bool
	RefusalReason string // 拒答理由（模型给的解释）。Text 保持空——
	// 理由不当答案，不许进判官与规则臂（真运行学到的：模型拒答天然
	// 带解释，硬要清空等于把有用信息扔掉，混进 Text 又成了夹带）
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
type RewriteStage struct {
	Query        string
	Hypothetical string           // 注入的改写（HyDE 式）；空 = 不改写
	Idx          *retrieval.Index // 漂移闸判语料内/外用；nil = 闸不启动
}

func (RewriteStage) Name() string     { return "intent-clarify" }
func (RewriteStage) Reads() []string  { return []string{"session"} }
func (RewriteStage) Writes() []string { return []string{KeyRewrite.String()} }
func (s RewriteStage) Run(c *context.Context) error {
	rw := Rewrite{Original: s.Query, Hypothetical: s.Hypothetical}
	// 漂移闸（BioHarness：改写管召回，原问管接地）：注入的改写丢掉原问
	// 的语料内内容词 → 拦下，检索回退原问，丢词清单入账
	if s.Hypothetical != "" && s.Idx != nil {
		var dropped []string
		for _, term := range dedupe(retrieval.Fields(s.Query)) {
			if !s.Idx.HasTerm(term) {
				continue // 语料外词：改写不可能保留，不算漂移
			}
			if !strings.Contains(s.Hypothetical, term) {
				dropped = append(dropped, term)
			}
		}
		if len(dropped) > 0 {
			rw.DriftRejected = true
			rw.DroppedTerms = dropped
			rw.Hypothetical = ""
		}
	}
	return context.Set(c, KeyRewrite, rw)
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

func (EvidenceStage) Name() string    { return "evidence-supply" }
func (EvidenceStage) Reads() []string { return []string{KeyRewrite.String()} }
func (EvidenceStage) Writes() []string {
	return []string{KeyWindows.String(), KeyRerank.String(), KeyDeep.String(), KeyCoverage.String()}
}
func (s EvidenceStage) Run(c *context.Context) error {
	r, ok := context.Get(c, KeyRewrite)
	if !ok {
		return errors.New("rewrite missing; stage 1 must run first")
	}
	// 复用短路：ReuseStage 命中时窗口已在 context 里，本 stage 直接让路——
	// 不调检索后端、不覆盖窗口（复用不是"再查一遍取平均"）
	if rs, hit := context.Get(c, KeyReuseState); hit && rs.Hit {
		return nil
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

// RouteStage 用可观测信号决定 fast / escalate / refuse（04 落点 1）：
// 草稿置信度由三个便宜信号算出——查询词覆盖度、候选区分度、死路率。
// 置信度只当路由代理，不当正确性裁决（BioHarness）；信号与决策全部
// 留痕，事后可回答"这次为什么升级/拒答"。
type RouteStage struct {
	// MinConfidence 是 escalate 阈值。零值用默认 0.5。
	MinConfidence float64
}

func (RouteStage) Name() string { return "sufficiency-route" }
func (RouteStage) Reads() []string {
	return []string{KeyWindows.String(), KeyCoverage.String(), KeyDeep.String()}
}
func (RouteStage) Writes() []string { return []string{KeyRoute.String()} }

func (s RouteStage) Run(c *context.Context) error {
	ws, _ := context.Get(c, KeyWindows)
	grounded := len(ws) > 0
	sig := gatherSignals(c, ws)

	d := RouteDecision{Grounded: grounded, Signals: sig}
	threshold := s.MinConfidence
	if threshold <= 0 {
		threshold = 0.5
	}
	switch {
	case !grounded:
		d.Action = "refuse" // 没有证据：诚实的不知道，不许硬答
		d.Reason = "no evidence window; refuse by grammar"
	case sig.Confidence < threshold:
		d.Action = "escalate"
		d.Reason = fmt.Sprintf("draft confidence %.3f below %.3f (coverage=%.3f margin=%.3f dead-rate=%.3f); escalate",
			sig.Confidence, threshold, sig.Coverage, sig.Margin, sig.DeadRate)
	default:
		d.Action = "fast"
		d.Reason = fmt.Sprintf("draft confidence %.3f (coverage=%.3f margin=%.3f)",
			sig.Confidence, sig.Coverage, sig.Margin)
	}
	return context.Set(c, KeyRoute, d)
}

// RouteSignals 是充足性路由的全部输入事实。全部来自流程内的可观测状态：
// 覆盖度（evidence.coverage）、区分度（窗口打分的头部分差）、死路率
// （deep 遥测）。没有调用方手填的数——手填置信度正是要修掉的旧形态。
type RouteSignals struct {
	Coverage   float64 `json:"coverage"`   // 查询词覆盖度（语料内可达词口径）
	Margin     float64 `json:"margin"`     // (top1-top2)/top1：候选区分度代理，0..1
	DeadRate   float64 `json:"dead_rate"`  // 死路/取样：翻过多少空文档
	Windows    int     `json:"windows"`    // 最终窗口数
	Confidence float64 `json:"confidence"` // 上三项的加权组合（DraftConfidence）
}

// gatherSignals 从 context 采集路由信号。
func gatherSignals(c *context.Context, ws []EvidenceWindow) RouteSignals {
	sig := RouteSignals{Windows: len(ws)}
	if ci, ok := context.Get(c, KeyCoverage); ok {
		sig.Coverage = ci.Value
	}
	if len(ws) >= 2 && ws[0].Score > 0 {
		sig.Margin = (ws[0].Score - ws[1].Score) / ws[0].Score
		if sig.Margin < 0 {
			sig.Margin = 0
		}
		if sig.Margin > 1 {
			sig.Margin = 1
		}
	}
	if tel, ok := context.Get(c, KeyDeep); ok && tel.SampledDocs > 0 {
		sig.DeadRate = float64(tel.DeadEnds) / float64(tel.SampledDocs)
	}
	sig.Confidence = DraftConfidence(sig)
	return sig
}

// DraftConfidence 权重公开可审：覆盖度 0.5（证据到没到位的直接度量）、
// 区分度 0.3（top 与次席分不开 = 没把握）、死路率 0.2（翻过多少空文档，
// 取负）。权重不是魔数，是默认值——改它要走评测对照，不靠感觉。
func DraftConfidence(sig RouteSignals) float64 {
	conf := 0.5*sig.Coverage + 0.3*sig.Margin + 0.2*(1-sig.DeadRate)
	if conf < 0 {
		return 0
	}
	if conf > 1 {
		return 1
	}
	return conf
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
//
// Verify 有两把尺子：
//   - lexical 尺（硬）：每条引用必须映射回窗口；无引用答案不存在；
//   - 语义尺（软，阈值 GroundingFloor > 0 时启用）：答案与证据窗口的
//     向量 cosine 低于地板 → 判“接地不足”。这一把治的是 lexical 抓不到
//     的情况：答案的词都在窗口里出现过，但整句和证据不是一回事
//     （拼接型幻觉）。语义尺不过 → 记 RouteDecision 不升级答案（见下），
//     由路由层改判 escalate/refuse。
type SynthesizeStage struct {
	Query          string
	Synth          SynthFunc
	GroundingFloor float64 // 0 = 不启用语义尺（缺 embedder 时的默认）
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
func (s SynthesizeStage) Verify(c *context.Context) error {
	a, ok := context.Get(c, KeyAnswer)
	if !ok {
		return errors.New("answer missing")
	}
	if a.Refused {
		if a.Text != "" {
			return errors.New("refused answer must not carry text")
		}
		if len(a.Citations) > 0 {
			return errors.New("refused answer must not carry citations")
		}
		// RefusalReason 允许非空（理由不是答案）
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
	// 语义尺（可选）：embedder 绑了才查；没绑跳过——尺子缺席时不许拿
	// lexical 通过冒充语义通过，也不许把流程搞失败（尺子是可选组件）。
	if s.GroundingFloor > 0 {
		emb, ok := context.Get(c, KeyEmbedder)
		if ok && emb != nil {
			cos, err := groundCosine(c, emb, a.Text, ws)
			if err != nil {
				return fmt.Errorf("grounding check failed: %w", err)
			}
			if cos < s.GroundingFloor {
				return fmt.Errorf("semantic grounding %.3f below floor %.3f: answer is not about the evidence", cos, s.GroundingFloor)
			}
		}
	}
	return nil
}

// groundCosine 算答案与全部证据窗口的最大 cosine。取最大而不是平均：
// 一条答案只需被它引用的那些窗口支撑，不是被所有窗口平均支撑。
func groundCosine(c *context.Context, emb embed.Embedder, answer string, ws []EvidenceWindow) (float64, error) {
	if strings.TrimSpace(answer) == "" || len(ws) == 0 {
		return 0, fmt.Errorf("empty answer or no windows")
	}
	texts := make([]string, 0, len(ws)+1)
	texts = append(texts, answer)
	for _, w := range ws {
		texts = append(texts, w.Text)
	}
	vecs, err := emb.Embed(gocontext.Background(), texts)
	if err != nil {
		return 0, err
	}
	if len(vecs) != len(texts) {
		return 0, fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(texts))
	}
	best := 0.0
	for _, wv := range vecs[1:] {
		if cos := embed.Cosine(vecs[0], wv); cos > best {
			best = cos
		}
	}
	return best, nil
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
		RewriteStage{Query: query, Hypothetical: opts.Hypothetical, Idx: opts.RewriteIdx},
		EvidenceStage{Retrieve: retrieve},
		EvictStage{Budget: opts.CtxBudget.WithDefaults()},
		RouteStage{},
		EscalateStage{Retrieve: opts.Escalate},
		SynthesizeStage{Query: query, Synth: synth, GroundingFloor: opts.GroundingFloor},
		AccountStage{},
	}
	if opts.Reuse != nil {
		// 复用查（evidence 前）与复用记（account 后）成对出现：
		// 只查不记，第二次永远冷；只记不查，记了白记。
		// **按名字定位插入，不用下标**：这个列表吃过下标硬编码的亏——
		// Evict/Escalate 插入后按旧下标重建，合成与记账两个 stage 被
		// 静默丢掉（配了复用的流程全在无声跳过合成），HTTP 面首测才
		// 暴露。按名字插，以后再加 stage 也不会错位。
		expanded := make([]flow.Stage, 0, len(stages)+1)
		for _, st := range stages {
			if st.Name() == (EvidenceStage{}).Name() {
				expanded = append(expanded, ReuseStage{Session: opts.Session, Store: opts.Reuse, Query: query})
			}
			expanded = append(expanded, st)
		}
		stages = expanded
	}
	if opts.LearnEnabled {
		stages = append(stages, LearnStage{Enabled: true})
	}
	if opts.Reuse != nil {
		stages = append(stages, ReuseRecordStage{Session: opts.Session, Store: opts.Reuse, Query: query})
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
	// GroundingFloor 语义接地地板：>0 且 embedder 绑定时启用语义尺。
	GroundingFloor float64
	// Reuse 会话复用件：非 nil 时流程在 evidence 前查复用、account 后记录。
	// 同一问题（归一化）再问直接取上轮窗口——"同类问题越问越快"的执行处。
	Reuse   *knowledge.ReuseStore
	Session string
	// Hypothetical 注入改写文本（HyDE 式；空 = 不改写）。漂移闸审核它。
	Hypothetical string
	// RewriteIdx 漂移闸用的索引（判语料内/外词）；nil = 闸不启动。
	RewriteIdx *retrieval.Index
	// Escalate 是升级（FAST→DEEP）的贵路取数函数：路由判 escalate 时
	// 跑它再判一次（BioHarness 级联）。nil = 升级无执行处（死标签，
	// 遥测里可见）。
	Escalate func(*context.Context, Rewrite) ([]EvidenceWindow, error)
	// CtxBudget 合成前的上下文预算（按源配额/语义近重合并/窗口预算）。
	// 零值 = 默认预算（MaxWindows 8 / PerSource 2 / Dedup 0.92——
	// 0.92 是 Volt 论文的合并阈值，不是我们拍的）。
	CtxBudget ctxmgmt.Budget
}

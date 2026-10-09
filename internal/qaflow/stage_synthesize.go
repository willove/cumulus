package qaflow

import (
	gocontext "context"
	"errors"
	"fmt"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/harness"
	"strings"
)

type SynthFunc func(question string, windows []EvidenceWindow, fx facts.Report) (Answer, Usage, error)

// PieceFunc 收一段流式输出（思考与正文分开标）。与 synth.PieceFunc 形状一致，
// 但**在 pipeline 侧重新声明**：pipeline 不能反向依赖 capabilities（boundary
// 门禁会拦），所以这里只声明"流程需要什么形状"，由 apps 层做适配。
type PieceFunc func(piece Piece) error

// Piece 是一段流式输出。
type Piece struct {
	Reasoning string // 思考过程（要全部显示）
	Content   string // 答案正文
}

// StreamSynthFunc 是**可选**的流式合成能力：填了它，流程边收边发事件；
// nil 就走整条路径（合法状态，不是错误）。
//
// 为什么是**显式字段**而不是"看合成器有没有实现某个接口"（真跑踩过）：
// `l.Synthesize` 是**方法值**——传进 Options.Synth 之后它就是一个普通函数类型，
// `SynthesizeStream` 这个方法**在类型层面已经不存在了**，断言永远失败、能力静默
// 消失（真跑表现：流式端点跑通、事件照发、但正文仍是整段 replace，一眼看不出来）。
// 能力靠**接线处声明**，不靠运行时猜。
type StreamSynthFunc func(question string, windows []EvidenceWindow, fx facts.Report, fn PieceFunc) (Answer, Usage, error)

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
	// Emit 非 nil 且合成器实现了 StreamSynth 时，**边收边发** reasoning/content
	// 事件（思考过程要"全部显示"，那就必须边到边显示，而不是等跑完再倒出来）。
	// 三种情况都合法：没有 emitter（不发）、合成器不支持流式（走整条）、
	// 消费者断开（事件出口吞掉错误，问答照常——降级可见见 harness 契约 3）。
	Stream StreamSynthFunc // 非 nil = 合成器支持流式（能力在接线处声明）
	Emit   *harness.Emitter
	RunID  string
}

func (SynthesizeStage) Name() string { return "synthesize" }
func (SynthesizeStage) Reads() []string {
	return []string{KeyWindows.String(), KeyRoute.String(), KeyAbstain.String()}
}
func (SynthesizeStage) Writes() []string { return []string{KeyAnswer.String(), KeySynthUsage.String()} }
func (s SynthesizeStage) synthFunc() SynthFunc {
	if s.Stream == nil || s.Emit == nil {
		return s.Synth // 没装出口或合成器不支持流式 = 整条路径（不是错误）
	}
	return func(q string, ws []EvidenceWindow, fx facts.Report) (Answer, Usage, error) {
		return s.Stream(q, ws, fx, s.pieces)
	}
}

// pieces 把流式片段翻成事件：思考与正文**分帧**，不混。
func (s SynthesizeStage) pieces(p Piece) error {
	if p.Reasoning != "" {
		if ev, err := harness.Reasoning(s.RunID, p.Reasoning); err == nil {
			_ = s.Emit.Emit(ev)
		}
	}
	if p.Content != "" {
		// 增量 append（replace 留空）：合成器的流式输出就是增量。
		if ev, err := harness.Content(s.RunID, p.Content, false); err == nil {
			_ = s.Emit.Emit(ev)
		}
	}
	return nil
}

func (s SynthesizeStage) Run(c *context.Context) error {
	d, ok := context.Get(c, KeyRoute)
	if !ok {
		return errors.New("route missing; stage 3 must run first")
	}
	if d.Action == "refuse" {
		return context.Set(c, KeyAnswer, Answer{Refused: true, Text: ""})
	}
	// abstain 早弃权：预测头说这题答不了（无样本 + 高 p_fail）——不烧
	// DEEP，直接拒（cumulus：这类题升级是纯燃烧）。理由带出来可查。
	if v, ok := context.Get(c, KeyAbstain); ok && v.Action == "refuse" && d.Action != "refuse" {
		return context.Set(c, KeyAnswer, Answer{Refused: true, Text: "", RefusalReason: v.Reason})
	}
	if s.Synth == nil {
		return errors.New("no synthesizer wired: refusing to emit an uncited answer (misconfiguration fails loud)")
	}
	ws, _ := context.Get(c, KeyWindows)
	fx, _ := context.Get(c, KeyFactReport)
	ans, usage, err := s.synthFunc()(s.Query, ws, fx)
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

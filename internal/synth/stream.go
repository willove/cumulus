package synth

import (
	gocontext "context"
	"fmt"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
)

// StreamPiece 是合成面流式吐出来的一段：**思考**与**正文**分开标。
//
// 为什么分开（而不是一个 Text）：产品里这是两件事——"全部思考过程"要显示，
// "答案正文"要交付。混在一个流里，调用方只能靠猜哪段是思考（真跑踩过：
// 推理模型的 reasoning_content 混进正文，前端把思考当答案念出来）。
type StreamPiece struct {
	Reasoning string
	Content   string
}

// PieceFunc 收一段流式输出。返回 error 时合成立刻中止。
type PieceFunc func(StreamPiece) error

// StreamSynthFunc 是**可选**的流式合成能力。qaflow.SynthFunc 不实现它就是
// "只能整段拿"，而那是**合法状态**：调用方问一句
// `if s, ok := synth.(qaflow.StreamSynth)` 就行，不必让所有合成器都实现
// （离线合成器、测试替身根本不需要流式）。
type StreamSynthFunc func(question string, windows []qaflow.EvidenceWindow, fx facts.Report, fn PieceFunc) (qaflow.Answer, qaflow.Usage, error)

// SupportsStream 问一句"这个合成器支持流式吗"（读数可见，不靠猜）。
// 形参取 any：qaflow.SynthFunc 是**函数类型**（不能直接做类型断言到接口），
// 所以调用方把合成器当值传进来。
func SupportsStream(f any) bool {
	_, ok := f.(StreamSynth)
	return ok
}

// StreamSynth 是实现了流式合成的合成器（能力标记接口）。
type StreamSynth interface {
	SynthesizeStream(question string, windows []qaflow.EvidenceWindow, fx facts.Report, fn PieceFunc) (qaflow.Answer, qaflow.Usage, error)
}

// SynthesizeStream 是 LLM 合成的**流式**路径。
//
// 三条纪律：
//  1. **能流才流**：`l.Client` 不实现 llm.Streamer 时，直接退回调
//     Synthesize（不假装流式、不换实现）；
//  2. **思考照发**：`reasoning_content` 逐段透给调用方（用户要"全部显示"），
//     但**不参与解析**——解析只看最终正文，思考里挖 JSON 仍是 llm 层的
//     最后兜底，不是合成面该管的事（真跑教训：JSON 兜底散在两处，
//     两个地方的口径迟早不一致）；
//  3. **同一请求两条路一致**：流式只是取答案的方式不同，最终 Answer 必须与
//     Synthesize 逐字段一致（同一份 parseSynthesis）。
func (l *LLM) SynthesizeStream(question string, windows []qaflow.EvidenceWindow, fx facts.Report, fn PieceFunc) (qaflow.Answer, qaflow.Usage, error) {
	if l == nil || l.Client == nil {
		return qaflow.Answer{}, qaflow.Usage{}, llm.ErrNotConfigured
	}
	st, ok := l.Client.(llm.Streamer)
	if !ok {
		// 上游不支持流式：退回整条路径（诚实降级，不是错误）。
		return l.Synthesize(question, windows, fx)
	}
	if len(windows) == 0 {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth llm: no evidence windows")
	}
	prompt, rendered := buildSynthesisPrompt(question, windows, fx)
	renderLabels := make([]string, len(rendered))
	for i := range renderLabels {
		renderLabels[i] = fmt.Sprintf("w%d", i+1)
	}
	var emitErr error
	resp, err := st.Stream(gocontext.Background(), llm.Request{
		System:    synthesisSystemPrompt,
		Prompt:    prompt,
		MaxTokens: 1024,
	}, func(ch llm.Chunk) error {
		if ch.Reasoning == "" && ch.Content == "" {
			return nil
		}
		if fn == nil {
			return nil
		}
		if e := fn(StreamPiece{Reasoning: ch.Reasoning, Content: ch.Content}); e != nil {
			emitErr = e
			return e // 中止流：下游已经不要了，别继续烧 token
		}
		return nil
	})
	if err != nil {
		if emitErr != nil {
			return qaflow.Answer{}, qaflow.Usage{}, emitErr
		}
		return qaflow.Answer{}, qaflow.Usage{}, err
	}
	ans, err := parseSynthesis(resp.Text, rendered, renderLabels)
	if err != nil {
		return qaflow.Answer{}, qaflow.Usage{}, err
	}
	return ans, qaflow.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown:        resp.Usage.CostKnown,
	}, nil
}

var _ StreamSynth = (*LLM)(nil)

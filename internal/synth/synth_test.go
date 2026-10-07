package synth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
)

// fakeCompleter 回放预定文本；usage 可编排（含“上游不报”）。
type fakeCompleter struct {
	text  string
	usage usage2
}

type usage2 struct {
	prompt, completion int
	costKnown          bool
}

func (f *fakeCompleter) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{
		Text: f.text,
		Usage: llm.Usage{
			PromptTokens:     f.usage.prompt,
			CompletionTokens: f.usage.completion,
			CostKnown:        f.usage.costKnown,
		},
	}, nil
}

func windows() []qaflow.EvidenceWindow {
	return []qaflow.EvidenceWindow{
		{SourceID: "law-1", Span: "rune[0:10]", Score: 5.5, Substrate: "text"},
		{SourceID: "ops-1", Span: "rune[0:8]", Score: 3.2, Substrate: "text"},
	}
}

// 合法 JSON：断言挂上窗口，引用换成坐标。
func TestLLMSynthesizeParsesContract(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{
		text:  `{"answer":"100","assertions":[{"text":"默认为100","window":"w1"},{"text":"端口8484","window":"w2"}]}`,
		usage: usage2{prompt: 100, completion: 20, costKnown: true},
	}}
	ans, usage, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "100" {
		t.Fatalf("want answer 100, got %q", ans.Text)
	}
	if len(ans.Citations) != 2 || ans.Citations[0] != "law-1#rune[0:10]" || ans.Citations[1] != "ops-1#rune[0:8]" {
		t.Fatalf("citations must resolve to coordinates: %v", ans.Citations)
	}
	if !usage.CostKnown || usage.PromptTokens != 100 || usage.CompletionTokens != 20 {
		t.Fatalf("usage must pass through: %+v", usage)
	}
}

// 引用不存在的窗口（模型编凭据）：整份输出转拒答——不 500 给用户，
// 也不放一半答案出去（契约是"要么全过要么不过"）。
func TestLLMSynthesizeUnknownWindowBecomesRefusal(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"100","assertions":[{"text":"x","window":"w9"}]}`}}
	ans, _, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatalf("编凭据必须转拒答而不是报错：%v", err)
	}
	if !ans.Refused {
		t.Fatalf("必须 refused：%+v", ans)
	}
}

// 输出不是契约 JSON：结构化错误，调用方重试，不许当答案。
func TestLLMSynthesizeRejectsMalformedOutput(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: "我觉得答案是100"}}
	if _, _, err := l.Synthesize("q", windows(), facts.Report{}); err == nil || !strings.Contains(err.Error(), "JSON contract") {
		t.Fatalf("malformed output must fail, got %v", err)
	}
}

// 有答案但零断言：不当错误抛（500 给用户），也不照发（无引用的话进答
// 案）——转成正式拒答，模型原话进 RefusalReason（小模型在分组提示下
// 偶发这个形态：对没把握的那条只写散文不挂引用）。
func TestLLMSynthesizeUncitedAnswerBecomesRefusal(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"现有证据未提供具体年限","assertions":[]}`}}
	ans, _, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatalf("零断言答案必须转拒答而不是报错：%v", err)
	}
	if !ans.Refused || ans.RefusalReason != "现有证据未提供具体年限" {
		t.Fatalf("必须 refused 且原话进理由：%+v", ans)
	}
}

// 上游不报 usage：CostKnown=false 一路带到台账。
func TestLLMSynthesizePropagatesCostUnknown(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{
		text:  `{"answer":"100","assertions":[{"text":"x","window":"w1"}]}`,
		usage: usage2{prompt: 10, completion: 5, costKnown: false},
	}}
	_, usage, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostKnown {
		t.Fatal("cost unknown must stay unknown, not become 0")
	}
}

// 没配提供方：明说，不许静默换桩。
func TestLLMWithoutClientErrors(t *testing.T) {
	if _, _, err := (&LLM{}).Synthesize("q", windows(), facts.Report{}); !errors.Is(err, llm.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// 容忍 ```json 围栏。
func TestLLMSynthesizeToleratesFencedOutput(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: "```json\n{\"answer\":\"100\",\"assertions\":[{\"text\":\"x\",\"window\":\"w1\"}]}\n```"}}
	ans, _, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) != 1 {
		t.Fatalf("fenced output must parse, got %v", ans.Citations)
	}
}

// 离线合成：确定性，无窗口即失败（和 LLM 版同一个门槛）。
func TestOfflineSynthesize(t *testing.T) {
	ans, _, err := Offline("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) != 2 {
		t.Fatalf("offline synthesis must cite every window, got %v", ans.Citations)
	}
	if _, _, err := Offline("q", nil, facts.Report{}); err == nil {
		t.Fatal("no windows must fail")
	}
}

// 拒答协议（真实运行学到的）：refused=true 且空答案空断言 → 合法拒答。
func TestLLMSynthesizeAcceptsRefusalProtocol(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{
		text:  `{"answer":"","assertions":[],"refused":true}`,
		usage: usage2{prompt: 10, completion: 2, costKnown: true},
	}}
	ans, usage, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Refused {
		t.Fatal("refusal must be honored")
	}
	if ans.Text != "" || len(ans.Citations) != 0 {
		t.Fatalf("refused answer must be empty: %+v", ans)
	}
	if !usage.CostKnown {
		t.Fatal("refusal still costs tokens; usage must pass through")
	}
}

// 拒答夹带答案/断言：混日子，按错误处理。
func TestLLMSynthesizeRefusalReasonIsKeptSeparate(t *testing.T) {
	// 模型拒答天然带解释（真运行学到的）：理由进 RefusalReason，
	// 不进 Text、不许带断言
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"现有证据不足以确定","assertions":[],"refused":true}`}}
	ans, _, err := l.Synthesize("q", windows(), facts.Report{})
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Refused || ans.Text != "" {
		t.Fatalf("reason must not leak into answer text: %+v", ans)
	}
	if ans.RefusalReason != "现有证据不足以确定" {
		t.Fatalf("reason must be preserved: %q", ans.RefusalReason)
	}
}

func TestLLMSynthesizeRejectsRefusalWithAssertions(t *testing.T) {
	// 既说不知道又摆断言：自相矛盾，按错误处理
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"不知道","assertions":[{"text":"x","window":"w1"}],"refused":true}`}}
	if _, _, err := l.Synthesize("q", windows(), facts.Report{}); err == nil {
		t.Fatal("refusal with assertions must be rejected")
	}
}

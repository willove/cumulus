package synth

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	ans, usage, err := l.Synthesize("q", windows())
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

// 引用不存在的窗口：整体失败，不许放一半答案出去。
func TestLLMSynthesizeRejectsUnknownWindow(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"100","assertions":[{"text":"x","window":"w9"}]}`}}
	if _, _, err := l.Synthesize("q", windows()); err == nil || !strings.Contains(err.Error(), "unknown window") {
		t.Fatalf("unknown window must fail the whole synthesis, got %v", err)
	}
}

// 输出不是契约 JSON：结构化错误，调用方重试，不许当答案。
func TestLLMSynthesizeRejectsMalformedOutput(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: "我觉得答案是100"}}
	if _, _, err := l.Synthesize("q", windows()); err == nil || !strings.Contains(err.Error(), "JSON contract") {
		t.Fatalf("malformed output must fail, got %v", err)
	}
}

// 没有窗口-backed 断言：宁可失败也不出无引用答案。
func TestLLMSynthesizeRejectsUncitedAnswer(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"answer":"100","assertions":[]}`}}
	if _, _, err := l.Synthesize("q", windows()); err == nil {
		t.Fatal("uncited answer must be rejected")
	}
}

// 上游不报 usage：CostKnown=false 一路带到台账。
func TestLLMSynthesizePropagatesCostUnknown(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{
		text:  `{"answer":"100","assertions":[{"text":"x","window":"w1"}]}`,
		usage: usage2{prompt: 10, completion: 5, costKnown: false},
	}}
	_, usage, err := l.Synthesize("q", windows())
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostKnown {
		t.Fatal("cost unknown must stay unknown, not become 0")
	}
}

// 没配提供方：明说，不许静默换桩。
func TestLLMWithoutClientErrors(t *testing.T) {
	if _, _, err := (&LLM{}).Synthesize("q", windows()); !errors.Is(err, llm.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// 容忍 ```json 围栏。
func TestLLMSynthesizeToleratesFencedOutput(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: "```json\n{\"answer\":\"100\",\"assertions\":[{\"text\":\"x\",\"window\":\"w1\"}]}\n```"}}
	ans, _, err := l.Synthesize("q", windows())
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) != 1 {
		t.Fatalf("fenced output must parse, got %v", ans.Citations)
	}
}

// 离线合成：确定性，无窗口即失败（和 LLM 版同一个门槛）。
func TestOfflineSynthesize(t *testing.T) {
	ans, _, err := Offline("q", windows())
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) != 2 {
		t.Fatalf("offline synthesis must cite every window, got %v", ans.Citations)
	}
	if _, _, err := Offline("q", nil); err == nil {
		t.Fatal("no windows must fail")
	}
}

package judge

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/llm"
)

type fakeCompleter struct{ text string }

func (f *fakeCompleter) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Text: f.text, Usage: llm.Usage{PromptTokens: 5, CompletionTokens: 1, CostKnown: true}}, nil
}

func TestJudgeYes(t *testing.T) {
	j := &LLM{Client: &fakeCompleter{text: "YES"}}
	v, err := j.Judge("q", "默认为 100", "100")
	if err != nil || !v.OK {
		t.Fatalf("want yes, got %v %v", v.OK, err)
	}
	if v.PromptTokens != 5 || v.CompletionTokens != 1 {
		t.Fatalf("judge tokens must come back for the bill: %+v", v)
	}
}

func TestJudgeNo(t *testing.T) {
	j := &LLM{Client: &fakeCompleter{text: "no"}}
	v, err := j.Judge("q", "8484", "100")
	if err != nil || v.OK {
		t.Fatalf("want no, got %v %v", v.OK, err)
	}
}

// 判不了就是判不了：不许猜。
func TestJudgeUnparseableFails(t *testing.T) {
	j := &LLM{Client: &fakeCompleter{text: "也许吧"}}
	if _, err := j.Judge("q", "a", "b"); err == nil {
		t.Fatal("unparseable verdict must fail")
	}
}

func TestJudgeWithoutClientFails(t *testing.T) {
	if _, err := (&LLM{}).Judge("q", "a", "b"); err == nil {
		t.Fatal("no client must fail")
	}
}

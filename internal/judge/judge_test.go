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

// 中文判定词必须认（提示词是中文，中文模型答中文——只认 YES/NO 等于
// 对中文模型没有判官）。
func TestJudgeChineseVerdicts(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"是", true},
		{"等价", true},
		{"对", true},
		{"否", false},
		{"不等价", false},
	}
	for _, c := range cases {
		v, err := (&LLM{Client: &fakeCompleter{text: c.text}}).Judge("q", "a", "b")
		if err != nil {
			t.Fatalf("%q must parse: %v", c.text, err)
		}
		if v.OK != c.want {
			t.Fatalf("%q must give %v", c.text, c.want)
		}
	}
}

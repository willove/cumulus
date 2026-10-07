package query

import (
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/llm"
)

type fakeCompleter struct{ text string }

func (f *fakeCompleter) Complete(_ gocontext.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Text: f.text}, nil
}

func TestExpandParsesContract(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"keywords":["饲养动物","噪声扰民","社会生活噪声"]}`}}
	got, err := l.Expand(gocontext.Background(), "养狗叫得太吵谁管")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "饲养动物" {
		t.Fatalf("want the expanded keywords, got %v", got)
	}
}

func TestExpandRejectsMalformed(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: "我觉得应该搜 饲养动物 和 噪声"}}
	if _, err := l.Expand(gocontext.Background(), "q"); err == nil {
		t.Fatal("non-contract output must fail (不许把废话当检索词)")
	}
}

func TestExpandEmptyKeywordsFails(t *testing.T) {
	l := &LLM{Client: &fakeCompleter{text: `{"keywords":[]}`}}
	if _, err := l.Expand(gocontext.Background(), "q"); err == nil {
		t.Fatal("empty keyword list must fail")
	}
}

func TestExpandWithoutClient(t *testing.T) {
	if _, err := (&LLM{}).Expand(gocontext.Background(), "q"); err == nil {
		t.Fatal("no client must fail")
	}
}

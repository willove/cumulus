package qaflow

import (
	"errors"
	"strings"
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/retrieval"
)

// fakeEmbedder 确定性假向量：词表命中法。vec = 词表中出现在 text 里的词
// 计数（L2 归一）。可控地造出“哪个文档和查询语义更近”。
type fakeEmbedder struct {
	vocab  []string
	dims   int
	failOn bool
}

func (f *fakeEmbedder) Dims() int { return f.dims }

func (f *fakeEmbedder) Embed(_ gocontext.Context, texts []string) ([][]float32, error) {
	if f.failOn {
		return nil, errors.New("embedder down")
	}
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		v := make([]float32, f.dims)
		for i, w := range f.vocab {
			if contains(t, w) {
				v[i] = 1
			}
		}
		out = append(out, embed.L2Norm(v))
	}
	return out, nil
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// rerankHits 单测：语义分高的排前面；同分保原序；nil 项保位。
func TestRerankHitsReordersBySemantics(t *testing.T) {
	c := context.New("default")
	fake := &fakeEmbedder{vocab: []string{"端口", "连接池"}, dims: 2}
	if err := context.Set[embed.Embedder](c, KeyEmbedder, fake); err != nil {
		t.Fatal(err)
	}
	hits := []retrieval.Hit{
		{DocID: "a", SpanText: "连接池配置说明"},     // 与“端口是多少”语义 0
		{DocID: "b", SpanText: "服务端口默认 8484"}, // 语义 1
	}
	got, state := rerankHits(c, hits, "默认端口是多少", 50)
	if !state.Applied {
		t.Fatalf("embedder bound must apply: %+v", state)
	}
	if got[0].DocID != "b" || got[1].DocID != "a" {
		t.Fatalf("semantic winner must come first, got %v", []string{got[0].DocID, got[1].DocID})
	}
}

// embedder 缺席：保序 + 原因留痕（degraded, not dropped, and visible）。
func TestRerankSkippedWithoutEmbedder(t *testing.T) {
	c := context.New("default")
	hits := []retrieval.Hit{
		{DocID: "a", SpanText: "x"},
		{DocID: "b", SpanText: "y"},
	}
	got, state := rerankHits(c, hits, "q", 50)
	if state.Applied || state.Reason == "" {
		t.Fatalf("missing embedder must skip with a reason: %+v", state)
	}
	if got[0].DocID != "a" || got[1].DocID != "b" {
		t.Fatal("skipped rerank must preserve BM25 order")
	}
}

// embed 失败：保序 + 原因留痕。不许半份重排。
func TestRerankSkipsOnEmbedFailure(t *testing.T) {
	c := context.New("default")
	if err := context.Set[embed.Embedder](c, KeyEmbedder, &fakeEmbedder{vocab: []string{"a"}, dims: 1, failOn: true}); err != nil {
		t.Fatal(err)
	}
	hits := []retrieval.Hit{{DocID: "a", SpanText: "a"}, {DocID: "b", SpanText: "b"}}
	got, state := rerankHits(c, hits, "a", 50)
	if state.Applied || state.Reason == "" {
		t.Fatalf("embed failure must skip with reason: %+v", state)
	}
	if got[0].DocID != "a" {
		t.Fatal("order must survive")
	}
}

// 分类器驱动：绑 embedder → 组件激活；撤销 → 停用。
func TestSemanticRerankFollowsClassifier(t *testing.T) {
	c := context.New("default")
	comp := &SemanticRerank{}
	c.RegisterComponent(comp)
	if comp.Active() {
		t.Fatal("must start inactive")
	}
	if err := context.Set[embed.Embedder](c, KeyEmbedder, &fakeEmbedder{dims: 2}); err != nil {
		t.Fatal(err)
	}
	if !comp.Active() {
		t.Fatal("bind must activate")
	}
	_ = c.UnwindTo(0)
	if comp.Active() {
		t.Fatal("unwind must deactivate")
	}
}

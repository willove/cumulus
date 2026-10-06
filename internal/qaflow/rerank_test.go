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

// 语义尺：答案词都在窗口里（lexical 全过），但整句与证据不是一回事
// （拼接型幻觉）——语义尺必须拦下。这正是 lexical 尺结构上抓不到的那类。
func TestGroundingScaleCatchesLexicalPassingHallucination(t *testing.T) {
	c := context.New("default")
	// 词表两点：维度0=“连接池”（证据有），维度1=“财务报表”（答案硬凑）
	fake := &fakeEmbedder{vocab: []string{"连接池", "财务报表"}, dims: 2}
	if err := context.Set[embed.Embedder](c, KeyEmbedder, fake); err != nil {
		t.Fatal(err)
	}
	// 证据窗口只含“连接池”语义
	if err := context.Set(c, KeyWindows, []EvidenceWindow{
		{SourceID: "law-1", Span: "rune[0:5]", Text: "连接池默认配置", Score: 5},
	}); err != nil {
		t.Fatal(err)
	}
	// 答案 lexically 引用合法（引用能映射回窗口），但语义半句是财务报表
	ans := Answer{
		Text:      "连接池与财务报表均见上文",
		Citations: []string{"law-1#rune[0:5]"},
	}
	if err := context.Set(c, KeyAnswer, ans); err != nil {
		t.Fatal(err)
	}
	stage := SynthesizeStage{GroundingFloor: 0.9}
	err := stage.Verify(c)
	if err == nil || !strings.Contains(err.Error(), "grounding") {
		t.Fatalf("semantic scale must catch lexical-passing hallucination, got %v", err)
	}
}

// 语义尺通过：答案与证据同向。
func TestGroundingScalePassesAlignedAnswer(t *testing.T) {
	c := context.New("default")
	fake := &fakeEmbedder{vocab: []string{"连接池", "财务报表"}, dims: 2}
	if err := context.Set[embed.Embedder](c, KeyEmbedder, fake); err != nil {
		t.Fatal(err)
	}
	if err := context.Set(c, KeyWindows, []EvidenceWindow{
		{SourceID: "law-1", Span: "rune[0:5]", Text: "连接池默认配置", Score: 5},
	}); err != nil {
		t.Fatal(err)
	}
	ans := Answer{Text: "连接池默认配置", Citations: []string{"law-1#rune[0:5]"}}
	if err := context.Set(c, KeyAnswer, ans); err != nil {
		t.Fatal(err)
	}
	if err := (SynthesizeStage{GroundingFloor: 0.9}).Verify(c); err != nil {
		t.Fatalf("aligned answer must pass, got %v", err)
	}
}

// 尺子缺席（没绑 embedder）：语义尺跳过，lexical 尺说了算——
// 尺子缺席不许冒充通过，也不许把流程搞失败。
func TestGroundingScaleSkippedWithoutEmbedder(t *testing.T) {
	c := context.New("default")
	if err := context.Set(c, KeyWindows, []EvidenceWindow{
		{SourceID: "law-1", Span: "rune[0:5]", Text: "任意证据", Score: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := context.Set(c, KeyAnswer, Answer{Text: "完全无关的话", Citations: []string{"law-1#rune[0:5]"}}); err != nil {
		t.Fatal(err)
	}
	if err := (SynthesizeStage{GroundingFloor: 0.9}).Verify(c); err != nil {
		t.Fatalf("scale absent must skip, not fail: %v", err)
	}
}

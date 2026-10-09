package docgen

import (
	gocontext "context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/retrieval"
)

type stubLLM struct{ text string }

func (s stubLLM) Complete(_ gocontext.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{Text: s.text}, nil
}

func hits() []retrieval.Hit {
	return []retrieval.Hit{
		{DocID: "d1", SpanCoord: "rune[0:4]", SpanText: "专��年费三百八十元，逾期补缴要加滞纳金。", Score: 9},
		{DocID: "d2", SpanCoord: "rune[8:14]", SpanText: "实用新型年费一百九十元。", Score: 6},
	}
}

// 正常路径：结构化条目 + 引用校验通过 + 正文可检索。
func TestGenerateAssemblesGroundedDocument(t *testing.T) {
	g := &Generator{Client: stubLLM{text: `{"title":"专利年费怎么交","sections":[` +
		`{"heading":"年费标准","claims":[{"text":"发明专利年费三百八十元","source":1},{"text":"实用新型一百九十元","source":2}]},` +
		`{"heading":"逾期怎么办","claims":[{"text":"可在期满后六个月内补缴","source":1}]},` +
		`{"heading":"","claims":[{"text":"空节应被丢掉","source":1}]}],` +
		`"gaps":[{"heading":"减免政策","note":"语料里没有减免条款"}]}`}}
	doc, err := g.Generate(gocontext.Background(), "专利每年要交多少钱", hits())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 2 {
		t.Fatalf("空节必须被丢掉: %+v", doc.Sections)
	}
	if doc.Sections[0].Claims[0].SourceID != "d1" || doc.Sections[0].Claims[1].SourceID != "d2" {
		t.Fatalf("claims must carry the source doc: %+v", doc.Sections[0].Claims)
	}
	if len(doc.Gaps) != 1 || doc.Gaps[0].Heading != "减免政策" {
		t.Fatalf("gaps must survive（边界要显式）: %+v", doc.Gaps)
	}
	if doc.Coverage.WindowsTotal != 2 || doc.Coverage.WindowsUsed != 2 {
		t.Fatalf("coverage must be honest: %+v", doc.Coverage)
	}
	// 正文要能被检索命中（进语料后问答要引得到）
	for _, want := range []string{"# 专利年费怎么交", "## 年费标准", "三百八十元", "[d1]"} {
		if !strings.Contains(doc.Body, want) {
			t.Fatalf("正文缺 %q：\n%s", want, doc.Body)
		}
	}
}

// **引用越界就是幻觉**：该条丢弃，不进文档（纪律 1 的落点）。
func TestGenerateDropsClaimsWithOutOfRangeCitation(t *testing.T) {
	g := &Generator{Client: stubLLM{text: `{"title":"T","sections":[{"heading":"H","claims":[` +
		`{"text":"真的","source":1},{"text":"引用了不存在的证据","source":9},{"text":"零号","source":0}]}]}`}}
	doc, err := g.Generate(gocontext.Background(), "q", hits())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections[0].Claims) != 1 || doc.Sections[0].Claims[0].Text != "真的" {
		t.Fatalf("越界引用必须丢弃: %+v", doc.Sections[0].Claims)
	}
	if strings.Contains(doc.Body, "不存在的证据") {
		t.Fatalf("幻觉内容不许进正文:\n%s", doc.Body)
	}
}

// 半截文档**不产出**：一条都挂不上引用时整体报错——半截文档会被当成可信资料存进语料。
func TestGenerateRefusesHalfDocument(t *testing.T) {
	for name, text := range map[string]string{
		"全越界":   `{"title":"T","sections":[{"heading":"H","claims":[{"text":"x","source":9}]}]}`,
		"空节":    `{"title":"T","sections":[]}`,
		"没JSON": "我认为应当这样写……",
		"空回包":   "",
	} {
		g := &Generator{Client: stubLLM{text: text}}
		doc, err := g.Generate(gocontext.Background(), "q", hits())
		if err == nil {
			t.Errorf("%s：必须报错，实际给了 %+v", name, doc)
		}
		if doc != nil {
			t.Errorf("%s：失败时不得返回半截文档", name)
		}
	}
}

// JSON 埋在解释文字里也要救（推理模型的常见形状）。
func TestGenerateRescuesJSONFromProse(t *testing.T) {
	g := &Generator{Client: stubLLM{text: "我按要求写：\n下面是文档。\n" +
		`{"title":"T","sections":[{"heading":"H","claims":[{"text":"真","source":1}]}]}` + "\n以上。"}}
	doc, err := g.Generate(gocontext.Background(), "q", hits())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("rescue failed: %+v", doc.Sections)
	}
}

// 缺席与空证据是**错误**（不是"生成一篇空文档"）。
func TestGenerateAbsentIsError(t *testing.T) {
	var nilG *Generator
	if _, err := nilG.Generate(gocontext.Background(), "q", hits()); err == nil {
		t.Fatal("nil generator must error")
	}
	g := &Generator{}
	if _, err := g.Generate(gocontext.Background(), "q", hits()); err == nil {
		t.Fatal("generator without client must error（生成能力缺席要说出来）")
	}
	if _, err := g.Generate(gocontext.Background(), "  ", hits()); err == nil {
		t.Fatal("empty topic must error")
	}
	g2 := &Generator{Client: stubLLM{text: "{}"}}
	if _, err := g2.Generate(gocontext.Background(), "q", nil); err == nil {
		t.Fatal("no evidence must error（先检索到东西再生成）")
	}
}

// 生成文档的**来源标记与降权口径**：marker 幂等、id 前缀可识别。
// 口径分两处（marker 认正文、前缀认 id）——两处必须一致，所以钉在一起测。
func TestGeneratedMarkerIsIdempotentAndDetectable(t *testing.T) {
	body := "# 标题\n\n- 论断 [d1]\n"
	if IsGenerated(body) {
		t.Fatal("普通文档不该被认成生成文档")
	}
	once := ApplyMarker(body)
	if !IsGenerated(once) {
		t.Fatal("marker 写入后必须可识别")
	}
	if twice := ApplyMarker(once); twice != once {
		t.Fatal("marker 必须幂等（写两次不该叠两行）")
	}
	if BoostGenerated >= 1 {
		t.Fatalf("生成文档权重必须低于源文档（否则永远排在源后面）: %v", BoostGenerated)
	}
}

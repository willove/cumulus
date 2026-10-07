package retrieval

import (
	"strings"
	"testing"
)

// 条文吸附：法律窗口从"第X条"边界开剪（合成面要完整条目，不要半句话）
func TestWindowSnapsToArticleBoundaries(t *testing.T) {
	body := "序言部分甲乙丙。第二十二条　　这是甲内容，讲无关的事情延续一段文字。第二十三条　　公安机关交通管理部门对机动车驾驶人的道路交通安全违法行为除给予行政处罚外，实行累积记分制度。第二十四条　　后面的内容。"
	idx := Build([]Document{{ID: "law", Body: body}})
	coord := idx.Window("law", []string{"累积记分"}, 40)
	text, err := ResolveSpan(body, coord)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "第二十三条") {
		t.Fatalf("window must start at the containing article, got %q", text[:40])
	}
	if strings.Contains(text, "第二十四条") {
		t.Fatalf("window must end before the next article, got %q", text)
	}
	if !strings.Contains(text, "累积记分") {
		t.Fatal("window must still contain the hit")
	}
}

// 命中密度定心：查询词散落多处的文档，窗口必须落在词最密的那段——
// 不是最早出现的位置（法律名里就带查询词，取最早则窗口永远停在开头）
func TestWindowCentersOnHitDensity(t *testing.T) {
	body := "中华人民共和国道路交通法示例\n> 序言甲乙丙。\n第一条　　早期的交通规定，内容平常。\n第五条　　信号灯交通规则：交通信号灯由红灯、绿灯、黄灯组成，红灯表示禁止通行，绿灯表示准许通行。\n第九条　　末尾交通内容。"
	idx := Build([]Document{{ID: "law", Body: body}})
	coord := idx.Window("law", []string{"交通信号灯", "红灯", "表示"}, 80)
	text, _ := ResolveSpan(body, coord)
	if !strings.Contains(text, "红灯表示禁止通行") {
		t.Fatalf("window must center on the dense hit (信号灯那条), got %q", text)
	}
	if strings.HasPrefix(text, "中华人民共和国") {
		t.Fatal("window must not sit at the document head just because the law name contains 交通")
	}
}

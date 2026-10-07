package synth

import (
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/qaflow"
)

// buildSynthesisPrompt 构造合成提示词，返回提示词与**实际渲染的窗集**
// （按标签序）。K≥2 的多事实问句走两层上下文（Noesis 的形状，
// arXiv:2609.07663）：先事实骨架（每条事实 + 有无支撑），再逐事实给窗
// 口原文。理由是该论文的核心测定：7B 以下模型的瓶颈不是检索质量而是
// **上下文利用**——正确证据平铺在上下文里，小模型照样丢失"哪条事实有
// 证据"然后编合理的数。分组是把结构喂到它嘴边。
//
// 两条真跑教训钉在这份代码里：
//  1. 分组在**最终窗集**上现算（facts.SupportsOf），不用事实报告里的
//     supports——报告产生于驱逐前，它的 supports 可能指向已被驱逐的窗，
//     拿过期归属喂模型，模型看到的就是"这条事实没有证据"；
//  2. 分组视图**重新连续编号**（w1..wN 按渲染序）——沿用原窗序号会跳号
//     （[w1][w4][w7]），模型被跳号搞晕，开始引用空标签/不存在的标签
//     （实测三次运行三种错：空 window、unknown window、答非所问）。
//     渲染序与解析用同一份窗集，标签才闭合。
func buildSynthesisPrompt(question string, windows []qaflow.EvidenceWindow, fx facts.Report) (string, []qaflow.EvidenceWindow) {
	var b strings.Builder
	fmt.Fprintf(&b, "问题：%s\n\n", question)
	if fx.K >= 2 {
		type group struct {
			fact facts.Fact
			ws   []qaflow.EvidenceWindow
		}
		var groups []group
		var rendered []qaflow.EvidenceWindow
		for _, f := range fx.Facts {
			g := group{fact: f}
			for _, s := range facts.SupportsOf(f, toFactWindows(windows)) {
				if w := findWindow(windows, s.SourceID, s.Span); w != nil {
					g.ws = append(g.ws, *w)
					rendered = append(rendered, *w)
				}
			}
			groups = append(groups, g)
		}
		if len(rendered) > 0 {
			b.WriteString("本问题包含以下事实，请逐条核对：\n")
			for _, g := range groups {
				state := "有支撑证据"
				if len(g.ws) == 0 {
					state = "（无支撑证据——必须明说）"
				}
				fmt.Fprintf(&b, "  事实 %s「%s」：%s\n", g.fact.ID, g.fact.Query, state)
			}
			b.WriteString("\n按事实分组的证据窗口：\n")
			n := 0
			for _, g := range groups {
				fmt.Fprintf(&b, "\n== 事实 %s「%s」 ==\n", g.fact.ID, g.fact.Query)
				if len(g.ws) == 0 {
					b.WriteString("（这条事实没有任何窗口支撑）\n")
					continue
				}
				for _, w := range g.ws {
					n++
					title := w.Title
					if title == "" {
						title = w.SourceID
					}
					fmt.Fprintf(&b, "[w%d] %s（%s）位置 %s 得分 %.2f\n原文：%s\n", n, title, w.SourceID, w.Span, w.Score, w.Text)
				}
			}
			b.WriteString("\n按系统提示的 JSON 契约作答。")
			return b.String(), rendered
		}
		// 全空兜底：分组一个窗都渲染不出来时退化平铺
	}
	b.WriteString("证据窗口：\n")
	for i, w := range windows {
		title := w.Title
		if title == "" {
			title = w.SourceID // 没身份的退化成 id（不该发生，但有兜底）
		}
		fmt.Fprintf(&b, "[w%d] %s（%s）位置 %s 得分 %.2f\n原文：%s\n", i+1, title, w.SourceID, w.Span, w.Score, w.Text)
	}
	b.WriteString("\n按系统提示的 JSON 契约作答。")
	return b.String(), windows
}

// findWindow 最终窗集里找 (sourceID, span) 对应的窗。
func findWindow(windows []qaflow.EvidenceWindow, sourceID, span string) *qaflow.EvidenceWindow {
	for i := range windows {
		if windows[i].SourceID == sourceID && windows[i].Span == span {
			return &windows[i]
		}
	}
	return nil
}

// toFactWindows 窗集转 facts.Window（现算分组的输入）。
func toFactWindows(ws []qaflow.EvidenceWindow) []facts.Window {
	out := make([]facts.Window, 0, len(ws))
	for _, w := range ws {
		out = append(out, facts.Window{SourceID: w.SourceID, Span: w.Span, Text: w.Text, Score: w.Score})
	}
	return out
}

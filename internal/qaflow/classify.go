package qaflow

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/llm"
)

// 窗口分级（GaRAGe 四类）。为什么要有：
//
//	ANSWER   直接给出了这个问题的答案
//	RELATED  只谈相关话题，没有答案
//	OUTDATED 给的是过时的答案
//	UNKNOWN  无法判断
//
// `file` 事件原本只带 rank/分数/预览：用户看到"检索到了这篇"，但**说不出为什么是它**。
// 分级把"检索日志"变成**可解释的**检索日志——分数说"排第几"，类别说"有没有用"。
//
// **这是可解释性诉求，不是准确率诉求**：分类不改变检索与排序，也不参与路由判据。
// 它只让答案旁边的证据带上一句"这条是答案 / 只相关 / 过时 / 判不了"，让用户能自己判断。
const (
	ClassAnswer   = "ANSWER"
	ClassRelated  = "RELATED"
	ClassOutdated = "OUTDATED"
	ClassUnknown  = "UNKNOWN"
)

// WindowClasses 是全部类别（顺序固定：展示与校验都按它）。
func WindowClasses() []string {
	return []string{ClassAnswer, ClassRelated, ClassOutdated, ClassUnknown}
}

// WindowClassIs 判类别名是否合法（**非法名不进事件流**：下游不该收到"也许吧"）。
func WindowClassIs(c string) bool {
	for _, x := range WindowClasses() {
		if c == x {
			return true
		}
	}
	return false
}

// WindowClassifier 把一批窗口分成四类。**可选能力**：nil = 不分级（事件里没有 class
// 字段，消费方自己按"未分级"显示——缺席是合法状态，不许用 UNKNOWN 冒充"没分"）。
type WindowClassifier struct {
	Client llm.Completer
	// MaxWindows 分级上限（0 = 默认 12）。分级是一次模型调用 + 与窗口数成正比的
	// token：**窗口太多时只分级靠前的那几条**（后面那些本来也不会被引用）。
	MaxWindows int
}

const classifySystem = `你是证据分级器。给每条证据标一个类别，只输出 JSON，不要解释。
类别只有四个：
- ANSWER：这条直接给出了问题的答案
- RELATED：只谈相关话题，但没有给出问题的答案
- OUTDATED：给出了答案，但是过时的（已被更新取代）
- UNKNOWN：无法判断
输出格式（四个键都必须有，没有内容的类别给空数组）：
{"ANSWER":[1,3],"RELATED":[2],"OUTDATED":[],"UNKNOWN":[]}`

// Classify 逐条分级。**一次调用**（不是每条一次）：分级是对同一批证据的整体判断，
// 分开问会让模型看不到邻居，也把成本乘以窗口数。
//
// 半截结果整体失败：解析不出、类别名非法、编号越界、编号漏标——都返回空。理由与
// 事件流的其他构造器一致：**半截信息比没信息更难解释**。
func (c *WindowClassifier) Classify(fc *context.Context, question string, ws []EvidenceWindow) (map[int]string, error) {
	if c == nil || c.Client == nil || len(ws) == 0 {
		return nil, nil // 缺席是合法状态
	}
	limit := c.MaxWindows
	if limit <= 0 {
		limit = 12
	}
	if len(ws) < limit {
		limit = len(ws)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "问题：%s\n\n", question)
	for i := 0; i < limit; i++ {
		fmt.Fprintf(&sb, "[%d] %s\n%s\n\n", i+1, ws[i].Title, trimRunes(ws[i].Text, 200))
	}
	resp, err := c.Client.Complete(gocontext.Background(), llm.Request{System: classifySystem, Prompt: sb.String(), MaxTokens: 300})
	if err != nil {
		return nil, fmt.Errorf("qaflow: classify: %w", err)
	}
	raw := llm.LastJSONObject(resp.Text)
	if raw == "" {
		raw = llm.LastJSONObject(truncatedAnswerHint(resp.Text))
	}
	if raw == "" {
		return nil, fmt.Errorf("qaflow: classify: 回包没有 JSON（raw=%q）", trimRunes(resp.Text, 160))
	}
	return parseWindowClasses(raw, limit)
}

// parseWindowClasses 解析 + 校验（任何不合都整体失败，不给"半张表"）。
func parseWindowClasses(raw string, n int) (map[int]string, error) {
	var byClass map[string][]int
	if err := json.Unmarshal([]byte(raw), &byClass); err != nil {
		return nil, fmt.Errorf("qaflow: classify: 回包不是约定的 JSON：%w; raw=%q", err, trimRunes(raw, 160))
	}
	out := make(map[int]string, n)
	for _, class := range WindowClasses() {
		for _, id := range byClass[class] {
			if id < 1 || id > n {
				return nil, fmt.Errorf("qaflow: classify: 编号 %d 越界（共 %d 条）", id, n)
			}
			if _, dup := out[id]; dup {
				return nil, fmt.Errorf("qaflow: classify: 编号 %d 被归了两类", id)
			}
			out[id] = class
		}
	}
	for id := 1; id <= n; id++ {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("qaflow: classify: 编号 %d 漏标（半截结果不许进事件流）", id)
		}
	}
	return out, nil
}

// truncatedAnswerHint 在推理链被当答案时给兜底一个标记（让 LastJSONObject 仍有机会）。
func truncatedAnswerHint(s string) string { return s }

func trimRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// WindowClassResult 是分级的结果与结局（**同一个挂点，三态同址**）。
//
// 为什么必须同址：结局原来写在 EscalationRecord 上，而分级发生在升级**之前**、
// 那条记录当时还不存在 → 结局恒空（这个坑踩了两次，与桥那次同一类）。**观测状态
// 必须住在生产者自己的挂点上**，不能寄生在别人稍后才写的记录里。
type WindowClassResult struct {
	// Classes 是 rank → 类别（空 = 没分级出结果）。
	Classes map[int]string `json:"classes,omitempty"`
	// Outcome：ok / failed:… / skipped:no-windows / skipped:no-rewrite。
	Outcome string `json:"outcome,omitempty"`
	// Ran 标记分级函数**被调用过**（Ran=false + 空 Classes = 压根没跑）。
	Ran bool `json:"ran"`
}

// KeyWindowClass 是窗口分级的挂点。
var KeyWindowClass = context.NewKey[WindowClassResult]("evidence.window.class")

// WindowClassOf 取分级结果（第二个返回值 = 挂点上有没有记录）。
func WindowClassOf(c *context.Context) (WindowClassResult, bool) {
	return context.Get(c, KeyWindowClass)
}

// SetWindowClass 写分级结局（**任何路径都要写**，含"跑了但失败"——
// 不写就分不出"没跑"与"跑了没成"）。
func SetWindowClass(c *context.Context, r WindowClassResult) error {
	return context.Set(c, KeyWindowClass, r)
}

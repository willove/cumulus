package harness

import "strings"

// StageLabel 是阶段名的**显示标签**（中文）。
//
// 为什么标签表放在 harness 而不是 UI：这是**事件元数据**——每个消费方都要它
// （老 cumulus 控制台有 STAGE_TEXT 映射、invoke-chat 要 label/detail），而在
// 界面里各写一份，迟早有两份不一致的说法。**一份表，多处消费**。
//
// 未登记的阶段名**原样返回**（不猜、不吞、不编造）：新加的阶段即使没登记，
// 消费方也能看到它真实的名字，而不是一句空白。
func StageLabel(name string) string {
	if l, ok := stageLabels[name]; ok {
		return l
	}
	return name
}

// stageLabels 是阶段名 → 中文标签。键与 qaflow 各 Stage 的 Name() 一一对应；
// 加新阶段时**必须**在这里登记（否则 UI 上会出现英文原名）。
var stageLabels = map[string]string{
	"intent-clarify":    "理解问题与检索意图",
	"reuse":             "复用上轮证据",
	"evidence-supply":   "检索证据窗口",
	"fact-coverage":     "核对事实覆盖",
	"context-evict":     "上下文预算与驱逐",
	"sufficiency-route": "判断证据够不够",
	"abstain-gate":      "弃权头裁决",
	"decision-gate":     "决策闸门",
	"escalate":          "升级重检",
	"synthesize":        "合成答案",
	"account":           "记账与信号",
	"reuse-record":      "记录可复用证据",
	"learn":             "学习本轮经验",
}

// StageOrder 是**流水线顺序**（消费方用它排序/算进度）。哑臂与可选件不在
// 标准序列里——它们不是"每次都走的步骤"，列进来会让进度条说谎。
var StageOrder = []string{
	"intent-clarify",
	"reuse",
	"evidence-supply",
	"fact-coverage",
	"context-evict",
	"sufficiency-route",
	"abstain-gate",
	"decision-gate",
	"escalate",
	"synthesize",
	"account",
	"reuse-record",
}

// IsKnownStage 判断阶段名是否登记过（消费方可用它决定要不要做中文渲染；
// 未知阶段仍照常显示原名）。
func IsKnownStage(name string) bool {
	_, ok := stageLabels[name]
	return ok
}

// StageProgress 按**已完成/总数**算百分比；不在标准序列里的阶段按已完成计，
// 总数按"标准步 + 1"给个保守分母——**宁可低估进度，不许高估**（进度条跳到
// 90% 然后卡住比慢一点更让人烦躁）。
func StageProgress(done, total int) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(done) / float64(total) * 100
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}

// oneLine 把详情压成一行（进度气泡宽度有限；换行会被当噪声）。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	const max = 80
	if len(rs) > max {
		return string(rs[:max]) + "…"
	}
	return s
}

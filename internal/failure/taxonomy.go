// Package failure 是失败模式六分类。评测 capture 时按此归因，
// 学习周期的诊断阶段只允许用这六类说话（流程文法 §三）。
//
// 六类都必须可达：定义了判据却没有任何一条规则返回它，等于把诊断
// 词汇表变装饰（SubstrateMismatch 曾经就是这样——BioHarness 的核心
// 贡献是"把底物错配单独命名"，我们定义了名字却没接判据）。判据的
// 观测输入见各分类的注释与 evalfcore.ClassifyInput。
package failure

import (
	"strings"
	"unicode"
)

// Category 是一类失败。判据与修复方向见 docs/flow-grammar.md §三。
type Category int

const (
	// SubstrateMismatch 底物错配：要的证据类型和用的检索类型不是一回事。
	SubstrateMismatch Category = iota
	// RecallMiss 召回不足：正确证据没进候选。
	RecallMiss
	// GroundingFail 接地失败：答案词在证据窗口里对不上。
	GroundingFail
	// RouteError 路由误判：该升级没升级，或不必要地升级了。
	RouteError
	// Rot 腐烂：长任务中直接放弃或提前给不确定答案。
	Rot
	// BudgetExceeded 超限：窗口或轮次打满。
	BudgetExceeded
)

var names = map[Category]string{
	SubstrateMismatch: "substrate-mismatch",
	RecallMiss:        "recall-miss",
	GroundingFail:     "grounding-fail",
	RouteError:        "route-error",
	Rot:               "rot",
	BudgetExceeded:    "budget-exceeded",
}

func (c Category) String() string {
	if s, ok := names[c]; ok {
		return s
	}
	return "unknown"
}

// All 返回全部类别，供评测面板枚举。
func All() []Category {
	return []Category{SubstrateMismatch, RecallMiss, GroundingFail, RouteError, Rot, BudgetExceeded}
}

// AsksNumeric 问题是否在要一个数值型答案（多少 / 几 / 百分比 / 期限 …）。
//
// 这是底物错配（BioHarness）在文本侧的代理判据：问题要的是数值测量，
// 系统却在叙述文本里翻——失败不在召回率，在供给的证据类型不对。
// 判据保守（宁漏勿滥）：拿不准就不算，避免把普通问句打成错配。
// 时敏问题（要日期/版本）不在此列——那是 GaRAGe 的 OUTDATED 轴，
// 归证据窗口分级管，不混进底物判据。
func AsksNumeric(q string) bool {
	for _, marker := range numericAsks {
		if strings.Contains(q, marker) {
			return true
		}
	}
	return false
}

// numericAsks 是数值诉求的标记词。只收"要一个数"的说法，不收时间问法。
var numericAsks = []string{
	"多少", "几", "多久", "多大", "多长", "多高", "多重", "几年", "几天",
	"几个月", "百分比", "百分之", "比例", "金额", "数量", "次数", "年限",
	"期限", "岁数", "价格", "费用", "工资", "利率", "税率", "浓度", "温度",
	"面积", "体积",
}

// HasNumeric 文本里是否出现可读的数值：阿拉伯数字，或中文数字**带计量
// 单位**（"二十年"算、"第X条"不算）。
//
// 中文数字必须带单位这条约束是判据可用的前提：法条语料里"第四十二条"
// 满篇都是，"第二条"这类序号一旦算数，HasNumeric 恒真、底物错配永不
// 触发——死标签被换成哑标签，等于没修。约束宁可漏判（后果只是不贴
// 标签），不可误判（后果是把真失败贴错类）。
func HasNumeric(texts ...string) bool {
	for _, t := range texts {
		if hasNumeric(t) {
			return true
		}
	}
	return false
}

func hasNumeric(s string) bool {
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsDigit(r) {
			return true
		}
		if !isCNNumeral(r) {
			continue
		}
		if i+1 < len(runes) && isMeasureUnit(runes[i+1]) {
			return true
		}
	}
	return false
}

func isCNNumeral(r rune) bool {
	return strings.ContainsRune("零一二三四五六七八九十百千万两", r)
}

func isMeasureUnit(r rune) bool {
	return strings.ContainsRune("年月日天时分秒元块米克吨度倍岁%％", r)
}

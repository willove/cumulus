// Package marks 解析"逐条判定"的模型回包。
//
// 形状只有一种：**每条一个编号 + 一个判定**（"1:Y"、"2:N"、"3:是"）。
// 它同时被两处用——清单式重排（选哪几条窗口）与分点覆盖判官（答没答到每个要点）——
// 所以解析口径只有这一处：两份实现漂移过一次（重排侧先写、判官侧再写一遍，
// 两边对"中文判定/分隔符形状"的容忍度一度不一致）。
//
// 真模型会给的形状（都见过）："1:Y\n2:N"、"1:Y 2:N"、"[1]:Y"、"1 - 是"。
package marks

import (
	"regexp"
	"strconv"
)

// Result 是一次逐条判定的解析结果。
type Result struct {
	Yes    []int // 判"是"的编号（按出现顺序）
	No     []int // 判"否"的编号
	Judged int   // 实际判了几条（**没判的要能看见**：否则"模型偷懒"会被误读成
	//             "模型认为都不相关"）
}

// Covers 报告编号 n 是否被判"是"。未判定视为否（宁可保守，不当成人判过）。
func (r Result) Covers(n int) bool {
	for _, id := range r.Yes {
		if id == n {
			return true
		}
	}
	return false
}

// markRe 是"编号 + 判定"的通用形状：编号与判定之间可以是空、冒号、连字符，
// 判定可以是 Y/N/是/否。用正则而不是"先按空格切再拼"——真模型给过
// "1 - Y"，按空格切会把它切成三个 token（真踩过）。
var markRe = regexp.MustCompile(`[\[\(【]?\s*(\d{1,3})\s*[\]\)】]?\s*[-–—:：]?\s*([YNynN]|是|否)`)

// Parse 解析逐条判定。
func Parse(text string) Result {
	var res Result
	for _, m := range markRe.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch m[2] {
		case "Y", "y", "是":
			res.Yes = append(res.Yes, n)
		case "N", "n", "否":
			res.No = append(res.No, n)
		}
		res.Judged++
	}
	return res
}

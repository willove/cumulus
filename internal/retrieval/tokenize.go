// Package retrieval 是证据供给层的检索后端。
//
// v0.1 接入 cumulus 验证过的最强零件：倒排索引 + BM25（RSJ 默认参数
// k1=1.2 / b=0.75，与 cumulus internal/index 同参数同公式），配同一个
// 确定性分词：CJK 连续段切二元组（相邻两字滑窗），字母数字连续段整词
// 小写化。查询与文档同走一个分词器，所以中文的匹配语义等价于子串匹配。
//
// 零 LLM、零模型、确定性：知识全部来自语料自身的词项统计。
package retrieval

import (
	"strings"
)

// Fields 把文本切成检索词项：CJK 连续段出二元组，字母数字连续段出整词。
// 与 cumulus 的 mcs.Fields 同语义（端口重写，不引依赖）。
//
// **两种脚本必须在边界处分开**。合并成一段的写法有一个致命后果：混排
// 文本（"iphone6照片流在哪"、"8月去关山牧场穿什么"）整串变成一个词，
// 它的 df 恒为 0——查询一个候选都取不到，症状是"拒答/召回不足"，根因
// 却是分词（DuReader 真实问句实测 10% 死在这一步：iphone6照片流、soc、
// ie、linux、gtx960 全是这一类）。语法边界就是语义边界，这里不许省。
func Fields(text string) []string {
	var out []string
	var cur []rune
	curCJK := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if curCJK {
			// CJK 段切二元组；单字段丢弃（与既有行为一致：单字噪声大）
			if len(cur) >= 2 {
				for i := 0; i+1 < len(cur); i++ {
					out = append(out, string(cur[i:i+2]))
				}
			}
		} else {
			out = append(out, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			if len(cur) > 0 && !curCJK {
				flush() // 字母数字段 → CJK 段
			}
			curCJK = true
			cur = append(cur, r)
		case isAlnum(r):
			if len(cur) > 0 && curCJK {
				flush() // CJK 段 → 字母数字段
			}
			curCJK = false
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func isCJK(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// UniqueTerms 去重且保序（查询侧与 RankTerms 共用）。
func UniqueTerms(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

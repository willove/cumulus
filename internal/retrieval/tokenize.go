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
func Fields(text string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			out = append(out, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range text {
		switch {
		case r >= 0x4e00 && r <= 0x9fff:
			cur = append(cur, r)
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	// CJK 段再切二元组；字母数字段保持整词。
	expanded := make([]string, 0, len(out))
	for _, f := range out {
		runes := []rune(f)
		if len(runes) >= 2 && isCJK(runes[0]) {
			for i := 0; i+1 < len(runes); i++ {
				expanded = append(expanded, string(runes[i:i+2]))
			}
		} else {
			expanded = append(expanded, f)
		}
	}
	return expanded
}

func isCJK(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }

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

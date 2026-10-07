package query

import (
	"strings"
	"unicode"
)

// SplitTerms 把文本切成索引可检索的词（与检索索引同口径：CJK 二元组
// + 拉丁/数字整词）。查询侧的多词关键词要过它才能进倒排——整词直接
// 搜必然零命中（索引里只有二元组，真跑踩过：扩展出"犬只扰民"四个字
// 一个都匹配不上）。
func SplitTerms(s string) []string {
	return fields(s)
}

// fields 与 retrieval.Fields 同口径（CJK 二元组 + 空白/标点切出的拉丁词）。
// query 包不引 retrieval：分析层是纯函数，语料事实经 CorpusTerms 接口进。
func fields(s string) []string {
	var out []string
	var lat []rune
	flush := func() {
		if len(lat) > 0 {
			out = append(out, string(lat))
			lat = nil
		}
	}
	var run []rune
	flushRun := func() {
		if len(run) == 0 {
			return
		}
		if len(run) == 1 {
			out = append(out, string(run))
		} else {
			for i := 0; i+1 < len(run); i++ {
				out = append(out, string(run[i:i+2]))
			}
		}
		run = nil
	}
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			run = append(run, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushRun()
			lat = append(lat, r)
		default:
			flushRun()
			flush()
		}
	}
	flushRun()
	flush()
	_ = strings.TrimSpace
	return out
}

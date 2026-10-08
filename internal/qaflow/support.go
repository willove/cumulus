package qaflow

import (
	"strings"

	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// AnswerSupport 是**带符号的词面支持**（SLC 的离线代理）：答案的内容词
// （去胶水/去停用词）有多大比例在证据窗口原文里出现过。
//
// 为什么换这个问法：检索侧的置信度（覆盖度/边际/死路率的加权）回答的是
// "窗口像不像问题"，而真正要下注的是"**答案有没有证据**"——见证必须落在
// 生成器实际给出的那段话上（prediction-aligned），不是落在它读过的材料上。
// 这条代理零成本、确定性、可离线算，所以能先量出它值不值得进路由。
//
// 返回 [0,1]；答案没有内容词（纯胶水/纯符号）时返回 0——"没有可验证的
// 断言"与"断言无支持"在这里同分，调用方若需区分请看 foundation=0 的计数。
func AnswerSupport(answer string, windows []EvidenceWindow) (support float64, foundation int) {
	terms := contentTerms(answer)
	if len(terms) == 0 || len(windows) == 0 {
		return 0, 0
	}
	var all strings.Builder
	for _, w := range windows {
		all.WriteString(w.Text)
		all.WriteByte('\n')
	}
	hay := all.String()
	found := 0
	for _, t := range terms {
		if strings.Contains(hay, t) {
			found++
		}
	}
	return float64(found) / float64(len(terms)), len(terms)
}

// contentTerms 取内容词（CJK 二元组/字母数字词，去胶水二元组与停用词）。
// 与 query.Analyze 的口径同源：胶水不是断言，不该进分母也不该进分子。
func contentTerms(text string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 16)
	for _, t := range retrieval.Fields(text) {
		if t == "" || seen[t] || glueWords[t] || query.IsGlue(t) {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// glueWords 是单字/整词级的胶水（query.IsGlue 判的是二元组沾字，这里补
// 检索分词后仍然留下的整词级疑问词/功能词）。
var glueWords = map[string]bool{
	"怎么": true, "什么": true, "如何": true, "为什么": true, "多少": true,
	"请问": true, "可以": true, "应该": true, "是不是": true, "能不能": true,
}

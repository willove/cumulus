package retrieval

import (
	"strings"

	"github.com/willove/cumulus/internal/prior"
)

// priorStructWidth 结构信号看窗宽度（与证据窗默认同档）。
const priorStructWidth = 240

// PriorDocs 把候选文档转成 prior 包的视图：TF 取倒排里查询词的词频，
// Struct 取命中窗口（第 X 条边界内）内该词的词频。words 是查询词集合
// （分析后的主关键词级或原始查询词）。
func (idx *Index) PriorDocs(words []string, docIDs []string) []prior.Doc {
	docs := make([]prior.Doc, 0, len(docIDs))
	for _, id := range docIDs {
		if _, ok := idx.byID[id]; !ok {
			continue
		}
		view := prior.Doc{ID: id, Title: idx.TitleOf(id), TF: map[string]int{}, Struct: map[string]int{}}
		for _, w := range words {
			view.TF[w] = idx.tfOf(id, w)
			view.Struct[w] = idx.termFreqInHitArticle(id, w)
		}
		docs = append(docs, view)
	}
	return docs
}

// tfOf 词在文档里的词频（倒排直查）。
func (idx *Index) tfOf(docID, term string) int {
	for _, e := range idx.Postings[term] {
		if e.DocID == docID {
			return e.TF
		}
	}
	return 0
}

// termFreqInHitArticle 词在文档命中窗口内出现的次数。窗口即证据窗
// （密度定心 + 条文吸附）——struct 信号与证据窗同域，信号说的事就是
// 合成面将看到的覆盖。
func (idx *Index) termFreqInHitArticle(docID, term string) int {
	d, ok := idx.byID[docID]
	if !ok || term == "" {
		return 0
	}
	coord := idx.Window(docID, []string{term}, priorStructWidth)
	if coord == "" {
		return 0
	}
	text, err := ResolveSpan(d.Body, coord)
	if err != nil {
		return 0
	}
	return strings.Count(text, term)
}

// PriorRank 对候选池做多信号重排（cumulus prior 的移植：lexical 不做
// 长度归一 + 标题 + 条文结构）。返回归一分（置顶为 1）。
func (idx *Index) PriorRank(words []string, docIDs []string, topK int) []prior.FileScore {
	df := map[string]int{}
	for _, w := range words {
		df[w] = len(idx.Postings[w])
	}
	return prior.Rank(idx.PriorDocs(words, docIDs), df, idx.N, topK)
}

// BoostMap 把 prior 结果转成 SearchWith 的 boost（docID → 分）。乘在
// BM25 分上：BM25 管词面命中，prior 管文档级置信（长文档深命中、标题
// 命中、条文命中），两者相乘即 LENS 的候选池 × 置信融合。
func (idx *Index) BoostMap(words []string, docIDs []string) map[string]float64 {
	scores := idx.PriorRank(words, docIDs, 0)
	out := make(map[string]float64, len(scores))
	for _, s := range scores {
		out[s.DocID] = s.Score
	}
	return out
}

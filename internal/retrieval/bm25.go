package retrieval

import ()

// BM25 参数：Robertson-Sparck Jones 默认值，五十年文献的共识。
// k1 控制词频饱和，b 控制文档长度归一。设计上不可调——每个搜索引擎
// 用的都是这组值（cumulus 的同款决定，照抄）。
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Document 是被检索的一条语料。Body 是全文（分词与窗口抽取都用它）。
type Document struct {
	ID   string
	Body string
}

// Posting 是倒排列表里的一条（文档，词频）。
type Posting struct {
	DocID string
	TF    int
}

// Index 是内存倒排索引。一次构建，查询期只读。
type Index struct {
	Postings map[string][]Posting
	DocLens  map[string]int
	N        int
	AvgLen   float64

	// Coord 是协调因子指数（0 = 关，默认）。打分时乘 (命中词数/查询词数)^Coord：
	// 只命中一个实体的文档不再压过"把问句里几个实体都覆盖了"的文档。
	//
	// 为什么需要（真跑）：多实体问句（DomainRAG multidoc："数学与应用数学
	// 专业**与**数据计算及应用专业在人才培养上的共同目标…"）里，纯 BM25 求和
	// 会把"某个实体词反复命中"的文档排在前面，金标被推到 10–20 名——
	// 池子里有（91.7% 在 top-20）但排不到前面。实测金标首次名次 ≤3 的只有
	// 8/48。协调因子治的正是这个。
	Coord float64

	byID map[string]Document
}

// Build 从语料建索引。确定性：同输入同索引。零 LLM，O(全文字长)。
func Build(docs []Document) *Index {
	idx := &Index{
		Postings: make(map[string][]Posting, 4096),
		DocLens:  make(map[string]int, len(docs)),
		byID:     make(map[string]Document, len(docs)),
	}
	var totalTokens int
	for _, d := range docs {
		if d.ID == "" {
			continue
		}
		idx.byID[d.ID] = d
		tokens := Fields(d.Body)
		idx.DocLens[d.ID] = len(tokens)
		totalTokens += len(tokens)
		tf := make(map[string]int, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for term, count := range tf {
			idx.Postings[term] = append(idx.Postings[term], Posting{DocID: d.ID, TF: count})
		}
		idx.N++
	}
	if idx.N > 0 {
		idx.AvgLen = float64(totalTokens) / float64(idx.N)
	}
	return idx
}

// Hit 是一条检索命中。SpanCoord 是证据窗口在原文里的 rune 坐标
// （"rune[起点:终点]"）；SpanText 是该坐标解析出的原文——合成面要的是
// 原文，坐标只是引用凭据。两样都在检索侧一次产出，别让下游再解一遍。

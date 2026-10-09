package retrieval

import "sync"

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
	// AvgRunes 是**平均字符数**（AvgLen 是 token 数，两者不是一回事）。
	//
	// 为什么单独存一个："这个窗口装不装得下一篇文档"是**字符**的问题，
	// 而 AvgLen 的单位是检索词元——中文 1700 字 ≈ 几百个二元组，用 AvgLen 判断
	// 会得出"文档很短"的错误结论（真跑踩过：长文档探针的锚定判据因此失灵）。
	AvgRunes float64

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

	// ── 分层（热区/冷区）状态；为 nil = 全内存（Build 出来的就是这种） ──
	//
	// 为什么要它：实测 1800 篇 / 1.49 MB 语料的索引占 26.5 MB，其中倒排 26.28 MB
	// （99.4%）。倒排随语料**线性增长**，所以不能全放内存。分层把倒排限制在热区，
	// 冷文档只留**词项指纹**（无假阴性）+ 需要时从磁盘取回原文精算。
	//
	// 关键性质：**分层只是内存策略**——预算 ≥ 语料规模时行为与 Build 逐字节一致，
	// 所以对小个人库零影响。详见 tier.go。
	tier *tiered
	// tierLock 保护分层状态；Once 让 Index 的零值也能安全用（惰性建锁）。
	tierLock     sync.Mutex
	tierLockOnce sync.Once
	// sketchWords 是每篇指纹的位数组长度（uint64 个数）；冷文档筛选时按位取。
	sketchWords int
}

// Build 从语料建索引。确定性：同输入同索引。零 LLM，O(全文字长)。
func Build(docs []Document) *Index {
	idx := &Index{
		Postings: make(map[string][]Posting, 4096),
		DocLens:  make(map[string]int, len(docs)),
		byID:     make(map[string]Document, len(docs)),
	}
	var totalTokens int
	var totalRunes int
	for _, d := range docs {
		if d.ID == "" {
			continue
		}
		tokens := Fields(d.Body)
		idx.indexDoc(d.ID, d.Body, tokens)
		totalTokens += len(tokens)
		idx.N++
		totalRunes += len([]rune(d.Body))
	}
	if idx.N > 0 {
		idx.AvgLen = float64(totalTokens) / float64(idx.N)
		idx.AvgRunes = float64(totalRunes) / float64(idx.N)
	}
	return idx
}

// Hit 是一条检索命中。SpanCoord 是证据窗口在原文里的 rune 坐标
// （"rune[起点:终点]"）；SpanText 是该坐标解析出的原文——合成面要的是
// 原文，坐标只是引用凭据。两样都在检索侧一次产出，别让下游再解一遍。

// indexDoc 把**一篇**文档的倒排放进索引（Build 与分层升权共用同一条路径——
// 两处各写一份，迟早会漂移）。
//
// tokens 为 nil 时**自己算**（分层升权时若忘了传，会得到"热区里一具没有倒排的空壳"
// ——真跑症状：升权成功、热区里有它、检索却查不到它，postings 中=0）。
func (idx *Index) indexDoc(id, body string, tokens []string) {
	if id == "" {
		return
	}
	if tokens == nil {
		tokens = Fields(body)
	}
	if idx.Postings == nil {
		idx.Postings = map[string][]Posting{}
	}
	if idx.DocLens == nil {
		idx.DocLens = map[string]int{}
	}
	if idx.byID == nil {
		idx.byID = map[string]Document{}
	}
	idx.byID[id] = Document{ID: id, Body: body}
	idx.DocLens[id] = len(tokens)
	tf := make(map[string]int, len(tokens))
	for _, t := range tokens {
		tf[t]++
	}
	for term, count := range tf {
		idx.Postings[term] = append(idx.Postings[term], Posting{DocID: id, TF: count})
	}
}

// BodyOf 取一篇文档的正文（**热区从内存，冷区从 loader**）。
//
// 窗口切片（WindowAnchored / WindowMultiSpans / ResolveSpan）都要按坐标回原文，
// 所以冷文档必须能在这里"现取"——否则窗口功能对冷文档等于不存在。
func (idx *Index) BodyOf(docID string) (string, bool) {
	if d, ok := idx.byID[docID]; ok {
		return d.Body, true
	}
	if idx.tier == nil || idx.tier.loader == nil {
		return "", false
	}
	return idx.tier.loader(docID)
}

package retrieval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

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
type Hit struct {
	DocID     string
	Score     float64
	SpanCoord string
	SpanText  string
	Title     string // 文档身份（首行标题）——窗口不带身份，模型不知道这段
	// "第二十二条"是哪部法律的（两部法律都有第二十二条，真跑踩过）
}

// Search 对查询做 BM25 排序，取前 k，并为每条命中抽证据窗口。
// 排序回到原查询（不由改写串决定），窗口宽度 width 个 rune。
func (idx *Index) Search(query string, k, width int) []Hit {
	return idx.SearchWith(query, k, width, nil)
}

// SearchWith 是带候选提升的检索：boost(docID) 返回该文档的加权系数
// （1 = 不动，>1 = 上浮，<1 = 下沉）。先取 k*poolFactor 个 BM25 候选，
// 再乘系数重排取前 k——信念因此能把 BM25 排在窗外的文档捞回来，
// 而不是只能在窗口内微调。
//
// boost 为 nil 时等价于 Search。确定性：加权后同分按文档 ID。
func (idx *Index) SearchWith(query string, k, width int, boost func(docID string) float64) []Hit {
	if boost == nil {
		boost = func(string) float64 { return 1 }
	}
	pool := k * poolFactor
	ids := idx.Rank(query, pool)
	if len(ids) == 0 {
		return nil
	}
	terms := UniqueTerms(Fields(query))
	scores := idx.scoreTerms(terms)

	type weighted struct {
		id    string
		total float64
	}
	ws := make([]weighted, 0, len(ids))
	for _, id := range ids {
		ws = append(ws, weighted{id: id, total: scores[id] * boost(id)})
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].total != ws[j].total {
			return ws[i].total > ws[j].total
		}
		return ws[i].id < ws[j].id
	})
	if len(ws) > k {
		ws = ws[:k]
	}
	hits := make([]Hit, 0, len(ws))
	for _, w := range ws {
		coord := idx.Window(w.id, terms, width)
		text := ""
		if coord != "" {
			if d, ok := idx.byID[w.id]; ok {
				text, _ = ResolveSpan(d.Body, coord)
			}
		}
		hits = append(hits, Hit{DocID: w.id, Score: scores[w.id], SpanCoord: coord, SpanText: text, Title: idx.TitleOf(w.id)})
	}
	return hits
}

// poolFactor：候选池相对返回数的倍数。信念重排要在池子里做，
// 池子是返回数的三倍——再大就成了全库重排，失去倒排的意义。
const poolFactor = 3

// Rank 返回前 k 个文档 ID（BM25 降序，同分按 ID 字典序——确定性）。
func (idx *Index) Rank(query string, k int) []string {
	if idx == nil || idx.N == 0 || k <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	terms := UniqueTerms(Fields(query))
	return idx.RankTerms(terms, k)
}

// RankTerms 对已分词的词项列表排序。存在的原因：改写/收窄后的词项
// 不能再拼回字符串重分词（会得到不同的二元组）。
func (idx *Index) RankTerms(terms []string, k int) []string {
	return idx.RankTermsCoord(terms, k)
}

// scoreTerms 只访问含查询词项的文档——这就是把 O(N) 全扫描换成
// O(命中) 的那一步。
func (idx *Index) scoreTerms(unique []string) map[string]float64 {
	scores, hits := idx.scoreTermsCoord(unique)
	_ = hits
	return scores
}

// scoreTermsCoord 是 BM25 + 命中词数（协调因子的原料）。确定性：同输入同输出。
func (idx *Index) scoreTermsCoord(unique []string) (map[string]float64, map[string]int) {
	scores := make(map[string]float64, 256)
	hits := make(map[string]int, 256)
	for _, term := range unique {
		postings := idx.Postings[term]
		if len(postings) == 0 {
			continue
		}
		df := float64(len(postings))
		idf := math.Log(1 + (float64(idx.N)-df+0.5)/(df+0.5))
		for _, p := range postings {
			dl := float64(idx.DocLens[p.DocID])
			tf := float64(p.TF)
			denom := tf + bm25K1*(1-bm25B+bm25B*dl/idx.AvgLen)
			scores[p.DocID] += idf * tf * (bm25K1 + 1) / denom
			hits[p.DocID]++
		}
	}
	return scores, hits
}

// coordFactor 返回该文档的协调乘子（1 = 开着但没惩罚）。指数 0 = 关。
//
// 分母只数**可达词**（语料里真的有的查询词）：胶水二元组与语料外词谁也匹配
// 不上，把它们算进分母等于凭空惩罚每一篇文档——真跑踩过：可达词分母下
// multidoc 27.1%→25.0%、faithful 81.6%→77.6%（协调因子反而有害）。这与
// 词面覆盖度的"分母只数可达词"是同一条纪律。
func (idx *Index) coordFactor(matched, reachable int) float64 {
	if idx.Coord <= 0 || reachable <= 0 {
		return 1
	}
	return math.Pow(float64(matched)/float64(reachable), idx.Coord)
}

// reachableTerms 是查询词里语料真有的那些（协调因子的分母）。
func (idx *Index) reachableTerms(unique []string) int {
	n := 0
	for _, t := range unique {
		if len(idx.Postings[t]) > 0 {
			n++
		}
	}
	return n
}

// RankCoord 按协调因子排序（协调因子关掉时与 Rank 同序）。
func (idx *Index) RankCoord(query string, k int) []string {
	terms := UniqueTerms(Fields(query))
	return idx.RankTermsCoord(terms, k)
}

// RankTermsCoord 是 RankTerms 的协调因子版：给定的词项表不能拼回字符串重分词，
// 所以协调因子也必须在这个入口可用。
func (idx *Index) RankTermsCoord(terms []string, k int) []string {
	if idx == nil || idx.N == 0 || k <= 0 {
		return nil
	}
	unique := UniqueTerms(terms)
	if len(unique) == 0 {
		return nil
	}
	scores, hits := idx.scoreTermsCoord(unique)
	reach := idx.reachableTerms(unique)
	type hit struct {
		id    string
		score float64
	}
	list := make([]hit, 0, len(scores))
	for id, sc := range scores {
		if sc <= 0 {
			continue
		}
		list = append(list, hit{id: id, score: sc * idx.coordFactor(hits[id], reach)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].id < list[j].id
	})
	if len(list) > k {
		list = list[:k]
	}
	out := make([]string, len(list))
	for i, h := range list {
		out[i] = h.id
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResolveSpan 把 "rune[起:止]" 坐标还原成原文片段。解析失败返回 error——
// 引用不可解析是硬错误，不许静默给空（qaflow 的 Verify 靠它）。
func ResolveSpan(body, coord string) (string, error) {
	var start, end int
	if _, err := fmt.Sscanf(coord, "rune[%d:%d]", &start, &end); err != nil {
		return "", fmt.Errorf("retrieval: bad span coord %q: %w", coord, err)
	}
	runes := []rune(body)
	if start < 0 || end > len(runes) || start >= end {
		return "", fmt.Errorf("retrieval: span %q out of range (body %d runes)", coord, len(runes))
	}
	return string(runes[start:end]), nil
}

// Doc 返回索引里的原文（窗口还原与事后审计用）。
func (idx *Index) Doc(docID string) (Document, bool) {
	d, ok := idx.byID[docID]
	return d, ok
}

// HasTerm 报告某词是否在语料里出现过（至少一篇文档含它）。
// 深循环的词面覆盖用它剔除语料外词——没有窗口能覆盖不存在的词，
// 把它算进分母等于把目标设成永不可达。
func (idx *Index) HasTerm(term string) bool {
	if idx == nil {
		return false
	}
	return len(idx.Postings[term]) > 0
}

// TitleOf 取文档身份：首行非空文本，截到 60 字（法律名/文档题）。这是
// 给人也給模型看的名字——内容哈希对两者都不可读。
func (idx *Index) TitleOf(docID string) string {
	d, ok := idx.byID[docID]
	if !ok {
		return ""
	}
	// 跳过前导空行（laws-full 有些文件首行是空行——不跳就取到空标题）
	line := strings.TrimLeft(d.Body, "\n\r\t ")
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len([]rune(line)) > 60 {
		line = string([]rune(line)[:60])
	}
	return line
}

// runeByteAt 字符偏移 → 字节偏移。
func runeByteAt(s string, runeOff int) int {
	n := 0
	for i := range s {
		if n == runeOff {
			return i
		}
		n++
	}
	return len(s)
}

// SearchWeighted 按查询侧给出的词权检索（query.Analysis 的产物：主关键
// 词级）。权乘在 idf 之上：着重词（2.0）与降权词（<1）区分开——这是
// cumulus 的 IDF 加权关键词级（闯红灯 vs 处罚那组取舍）在本项目的落点。
// weights 为空时返回空（调用方应退化到 SearchWith）。
func (idx *Index) SearchWeighted(weights map[string]float64, k, width int, boost func(docID string) float64) []Hit {
	if len(weights) == 0 {
		return nil
	}
	scores := idx.scoreWeighted(weights)
	if boost != nil {
		for id, s := range scores {
			scores[id] = s * boost(id)
		}
	}
	ranked := rankByScore(scores, idx.DocLens)
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	terms := make([]string, 0, len(weights))
	for term := range weights {
		terms = append(terms, term)
	}
	return idx.hitsFrom(ranked, terms, width, scores)
}

// scoreWeighted 按词权算分：idf × 权 × tf 分量（与 scoreTerms 同式，
// 只多一个权因子）。
func (idx *Index) scoreWeighted(weights map[string]float64) map[string]float64 {
	scores := make(map[string]float64, 256)
	for term, w := range weights {
		postings := idx.Postings[term]
		if len(postings) == 0 {
			continue
		}
		df := float64(len(postings))
		idf := math.Log(1 + (float64(idx.N)-df+0.5)/(df+0.5))
		for _, p := range postings {
			dl := float64(idx.DocLens[p.DocID])
			tf := float64(p.TF)
			denom := tf + bm25K1*(1-bm25B+bm25B*dl/idx.AvgLen)
			scores[p.DocID] += w * idf * tf * (bm25K1 + 1) / denom
		}
	}
	return scores
}

// hitsFrom 与 SearchWith 的产物体一致（排序、窗口、坐标、Title）。
func (idx *Index) hitsFrom(ranked []string, terms []string, width int, scores map[string]float64) []Hit {
	hits := make([]Hit, 0, len(ranked))
	for _, id := range ranked {
		coord := idx.Window(id, terms, width)
		text := ""
		if coord != "" {
			if d, ok := idx.byID[id]; ok {
				text, _ = ResolveSpan(d.Body, coord)
			}
		}
		hits = append(hits, Hit{DocID: id, Score: scores[id], SpanCoord: coord, SpanText: text, Title: idx.TitleOf(id)})
	}
	return hits
}

func rankByScore(scores map[string]float64, docLens map[string]int) []string {
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return docLens[ids[i]] < docLens[ids[j]] // 同分短文档在前：确定性
	})
	return ids
}

// DFOf 词的文档频率（query.Analysis 的语料事实面）。索引自带 HasTerm+
// DFOf 即满足 query.CorpusTerms——分析层不需要认索引。
func (idx *Index) DFOf(term string) int { return len(idx.Postings[term]) }

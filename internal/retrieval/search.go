package retrieval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// search.go —— BM25 检索与打分（Search / Rank / SearchWeighted / SearchFactWeighted）。
//
// （拆文件的理由：bm25.go 原本把"倒排怎么建"与"查询怎么打分"放一起。建索引是数据
// 结构（Build），打分是排序策略（可替换、可实验）——混着时换打分策略会动到索引。）

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
	terms := UniqueTerms(Fields(query))
	ids := idx.Rank(query, pool)
	if len(ids) == 0 && !idx.hasCold() {
		return nil
	}
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
		// **Hit.Score 保持"未乘 boost"**（与分层之前逐字节一致）：排序用乘过的分
		// （ws.total），但对外暴露的分是原始 BM25。分层那版图省事改成了乘过的分，
		// 结果长文档探针 10/10 → 9/10（下游的驱逐/重排按分做决策，一改就变）。
		hits = append(hits, idx.hitsFor(w.id, terms, width, scores[w.id])...)
	}
	// 冷区：指纹筛选 → 取回正文精算 → 与热区合并（分层只是内存策略，召回不变）
	return idx.mergeCold(hits, terms, boost, k, width)
}

// windowHit 给一篇文档切窗口并取回文本（热区走内存、冷区走 loader）。
func (idx *Index) windowHit(docID string, terms []string, width int, score float64) Hit {
	hs := idx.hitsFor(docID, terms, width, score)
	if len(hs) == 0 {
		return Hit{DocID: docID, Score: score, Title: idx.TitleOf(docID)}
	}
	return hs[0] // 多出来的窗口由调用方通过 hitsFor 拿（快路径见 SearchWith）
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
		// df 取**全量**（分层索引的冷文档没有倒排，用 len(postings) 会把
		// 热区的 idf 算大——于是冷文档的分与热文档不可比，实测把热区的正确答案
		// 挤出了 top-9：100 次查询里 37 次结果集不一致）。
		df := float64(idx.df(term))
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
	hits := idx.hitsFrom(ranked, terms, width, scores)
	// 冷区合并：**桥的加权重取也必须够得到冷文档**——桥扩出的词多半是"库里少见的
	// 词"，而少见词往往正落在冷文档上（真跑踩过：桥明明扩对了，加权检索却 0 命中）。
	return idx.mergeCold(hits, terms, boost, k, width)
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
		hits = append(hits, idx.hitsFor(id, terms, width, scores[id])...)
	}
	return hits
}

// hitsFor 给一篇文档生成命中：**长文档给多个互补窗口**（见 windowSpans）。
//
// 为什么一篇文档可以出多条命中：证据集按 `docID#span` 去重，两条不同 span 的命中
// 都会保留；而长文档里"答案在 84% 处"正是靠第二条窗口才被覆盖到的（真跑实测）。
// 短文档**只出一条**（逐字节等价于旧行为）。
func (idx *Index) hitsFor(id string, terms []string, width int, score float64) []Hit {
	coords := idx.windowSpans(id, terms, width, multiWindowLimit)
	if len(coords) == 0 {
		return nil
	}
	body, _ := idx.BodyOf(id)
	out := make([]Hit, 0, len(coords))
	for _, coord := range coords {
		text := ""
		if body != "" {
			text, _ = ResolveSpan(body, coord)
		}
		out = append(out, Hit{DocID: id, Score: score, SpanCoord: coord, SpanText: text, Title: idx.TitleOf(id)})
	}
	return out
}

// multiWindowLimit 是长文档里**一篇最多给几个窗口**。
//
// 为什么是 3：实测《劳动合同法》那条答案在 84% 处，第 2 个窗口就够；而每个窗口都是
// 要进合成面的正文，窗口数×宽度 = token 成本，不能放开。
const multiWindowLimit = 3

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

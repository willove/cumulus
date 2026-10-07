package retrieval

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
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
	if idx == nil || idx.N == 0 || k <= 0 {
		return nil
	}
	unique := UniqueTerms(terms)
	if len(unique) == 0 {
		return nil
	}
	scores := idx.scoreTerms(unique)
	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(scores))
	for id, sc := range scores {
		if sc > 0 {
			hits = append(hits, hit{id, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	if len(hits) == 0 {
		return nil // 无命中返回 nil，不是空切片——调用方用 nil 判“没有”
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.id
	}
	return out
}

// scoreTerms 只访问含查询词项的文档——这就是把 O(N) 全扫描换成
// O(命中) 的那一步。
func (idx *Index) scoreTerms(unique []string) map[string]float64 {
	scores := make(map[string]float64, 256)
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
		}
	}
	return scores
}

// Window 在文档里定位证据窗口：找查询词项首次命中的 rune 偏移，
// 向两侧各扩 width/2 个 rune。返回 "rune[起:止]" 坐标；找不到返回 ""。
// 坐标而不是文本：引用可核的前提是坐标可回溯（ResolveSpan）。
func (idx *Index) Window(docID string, terms []string, width int) string {
	d, ok := idx.byID[docID]
	if !ok || width <= 0 {
		return ""
	}
	runes := []rune(d.Body)
	// 收集全部查询词的命中位置（字符坐标）
	var positions []windowPos
	for _, term := range terms {
		if term == "" {
			continue
		}
		from := 0
		body := d.Body
		for {
			at := strings.Index(body[from:], term)
			if at < 0 {
				break
			}
			byteOff := from + at
			positions = append(positions, windowPos{at: len([]rune(body[:byteOff])), termRunes: len([]rune(term))})
			from = byteOff + len(term)
		}
	}
	if len(positions) == 0 {
		return ""
	}
	// 窗口中心 = 命中密度最高处：滑动 width 宽的位置，取覆盖词数最多的
	// 起点。**不是最早命中处**——法律名里就带"交通"二字，取最早命中的
	// 话窗口永远停在文档开头（真跑教训：问"交通信号灯"返回法律序言）。
	half := width / 2
	bestStart, bestCover := 0, -1
	for _, p := range positions {
		start := p.at - half
		if start < 0 {
			start = 0
		}
		end := start + width
		cover := 0
		for _, q := range positions {
			if q.at >= start && q.at < end {
				cover++
			}
		}
		if cover > bestCover {
			bestCover, bestStart = cover, start
		}
	}
	// 命中簇：密度窗 [bestStart, bestStart+width) 内的全部命中——不是单个
	// 位置（单位置的簇会把隔壁条目的命中漏掉，真跑踩过：问"红灯表示
	// 什么"命中簇停在第二十五条，答案在第二十六条）
	from, to := -1, -1
	for _, p := range positions {
		if p.at >= bestStart && p.at < bestStart+width {
			if from < 0 || p.at < from {
				from = p.at
			}
			if end := p.at + p.termRunes; end > to {
				to = end
			}
		}
	}
	if from < 0 { // 理论上不发生（bestStart 由某个命中推出）
		from, to = bestStart, bestStart+width
	}
	// 条文吸附围着**命中簇**扩（不是围着窗口扩——围着窗口扩会把命中
	// 所在的那一条整条吃掉，真跑踩过）：起点回到命中前的最后一个"第X条"
	// 标记，终点推到命中后的第一个标记。窗内至少一条完整条目，且命中必
	// 在窗内。无标记的文档（非法条）保持密度心窗口。
	start, end := snapToArticles(runes, from, to, bestStart, bestStart+width)
	if end > len(runes) {
		end = len(runes)
	}
	return fmt.Sprintf("rune[%d:%d]", start, end)
}

// windowPos 是一个词的一个命中位置。
type windowPos struct {
	at        int
	termRunes int
}

// articleMark 匹配"第X条"（中文数字）。吸附只认这个形态——法律的条目
// 边界在中文语料里足够规整，司法解释/宪法同款。
var articleMark = regexp.MustCompile(`第[一二三四五六七八九十百零零]+条`)

// snapToArticles 把命中区间 [lo,hi) 扩展到条文边界：起点回到 lo 前最后
// 一个"第X条"标记，终点推到 hi 后第一个标记。命中必在窗内。没有标记
// 可用时退回原窗口 [fallbackStart, fallbackEnd)。
func snapToArticles(runes []rune, lo, hi, fallbackStart, fallbackEnd int) (int, int) {
	body := string(runes)
	runeAt := func(byteOff int) int { return utf8.RuneCountInString(body[:byteOff]) }
	markBefore := func(runeOff int) (int, bool) {
		locs := articleMark.FindAllStringIndex(body[:runeByteAt(body, runeOff)], -1)
		if len(locs) == 0 {
			return 0, false
		}
		return runeAt(locs[len(locs)-1][0]), true
	}
	markAtOrAfter := func(runeOff int) (int, bool) {
		tail := body[runeByteAt(body, runeOff):]
		loc := articleMark.FindStringIndex(tail)
		if loc == nil {
			return 0, false
		}
		return runeAt(runeByteAt(body, runeOff) + loc[0]), true
	}
	start, ok1 := markBefore(lo)
	if !ok1 {
		start = fallbackStart
	}
	end, ok2 := markAtOrAfter(hi)
	if !ok2 {
		end = fallbackEnd
	}
	if end <= lo { // 极端：标记极近，保命要紧
		return fallbackStart, fallbackEnd
	}
	if start > lo {
		start = lo
	}
	return start, end
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
	line := d.Body
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

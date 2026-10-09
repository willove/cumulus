package retrieval

import (
	"fmt"
	"sort"
	"strings"
)

// anchor.go —— 窗口的**锚定**：给定查询词与可选锚短语，挑出最能覆盖问题的那一段。
//
// （拆文件的理由：window.go 原本把"怎么切窗口"与"怎么挑窗口"放在一起——前者是纯
// 坐标算法（可单测），后者是排序启发式（依赖检索命中）。两者混着时，改启发式会
// 让人怀疑切窗是不是也变了。）

func (idx *Index) WindowAnchored(docID string, terms []string, width int, anchor string) string {
	if anchor == "" {
		return idx.Window(docID, terms, width)
	}
	body, ok := idx.BodyOf(docID)
	if !ok || width <= 0 {
		return ""
	}
	runes := []rune(body)
	n := len(runes)
	if n == 0 {
		return ""
	}
	ar := []rune(anchor)
	if len(ar) < 3 {
		return idx.Window(docID, terms, width)
	}
	content := contentCores(anchor)
	if len(content) == 0 {
		return idx.Window(docID, terms, width)
	}

	// ---- 一次预计算：候选串（全部 3 字核心 + 全部内容词）的所有出现
	// 位置（rune 偏移，升序）。同串去重。
	seen := map[string][]int{}
	pos := func(s string) []int {
		if at, ok := seen[s]; ok {
			return at
		}
		sr := []rune(s)
		var at []int
		for i := 0; i+len(sr) <= n; i++ {
			match := true
			for j := range sr {
				if runes[i+j] != sr[j] {
					match = false
					break
				}
			}
			if match {
				at = append(at, i)
			}
		}
		seen[s] = at
		return at
	}
	has := func(s string, lo, hi int) bool { // s 在 [lo,hi] 出现过
		for _, p := range pos(s) {
			if p >= lo && p <= hi {
				return true
			}
		}
		return false
	}
	hitsIn := func(lo, hi int) int { // 内容词在 [lo,hi] 的命中数
		hits := 0
		for _, k := range content {
			if has(k, lo, hi) {
				hits++
			}
		}
		return hits
	}
	// 覆盖判定：内容词过半 + 任意 3 字核心在窗内（与 facts.Evaluate 同口径）
	covered := func(lo, hi int) bool {
		if float64(hitsIn(lo, hi))/float64(len(content)) < 0.5 {
			return false
		}
		for i := 0; i+2 < len(ar); i++ {
			if has(string(ar[i:i+3]), lo, hi) {
				return true
			}
		}
		return false
	}
	snap := func(at int) (lo, hi int, marked bool) {
		// 兜底窗必须钳在 [0,n]：at-width/2 在文档头部是负数， snapToArticles
		// 找不到条文标记时原样返回它 → 坐标 rune[-104:168] → ResolveSpan
		// 取出空文本，锚点静默失效（真跑教训：专利法的锚一直落在负坐标
		// 上，f1 的窗全是空壳）
		fbLo, fbHi := at-width/2, at+width/2
		if fbLo < 0 {
			fbLo = 0
		}
		if fbHi > n {
			fbHi = n
		}
		start, end, marked := snapToArticlesInfo(runes, at, at+3, fbLo, fbHi)
		if start < 0 {
			start = 0
		}
		if end > n {
			end = n
		}
		return start, end, marked
	}

	// 一级锚：3 字核心，稀有度升序 × 每次出现。覆盖判定选窗。
	type occ struct {
		at    int
		count int
	}
	var coreOcc []occ
	for i := 0; i+2 < len(ar); i++ {
		core := string(ar[i : i+3])
		at := pos(core)
		for _, p := range at {
			coreOcc = append(coreOcc, occ{at: p, count: len(at)})
		}
	}
	sort.SliceStable(coreOcc, func(i, j int) bool { return coreOcc[i].count < coreOcc[j].count })
	var fallback string
	for _, o := range coreOcc {
		lo, hi, _ := snap(o.at)
		coord := fmt.Sprintf("rune[%d:%d]", lo, hi)
		if fallback == "" {
			fallback = coord
		}
		if covered(lo, hi) {
			return coord
		}
	}
	if fallback != "" {
		return fallback
	}

	// 二级锚：内容词齐聚处（耐改写）。每个内容词位置的半径 12 内，数
	// 有多少不同内容词命中——答案就在齐聚处。
	radius := 12
	var allPos []int
	for _, k := range content {
		allPos = append(allPos, pos(k)...)
	}
	if len(allPos) == 0 {
		return idx.Window(docID, terms, width) // 锚词一个都不在：密度定心
	}
	// 优先**紧跟条文标记**的位置（正文条目 = "第X条"后几个字；目录里同
	// 名章节名的簇一样大，但最近的标记是上一条目录项、距离远——真跑教
	// 训：专利法目录里就有"第四十二条 发明专利权的期限为二十年"整句，
	// 簇大小与正文并列，不区分就锚进目录），其次簇最大，再其次最靠前。
	marks := articleMark.FindAllStringIndex(body, -1)
	markRune := make([]int, 0, len(marks))
	for _, m := range marks {
		markRune = append(markRune, len([]rune(body[:m[0]])))
	}
	markDist := func(p int) int { // p 到最近的前置条文标记的距离（无 = -1）
		closest := -1
		for _, m := range markRune {
			if m <= p && m > closest {
				closest = m
			}
		}
		if closest < 0 {
			return -1
		}
		return p - closest
	}
	bestAt, bestHits, bestTight := -1, -1, false
	for _, p := range allPos {
		dist := markDist(p)
		tight := dist >= 0 && dist <= tightMarkRunes
		h := hitsIn(p-radius, p+radius)
		if betterAnchor(h, tight, bestHits, bestTight) {
			bestAt, bestHits, bestTight = p, h, tight
		}
	}
	if bestAt < 0 {
		return idx.Window(docID, terms, width)
	}
	lo, hi, _ := snap(bestAt)
	return fmt.Sprintf("rune[%d:%d]", lo, hi)
}

// tightMarkRunes 位置离最近条文标记多远还算"正文条目内"。目录项的上
// 一个标记是上一条目录项（隔着一整行），正文条目紧跟自己的标记（几
// 个全角空格）。40 = 正文条目内的典型距离的上界。
const tightMarkRunes = 40

// betterAnchor 簇位置择优：有条文标记 > 簇大 > 位置靠前。
func betterAnchor(hits int, marked bool, bestHits int, bestMarked bool) bool {
	if marked != bestMarked {
		return marked // 有标记的赢（目录不是证据所在）
	}
	return hits > bestHits
}

// contentCores 事实的内容词（二元组，去胶水——与 query.IsGlue 同口径的
// 小子集，避免 retrieval 引 query 造成环）。
func contentCores(q string) []string {
	const glue = "的吗呢吧啊什怎多极少几谁何与或及于在被把就都还很"
	var out []string
	var lat []rune
	var han []rune
	emit := func() {
		if len(han) == 1 {
			out = append(out, string(han))
		} else if len(han) > 1 {
			for i := 0; i+1 < len(han); i++ {
				out = append(out, string(han[i:i+2]))
			}
		}
		han = nil
	}
	flushLat := func() {
		if len(lat) > 0 {
			out = append(out, string(lat))
			lat = nil
		}
	}
	for _, r := range q {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			// 类切换才冲刷：Han 字符前只冲拉丁，**不许冲 Han**（每次
			// Han 前都 emit 会把连续汉字切成单字，"专利期限"退化成
			// 专/利/期/限——单字随便命中（"社会公德"的公就能凑覆
			// 盖率），真跑教训：锚点被假覆盖钉在第五条）
			flushLat()
			han = append(han, r)
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			emit()
			lat = append(lat, r)
		default:
			emit()
			flushLat()
		}
	}
	emit()
	flushLat()
	kept := out[:0]
	for _, w := range out {
		if w == "" {
			continue
		}
		glued := false
		for _, ch := range w {
			if strings.ContainsRune(glue, ch) {
				glued = true
				break
			}
		}
		if !glued {
			kept = append(kept, w)
		}
	}
	return kept
}

// SearchFactWeighted 与 SearchWeighted 同排序，但每个命中文档的窗口用
// WindowAnchored（吸附到事实自己的核心所在条文）。fan-out 用：每条事
// 实的窗收在答案所在条文，不被密度/稀有度带到隔壁条文。
func (idx *Index) SearchFactWeighted(weights map[string]float64, k, width int, anchorPhrase string) []Hit {
	if len(weights) == 0 {
		return nil
	}
	scores := idx.scoreWeighted(weights)
	ranked := rankByScore(scores, idx.DocLens)
	if len(ranked) > k {
		ranked = ranked[:k]
	}
	terms := make([]string, 0, len(weights))
	for term := range weights {
		terms = append(terms, term)
	}
	hits := make([]Hit, 0, len(ranked))
	for _, id := range ranked {
		coord := idx.WindowAnchored(id, terms, width, anchorPhrase)
		text := ""
		if coord != "" {
			if body, ok := idx.BodyOf(id); ok {
				text, _ = ResolveSpan(body, coord)
			}
		}
		hits = append(hits, Hit{DocID: id, Score: scores[id], SpanCoord: coord, SpanText: text, Title: idx.TitleOf(id)})
	}
	return idx.mergeCold(hits, terms, nil, k, width)
}

// WindowMultiSpans 在**同一篇文档**里取多个**互不重叠**的段位窗口，按词面密度贪心。
//
// 为什么需要（真跑，长文档探针）：BM25 给一篇文档**只有一个分数**，所以任何"每篇
// 一个窗口"的检索在长文里都会漏——答案所在段落的词密度可能与开头段落**并列最高**
// （实测《专利审查指南》：开头"宣告/无效/专利"与末尾答案段各 3 个命中），而窗口
// 只取密度最高的那一处，于是**答案在 98% 处却完全没被覆盖**（window hit 9/10 的
// 那 1 题）。
//
// 做法：按段位扫描 → 每轮取当前命中最多的段 → 跳过与已取窗口重叠的部分 → 直到
// 取够 `maxSpans`。**不是**把整篇切成 n 块全给（那等于搬运），而是"**只给最像
// 证据的那几处**"。
//
// 为什么不会伤害短文档：短文档的段位数不足，贪心自然退化成原来那一个窗口。
func (idx *Index) WindowMultiSpans(docID string, terms []string, width, maxSpans int) []Hit {
	if maxSpans <= 0 {
		maxSpans = 2
	}
	raw, ok := idx.BodyOf(docID)
	if !ok {
		return nil
	}
	d := Document{ID: docID, Body: raw}
	body := []rune(d.Body)
	if len(body) == 0 {
		return nil
	}
	type span struct{ lo, hi, hits int }
	// 段位按窗口宽切（比窗口略宽，让相邻证据不被切碎）
	step := width
	if step <= 0 {
		step = 400
	}
	spans := make([]span, 0, len(body)/step+1)
	for lo := 0; lo < len(body); lo += step {
		hi := lo + step
		if hi > len(body) {
			hi = len(body)
		}
		seg := string(body[lo:hi])
		n := 0
		for _, t := range terms {
			if t == "" {
				continue
			}
			if idx.coordCount(seg, t) > 0 {
				n++
			}
		}
		spans = append(spans, span{lo, hi, n})
	}
	var out []Hit
	taken := make([][2]int, 0, maxSpans)
	for len(out) < maxSpans {
		best := -1
		for i, sp := range spans {
			if sp.hits == 0 {
				continue
			}
			if overlaps(taken, sp.lo, sp.hi) {
				continue
			}
			if best == -1 || sp.hits > spans[best].hits {
				best = i
			}
		}
		if best == -1 {
			break
		}
		sp := spans[best]
		// 该段向两侧扩到窗口宽（段本身可能比窗口窄）
		lo, hi := sp.lo, sp.hi
		if hi-lo < step {
			lo = maxInt(0, hi-step)
			hi = minInt(len(body), lo+step)
		}
		// **扩展后再判一次重叠**：扩展可能把窗口推进已取区域（真跑：
		// rune[0:400] 之后的第二窗扩成 rune[68:468]，与第一窗重叠 332 字——
		// 那样"多段位"只是把同一段给了两遍，还白花 token）。
		if overlaps(taken, lo, hi) {
			// 收缩到不重叠的剩余部分；实在没有就跳过这一段
			lo = hi
			for i := len(taken) - 1; i >= 0; i-- {
				if taken[i][0] >= lo {
					break
				}
				if taken[i][1] > lo {
					lo = taken[i][1]
				}
			}
			hi = minInt(len(body), lo+step)
			if hi-lo < step/4 {
				// 剩余太小，**不值得给一个窗口**（半截窗口既不够读又费 token）
				break
			}
		}
		coord := fmt.Sprintf("rune[%d:%d]", lo, hi)
		text, _ := ResolveSpan(d.Body, coord)
		out = append(out, Hit{DocID: docID, SpanCoord: coord, SpanText: text,
			Title: idx.TitleOf(docID), Score: float64(sp.hits)})
		taken = append(taken, [2]int{lo, hi})
	}
	return out
}

// coordCount 数 term 在文本里出现多少次（按二元组口径，见 Fields）。
func (idx *Index) coordCount(text, term string) int {
	if term == "" {
		return 0
	}
	return strings.Count(text, term)
}

func overlaps(taken [][2]int, lo, hi int) bool {
	for _, t := range taken {
		if lo < t[1] && t[0] < hi {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

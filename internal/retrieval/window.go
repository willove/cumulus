package retrieval

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 窗口机械（本文件从 bm25.go 按职责拆出）：命中 → 窗口坐标 → 条文吸附 →
// 事实锚定。BM25 的打分与排序在 bm25.go，这里是"证据窗怎么开"。

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
	for ti, term := range terms {
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
			positions = append(positions, windowPos{at: len([]rune(body[:byteOff])), term: ti, termRunes: len([]rune(term))})
			from = byteOff + len(term)
		}
	}
	if len(positions) == 0 {
		return ""
	}
	// 窗口中心 = 命中密度最高处：滑动 width 宽的位置，取覆盖词数最多的
	// 起点。**不是最早命中处**——法律名里就带"交通"二字，取最早命中的
	// 话窗口永远停在文档开头（真跑教训：问"交通信号灯"返回法律序言）。
	// 每个命中位置的稀有度 = 其词的 idf 代理（N/df）。**密度按稀有度加
	// 权**，不数命中个数：高频词（"专利"在一部法里出现几十次）会假装
	// 密集，把窗口吸到错误的条文，而事实的真内容在稀缺席（"期限"只在
	// 第四十二条）。真跑教训：专利期限两连问，窗口停在保密审查条。
	posRarity := make([]float64, len(positions))
	for i, q := range positions {
		if df := len(idx.Postings[terms[q.term]]); df > 0 && idx.N > 0 {
			posRarity[i] = float64(idx.N) / float64(df)
		} else {
			posRarity[i] = 1.0
		}
	}
	half := width / 2
	bestStart, bestCover := 0, -1.0
	for _, p := range positions {
		start := p.at - half
		if start < 0 {
			start = 0
		}
		end := start + width
		cover := 0.0
		for j, q := range positions {
			if q.at >= start && q.at < end {
				cover += posRarity[j]
			}
		}
		if cover > bestCover {
			bestCover, bestStart = cover, start
		}
	}
	// 命中簇：密度窗 [bestStart, bestStart+width) 内的全部命中——不是单个
	// 位置（单位置的簇会把隔壁条目的命中漏掉，真跑教训：问"红灯表示
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
	term      int // 命中它的查询词下标（稀有度加权用）
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

// factsCovers 一条事实（以文本给出）是否被这段窗口覆盖（与 facts 包同
// 口径：内容词占比过半 + 事实的 3 字核心在窗内）。
func factsCovers(anchor, text string) bool {
	kws := contentCores(anchor)
	if len(kws) == 0 {
		return false
	}
	hits := 0
	for _, k := range kws {
		if countSub(text, k) > 0 {
			hits++
		}
	}
	if float64(hits)/float64(len(kws)) < 0.5 {
		return false
	}
	r := []rune(anchor)
	if len(r) < 3 {
		return true
	}
	for i := 0; i+2 < len(r); i++ {
		if countSub(text, string(r[i:i+3])) > 0 {
			return true
		}
	}
	return false
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
	for _, r := range q {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			emit()
			if len(lat) > 0 {
				out = append(out, string(lat))
				lat = nil
			}
			han = append(han, r)
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			emit()
			lat = append(lat, r)
		default:
			emit()
			if len(lat) > 0 {
				out = append(out, string(lat))
				lat = nil
			}
		}
	}
	emit()
	if len(lat) > 0 {
		out = append(out, string(lat))
	}
	// 去胶水
	kept := out[:0]
	for _, w := range out {
		if w == "" {
			continue
		}
		glued := false
		for _, ch := range w {
			if indexRune(glue, ch) {
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

func indexRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// countSub 子串出现次数。
func countSub(body, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(body); i++ {
		if body[i:i+len(sub)] == sub {
			n++
			i += len(sub) - 1
		}
	}
	return n
}

// runeIndex 子串的字符偏移（-1 = 不在）。
func runeIndex(body, sub string) int {
	for i := range body {
		if i+len(sub) <= len(body) && body[i:i+len(sub)] == sub {
			return len([]rune(body[:i]))
		}
	}
	return -1
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
			if d, ok := idx.byID[id]; ok {
				text, _ = ResolveSpan(d.Body, coord)
			}
		}
		hits = append(hits, Hit{DocID: id, Score: scores[id], SpanCoord: coord, SpanText: text, Title: idx.TitleOf(id)})
	}
	return hits
}

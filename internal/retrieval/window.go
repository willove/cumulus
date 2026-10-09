package retrieval

import (
	"fmt"
	"regexp"
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
	n := len(runes)
	if n == 0 {
		return ""
	}
	// 密度定心：窗口 [bestStart, bestStart+width) 内查询词命中数最多
	// 的起点。命中计数用窗口内异词数（不是总命中数——一个词命中 10 次
	// 的窗不应该赢过一个 3 个词各中 1 次的窗）。
	type windowPos struct {
		at        int
		term      string
		termRunes int
	}
	var positions []windowPos
	for _, term := range terms {
		tr := []rune(term)
		from := 0
		for {
			loc := byteIndexRune(runes, tr, from)
			if loc < 0 {
				break
			}
			positions = append(positions, windowPos{at: loc, term: term, termRunes: len(tr)})
			from = loc + len(tr)
		}
	}
	if len(positions) == 0 {
		return "" // 一个词都不中：没有窗口可开
	}
	bestStart, bestHits := -1, -1
	for _, p := range positions {
		lo := p.at - width/2
		if lo < 0 {
			lo = 0
		}
		hi := lo + width
		if hi > n {
			hi = n
			lo = hi - width
			if lo < 0 {
				lo = 0
			}
		}
		seen := map[string]bool{}
		for _, q := range positions {
			if q.at >= lo && q.at < hi {
				seen[q.term] = true
			}
		}
		if len(seen) > bestHits {
			bestHits, bestStart = len(seen), lo
		}
	}
	// 命中簇：密度窗内的全部命中
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
	// 条文吸附围着命中簇扩（不是围着窗口扩——围着窗口扩会把命中
	// 所在条文之外的噪声条文拖进来）
	start, end := snapToArticles(runes, from, to, bestStart, bestStart+width)
	if end > n {
		end = n
	}
	return fmt.Sprintf("rune[%d:%d]", start, end)
}

// byteIndexRune 子串在 rune 序列里从 from 起首次出现的 rune 偏移（-1 = 不在）。
func byteIndexRune(runes []rune, sub []rune, from int) int {
	for i := from; i+len(sub) <= len(runes); i++ {
		match := true
		for j := range sub {
			if runes[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// articleMark 匹配"第X条"（中文数字）。吸附只认这个形态——法律的条目
// 边界在中文语料里足够规整，司法解释/宪法同款。
var articleMark = regexp.MustCompile(`第[一二三四五六七八九十百零零]+条`)

// snapToArticles 把命中区间 [lo,hi) 扩展到条文边界：起点回到 lo 前最后
// 一个"第X条"标记，终点推到 hi 后第一个标记。命中必在窗内。没有标记
// 可用时退回原窗口 [fallbackStart, fallbackEnd)。
func snapToArticles(runes []rune, lo, hi, fallbackStart, fallbackEnd int) (int, int) {
	start, end, _ := snapToArticlesInfo(runes, lo, hi, fallbackStart, fallbackEnd)
	return start, end
}

// snapToArticlesInfo 同 snapToArticles，另返回"是否真用上了条文标记"。
// 锚点用它排除目录/前言（没有"第X条"标记的区域不是证据所在——真跑教
// 训：专利法的目录里就有"期限"章节名，簇搜先撞上目录，答案条文第四十
// 二条反而落选）。
func snapToArticlesInfo(runes []rune, lo, hi, fallbackStart, fallbackEnd int) (int, int, bool) {
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
		return fallbackStart, fallbackEnd, false
	}
	if start > lo {
		start = lo
	}
	return start, end, ok1 && ok2
}

// WindowAnchored 与 Window 同逻辑，但吸附锚点是**事实自己在文档里的落
// 点**（fan-out 用：每条事实的窗口收在事实答案所在条文）。找不到锚时
// 退化成 Window（密度定心）。
//
// 锚的三级递进（全部在"一次预计算的出现位置小列表"上判定）：
//  1. 事实的 3 字核心（锚短语的每个三字滑窗）：按出现次数升序、每次
//     出现都试——同一个"专利权"在第一条和第四十三条都有，答案往往在
//     靠后的那次，只试第一次会把锚点扔错条文；
//  2. 用**覆盖判定**选哪个核心：哪个核心吸附出来的窗能让事实的内容
//     词过半命中 + 有 3 字核心在窗内，就用哪个；
//  3. 二级锚：事实的 3 字核心一个都不在文档里（法条换了说法——"期限
//     是多少年" vs "发明专利权的期限为二十年"，3 字连续断了）时，锚
//     在**内容词落点最密处**（半径内内容词全部命中的位置）。这是
//     corePresent 的耐改写版——不要求 3 字连续，要求内容词齐聚。
//
// 性能纪律（真跑教训钉在这）：早期实现对每个候选都在全文档里反复找
// 子串（runeIndex 每调一次扫一遍全文、countInRange 再扫一遍），加深
// 路径 k×2 后，专利法这种长文档直接烧成分钟级 CPU（再问 240s 不返
// 回，goroutine 栈 runnable 停在 fanout）。现在：**一次 O(n·候选数)
// 预计算全部出现位置，之后所有判定只在这些小列表上做**。

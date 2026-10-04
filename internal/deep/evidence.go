// evidence — 证据窗口的整理与度量：合并/重同步/外扩、top 窗口、覆盖与置信度量、引用语料。
// 从 deep.go 纯搬运（2026-10-04 拆分），无语义改动。
package deep

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// srcLabel is the deterministic template's source label: the title, falling
// back to the id. Kept identical to the pre-refactor inline expression so the
// fallback text never changes.
func srcLabel(s source.Source) string {
	if s.Title != "" {
		return s.Title
	}
	return s.ID
}

// deepSynthBonus is the DEEP-tier synthesis calibration bump, applied at
// most once and never past 1.0. Default is now 0 — disabled. MEASURED
// 2026-09-26 (paired single run, same frozen base, 13-query set,
// scripts/bench/results-bonus0.json vs results-head.json): the 0.1 bonus
// bought nothing measurable — total wall 778s → 710s, refusals 4 → 3 with
// it OFF, i.e. no axis improved with it on. It also landed on
// fast.SkipBelow (0.35) and could move an answer across the answered/
// skipped line by itself (mean 4, coverage 0.2 → 0.30 → 0.40), which is
// no way for an unprovenanced constant to behave. Set
// CLUS_DEEP_SYNTH_BONUS=0.1 to restore the old behavior; a repeat run
// (R-E6) would firm the margins up.
func deepSynthBonus() float64 {
	v := strings.TrimSpace(os.Getenv("CLUS_DEEP_SYNTH_BONUS"))
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return f
	}
	return 0
}

// deepMetrics derives the DEEP answer's coverage, confidence and deterministic
// template from the FINAL kept set. Both the primary build and the post-widen
// rebuild go through here: they used to be two ~30-line copies that had already
// drifted apart (the second re-applied the 0.45 incomplete-cover cap, the first
// did not), so a refused-and-recovered answer was scored by different rules
// than a first-pass one.
//
// The +0.1 synthesis bonus and the 0.45 incomplete-cover cap are the DEEP-tier
// calibration; they live in one place so they can be re-measured together.
func deepMetrics(query, srcTitle string, kept []mcs.Sample, rep facts.Report) (coverage, confidence float64, template string) {
	cov := mcs.Coverage(query, kept)
	mean := 0.0
	for _, sm := range kept {
		mean += sm.Score
	}
	if len(kept) > 0 {
		mean /= float64(len(kept))
	}
	conf := mcs.Confidence(mean, cov)
	if b := deepSynthBonus(); b > 0 && conf < 1 {
		conf = min1(conf + b)
	}
	var b strings.Builder
	b.WriteString("【DEEP 摘要】")
	b.WriteString(query)
	b.WriteString("\n")
	// 证据不足时必须先说结论，再说依据。以前这里直接跳到【来源】贴一段最接近的
	// 条文，只在末尾挂一行内部事实 id（"f1"），于是「拒答」在界面上长成了一个像
	// 答案的摘要——用户看到的是引文，读不出「这句话回答不了你的问题」。
	// 头部前缀保持原样：模板识别（fast.RefusedOfSummary 依赖「摘要】」）不能破。
	if !rep.Complete {
		b.WriteString("⚠ 证据不足：这份语料里没有能直接回答这个问题的依据。以下是最接近的原文片段，它不等于答案；请补充相关文档后再问。\n")
	}
	b.WriteString("【来源】")
	b.WriteString(srcTitle)
	b.WriteString("\n")
	for i, sm := range kept {
		fmt.Fprintf(&b, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, trim(sm.Content, 200))
	}
	if !rep.Complete {
		b.WriteString("\n【未覆盖需求】")
		b.WriteString(missingTexts(rep))
		// Weakest-requirement floor: open facts cap confidence.
		if conf > 0.45 {
			conf = 0.45
		}
	}
	return cov, conf, b.String()
}

// missingTexts renders uncovered requirements as the questions a reader asked,
// not the internal fact ids ("f1" means nothing to whoever typed the query).
func missingTexts(rep facts.Report) string {
	texts := map[string]string{}
	for _, f := range rep.Facts {
		if f.Query != "" {
			texts[f.ID] = f.Query
		}
	}
	out := make([]string, 0, len(rep.Missing))
	for _, id := range rep.Missing {
		if text, ok := texts[id]; ok {
			out = append(out, text)
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return "（未细分）"
	}
	return strings.Join(out, "; ")
}

// topKeeps sorts kept windows by score and truncates to the synthesis budget.
// Failed observations are dropped; overlapping/adjacent windows on the same
// source are merged first (GrepRAG: information density > rerank).
func topKeeps(kept []mcs.Sample) []mcs.Sample {
	return topKeepsWith(kept, nil)
}

// topKeepsWith is topKeeps plus optional body-aware boundary expansion (A5).
func topKeepsWith(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	live := kept[:0:0]
	for _, sm := range kept {
		if sm.Failed() {
			continue
		}
		live = append(live, sm)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Score > live[j].Score })
	live = consolidateWindows(live)
	if sources != nil {
		live = expandWindows(live, sources)
		live = consolidateWindows(live)
		// Consolidation can span further than any single window it absorbed,
		// and it has no body in scope to rebuild the text. Re-derive every kept
		// window from its coordinates last: otherwise the span and the text
		// describe different ranges, citations fail to resolve, and the cluster
		// built from them is judged stale on the next read.
		live = resyncContent(live, sources)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Score > live[j].Score })
	keep := maxKeepWindows
	if n, err := strconv.Atoi(os.Getenv("CLUS_DEEP_KEEP_WINDOWS")); err == nil && n > 0 {
		keep = n
	}
	if len(live) > keep {
		live = live[:keep]
	}
	return live
}

// mergeGap is the maximum rune gap between two windows on the same source
// before they stop being "adjacent" (GrepRAG merges overlapping OR adjacent
// slices into one continuous block).
const mergeGap = 1

// consolidateWindows merges overlapping or adjacent windows on the same
// source into continuous spans (GrepRAG structure-aware dedup). Score takes
// the max, covers are unioned. Input need not be sorted; output is by
// (source, start). Content keeps the longest original slice — citations still
// pin (source, start, end).
func consolidateWindows(kept []mcs.Sample) []mcs.Sample {
	if len(kept) <= 1 {
		return kept
	}
	work := append([]mcs.Sample(nil), kept...)
	sort.Slice(work, func(i, j int) bool {
		if work[i].Source != work[j].Source {
			return work[i].Source < work[j].Source
		}
		if work[i].Start != work[j].Start {
			return work[i].Start < work[j].Start
		}
		return work[i].End < work[j].End
	})
	out := work[:0]
	for _, sm := range work {
		if len(out) == 0 {
			out = append(out, sm)
			continue
		}
		last := &out[len(out)-1]
		if last.Source != sm.Source || sm.Start > last.End+mergeGap {
			out = append(out, sm)
			continue
		}
		if sm.End > last.End {
			last.End = sm.End
		}
		if sm.Start < last.Start {
			last.Start = sm.Start
		}
		if sm.Score > last.Score {
			last.Score = sm.Score
			if sm.Arm != "" {
				last.Arm = sm.Arm
			}
		}
		last.Covers = unionStrings(last.Covers, sm.Covers)
		if len(sm.Content) > len(last.Content) {
			last.Content = sm.Content
		}
		if len(sm.Reasoning) > len(last.Reasoning) {
			last.Reasoning = sm.Reasoning
		}
	}
	return out
}

// resyncContent re-derives each window's text from its source coordinates.
// Samples carrying a sampling-method label instead of a document id (the
// FAST/cluster-reuse shape) have no body here and are left alone.
func resyncContent(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	if len(kept) == 0 || len(sources) == 0 {
		return kept
	}
	byID := make(map[string][]rune, len(sources))
	for _, s := range sources {
		byID[s.ID] = []rune(s.Body)
	}
	for i := range kept {
		body, ok := byID[kept[i].Source]
		if !ok {
			continue
		}
		start, end := kept[i].Start, kept[i].End
		if start < 0 || start >= end || end > len(body) {
			continue
		}
		kept[i].Content = string(body[start:end])
	}
	return kept
}

// expandWindows grows each kept span by expandMargin runes against the live
// body (expand boundaries for readable continuous blocks
// while the span stays the source of truth for citations).
const expandMargin = 24

func expandWindows(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	if len(kept) == 0 || len(sources) == 0 {
		return kept
	}
	byID := map[string]source.Source{}
	for _, s := range sources {
		byID[s.ID] = s
	}
	out := append([]mcs.Sample(nil), kept...)
	for i := range out {
		src, ok := byID[out[i].Source]
		if !ok {
			continue
		}
		runes := []rune(src.Body)
		start := out[i].Start - expandMargin
		if start < 0 {
			start = 0
		}
		end := out[i].End + expandMargin
		if end > len(runes) {
			end = len(runes)
		}
		if start >= end {
			continue
		}
		if start != out[i].Start || end != out[i].End {
			out[i].Start, out[i].End = start, end
			out[i].Content = string(runes[start:end])
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// citationCorpus is the source set citations resolve against: the corpus the
// run started from plus whatever widening admitted from outside it. Without
// the extras a widened window resolves to no title and no quote, so the answer
// cites a source it cannot show.
func citationCorpus(corpus, widened []source.Source) []source.Source {
	if len(widened) == 0 {
		return corpus
	}
	out := make([]source.Source, 0, len(corpus)+len(widened))
	out = append(out, corpus...)
	return append(out, widened...)
}

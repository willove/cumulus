package main

// widenFunc builds the DEEP widening callback (Sirchmunk ReAct 对齐): fresh
// keyword cascade over the full corpus first; when that admits nothing — the
// colloquial-query-no-lexical-bridge case — fall back to the semantic arm:
// embed the query (minilm, if available) and admit the KNN neighbours not
// yet tried. Keyword-only widening dead-ends exactly where a user's wording
// never overlaps the legal text (实战: 39 锚点里词面无桥的全灭).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/source"
)

func widenFunc(fe *fast.Engine, st *ingest.Store, c cumulite.Port, sourcesColl string, refiner *llm.AigateKeywordRefiner) func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
	return func(ctx context.Context, query string, exclude map[string]bool, m int, affinity map[string]bool) ([]source.Source, error) {
		all, err := st.ActiveSources(ctx)
		if err != nil {
			return nil, err
		}
		var failed []string
		if out, tried, err := fe.WidenSources(ctx, query, all, exclude, m+len(all)); err != nil {
			return nil, err
		} else if len(out) > 0 {
			return affinityFirst(out, affinity, m), nil
		} else {
			for _, fs := range tried {
				failed = append(failed, fs...)
			}
		}
		if out, _ := widenSemantic(ctx, c, sourcesColl, all, exclude, query, m); len(out) > 0 {
			return out, nil
		}
		// ReAct 精炼轮: both arms empty → regenerate keywords in statutory
		// register and retry the ranking once (Sirchmunk 的迭代改写位).
		if refiner == nil {
			return nil, nil
		}
		refined, err := refiner.Refine(ctx, query, failed)
		if err != nil || len(refined) == 0 {
			return nil, nil
		}
		return fe.AdmitByFields(refined, all, exclude, m), nil
	}
}

// affinityFirst stably partitions candidates: same-family documents
// (business_key prefix in affinity) before the rest — answers cluster within
// a document family.
func affinityFirst(cands []source.Source, affinity map[string]bool, m int) []source.Source {
	if len(affinity) == 0 {
		if len(cands) > m {
			return cands[:m]
		}
		return cands
	}
	var first, rest []source.Source
	for _, s := range cands {
		i := strings.Index(s.BusinessKey, "-")
		if i > 0 && affinity[s.BusinessKey[:i]] {
			first = append(first, s)
		} else {
			rest = append(rest, s)
		}
	}
	out := append(first, rest...)
	if len(out) > m {
		out = out[:m]
	}
	return out
}

// rankFunc builds the DEEP admission ranker: keyword cascade first, then the
// semantic arm's KNN neighbours APPENDED (not replacing). Keyword-only
// ranking at 10k scale floats short high-TFIDF noise statutes to the top
// (diagnosis: 反家庭暴力法's real articles lost to short unrelated ones);
// the semantic neighbours recover the statutes the wording never touches
// (Sirchmunk dir_scan 对齐).
func rankFunc(fe *fast.Engine, st *ingest.Store, c cumulite.Port, sourcesColl string, usage *usageWeights) func(context.Context, string, []source.Source, map[string]bool) ([]source.Source, error) {
	return func(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) ([]source.Source, error) {
		// Sweep wide, then let affinityFirst cut the loop budget: the global
		// top-6 rarely contains the right member of the right family, but the
		// same-family slice of a wide sweep does (gold@4/73 实证).
		out, _, err := fe.WidenSources(ctx, query, sources, nil, 500)
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, s := range out {
			have[s.ID] = true
		}
		extra, err := widenSemantic(ctx, c, sourcesColl, sources, map[string]bool{}, query, maxDeepLoops)
		if err != nil {
			return out, nil
		}
		for _, s := range extra {
			if !have[s.ID] {
				have[s.ID] = true
				out = append(out, s)
			}
		}
		// The semantic arm above is APPENDED after a 500-document lexical
		// sweep, and both usageFirst and mixedAffinity cut the list to
		// maxDeepLoops (=4). A document the embedder ranked #1 by cosine
		// therefore lands at position ~500 and is truncated away before the
		// DEEP loop ever sees it — the arm is computed, paid for, and
		// discarded unless it also happened to be a lexical hit.
		//
		// Evidence it is worth using: -l1pre, which REPLACES the candidate
		// list with the KNN hits in distance order, moved the same 30 cn-law
		// anchors from Ev.Rec 30.0%→43.3% and EM 43.3%→66.7%
		// (var/realeval/results.jsonl vs results_l1.jsonl).
		//
		// promoteSemanticHead is the surgical version: same candidate SET,
		// different ORDER, so the only thing that changes is which documents
		// the loop reaches first. Default off (k=0) is today's behaviour
		// byte-for-byte.
		out = promoteSemanticHead(out, extra, semanticHead())
		// 使用权重提升：本会话刚引过/账本里这些词元反复命中的文档排到探索
		// 最前（加速但不淹没：配额 4，稳定保序，同分不吃掉全局最优）。
		uw := usage.get()
		// 权重文档若没进宽扫（词面完全不匹配的问法），先注入再提升：
		// 重排只能重排已存在的候选，会话/账本背书的文档必须在场。
		out = injectWeighted(out, sources, uw)
		out = usageFirst(out, uw, maxDeepLoops, usageCap())
		if os.Getenv("CLUS_AFFINITY_DEBUG") == "1" {
			names := make([]string, 0, 6)
			for i, s := range out {
				if i >= 6 {
					break
				}
				mark := ""
				if w, ok := uw[s.ID]; ok && w > 0 {
					mark = fmt.Sprintf("(u%.2f)", w)
				}
				names = append(names, s.ID+mark)
			}
			fmt.Fprintf(os.Stderr, "[rankAdmission] %s → %v\n", query, names)
		}
		// 亲缘配额混合：同族最多 3（弱 FAST 答案会把亲缘带偏——真机:
		// 「保护」一词命中妇女权益全家，反家暴法被挤出前 6），全局最优补足。
		return mixedAffinity(out, affinity, maxDeepLoops, 3), nil
	}
}

// semanticHead is how many of the query's KNN neighbours get promoted to the
// head of the DEEP admission list (perf-plan §4.2 / P1-3).
//
// DEFAULT 0 — REVERTED 2026-09-28 from 4.
//
// Why it was set to 4: a paired endpoint-tier A/B on 12 cn-law anchors showed
// ev_rec 4/12 → 6/12 with discordant 0:2 and search_tokens 0.81×.
//
// Why it is back to 0: that A/B's corpus has p50 document length of 145
// CHARACTERS (min 69 / max 470), which fits entirely inside MiniLM's
// 128-token embedding window (internal/minilm/tokenizer.go). The same
// measurement on a real-length document — a 15.5M-rune novel split into 2,212
// blocks, queried with the book's own 4,198 chapter titles — gives:
//
//	MiniLM-384   R@1 0.3%  R@4 1.1%  R@32 4.2%     (random baseline 0.05/0.18/1.4)
//	local-hash-64 R@1 0.1% R@4 0.5%
//
// i.e. the semantic arm is ~6x better than random on 2,212 real blocks, which
// is enough to look like it works and nowhere near enough to be useful. The
// mechanism is the tokenizer's 128-token cap: an 8,000-rune block is 62x the
// window, so its vector represents only its first ~300 characters.
//
// So the +2 was a SHORT-DOCUMENT result and it does not transfer. Promoting a
// ranking that is near-random at document scale is not a free default. The
// lexical arm measures 99.0% R@4 on the same blocks with no embedding at all.
//
// Set CLUS_ADMIT_SEMANTIC_HEAD>0 to opt back in; the arithmetic is unchanged,
// and it remains the right setting for corpora whose documents fit the window.
func semanticHead() int {
	if v := os.Getenv("CLUS_ADMIT_SEMANTIC_HEAD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// promoteSemanticHead moves the first k of `semantic` (KNN distance order) to
// the front of `cands`, keeping every other candidate in its original relative
// order.
//
// Invariants, both gate-pinned in widen_test.go:
//   - the candidate SET is preserved (nothing dropped, nothing duplicated) —
//     only the order changes;
//   - k <= 0 is an identity, so the default path is untouched.
func promoteSemanticHead(cands, semantic []source.Source, k int) []source.Source {
	if k <= 0 || len(semantic) == 0 {
		return cands
	}
	if k > len(semantic) {
		k = len(semantic)
	}
	head := semantic[:k]
	claimed := make(map[string]bool, k)
	for _, s := range head {
		claimed[s.ID] = true
	}
	tail := make([]source.Source, 0, len(cands))
	for _, s := range cands {
		if claimed[s.ID] {
			continue
		}
		tail = append(tail, s)
	}
	out := make([]source.Source, 0, k+len(tail))
	out = append(out, head...)
	return append(out, tail...)
}

// injectWeighted appends usage-weighted documents missing from the sweep.
// A follow-up phrased with zero lexical overlap ("那赔偿呢") produces a
// keyword sweep that never mentions the remembered document — promotion
// alone would then be a no-op, so the document joins the candidate list.
func injectWeighted(cands []source.Source, all []source.Source, weights map[string]float64) []source.Source {
	if len(weights) == 0 {
		return cands
	}
	have := map[string]bool{}
	for _, s := range cands {
		have[s.ID] = true
	}
	var added []source.Source
	for _, s := range all {
		if have[s.ID] {
			continue
		}
		if w, ok := weights[s.ID]; ok && w > 0 {
			added = append(added, s)
			have[s.ID] = true
		}
	}
	return append(cands, added...)
}

// usageCap bounds how many session-weighted documents the DEEP admission may
// promote ahead of the global best (2 by default, was 4 — same tiebreaker
// logic as fast.usageShare). CLUS_USAGE_CAP tunes it.
func usageCap() int {
	if v := os.Getenv("CLUS_USAGE_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// usageFirst stably promotes documents carrying query-conditioned usage
// weight (session evidence stack ∪ affinity ledger) ahead of the rest,
// capped so remembered documents accelerate the loop without flooding it.
func usageFirst(cands []source.Source, weights map[string]float64, m, cap int) []source.Source {
	if len(weights) == 0 || cap <= 0 {
		if len(cands) > m {
			return cands[:m]
		}
		return cands
	}
	var first, rest []source.Source
	for _, s := range cands {
		if w, ok := weights[s.ID]; ok && w > 0 && len(first) < cap {
			first = append(first, s)
			continue
		}
		rest = append(rest, s)
	}
	out := append(first, rest...)
	if len(out) > m {
		out = out[:m]
	}
	return out
}

// mixedAffinity cuts candidates to m: up to cap from the affinity families
// (stable order), then the global best for the rest — 亲缘加速但不淹没全局。
func mixedAffinity(cands []source.Source, affinity map[string]bool, m, cap int) []source.Source {
	var fam, rest []source.Source
	for _, s := range cands {
		i := strings.Index(s.BusinessKey, "-")
		if i > 0 && affinity[s.BusinessKey[:i]] && len(fam) < cap {
			fam = append(fam, s)
		} else {
			rest = append(rest, s)
		}
	}
	out := append(fam, rest...)
	if len(out) > m {
		out = out[:m]
	}
	return out
}

// widenSemantic admits the query's KNN neighbours (索引是缓存：no embedder
// configured, no index, or a KNN error → empty, the search simply stays
// keyword-only — never an error path for the caller).
func widenSemantic(ctx context.Context, c cumulite.Port, sourcesColl string, all []source.Source, exclude map[string]bool, query string, m int) ([]source.Source, error) {
	// The semantic arm is an accelerator: a strict-mode failure degrades to
	// keyword-only with a note, never a failed search (索引是缓存).
	embedFn, _, _, aerr := embedderFor()
	if aerr != nil {
		if os.Getenv("CLUS_VERBOSE") == "1" {
			fmt.Fprintf(os.Stderr, "[widenSemantic] %v — 语义臂缺席，仅词面\n", aerr)
		}
		return nil, nil
	}
	qv, err := embedFn(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, nil
	}
	knn, err := c.KNN(ctx, sourcesColl, contract.KNNRequest{
		Field: "body_embed", Vector: qv[0], K: m, Metric: "cosine",
		Filter: map[string]any{"status": source.StatusActive},
	})
	if err != nil {
		return nil, nil
	}
	byID := map[string]source.Source{}
	for _, s := range all {
		byID[s.ID] = s
	}
	var out []source.Source
	for _, d := range knn.Documents {
		id, _ := d["_id"].(string)
		if exclude[id] {
			continue
		}
		if s, ok := byID[id]; ok {
			out = append(out, s)
			if len(out) >= m {
				break
			}
		}
	}
	return out, nil
}

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
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/source"
)

func widenFunc(fe *fast.Engine, st *ingest.Store, c cumulite.Port, sourcesColl string, refiner *llm.KeywordRefiner) func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
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
		if out := widenSemantic(ctx, c, sourcesColl, all, exclude, query, m); len(out) > 0 {
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
		extra := widenSemantic(ctx, c, sourcesColl, sources, map[string]bool{}, query, maxDeepLoops)
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

// widenSemantic admits the query's KNN neighbours. It returns no error ON
// PURPOSE: the semantic arm is an accelerator, and D7 (索引是缓存) says a
// missing cache makes a search slower, never failed. The signature used to
// carry an `error` that no path ever set — one caller discarded it with `_`,
// the other checked it and then returned nil itself — so the contract advertised
// a channel that did not exist, and the dead check hid the fact.
//
// What it must NOT do is degrade silently. Three different things can fail here
// and they used to collapse into the same empty result with no output at all
// (the single note was gated behind CLUS_VERBOSE=1 and covered only the first):
//
//	seat  — embedderFor refused. Post-G2 this is unreachable in production: the
//	        stack is built through the same call and hard-fails first.
//	embed — the query embedding call itself failed. THIS IS A LIVE DEPENDENCY
//	        FAILING (a remote LLM embedder 401/timeout), and it was the case
//	        with zero diagnostics: every search quietly lost its semantic arm.
//	knn   — no body_embed index, or the engine refused. The normal state for
//	        anyone who never ran `ensure -embed`.
func widenSemantic(ctx context.Context, c cumulite.Port, sourcesColl string, all []source.Source, exclude map[string]bool, query string, m int) []source.Source {
	embedFn, _, _, aerr := embedderFor()
	if aerr != nil {
		noteSemanticArm("seat", aerr)
		return nil
	}
	qv, err := embedFn(ctx, []string{query})
	if err != nil {
		noteSemanticArm("embed", err)
		return nil
	}
	if len(qv) != 1 {
		noteSemanticArm("embed", fmt.Errorf("wanted 1 query vector, got %d", len(qv)))
		return nil
	}
	knn, err := c.KNN(ctx, sourcesColl, contract.KNNRequest{
		Field: "body_embed", Vector: qv[0], K: m, Metric: "cosine",
		Filter: map[string]any{"status": source.StatusActive},
	})
	if err != nil {
		noteSemanticArm("knn", err)
		return nil
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
	return out
}

// noteSemanticArm prints one line per distinct reason per process, to stderr,
// unconditionally. Once-per-process because a missing body_embed index is a
// STATE that would otherwise print on every search; unconditional because a
// flag nobody sets is the same as no output, and this suite's defect history is
// dominated by degradation that looked identical to success.
var (
	semanticArmMu   sync.Mutex
	semanticArmSaid           = map[string]bool{}
	semanticArmOut  io.Writer = os.Stderr // swapped by tests
)

// semanticArmHints turns "the arm is off" into "here is what to do about it".
// A degradation note that names no remedy gets read once and then ignored, which
// is a slower version of the silence it replaced.
var semanticArmHints = map[string]string{
	"seat":  "embedder 座位被拒——先跑 `cumulus-cluster model status` 看权重在不在",
	"embed": "查询嵌入这次调用本身失败了——端点/密钥/模型名都值得查，这不是配置缺席",
	"knn":   "没有 body_embed 向量索引——跑 `cumulus-cluster ensure -embed` 才会物化（它是 opt-in 加速器）",
}

func noteSemanticArm(reason string, err error) {
	semanticArmMu.Lock()
	defer semanticArmMu.Unlock()
	if semanticArmSaid[reason] {
		return
	}
	semanticArmSaid[reason] = true
	fmt.Fprintf(semanticArmOut,
		"[widenSemantic] 语义臂不可用（%s）：%v\n  → 检索继续，但只走词面。%s\n  → 本进程内同一原因只报这一次。\n",
		reason, err, semanticArmHints[reason])
}

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
	"strings"

	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

func widenFunc(fe *fast.Engine, st *ingest.Store, c *client.Client, sourcesColl string, refiner *llm.AigateKeywordRefiner) func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
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
func rankFunc(fe *fast.Engine, st *ingest.Store, c *client.Client, sourcesColl string) func(context.Context, string, []source.Source, map[string]bool) ([]source.Source, error) {
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
		// 亲缘配额混合：同族最多 3（弱 FAST 答案会把亲缘带偏——真机:
		// 「保护」一词命中妇女权益全家，反家暴法被挤出前 6），全局最优补足。
		return mixedAffinity(out, affinity, maxDeepLoops, 3), nil
	}
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
func widenSemantic(ctx context.Context, c *client.Client, sourcesColl string, all []source.Source, exclude map[string]bool, query string, m int) ([]source.Source, error) {
	embedFn, _, _ := embedderFor()
	qv, err := embedFn(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, nil
	}
	knn, err := c.KNN(ctx, sourcesColl, client.KNNRequest{
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

var _ = fmt.Sprintf // keep fmt for future diagnostics without churn

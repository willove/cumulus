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

	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

func widenFunc(fe *fast.Engine, st *ingest.Store, c *client.Client, sourcesColl string) func(context.Context, string, map[string]bool, int) ([]source.Source, error) {
	return func(ctx context.Context, query string, exclude map[string]bool, m int) ([]source.Source, error) {
		all, err := st.ActiveSources(ctx)
		if err != nil {
			return nil, err
		}
		if out, err := fe.WidenSources(ctx, query, all, exclude, m); err != nil {
			return nil, err
		} else if len(out) > 0 {
			return out, nil
		}
		return widenSemantic(ctx, c, sourcesColl, all, exclude, query, m)
	}
}

// rankFunc builds the DEEP admission ranker: keyword cascade first, then the
// semantic arm's KNN neighbours APPENDED (not replacing). Keyword-only
// ranking at 10k scale floats short high-TFIDF noise statutes to the top
// (diagnosis: 反家庭暴力法's real articles lost to short unrelated ones);
// the semantic neighbours recover the statutes the wording never touches
// (Sirchmunk dir_scan 对齐).
func rankFunc(fe *fast.Engine, st *ingest.Store, c *client.Client, sourcesColl string) func(context.Context, string, []source.Source) ([]source.Source, error) {
	return func(ctx context.Context, query string, sources []source.Source) ([]source.Source, error) {
		out, err := fe.WidenSources(ctx, query, sources, nil, maxDeepLoops)
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
		return out, nil
	}
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

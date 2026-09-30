// Rerank is the passage-level minilm comparator: after BM25 narrows to
// top-K candidates, this reorders them by direct semantic similarity
// between the query and each document's text. This is minilm's designed
// use case — sentence/passage-level comparison — NOT document-level
// retrieval (the old body_embed KNN arm, retired with this change).
//
// The distinction matters: KNN scans all 9,600 document vectors to find
// nearest neighbors (poor discrimination among similar legal text), while
// Rerank compares the query directly against 50 pre-filtered candidates
// (high discrimination, small set, minilm's sweet spot).
package index

import (
	"context"
	"math"
	"sort"

	"github.com/willove/cumulus/internal/source"
)

// Rerank reorders candidates by cosine similarity between the query
// embedding and each candidate's body embedding. Candidates that fail to
// embed keep their BM25 rank (degraded, not dropped). The returned slice
// is a reordered copy — the input is not mutated. The embed parameter is
// structurally identical to ingest.EmbedderFn (accepted as a raw func so
// no import cycle).
func Rerank(ctx context.Context, query string, candidates []source.Source,
	embed func(ctx context.Context, texts []string) ([][]float64, error)) []source.Source {
	if len(candidates) <= 1 || embed == nil {
		return candidates
	}
	// Embed the query + all candidate bodies in one batch.
	texts := make([]string, 0, len(candidates)+1)
	texts = append(texts, query)
	for _, s := range candidates {
		texts = append(texts, s.Body)
	}
	vecs, err := embed(ctx, texts)
	if err != nil || len(vecs) != len(texts) {
		return candidates // embedder down — keep BM25 order
	}
	q := vecs[0]
	type scored struct {
		src   source.Source
		score float64
		bm25  int // original BM25 rank for stable tie-breaking
	}
	out := make([]scored, len(candidates))
	for i, s := range candidates {
		cos := cosinef(q, vecs[i+1])
		out[i] = scored{src: s, score: cos, bm25: i}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].bm25 < out[j].bm25
	})
	result := make([]source.Source, len(out))
	for i, s := range out {
		result[i] = s.src
	}
	return result
}

// cosinef is the float64 cosine similarity.
func cosinef(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Corpus-bounded conditional query expansion (Sirchmunk's Expander, v3+v4):
// v3: when BM25 recall is insufficient, LLM selects from corpus vocabulary.
// v4 (RewriteWhenEmpty): when BM25 returns ZERO results, ONE LLM call
// rewrites the query from colloquial to formal corpus language ("帮信罪"
// → "帮助信息网络犯罪活动罪"), then BM25 retries. Only fires on complete
// failure — partial results never trigger it. The answer still comes from
// the corpus; the LLM only bridges the vocabulary of the QUESTION.
package index

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/prompts"
	"github.com/willove/cumulus/internal/source"
)

// MinRecall is the candidate count below which expansion fires.
const MinRecall = 3

// VocabSize is how many top corpus terms are offered to the LLM.
const VocabSize = 200

// Expander selects relevant corpus terms for a query.
type Expander func(ctx context.Context, query string, vocab []string) ([]string, error)

// MakeLLMExpander builds a corpus-bounded Expander: the LLM sees the query
// plus the corpus's own top terms and SELECTS from that list. It cannot
// produce terms the corpus doesn't contain — the boundary is structural.
func MakeLLMExpander(completeWithEffort func(ctx context.Context, prompt string, effort string) (string, error), effort string) Expander {
	return func(ctx context.Context, query string, vocab []string) ([]string, error) {
		tmpl := prompts.MustRender(prompts.SelectTerms, map[string]string{
			"query": query,
			"terms": strings.Join(vocab, "\n"),
		})
		raw, err := completeWithEffort(ctx, tmpl, effort)
		if err != nil {
			return nil, err
		}
		return ParseExpandTerms(raw), nil
	}
}

// ParseExpandTerms extracts the JSON string array from the LLM response.
func ParseExpandTerms(raw string) []string {
	start := strings.Index(raw, "[")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(raw, "]")
	if end <= start {
		return nil
	}
	var terms []string
	if err := json.Unmarshal([]byte(raw[start:end+1]), &terms); err != nil {
		return nil
	}
	seen := make(map[string]bool, len(terms))
	var out []string
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" || len([]rune(t)) > 16 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// TopVocab returns the top-N corpus terms by document frequency.
func (idx *Index) TopVocab(n int) []string {
	if idx == nil || n <= 0 {
		return nil
	}
	type termDF struct {
		term string
		df   int
	}
	var terms []termDF
	for term, postings := range idx.Postings {
		terms = append(terms, termDF{term, len(postings)})
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].df != terms[j].df {
			return terms[i].df > terms[j].df
		}
		return terms[i].term < terms[j].term
	})
	if len(terms) > n {
		terms = terms[:n]
	}
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.term
	}
	return out
}

// NarrowWithExpansion: BM25 first → if recall < MinRecall, LLM selects from
// corpus vocab → BM25 second pass. Selected terms are validated against
// the index (must exist in Postings) — the corpus-primacy boundary.
func (idx *Index) NarrowWithExpansion(ctx context.Context, query string, sources []source.Source, k int, expand Expander) []source.Source {
	if idx == nil {
		return sources
	}
	top := idx.Narrow(query, sources, k)
	if len(top) >= MinRecall || expand == nil {
		return top
	}
	vocab := idx.TopVocab(VocabSize)
	if len(vocab) == 0 {
		return top
	}
	selected, err := expand(ctx, query, vocab)
	if err != nil || len(selected) == 0 {
		return top
	}
	// Validate: only terms that actually exist in the index.
	var valid []string
	for _, t := range selected {
		if _, ok := idx.Postings[t]; ok {
			valid = append(valid, t)
		}
	}
	if len(valid) == 0 {
		return top
	}
	enriched := query + " " + strings.Join(valid, " ")
	retry := idx.Narrow(enriched, sources, k)
	if len(retry) > len(top) {
		return retry
	}
	return top
}

// Rewriter converts a query from colloquial to corpus language.
// Production wires a deep-thinking LLM call; tests wire a stub.
type Rewriter func(ctx context.Context, query string) (string, error)

// MakeLLMRewriter builds a Rewriter from the rewrite_query prompt with
// the given thinking depth. The LLM's job is reading comprehension —
// understand the abbreviation/colloquialism and produce the formal term
// the corpus uses. It does NOT answer the question (corpus-primacy).
func MakeLLMRewriter(completeWithEffort func(ctx context.Context, prompt string, effort string) (string, error), effort string) Rewriter {
	return func(ctx context.Context, query string) (string, error) {
		tmpl := prompts.MustRender(prompts.RewriteQuery, map[string]string{"query": query})
		raw, err := completeWithEffort(ctx, tmpl, effort)
		if err != nil {
			return "", err
		}
		rewritten := strings.TrimSpace(raw)
		// Strip "改写后的查询:" prefix if the model includes it.
		if i := strings.Index(rewritten, ":"); i >= 0 && i < 20 {
			rewritten = strings.TrimSpace(rewritten[i+1:])
		}
		// Strip quotes.
		rewritten = strings.Trim(rewritten, "\"'“”‘’ \n\t")
		if rewritten == "" || rewritten == query {
			return "", nil // no useful rewrite
		}
		return rewritten, nil
	}
}

// MinRewriteScore is the BM25 top-score confidence line. Below it, the
// results are garbage matches via common bigrams ("什么" IDF≈0 matching
// everything) — as useless as zero for vocabulary-gap queries, but they
// mask the miss from a count-based check.
const MinRewriteScore = 1.0

// TopBM25Score returns the BM25 score of the best-matching document for
// this query. 0 means no matching document at all.
func (idx *Index) TopBM25Score(query string) float64 {
	if idx == nil || idx.N == 0 || strings.TrimSpace(query) == "" {
		return 0
	}
	terms := mcs.Fields(query)
	if len(terms) == 0 {
		return 0
	}
	seen := make(map[string]bool)
	scores := make(map[string]float64)
	for _, term := range terms {
		if seen[term] {
			continue
		}
		seen[term] = true
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
	top := 0.0
	for _, sc := range scores {
		if sc > top {
			top = sc
		}
	}
	return top
}

// RewriteWhenEmpty: if BM25's top score is below MinRewriteScore (the
// results are garbage matches via common bigrams, not real hits), ONE LLM
// call rewrites the query into corpus language and BM25 retries. This is
// the "just try once" the user asked for: 帮信罪 → 帮助信息网络犯罪
// 活动罪 → retry. If the rewrite still yields nothing, the honest empty
// result stands.
func (idx *Index) RewriteWhenEmpty(ctx context.Context, query string, sources []source.Source, k int, rewrite Rewriter) []source.Source {
	if idx == nil {
		return sources
	}
	// Confidence check: BM25 top score. High score → real hits, no rewrite.
	// Low score → garbage matches or nothing → rewrite fires.
	topScore := idx.TopBM25Score(query)
	if topScore >= MinRewriteScore || rewrite == nil {
		ids := idx.Rank(query, k)
		if len(ids) == 0 {
			return sources // no rewriter or no signal — full list
		}
		return buildFromIDs(ids, sources)
	}
	// Low confidence: one rewrite attempt.
	rewritten, err := rewrite(ctx, query)
	if err != nil || rewritten == "" {
		return sources // rewrite failed — full list fallback
	}
	ids := idx.Rank(rewritten, k)
	if len(ids) == 0 {
		return nil // rewrite still finds nothing — honest empty
	}
	return buildFromIDs(ids, sources)
}

// buildFromIDs returns active sources matching the ranked IDs, in rank order.
func buildFromIDs(ids []string, sources []source.Source) []source.Source {
	idSet := make(map[string]int, len(ids))
	for i, id := range ids {
		idSet[id] = i
	}
	var out []source.Source
	for _, s := range sources {
		if rank, ok := idSet[s.ID]; ok && s.Status == source.StatusActive {
			out = append(out, s)
			_ = rank
		}
	}
	return out
}

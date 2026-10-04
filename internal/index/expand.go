// Corpus-bounded query vocabulary bridging — this file holds two generations
// and only one of them runs.
//
// v4 (RewriteWhenEmpty) — LIVE, wired in searchapi.go loadCandidates and
// evalrun.go. On a vocabulary gap, ONE LLM call rewrites the whole question from
// colloquial to the corpus's own formal language ("帮信罪" →
// "帮助信息网络犯罪活动罪"), then BM25 retries. The trigger has been
// `top score < MinRewriteScore OR VocabGapFraction ≥ RewriteGapFraction` since
// 2124b76 — NOT "BM25 returned zero", and NOT "partial results never trigger
// it": on the 1,548-document law corpus a pure-noise match scored 14.2 against
// an absolute line of 1.0, so an absolute threshold could not separate gap from
// noise there. The answer still comes from the corpus; the LLM only bridges the
// vocabulary of the QUESTION.
//
// v3 (NarrowWithExpansion, MakeLLMExpander, Expander, VocabSize,
// ParseExpandTerms, MinRecall) — NOT WIRED. Zero production call sites; only
// expand_test.go reaches it. It added corpus-derived terms when recall fell
// under MinRecall, where v4 rewrites the question instead. Kept as the
// comparison arm for the open admission-ranking gap
// (docs/open-decisions.md §一 — v3-vs-v4 has never been measured), not because
// anything uses it. Do not describe it as restored or as "Sirchmunk's Expander
// back in place": two comments in cmd/cumulus-cluster asserted exactly that
// until 2026-10-02 and were wrong.
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
// NOT WIRED — v3 only; see the package comment.
const MinRecall = 3

// VocabSize is how many top corpus terms are offered to the LLM.
const VocabSize = 200

// Expander selects relevant corpus terms for a query.
type Expander func(ctx context.Context, query string, vocab []string) ([]string, error)

// MakeLLMExpander builds a corpus-bounded Expander: the LLM sees the query
// plus the corpus's own top terms and SELECTS from that list. It cannot
// produce terms the corpus doesn't contain — the boundary is structural.
//
// NOT WIRED — v3 only; see the package comment.
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
//
// NOT WIRED — v3, superseded by RewriteWhenEmpty; see the package comment.
// expand_test.go is the only caller, and it is what holds the corpus-primacy
// contract that v4's own rewrite_test.go does not restate term-by-term.
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
//
// It is a NEAR-ZERO line only: on a real corpus, pure noise can score far
// above it (measured 2026-10-01 on laws-full, 1,548 docs: "帮信罪是什么"
// noise-scores 14.2 while a genuine hit scores 54), so this line alone
// cannot separate garbage from signal. The discriminator for that is
// VocabGapFraction below.
const MinRewriteScore = 1.0

// RewriteGapFraction is the vocabulary-gap line: if at least this fraction
// of the query's IDF mass sits on terms the corpus does not contain at all
// (df=0), the query is speaking words this corpus has never heard (口语缩写
// "帮信罪" vs 法条正式名 "帮助信息网络犯罪活动罪" — the abbreviation's
// bigrams 帮信/信罪 have zero postings while the generic bigrams 什么/罪是
// match everything weakly). That is the corpus-independent signature of a
// vocabulary gap, unlike an absolute BM25 score which scales with corpus
// size and document length.
const RewriteGapFraction = 0.5

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

// VocabGapFraction returns the fraction of the query's IDF weight that sits
// on terms with ZERO postings — terms this corpus has never contained. A
// high fraction means the query's most discriminative words don't exist in
// corpus language (帮信/是什 for 帮信罪), so whatever BM25 did match came from
// generic bigrams and is noise regardless of its score.
//
// Weighting is idf² — rarity squared. Measured on laws-full (1,548 docs,
// 2026-10-01) this is what separates a true vocabulary gap from a working
// colloquial query whose boundary bigrams also happen to be absent:
//
//	帮信罪是什么      gap 0.589 → fires  (survivors 信罪 df4/罪是 df6/
//	                                什么 df9 are coincidental fragments)
//	闯红灯会有什么处罚 gap 0.457 → stays (红灯 df3/灯会 df1/处罚 df771 are
//	                                real content hits, keep their mass)
//
// Linear idf weighting cannot separate these two (0.495 vs 0.368): the
// generic survivors of a legal-language corpus carry too much plain idf.
// Deterministic, zero-LLM, O(query terms).
func (idx *Index) VocabGapFraction(query string) float64 {
	if idx == nil || idx.N == 0 || strings.TrimSpace(query) == "" {
		return 0
	}
	seen := make(map[string]bool)
	var total, gap float64
	for _, term := range mcs.Fields(query) {
		if seen[term] {
			continue
		}
		seen[term] = true
		df := float64(len(idx.Postings[term]))
		w := math.Log(1 + (float64(idx.N)-df+0.5)/(df+0.5))
		w *= w
		total += w
		if df == 0 {
			gap += w
		}
	}
	if total <= 0 {
		return 0
	}
	return gap / total
}

// RewriteWhenEmpty: if BM25's top score is below MinRewriteScore (near-zero
// signal) OR the query's IDF mass is mostly on terms absent from the corpus
// vocabulary (VocabGapFraction ≥ RewriteGapFraction — the 帮信案 signature:
// noise can outscore the absolute line, but the missing terms betray it),
// ONE LLM call rewrites the query into corpus language and BM25 retries.
// This is the "just try once" the user asked for: 帮信罪 → 帮助信息网络
// 犯罪活动罪 → retry. If the rewrite still yields nothing, the honest empty
// result stands.
// RewriteWhenEmpty reranks via BM25 and, when the first pass looks like a
// vocabulary gap (top score under the near-zero line, or the discriminative
// terms missing from the corpus), makes ONE rewritten attempt. The second
// return is the rewritten query actually used — "" means the original ranked
// (or the rewrite never fired); it exists so callers can show the user WHY
// the retrieval terms changed, not just that they did.
func (idx *Index) RewriteWhenEmpty(ctx context.Context, query string, sources []source.Source, k int, rewrite Rewriter) ([]source.Source, string) {
	if idx == nil {
		return sources, ""
	}
	// Confidence check, two independent miss signatures:
	//   1. top score below the near-zero line — nothing matched at all;
	//   2. vocabulary gap — the discriminative terms don't exist in the
	//      corpus, so any matches are generic-bigram noise by construction.
	// Only when BOTH are clean (score above the line AND gap small) is the
	// BM25 result trusted without a rewrite.
	topScore := idx.TopBM25Score(query)
	gap := idx.VocabGapFraction(query)
	if (topScore >= MinRewriteScore && gap < RewriteGapFraction) || rewrite == nil {
		ids := idx.Rank(query, k)
		if len(ids) == 0 {
			return sources, "" // no rewriter or no signal — full list
		}
		return buildFromIDs(ids, sources), ""
	}
	// Low confidence: one rewrite attempt.
	rewritten, err := rewrite(ctx, query)
	if err != nil || rewritten == "" {
		return sources, "" // rewrite failed — full list fallback
	}
	ids := idx.Rank(rewritten, k)
	if len(ids) == 0 {
		return nil, rewritten // rewrite still finds nothing — honest empty
	}
	return buildFromIDs(ids, sources), rewritten
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

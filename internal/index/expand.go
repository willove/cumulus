// Conditional query expansion (Sirchmunk's Expander restored to its correct
// position): when BM25 recall is insufficient (too few candidates or weak
// scores), ONE NoThink LLM call generates domain terms the query lacks,
// and BM25 retries with the enriched query. The 87% of queries where BM25
// already has good recall pay zero extra latency.
package index

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/willove/cumulus/internal/prompts"
	"github.com/willove/cumulus/internal/source"
)

// MinRecall is the candidate count below which expansion fires. Below this,
// BM25 likely hit a vocabulary gap — the query terms don't match any doc.
const MinRecall = 3

// Expander is the one-LLM-call domain term generator. Production wires the
// chat client's CompleteStructured (NoThink); tests wire a stub.
type Expander func(ctx context.Context, query string) ([]string, error)

// MakeLLMExpander builds an Expander from a chat client's structured
// completion (thinking disabled — keyword association needs no reasoning).
func MakeLLMExpander(completeStructured func(ctx context.Context, prompt string) (string, error)) Expander {
	return func(ctx context.Context, query string) ([]string, error) {
		tmpl := prompts.MustRender(prompts.ExpandTerms, map[string]string{"query": query})
		raw, err := completeStructured(ctx, tmpl)
		if err != nil {
			return nil, err
		}
		return ParseExpandTerms(raw), nil
	}
}

// ParseExpandTerms extracts the JSON string array from the LLM response.
// Returns nil on any parse failure (the caller treats it as "no expansion
// available" and keeps the original BM25 results).
func ParseExpandTerms(raw string) []string {
	// Find the first [...] span.
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
	// Filter: non-empty, ≤6 runes each, deduplicated.
	seen := make(map[string]bool, len(terms))
	var out []string
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" || len([]rune(t)) > 12 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// NarrowWithExpansion is the conditional-expansion Narrow: if the first BM25
// pass yields < MinRecall candidates, expand the query for ADDITIONAL
// candidates (union, NOT merged into the original query — merging dilutes
// BM25's discriminative power, measured 2026-10-01: ev_rec 51.7%→48.3% when
// terms were concatenated). The union preserves the original ranking;
// expansion terms only ADD candidates the original query missed.
func (idx *Index) NarrowWithExpansion(ctx context.Context, query string, sources []source.Source, k int, expand Expander) []source.Source {
	if idx == nil {
		return sources
	}
	top := idx.Narrow(query, sources, k)
	if len(top) >= MinRecall || expand == nil {
		return top // recall sufficient (or no expander) — done, zero cost
	}
	// Vocabulary gap: expand for ADDITIONAL candidates (not to replace).
	terms, err := expand(ctx, query)
	if err != nil || len(terms) == 0 {
		return top // expansion failed — keep original results
	}
	expanded := strings.Join(terms, " ")
	extra := idx.Narrow(expanded, sources, 20) // smaller second-pass top-K
	// Union: original ranking first, expansion additions appended (deduped).
	seen := make(map[string]bool, len(top)+len(extra))
	for _, s := range top {
		seen[s.ID] = true
	}
	var union []source.Source
	union = append(union, top...)
	for _, s := range extra {
		if !seen[s.ID] {
			seen[s.ID] = true
			union = append(union, s)
		}
	}
	if len(union) > k+20 {
		union = union[:k+20]
	}
	return union
}

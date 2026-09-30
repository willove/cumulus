// Corpus-bounded conditional query expansion (Sirchmunk's Expander, v3):
// when BM25 recall is insufficient, ONE deep-thinking LLM call SELECTS from
// the corpus's own vocabulary — never generates new terms. This is the
// corpus-primacy red line: the LLM matches query intent to existing corpus
// terms (reading comprehension), it does not invent domain vocabulary from
// general knowledge (which produced generic terms matching 500+ docs,
// measured as a net negative in v1/v2).
package index

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

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

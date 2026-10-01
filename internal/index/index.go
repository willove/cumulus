// Package index is the inverted index + BM25 ranker that replaces the O(N)
// full-corpus keyword scan in admission (the P0 fix measured 2026-09-30: at
// 9,600 docs, every query did 3-5 full scans — gold_not_admitted 100%, p95
// 93s). The index is built once at ingest/serve from the corpus's own terms
// (mcs.Fields bigram tokenization — same tokenizer as the query side, so
// matching semantics are equivalent to substring matching for CJK), and at
// query time a BM25 lookup narrows the candidate set from N to top-K.
//
// The whole package is zero-LLM, zero-model, deterministic: the knowledge is
// the corpus's own term statistics.
package index

import (
	"math"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// BM25 parameters — the Robertson-Sparck Jones defaults, fifty years of
// literature. k1 controls TF saturation, b controls document-length
// normalization. Not tunable by design: these are the values every search
// engine uses.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Posting is one (document, term-frequency) pair in the inverted list.
type Posting struct {
	DocID string
	TF    int
}

// Index is the in-memory inverted index over active sources.
type Index struct {
	// Postings maps each term to its inverted list (docs containing it).
	Postings map[string][]Posting
	// DocLens maps doc ID to total token count (for BM25 length norm).
	DocLens map[string]int
	// N is the number of indexed documents.
	N int
	// AvgLen is the mean token count across documents.
	AvgLen float64
	// byID maps doc ID to the source (for Narrow).
	byID map[string]source.Source
}

// Build constructs the inverted index from active sources. Terms come from
// mcs.Fields(body) — the same tokenizer the query side uses, so a CJK
// bigram in the query matches exactly the bigrams indexed from the body.
// Deterministic, zero-LLM, O(total_body_length).
func Build(sources []source.Source) *Index {
	idx := &Index{
		Postings: make(map[string][]Posting, 4096),
		DocLens:  make(map[string]int, len(sources)),
		byID:     make(map[string]source.Source, len(sources)),
	}
	var totalTokens int
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		idx.byID[s.ID] = s
		tokens := mcs.Fields(s.Body)
		idx.DocLens[s.ID] = len(tokens)
		totalTokens += len(tokens)
		// Per-doc term frequencies.
		tf := make(map[string]int, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for term, count := range tf {
			idx.Postings[term] = append(idx.Postings[term], Posting{DocID: s.ID, TF: count})
		}
		idx.N++
	}
	if idx.N > 0 {
		idx.AvgLen = float64(totalTokens) / float64(idx.N)
	}
	return idx
}

// Rank scores documents against the query with BM25 and returns the top-K
// doc IDs. Deterministic: ties broken by doc ID (lexicographic).
func (idx *Index) Rank(query string, k int) []string {
	if idx == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	terms := mcs.Fields(query)
	if len(terms) == 0 {
		return nil
	}
	// Deduplicate query terms (bigram expansion produces overlaps).
	seen := make(map[string]bool, len(terms))
	var unique []string
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	return idx.RankTerms(unique, k)
}

// RankTerms is Rank over an already-tokenized term list. It exists because
// re-ranking a SUBSET of the query's terms cannot go through the query
// string: mcs.Fields would re-tokenize any string we built and hand back
// different bigrams than the ones we selected. The vocabulary-gap repair
// (drop the terms the corpus has never seen, keep the rest) is exactly that
// case — see VocabGapFraction for what "gap" means. Callers pass terms from
// mcs.Fields(query); duplicates are ignored.
func (idx *Index) RankTerms(terms []string, k int) []string {
	if idx == nil || idx.N == 0 || k <= 0 {
		return nil
	}
	seen := make(map[string]bool, len(terms))
	var unique []string
	for _, t := range terms {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		unique = append(unique, t)
	}
	if len(unique) == 0 {
		return nil
	}

	// BM25 scoring: only visit docs that contain at least one query term —
	// this is the O(matching) lookup that replaces the O(N) full scan.
	scores := make(map[string]float64, 256)
	for _, term := range unique {
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
	if len(scores) == 0 {
		return nil
	}

	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(scores))
	for id, sc := range scores {
		if sc > 0 {
			hits = append(hits, hit{id, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.id
	}
	return out
}

// Narrow returns the top-K source.Source objects matching the query,
// restricted to the given candidate list (usually the full ActiveSources
// read). The returned slice is a new allocation — the caller's list is
// untouched. Sources not in the index (stale, new, filtered) are dropped.
func (idx *Index) Narrow(query string, sources []source.Source, k int) []source.Source {
	if idx == nil || k <= 0 {
		return sources
	}
	top := idx.Rank(query, k)
	if len(top) == 0 {
		return sources // no signal — fall through to the full list
	}
	rank := make(map[string]int, len(top))
	for i, id := range top {
		rank[id] = i
	}
	out := make([]source.Source, 0, len(top))
	for _, s := range sources {
		if _, ok := rank[s.ID]; ok && s.Status == source.StatusActive {
			out = append(out, s)
		}
	}
	// Preserve BM25 rank order (not source-list order).
	sort.SliceStable(out, func(i, j int) bool {
		return rank[out[i].ID] < rank[out[j].ID]
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

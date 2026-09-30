// Package vocab is the corpus's self-describing vocabulary (B1 第一砖,
// PRF 治本线第三层): terms mined from the corpus by PURE statistics — no
// LLM anywhere in the build — each carrying a local-embedding vector.
//
// Its query-time role is the vocabulary gap: when a query's words lexically
// miss the whole corpus (primary AND fallback rank zero — the 女儿国/
// 西梁女界 shape), the bridge returns the corpus's own nearest terms and
// the cascade retries with them BEFORE paying the LLM expander. Knowledge
// stays corpus-derived end to end: the model never supplies vocabulary.
package vocab

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"sync"
	"unicode"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"

	"github.com/willove/cumulus/internal/ingest"
)

// Entry is one vocabulary term with its corpus statistics and vector.
type Entry struct {
	Term  string    `json:"term"`
	DF    int       `json:"df"`   // documents containing the term
	Freq  int       `json:"freq"` // total occurrences
	Embed []float64 `json:"embed,omitempty"`
}

// Table is the loaded vocabulary. embedFn is stamped at runtime by the
// Store (the same local seat the corpus uses) so the cache never bakes a
// seat in.
type Table struct {
	Entries []Entry `json:"entries"`
	Model   string  `json:"model,omitempty"`
	Dims    int     `json:"dims,omitempty"`

	embedFn ingest.EmbedderFn `json:"-"`
}

// Mine extracts the corpus's characteristic CJK terms: n-grams (2..6 runes)
// with document frequency ≥ minDF, maximal-substring filtered (a fragment
// that mostly occurs inside a longer frequent run loses to the run), ranked
// by df then freq. Deterministic — no model anywhere.
func Mine(docs []string, topK, minDF int) []Entry {
	if topK <= 0 {
		topK = 2000
	}
	if minDF < 1 {
		minDF = 2
	}
	const maxRunes = 6
	freq, df := map[string]int{}, map[string]int{}
	for _, doc := range docs {
		seen := map[string]bool{}
		runes := []rune(doc)
		for i := 0; i < len(runes); i++ {
			if !unicode.Is(unicode.Han, runes[i]) {
				continue
			}
			j := i
			for j < len(runes) && unicode.Is(unicode.Han, runes[j]) {
				j++
			}
			run := runes[i:j]
			for a := 0; a < len(run); a++ {
				for n := 2; n <= maxRunes && a+n <= len(run); n++ {
					g := string(run[a : a+n])
					freq[g]++
					seen[g] = true
				}
			}
			i = j
		}
		for g := range seen {
			df[g]++
		}
	}
	// Extension index: for length n, which (n+1)-grams extend it at either
	// end — built once, so the domination check is O(candidates × exts).
	prefixExt := map[string][]string{}
	suffixExt := map[string][]string{}
	for k := range freq {
		r := []rune(k)
		if len(r) < 3 || len(r) > maxRunes {
			continue
		}
		prefixExt[string(r[1:])] = append(prefixExt[string(r[1:])], k)
		suffixExt[string(r[:len(r)-1])] = append(suffixExt[string(r[:len(r)-1])], k)
	}
	dominated := func(g string) bool {
		for _, ext := range append(append([]string{}, prefixExt[g]...), suffixExt[g]...) {
			if len([]rune(ext)) == len([]rune(g))+1 && freq[ext] >= 8*freq[g]/10 {
				return true
			}
		}
		return false
	}
	type cand struct {
		term      string
		df, freqv int
	}
	var out []cand
	for g, d := range df {
		if d < minDF || dominated(g) {
			continue
		}
		out = append(out, cand{g, d, freq[g]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].df != out[j].df {
			return out[i].df > out[j].df
		}
		if out[i].freqv != out[j].freqv {
			return out[i].freqv > out[j].freqv
		}
		return out[i].term < out[j].term
	})
	if len(out) > topK {
		out = out[:topK]
	}
	entries := make([]Entry, len(out))
	for i, c := range out {
		entries[i] = Entry{Term: c.term, DF: c.df, Freq: c.freqv}
	}
	return entries
}

// Build mines then embeds the table with the SAME local seat the corpus
// uses (no LLM): bridge vectors and corpus vectors must share a space for
// cosine to mean anything.
func Build(ctx context.Context, docs []string, topK int, embed ingest.EmbedderFn, model string) (*Table, error) {
	entries := Mine(docs, topK, 2)
	if len(entries) == 0 {
		return &Table{Model: model}, nil
	}
	terms := make([]string, len(entries))
	for i, e := range entries {
		terms[i] = e.Term
	}
	vecs, err := embed(ctx, terms)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if i < len(vecs) {
			entries[i].Embed = vecs[i]
		}
	}
	dims := 0
	if len(vecs) > 0 {
		dims = len(vecs[0])
	}
	return &Table{Entries: entries, Model: model, Dims: dims}, nil
}

// Nearest returns up to k vocabulary terms whose cosine to the query clears
// 0.3, closest first. nil when no table / no embedder / embed failure — the
// caller treats the bridge as simply unavailable.
func (t *Table) Nearest(query string, k int) []string {
	if t == nil || len(t.Entries) == 0 || k <= 0 || t.embedFn == nil {
		return nil
	}
	vecs, err := t.embedFn(context.Background(), []string{query})
	if err != nil || len(vecs) != 1 {
		return nil
	}
	q := vecs[0]
	type hit struct {
		term  string
		score float64
	}
	var hits []hit
	for _, e := range t.Entries {
		if len(e.Embed) != len(q) {
			continue
		}
		if s := cosine(q, e.Embed); s > 0.3 {
			hits = append(hits, hit{e.Term, s})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].term < hits[j].term
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.term
	}
	return out
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Collection layout: clus_vocab documents + a meta KV record.
const (
	collection = "clus_vocab"
	metaKey    = "clus:vocab:meta"
)

// Store persists and loads tables over a cumulite Port with a per-process
// cache (serve is long-lived; a rebuilt table needs a process restart —
// the v1 contract, stated in the subcommand help).
type Store struct {
	c  cumulite.Port
	ns string

	mu    sync.Mutex
	cache *Table
}

// NewStore builds a cached store handle (namespace reserved for per-scenario
// vocabularies once D12 lands; today one table per store).
func NewStore(c cumulite.Port, namespace string) *Store {
	return &Store{c: c, ns: namespace}
}

// Save persists the table. Per-term documents carry the vectors; the meta KV
// records the seat so a table built with one embedder is never silently
// queried with another.
func (s *Store) Save(ctx context.Context, t *Table) error {
	if err := s.c.EnsureCollection(ctx, collection); err != nil {
		return err
	}
	docs := make([]map[string]any, 0, len(t.Entries))
	for _, e := range t.Entries {
		docs = append(docs, map[string]any{
			"term": e.Term, "df": e.DF, "freq": e.Freq, "embed": e.Embed,
		})
	}
	if len(docs) > 0 {
		if _, err := s.c.Insert(ctx, collection, docs); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(map[string]any{"model": t.Model, "dims": t.Dims, "count": len(t.Entries)})
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, metaKey, raw, 0)
}

// Load returns the cached table or loads it once (paginated). nil table =
// vocabulary not built yet. The returned copy carries the runtime embedFn.
func (s *Store) Load(ctx context.Context, embedFn ingest.EmbedderFn) (*Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		return stamp(s.cache, embedFn), nil
	}
	var entries []Entry
	skip := 0
	for {
		res, err := s.c.Query(ctx, collection, contract.Query{Limit: 500, Skip: skip})
		if err != nil {
			if contract.IsNotFound(err) {
				break
			}
			return nil, err
		}
		if len(res.Documents) == 0 {
			break
		}
		skip += len(res.Documents)
		for _, d := range res.Documents {
			term, _ := d["term"].(string)
			if term == "" {
				continue
			}
			e := Entry{Term: term}
			switch n := d["df"].(type) {
			case float64:
				e.DF = int(n)
			case int:
				e.DF = n
			}
			switch n := d["freq"].(type) {
			case float64:
				e.Freq = int(n)
			case int:
				e.Freq = n
			}
			if raw, ok := d["embed"].([]any); ok {
				e.Embed = make([]float64, len(raw))
				for i, v := range raw {
					f, _ := v.(float64)
					e.Embed[i] = f
				}
			}
			entries = append(entries, e)
		}
		if len(res.Documents) < 500 {
			break
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	t := &Table{Entries: entries}
	s.cache = t
	return stamp(t, embedFn), nil
}

func stamp(t *Table, embedFn ingest.EmbedderFn) *Table {
	cp := *t
	cp.embedFn = embedFn
	return &cp
}

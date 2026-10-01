package vocab

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulite"
)

// TestMineMaximalSubstring pins the mining contract: the LONGEST frequent
// run wins (西梁女界), fragments mostly inside it (女儿) lose, rare noise
// is below the df floor, and ranking is df-desc deterministic.
func TestMineMaximalSubstring(t *testing.T) {
	docs := []string{
		"西梁女界尽是女子，女儿国之说由此而来", // 西梁女界 ×1, 女子, 女儿
		"唐僧一行路过高粱 EVT——西梁女界国主设宴",
		"西梁女界之外另有一城，其民亦皆女子",
		"取经人至西梁女界，欲倒换关文",
	}
	entries := Mine(docs, 100, 2)
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Term] = true
	}
	if !have["西梁女界"] {
		t.Fatalf("the maximal run 西梁女界 must be mined: %+v", entries)
	}
	if have["梁女"] {
		t.Fatal("梁女 is a fragment inside the run — maximal filtering must drop it")
	}
	// Determinism: same input, same output.
	again := Mine(docs, 100, 2)
	if len(again) != len(entries) {
		t.Fatal("mining is not deterministic")
	}
	for i := range again {
		if again[i].Term != entries[i].Term {
			t.Fatal("mining order is not deterministic")
		}
	}
}

// fakeEmbed places planted synonyms next to each other in a 4-dim space and
// everything else orthogonal — the bridge's semantics without any model.
func fakeEmbed(syn map[string][4]float64) func(ctx context.Context, texts []string) ([][]float64, error) {
	return func(_ context.Context, texts []string) ([][]float64, error) {
		out := make([][]float64, len(texts))
		for i, s := range texts {
			if v, ok := syn[s]; ok {
				out[i] = []float64{v[0], v[1], v[2], v[3]}
				continue
			}
			// Deterministic far-away vector for unlisted strings.
			out[i] = []float64{0, 0, 1, float64(len(s))}
		}
		return out, nil
	}
}

// TestNearestBridgesTheGap is the 女儿国 shape: the query word is lexically
// ABSENT from the corpus, the corpus's own term is its semantic neighbour,
// and the bridge returns it — zero LLM anywhere.
func TestNearestBridgesTheGap(t *testing.T) {
	syn := map[string][4]float64{
		"女儿国":  {1, 0, 0, 0},
		"西梁女界": {1, 0, 0, 0.01},
	}
	tbl := &Table{
		Entries: []Entry{{Term: "西梁女界", DF: 4, Embed: []float64{1, 0, 0, 0.01}}},
		embedFn: fakeEmbed(syn),
	}
	got := tbl.Nearest("女儿国", 3)
	if len(got) == 0 || got[0] != "西梁女界" {
		t.Fatalf("bridge must return the corpus term, got %v", got)
	}
	// With no embedder the bridge is inert (nil), never an error.
	bare := &Table{Entries: []Entry{{Term: "西梁女界"}}}
	if got := bare.Nearest("女儿国", 3); got != nil {
		t.Fatalf("no embedder must yield nil, got %v", got)
	}
}

// TestStoreRoundtripCached pins persistence + the process cache: save,
// load (fresh store handle), terms and vectors survive; the second load
// comes from cache with a freshly stamped embedder.
func TestStoreRoundtripCached(t *testing.T) {
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tbl := &Table{
		Entries: []Entry{{Term: "西梁女界", DF: 4, Freq: 9, Embed: []float64{1, 0, 0, 0.01}}},
		Model:   "test", Dims: 4,
	}
	if err := NewStore(c, "law").Save(ctx, tbl); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewStore(c, "law").Load(ctx, fakeEmbed(nil))
	if err != nil || loaded == nil {
		t.Fatalf("load = %v err=%v", loaded, err)
	}
	if len(loaded.Entries) != 1 || loaded.Entries[0].Term != "西梁女界" {
		t.Fatalf("roundtrip lost the term: %+v", loaded.Entries)
	}
	if len(loaded.Entries[0].Embed) != 4 {
		t.Fatalf("roundtrip lost the vector: %+v", loaded.Entries[0])
	}
	// Empty store: nil table, not an error.
	c2, _ := cumulite.Open("", cumulite.WithInMemory())
	if lt, err := NewStore(c2, "law").Load(ctx, nil); err != nil || lt != nil {
		t.Fatalf("empty store must be nil/nil, got %v %v", lt, err)
	}
}

// TestMineRealCorpusShape is a smoke on the poetry corpus shape: mining a
// same-title corpus yields title terms (the self-describing part).
func TestMineRealCorpusShape(t *testing.T) {
	var docs []string
	for i := 0; i < 5; i++ {
		docs = append(docs, "《值雨》 作者：甲。全文："+strings.Repeat("慘慘雲頭暗，繩繩雨腳垂。", 3))
	}
	entries := Mine(docs, 50, 3)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Term, "雨") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("a rain-heavy corpus must yield rain vocabulary: %+v", entries[:min(5, len(entries))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

package fast

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// stubVocab answers with fixed terms — the bridge contract without a model.
type stubVocab struct {
	terms []string
}

func (s stubVocab) Nearest(string, int) []string { return s.terms }

// TestVocabBridgeFillsTheLexicalGap is the cascade contract: a query whose
// words miss the whole corpus, with primary+fallback both ranking zero,
// gets retried with the CORPUS's own terms before any LLM expander runs —
// and without a bridge the same query finds nothing.
func TestVocabBridgeFillsTheLexicalGap(t *testing.T) {
	// The corpus speaks 西梁女界; the query says 女儿国 — zero lexical overlap.
	corpus := []source.Source{
		source.New("doc1", "md", "file://doc1", "doc1", "zh",
			"西梁女界尽是女子。国主设宴款待取经人。", nil),
		source.New("doc2", "md", "file://doc2", "doc2", "zh",
			"行至西梁女界，欲倒换关文。", nil),
	}
	mk := func(vocab interface {
		Nearest(string, int) []string
	}) *Engine {
		e := New(mcs.KeywordScorer{Keywords: []string{"西梁女界"}})
		e.Vocab = vocab
		return e
	}
	// Rule analyzer keeps this hermetic: primary=女儿国, fallback empty.
	e := mk(stubVocab{terms: []string{"西梁女界"}})
	ans, err := e.Search(context.Background(), "女儿国在哪", corpus)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Skipped || len(ans.Samples) == 0 {
		t.Fatalf("with the bridge the lexical gap must close: skipped=%v samples=%d", ans.Skipped, len(ans.Samples))
	}
	// Control: no bridge, same query — nothing found.
	e2 := mk(nil)
	ans2, err := e2.Search(context.Background(), "女儿国在哪", corpus)
	if err != nil {
		t.Fatal(err)
	}
	if !ans2.Skipped || len(ans2.Samples) != 0 {
		t.Fatalf("without the bridge the gap must stay open: skipped=%v samples=%d", ans2.Skipped, len(ans2.Samples))
	}
}

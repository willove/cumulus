package ingest

import (
	"context"
	"testing"
	"unicode/utf8"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/source"
)

func memStore(t *testing.T) *Store {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	for _, coll := range []string{"clus_sources", "clus_evidence", "clus_clusters"} {
		if err := engine.EnsureCollection(context.Background(), coll); err != nil {
			t.Fatal(err)
		}
	}
	return New(engine, "clus_sources", "clus_evidence", "clus_clusters", "alpha")
}

func queryRange(lo, hi string, skip, limit int) contract.Query {
	return contract.Query{
		Filter: map[string]any{
			"business_key": map[string]any{"$gte": lo, "$lt": hi},
		},
		Skip:  skip,
		Limit: limit,
	}
}

func blockCfg() BlockConfig {
	return BlockConfig{Runes: 1_000, Overlap: 100, MinRunes: 2_000}
}

func longDoc(n int, marker string) string {
	r := make([]rune, n)
	for i := range r {
		r[i] = '填'
	}
	at := n / 2
	m := []rune(marker)
	copy(r[at:at+len(m)], m)
	return string(r)
}

func activeBlockKeys(t *testing.T, s *Store, parent string) []string {
	t.Helper()
	lo, hi := source.BlockKeyRange(parent)
	var out []string
	for skip := 0; ; skip += 200 {
		res, err := s.c.Query(context.Background(), s.sources, queryRange(lo, hi, skip, 200))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Documents {
			b, err := fromDoc(d)
			if err != nil {
				t.Fatal(err)
			}
			if b.Status == source.StatusActive {
				out = append(out, b.BusinessKey)
			}
		}
		if len(res.Documents) < 200 {
			break
		}
	}
	return out
}

func TestBlockConfigDefaults(t *testing.T) {
	// On by default, but a no-op below MinRunes (16,000 runes by default), so
	// short corpora are untouched: 0/40 of the cn-law anchors and 60/1,548
	// (3.9%) of the chinalaw articles are affected, and those 60 are exactly
	// the documents whose retrieval was previously reading 0.031% of itself.
	t.Setenv("CLUS_INGEST_BLOCKS", "")
	t.Setenv("CLUS_INGEST_BLOCK_MIN", "")
	t.Setenv("CLUS_INGEST_BLOCK_OVERLAP", "")
	cfg := EnvBlockConfig()
	if !cfg.enabled() {
		t.Fatal("block splitting is the default long-document path")
	}
	if cfg.resolved().MinRunes <= source.DefaultBlockRunes {
		t.Fatalf("MinRunes (%d) must exceed the block size (%d) so short documents stay whole",
			cfg.resolved().MinRunes, source.DefaultBlockRunes)
	}
	// A document at or under MinRunes must take the whole-store path, which is
	// what makes default-on safe.
	small := BlockConfig{Runes: 8_000, Overlap: 1_000, MinRunes: 16_000}
	whole, err := memStore(t).PutBlock(context.Background(),
		source.New("t", "md", "", "k", "zh", longDoc(5_000, "x"), nil), small)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Blocks != 1 {
		t.Fatalf("a body under MinRunes must stay whole, got %d blocks", whole.Blocks)
	}

	// The explicit opt-out must still work.
	t.Setenv("CLUS_INGEST_BLOCKS", "0")
	if cfg := EnvBlockConfig(); cfg.enabled() {
		t.Fatal("CLUS_INGEST_BLOCKS=0 must disable splitting")
	}

	t.Setenv("CLUS_INGEST_BLOCKS", "8000")
	cfg = EnvBlockConfig().resolved()
	if !cfg.enabled() || cfg.Runes != 8_000 {
		t.Fatalf("explicit size = %+v, want enabled at 8000", cfg)
	}
	if cfg.Overlap >= cfg.Runes {
		t.Fatalf("resolved overlap %d must be < size %d", cfg.Overlap, cfg.Runes)
	}
	// An overlap at or over the size would never advance; it must degrade.
	t.Setenv("CLUS_INGEST_BLOCK_OVERLAP", "9999")
	if got := EnvBlockConfig().resolved(); got.Overlap >= got.Runes {
		t.Fatalf("overlap >= size survived resolution: %+v", got)
	}
}

func TestPutBlockDisabledIsPut(t *testing.T) {
	s := memStore(t)
	body := longDoc(50_000, "【ANSWER】在中间")
	src := source.New("长文", "md", "file:///a.txt", "book.txt", "zh", body, nil)
	res, err := s.PutBlock(context.Background(), src, BlockConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks != 1 {
		t.Fatalf("disabled config must store the document whole, got %d blocks", res.Blocks)
	}
	// The whole body must be there, not a slice of it.
	got, err := s.getSource(context.Background(), res.IDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body {
		t.Fatalf("disabled path altered the body: %d vs %d bytes", len(got.Body), len(body))
	}
}

func TestPutBlockSmallBodyStaysWhole(t *testing.T) {
	s := memStore(t)
	body := longDoc(1_500, "短")
	src := source.New("短文", "md", "", "small.txt", "zh", body, nil)
	res, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks != 1 || res.Retired != 0 {
		t.Fatalf("a body under MinRunes must stay whole: %+v", res)
	}
	if len(activeBlockKeys(t, s, "small.txt")) != 0 {
		t.Fatal("a whole body must not leave blocks behind")
	}
}

func TestPutBlockKeylessBodyIsNotAnError(t *testing.T) {
	s := memStore(t)
	body := longDoc(50_000, "无键")
	src := source.New("无键", "md", "file:///x.txt", "", "zh", body, nil)
	// Splitting needs a stable parent identity. Refusing the FEATURE must not
	// fail an ingest that was legal a moment ago.
	if _, err := s.PutBlock(context.Background(), src, blockCfg()); err != nil {
		t.Fatalf("a keyless body must still ingest: %v", err)
	}
}

func TestPutBlockSplitsAndTilers(t *testing.T) {
	s := memStore(t)
	const n = 20_000
	body := longDoc(n, "【ANSWER】")
	src := source.New("书", "md", "file:///b.txt", "b.txt", "zh", body, nil)
	res, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks < 20 {
		t.Fatalf("expected ~%d blocks, got %d", n/1000+1, res.Blocks)
	}
	if res.Written != res.Blocks {
		t.Fatalf("wrote %d of %d blocks", res.Written, res.Blocks)
	}
	keys := activeBlockKeys(t, s, "b.txt")
	if len(keys) != res.Blocks {
		t.Fatalf("store holds %d live blocks, result reported %d", len(keys), res.Blocks)
	}
	// Every block must be at or under the configured size, and the parent
	// linkage must survive to the stored document.
	lo, hi := source.BlockKeyRange("b.txt")
	res2, err := s.c.Query(context.Background(), s.sources, queryRange(lo, hi, 0, 500))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res2.Documents {
		b, err := fromDoc(d)
		if err != nil {
			t.Fatal(err)
		}
		if l := utf8.RuneCountInString(b.Body); l > 1_000 {
			t.Fatalf("block %s holds %d runes, over the configured 1000", b.BusinessKey, l)
		}
		if !source.IsBlock(b.Meta) {
			t.Fatalf("block %s lost its block metadata", b.BusinessKey)
		}
		if parent, _, _, ok := source.BlockParent(b.Meta); !ok || parent != "b.txt" {
			t.Fatalf("block %s cannot name its parent: %q ok=%v", b.BusinessKey, parent, ok)
		}
	}
}

// The retirement obligation: a shorter re-ingest must not leave the previous
// edition's tail blocks live and serving stale text.
func TestPutBlockRetiresOrphansOnShorterEdition(t *testing.T) {
	s := memStore(t)
	first := longDoc(20_000, "【ANSWER】第一版")
	src := source.New("书", "md", "", "b.txt", "zh", first, nil)
	res1, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}

	// A second edition that is materially shorter.
	second := longDoc(5_000, "第二版")
	src2 := source.New("书", "md", "", "b.txt", "zh", second, nil)
	res2, err := s.PutBlock(context.Background(), src2, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Blocks >= res1.Blocks {
		t.Fatalf("the shorter edition produced %d blocks, expected fewer than %d", res2.Blocks, res1.Blocks)
	}
	if res2.Retired != res1.Blocks-res2.Blocks {
		t.Fatalf("retired %d orphans, expected %d", res2.Retired, res1.Blocks-res2.Blocks)
	}
	live := activeBlockKeys(t, s, "b.txt")
	if len(live) != res2.Blocks {
		t.Fatalf("%d live blocks after the shorter re-ingest, want %d — stale edition is still being served",
			len(live), res2.Blocks)
	}
}

// Re-ingesting the SAME document must converge to "unchanged" and retire
// nothing: idempotence is what the whole revision contract rests on.
func TestPutBlockIdempotent(t *testing.T) {
	s := memStore(t)
	body := longDoc(20_000, "【ANSWER】")
	src := source.New("书", "md", "", "b.txt", "zh", body, nil)
	first, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 {
		t.Fatalf("a byte-identical re-ingest wrote %d blocks, want 0", second.Written)
	}
	if second.Unchanged != first.Blocks {
		t.Fatalf("unchanged %d, want %d", second.Unchanged, first.Blocks)
	}
	if second.Retired != 0 {
		t.Fatalf("an identical re-ingest retired %d blocks", second.Retired)
	}
}

// A block fed back through the same path must not split again.
func TestPutBlockDoesNotResplitBlocks(t *testing.T) {
	s := memStore(t)
	body := longDoc(20_000, "【ANSWER】")
	src := source.New("书", "md", "", "b.txt", "zh", body, nil)
	res, err := s.PutBlock(context.Background(), src, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	// Take a stored block and re-put it through the same entry point.
	lo, hi := source.BlockKeyRange("b.txt")
	q, err := s.c.Query(context.Background(), s.sources, queryRange(lo, hi, 0, 1))
	_ = q
	if err != nil {
		t.Fatal(err)
	}
	b, err := fromDoc(q.Documents[0])
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.PutBlock(context.Background(), *b, blockCfg())
	if err != nil {
		t.Fatal(err)
	}
	if again.Blocks != 1 {
		t.Fatalf("a block re-ingested as %d blocks — the split recursed", again.Blocks)
	}
	if got := len(activeBlockKeys(t, s, "b.txt")); got != res.Blocks {
		t.Fatalf("block count moved from %d to %d", res.Blocks, got)
	}
}

// Two different parents must not share a key space: a range query for one
// parent's orphans must not reach another's blocks.
func TestBlockKeySpaceIsolated(t *testing.T) {
	s := memStore(t)
	for _, k := range []string{"a.txt", "b.txt", "dir/c.txt", "a.txt#x"} {
		body := longDoc(20_000, "【ANSWER】"+k)
		if _, err := s.PutBlock(context.Background(),
			source.New(k, "md", "", k, "zh", body, nil), blockCfg()); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"a.txt", "b.txt", "dir/c.txt", "a.txt#x"} {
		lo, hi := source.BlockKeyRange(k)
		keys := activeBlockKeys(t, s, k)
		if len(keys) == 0 {
			t.Fatalf("parent %q has no live blocks", k)
		}
		for _, bk := range keys {
			if bk < lo || bk > hi {
				t.Fatalf("parent %q range leaked key %q", k, bk)
			}
		}
	}
}

package source

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The splitter's whole safety story is that a block TILES the body. If it does
// not, a character in the gap is unreachable forever — invisible to every
// downstream metric, because nothing can cite what it cannot read. That is the
// same failure shape as the admission tail-append, so it gets its own gate.

func blockText(r []rune, b BlockOf) string { return string(r[b.Start:b.End]) }

func TestSplitBlocksTiles(t *testing.T) {
	const n = 100_000
	r := make([]rune, n)
	for i := range r {
		r[i] = rune('甲' + i%20)
	}
	body := string(r)

	for _, tc := range []struct{ size, overlap int }{
		{8_000, 1_000}, {8_000, 0}, {1, 0}, {999, 1}, {n - 1, 10}, {n, 100}, {n + 1, 100},
	} {
		blocks := SplitBlocks(body, tc.size, tc.overlap)
		if len(blocks) == 0 {
			t.Fatalf("size=%d overlap=%d produced no blocks", tc.size, tc.overlap)
		}
		if blocks[0].Start != 0 {
			t.Fatalf("size=%d: first block starts at %d, want 0", tc.size, blocks[0].Start)
		}
		if got := blocks[len(blocks)-1].End; got != n {
			t.Fatalf("size=%d: last block ends at %d, want %d (gap at the tail)", tc.size, got, n)
		}
		for i, b := range blocks {
			if b.End <= b.Start {
				t.Fatalf("size=%d block %d is empty: %+v", tc.size, i, b)
			}
			if b.End-b.Start > tc.size && b.End != n {
				t.Fatalf("size=%d block %d is %d runes, over the cap: %+v",
					tc.size, i, b.End-b.Start, b)
			}
			if b.Index != i {
				t.Fatalf("size=%d block %d has Index %d", tc.size, i, b.Index)
			}
			if i > 0 && b.Start <= blocks[i-1].Start {
				t.Fatalf("size=%d block %d starts at %d, not after its predecessor's %d",
					tc.size, i, b.Start, blocks[i-1].Start)
			}
		}
		// Contiguity: no gap between consecutive blocks unless an overlap was
		// requested. A gap means unreachable text.
		for i := 1; i < len(blocks); i++ {
			prev, cur := blocks[i-1], blocks[i]
			if cur.Start > prev.End {
				t.Fatalf("size=%d gap between block %d (end %d) and %d (start %d)",
					tc.size, i-1, prev.End, i, cur.Start)
			}
			if tc.overlap > 0 && cur.End != n && prev.End-cur.Start < tc.overlap {
				t.Fatalf("size=%d block %d overlaps its predecessor by %d, want >= %d",
					tc.size, i, prev.End-cur.Start, tc.overlap)
			}
		}
	}
}

// Every rune of the body must be reachable from at least one block — the
// direct statement of "no unreachable text".
func TestSplitBlocksEveryRuneReachable(t *testing.T) {
	const n = 37_777
	r := make([]rune, n)
	for i := range r {
		r[i] = rune('a' + i%26)
	}
	body := string(r)
	covered := make([]bool, n)
	for _, b := range SplitBlocks(body, 8_000, 1_000) {
		for i := b.Start; i < b.End; i++ {
			covered[i] = true
		}
	}
	for i, ok := range covered {
		if !ok {
			t.Fatalf("rune %d is in no block", i)
		}
	}
}

// A body at or under the cap is one block, not a degenerate multi-block split —
// this is the path the statute corpus already takes and must not change.
func TestSplitBlocksSmallBodyIsOneBlock(t *testing.T) {
	for _, n := range []int{1, 7, 8_000, 8_001} {
		body := strings.Repeat("字", n)
		blocks := SplitBlocks(body, DefaultBlockRunes, DefaultBlockOverlap)
		want := 1
		if n > DefaultBlockRunes {
			want = 2
		}
		if len(blocks) != want {
			t.Fatalf("n=%d: %d blocks, want %d", n, len(blocks), want)
		}
		if blocks[0].Start != 0 || blocks[len(blocks)-1].End != n {
			t.Fatalf("n=%d: blocks do not tile: %+v", n, blocks)
		}
	}
}

func TestSplitBlocksDegenerateInputs(t *testing.T) {
	if got := SplitBlocks("", 8_000, 1_000); got != nil {
		t.Fatalf("empty body must yield no blocks, got %+v", got)
	}
	if got := SplitBlocks("内容", 0, 1_000); got != nil {
		t.Fatalf("size<=0 must yield no blocks, got %+v", got)
	}
	if got := SplitBlocks("内容", -5, 1_000); got != nil {
		t.Fatalf("negative size must yield no blocks, got %+v", got)
	}
	// An overlap that is not smaller than the size would never advance; it
	// must degrade rather than loop.
	body := strings.Repeat("字", 20_000)
	blocks := SplitBlocks(body, 100, 100)
	if len(blocks) == 0 {
		t.Fatal("overlap == size must still produce blocks")
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i].Start <= blocks[i-1].Start {
			t.Fatalf("overlap == size did not advance at block %d: %+v", i, blocks)
		}
	}
}

// A fact straddling a boundary must survive in at least one block whole. This
// is the reason overlap exists, and without it the failure is silent.
func TestSplitBlocksBoundaryFactSurvives(t *testing.T) {
	const size, overlap = 100, 20
	r := make([]rune, 500)
	for i := range r {
		r[i] = '·'
	}
	// A 30-rune fact centred on the 200 boundary — every block boundary.
	fact := []rune("这是一条跨越边界的事实陈述必须完整可见")
	copy(r[200-len(fact)/2:], fact)
	body := string(r)
	blocks := SplitBlocks(body, size, overlap)
	whole := 0
	for _, b := range blocks {
		if strings.Contains(blockText(r, b), string(fact)) {
			whole++
		}
	}
	if whole == 0 {
		t.Fatal("a fact straddling a block boundary exists whole in no block")
	}
}

func TestBlockParentRoundTrip(t *testing.T) {
	body := strings.Repeat("字", 20_000)
	blocks := SplitBlocks(body, DefaultBlockRunes, DefaultBlockOverlap)
	if len(blocks) < 2 {
		t.Fatalf("need a multi-block body, got %d", len(blocks))
	}
	for _, b := range blocks {
		meta := BlockMeta("/corpus/book-01.txt", b, utf8.RuneCountInString(body))
		if !IsBlock(meta) {
			t.Fatalf("block %d: meta not recognised as a block", b.Index)
		}
		parent, start, end, ok := BlockParent(meta)
		if !ok {
			t.Fatalf("block %d: parent not recoverable", b.Index)
		}
		if parent != "/corpus/book-01.txt" || start != b.Start || end != b.End {
			t.Fatalf("block %d: parent round-trip = (%q,%d,%d), want (%q,%d,%d)",
				b.Index, parent, start, end, "/corpus/book-01.txt", b.Start, b.End)
		}
		// The key must be unique per block, or two blocks collide in the store.
		if got := BlockParentKey("/corpus/book-01.txt", b); got == BlockParentKey("/corpus/book-01.txt", BlockOf{Index: b.Index + 1}) {
			t.Fatalf("block %d: parent key collides with its neighbour", b.Index)
		}
	}
	// Identity must survive a metadata map that lost the non-key fields.
	partial := map[string]any{"block_scheme": "fixed-rune-overlap-v1"}
	if !IsBlock(partial) {
		t.Fatal("a block must still be identifiable after losing its offset fields")
	}
	if _, _, _, ok := BlockParent(partial); ok {
		t.Fatal("without a parent key the parent must report not-ok, not a bogus one")
	}
	if IsBlock(nil) || IsBlock(map[string]any{"block_scheme": "something-else"}) {
		t.Fatal("non-block metadata must not be mistaken for a block")
	}
}

func TestBlockTextIsRuneExact(t *testing.T) {
	// Multi-byte body: a byte-offset implementation would slice mid-rune here.
	body := strings.Repeat("漢字テスト", 5_000)
	r := []rune(body)
	for _, b := range SplitBlocks(body, 8_000, 1_000) {
		got := BlockText(body, b)
		if utf8.RuneCountInString(got) != b.End-b.Start {
			t.Fatalf("block %d: %d runes, want %d", b.Index, utf8.RuneCountInString(got), b.End-b.Start)
		}
		// Compare against the RUNE slice: b.Start is a rune offset, so
		// body[b.Start:] (a byte slice) would be the wrong reference and would
		// hide a real mid-rune bug behind a lucky prefix match.
		if string(r[b.Start:b.End]) != got {
			t.Fatalf("block %d: text is not the body's own rune slice at %d", b.Index, b.Start)
		}
	}
	// Out-of-range blocks return empty rather than panicking — a bad offset must
	// not take the ingest down.
	if got := BlockText(body, BlockOf{Start: 10, End: 5}); got != "" {
		t.Fatalf("inverted range must return empty, got %q", got)
	}
	if got := BlockText(body, BlockOf{Start: 0, End: len(r) + 100}); got != "" {
		t.Fatalf("out-of-range end must return empty, got %q", got)
	}
}

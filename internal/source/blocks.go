package source

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Block defaults. The sizes are NOT tuned to any document genre — they are
// chosen so a block lands inside the range where this suite is already
// verified to work, and the reasoning is recorded here because a bare constant
// would invite exactly the kind of genre-fitting this package must not do.
//
// The measured constraint chain (perf-plan §5):
//
//	mcs.SmallFileRunes = 100_000   — a body at or under this is handed to the
//	                                 synthesizer WHOLE, with no sampling, so
//	                                 nothing can be missed inside it.
//	Window half-width  =       240 — one scoring window is ~480 runes.
//	Observed real corpora: Chinese statute articles ≈10_000 runes/篇; novel
//	                      chapters p50 2_987 / mean 7_987; encyclopedia
//	                      entries ~10_000.
//
// So DefaultBlockRunes (8_000) sits an order of magnitude BELOW the whole-body
// threshold: a block is always read whole, always in one scoring call, and
// never depends on Monte-Carlo sampling to find the evidence inside it. That
// is the property that makes blocks safe to treat as units — it is the same
// property the statute corpus has for free.
const (
	DefaultBlockRunes   = 8_000
	DefaultBlockOverlap = 1_000

	// blockIndexDigits fixes the width of the per-block suffix, which is what
	// makes BlockKeyRange a bounded range rather than an open-ended prefix.
	blockIndexDigits = 6
)

// BlockOf is one contiguous slice of a parent document, addressed in RUNE
// indices (the same unit mcs sample windows use, so a block and a window share
// one coordinate system).
//
// Rune rather than byte offsets is not a preference: mcs.SampleBody converts
// the body with []rune and indexes that slice, and citation resolution does
// string(body[Start:End]) == Content. A byte-offset block would silently break
// both.
type BlockOf struct {
	Index int // 0-based position in the block sequence
	Start int // rune offset in the parent body, inclusive
	End   int // rune offset in the parent body, exclusive
}

// SplitBlocks divides body into fixed-size, overlapping blocks.
//
// Structure-agnostic BY CONSTRUCTION: it looks only at length, never at
// headings, chapters, line patterns or any other content convention. That is
// deliberate — a splitter that recognises 第N章 solves novels and nothing else,
// and the next format (logs, exports, code, scanned corpora) re-opens the whole
// question. Documents that carry their own hierarchy should keep it (see
// BuildStructure); this is the fallback for the ones that do not.
//
// Invariants, each pinned in blocks_test.go:
//   - blocks[0].Start == 0 and blocks[last].End == runeCount: the blocks TILE
//     the body, so no character is unreachable
//   - every block is at most size runes
//   - consecutive blocks overlap by at least the requested amount, so a
//     sentence straddling a boundary is still whole somewhere
//   - a body at or under size yields exactly one block
//   - an empty body yields none
//
// Why overlap rather than a boundary-only split: with no overlap a fact
// spanning offset 7_990–8_010 exists in neither neighbour and is
// unreachable — the same class of silent miss as the tail-append defect in the
// admission ordering.
func SplitBlocks(body string, size, overlap int) []BlockOf {
	// Count runes, do NOT materialise them. The block offsets are what this
	// function returns; the caller slices the body itself. Converting a
	// 18.6M-rune novel to []rune would allocate ~74MB per call for nothing —
	// which is the whole class of document this exists to serve.
	n := utf8.RuneCountInString(body)
	if n == 0 || size <= 0 {
		return nil
	}
	if n <= size {
		return []BlockOf{{Index: 0, Start: 0, End: n}}
	}
	if overlap < 0 {
		overlap = 0
	}
	// A degenerate overlap >= size would never advance.
	if overlap >= size {
		overlap = size / 4
	}
	stride := size - overlap

	out := make([]BlockOf, 0, n/stride+1)
	for start := 0; start < n; start += stride {
		end := start + size
		if end >= n {
			end = n
		}
		out = append(out, BlockOf{Index: len(out), Start: start, End: end})
		if end == n {
			break
		}
	}
	// The final block is clamped to the end, which can leave it shorter than
	// `overlap` behind the previous start when the tail is small. That is
	// correct (it is the last block, nothing follows) and is why the tiling
	// invariant is stated on End, not on a uniform stride.
	return out
}

// BlockText returns the parent body slice a block covers.
func BlockText(body string, b BlockOf) string {
	r := []rune(body)
	if b.Start < 0 || b.End > len(r) || b.Start >= b.End {
		return ""
	}
	return string(r[b.Start:b.End])
}

// BlockParentKey is the business key a block is filed under.
//
// Shape: "blk/<escaped-parent>/<6-digit index>". Two properties the ingest
// path depends on, both pinned in blocks_test.go:
//
//   - The parent is ESCAPED (url.PathEscape), so it can contain slashes,
//     hashes, colons and CJK without breaking the key structure or the prefix.
//   - The index is fixed-width, so every key of one parent is bounded by
//     "blk/<esc>/000000" .. "blk/<esc>/999999". That is a clean range query
//     with the operators the storage contract actually offers — the lite
//     engine's Filter supports equality/$in/$gte/$gt/$lte/$lt and NO prefix
//     operator, so "all blocks of this parent" is expressed as
//     BlockKeyRange, not as a pattern match.
//
// The parent is also recoverable from Meta, so a citation never has to parse
// the key back.
func BlockParentKey(parentKey string, b BlockOf) string {
	return blockPrefix(parentKey) + fmt.Sprintf("%0*d", blockIndexDigits, b.Index)
}

func blockPrefix(parentKey string) string {
	return "blk/" + url.PathEscape(parentKey) + "/"
}

// BlockKeyRange returns the inclusive/exclusive business-key bounds covering
// every block of parentKey, for a range query with only $gte/$lt.
func BlockKeyRange(parentKey string) (lo, hi string) {
	p := blockPrefix(parentKey)
	return p, p + strings.Repeat("9", blockIndexDigits)
}

// BlockMeta is the parent linkage stored alongside a block source.
//
// The offsets are recorded so a citation can be mapped back to the parent
// document's coordinate system. They are metadata, NOT the identity: identity
// is the block's own content digest, so a block whose offset metadata is
// stripped still de-duplicates correctly.
func BlockMeta(parentKey string, b BlockOf, parentRunes int) map[string]any {
	return map[string]any{
		"block_of":     parentKey,
		"block_index":  b.Index,
		"block_start":  b.Start,
		"block_end":    b.End,
		"parent_runes": parentRunes,
		"block_scheme": "fixed-rune-overlap-v1",
	}
}

// IsBlock reports whether a source was produced by the block splitter, by
// looking for the scheme marker rather than by parsing keys — a metadata map
// that lost one field should still identify its blocks, not silently become an
// unrecognised document.
func IsBlock(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	s, _ := meta["block_scheme"].(string)
	return strings.HasPrefix(s, "fixed-rune-overlap-")
}

// BlockParent returns the parent key and the block's rune range within it.
func BlockParent(meta map[string]any) (parent string, start, end int, ok bool) {
	if !IsBlock(meta) {
		return "", 0, 0, false
	}
	parent, _ = meta["block_of"].(string)
	start = intFromMeta(meta["block_start"])
	end = intFromMeta(meta["block_end"])
	return parent, start, end, parent != ""
}

func intFromMeta(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64: // JSON round-trip
		return int(t)
	default:
		return 0
	}
}

// BlockIndex recovers a block's ordinal from its stored metadata. It is the
// retirement criterion: a parent with N blocks must have exactly indices
// 0..N-1 live, so anything at or past N is a tail the current edition no
// longer produces.
func BlockIndex(s Source) (int, bool) {
	if !IsBlock(s.Meta) {
		return 0, false
	}
	return intFromMeta(s.Meta["block_index"]), true
}

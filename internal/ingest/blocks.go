package ingest

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/source"
)

// BlockConfig controls the optional long-document split. Zero value = off.
type BlockConfig struct {
	// Runes is the per-block size in runes. <= 0 means the package default.
	Runes int
	// Overlap is how many runes consecutive blocks share.
	Overlap int
	// MinRunes is the body size at or above which splitting kicks in. A body
	// below it is stored whole, which is the path every existing corpus takes.
	MinRunes int
}

func (c BlockConfig) resolved() BlockConfig {
	if c.Runes <= 0 {
		c.Runes = source.DefaultBlockRunes
	}
	if c.Overlap < 0 {
		c.Overlap = 0
	}
	if c.Overlap >= c.Runes {
		c.Overlap = source.DefaultBlockOverlap
	}
	if c.MinRunes <= 0 {
		c.MinRunes = c.Runes * 2
	}
	return c
}

func (c BlockConfig) enabled() bool { return c.Runes != 0 }

// BlockResult is what a split write reports. It is deliberately NOT a
// source.Result: a parent document becomes N independent sources, and a caller
// that only sees one ID would think the write was atomic when it is not.
type BlockResult struct {
	Parent    string   `json:"parent"`
	Blocks    int      `json:"blocks"`
	Written   int      `json:"written"`
	Unchanged int      `json:"unchanged"`
	Retired   int      `json:"retired"` // orphans from a previous, longer edition
	IDs       []string `json:"ids,omitempty"`
	// Status mirrors the single-document result on the unsplit path
	// (created | updated | unchanged) and is empty when the body was split.
	Status string `json:"status,omitempty"`

	// now is the single timestamp every block revision and retirement shares.
	// Per-block time.Now() would make a split's revisions disagree with each
	// other by milliseconds, which makes "did this edition change" harder to
	// read in a report.
	now time.Time
}

// EnvBlockConfig reads the split switch. ON by default since 2026-09-28.
//
// Why default-on is low risk: MinRunes defaults to twice the block size
// (16,000 runes), so a document at or under that is stored WHOLE and this
// configuration is a literal no-op for it. Measured on the corpora in this
// repo:
//
//	40 cn-law anchors (p50 145 chars)   → 0/40 documents affected
//	1,548 chinalaw articles (p50 3,539) → 60/1,548 = 3.9% affected
//
// and the 60 are exactly the long ones (max 181,208 chars) whose retrieval was
// previously broken: a 15.5M-rune book read 0.031% of itself per query
// (perf-plan §5.1). Blocking turns that into whole-block reads.
//
//	CLUS_INGEST_BLOCKS=0        disable (store long documents whole)
//	CLUS_INGEST_BLOCKS=<runes>  enable with an explicit block size
//	CLUS_INGEST_BLOCK_MIN=<n>   body size that triggers the split
//	CLUS_INGEST_BLOCK_OVERLAP=n  overlap between blocks
func EnvBlockConfig() BlockConfig {
	v := strings.TrimSpace(os.Getenv("CLUS_INGEST_BLOCKS"))
	if v == "0" {
		return BlockConfig{} // explicit opt-out
	}
	cfg := BlockConfig{Runes: source.DefaultBlockRunes}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		cfg.Runes = n
	}
	if n, err := strconv.Atoi(os.Getenv("CLUS_INGEST_BLOCK_MIN")); err == nil && n > 0 {
		cfg.MinRunes = n
	}
	if n, err := strconv.Atoi(os.Getenv("CLUS_INGEST_BLOCK_OVERLAP")); err == nil && n >= 0 {
		cfg.Overlap = n
	}
	return cfg
}

// PutBlock expands src into blocks when it exceeds the threshold, writes each
// one, and retires the blocks of the same parent that the new write did not
// produce.
//
// The retirement is not optional bookkeeping. A parent keyed
// "corpus/book.txt" that shrinks from 2,656 blocks to 2,400 would otherwise
// leave blocks 2,401..2,656 live and serving the PREVIOUS edition's text —
// silently, with no revision to notice, which is the same shape as the
// unreclaimed-stale finding this store already guards against for whole
// documents.
func (s *Store) PutBlock(ctx context.Context, src source.Source, cfg BlockConfig) (BlockResult, error) {
	whole := func() (BlockResult, error) {
		r, err := s.put(ctx, src, 0)
		return BlockResult{
			Parent: src.BusinessKey, Blocks: 1, Status: r.Status,
			IDs:       []string{r.ID},
			Written:   boolToInt(r.Status == "created"),
			Unchanged: boolToInt(r.Status == "unchanged"),
		}, err
	}
	// enabled() BEFORE resolved(), never after. resolved() fills a zero Runes
	// with the package default, so checking afterwards would make the
	// opt-in switch a no-op: every ingest would split, unasked. The test
	// TestPutBlockDisabledIsPut exists to catch exactly that inversion.
	if !cfg.enabled() {
		return whole()
	}
	cfg = cfg.resolved()
	// A block never re-splits: SplitBlocks returns a single block for a body at
	// or under the size, and this guard makes that explicit rather than relying
	// on the size comparison alone.
	if source.IsBlock(src.Meta) {
		return whole()
	}
	if utf8.RuneCountInString(src.Body) < cfg.MinRunes {
		return whole()
	}

	parent := src.BusinessKey
	if parent == "" {
		// Blocks are addressed by parent; a keyless document has no stable
		// parent identity. Storing it whole keeps every ingest that was legal a
		// moment ago legal now.
		return whole()
	}

	blocks := source.SplitBlocks(src.Body, cfg.Runes, cfg.Overlap)
	res := BlockResult{Parent: parent, Blocks: len(blocks)}

	// Batch, not per-block put. The per-document path runs a paged revisions()
	// query for EVERY document, so splitting one 15.4M-rune book into 2,212
	// blocks turned a 1.2s ingest into one that overruns the 60s default
	// budget (measured: ~7.4 blocks/s). BatchIngester seeds its live-revision
	// index with ONE paged metadata-only scan and is then O(1) per document,
	// which is the same fix commit 73ad28c applied to ingest-jsonl.
	//
	// PutBatch's per-identity semantics are what blocks need: business keys
	// are unique per block, so the digest-unchanged skip, the version
	// arithmetic and the retire-previous-revision all apply unchanged.
	srcs := make([]source.Source, 0, len(blocks))
	for _, b := range blocks {
		bs := source.Source{
			Title:      blockTitle(src, b),
			SourceType: blockType(src),
			SourceURI:  src.SourceURI,
			// Identity is the block's own content: stripping Meta must not
			// change de-duplication, and the offset metadata is not identity.
			Body:        source.BlockText(src.Body, b),
			Lang:        src.Lang,
			BusinessKey: source.BlockParentKey(parent, b),
			Meta:        source.BlockMeta(parent, b, utf8.RuneCountInString(src.Body)),
		}
		for k, v := range src.Meta {
			if _, taken := bs.Meta[k]; !taken {
				bs.Meta[k] = v
			}
		}
		srcs = append(srcs, bs)
	}
	bi, err := s.NewBatchIngester(ctx)
	if err != nil {
		return res, err
	}
	stored, err := bi.PutBatch(ctx, srcs)
	if err != nil {
		return res, err
	}
	// PutBatch returns how many it actually stored; an identical re-ingest
	// skips every block, so Written must come from its return value rather
	// than from len(srcs) — otherwise the idempotence report always claims a
	// full rewrite.
	res.Written = stored
	res.Unchanged = len(srcs) - stored
	res.now = time.Now().UTC()

	retired, err := s.retireOrphanBlocks(ctx, parent, len(blocks), res.now)
	if err != nil {
		return res, err
	}
	res.Retired = len(retired)
	return res, nil
}

// retireOrphanBlocks marks stale every live block of parent whose INDEX is at
// or beyond the current block count — i.e. the tail a shorter edition no
// longer produces.
//
// Index-based rather than "not in the written set" on purpose: PutBatch does
// not return the ids it wrote (it returns a count), and a keep-set would have
// to reconstruct revision ids the batch already decided. Block index is a
// denser invariant: a parent with N blocks must have exactly indices 0..N-1
// live, so anything at or past N is an orphan by construction. That also makes
// the sweep O(N) page reads rather than a set diff over every id.
//
// The query is a bounded key range because the storage contract has no prefix
// operator.
func (s *Store) retireOrphanBlocks(ctx context.Context, parent string, blockCount int, now time.Time) ([]string, error) {
	lo, _ := source.BlockKeyRange(parent)

	const page = 500
	var retired []string
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.sources, contract.Query{
			Filter: map[string]any{
				"business_key": map[string]any{"$gte": lo},
			},
			Skip:  skip,
			Limit: page,
		})
		if err != nil {
			return retired, fmt.Errorf("ingest: querying blocks of %s: %w", parent, err)
		}
		if len(res.Documents) == 0 {
			break
		}
		for _, d := range res.Documents {
			b, err := fromDoc(d)
			if err != nil {
				return retired, err
			}
			if b.Status != source.StatusActive {
				continue
			}
			idx, ok := source.BlockIndex(*b)
			if !ok || idx < blockCount {
				continue
			}
			if _, err := s.c.PatchDocument(ctx, s.sources, b.ID, map[string]any{
				"$set": map[string]any{
					"status":     source.StatusStale,
					"updated_at": now.Format(time.RFC3339Nano),
				},
			}); err != nil {
				return retired, fmt.Errorf("ingest: retiring orphan block %s: %w", b.ID, err)
			}
			if _, err := s.invalidateEvidence(ctx, b.ID); err != nil {
				return retired, err
			}
			retired = append(retired, b.ID)
		}
		if len(res.Documents) < page {
			break
		}
	}
	return retired, nil
}

func blockTitle(src source.Source, b source.BlockOf) string {
	base := src.Title
	if base == "" {
		base = src.SourceURI
	}
	return fmt.Sprintf("%s [%d]", base, b.Index)
}

// blockType keeps the parent's source_type so downstream format handling (html
// extraction, docx) is unchanged; a block is the same format as its parent.
func blockType(src source.Source) string {
	if src.SourceType == "" {
		return "md"
	}
	return src.SourceType
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

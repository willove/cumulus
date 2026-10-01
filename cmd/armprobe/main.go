// Command armprobe answers the question the optimisation plan calls A-2:
// where does Ev.Rec actually come from?
//
// The archived end-to-end run reports Ev.Rec 51.7% for the full pipeline
// (MiniMax-M3.1-Flash-Preview, 60 questions). That number has no anchor: the
// repo has no retrieval-side baseline at all, so it cannot answer the only
// question that matters — "the full pipeline costs 6–15s per query; what does
// that buy over plain lexical retrieval?"
//
// Three arms, one scoring function:
//
//	arm1  BM25 as-is                              — the lexical floor
//	arm2  BM25 + deterministic gap repair        — what a pure-lexical fix
//	                                                of the vocabulary gap
//	                                                is worth (lower-bound
//	                                                stand-in for the LLM
//	                                                rewriter, NOT a
//	                                                reproduction of it)
//	arm3  the full pipeline                      — read from a prior run,
//	                                                never re-measured here
//
// Why arm2 exists at all, and why it is the arm that matters: the vocabulary
// gap (帮信罪 → 帮助信息网络犯罪活动罪, zero bigram overlap) is the one place
// this system is knowingly "missing semantics" — the same disease the
// grep-only camp is criticised for. The production answer is an LLM rewrite
// (index.RewriteWhenEmpty). What that rewrite is WORTH has never been
// measured. arm2 is the cheapest honest approximation available without
// spending tokens: when the gap triggers, drop the terms the corpus has never
// seen and re-rank on the rest.
//
// Interpretation (see docs/optimization-plan.md §1 for the full table):
//
//	arm2 ≈ arm3  → the rewrite already closes the gap; the LLM scoring and
//	               synthesis stages are buying very little
//	arm2 ≈ arm1  → the gap is not repairable lexically; a real rewriter is
//	               the only fix, and the current one may be doing the work
//	arm1 ≈ 51.7% → the pipeline buys nothing over BM25
//
// Measurement discipline this probe keeps:
//   - arm1/arm2 make ZERO LLM calls and are deterministic, so they are
//     mechanism-grade and may be gated.
//   - arm3 is endpoint-grade (the archived `correct` metric flips 10/30 under
//     the judge and is self-declared unreliable), so it is report-only.
//     Arms 1/2 and arm 3 are NOT subtracted from each other without saying so:
//     different provenance, different grade.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/index"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

type corpusRow struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

func main() {
	var (
		itemsPath  = flag.String("items", "", "items JSONL (eval.Item: id/query/answer/gold_sources)")
		corpusPath = flag.String("corpus", "", "corpus JSONL (key/title/text)")
		arm3Path   = flag.String("arm3", "", "optional prior-run ItemScore JSONL to fold in as arm3")
		k          = flag.Int("k", 10, "retrieval depth for every arm")
		outPath    = flag.String("out", "", "optional JSON report path")
		verbose    = flag.Bool("v", false, "print per-item detail")
		idForm     = flag.String("id-form", "key", "corpus doc id form: key (src:<corpus key>, default) | revision (src:<key>#1) | bare (<key> with no prefix)")
	)
	flag.Parse()
	if *itemsPath == "" || *corpusPath == "" {
		fmt.Fprintln(os.Stderr, "armprobe: -items and -corpus are required")
		flag.Usage()
		os.Exit(2)
	}

	sources, err := loadCorpus(*corpusPath, *idForm)
	if err != nil {
		fatal(err)
	}
	items, err := loadItems(*itemsPath)
	if err != nil {
		fatal(err)
	}
	idx := index.Build(sources)

	report := map[string]any{
		"items":  len(items),
		"corpus": idx.N,
		"k":      *k,
	}
	fmt.Printf("armprobe  题=%d  文档=%d  k=%d  id-form=%s\n\n", len(items), idx.N, *k, *idForm)

	// ---- arm1: BM25 as-is -------------------------------------------------
	var a1, a2 []eval.ItemScore
	gapFired := 0
	var repaired []int
	for _, it := range items {
		a1 = append(a1, eval.Score(it, eval.Prediction{Query: it.Query, SourceIDs: idx.Rank(it.Query, *k)}))

		ids, fired := arm2IDs(idx, it.Query, *k)
		if fired {
			gapFired++
			repaired = append(repaired, len(ids))
		}
		a2 = append(a2, eval.Score(it, eval.Prediction{Query: it.Query, SourceIDs: ids}))
	}

	r1, r2 := eval.Aggregate(a1), eval.Aggregate(a2)
	report["arm1_bm25"] = r1
	report["arm2_gap_repair"] = r2
	report["gap_fired"] = gapFired
	report["gap_fire_rate"] = rate(gapFired, len(items))
	report["arm2_repaired_hits"] = repaired

	fmt.Printf("arm1  BM25 原样             Ev.Rec %5.1f%%   (%d/%d 题)\n",
		pct(r1.EvRec), evHits(a1), len(items))
	fmt.Printf("arm2  BM25 + 词汇鸿沟修复    Ev.Rec %5.1f%%   (%d/%d 题)  [触发 %d 题 (%.1f%%)]\n",
		pct(r2.EvRec), evHits(a2), len(items), gapFired, rate(gapFired, len(items)))
	fmt.Printf("      语义补丁净贡献        %+.1f pp\n", pct(r2.EvRec-r1.EvRec))

	if *arm3Path != "" {
		if a3, err := loadArm3(*arm3Path); err != nil {
			fmt.Fprintf(os.Stderr, "armprobe: arm3 skipped: %v\n", err)
		} else {
			r3 := eval.Aggregate(a3)
			report["arm3_pipeline"] = r3
			fmt.Printf("arm3  全管线（读入既往运行）   Ev.Rec %5.1f%%   (%d/%d 题)  [端点档·只记录不设门]\n",
				pct(r3.EvRec), evHitCount(a3), r3.N)
			fmt.Printf("      LLM 管线净贡献         %+.1f pp   (口径不同，慎作差)\n",
				pct(r3.EvRec-r2.EvRec))
		}
	}

	if *verbose {
		fmt.Println("\n--- 逐题 ---")
		for i, it := range items {
			mark := " "
			if a2[i].EvRec && !a1[i].EvRec {
				mark = "+" // arm2 修复救回来的
			} else if !a2[i].EvRec && a1[i].EvRec {
				mark = "-" // arm2 修复弄丢的
			}
			fmt.Printf("  %s %-14s a1=%v a2=%v  %s\n", mark, it.ID, a1[i].EvRec, a2[i].EvRec,
				trunc(it.Query, 44))
		}
	}

	if *outPath != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*outPath, b, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("\nreport → %s\n", *outPath)
	}
}

// arm2IDs is the gap-repair arm. It fires on the SAME two miss signatures the
// production rewriter uses (index.RewriteWhenEmpty), then repairs the query
// the only way a zero-LLM probe can: keep the terms the corpus actually
// contains, drop the ones it has never seen, and re-rank.
//
// This is a LOWER BOUND stand-in for the LLM rewrite, not a reproduction of
// it. The LLM can invent the corpus-side phrasing (帮信罪 → 帮助信息网络犯罪
// 活动罪); dropping the out-of-vocabulary term can only fall back to whatever
// the surviving bigrams match. Read a low arm2 as "a lexical repair cannot
// close this gap", which is evidence the LLM rewriter is doing real work.
func arm2IDs(idx *index.Index, query string, k int) ([]string, bool) {
	triggered := idx.TopBM25Score(query) < index.MinRewriteScore ||
		idx.VocabGapFraction(query) >= index.RewriteGapFraction
	if !triggered {
		return idx.Rank(query, k), false
	}
	terms := mcs.Fields(query)
	kept := make([]string, 0, len(terms))
	for _, t := range terms {
		if len(idx.Postings[t]) > 0 {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return idx.Rank(query, k), true // everything was OOV — nothing to repair
	}
	ids := idx.RankTerms(kept, k)
	if len(ids) == 0 {
		ids = idx.Rank(query, k)
	}
	return ids, true
}

func loadCorpus(path, idForm string) ([]source.Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open corpus: %w", err)
	}
	defer f.Close()
	var out []source.Source
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r corpusRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Text == "" {
			continue
		}
		s := source.New(r.Title, "md", "", r.Key, "zh", r.Text, nil)
		// The corpus file's own `key` IS the document identity: on the scale-eval
		// set it is the 12-hex digest the gold and the archived cites both use
		// verbatim, and on the golden set it is the L00X-AXXXXX business key.
		// Recomputing a digest from the body is WRONG here — source.New is
		// content-addressed at 16 chars, which canonicalizes to something no
		// gold matches, and silently scores 0%. eval.canonicalKeys strips the
		// "src:" prefix and any "#N" suffix, so either spelling lines up.
		if idForm == "bare" {
			s.ID = r.Key
		} else if idForm == "revision" {
			s.ID = source.RevisionID(r.Key, r.Title, s.Digest, 1)
		} else {
			s.ID = "src:" + r.Key
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

func loadItems(path string) ([]eval.Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read items: %w", err)
	}
	var out []eval.Item
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it eval.Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// loadArm3 reads a prior eval-run results file. Its shape is NOT the flat
// ItemScore one — eval-run nests the verdict under "eval" and keeps a sibling
// "closed_book" block, so a flat decode silently yields every row false:
//
//	{"id":..., "mode":..., "eval":{"ev_rec":...}, "closed_book":{...}}
func loadArm3(path string) ([]eval.ItemScore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read arm3: %w", err)
	}
	var out []eval.ItemScore
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			ID   string          `json:"id"`
			Eval json.RawMessage `json:"eval"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if len(row.Eval) == 0 {
			continue
		}
		var s eval.ItemScore
		if err := json.Unmarshal(row.Eval, &s); err != nil {
			continue
		}
		if s.ID == "" {
			s.ID = row.ID
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("arm3 file produced no ItemScore rows (expected an \"eval\" block per line)")
	}
	return out, nil
}

func evHits(sc []eval.ItemScore) int {
	n := 0
	for _, s := range sc {
		if s.EvRec {
			n++
		}
	}
	return n
}

func evHitCount(sc []eval.ItemScore) int { return evHits(sc) }

func rate(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func pct(v float64) float64 { return v * 100 }

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "armprobe:", err)
	os.Exit(1)
}

package charset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This probe exists because the repo carried two mutually exclusive readings of
// the SAME 1,251-file corpus and nothing could say which one is current:
//
//	perf-plan.md §5.3   UTF-8 68   GB18030 1088   refused     95   (heading: 98.2%)
//	charset.go doc      UTF-8 68   GB18030 1150   undecodable 33
//
// Both sum to 1,251, and the gap is exactly 62 files moving from "refused" to
// "GB18030" — which smells like the decoder got more permissive afterwards, but
// that is a hypothesis. Only the shipped classifier can settle it, so this runs
// Decode over the corpus instead of reimplementing a judgement that would then
// agree with itself by construction.
//
// Decode always sets Result.Tier before returning an error (both refusal paths
// assign TierUndecodable), so the tier tally ignores err on purpose: an error
// here IS a tier, not a missing measurement.
//
// Gated on CHARSET_CORPUS for the same reason as the index probes — the corpus
// is the operator's local download, not a fixture. Tag the printed numbers with
// the code shape they came from (baseline §十): run this at a recorded HEAD.
func TestCorpusTierCensus(t *testing.T) {
	root := os.Getenv("CHARSET_CORPUS")
	if root == "" {
		t.Skip("CHARSET_CORPUS not set")
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("CHARSET_CORPUS must be a directory: %v", err)
	}

	tiers := map[Tier]int{}
	bytesByTier := map[Tier]int64{}
	var totalFiles, txtFiles, txtLower, txtUpper, unreadable, refused int

	werr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Logf("walk err %s: %v", p, err)
			return nil
		}
		if fi.IsDir() {
			return nil
		}
		totalFiles++
		// Case-insensitive on purpose. This corpus is 1,220 `.txt` + 31 `.TXT`
		// = the 1,251 the two conflicting readings both claim: a `.txt` filter
		// silently drops 2.5% of the sample, and the dropped ones are the
		// Windows-era downloads — i.e. precisely the likely-GB18030 tail. Any
		// census that disagrees with a count of 1,251 should check this first.
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".txt" {
			return nil
		}
		txtFiles++
		if filepath.Ext(p) == ".TXT" {
			txtUpper++
		} else {
			txtLower++
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			unreadable++
			return nil
		}
		res, derr := Decode(raw)
		if derr != nil {
			refused++
		}
		if res.Tier == "" {
			t.Errorf("Decode returned no tier for %s (err=%v)", p, derr)
			return nil
		}
		tiers[res.Tier]++
		bytesByTier[res.Tier] += int64(res.SrcBytes)
		return nil
	})
	if werr != nil {
		t.Fatal(werr)
	}

	n := tiers[TierUTF8] + tiers[TierGB18030] + tiers[TierUndecodable]
	if n == 0 {
		t.Fatalf("classified nothing: %s has %d files, %d of them .txt", root, totalFiles, txtFiles)
	}
	pct := func(c int) float64 { return 100 * float64(c) / float64(n) }

	t.Logf("CENSUS root=%s", root)
	t.Logf("  files_seen=%d txt=%d (lower=%d upper=%d) unreadable=%d classified=%d Decode-errored=%d",
		totalFiles, txtFiles, txtLower, txtUpper, unreadable, n, refused)
	for _, tr := range []Tier{TierUTF8, TierGB18030, TierUndecodable} {
		t.Logf("  %-12s %5d  %5.1f%%  src=%6.1f MB", tr, tiers[tr], pct(tiers[tr]),
			float64(bytesByTier[tr])/(1<<20))
	}
	// perf-plan §5.3's heading wanted a "how much is NOT already UTF-8" figure.
	// Its own table gives this number, not 98.2%.
	nonUTF8 := tiers[TierGB18030] + tiers[TierUndecodable]
	t.Logf("  非 UTF-8 = %d/%d = %.1f%%  ← 标题该引这个数", nonUTF8, n, pct(nonUTF8))
}

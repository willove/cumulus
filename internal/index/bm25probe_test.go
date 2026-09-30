package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// One-off probe: measure raw BM25 behaviour on the real laws-full corpus for
// the queries the user actually asked. Skipped unless PROBE_CORPUS is set.
func TestBM25ProbeLawsFull(t *testing.T) {
	root := os.Getenv("PROBE_CORPUS")
	if root == "" {
		t.Skip("PROBE_CORPUS not set")
	}
	var srcs []source.Source
	werr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			t.Logf("walk err %s: %v", p, err)
			return nil
		}
		if fi.IsDir() || !strings.HasSuffix(p, ".txt") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		srcs = append(srcs, source.Source{ID: fi.Name(), Title: fi.Name(), Body: string(b), Status: source.StatusActive})
		return nil
	})
		t.Logf("walk root=%s err=%v docs=%d", root, werr, len(srcs))
	idx := Build(srcs)
	if t.Failed() {
		return
	}
	t.Logf("docs=%d avgLen=%.0f", idx.N, idx.AvgLen)
	for _, q := range []string{"帮信罪是什么", "帮助信息网络犯罪活动罪是什么", "闯红灯会有什么处罚"} {
		top := idx.TopBM25Score(q)
		t.Logf("q=%q topScore=%.3f gapFraction=%.3f", q, top, idx.VocabGapFraction(q))
		seen := map[string]bool{}
		for _, term := range mcs.Fields(q) {
			if seen[term] {
				continue
			}
			seen[term] = true
			t.Logf("    term=%s df=%d", term, len(idx.Postings[term]))
		}
		for i, id := range idx.Rank(q, 8) {
			t.Logf("  #%d %s", i+1, id)
		}
	}
}

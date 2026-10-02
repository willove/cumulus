package index

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/source"
)

// Two costs in the hot retrieval path have never been measured, and both are
// claimed in prose:
//
//   - index.Build runs once per QUERY, not per process: searchStack carries the
//     sync.Once and a stack is built per request (searchapi.go). Nobody has said
//     what that costs at 9,600 docs.
//   - Rerank embeds 50 candidate bodies + the query per query. open-decisions
//     §1.3 estimates ~5.75s from a 115ms/record BACKFILL figure — a different
//     operation — and admits the recall benefit was never measured either.
//
// This probe measures both on the real corpus so the two numbers can finally be
// compared to each other. Gated on COST_CORPUS (a jsonl of key/text/title) for
// the same reason as the other probes: the corpus is the operator's local data.
//
// Timings are machine-load dependent — record `uptime` and HEAD alongside any
// number taken from here (baseline §十). The Rerank arm additionally needs
// local MiniLM weights and skips without them.
func TestHotPathCosts(t *testing.T) {
	path := os.Getenv("COST_CORPUS")
	if path == "" {
		t.Skip("COST_CORPUS not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var list []source.Source
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<22), 1<<22)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad jsonl: %v", err)
		}
		key, _ := m["key"].(string)
		title, _ := m["title"].(string)
		text, _ := m["text"].(string)
		if text == "" {
			continue
		}
		list = append(list, source.New(title, "jsonl", "", key, "zh", text, m))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(list) < 100 {
		t.Fatalf("corpus too small to be the one under discussion: %d docs", len(list))
	}
	var runes int
	for _, s := range list {
		runes += len([]rune(s.Body))
	}
	// The arm width is part of what is being measured (Rerank embeds the whole
	// candidate set in one batch), so the query must actually fill it. A
	// off-domain query silently shrinks the arm and makes the per-candidate
	// arithmetic the only honest reading of the result.
	query := os.Getenv("COST_QUERY")
	if query == "" {
		query = "家庭暴力 人身安全保护令 申请条件"
	}
	t.Logf("CORPUS docs=%d total_runes=%d mean_runes=%d file=%s query=%q",
		len(list), runes, runes/len(list), path, query)

	// --- Build: once per query today ---------------------------------------
	const buildRuns = 7
	var buildTimes []time.Duration
	for i := 0; i < buildRuns; i++ {
		start := time.Now()
		idx := Build(list)
		buildTimes = append(buildTimes, time.Since(start))
		if idx == nil {
			t.Fatal("Build returned nil on a non-empty corpus")
		}
	}
	sort.Slice(buildTimes, func(i, j int) bool { return buildTimes[i] < buildTimes[j] })
	t.Logf("BUILD        n=%d  p50=%v  min=%v  max=%v",
		buildRuns, buildTimes[buildRuns/2], buildTimes[0], buildTimes[buildRuns-1])

	idx := Build(list)
	// Preconditions, printed not assumed. Without these a probe can happily time
	// an EMPTY index (Build skips non-active docs) and still emit plausible
	// millisecond figures — the failure mode this whole exercise is about.
	t.Logf("PRECONDITION Build: N=%d distinct_doc_lens=%d postings=%d avg_len=%.1f tokens",
		idx.N, len(idx.DocLens), len(idx.Postings), idx.AvgLen)
	if idx.N == 0 || len(idx.Postings) == 0 {
		t.Fatalf("index is EMPTY (N=%d postings=%d) — every timing below is meaningless", idx.N, len(idx.Postings))
	}
	var rankTimes []time.Duration
	for i := 0; i < buildRuns; i++ {
		start := time.Now()
		_ = idx.Rank(query, 50)
		rankTimes = append(rankTimes, time.Since(start))
	}
	sort.Slice(rankTimes, func(i, j int) bool { return rankTimes[i] < rankTimes[j] })
	t.Logf("RANK top-50  n=%d  p50=%v  min=%v  max=%v",
		buildRuns, rankTimes[buildRuns/2], rankTimes[0], rankTimes[buildRuns-1])

	// --- Rerank: the never-measured arm ------------------------------------
	if !minilm.Available() {
		t.Logf("RERANK       skipped: local MiniLM weights absent at %s", minilm.DefaultDir())
		t.Log("  ⇒ the 5.75s/query figure in open-decisions §1.3 stays unverified;")
		t.Log("    it was inferred from a BACKFILL rate, which is a different operation.")
		return
	}
	emb, rerr := minilm.Resolve()
	if rerr != nil {
		t.Fatalf("weights advertised as present but Resolve failed: %v", rerr)
	}
	// Rank returns doc IDs (see Narrow, which matches on s.ID) — NOT business
	// keys. Keying this map by BusinessKey silently yields zero candidates and
	// would then time an empty Rerank, which is why the guard below is a hard
	// failure rather than a skip.
	top := idx.Rank(query, 50)
	byID := map[string]source.Source{}
	for _, s := range list {
		if s.ID != "" {
			byID[s.ID] = s
		}
	}
	var cands []source.Source
	for _, id := range top {
		if s, ok := byID[id]; ok {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		t.Fatalf("Rerank arm got no candidates (Rank returned %d ids, byID has %d) — "+
			"the tally below would be meaningless", len(top), len(byID))
	}
	t.Logf("RERANK shape: Rank returned %d ids, resolved %d sources (production arm is 50)",
		len(top), len(cands))
	ctx := context.Background()
	const rerunN = 3
	var rerankTimes []time.Duration
	for i := 0; i < rerunN; i++ {
		start := time.Now()
		out := Rerank(ctx, query, cands, emb.Embed)
		rerankTimes = append(rerankTimes, time.Since(start))
		if len(out) != len(cands) {
			t.Fatalf("Rerank dropped candidates: %d → %d", len(cands), len(out))
		}
	}
	sort.Slice(rerankTimes, func(i, j int) bool { return rerankTimes[i] < rerankTimes[j] })
	t.Logf("RERANK       candidates=%d  n=%d  p50=%v  min=%v  max=%v",
		len(cands), rerunN, rerankTimes[rerunN/2], rerankTimes[0], rerankTimes[rerunN-1])
	t.Logf("VERDICT      build p50=%v vs rerank p50=%v  → rerank/build = %.1fx",
		buildTimes[buildRuns/2], rerankTimes[rerunN/2],
		float64(rerankTimes[rerunN/2])/float64(buildTimes[buildRuns/2]))
}

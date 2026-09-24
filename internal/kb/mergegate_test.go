package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// seedQuery/bandQuery are a matched pair: same intent, different wording, whose
// max-over-keys score lands INSIDE the cross-topic merge band
// [MergeTheta, ReuseTheta) rather than above the reuse line. The precondition
// asserts below keep the fixture from silently drifting out of the band it
// exists to exercise.
const (
	seedQuery = "连接池最大连接数是多少啊"
	bandQuery = "连接池上限是多大"
)

// e2engine builds a bare engine over an in-memory cluster store.
func e2engine(t *testing.T) *Engine {
	t.Helper()
	return New(fast.New(mcs.KeywordScorer{}), cluster.NewMemory(), cluster.Local{N: 64})
}

// foreignCluster is a cluster on a DIFFERENT topic key that is close enough to
// the incoming query to be offered as a merge candidate. claim is the numeric
// claim its evidence carries ("" = no claim, so AcceptFold cannot refuse on
// divergence).
func foreignCluster(t *testing.T, e *Engine, claim string) cluster.Cluster {
	t.Helper()
	c := cluster.Cluster{
		ID:        "Cforeign",
		TopicKey:  "foreigntopickey1",
		Name:      "连接池",
		Content:   "连接池的配置说明。",
		Queries:   []string{seedQuery},
		Embed:     mustEmbed(t, e, seedQuery),
		Lifecycle: cluster.LifecycleStable,
		Version:   1,
		Hotness:   0.5,
	}
	if claim != "" {
		c.Content = "连接池最大 " + claim + "。"
		c.Evidence = []mcs.Sample{{Start: 0, End: 8, Content: "连接池最大 " + claim, Score: 9}}
	}
	return c
}

// sameTopicCluster sits on the incoming query's OWN topic key but below the
// merge line, so pickMergeable skips it. It is what the old SplitCap fallback
// used to fold into after AcceptFold had refused the foreign target.
func sameTopicCluster(t *testing.T, e *Engine) cluster.Cluster {
	t.Helper()
	c := cluster.Cluster{
		ID:       "Csame",
		TopicKey: cluster.TopicKey(bandQuery),
		Name:     "同题簇",
		Content:  "同题簇自有内容。",
		// The retained wording has rotated out of the FIFO and the stored
		// embed predates an embedder change (Cosine returns 0 on a dim
		// mismatch), so this cluster is same-topic by IDENTITY but below the
		// merge line by SCORE — exactly the shape the SplitCap fallback acts on.
		Queries:   []string{"已被淘汰的旧问法"},
		Embed:     make([]float64, 32),
		Lifecycle: cluster.LifecycleStable,
		Version:   1,
		Hotness:   0.5,
	}
	if cluster.CanMerge(&c, bandQuery, mustEmbed(t, e, bandQuery), e.MergeTheta) {
		t.Fatal("fixture: the same-topic cluster must sit below merge_theta")
	}
	return c
}

func bandAnswer() fast.Answer {
	return fast.Answer{
		Query: bandQuery, Mode: fast.ModeFAST, Summary: "连接池最大 128。",
		SourceID: "src:d", Confidence: 0.9,
		Samples: []mcs.Sample{{Start: 0, End: 8, Content: "连接池最大 128", Score: 9}},
	}
}

// H6: the cross-topic merge band must be REACHABLE. SSOT D3 / §6.4 G-merge:
// "未过复用线但与既有簇 embed_sim >= merge_th 时 merge 进旧簇（追加
// evidence/query），不新建". crossTopicNear used to pre-filter candidates at
// ReuseTheta, which truncated the set to >=0.85 and made MergeTheta unreachable
// cross-topic — a paraphrase in the [merge_th, reuse_th) band fractured into a
// second cluster instead of merging.
func TestCrossTopicMergeBandIsReachable(t *testing.T) {
	ctx := context.Background()
	e := e2engine(t)
	if err := e.Store.Save(ctx, foreignCluster(t, e, "")); err != nil {
		t.Fatal(err)
	}
	qe, err := e.embed(ctx, bandQuery)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := e.Store.Get(ctx, "Cforeign")
	score := cluster.ReuseScore(seed, bandQuery, qe)
	if score >= e.ReuseTheta {
		t.Fatalf("fixture must land BELOW the reuse line (got %.3f, reusetheta=%.2f)", score, e.ReuseTheta)
	}
	if score < e.MergeTheta {
		t.Fatalf("fixture must land INSIDE the merge band (got %.3f, mergetheta=%.2f)", score, e.MergeTheta)
	}
	// The candidate must be OFFERED at the merge bar.
	if got := e.crossTopicNear(ctx, cluster.TopicKey(bandQuery), qe, bandQuery); len(got) != 1 {
		t.Fatalf("cross-topic candidate not offered at merge_theta: %+v", got)
	}
	// And a persisted answer must fold into it rather than fracture.
	res, err := e.Persist(ctx, bandAnswer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ClusterID != "Cforeign" {
		t.Fatalf("merge-band hit must fold into the existing cluster: %q", res.ClusterID)
	}
	if !res.Merged {
		t.Fatalf("merge-band hit must report merged: %+v", res)
	}
	all, _ := e.Store.All(ctx)
	if len(all) != 1 {
		t.Fatalf("no fracture allowed: %d clusters", len(all))
	}
}

// A genuinely different topic must NOT be dragged into the merge band.
func TestDistinctTopicsStaySeparate(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{}), st, cluster.Local{N: 64})
	srcs := []source.Source{
		source.New("路由器", "md", "file://r", "r", "zh", "路由器基本配置步骤", nil),
		source.New("防火墙", "md", "file://f", "f", "zh", "防火墙端口开放策略", nil),
	}
	r1, err := e.Ask(ctx, "路由器基本配置步骤", srcs)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.Ask(ctx, "防火墙端口开放策略", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ClusterID == r2.ClusterID {
		t.Fatalf("distinct topics must not merge: both %s", r1.ClusterID)
	}
}

// H5: when AcceptFold refuses a fold, the verdict must stand. The old code
// fell through to the SplitCap fallback, which evolved the very cluster that
// had just been refused and reported it as a successful merge — so the A2 gate
// could never block anything, and RejectedProposals counted a fold that in
// fact happened.
func TestRejectedFoldIsNotUndoneByFallback(t *testing.T) {
	ctx := context.Background()
	e := e2engine(t)
	// Foreign target carries 256; the incoming answer claims 128 -> AcceptFold
	// refuses the cross-topic fold (divergent claims).
	if err := e.Store.Save(ctx, foreignCluster(t, e, "256")); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Save(ctx, sameTopicCluster(t, e)); err != nil {
		t.Fatal(err)
	}
	before := e.RejectedProposals
	res, err := e.Persist(ctx, bandAnswer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.RejectedProposals <= before {
		t.Fatalf("precondition: the A2 gate must refuse this fold (rejected=%d)", e.RejectedProposals)
	}
	if res.Merged {
		t.Fatalf("a refused fold was reported as merged: %+v", res)
	}
	if res.FoldRejected == "" {
		t.Fatalf("the refusal reason was discarded: %+v", res)
	}
	// The refused content must not reach the surviving same-topic cluster.
	survivor, _ := e.Store.Get(ctx, "Csame")
	if survivor == nil {
		t.Fatal("same-topic cluster vanished")
	}
	if strings.Contains(survivor.Content, "128") {
		t.Fatalf("refused content was folded in anyway: %q", survivor.Content)
	}
	// The query-only half of the fallback still happened: the ask is recorded
	// on the surviving cluster (that is what keeps G-id from fracturing), but
	// it is reported as NOT merged because nothing was folded.
	recorded := false
	for _, q := range survivor.Queries {
		if q == bandQuery {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("the refused fold must still record the ask: %v", survivor.Queries)
	}
	// G-id: the topic did not fracture either.
	all, _ := e.Store.All(ctx)
	same := 0
	for _, c := range all {
		if c.TopicKey == cluster.TopicKey(bandQuery) {
			same++
		}
	}
	if same > e.SplitCap {
		t.Fatalf("topic fractured into %d clusters (split_cap=%d)", same, e.SplitCap)
	}
}

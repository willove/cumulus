// Package kb wires L2 cluster reuse into the search path: try reuse before
// sampling; on a fresh answer, create or merge a cluster (query-driven embed).
package kb

import (
	"context"
	"fmt"
	"time"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

// DefaultReuseTheta is the cosine line for reuse; DefaultMergeTheta is looser
// so near-paraphrases merge instead of fracturing.
const (
	DefaultReuseTheta = 0.85
	DefaultMergeTheta = 0.55
)

// CiteStore records cluster → source evidence windows (ask_cites).
type CiteStore interface {
	SaveCite(ctx context.Context, clusterID, sourceID string, start, end int, score float64) error
}

// Engine adds cluster reuse/evolve and graph expansion around FAST.
type Engine struct {
	Fast       *fast.Engine
	Store      cluster.Store
	Embedder   cluster.Embedder
	Edges      graph.Store // optional; nil = no expansion
	Cites      CiteStore   // optional; nil = no cite edges
	ReuseTheta float64
	MergeTheta float64
	SplitCap   int
	HopKNN     int
	// HopTS prunes expanded neighbors whose linked source is staler than this
	// (D4 optional freshness pass 时序剪枝; 0 = off).
	HopTS time.Duration
}

func New(f *fast.Engine, st cluster.Store, emb cluster.Embedder) *Engine {
	return &Engine{
		Fast:       f,
		Store:      st,
		Embedder:   emb,
		ReuseTheta: DefaultReuseTheta,
		MergeTheta: DefaultMergeTheta,
		SplitCap:   cluster.DefaultSplitCap,
	}
}

// Result wraps a FAST answer with cluster bookkeeping and graph expansion.
type Result struct {
	Answer      fast.Answer          `json:"answer"`
	Reused      bool                 `json:"reused"`
	ClusterID   string               `json:"cluster_id,omitempty"`
	ClusterVer  int                  `json:"cluster_version,omitempty"`
	Sampled     int                  `json:"sampled"` // 0 on reuse
	Persisted   bool                 `json:"persisted"`
	Merged      bool                 `json:"merged"`
	Neighbors   []graph.ExpandResult `json:"neighbors,omitempty"`
	PrevCluster string               `json:"prev_cluster,omitempty"`
}

// Ask runs reuse-or-search. Reuse path: 0 samples. Fresh path: FAST + save/merge.
func (e *Engine) Ask(ctx context.Context, query string, sources []source.Source) (Result, error) {
	if e.Store == nil || e.Embedder == nil {
		ans, err := e.Fast.Search(ctx, query, sources)
		return Result{Answer: ans, Sampled: len(ans.Samples)}, err
	}
	qe, err := e.embed(ctx, query)
	if err != nil {
		return Result{}, err
	}
	key := cluster.TopicKey(query)
	prevID := e.lastClusterID(ctx)

	finish := func(res Result) Result {
		if res.ClusterID != "" {
			if prevID != "" && prevID != res.ClusterID {
				_ = graph.LinkQuerySeq(ctx, e.edgeStore(), prevID, res.ClusterID)
			}
			res.PrevCluster = prevID
			res.Neighbors = e.expand(ctx, res.ClusterID, qe, sources)
		}
		return res
	}

	// Phase 0: reuse (same topic first, then any close cluster).
	same, err := e.Store.FindByTopic(ctx, key)
	if err != nil {
		return Result{}, err
	}
	if c := e.pickReusable(same, qe, query); c != nil {
		// B8 warm-prior validation (LENS): a prior is only warm while it
		// still matches the CURRENT corpus. Any evidence window that no
		// longer pins back exactly (source updated/gone) disqualifies the
		// prior — mark it 待复核 and fall through to L0, which self-heals
		// the cluster through the merge path below.
		if e.priorStale(ctx, c, sources) {
			c.Lifecycle = cluster.LifecycleEmerging
			_ = e.Store.Save(ctx, *c)
		} else {
			if c.Lifecycle == cluster.LifecycleEmerging {
				// B8: the prior just validated against the current corpus —
				// self-heal complete.
				c.Lifecycle = cluster.LifecycleStable
			}
			c.Evolve(query, e.querySetEmbed(ctx, *c))
			_ = e.Store.Save(ctx, *c)
			// Reuse still surfaces the stored evidence windows (0 new samples).
			samples := c.Evidence
			cov := mcs.Coverage(query, samples)
			return finish(Result{
				Answer: fast.Answer{
					Query: query, Mode: "FAST", Confidence: c.Confidence,
					Summary: c.Content, SourceID: c.SourceID, LLMCalls: 0,
					Samples:  samples,
					Coverage: cov,
				},
				Reused: true, ClusterID: c.ID, ClusterVer: c.Version,
				Sampled: 0, Persisted: true,
			}), nil
		}
	}

	// L0 path.
	ans, err := e.Fast.Search(ctx, query, sources)
	if err != nil {
		return Result{}, err
	}
	res := Result{Answer: ans, Sampled: len(ans.Samples)}
	if ans.Skipped || ans.SourceID == "" {
		return finish(res), nil
	}

	// G-pollute: refuse to persist a cluster that does not answer the query.
	if !cluster.RelevanceGate(query, cluster.Cluster{Content: ans.Summary}, 0.15) {
		return finish(res), nil
	}

	// Create or merge (G-id / G-merge).
	if target := e.pickMergeable(same, qe); target != nil {
		target.Evolve(query, e.querySetEmbed(ctx, *target))
		// B8 self-heal: a 待复核 (stale-prior) cluster re-anchors onto the
		// live source — its dead evidence windows are replaced, not stacked.
		wasStale := target.Lifecycle == cluster.LifecycleEmerging
		target.SourceID = ans.SourceID
		// Fold the new answer in.
		target.Content = target.Content + "\n---\n" + ans.Summary
		target.Confidence = (target.Confidence + ans.Confidence) / 2
		if len(ans.Samples) > 0 {
			if wasStale {
				target.Evidence = ans.Samples[:1]
			} else {
				target.Evidence = append(target.Evidence, ans.Samples[0])
			}
			e.writeCites(ctx, target.ID, ans.SourceID, ans.Samples[:1])
		}
		if err := e.Store.Save(ctx, *target); err != nil {
			return res, err
		}
		res.Merged = true
		res.ClusterID = target.ID
		res.ClusterVer = target.Version
		res.Persisted = true
		return finish(res), nil
	}

	// Split-cap: if the topic already has DefaultSplitCap clusters, evolve the
	// first instead of creating a sibling.
	if existing := cluster.SplitCap(mustAll(e.Store, ctx), key, e.SplitCap); len(existing) > 0 {
		c := existing[0]
		c.Evolve(query, e.querySetEmbed(ctx, c))
		if err := e.Store.Save(ctx, c); err != nil {
			return res, err
		}
		res.ClusterID = c.ID
		res.ClusterVer = c.Version
		res.Persisted = true
		res.Merged = true
		return finish(res), nil
	}

	c := cluster.New(key, query, ans.Summary, query, ans.SourceID, ans.Samples, qe, ans.Confidence)
	if err := e.Store.Save(ctx, c); err != nil {
		return res, err
	}
	e.writeCites(ctx, c.ID, ans.SourceID, ans.Samples)
	res.ClusterID = c.ID
	res.ClusterVer = c.Version
	res.Persisted = true
	return finish(res), nil
}

func (e *Engine) edgeStore() graph.Store {
	if e.Edges == nil {
		e.Edges = graph.NewMemory()
	}
	return e.Edges
}

func (e *Engine) expand(ctx context.Context, start string, probe []float64, sources []source.Source) []graph.ExpandResult {
	if e.Edges == nil {
		return nil
	}
	fresh := make(map[string]time.Time, len(sources))
	for _, s := range sources {
		fresh[s.ID] = s.UpdatedAt
	}
	ex := graph.NewExpander(e.Edges, e.Store)
	ex.HopTS = e.HopTS
	ex.Freshness = func(sourceID string) (time.Time, bool) {
		t, ok := fresh[sourceID]
		return t, ok
	}
	hop := e.HopKNN
	if hop <= 0 {
		hop = 3
	}
	got, err := ex.Expand(ctx, graph.ExpandRequest{
		StartID: start, MaxDepth: 2, MaxResults: 16,
		MinWeight: 0.5, HopKNN: hop, Probe: probe,
	})
	if err != nil {
		return nil
	}
	return got
}

// writeCites records cluster → source evidence windows (ask_cites). Best
// effort: a cite failure never fails the search.
func (e *Engine) writeCites(ctx context.Context, clusterID, sourceID string, samples []mcs.Sample) {
	if e.Cites == nil || sourceID == "" {
		return
	}
	for _, sm := range samples {
		_ = e.Cites.SaveCite(ctx, clusterID, sourceID, sm.Start, sm.End, sm.Score)
	}
}

func (e *Engine) lastClusterID(ctx context.Context) string {
	all, err := e.Store.All(ctx)
	if err != nil || len(all) == 0 {
		return ""
	}
	best := all[0]
	for _, c := range all[1:] {
		if c.UpdatedAt.After(best.UpdatedAt) {
			best = c
		}
	}
	return best.ID
}

// priorStale validates a warm prior against the CURRENT corpus (B8): the
// cluster's source must still exist and every stored evidence window must
// pin back exactly (rune-exact slice of the live body). Empty evidence is
// treated as valid — nothing there can contradict the corpus.
func (e *Engine) priorStale(ctx context.Context, c *cluster.Cluster, sources []source.Source) bool {
	if len(c.Evidence) == 0 {
		return false
	}
	byID := map[string]source.Source{}
	for _, s := range sources {
		byID[s.ID] = s
	}
	src, ok := byID[c.SourceID]
	if !ok {
		return true // source gone/updated to a new content address
	}
	runes := []rune(src.Body)
	for _, ev := range c.Evidence {
		if ev.Start < 0 || ev.End > len(runes) || ev.Start >= ev.End {
			return true
		}
		if string(runes[ev.Start:ev.End]) != ev.Content {
			return true
		}
	}
	return false
}

func (e *Engine) pickReusable(cs []cluster.Cluster, qe []float64, query string) *cluster.Cluster {
	for i := range cs {
		c := &cs[i]
		if !cluster.ShouldReuse(c, qe, e.ReuseTheta) {
			continue
		}
		if !cluster.RelevanceGate(query, *c, 0.15) {
			continue
		}
		return c
	}
	return nil
}

func (e *Engine) pickMergeable(cs []cluster.Cluster, qe []float64) *cluster.Cluster {
	for i := range cs {
		c := &cs[i]
		if cluster.CanMerge(c, qe, e.MergeTheta) {
			return c
		}
	}
	return nil
}

func (e *Engine) embed(ctx context.Context, text string) ([]float64, error) {
	vs, err := e.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vs) != 1 {
		return nil, fmt.Errorf("kb: embedder returned %d vectors", len(vs))
	}
	return vs[0], nil
}

// querySetEmbed recomputes the cluster embedding from its retained queries
// (query-driven, not content-driven).
func (e *Engine) querySetEmbed(ctx context.Context, c cluster.Cluster) []float64 {
	if len(c.Queries) == 0 {
		return c.Embed
	}
	vs, err := e.Embedder.Embed(ctx, c.Queries)
	if err != nil {
		return c.Embed
	}
	// Mean-pool.
	dims := len(vs[0])
	mean := make([]float64, dims)
	for _, v := range vs {
		for i, x := range v {
			mean[i] += x
		}
	}
	for i := range mean {
		mean[i] /= float64(len(vs))
	}
	return mean
}

func mustAll(st cluster.Store, ctx context.Context) []cluster.Cluster {
	all, _ := st.All(ctx)
	return all
}

var _ = mcs.Sample{}

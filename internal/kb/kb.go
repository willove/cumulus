// Package kb wires L2 cluster reuse into the search path: try reuse before
// sampling; on a fresh answer, create or merge a cluster (query-driven embed).
package kb

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// DefaultReuseTheta is the cosine line for reuse; DefaultMergeTheta is looser
// so near-paraphrases merge instead of fracturing.
const (
	DefaultReuseTheta = 0.85
	DefaultMergeTheta = 0.55
)

// CiteStore records cluster → source evidence windows (clus_cites).
type CiteStore interface {
	SaveCite(ctx context.Context, clusterID, sourceID string, start, end int, score float64) error
}

// Engine adds cluster reuse/evolve and graph expansion around FAST.
type Engine struct {
	Fast     *fast.Engine
	Store    cluster.Store
	Embedder cluster.Embedder
	Edges    graph.Store // optional; nil = no expansion
	Cites    CiteStore   // optional; nil = no cite edges
	// SourceReader narrows the warm-prior validation to the documents a
	// cluster anchors on. nil = validation uses the caller's list.
	SourceReader SourceReader
	ReuseTheta   float64
	MergeTheta   float64
	// writeMu serializes the cluster write path. Choosing a fold target is a
	// read-modify-write — read the candidates, decide, save the merged snapshot
	// — so two concurrent asks for one topic would either both create the same
	// cluster (same deterministic id, one snapshot lost) or both fold into the
	// same stale copy. The store is single-process (Badger holds an exclusive
	// directory lock), so this lock is the whole scope of that race.
	writeMu  sync.Mutex
	SplitCap int
	HopKNN   int
	// RejectedProposals counts AcceptFold refusals (Self-Index cost line:
	// rejected proposals are spend, not free). Read by eval-run per item.
	RejectedProposals int
	// HopTS prunes expanded neighbors whose linked source is staler than this
	// (D4 optional freshness pass 时序剪枝; 0 = off).
	HopTS time.Duration
	// MinHotness / MinConfidence prune expanded neighbors by structured
	// fields (D4 结构化剪枝; 0 = off).
	MinHotness    float64
	MinConfidence float64
	// Cursor persists the ask sequence's "previous cluster" (KV,
	// namespace-scoped). Without it the engine derives prev from the most
	// recently UPDATED cluster — which the warm reuse path masks (a reuse
	// bumps its own cluster before the link decision), so a repeated walk
	// never accumulates and query_seq edges stay one-offs. nil = fallback.
	Cursor LastClusterCursor
}

// LastClusterCursor is the persisted ask-sequence cursor: the cluster the
// previous ask resolved to. Implementations are KV-backed and scoped per
// namespace (one tenant's walk never links another's).
type LastClusterCursor interface {
	LoadLastCluster(ctx context.Context) (string, error)
	SaveLastCluster(ctx context.Context, id string) error
}

// SourceReader fetches sources by id (implemented by ingest.Store).
type SourceReader interface {
	SourcesByIDs(ctx context.Context, ids []string) ([]source.Source, error)
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
	Answer     fast.Answer `json:"answer"`
	Reused     bool        `json:"reused"`
	ClusterID  string      `json:"cluster_id,omitempty"`
	ClusterVer int         `json:"cluster_version,omitempty"`
	Sampled    int         `json:"sampled"` // 0 on reuse
	Persisted  bool        `json:"persisted"`
	Merged     bool        `json:"merged"`
	// FoldRejected carries the AcceptFold verdict when a proposed fold was
	// refused. Non-empty means the answer was NOT folded into the target's
	// content/evidence (only the ask was recorded, so the topic does not
	// fracture); it used to be discarded, making a refused fold look like a
	// successful merge in the eval cost line.
	FoldRejected string               `json:"fold_rejected,omitempty"`
	Neighbors    []graph.ExpandResult `json:"neighbors,omitempty"`
	PrevCluster  string               `json:"prev_cluster,omitempty"`
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
	prevID := e.prevClusterID(ctx)

	finish := func(res Result) Result {
		if res.ClusterID != "" {
			if prevID != "" && prevID != res.ClusterID {
				_ = graph.LinkQuerySeq(ctx, e.edgeStore(), prevID, res.ClusterID)
			}
			e.rememberCluster(ctx, res.ClusterID)
			res.PrevCluster = prevID
			res.Neighbors = e.expand(ctx, res.ClusterID, qe, sources)
		}
		return res
	}

	// Phase 0a: reuse within the same topic_key / aliases (max-over-keys).
	same, err := e.Store.FindByTopic(ctx, key)
	if err != nil {
		return Result{}, err
	}
	if res, ok := e.reuseAttempt(ctx, query, qe, same, sources); ok {
		return finish(res), nil
	}

	// Phase 0b: cross-topic near hits are merge candidates only — never
	// returned as a reused answer (G-pollute). L0 still answers; saveAnswer
	// folds into the strongest candidate when AcceptFold passes.
	ans, err := e.Fast.Search(ctx, query, sources)
	if err != nil {
		return Result{}, err
	}
	res, err := e.saveAnswer(ctx, ans, sources, qe, false)
	if err != nil {
		return res, err
	}
	return finish(res), nil
}

// TryReuseNarrow is the narrow reuse attempt: the warm-prior validation runs
// against ONLY the sources the candidate cluster anchors on (its evidence
// windows + answer source), so a warm hit costs one small read instead of a
// full-corpus page walk. Returns ok=false whenever the caller must fall back
// to the full path (no reader wired, no candidate, prior stale against its
// own anchors). On a reader error it also returns false — a failed narrow
// read must not be mistaken for a stale prior.
func (e *Engine) TryReuseNarrow(ctx context.Context, query string) (Result, []source.Source, bool, error) {
	if e.Store == nil || e.Embedder == nil || e.SourceReader == nil {
		return Result{}, nil, false, nil
	}
	qe, err := e.embed(ctx, query)
	if err != nil {
		return Result{}, nil, false, err
	}
	key := cluster.TopicKey(query)
	same, err := e.Store.FindByTopic(ctx, key)
	if err != nil {
		return Result{}, nil, false, err
	}
	c := e.pickReusable(same, qe, query)
	if c == nil {
		return Result{}, nil, false, nil
	}
	narrow, err := e.SourceReader.SourcesByIDs(ctx, anchoredSourceIDs(*c))
	if err != nil {
		return Result{}, nil, false, nil // conservative: let the full path decide
	}
	res, ok := e.reuseAttemptWith(ctx, query, qe, same, c, narrow)
	if !ok {
		return Result{}, nil, false, nil
	}
	// finish() equivalent: graph links + neighbours over the narrow set.
	prevID := e.prevClusterID(ctx)
	if res.ClusterID != "" {
		if prevID != "" && prevID != res.ClusterID {
			_ = graph.LinkQuerySeq(ctx, e.edgeStore(), prevID, res.ClusterID)
		}
		e.rememberCluster(ctx, res.ClusterID)
		res.PrevCluster = prevID
		res.Neighbors = e.expand(ctx, res.ClusterID, qe, narrow)
	}
	return res, narrow, true, nil
}

// reuseAttempt is Phase 0a against the caller's source list.
func (e *Engine) reuseAttempt(ctx context.Context, query string, qe []float64, same []cluster.Cluster, sources []source.Source) (Result, bool) {
	c := e.pickReusable(same, qe, query)
	if c == nil {
		return Result{}, false
	}
	return e.reuseAttemptWith(ctx, query, qe, same, c, sources)
}

// reuseAttemptWith validates the chosen candidate against `validateAgainst`
// (full corpus or the narrow anchor set) and, when warm, evolves + returns
// the reuse result. A stale prior is marked 待复核 and reported as "no
// reuse" — the L0 path self-heals it.
func (e *Engine) reuseAttemptWith(ctx context.Context, query string, qe []float64, same []cluster.Cluster, c *cluster.Cluster, validateAgainst []source.Source) (Result, bool) {
	// warm-prior validation: a prior is only warm while it
	// still matches the CURRENT corpus. Any evidence window that no
	// longer pins back exactly (source updated/gone) disqualifies the
	// prior — mark it 待复核 and fall through to L0, which self-heals
	// the cluster through the merge path below.
	if e.priorStale(ctx, c, validateAgainst) {
		c.Lifecycle = cluster.LifecycleEmerging
		_ = e.Store.Save(ctx, *c)
		return Result{}, false
	}
	if c.Lifecycle == cluster.LifecycleEmerging {
		// the prior just validated against the current corpus —
		// self-heal complete.
		c.Lifecycle = cluster.LifecycleStable
	}
	c.Evolve(query, nil)
	e.refreshEmbeds(ctx, c)
	_ = e.Store.Save(ctx, *c)
	// Reuse still surfaces the stored evidence windows (0 new samples).
	samples := cluster.NormalizeEvidence(c.SourceID, c.Evidence)
	cov := mcs.Coverage(query, samples)
	return Result{
		Answer: fast.Answer{
			Query: query, Mode: "FAST", Confidence: c.Confidence,
			Summary: c.Content, SourceID: c.SourceID, LLMCalls: 0,
			Samples:  samples,
			Coverage: cov,
		},
		Reused: true, ClusterID: c.ID, ClusterVer: c.Version,
		Sampled: 0, Persisted: true,
	}, true
}

// anchoredSourceIDs lists the documents a cluster's answer rests on: its
// answer source plus every evidence window's source.
func anchoredSourceIDs(c cluster.Cluster) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(c.SourceID)
	for _, ev := range c.Evidence {
		add(ev.Source)
	}
	return out
}

// crossTopicNear returns clusters outside this topic_key that are close enough
// to be MERGE candidates and that pass the G-pollute relevance gate. They are
// offered to the merge path only — never returned as a reused answer.
//
// The bar is MergeTheta, NOT ReuseTheta. SSOT D3: "未过复用线但与既有簇
// embed_sim ≥ merge_θ 时 merge 进旧簇（追加 evidence/query），不新建" — the
// merge band is precisely the band BELOW the reuse line. Pre-filtering at
// ReuseTheta here made MergeTheta unreachable cross-topic (the candidate set
// was already truncated to ≥0.85), so G-merge only ever fired inside one
// topic_key where reuse usually fires first.
func (e *Engine) crossTopicNear(ctx context.Context, key string, qe []float64, query string) []cluster.Cluster {
	all, err := e.Store.All(ctx)
	if err != nil {
		return nil
	}
	var out []cluster.Cluster
	for _, c := range all {
		if c.TopicKey == key || containsTopicAlias(c, key) {
			continue
		}
		if !cluster.CanMerge(&c, query, qe, e.MergeTheta) {
			continue
		}
		if !cluster.RelevanceGate(query, c, 0.15) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// mergeEvidence appends the new windows to the cluster's, dropping any window
// already present by (source, start, end). Without the dedup every ask that
// lands in the [MergeTheta, ReuseTheta) band re-appended the same windows, so a
// hot cluster's evidence list grew without bound and G-idem ("evidence 不重复
// 追加") was violated.
func mergeEvidence(have, add []mcs.Sample) []mcs.Sample {
	if len(add) == 0 {
		return have
	}
	seen := make(map[string]bool, len(have))
	for _, sm := range have {
		seen[evidenceKey(sm)] = true
	}
	out := have
	for _, sm := range add {
		k := evidenceKey(sm)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, sm)
	}
	return out
}

func evidenceKey(sm mcs.Sample) string {
	return fmt.Sprintf("%s|%d|%d", sm.Source, sm.Start, sm.End)
}

func containsTopicAlias(c cluster.Cluster, key string) bool {
	for _, k := range c.TopicKeys {
		if k == key {
			return true
		}
	}
	return false
}

func (e *Engine) Persist(ctx context.Context, ans fast.Answer, sources []source.Source) (Result, error) {
	res := Result{Answer: ans, Sampled: len(ans.Samples)}
	if e.Store == nil || e.Embedder == nil || ans.Skipped || ans.Refused || ans.SourceID == "" {
		return res, nil
	}
	qe, err := e.embed(ctx, ans.Query)
	if err != nil {
		return res, err
	}
	return e.saveAnswer(ctx, ans, sources, qe, true)
}

// candidates is the fold-candidate set: same-topic clusters plus cross-topic
// near hits. saveAnswer re-derives it under the write lock, because the fold
// decision must see what the store holds now, not what it held before the
// caller's search ran.
func (e *Engine) candidates(ctx context.Context, key string, qe []float64, query string) ([]cluster.Cluster, error) {
	same, err := e.Store.FindByTopic(ctx, key)
	if err != nil {
		return nil, err
	}
	if cross := e.crossTopicNear(ctx, key, qe, query); len(cross) > 0 {
		same = append(append([]cluster.Cluster(nil), same...), cross...)
	}
	return same, nil
}

func (e *Engine) saveAnswer(ctx context.Context, ans fast.Answer, sources []source.Source, qe []float64, replace bool) (Result, error) {
	ans.Samples = cluster.NormalizeEvidence(ans.SourceID, ans.Samples)
	res := Result{Answer: ans, Sampled: len(ans.Samples)}
	if ans.Skipped || ans.Refused || ans.SourceID == "" || !cluster.RelevanceGate(ans.Query, cluster.Cluster{Content: ans.Summary}, 0.15) {
		return res, nil
	}
	key := cluster.TopicKey(ans.Query)
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	same, err := e.candidates(ctx, key, qe, ans.Query)
	if err != nil {
		return res, err
	}
	target := e.pickMergeable(same, qe, ans.Query)
	if target == nil && replace {
		if existing := cluster.SplitCap(same, key, e.SplitCap); len(existing) > 0 {
			target = &existing[0]
		}
	}
	// A rejected fold must stay rejected. The A2 verdict (specificity /
	// separation / cross-topic divergent claims) decides whether the new
	// answer's CONTENT may be folded in; falling through to a query-only
	// evolve and reporting it as a merge used to undo the verdict silently —
	// and RejectedProposals then counted a fold that in fact happened.
	foldRejected := ""
	if target != nil {
		if ok, why := cluster.AcceptFold(*target, cluster.Cluster{
			ID: "proposed", TopicKey: key, Queries: []string{ans.Query},
			Content: ans.Summary,
		}, same, 3); !ok {
			foldRejected = why
			e.RejectedProposals++
			target = nil
		}
	}
	if target != nil {
		wasStale := e.priorStale(ctx, target, sources)
		target.Evidence = cluster.NormalizeEvidence(target.SourceID, target.Evidence)
		target.Evolve(ans.Query, nil)
		// A cross-topic fold must leave the loser's topic key behind as an
		// alias, or the new wording can never be found by FindByTopic again —
		// every repeat would re-run L0 and re-merge instead of reusing.
		if key != target.TopicKey && !containsTopicAlias(*target, key) {
			target.TopicKeys = append(target.TopicKeys, key)
		}
		e.refreshEmbeds(ctx, target)
		if replace || wasStale {
			target.Content = ans.Summary
			target.Confidence = ans.Confidence
			target.Evidence = ans.Samples
		} else {
			// The appended summary numbers its own evidence from 1, and that
			// list now starts after the cluster's own: shift every marker, or
			// the appended text cites the survivor's evidence.
			remap := make(map[int]int, len(ans.Samples))
			for k := range ans.Samples {
				remap[k+1] = len(target.Evidence) + k + 1
			}
			target.Content += "\n---\n" + cluster.RenumberCitations(ans.Summary, remap)
			target.Confidence = (target.Confidence + ans.Confidence) / 2
			// G-idem: a repeated ask must not append the same window twice.
			target.Evidence = mergeEvidence(target.Evidence, ans.Samples)
		}
		target.SourceID = ans.SourceID
		if wasStale {
			target.Lifecycle = cluster.LifecycleEmerging
		}
		if err := e.Store.Save(ctx, *target); err != nil {
			return res, err
		}
		e.writeCites(ctx, target.ID, ans.SourceID, ans.Samples)
		res.Merged = true
		res.ClusterID = target.ID
		res.ClusterVer = target.Version
		res.Persisted = true
		return res, nil
	}

	// No foldable target. The topic may still already exist, and G-id caps
	// same-topic clusters at split_cap (default 1): record the ask on the
	// existing cluster instead of fracturing it. This is a QUERY-ONLY
	// evolution — content and evidence stay exactly as they were, because the
	// A2 gate just refused them — so it is reported as NOT merged. Without the
	// honest flag a rejected fold looked like a successful one.
	if existing := cluster.SplitCap(same, key, e.SplitCap); len(existing) > 0 {
		c := existing[0]
		c.Evolve(ans.Query, nil)
		e.refreshEmbeds(ctx, &c)
		if err := e.Store.Save(ctx, c); err != nil {
			return res, err
		}
		res.ClusterID = c.ID
		res.ClusterVer = c.Version
		res.Persisted = true
		res.Merged = false
		res.FoldRejected = foldRejected
		return res, nil
	}

	c := cluster.New(key, ans.Query, ans.Summary, ans.Query, ans.SourceID, ans.Samples, qe, ans.Confidence)
	e.refreshEmbeds(ctx, &c)
	if err := e.Store.Save(ctx, c); err != nil {
		return res, err
	}
	e.writeCites(ctx, c.ID, ans.SourceID, ans.Samples)
	res.ClusterID = c.ID
	res.ClusterVer = c.Version
	res.Persisted = true
	return res, nil
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
	// a narrow reuse hit only carries the cluster's own anchors, so a
	// neighbour's source may be missing from `fresh`. When structured/TS
	// pruning is configured, read that one document instead of silently
	// disabling the prune (regression).
	var reader SourceReader
	if e.SourceReader != nil && (e.HopTS > 0 || e.MinHotness > 0 || e.MinConfidence > 0) {
		reader = e.SourceReader
	}
	ex := graph.NewExpander(e.Edges, e.Store)
	ex.HopTS = e.HopTS
	ex.Freshness = func(sourceID string) (time.Time, bool) {
		if t, ok := fresh[sourceID]; ok {
			return t, true
		}
		if reader == nil {
			return time.Time{}, false
		}
		got, err := reader.SourcesByIDs(ctx, []string{sourceID})
		if err != nil || len(got) != 1 {
			return time.Time{}, false
		}
		fresh[sourceID] = got[0].UpdatedAt
		return got[0].UpdatedAt, true
	}
	hop := e.HopKNN
	if hop <= 0 {
		hop = 3
	}
	got, err := ex.Expand(ctx, graph.ExpandRequest{
		StartID: start, MaxDepth: 2, MaxResults: 16,
		MinWeight: 0.5, HopKNN: hop, Probe: probe,
		MinHotness: e.MinHotness, MinConfidence: e.MinConfidence,
	})
	if err != nil {
		return nil
	}
	return got
}

// writeCites records cluster → source evidence windows (clus_cites). Best
// effort: a cite failure never fails the search. Also records co_occur edges
// to sibling clusters anchored on the same source (ir-rag A4 profile).
func (e *Engine) writeCites(ctx context.Context, clusterID, sourceID string, samples []mcs.Sample) {
	if e.Cites == nil || sourceID == "" {
		return
	}
	for _, sm := range cluster.NormalizeEvidence(sourceID, samples) {
		_ = e.Cites.SaveCite(ctx, clusterID, sm.Source, sm.Start, sm.End, sm.Score)
	}
	e.linkCoOccur(ctx, clusterID, sourceID)
}

// linkCoOccur bumps co_occur between this cluster and any other cluster that
// already anchors the same source (shared-evidence co-mention).
func (e *Engine) linkCoOccur(ctx context.Context, clusterID, sourceID string) {
	if e.Store == nil || e.Edges == nil || sourceID == "" {
		return
	}
	all, err := e.Store.All(ctx)
	if err != nil {
		return
	}
	for _, c := range all {
		if c.ID == clusterID {
			continue
		}
		if c.SourceID == sourceID {
			_ = graph.LinkCoOcur(ctx, e.edgeStore(), clusterID, c.ID)
		}
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

// prevClusterID is the ask sequence's previous cluster: the persisted cursor
// when wired, the latest-updated cluster otherwise (fallback for stores
// without a cursor — tests, embeds).
func (e *Engine) prevClusterID(ctx context.Context) string {
	if e.Cursor != nil {
		if id, err := e.Cursor.LoadLastCluster(ctx); err == nil {
			return id
		}
	}
	return e.lastClusterID(ctx)
}

// rememberCluster advances the sequence cursor. A cursor error is ignored:
// the worst case is one missing walk link, never a failed search.
func (e *Engine) rememberCluster(ctx context.Context, id string) {
	if e.Cursor == nil || id == "" {
		return
	}
	_ = e.Cursor.SaveLastCluster(ctx, id)
}

// priorStale validates a warm prior against the CURRENT corpus: the
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
	for _, ev := range cluster.NormalizeEvidence(c.SourceID, c.Evidence) {
		src, ok := byID[ev.Source]
		if !ok {
			return true
		}
		runes := []rune(src.Body)
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
		if !cluster.ShouldReuse(c, query, qe, e.ReuseTheta) {
			continue
		}
		if !cluster.RelevanceGate(query, *c, 0.15) {
			continue
		}
		return c
	}
	return nil
}

func (e *Engine) pickMergeable(cs []cluster.Cluster, qe []float64, query string) *cluster.Cluster {
	for i := range cs {
		c := &cs[i]
		if cluster.CanMerge(c, query, qe, e.MergeTheta) {
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
// (query-driven, not content-driven); falls back to the stored embedding when
// the embedder fails or the query set is empty.
func (e *Engine) querySetEmbed(ctx context.Context, c cluster.Cluster) []float64 {
	if len(c.Queries) == 0 {
		return c.Embed
	}
	qe, err := cluster.QuerySetEmbed(ctx, e.Embedder, c.Queries)
	if err != nil {
		return c.Embed
	}
	return qe
}

// refreshEmbeds recomputes the cluster's query-set embed AND its per-level-key
// segment embeds in one pass (MVR-cache 2.5). Segment embeds come from a
// single batch call; an error leaves the cluster with whatever it had
// (misaligned segment maps are dropped, never silently scored).
func (e *Engine) refreshEmbeds(ctx context.Context, c *cluster.Cluster) {
	if e.Embedder == nil {
		return
	}
	c.Embed = e.querySetEmbed(ctx, *c)
	keys := c.LevelKeyTexts()
	if len(keys) == 0 {
		return
	}
	vs, err := e.Embedder.Embed(ctx, keys)
	if err != nil || len(vs) != len(keys) {
		return
	}
	c.AttachKeyEmbeds(vs)
}

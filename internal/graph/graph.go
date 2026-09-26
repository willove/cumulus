// Package graph is the L2 knowledge-graph layer: weak edges between clusters
// (co_occur / query_seq / embed_sim), bounded BFS expansion (1..2 hops), and
// hopKNN pruning. Empty graph must fall back to L0 — edges are an accelerator,
// never a correctness source (S5 plan D1/D4). P4 adds two rich kinds on the
// same edge documents: barrier (contested — expansion refuses) and pathway
// (walked repeatedly — expansion prefers).
package graph

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/willove/cumulus/internal/cluster"
)

// Edge sources (WeakSemanticEdge in the S5 plan).
const (
	SourceCoOcur   = "co_occur"
	SourceQuerySeq = "query_seq"
	SourceEmbedSim = "embed_sim"
)

// Rich edge kinds (P4). Kind is empty for plain weak edges — the zero value
// keeps every pre-P4 edge behaving exactly as before.
const (
	// KindPathway marks a walked-cognitive-path edge: the query_seq link was
	// traversed repeatedly (>= PathwayMinHits), so traversal prefers it.
	KindPathway = "pathway"
	// KindBarrier marks a contested link (conflict pair): traversal must NOT
	// serve the barred neighbor as evidence for this side.
	KindBarrier = "barrier"
)

// PathwayMinHits is how many query_seq traversals upgrade a weak edge to a
// pathway (and PathwayBonus below is how much traversal prefers it).
const (
	PathwayMinHits  = 2
	PathwayBonus    = 1.5
	defaultQuerySeq = 0.6
)

// Edge is a directed weak semantic link between two clusters.
type Edge struct {
	ID     string    `json:"_id"`
	From   string    `json:"_from"`
	To     string    `json:"_to"`
	Weight float64   `json:"weight"` // [0,1]
	Source string    `json:"source"` // co_occur | query_seq | embed_sim
	Kind   string    `json:"kind,omitempty"`
	Reason string    `json:"reason,omitempty"` // human-readable derivation (UI/API)
	Hits   int       `json:"hits,omitempty"`   // query_seq traversals so far
	TS     time.Time `json:"ts"`               // write timestamp (diagnostic read faces surface it)
}

// Store persists edges (clus_weak_edges in production).
type Store interface {
	Save(ctx context.Context, e Edge) error
	From(ctx context.Context, id string) ([]Edge, error)
	To(ctx context.Context, id string) ([]Edge, error)
	All(ctx context.Context) ([]Edge, error)
	Delete(ctx context.Context, id string) error
}

// Memory is an in-memory edge store for gates and unit tests.
type Memory struct {
	m map[string]Edge
}

func NewMemory() *Memory { return &Memory{m: map[string]Edge{}} }

func edgeID(from, to, source string) string { return "e:" + from + "-" + to + "-" + source }

// edgeIDWithKind composes an edge id that also carries the rich kind. Plain
// weak edges keep the historical id (pre-P4 documents stay readable); rich
// edges (pathway/barrier) get their own namespace so a later plain save of the
// same (from,to,source) can never clobber them. Without this, LinkCoOcur
// overwrote a barrier edge — the conflict bar vanished and expansion served
// the contested cluster as evidence (P4 / G-pollute violated).
func edgeIDWithKind(from, to, source, kind string) string {
	if kind == "" {
		return edgeID(from, to, source)
	}
	return "e:" + kind + ":" + from + "-" + to + "-" + source
}

func (s *Memory) Save(_ context.Context, e Edge) error {
	if e.ID == "" {
		e.ID = edgeIDWithKind(e.From, e.To, e.Source, e.Kind)
	}
	s.m[e.ID] = e
	return nil
}

func (s *Memory) From(_ context.Context, id string) ([]Edge, error) {
	var out []Edge
	for _, e := range s.m {
		if e.From == id {
			out = append(out, e)
		}
	}
	sortEdges(out)
	return out, nil
}

func (s *Memory) To(_ context.Context, id string) ([]Edge, error) {
	var out []Edge
	for _, e := range s.m {
		if e.To == id {
			out = append(out, e)
		}
	}
	sortEdges(out)
	return out, nil
}

func (s *Memory) All(_ context.Context) ([]Edge, error) {
	out := make([]Edge, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sortEdges(out)
	return out, nil
}

func (s *Memory) Delete(_ context.Context, id string) error {
	delete(s.m, id)
	return nil
}

func sortEdges(es []Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From == es[j].From {
			return es[i].To < es[j].To
		}
		return es[i].From < es[j].From
	})
}

// ExpandRequest bounds one BFS expansion (four-limit style).
type ExpandRequest struct {
	StartID    string
	MaxDepth   int    // default 2 (plan: 1..2 hops)
	MaxResults int    // default 32
	Direction  string // out | in | any (default any)
	// MinWeight drops weak links below this.
	MinWeight float64
	// HopKNN, when set, keeps at most K neighbors per hop closest to Probe
	// (cosine on cluster embeddings) — the multi-modal prune of D4.
	HopKNN int
	Probe  []float64
	// MinHotness / MinConfidence filter candidate clusters (structured prune).
	MinHotness    float64
	MinConfidence float64
}

// ExpandResult is one neighbor at some depth.
type ExpandResult struct {
	Cluster cluster.Cluster `json:"cluster"`
	Depth   int             `json:"depth"`
	Via     []string        `json:"via"`   // edge ids on the path
	Score   float64         `json:"score"` // accumulated edge weight
}

// Expander walks weak edges over a cluster store.
type Expander struct {
	Edges    Store
	Clusters cluster.Store
	// PathwayBonus multiplies a pathway edge's weight during expansion
	// (P4: walked paths rank above one-off links). 0 = off.
	PathwayBonus float64
	// HopTS, when > 0 together with Freshness, prunes neighbors whose linked
	// source is staler than this (D4 optional freshness pass 时序剪枝).
	// Clusters without a resolvable source are never pruned (阙疑不剪).
	HopTS     time.Duration
	Freshness func(sourceID string) (time.Time, bool)
}

func NewExpander(es Store, cs cluster.Store) *Expander {
	return &Expander{Edges: es, Clusters: cs, PathwayBonus: PathwayBonus}
}

// Expand runs bounded BFS. Empty neighborhood returns nil, nil — callers fall
// back to L0 on an empty graph.
// startBarriers collects the clusters the start is contested with (both
// directions — LinkBarrier writes them pairwise). A store error degrades to
// an empty set: barriers are a guard, never a correctness source.
func (e *Expander) startBarriers(ctx context.Context, start, dir string) map[string]bool {
	barred := map[string]bool{}
	if start == "" || e.Edges == nil {
		return barred
	}
	collect := func(es []Edge) {
		for _, ed := range es {
			if ed.Kind != KindBarrier {
				continue
			}
			if ed.To != start {
				barred[ed.To] = true
			}
			if ed.From != start {
				barred[ed.From] = true
			}
		}
	}
	if out, err := e.Edges.From(ctx, start); err == nil {
		collect(out)
	}
	if dir == "any" || dir == "in" {
		if in, err := e.Edges.To(ctx, start); err == nil {
			collect(in)
		}
	}
	return barred
}

func (e *Expander) Expand(ctx context.Context, req ExpandRequest) ([]ExpandResult, error) {
	if req.StartID == "" {
		return nil, nil
	}
	if req.MaxDepth <= 0 {
		req.MaxDepth = 2
	}
	if req.MaxResults <= 0 {
		req.MaxResults = 32
	}
	if req.Direction == "" {
		req.Direction = "any"
	}

	type node struct {
		id    string
		depth int
		via   []string
		score float64
	}
	seen := map[string]bool{req.StartID: true}
	frontier := []node{{id: req.StartID, depth: 0}}
	var out []ExpandResult

	// P4 barrier set of the START: a contested link between the start and X
	// keeps X out of this expansion at EVERY depth — "问 A 时别把冲突簇 B
	// 当邻域". Bidirectional edges make start→X and X→start the same bar.
	// Barriers between other nodes only stop being traversal links (handled
	// per-edge below); they do not bar anyone from the start's neighborhood.
	barred := e.startBarriers(ctx, req.StartID, req.Direction)

	for depth := 1; depth <= req.MaxDepth && len(frontier) > 0; depth++ {
		var next []node
		for _, n := range frontier {
			edges, err := e.edgesFrom(ctx, n.id, req.Direction)
			if err != nil {
				return nil, err
			}
			// Best link per target wins: parallel edges (co_occur + query_seq,
			// say) must not let a one-off outrank a walked pathway, and an
			// inbound view carries its rich fields (a barrier seen inbound is
			// the same bar).
			type cand struct {
				to    string
				edge  Edge
				score float64
				via   []string
			}
			best := map[string]cand{}
			for _, ed := range edges {
				if ed.Weight < req.MinWeight || seen[ed.To] {
					continue
				}
				// P4 barrier: a contested link never serves its target as
				// evidence for this side (direct hop only — the prune does
				// not propagate down the path).
				if ed.Kind == KindBarrier || barred[ed.To] {
					continue
				}
				// P4 pathway: a walked link outranks a one-off of the same
				// weight.
				score := n.score + ed.Weight
				if ed.Kind == KindPathway && e.PathwayBonus > 0 {
					score = n.score + ed.Weight*e.PathwayBonus
				}
				c := cand{to: ed.To, edge: ed, score: score, via: append(append([]string{}, n.via...), ed.ID)}
				if prev, ok := best[ed.To]; !ok || c.score > prev.score {
					best[ed.To] = c
				}
			}
			cands := make([]cand, 0, len(best))
			for _, c := range best {
				cands = append(cands, c)
			}
			sort.Slice(cands, func(i, j int) bool { return cands[i].edge.ID < cands[j].edge.ID })
			// hopKNN prune.
			if req.HopKNN > 0 && req.Probe != nil && len(cands) > req.HopKNN {
				type scored struct {
					c   cand
					sim float64
				}
				var ss []scored
				for _, c := range cands {
					cl, err := e.Clusters.Get(ctx, c.to)
					if err != nil || cl == nil {
						continue
					}
					if !passStructured(*cl, req) {
						continue
					}
					ss = append(ss, scored{c: c, sim: cluster.Cosine(cl.Embed, req.Probe)})
				}
				sort.Slice(ss, func(i, j int) bool { return ss[i].sim > ss[j].sim })
				// Keep the top-K by probe similarity.
				kept := make([]cand, 0, req.HopKNN)
				for i, s := range ss {
					if i >= req.HopKNN {
						break
					}
					kept = append(kept, s.c)
				}
				cands = kept
			}

			for _, c := range cands {
				if seen[c.to] {
					continue
				}
				seen[c.to] = true
				cl, err := e.Clusters.Get(ctx, c.to)
				if err != nil {
					return nil, err
				}
				if cl == nil {
					continue
				}
				if !passStructured(*cl, req) {
					continue
				}
				if e.stale(ctx, *cl, req) {
					continue
				}
				out = append(out, ExpandResult{Cluster: *cl, Depth: depth, Via: c.via, Score: c.score})
				next = append(next, node{id: c.to, depth: depth, via: c.via, score: c.score})
				if len(out) >= req.MaxResults {
					return out, nil
				}
			}
		}
		frontier = next
	}
	return out, nil
}

func (e *Expander) edgesFrom(ctx context.Context, id, dir string) ([]Edge, error) {
	var out []Edge
	if dir == "out" || dir == "any" {
		es, err := e.Edges.From(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	if dir == "in" || dir == "any" {
		es, err := e.Edges.To(ctx, id)
		if err != nil {
			return nil, err
		}
		// Reverse: neighbor is the From side. The rich fields travel with
		// the edge — an inbound barrier/pathway keeps its kind.
		for _, ed := range es {
			out = append(out, Edge{
				ID: ed.ID, From: ed.To, To: ed.From,
				Weight: ed.Weight, Source: ed.Source,
				Kind: ed.Kind, Reason: ed.Reason, Hits: ed.Hits,
			})
		}
	}
	return out, nil
}

func passStructured(c cluster.Cluster, req ExpandRequest) bool {
	if c.Lifecycle == cluster.LifecycleDeprecated {
		return false
	}
	if c.Hotness < req.MinHotness {
		return false
	}
	if c.Confidence < req.MinConfidence {
		return false
	}
	return true
}

// LinkEmbedSim adds a cosine-derived weak edge between two clusters (periodic
// backfill; also callable after save).
func LinkEmbedSim(ctx context.Context, st Store, a, b cluster.Cluster) error {
	if a.ID == b.ID {
		return nil
	}
	sim := cluster.Cosine(a.Embed, b.Embed)
	if sim < 0.55 {
		return nil
	}
	// Undirected: store one direction, lower id first for stability.
	from, to := a.ID, b.ID
	if from > to {
		from, to = to, from
	}
	return st.Save(ctx, Edge{From: from, To: to, Weight: sim, Source: SourceEmbedSim})
}

// stale reports whether a neighbor's linked source is older than HopTS.
func (e *Expander) stale(_ context.Context, cl cluster.Cluster, req ExpandRequest) bool {
	if e.HopTS <= 0 || e.Freshness == nil || cl.SourceID == "" {
		return false
	}
	ts, ok := e.Freshness(cl.SourceID)
	if !ok {
		return false
	}
	return time.Since(ts) > e.HopTS
}

// LinkQuerySeq records that users moved from cluster a to b (session order).
// Repeat traversals of the same link upgrade it to a pathway: the edge has
// been WALKED (>= PathwayMinHits times), so expansion prefers it over a
// one-off query_seq — accumulation is empirical, never an LLM guess.
func LinkQuerySeq(ctx context.Context, st Store, from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	ed := Edge{From: from, To: to, Weight: defaultQuerySeq, Source: SourceQuerySeq, Hits: 1}
	for _, prev := range mustFrom(ctx, st, from) {
		if prev.To == to && prev.Source == SourceQuerySeq {
			ed.Hits = prev.Hits + 1
			// Carry the previous id so an upgrade REPLACES the plain edge
			// instead of leaving a duplicate beside it (rich kinds now own
			// their id namespace — see edgeIDWithKind).
			ed.ID = prev.ID
			if ed.Hits >= PathwayMinHits {
				ed.Kind = KindPathway
				ed.Reason = fmt.Sprintf("query_seq×%d", ed.Hits)
			}
			break
		}
	}
	return st.Save(ctx, ed)
}

// LinkBarrier records a contested link between two clusters (a conflict
// pair): bidirectionally, idempotently. Expansion then refuses to serve the
// barred neighbor from either side — "别把 A 的结论套到 B".
func LinkBarrier(ctx context.Context, st Store, a, b, reason string) error {
	if st == nil || a == "" || b == "" || a == b {
		return nil
	}
	if reason == "" {
		reason = "contested"
	}
	for _, dir := range [][2]string{{a, b}, {b, a}} {
		if err := st.Save(ctx, Edge{
			From: dir[0], To: dir[1], Weight: 1, Source: SourceCoOcur,
			Kind: KindBarrier, Reason: reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// mustFrom reads a node's outbound edges; a store error degrades to no
// history (the link still saves as a first traversal).
func mustFrom(ctx context.Context, st Store, id string) []Edge {
	if st == nil {
		return nil
	}
	es, err := st.From(ctx, id)
	if err != nil {
		return nil
	}
	return es
}

// LinkCoOcur records that two clusters shared evidence/source (co-mention).
// Repeat co-mentions bump the weight (capped at 1) so the profile accumulates
// like Self-Index co-retrieval counts (ir-rag A4).
func LinkCoOcur(ctx context.Context, st Store, a, b string) error {
	if a == "" || b == "" || a == b {
		return nil
	}
	from, to := a, b
	if from > to {
		from, to = to, from
	}
	w := 0.7
	if prev := CoOccurWeight(ctx, st, from, to); prev > 0 {
		w = prev + 0.1
		if w > 1 {
			w = 1
		}
	}
	return st.Save(ctx, Edge{From: from, To: to, Weight: w, Source: SourceCoOcur})
}

// CoOccurWeight is the current co_occur weight between two clusters (0 if none).
// Barrier edges are excluded: they record a CONTESTED link (weight 1 by
// construction), so counting them would report a contested pair as the
// strongest co-retrieval partner and inflate tidy's co signal.
func CoOccurWeight(ctx context.Context, st Store, a, b string) float64 {
	if st == nil || a == "" || b == "" || a == b {
		return 0
	}
	es, err := st.From(ctx, a)
	if err != nil {
		return 0
	}
	for _, e := range es {
		if e.Source == SourceCoOcur && e.Kind != KindBarrier && ((e.From == a && e.To == b) || (e.From == b && e.To == a)) {
			return e.Weight
		}
	}
	// Undirected storage uses lower id first; also check To.
	ts, err := st.To(ctx, a)
	if err != nil {
		return 0
	}
	for _, e := range ts {
		if e.Source == SourceCoOcur && e.Kind != KindBarrier && (e.From == b || e.To == b) {
			return e.Weight
		}
	}
	return 0
}

// CoOccurPartners is the co-retrieval profile of one cluster: partners sorted
// by weight desc (Self-Index A.1.2 comparative diagnosis input). Barrier edges
// are not co-retrieval signal and are left out.
func CoOccurPartners(ctx context.Context, st Store, id string) []Edge {
	if st == nil || id == "" {
		return nil
	}
	var out []Edge
	if es, err := st.From(ctx, id); err == nil {
		for _, e := range es {
			if e.Source == SourceCoOcur && e.Kind != KindBarrier {
				out = append(out, e)
			}
		}
	}
	if ts, err := st.To(ctx, id); err == nil {
		for _, e := range ts {
			if e.Source == SourceCoOcur && e.Kind != KindBarrier {
				out = append(out, e)
			}
		}
	}
	// Dedup undirected duplicates.
	seen := map[string]bool{}
	var uniq []Edge
	for _, e := range out {
		key := e.From + "|" + e.To + "|" + e.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool {
		if uniq[i].Weight != uniq[j].Weight {
			return uniq[i].Weight > uniq[j].Weight
		}
		return uniq[i].From+uniq[i].To < uniq[j].From+uniq[j].To
	})
	return uniq
}

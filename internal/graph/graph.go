// Package graph is the L2 knowledge-graph layer: weak edges between clusters
// (co_occur / query_seq / embed_sim), bounded BFS expansion (1..2 hops), and
// hopKNN pruning. Empty graph must fall back to L0 — edges are an accelerator,
// never a correctness source (S5 plan D1/D4).
package graph

import (
	"context"
	"sort"
	"time"

	"github.com/cumubase/ask/internal/cluster"
)

// Edge sources (WeakSemanticEdge in the S5 plan).
const (
	SourceCoOcur   = "co_occur"
	SourceQuerySeq = "query_seq"
	SourceEmbedSim = "embed_sim"
)

// Edge is a directed weak semantic link between two clusters.
type Edge struct {
	ID     string  `json:"_id"`
	From   string  `json:"_from"`
	To     string  `json:"_to"`
	Weight float64 `json:"weight"` // [0,1]
	Source string  `json:"source"` // co_occur | query_seq | embed_sim
}

// Store persists edges (cumudb ask_weak_edges in production).
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

func (s *Memory) Save(_ context.Context, e Edge) error {
	if e.ID == "" {
		e.ID = edgeID(e.From, e.To, e.Source)
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

// ExpandRequest bounds one BFS expansion (four-limit style, plan G1 analogy).
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
	Cluster cluster.Cluster
	Depth   int
	Via     []string // edge ids on the path
	Score   float64  // accumulated edge weight
	Pruned  bool     // dropped by hopKNN this hop (kept for assertions)
}

// Expander walks weak edges over a cluster store.
type Expander struct {
	Edges    Store
	Clusters cluster.Store
	// HopTS, when > 0 together with Freshness, prunes neighbors whose linked
	// source is staler than this (D4 optional freshness pass 时序剪枝).
	// Clusters without a resolvable source are never pruned (阙疑不剪).
	HopTS     time.Duration
	Freshness func(sourceID string) (time.Time, bool)
}

func NewExpander(es Store, cs cluster.Store) *Expander {
	return &Expander{Edges: es, Clusters: cs}
}

// Expand runs bounded BFS. Empty neighborhood returns nil, nil — callers fall
// back to L0 (门 C: 空图回落).
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

	for depth := 1; depth <= req.MaxDepth && len(frontier) > 0; depth++ {
		var next []node
		for _, n := range frontier {
			edges, err := e.edgesFrom(ctx, n.id, req.Direction)
			if err != nil {
				return nil, err
			}
			// Collect candidates then hopKNN-prune this expansion step.
			type cand struct {
				to    string
				edge  Edge
				score float64
				via   []string
			}
			var cands []cand
			for _, ed := range edges {
				if ed.Weight < req.MinWeight {
					continue
				}
				if seen[ed.To] {
					continue
				}
				cands = append(cands, cand{to: ed.To, edge: ed, score: n.score + ed.Weight, via: append(append([]string{}, n.via...), ed.ID)})
			}
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
				for i, s := range ss {
					if i < req.HopKNN {
						cands = []cand{} // rebuild
						break
					}
					_ = s
				}
				// Rebuild kept list.
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
		// Reverse: neighbor is the From side.
		for _, ed := range es {
			out = append(out, Edge{ID: ed.ID, From: ed.To, To: ed.From, Weight: ed.Weight, Source: ed.Source})
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
// backfill in P2/P3; also callable after save).
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
func LinkQuerySeq(ctx context.Context, st Store, from, to string) error {
	if from == "" || to == "" || from == to {
		return nil
	}
	return st.Save(ctx, Edge{From: from, To: to, Weight: 0.6, Source: SourceQuerySeq})
}

// LinkCoOcur records that two clusters shared evidence/source (co-mention).
func LinkCoOcur(ctx context.Context, st Store, a, b string) error {
	if a == "" || b == "" || a == b {
		return nil
	}
	from, to := a, b
	if from > to {
		from, to = to, from
	}
	return st.Save(ctx, Edge{From: from, To: to, Weight: 0.7, Source: SourceCoOcur})
}

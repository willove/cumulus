package graph

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/storedoc"
)

// CumuStore persists weak edges in clus_weak_edges and caches adjacency in
// process. Two facts make the cache sound:
//
//   - the engine takes an EXCLUSIVE directory lock, so one process is the
//     only writer of a store — no cross-process staleness is possible;
//   - NewCumuStore memoizes per (engine, collection), so every writer and
//     reader in the process shares ONE cache — the per-request search stacks
//     (newSearchStack builds one per query) get a process-lifetime cache
//     instead of a per-query one, and a Save/Delete anywhere invalidates
//     exactly the cache everyone reads.
//
// Bounded at maxCachedEdges; past the bound queries fall through to the
// engine (correct, just uncached).
type CumuStore struct {
	c    cumulite.Port
	coll string

	mu     sync.RWMutex
	from   map[string][]Edge // _from -> outbound edges
	to     map[string][]Edge // _to -> inbound edges
	cached int               // edges held in from (the to view mirrors them)
}

const maxCachedEdges = 100_000

// storeRegistry memoizes instances: any(port) -> *sync.Map(coll -> store).
var storeRegistry sync.Map

// NewCumuStore returns the process-wide store for (engine, collection) —
// see the type comment for why construction is memoized.
func NewCumuStore(c cumulite.Port, coll string) *CumuStore {
	if coll == "" {
		coll = "clus_weak_edges"
	}
	innerAny, _ := storeRegistry.LoadOrStore(c, &sync.Map{})
	inner := innerAny.(*sync.Map)
	if s, ok := inner.Load(coll); ok {
		return s.(*CumuStore)
	}
	s := &CumuStore{c: c, coll: coll}
	// Declare the collection's canonical shape (cumulite ShapePort) so any
	// non-typed write to this collection is audited against Edge's tags.
	storedoc.DeclareShape(context.Background(), c, coll, Edge{})
	actual, _ := inner.LoadOrStore(coll, s)
	return actual.(*CumuStore)
}

// Save writes through cumulite's typed path (StructPort) when available and
// a tag-derived document otherwise — never a hand-maintained field table,
// which is what used to drop new Edge fields silently. The write timestamp
// rides as a real struct field (TS) so the stored shape is unchanged.
func (s *CumuStore) Save(ctx context.Context, e Edge) error {
	if e.ID == "" {
		e.ID = edgeIDWithKind(e.From, e.To, e.Source, e.Kind)
	}
	e.TS = time.Now().UTC()
	exists := false
	if existing, err := s.c.GetDocument(ctx, s.coll, e.ID); err == nil && existing != nil {
		exists = true
	}
	if err := storedoc.WriteStruct(ctx, s.c, s.coll, e.ID, e, exists); err != nil {
		return err
	}
	s.invalidate(e.From, e.To)
	return nil
}

// From returns e's outbound edges (cache-first).
func (s *CumuStore) From(ctx context.Context, id string) ([]Edge, error) {
	if edges, ok := s.cachedEdges("from", id); ok {
		return edges, nil
	}
	edges, err := s.queryBy(ctx, "_from", id)
	if err != nil {
		return nil, err
	}
	s.admit("from", id, edges)
	return edges, nil
}

// To returns e's inbound edges (cache-first).
func (s *CumuStore) To(ctx context.Context, id string) ([]Edge, error) {
	if edges, ok := s.cachedEdges("to", id); ok {
		return edges, nil
	}
	edges, err := s.queryBy(ctx, "_to", id)
	if err != nil {
		return nil, err
	}
	s.admit("to", id, edges)
	return edges, nil
}

// All is the unfiltered edge list (maintenance faces; not cached). Paginated:
// a hardcoded single page silently truncated at the design's weak-edge ceiling
// (≤10⁴), so embed_sim backfill and the tidy sweep saw a partial graph.
func (s *CumuStore) All(ctx context.Context) ([]Edge, error) {
	const page = 1000
	var out []Edge
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: page, Skip: skip})
		if err != nil {
			return nil, err
		}
		out = append(out, docsToEdges(res.Documents)...)
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

func (s *CumuStore) Delete(ctx context.Context, id string) error {
	// Learn the endpoints first so invalidation drops exactly the two
	// affected adjacency views; an unreadable doc drops the whole cache
	// (maintenance path — rare, and correctness beats warmth).
	var from, to string
	if doc, err := s.c.GetDocument(ctx, s.coll, id); err == nil && doc != nil {
		from, to = str(doc["_from"]), str(doc["_to"])
	}
	ok, err := s.c.DeleteDocument(ctx, s.coll, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("edge %s not found", id)
	}
	if from != "" {
		s.invalidate(from, to)
		return nil
	}
	s.mu.Lock()
	s.from = map[string][]Edge{}
	s.to = map[string][]Edge{}
	s.cached = 0
	s.mu.Unlock()
	return nil
}

// queryBy returns every edge with field == id, paginated. A single page of 500
// used to truncate a hub cluster's adjacency silently — and since startBarriers
// and every Expand hop read this view, a barrier edge past the cap was missed
// and the contested neighbour came back as evidence (the H4 hole, at scale).
func (s *CumuStore) queryBy(ctx context.Context, field, id string) ([]Edge, error) {
	const page = 500
	var out []Edge
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.coll, contract.Query{
			Filter: map[string]any{field: id},
			Limit:  page, Skip: skip,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, docsToEdges(res.Documents)...)
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

// cachedEdges copies a hit under the read lock so callers own their slice
// and no map read races a concurrent write.
func (s *CumuStore) cachedEdges(dir, id string) ([]Edge, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hit []Edge
	var ok bool
	if dir == "from" {
		hit, ok = s.from[id]
	} else {
		hit, ok = s.to[id]
	}
	if !ok {
		return nil, false
	}
	out := make([]Edge, len(hit))
	copy(out, hit)
	return out, true
}

// admit inserts a query result. The to view mirrors the same edges, so the
// bound counts them once.
func (s *CumuStore) admit(dir, id string, edges []Edge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached > maxCachedEdges {
		return // bounded: fall through to the engine past the cap
	}
	if s.from == nil {
		s.from = map[string][]Edge{}
		s.to = map[string][]Edge{}
	}
	if dir == "from" {
		if old, ok := s.from[id]; ok {
			s.cached -= len(old)
		}
		s.from[id] = edges
		s.cached += len(edges)
		return
	}
	s.to[id] = edges
}

// invalidate drops the two adjacency views a write to (from → to) touches.
func (s *CumuStore) invalidate(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.from[from]; ok {
		s.cached -= len(old)
		delete(s.from, from)
	}
	delete(s.to, from)
	delete(s.to, to)
	if to != from {
		delete(s.from, to)
	}
}

func docsToEdges(docs []map[string]any) []Edge {
	out := make([]Edge, 0, len(docs))
	for _, d := range docs {
		e := Edge{ID: str(d["_id"]), From: str(d["_from"]), To: str(d["_to"]), Source: str(d["source"])}
		e.Kind = str(d["kind"])
		e.Reason = str(d["reason"])
		if h, ok := d["hits"].(float64); ok {
			e.Hits = int(h)
		}
		switch w := d["weight"].(type) {
		case float64:
			e.Weight = w
		case int:
			e.Weight = float64(w)
		}
		out = append(out, e)
	}
	sortEdges(out)
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

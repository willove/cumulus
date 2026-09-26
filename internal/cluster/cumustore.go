package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/storedoc"
)

// CumuStore persists clusters in a store collection (default clus_clusters).
type CumuStore struct {
	c    cumulite.Port
	coll string
	// mu serializes insert-or-replace against delete, so a delete can never
	// interleave with the read-then-write pair and have a removed cluster
	// resurrected by the write half. Callers hold kb.Engine's write lock for
	// the fold decision; this one guards the store primitive.
	mu sync.Mutex
}

func NewCumuStore(c cumulite.Port, coll string) *CumuStore {
	if coll == "" {
		coll = "clus_clusters"
	}
	s := &CumuStore{c: c, coll: coll}
	// Declare the collection's canonical shape (cumulite ShapePort): the
	// zero Cluster's json tags. Writes that reach storage through any
	// other path (a patch, a future map) are then audited against the
	// struct instead of trusted.
	storedoc.DeclareShape(context.Background(), c, coll, Cluster{})
	return s
}

// Save writes the cluster through cumulite's typed path (StructPort) when
// the engine offers it, and through a doc DERIVED from the struct's json
// tags otherwise — never through a hand-maintained field table. The table
// this replaced is exactly where the C2 judge stamp vanished: a field
// added to Cluster was dropped by the table while the in-memory store
// (whole-struct marshal) kept it, and nothing on the write path errored.
func (s *CumuStore) Save(ctx context.Context, c Cluster) error {
	// Insert-or-replace by _id (content-stable id) — but never silently
	// across identities: two topic_keys colliding on one id must be loud,
	// or the second Save quietly eats the first cluster.
	s.mu.Lock()
	defer s.mu.Unlock()
	exists := false
	if existing, err := s.c.GetDocument(ctx, s.coll, c.ID); err == nil && existing != nil {
		if tk, _ := existing["topic_key"].(string); tk != "" && c.TopicKey != "" && tk != c.TopicKey {
			return fmt.Errorf("cluster %s already holds topic_key %s, refusing to overwrite with %s", c.ID, tk, c.TopicKey)
		}
		exists = true
	}
	return storedoc.WriteStruct(ctx, s.c, s.coll, c.ID, c, exists)
}

func (s *CumuStore) Get(ctx context.Context, id string) (*Cluster, error) {
	d, err := s.c.GetDocument(ctx, s.coll, id)
	if err != nil {
		if contract.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return fromDoc(d)
}

func (s *CumuStore) FindByTopic(ctx context.Context, topicKey string) ([]Cluster, error) {
	res, err := s.c.Query(ctx, s.coll, contract.Query{
		Filter: map[string]any{"$or": []map[string]any{
			{"topic_key": topicKey},
			{"topic_keys": map[string]any{"$in": []string{topicKey}}},
		}},
		Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(res.Documents))
	for _, d := range res.Documents {
		c, err := fromDoc(d)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// All returns every cluster in the collection, paginated. The limit used to be
// a hardcoded 1000 — exactly the design's scale ceiling (clusters ≤10³) — so a
// full population silently truncated and maintenance faces (tidy, embed_sim
// backfill, the scoreboard) operated on a partial view.
func (s *CumuStore) All(ctx context.Context) ([]Cluster, error) {
	const page = 1000
	var out []Cluster
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: page, Skip: skip})
		if err != nil {
			return nil, err
		}
		for _, d := range res.Documents {
			c, err := fromDoc(d)
			if err != nil {
				return nil, err
			}
			out = append(out, *c)
		}
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

func (s *CumuStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, err := s.c.DeleteDocument(ctx, s.coll, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cluster %s not found", id)
	}
	return nil
}

func fromDoc(d map[string]any) (*Cluster, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decoding cluster: %w", err)
	}
	if c.ID == "" {
		if id, ok := d["_id"].(string); ok {
			c.ID = id
		}
	}
	return &c, nil
}

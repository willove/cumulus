package deep

import (
	"context"
	"strconv"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// CumuCiteStore persists cluster → source evidence edges (clus_cites).
type CumuCiteStore struct {
	c    cumulite.Port
	coll string
}

func NewCumuCiteStore(c cumulite.Port, coll string) *CumuCiteStore {
	if coll == "" {
		coll = "clus_cites"
	}
	return &CumuCiteStore{c: c, coll: coll}
}

// SaveCite records one citation edge; idempotent by (cluster, source, window).
func (s *CumuCiteStore) SaveCite(ctx context.Context, clusterID, sourceID string, start, end int, score float64) error {
	id := "cite:" + clusterID + ":" + sourceID + ":" + strconv.Itoa(start) + ":" + strconv.Itoa(end)
	doc := map[string]any{
		"_id":      id,
		"_from":    clusterID,
		"_to":      sourceID,
		"start":    start,
		"end":      end,
		"score":    score,
		"kind":     "evidence",
		"recorded": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if existing, err := s.c.GetDocument(ctx, s.coll, id); err == nil && existing != nil {
		_, err := s.c.ReplaceDocument(ctx, s.coll, id, doc)
		return err
	}
	_, err := s.c.Insert(ctx, s.coll, []map[string]any{doc})
	return err
}

// List returns all cite edges (read face for CLI/assertions). Paginated: a
// single hardcoded page silently truncated the evidence-edge read face.
func (s *CumuCiteStore) List(ctx context.Context) ([]map[string]any, error) {
	const page = 1000
	var out []map[string]any
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: page, Skip: skip})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Documents...)
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

// CumuStore persists conflict edges in a store collection (clus_conflicts).
type CumuStore struct {
	c    cumulite.Port
	coll string
}

func NewCumuStore(c cumulite.Port, coll string) *CumuStore {
	if coll == "" {
		coll = "clus_conflicts"
	}
	return &CumuStore{c: c, coll: coll}
}

func (s *CumuStore) Save(ctx context.Context, c Conflict) error {
	if c.ID == "" {
		c.ID = "x:" + c.A + "-" + c.B + "-" + c.Group
	}
	doc := map[string]any{
		"_id":    c.ID,
		"a":      c.A,
		"b":      c.B,
		"group":  c.Group,
		"reason": c.Reason,
		"saved":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if existing, err := s.c.GetDocument(ctx, s.coll, c.ID); err == nil && existing != nil {
		_, err := s.c.ReplaceDocument(ctx, s.coll, c.ID, doc)
		return err
	}
	_, err := s.c.Insert(ctx, s.coll, []map[string]any{doc})
	return err
}

func (s *CumuStore) Between(ctx context.Context, a, b string) ([]Conflict, error) {
	all, err := s.All(ctx)
	if err != nil {
		return nil, err
	}
	var out []Conflict
	for _, c := range all {
		if (c.A == a && c.B == b) || (c.A == b && c.B == a) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *CumuStore) All(ctx context.Context) ([]Conflict, error) {
	const page = 1000
	out := make([]Conflict, 0)
	for skip := 0; ; skip += page {
		res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: page, Skip: skip})
		if err != nil {
			return nil, err
		}
		for _, d := range res.Documents {
			out = append(out, Conflict{
				ID:     str(d["_id"]),
				A:      str(d["a"]),
				B:      str(d["b"]),
				Group:  str(d["group"]),
				Reason: str(d["reason"]),
			})
		}
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

var _ ConflictStore = (*CumuStore)(nil)

package deep

import (
	"context"
	"strconv"
	"time"

	"github.com/cumubase/cumudb/pkg/client"
)

// CumuCiteStore persists cluster → source evidence edges (ask_cites).
type CumuCiteStore struct {
	c    *client.Client
	coll string
}

func NewCumuCiteStore(c *client.Client, coll string) *CumuCiteStore {
	if coll == "" {
		coll = "ask_cites"
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

// List returns all cite edges (read face for CLI/assertions).
func (s *CumuCiteStore) List(ctx context.Context) ([]map[string]any, error) {
	res, err := s.c.Query(ctx, s.coll, client.Query{Limit: 1000})
	if err != nil {
		return nil, err
	}
	return res.Documents, nil
}

// CumuStore persists conflict edges in a cumudb collection (ask_conflicts).
type CumuStore struct {
	c    *client.Client
	coll string
}

func NewCumuStore(c *client.Client, coll string) *CumuStore {
	if coll == "" {
		coll = "ask_conflicts"
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
	res, err := s.c.Query(ctx, s.coll, client.Query{Limit: 1000})
	if err != nil {
		return nil, err
	}
	out := make([]Conflict, 0, len(res.Documents))
	for _, d := range res.Documents {
		c := Conflict{
			ID:     str(d["_id"]),
			A:      str(d["a"]),
			B:      str(d["b"]),
			Group:  str(d["group"]),
			Reason: str(d["reason"]),
		}
		out = append(out, c)
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

var _ ConflictStore = (*CumuStore)(nil)

package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// CumuStore persists weak edges in ask_weak_edges.
type CumuStore struct {
	c    cumulite.Port
	coll string
}

func NewCumuStore(c cumulite.Port, coll string) *CumuStore {
	if coll == "" {
		coll = "ask_weak_edges"
	}
	return &CumuStore{c: c, coll: coll}
}

func (s *CumuStore) Save(ctx context.Context, e Edge) error {
	if e.ID == "" {
		e.ID = edgeID(e.From, e.To, e.Source)
	}
	doc := map[string]any{
		"_id":    e.ID,
		"_from":  e.From,
		"_to":    e.To,
		"weight": e.Weight,
		"source": e.Source,
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if existing, err := s.c.GetDocument(ctx, s.coll, e.ID); err == nil && existing != nil {
		_, err := s.c.ReplaceDocument(ctx, s.coll, e.ID, doc)
		return err
	}
	_, err := s.c.Insert(ctx, s.coll, []map[string]any{doc})
	return err
}

func (s *CumuStore) From(ctx context.Context, id string) ([]Edge, error) {
	res, err := s.c.Query(ctx, s.coll, contract.Query{
		Filter: map[string]any{"_from": id},
		Limit:  500,
	})
	if err != nil {
		return nil, err
	}
	return docsToEdges(res.Documents), nil
}

func (s *CumuStore) To(ctx context.Context, id string) ([]Edge, error) {
	res, err := s.c.Query(ctx, s.coll, contract.Query{
		Filter: map[string]any{"_to": id},
		Limit:  500,
	})
	if err != nil {
		return nil, err
	}
	return docsToEdges(res.Documents), nil
}

func (s *CumuStore) All(ctx context.Context) ([]Edge, error) {
	res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: 1000})
	if err != nil {
		return nil, err
	}
	return docsToEdges(res.Documents), nil
}

func (s *CumuStore) Delete(ctx context.Context, id string) error {
	ok, err := s.c.DeleteDocument(ctx, s.coll, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("edge %s not found", id)
	}
	return nil
}

func docsToEdges(docs []map[string]any) []Edge {
	out := make([]Edge, 0, len(docs))
	for _, d := range docs {
		e := Edge{ID: str(d["_id"]), From: str(d["_from"]), To: str(d["_to"]), Source: str(d["source"])}
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

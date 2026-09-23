// Package cumuport adapts the CumuDB HTTP client to the lite engine's Port
// contract.
//
// The two engines deliberately do not share code anymore: cumulite forked the
// wire contract into its own package so neither project depends on the other.
// The cost of that separation is nominal typing — *client.Client's Query is not
// contract's Query — and this package pays it in one place instead of at every
// call site. ask's default path stays the HTTP client; the lite path stays the
// embedded engine; the code between them only ever sees Port.
package cumuport

import (
	"context"
	"time"

	"github.com/willove/cumudb/pkg/client"
	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// New returns the HTTP client as a Port, converting request and result types
// at the boundary.
func New(c *client.Client) cumulite.Port {
	return clientPort{c}
}

type clientPort struct{ *client.Client }

func (p clientPort) Health(ctx context.Context) (contract.Health, error) {
	h, err := p.Client.Health(ctx)
	return contract.Health{
		Status: h.Status, Version: h.Version, Backend: h.Backend,
		InMemory: h.InMemory, UptimeSec: h.UptimeSec,
	}, err
}

func (p clientPort) Insert(ctx context.Context, coll string, documents []map[string]any) ([]string, error) {
	return p.Client.Insert(ctx, coll, documents)
}

func (p clientPort) GetDocument(ctx context.Context, coll, id string) (map[string]any, error) {
	return p.Client.GetDocument(ctx, coll, id)
}

func (p clientPort) ReplaceDocument(ctx context.Context, coll, id string, document map[string]any) (map[string]any, error) {
	return p.Client.ReplaceDocument(ctx, coll, id, document)
}

func (p clientPort) PatchDocument(ctx context.Context, coll, id string, update map[string]any) (map[string]any, error) {
	return p.Client.PatchDocument(ctx, coll, id, update)
}

func (p clientPort) DeleteDocument(ctx context.Context, coll, id string) (bool, error) {
	return p.Client.DeleteDocument(ctx, coll, id)
}

func (p clientPort) Query(ctx context.Context, coll string, q contract.Query) (*contract.QueryResult, error) {
	res, err := p.Client.Query(ctx, coll, client.Query{
		Filter: q.Filter, Sort: q.Sort, Projection: q.Projection, Skip: q.Skip, Limit: q.Limit,
	})
	if err != nil {
		return nil, err
	}
	return &contract.QueryResult{
		Documents: res.Documents, Count: res.Count, Plan: res.Plan,
		Examined: res.Examined, Matched: res.Matched, Sorted: res.Sorted,
		Skip: res.Skip, Limit: res.Limit,
	}, nil
}

func (p clientPort) KVPut(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return p.Client.KVPut(ctx, key, value, ttl)
}

func (p clientPort) KVGet(ctx context.Context, key string) ([]byte, error) {
	return p.Client.KVGet(ctx, key)
}

func (p clientPort) KVDelete(ctx context.Context, key string) (bool, error) {
	return p.Client.KVDelete(ctx, key)
}

func (p clientPort) KVKeys(ctx context.Context, prefix string, limit int) ([]string, error) {
	return p.Client.KVKeys(ctx, prefix, limit)
}

func (p clientPort) CreateIndexRequest(ctx context.Context, coll string, request contract.IndexRequest) error {
	return p.Client.CreateIndexRequest(ctx, coll, client.IndexRequest{
		Name: request.Name, Field: request.Field, Fields: request.Fields,
		Unique: request.Unique, Include: request.Include, Type: request.Type,
		Dims: request.Dims, Metric: request.Metric, Model: request.Model,
		Ann: request.Ann, NList: request.NList, NProbe: request.NProbe,
		Hnsw: request.Hnsw, M: request.M, EfConstruction: request.EfConstruction,
		EfSearch: request.EfSearch, Tokenizer: request.Tokenizer, K1: request.K1,
		B: request.B, Precision: request.Precision, Datum: request.Datum,
	})
}

func (p clientPort) KNN(ctx context.Context, coll string, request contract.KNNRequest) (*contract.KNNResult, error) {
	res, err := p.Client.KNN(ctx, coll, client.KNNRequest{
		Field: request.Field, Vector: request.Vector, K: request.K,
		Metric: request.Metric, Filter: request.Filter, Index: request.Index,
	})
	if err != nil {
		return nil, err
	}
	return &contract.KNNResult{
		Documents: res.Documents, Distances: res.Distances, Count: res.Count,
		Field: res.Field, Metric: res.Metric, Plan: res.Plan,
		Examined: res.Examined, Matched: res.Matched,
	}, nil
}

func (p clientPort) SetChangelog(ctx context.Context, coll string, enabled bool) error {
	return p.Client.SetChangelog(ctx, coll, enabled)
}

func (p clientPort) Changes(ctx context.Context, coll string, cursor uint64, limit int) (*contract.ChangesPage, error) {
	page, err := p.Client.Changes(ctx, coll, cursor, limit)
	if err != nil {
		return nil, err
	}
	out := &contract.ChangesPage{Count: page.Count, Cursor: page.Cursor, Enabled: page.Enabled}
	for _, rec := range page.Changes {
		out.Changes = append(out.Changes, contract.ChangeRecord{
			Sequence: rec.Sequence, Op: rec.Op, ID: rec.ID,
			Created: rec.Created, At: rec.At,
		})
	}
	return out, nil
}

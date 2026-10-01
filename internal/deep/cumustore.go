package deep

import (
	"context"
	"strconv"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/storedoc"
)

// citeDoc is the stored shape of one citation edge — the same keys the
// hand-maintained table used to write, now a struct so the typed path and
// the shape audit speak one vocabulary.
type citeDoc struct {
	ID       string    `json:"_id"`
	From     string    `json:"_from"`
	To       string    `json:"_to"`
	Start    int       `json:"start"`
	End      int       `json:"end"`
	Score    float64   `json:"score"`
	Kind     string    `json:"kind"`
	Recorded time.Time `json:"recorded"`
}

// CumuCiteStore persists cluster → source evidence edges (clus_cites).
type CumuCiteStore struct {
	c    cumulite.Port
	coll string
}

func NewCumuCiteStore(c cumulite.Port, coll string) *CumuCiteStore {
	if coll == "" {
		coll = "clus_cites"
	}
	storedoc.DeclareShape(context.Background(), c, coll, citeDoc{})
	return &CumuCiteStore{c: c, coll: coll}
}

// SaveCite records one citation edge; idempotent by (cluster, source, window).
func (s *CumuCiteStore) SaveCite(ctx context.Context, clusterID, sourceID string, start, end int, score float64) error {
	id := "cite:" + clusterID + ":" + sourceID + ":" + strconv.Itoa(start) + ":" + strconv.Itoa(end)
	doc := citeDoc{
		ID: id, From: clusterID, To: sourceID,
		Start: start, End: end, Score: score, Kind: "evidence",
		Recorded: time.Now().UTC(),
	}
	exists := false
	if existing, err := s.c.GetDocument(ctx, s.coll, id); err == nil && existing != nil {
		exists = true
	}
	return storedoc.WriteStruct(ctx, s.c, s.coll, id, doc, exists)
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
	storedoc.DeclareShape(context.Background(), c, coll, Conflict{})
	return &CumuStore{c: c, coll: coll}
}

func (s *CumuStore) Save(ctx context.Context, c Conflict) error {
	if c.ID == "" {
		c.ID = "x:" + c.A + "-" + c.B + "-" + c.Group
	}
	c.Saved = time.Now().UTC()
	exists := false
	if existing, err := s.c.GetDocument(ctx, s.coll, c.ID); err == nil && existing != nil {
		exists = true
	}
	return storedoc.WriteStruct(ctx, s.c, s.coll, c.ID, c, exists)
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
				Saved:  stamp(d["saved"]),
			})
		}
		if len(res.Documents) < page {
			return out, nil
		}
	}
}

// stamp reads back a write timestamp. Save stamps c.Saved on every write and
// the field is documented as surfaced by the read faces, but All never read
// it — so every conflict came back with a zero Saved no matter how it was
// persisted. The engine may hand the value back as a time.Time (in-process
// store) or as an RFC3339 string (serialised doc), so accept both.
func stamp(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		if t == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

var _ ConflictStore = (*CumuStore)(nil)

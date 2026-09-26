// Package bucket is the corpus-partition layer on top of the namespace
// mechanism. A bucket is a NAMED namespace plus a registry entry, and every
// retrieval face requires the caller to name one.
//
// Why this exists on top of -ns rather than instead of it: the namespace is the
// engine-level isolation unit (every collection and KV key the suite owns is
// composed through ns.Coll / ns.KV), and it already guarantees that one
// namespace's reuse path can never see another's. What it does NOT give you is
// (a) a durable, discoverable list of the partitions an operator has made, or
// (b) a refusal when nobody named one. Without (b) a chat query silently lands
// in the default library and reads whatever happened to be ingested there —
// cross-topic contamination with no signal.
//
// So: buckets ARE namespaces (same composite identity, same isolation), with a
// registry for enumeration and a required-selection rule at the query faces.
package bucket

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/ns"
)

// RegistryPrefix is the KV prefix a bucket's own record lives under. The bucket
// list itself is one KV document so enumeration does not scan the keyspace.
const (
	RegistryPrefix = "clus:bucket:"
	IndexKey       = "clus:buckets"
)

// Bucket is one named corpus partition. Counters are maintained by the faces
// that write (ingest) and query (search) so an operator can see what is in a
// bucket without counting documents by hand.
type Bucket struct {
	Name      string    `json:"name"`
	Label     string    `json:"label,omitempty"` // human-facing name (defaults to Name)
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastQueryAt is when a retrieval face last served from this bucket. It is
	// the signal for "which bucket is actually being used" — an unused bucket
	// is dead weight, an over-used one is a contamination candidate.
	LastQueryAt time.Time `json:"last_query_at,omitempty"`
	Sources     int       `json:"sources"`
	Clusters    int       `json:"clusters"`
	Queries     int64     `json:"queries"`
}

// Store is the bucket registry over a cumulite Port.
type Store struct {
	c cumulite.Port
}

func New(c cumulite.Port) *Store { return &Store{c: c} }

// Create registers a bucket, or returns the existing one unchanged (idempotent:
// re-creating must not wipe counters or reset CreatedAt).
func (s *Store) Create(ctx context.Context, name, label, note string) (Bucket, error) {
	if err := ns.Validate(name); err != nil {
		return Bucket{}, err
	}
	if name == "" {
		return Bucket{}, fmt.Errorf("bucket: name required")
	}
	existing, err := s.Get(ctx, name)
	if err != nil {
		return Bucket{}, err
	}
	now := time.Now().UTC()
	if existing != nil {
		b := *existing
		// Only fill in what the caller supplied; never reset counters.
		if label != "" {
			b.Label = label
		}
		if note != "" {
			b.Note = note
		}
		b.UpdatedAt = now
		if err := s.put(ctx, b); err != nil {
			return Bucket{}, err
		}
		return b, nil
	}
	b := Bucket{
		Name: name, Label: firstNonEmpty(label, name), Note: note,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.put(ctx, b); err != nil {
		return Bucket{}, err
	}
	if err := s.addToIndex(ctx, name); err != nil {
		return Bucket{}, err
	}
	return b, nil
}

// Get returns one bucket, or nil when it is not registered.
func (s *Store) Get(ctx context.Context, name string) (*Bucket, error) {
	if err := ns.Validate(name); err != nil {
		return nil, err
	}
	raw, err := s.c.KVGet(ctx, RegistryPrefix+name)
	if err != nil {
		if contract.IsNotFound(err) {
			return nil, nil // missing key is not an error: "not registered"
		}
		// A real read failure must NOT fold into "not registered": Create
		// would then register a fresh zero bucket over the existing one and
		// wipe its counters on the next put.
		return nil, fmt.Errorf("bucket %s: %w", name, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var b Bucket
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("bucket %s: %w", name, err)
	}
	return &b, nil
}

// List returns every registered bucket, name-sorted for a stable UI.
func (s *Store) List(ctx context.Context) ([]Bucket, error) {
	raw, err := s.c.KVGet(ctx, IndexKey)
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("bucket index: %w", err)
	}
	sort.Strings(names)
	out := make([]Bucket, 0, len(names))
	for _, n := range names {
		b, err := s.Get(ctx, n)
		if err != nil {
			return nil, err
		}
		if b == nil {
			continue // index is a hint; the record is the truth
		}
		out = append(out, *b)
	}
	return out, nil
}

// Remove drops a bucket's registry entry. It deliberately does NOT delete the
// namespace's collections: "保留是决策不是副作用" — dropping the registry entry
// makes the bucket unselectable, and reclaiming its data stays an explicit
// operator action.
func (s *Store) Remove(ctx context.Context, name string) error {
	if err := ns.Validate(name); err != nil {
		return err
	}
	if _, err := s.c.KVDelete(ctx, RegistryPrefix+name); err != nil {
		return err
	}
	return s.removeFromIndex(ctx, name)
}

// Touch records a query against the bucket and refreshes its counters.
func (s *Store) Touch(ctx context.Context, name string, sources, clusters int) error {
	b, err := s.Get(ctx, name)
	if err != nil || b == nil {
		return err
	}
	now := time.Now().UTC()
	b.LastQueryAt = now
	b.UpdatedAt = now
	b.Queries++
	if sources >= 0 {
		b.Sources = sources
	}
	if clusters >= 0 {
		b.Clusters = clusters
	}
	return s.put(ctx, *b)
}

// SetCounts records the corpus/cluster sizes observed at ingest time.
func (s *Store) SetCounts(ctx context.Context, name string, sources, clusters int) error {
	b, err := s.Get(ctx, name)
	if err != nil || b == nil {
		return err
	}
	if sources >= 0 {
		b.Sources = sources
	}
	if clusters >= 0 {
		b.Clusters = clusters
	}
	b.UpdatedAt = time.Now().UTC()
	return s.put(ctx, *b)
}

func (s *Store) put(ctx context.Context, b Bucket) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, RegistryPrefix+b.Name, raw, 0)
}

func (s *Store) addToIndex(ctx context.Context, name string) error {
	names, err := s.index(ctx)
	if err != nil {
		return err
	}
	for _, n := range names {
		if n == name {
			return nil
		}
	}
	names = append(names, name)
	return s.writeIndex(ctx, names)
}

func (s *Store) removeFromIndex(ctx context.Context, name string) error {
	names, err := s.index(ctx)
	if err != nil {
		return err
	}
	out := names[:0]
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	return s.writeIndex(ctx, out)
}

func (s *Store) index(ctx context.Context) ([]string, error) {
	raw, err := s.c.KVGet(ctx, IndexKey)
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	return names, nil
}

func (s *Store) writeIndex(ctx context.Context, names []string) error {
	raw, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, IndexKey, raw, 0)
}

// Require is the query-face gate: an empty or unregistered bucket is REFUSED
// rather than silently falling back to the default library. That fallback is
// the contamination the bucket mechanism exists to prevent — a chat query with
// no bucket named would read whatever happened to be ingested into the default
// namespace, across topics, with no signal that it did.
func (s *Store) Require(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("bucket: a bucket must be named (see `cumulus-cluster bucket list`)")
	}
	b, err := s.Get(ctx, name)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("bucket %q is not registered (see `cumulus-cluster bucket list`)", name)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

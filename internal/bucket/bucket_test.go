package bucket

import (
	"context"
	"errors"
	"testing"

	"github.com/willove/cumulite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return New(engine)
}

func TestCreateIsIdempotentAndKeepsCounters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	b1, err := s.Create(ctx, "law", "法条", "cn-law-rag")
	if err != nil {
		t.Fatal(err)
	}
	if b1.Label != "法条" || b1.Name != "law" {
		t.Fatalf("create: %+v", b1)
	}
	if err := s.SetCounts(ctx, "law", 3932, 77); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, "law", 3932, 77); err != nil {
		t.Fatal(err)
	}
	// Re-create with different metadata: must not reset counters or CreatedAt.
	b2, err := s.Create(ctx, "law", "改个名", "")
	if err != nil {
		t.Fatal(err)
	}
	if b2.Sources != 3932 || b2.Clusters != 77 || b2.Queries != 1 {
		t.Fatalf("re-create wiped state: %+v", b2)
	}
	if !b2.CreatedAt.Equal(b1.CreatedAt) {
		t.Fatalf("re-create reset CreatedAt: %v vs %v", b2.CreatedAt, b1.CreatedAt)
	}
	if b2.Label != "改个名" {
		t.Fatalf("label should update: %q", b2.Label)
	}
	// An empty note must not overwrite an existing one.
	b3, err := s.Create(ctx, "law", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if b3.Note != "cn-law-rag" {
		t.Fatalf("empty note must not clobber: %q", b3.Note)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("re-create must not duplicate the index entry: %+v", list)
	}
}

func TestRemoveUnregistersButKeepsData(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Create(ctx, "tmp", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "tmp"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("removed bucket must not be registered: %+v", got)
	}
	list, _ := s.List(ctx)
	if len(list) != 0 {
		t.Fatalf("index must drop the name: %+v", list)
	}
	// Remove is idempotent.
	if err := s.Remove(ctx, "tmp"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestListIsSortedAndSkipsOrphans(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if _, err := s.Create(ctx, n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// An index entry whose record is gone must be skipped, not fatal.
	if err := s.addToIndex(ctx, "ghost"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("orphan index entry must be skipped: %+v", list)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("list must be name-sorted: %+v", list)
		}
	}
}

func TestInvalidNamesRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, bad := range []string{"", "..", ".x", "-x", "_x", "a:b", "a b"} {
		if _, err := s.Create(ctx, bad, "", ""); err == nil {
			t.Errorf("Create(%q) must fail", bad)
		}
		if _, err := s.Get(ctx, bad); err == nil && bad != "" {
			t.Errorf("Get(%q) must fail", bad)
		}
	}
}

// Requirement is the contract that enforces "you must name a bucket": it
// rejects an empty selection rather than silently falling back.
func TestRequireRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Create(ctx, "law", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Require(ctx, ""); err == nil {
		t.Fatal("an unnamed bucket must be refused, not defaulted")
	}
	if err := s.Require(ctx, "nope"); err == nil {
		t.Fatal("an unregistered bucket must be refused")
	}
	if err := s.Require(ctx, "law"); err != nil {
		t.Fatalf("a registered bucket must be accepted: %v", err)
	}
}

// failingKVPort fails KVGet like a transient storage outage.
type failingKVPort struct {
	cumulite.Port
}

func (p failingKVPort) KVGet(ctx context.Context, key string) ([]byte, error) {
	return nil, errors.New("storage unavailable")
}

// A registry read failure must NOT read as "not registered": Create would
// then write a fresh zero bucket over the live record and wipe its counters
// on the spot.
func TestCreateFailsLoudlyOnRegistryReadError(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	good := New(engine)
	if _, err := good.Create(ctx, "law", "法条", ""); err != nil {
		t.Fatal(err)
	}
	if err := good.SetCounts(ctx, "law", 3932, 77); err != nil {
		t.Fatal(err)
	}
	s := New(failingKVPort{Port: engine})
	if _, err := s.Create(ctx, "law", "改名", ""); err == nil {
		t.Fatal("a registry read failure must fail Create, not re-register a zero bucket")
	}
	// The live record survived untouched.
	kept, err := good.Get(ctx, "law")
	if err != nil || kept == nil {
		t.Fatalf("get after failed create: %v %v", kept, err)
	}
	if kept.Sources != 3932 || kept.Label != "法条" {
		t.Fatalf("failed create disturbed the live record: %+v", kept)
	}
}

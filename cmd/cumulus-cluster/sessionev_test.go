package main

import (
	"testing"
	"time"
)

// Threads are keyed by namespace+sessionID: source IDs are content-addressed,
// so two tenants ingesting the same statute share doc IDs — a bare sessionID
// would let one tenant's thread steer the other's ranking.
func TestSessionEvidenceIsNamespaced(t *testing.T) {
	s := &sessionEvidence{m: map[string]*sessionThread{}}
	now := time.Now()
	s.Add("tenant-a", "s1", []string{"src:x"}, "src:x", now)
	if w := s.Weights("tenant-a", "s1", now); w["src:x"] != 1.0 {
		t.Fatalf("same-namespace weights lost: %v", w)
	}
	if w := s.Weights("tenant-b", "s1", now); len(w) != 0 {
		t.Fatalf("cross-tenant leak through identical sessionID: %v", w)
	}
	// The default namespace ("" — the serve-level library) is its own tenant.
	s.Add("", "s1", []string{"src:x"}, "", now)
	if w := s.Weights("tenant-a", "s1", now); w["src:x"] != 1.0 {
		t.Fatalf("default-namespace write bled into tenant-a: %v", w)
	}
}

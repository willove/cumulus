package main

// Session-scoped evidence memory (L1). Within one conversation, the
// documents an earlier answer cited stay warm for the follow-ups: a
// "宠物伤人" question after "在家养宠物影响别人" starts its ranking with
// the documents that just served that thread instead of re-walking the
// whole corpus from zero.
//
// In-memory only, by design: the ledger (affinity) is the durable memory
// across sessions; this one is the fast, private one. A TTL keeps dead
// sessions from accumulating, and turn-distance decay keeps a thread from
// dragging a stale document forever.

import (
	"sync"
	"time"

	"github.com/willove/cumulus/internal/ns"
)

const (
	sessionEvidenceTTL     = 30 * time.Minute // idle lifetime of one thread's stack
	sessionEvidenceDecay   = 0.6              // per-turn multiplier
	sessionEvidenceFloor   = 0.1              // below this a doc is forgotten
	sessionEvidenceMaxDocs = 40               // per-thread cap
)

type sessionEvidence struct {
	mu sync.Mutex
	m  map[string]*sessionThread
}

type sessionThread struct {
	docs map[string]float64 // sourceID → decayed weight
	at   time.Time
}

var sessEvidence = &sessionEvidence{m: map[string]*sessionThread{}}

// Add ages the thread by one turn and stamps the freshly cited documents.
// anchor (the answer's primary document, "" when unknown) gets full weight;
// the secondary citations get 0.6 — the same evidence, ranked by role, so a
// follow-up promotes the document the answer actually led with.
//
// Threads are keyed ns.KV(namespace, sessionID): source IDs are
// content-addressed, so two tenants ingesting the same statute share doc IDs
// — a bare sessionID would let one tenant's thread steer the other's ranking
// and trip DEEP's session-bridge early exit on foreign evidence.
func (s *sessionEvidence) Add(namespace, sessionID string, docIDs []string, anchor string, now time.Time) {
	if sessionID == "" || len(docIDs) == 0 {
		return
	}
	sessionID = ns.KV(namespace, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.m[sessionID]
	if th == nil {
		th = &sessionThread{docs: map[string]float64{}}
		s.m[sessionID] = th
	}
	for id, w := range th.docs {
		w *= sessionEvidenceDecay
		if w < sessionEvidenceFloor {
			delete(th.docs, id)
			continue
		}
		th.docs[id] = w
	}
	for _, id := range docIDs {
		if id == "" {
			continue
		}
		if anchor != "" && id == anchor {
			th.docs[id] = 1.0
			continue
		}
		th.docs[id] = 0.6
	}
	if anchor != "" {
		if _, ok := th.docs[anchor]; !ok {
			th.docs[anchor] = 1.0
		}
	}
	if len(th.docs) > sessionEvidenceMaxDocs {
		trimSmallest(th.docs, sessionEvidenceMaxDocs)
	}
	th.at = now
}

// Weights returns the thread's live document weights (empty for unknown or
// idle-expired threads). Expired threads are dropped on read.
func (s *sessionEvidence) Weights(namespace, sessionID string, now time.Time) map[string]float64 {
	if sessionID == "" {
		return nil
	}
	sessionID = ns.KV(namespace, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	th := s.m[sessionID]
	if th == nil {
		return nil
	}
	if now.Sub(th.at) > sessionEvidenceTTL {
		delete(s.m, sessionID)
		return nil
	}
	out := make(map[string]float64, len(th.docs))
	for id, w := range th.docs {
		out[id] = w
	}
	return out
}

func trimSmallest(m map[string]float64, keep int) {
	type kv struct {
		id string
		w  float64
	}
	all := make([]kv, 0, len(m))
	for id, w := range m {
		all = append(all, kv{id, w})
	}
	// insertion sort by weight descending; maps here are ≤ ~40 entries
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].w > all[j-1].w; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for _, e := range all[keep:] {
		delete(m, e.id)
	}
}

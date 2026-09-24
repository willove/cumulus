package monitor

import (
	"testing"
	"time"
)

func rec(ns, mode string, reused bool, lat int64, conf float64) Query {
	return Query{At: time.Now().UTC(), Namespace: ns, Mode: mode, Reused: reused,
		LatencyMS: lat, Confidence: conf, Coverage: 0.5, LLMCalls: 2, Tokens: 100}
}

func TestSnapshotAggregates(t *testing.T) {
	tr := New()
	tr.Record(rec("law", "FAST", false, 300, 0.8))
	tr.Record(rec("law", "FAST", true, 5, 0.9))     // warm hit
	tr.Record(rec("ecom", "DEEP", false, 900, 0.4)) // cold, slow
	tr.Record(rec("law", "DEEP", true, 7, 0.9))     // warm hit in another ns
	s := tr.Snapshot(1234, "/tmp/store")

	if s.Queries != 4 {
		t.Fatalf("queries=%d", s.Queries)
	}
	if s.Retrieval.ByMode["FAST"] != 2 || s.Retrieval.ByMode["DEEP"] != 2 {
		t.Fatalf("by_mode=%v", s.Retrieval.ByMode)
	}
	if s.Retrieval.ReuseHits != 2 || s.Retrieval.ReuseRate != 0.5 {
		t.Fatalf("reuse=%d/%v", s.Retrieval.ReuseHits, s.Retrieval.ReuseRate)
	}
	// upper median: warm {5,7} → 7; cold {300,900} → 900
	if s.Retrieval.WarmP50MS != 7 {
		t.Fatalf("warm p50=%d", s.Retrieval.WarmP50MS)
	}
	if s.Retrieval.ColdP50MS != 900 {
		t.Fatalf("cold p50=%d", s.Retrieval.ColdP50MS)
	}
	if s.Retrieval.WarmCount != 2 || s.Retrieval.ColdCount != 2 {
		t.Fatalf("warm/cold counts: %d/%d", s.Retrieval.WarmCount, s.Retrieval.ColdCount)
	}
	if s.LLM.Calls != 8 || s.LLM.Tokens != 400 {
		t.Fatalf("llm=%+v", s.LLM)
	}
	if s.LLM.TokensPerQ != 100 {
		t.Fatalf("tokens per query=%v", s.LLM.TokensPerQ)
	}
	if len(s.Namespaces) != 2 {
		t.Fatalf("namespaces=%+v", s.Namespaces)
	}
	// busiest bucket first
	if s.Namespaces[0].Namespace != "law" {
		t.Fatalf("namespaces must be busiest-first: %+v", s.Namespaces)
	}
	for _, ns := range s.Namespaces {
		if ns.AvgP50US <= 0 {
			t.Fatalf("per-bucket p50 must be populated: %+v", ns)
		}
	}
	if s.System.StoreFilesBytes != 1234 || s.System.StoreDir != "/tmp/store" {
		t.Fatalf("system=%+v", s.System)
	}
	if len(s.Recent) != 4 {
		t.Fatalf("recent=%d", len(s.Recent))
	}
}

// The ring must stay bounded, and dropping old rows must not make the aggregate
// counters disagree with each other.
func TestRingIsBounded(t *testing.T) {
	tr := New()
	const n = maxQueries + 500
	for i := 0; i < n; i++ {
		tr.Record(rec("law", "FAST", i%2 == 0, int64(i), 0.5))
	}
	s := tr.Snapshot(0, "")
	if s.Queries > maxQueries {
		t.Fatalf("ring grew past the bound: %d", s.Queries)
	}
	sum := 0
	for _, c := range s.Retrieval.ByMode {
		sum += c
	}
	if sum != s.Queries {
		t.Fatalf("by_mode sums to %d but %d queries retained", sum, s.Queries)
	}
	if s.Retrieval.ReuseHits+s.Retrieval.ColdCount != s.Queries {
		t.Fatalf("reuse+cold=%d, queries=%d", s.Retrieval.ReuseHits+s.Retrieval.ColdCount, s.Queries)
	}
}

// A query that failed must be visible as an error, not silently absent.
func TestErrorsCounted(t *testing.T) {
	tr := New()
	q := rec("law", "FAST", false, 10, 0)
	q.Error = "boom"
	tr.Record(q)
	s := tr.Snapshot(0, "")
	if s.Retrieval.Errors != 1 {
		t.Fatalf("errors=%d", s.Retrieval.Errors)
	}
}

// p50 of an even-length list is the UPPER median (index len/2), which is the
// convention this package documents.
func TestP50EvenLength(t *testing.T) {
	if got := p50([]int64{10, 20, 30, 40}); got != 30 {
		t.Fatalf("p50=%d, want 30", got)
	}
	if got := p50([]int64{7}); got != 7 {
		t.Fatalf("p50 single=%d", got)
	}
	if got := p50(nil); got != 0 {
		t.Fatalf("p50 empty=%d", got)
	}
}

func TestConcurrentRecord(t *testing.T) {
	tr := New()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				tr.Record(rec("law", "FAST", false, 1, 0.5))
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	s := tr.Snapshot(0, "")
	if s.Queries != 1600 {
		t.Fatalf("lost records under concurrency: %d", s.Queries)
	}
}

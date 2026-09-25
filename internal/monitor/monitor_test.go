package monitor

import (
	"encoding/json"
	"strings"
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

// Counted, yes — and also kept OUT of the aggregates: a failed attempt arrives
// as a zero-value shape (no mode, 0 confidence, partial latency), and those
// zeros used to average straight into the headline numbers, biasing every
// Average/p50 the operator reads.
func TestErrorsExcludedFromAggregates(t *testing.T) {
	tr := New()
	tr.Record(rec("law", "FAST", false, 1000, 0.8))
	failed := rec("law", "", false, 50, 0)
	failed.Error = "boom"
	tr.Record(failed)
	s := tr.Snapshot(0, "")
	if s.Retrieval.Errors != 1 {
		t.Fatalf("errors=%d, want 1", s.Retrieval.Errors)
	}
	if s.Retrieval.AvgConf != 0.8 {
		t.Fatalf("avg conf = %.3f, want 0.8 (the failure's zero must not dilute)", s.Retrieval.AvgConf)
	}
	if s.Retrieval.AvgCov != 0.5 {
		t.Fatalf("avg cov = %.3f, want 0.5", s.Retrieval.AvgCov)
	}
	if s.Retrieval.ColdP50US != 1000*1000 { // rec fills LatencyMS; latencyUS() scales it
		t.Fatalf("cold p50 = %dµs, want 1000ms (the failed attempt's partial latency is not a retrieval time)", s.Retrieval.ColdP50US)
	}
	if _, ok := s.Retrieval.ByMode[""]; ok {
		t.Fatalf("a mode-less failure must not create an empty-mode bucket: %+v", s.Retrieval.ByMode)
	}
	if s.Queries != 2 {
		t.Fatalf("queries=%d, want 2 (failures still count as activity)", s.Queries)
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

// The knowledge block is attached by the API layer (the tracker owns no cluster
// store) and must round-trip into the snapshot.
func TestKnowledgeAttaches(t *testing.T) {
	tr := New()
	if s := tr.Snapshot(0, ""); s.Knowledge != nil {
		t.Fatal("no knowledge block before one is attached")
	}
	tr.WithKnowledge(&Knowledge{
		Clusters: 3, ByLifecycle: map[string]int{"stable": 2, "emerging": 1},
		AvgConfidence: 0.8, AvgHotness: 0.4, Contested: 1, NeedingReview: 1,
	})
	s := tr.Snapshot(0, "")
	if s.Knowledge == nil || s.Knowledge.Clusters != 3 || s.Knowledge.ByLifecycle["emerging"] != 1 {
		t.Fatalf("knowledge block lost: %+v", s.Knowledge)
	}
	// A later refresh must not drop it.
	tr.Record(rec("law", "FAST", false, 10, 0.5))
	if tr.Snapshot(0, "").Knowledge == nil {
		t.Fatal("knowledge block must survive subsequent queries")
	}
}

// The UI reads snake_case keys. A struct without a json tag marshals under its
// Go field name (Confidence, LatencyUS), the pane reads `confidence`, and it
// dies with "Cannot read properties of undefined (reading 'toFixed')" — which
// is exactly how this was found. Assert the wire shape, not just the values.
func TestSnapshotJSONContract(t *testing.T) {
	tr := New()
	tr.Record(Query{
		Namespace: "law", Mode: "FAST", Reused: true, Confidence: 0.75, Coverage: 0.5,
		Samples: 2, LLMCalls: 2, Tokens: 10, LatencyMS: 3, LatencyUS: 3210, Embedder: "local-hash-64",
		StopReason: "utility",
	})
	s := tr.Snapshot(1, "/tmp/s")
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	// Every field the UI touches must exist under its snake_case name.
	for _, k := range []string{"started_at", "uptime_sec", "queries", "system", "llm",
		"retrieval", "namespaces", "recent"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("snapshot is missing %q: %s", k, raw)
		}
	}
	recent, ok := m["recent"].([]any)
	if !ok {
		t.Fatalf("recent must be an array, got %T (null would crash v-for)", m["recent"])
	}
	if len(recent) != 1 {
		t.Fatalf("recent len=%d", len(recent))
	}
	q0 := recent[0].(map[string]any)
	for _, k := range []string{"at", "namespace", "mode", "reused", "confidence", "coverage",
		"samples", "loops", "widened", "llm_calls", "tokens", "latency_ms", "latency_us",
		"embedder", "self_corrected", "refused", "stop_reason"} {
		if _, ok := q0[k]; !ok {
			t.Fatalf("recent[0] is missing %q — the UI would crash reading it: %s", k, raw)
		}
	}
	// No Go-style field names may leak: that is the regression signature.
	for _, bad := range []string{"Confidence", "LatencyUS", "Namespace", "SelfCorr", "ReuseHits", "AvgP50US"} {
		if strings.Contains(string(raw), `"`+bad+`"`) {
			t.Fatalf("wire shape leaked a Go field name %q: %s", bad, raw)
		}
	}
	nsList, ok := m["namespaces"].([]any)
	if !ok {
		t.Fatalf("namespaces must be an array, got %T", m["namespaces"])
	}
	if len(nsList) != 1 {
		t.Fatalf("namespaces len=%d", len(nsList))
	}
	for _, k := range []string{"namespace", "queries", "reuse_hits", "avg_p50_us"} {
		if _, ok := nsList[0].(map[string]any)[k]; !ok {
			t.Fatalf("namespaces[0] is missing %q", k)
		}
	}
}

// An empty tracker must still emit arrays, not nulls.
func TestSnapshotEmptyIsArraysNotNull(t *testing.T) {
	raw, err := json.Marshal(New().Snapshot(0, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"recent":null`, `"namespaces":null`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("empty snapshot must not emit %s", k)
		}
	}
	if !strings.Contains(string(raw), `"recent":[]`) || !strings.Contains(string(raw), `"namespaces":[]`) {
		t.Fatalf("expected empty arrays: %s", raw)
	}
}

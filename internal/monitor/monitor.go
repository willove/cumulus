// Package monitor is the observability face: what the search pipeline is
// actually doing, per query and in aggregate.
//
// Two halves, deliberately separated:
//
//   - the Sirchmunk-shaped system/LLM block (process CPU/RSS, store size, LLM
//     calls and tokens), which answers "is this process healthy";
//   - the suite's own retrieval metrics (tier mix, cluster reuse hit rate, warm
//     vs cold latency, which embedder actually served), which answer "is the
//     cognitive path working". The second half is the one that matters for this
//     suite and is not available from any generic monitor.
//
// All of it is in-process and in-memory: one process owns the store (Badger
// takes an exclusive directory lock), so there is no cross-process aggregation
// to design for. Restarting serve resets the counters, which the API reports
// honestly via StartedAt rather than pretending they are lifetime totals.
package monitor

import (
	"runtime"
	"sort"
	"sync"
	"time"
)

// Query is one finished retrieval, as recorded by the search face.
type Query struct {
	At         time.Time `json:"at"`
	Namespace  string    `json:"namespace"` // bucket
	Mode       string    `json:"mode"`      // FAST | DEEP | FILENAME_ONLY | CHAT | DOC_SUMMARY
	Escalated  bool      `json:"escalated"`
	Reused     bool      `json:"reused"` // cluster reuse hit (the "越问越快" path)
	Confidence float64   `json:"confidence"`
	Coverage   float64   `json:"coverage"`
	Samples    int       `json:"samples"`
	Loops      int       `json:"loops"`
	Widened    int       `json:"widened"`
	LLMCalls   int       `json:"llm_calls"`
	Tokens     int64     `json:"tokens"`
	LatencyMS  int64     `json:"latency_ms"`
	LatencyUS  int64     `json:"latency_us"`     // preferred: microsecond precision
	Embedder   string    `json:"embedder"`       // which embedder actually served, e.g. minilm-384
	SelfCorr   bool      `json:"self_corrected"` // bounded self-correction ran
	Refused    bool      `json:"refused"`        // synthesis refused / insufficient evidence
	// StopReason is why the DEEP loop ended: sufficient | utility | budget;
	// "" when no loop ran or candidates ran out. Its distribution is the raw
	// material for tuning the escalation/budget knobs — without it, tuning
	// runs on anecdotes (the "96s for a guaranteed refusal" kind).
	StopReason string `json:"stop_reason,omitempty"`
	// Stages is the per-stage wall-time split (microseconds): analyze,
	// cascade, sample, synth on the FAST path; deep_sample / deep_synth on
	// the DEEP loop. Telemetry only — the "where did the seconds go"
	// question (and every compression-worth-it debate) is unanswerable
	// without it. Empty when the stage hooks were not wired (gates).
	Stages map[string]int64 `json:"stages,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// Tracker accumulates query records. It is safe for concurrent use: serve's
// HTTP handlers run searches in parallel goroutines.
type Tracker struct {
	mu        sync.Mutex
	started   time.Time
	knowledge *Knowledge
	queries   []Query // ring-bounded; see maxQueries
	llmCalls  int64
	llmTok    int64
	// per-mode and reuse counters are derived from queries on read, so a
	// truncation of the ring cannot make them disagree with each other.
}

// maxQueries bounds the retained ring. Recent behaviour is what an operator
// looks at; a month of history is not worth unbounded memory.
const maxQueries = 2048

func New() *Tracker {
	return &Tracker{started: time.Now().UTC()}
}

// Record appends one finished query.
func (t *Tracker) Record(q Query) {
	if q.At.IsZero() {
		q.At = time.Now().UTC()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queries) >= maxQueries {
		// Drop the oldest half rather than shifting one element at a time.
		copy(t.queries, t.queries[maxQueries/2:])
		t.queries = t.queries[:maxQueries/2]
	}
	t.queries = append(t.queries, q)
	t.llmCalls += int64(q.LLMCalls)
	t.llmTok += q.Tokens
}

// RecordLLM accounts a model call that is not tied to a finished query (for
// example a scan ranking), so the LLM block does not under-report.
func (t *Tracker) RecordLLM(calls int, tokens int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.llmCalls += int64(calls)
	t.llmTok += tokens
}

// Snapshot is the read model served over HTTP.
type Snapshot struct {
	StartedAt  time.Time  `json:"started_at"`
	UptimeSec  int64      `json:"uptime_sec"`
	Queries    int        `json:"queries"` // retained in the ring
	System     System     `json:"system"`
	LLM        LLM        `json:"llm"`
	Retrieval  Retrieval  `json:"retrieval"`
	Knowledge  *Knowledge `json:"knowledge,omitempty"` // nil when no cluster store is wired
	Namespaces []NSStat   `json:"namespaces"`
	Recent     []Query    `json:"recent"`
}

// Knowledge is the self-evolving layer's population snapshot — the "资料库
// 自整理" half of the monitor. Counts alone would hide drift (a rising
// `emerging` count means clusters waiting for re-validation), so the lifecycle
// distribution is reported explicitly.
type Knowledge struct {
	Clusters        int            `json:"clusters"`
	ByLifecycle     map[string]int `json:"by_lifecycle"`
	AvgConfidence   float64        `json:"avg_confidence"`
	AvgHotness      float64        `json:"avg_hotness"`
	EvidenceWindows int            `json:"evidence_windows"`
	Contested       int            `json:"contested"`      // has a conflict edge
	NeedingReview   int            `json:"needing_review"` // emerging: prior went stale
}

// System is the process/storage block (Sirchmunk's monitor shapes).
type System struct {
	Goroutines int     `json:"goroutines"`
	HeapMB     float64 `json:"heap_mb"`
	RSSMB      float64 `json:"rss_mb"`
	NumGC      uint32  `json:"num_gc"`
	// StoreFilesBytes is the apparent size of the store directory. It is an
	// UPPER BOUND, not usage: Badger preallocates its value logs, so an
	// almost-empty store still reports ~2 GB. Renamed from StoreBytes to stop
	// it being read as "bytes in use".
	StoreFilesBytes int64  `json:"store_files_bytes"`
	StoreDir        string `json:"store_dir"`
}

// LLM is the model-call block.
type LLM struct {
	Calls       int64   `json:"calls"`
	Tokens      int64   `json:"tokens"`
	TokensPerQ  float64 `json:"tokens_per_query"`
	CallsPerMin float64 `json:"calls_per_min"`
}

// Retrieval is the suite's own signal.
type Retrieval struct {
	ByMode        map[string]int `json:"by_mode"`
	ReuseHits     int            `json:"reuse_hits"`
	ReuseRate     float64        `json:"reuse_rate"`
	Escalations   int            `json:"escalations"`
	SelfCorrected int            `json:"self_corrected"`
	Refused       int            `json:"refused"`
	Errors        int            `json:"errors"`
	// Warm/cold split: a reuse hit is the "warm" path (0 samples) and
	// everything else paid for sampling. The design's headline promise is
	// "同类问题越问越快", and this is the number that shows whether it holds.
	WarmCount int     `json:"warm_count"`
	WarmP50MS int64   `json:"warm_p50_ms"`
	WarmP50US int64   `json:"warm_p50_us"`
	ColdCount int     `json:"cold_count"`
	ColdP50MS int64   `json:"cold_p50_ms"`
	ColdP50US int64   `json:"cold_p50_us"`
	AvgConf   float64 `json:"avg_confidence"`
	AvgCov    float64 `json:"avg_coverage"`
	Embedder  string  `json:"embedder"` // last embedder that actually served
}

// NSStat is one bucket's activity.
type NSStat struct {
	Namespace string `json:"namespace"`
	Queries   int    `json:"queries"`
	ReuseHits int    `json:"reuse_hits"`
	AvgP50US  int64  `json:"avg_p50_us"`
}

// WithKnowledge attaches a knowledge snapshot to subsequent reads. The tracker
// owns no cluster store (that is the engine's job), so the API layer computes
// the block and hands it over.
func (t *Tracker) WithKnowledge(k *Knowledge) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.knowledge = k
}

// KnowledgeStats returns the attached knowledge block, or nil.
func (t *Tracker) KnowledgeStats() *Knowledge {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.knowledge
}

// Snapshot renders the current state. storeBytes/storeDir are supplied by the
// caller because the engine port is what knows them.
func (t *Tracker) Snapshot(storeBytes int64, storeDir string) Snapshot {
	k := t.KnowledgeStats()
	t.mu.Lock()
	defer t.mu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	out := Snapshot{
		StartedAt: t.started,
		UptimeSec: int64(time.Since(t.started).Seconds()),
		Queries:   len(t.queries),
		System: System{
			Goroutines:      runtime.NumGoroutine(),
			HeapMB:          float64(ms.HeapAlloc) / (1 << 20),
			RSSMB:           float64(ms.Sys) / (1 << 20),
			NumGC:           ms.NumGC,
			StoreFilesBytes: storeBytes,
			StoreDir:        storeDir,
		},
		LLM:       LLM{Calls: t.llmCalls, Tokens: t.llmTok},
		Retrieval: Retrieval{ByMode: map[string]int{}},
		Knowledge: k,
	}
	var warm, cold []int64 // microseconds
	var confSum, covSum float64
	nsAgg := map[string]*NSStat{}
	nsLat := map[string][]int64{}
	for _, q := range t.queries {
		failed := q.Error != ""
		if q.Mode != "" {
			out.Retrieval.ByMode[q.Mode]++
		}
		if q.Reused {
			out.Retrieval.ReuseHits++
			warm = append(warm, q.latencyUS())
		} else if !failed {
			// A failed attempt's latency is a partial measurement, not a
			// retrieval time — counting it dragged the cold p50 down, exactly
			// once errors started being recorded at all.
			cold = append(cold, q.latencyUS())
		}
		if q.Escalated {
			out.Retrieval.Escalations++
		}
		if q.SelfCorr {
			out.Retrieval.SelfCorrected++
		}
		if q.Refused {
			out.Retrieval.Refused++
		}
		if failed {
			out.Retrieval.Errors++
		} else {
			confSum += q.Confidence
			covSum += q.Coverage
		}
		if q.Embedder != "" {
			out.Retrieval.Embedder = q.Embedder
		}
		st := nsAgg[q.Namespace]
		if st == nil {
			st = &NSStat{Namespace: q.Namespace}
			nsAgg[q.Namespace] = st
		}
		st.Queries++
		if q.Reused {
			st.ReuseHits++
		}
		if !failed {
			nsLat[q.Namespace] = append(nsLat[q.Namespace], q.latencyUS())
		}
	}
	n := float64(len(t.queries))
	scored := n - float64(out.Retrieval.Errors)
	if n > 0 {
		out.Retrieval.ReuseRate = float64(out.Retrieval.ReuseHits) / n
		if scored > 0 {
			out.Retrieval.AvgConf = confSum / scored
			out.Retrieval.AvgCov = covSum / scored
		}
		out.LLM.TokensPerQ = float64(t.llmTok) / n
	}
	elapsed := time.Since(t.started).Minutes()
	if elapsed > 0 {
		out.LLM.CallsPerMin = float64(t.llmCalls) / elapsed
	}
	out.Retrieval.WarmCount = len(warm)
	out.Retrieval.ColdCount = len(cold)
	out.Retrieval.WarmP50US = p50(warm)
	out.Retrieval.ColdP50US = p50(cold)
	out.Retrieval.WarmP50MS = out.Retrieval.WarmP50US / 1000
	out.Retrieval.ColdP50MS = out.Retrieval.ColdP50US / 1000
	// Recent and Namespaces must stay NON-NIL: a nil slice marshals as JSON
	// null, and the UI's v-for / .length over null is a TypeError.
	out.Namespaces = make([]NSStat, 0, len(nsAgg))
	for ns, st := range nsAgg {
		st.AvgP50US = p50(nsLat[ns])
		out.Namespaces = append(out.Namespaces, *st)
	}
	sort.Slice(out.Namespaces, func(i, j int) bool {
		if out.Namespaces[i].Queries != out.Namespaces[j].Queries {
			return out.Namespaces[i].Queries > out.Namespaces[j].Queries
		}
		return out.Namespaces[i].Namespace < out.Namespaces[j].Namespace
	})
	// Recent, newest last (a UI renders a timeline). Non-nil so the JSON is an
	// empty array rather than null (see the note on Namespaces above).
	out.Recent = make([]Query, 0, 20)
	if len(t.queries) > 20 {
		out.Recent = append(out.Recent, t.queries[len(t.queries)-20:]...)
	} else {
		out.Recent = append(out.Recent, t.queries...)
	}
	return out
}

// p50 is the upper median of xs (element at index len/2 after sorting), so a
// 4-element list reports its 3rd value. Consistent and cheap; a true
// interpolated median is not worth the surprise for an ops dashboard.
// latencyUS prefers the microsecond field and falls back to the millisecond one
// for callers that only filled that.
func (q Query) latencyUS() int64 {
	if q.LatencyUS > 0 {
		return q.LatencyUS
	}
	return q.LatencyMS * 1000
}

func p50(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	// insertion sort is fine for the bound we keep
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	return cp[len(cp)/2]
}

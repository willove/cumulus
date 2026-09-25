package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/source"
)

func v2Corpus() []source.Source {
	s := source.New("Pool", "text", "", "pool", "en", "Pool maximum 128", map[string]any{"nested": map[string]any{"value": "original"}})
	s.ID = "src:pool#1"
	return []source.Source{s}
}
func v2Content(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "{\"id\":\"q%d\",\"query\":\"Pool maximum?\",\"answer\":\"128\",\"gold_sources\":[\"pool\"]}\n", i)
	}
	return b.String()
}
func TestV2DatasetValidation(t *testing.T) {
	corpus := v2Corpus()
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", v2Content(2), true}, {"empty", "", false}, {"malformed", "{", false}, {"duplicate", v2Content(1) + v2Content(1), false},
		{"missing-answer", `{"id":"x","query":"q","gold_sources":["pool"]}`, false},
		{"missing-gold", `{"id":"x","query":"q","answer":"a"}`, false},
		{"wrong-gold", `{"id":"x","query":"q","answer":"a","gold_sources":["poo"]}`, false},
		{"stale-revision", `{"id":"x","query":"q","answer":"a","gold_sources":["src:pool#0"]}`, false},
		{"exact-id", `{"id":"x","query":"q","answer":"a","gold_sources":["src:pool#1"]}`, true},
		{"max-items", v2Content(500), true}, {"too-many", v2Content(501), false}, {"too-large", strings.Repeat("x", MaxBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateDataset(tc.content, corpus)
			if v.Valid != tc.valid {
				t.Fatalf("validation: %+v", v)
			}
			if !v.Valid && len(v.Errors) == 0 {
				t.Fatal("missing diagnostic")
			}
		})
	}
}
func TestV2SummarySeparateScoresAndCosts(t *testing.T) {
	yes, no := true, false
	items := []ItemResult{
		{RuleMatch: true, JudgeCorrect: &no, SearchTokens: 2, JudgeTokens: 3, ClosedBookTokens: 5, ClosedBookMatch: &yes, Attempts: []ItemResult{{SearchTokens: 7, JudgeTokens: 11, ClosedBookTokens: 13}}},
		{JudgeError: "judge failed", JudgeCorrect: &yes, SearchTokens: 17},
	}
	s := Summarize(items)
	if *s.RuleMatch != .5 || *s.JudgeCorrect != 0 || s.JudgeN != 1 || *s.ClosedBookMatch != 1 || s.SearchTokens != 26 || s.JudgeTokens != 14 || s.ClosedBookTokens != 18 {
		t.Fatalf("summary: %+v", s)
	}
	if empty := Summarize(nil); empty.RuleMatch != nil || empty.JudgeCorrect != nil || empty.ClosedBookMatch != nil {
		t.Fatal("N/A must be null")
	}
	if offline := Summarize([]ItemResult{{RuleMatch: true}}); offline.JudgeCorrect != nil || offline.ClosedBookMatch != nil {
		t.Fatal("offline model scores must be N/A")
	}
}
func TestV2ConfigAndCSV(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(false, false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){func(c *Config) { c.Mode = "invalid" }, func(c *Config) { c.Judge = true }, func(c *Config) { c.ClosedBook = true }, func(c *Config) { c.L1Pre = true }, func(c *Config) { c.Mode = "live" }, func(c *Config) { c.TokenBudget = 0 }, func(c *Config) { c.Timeout = -1 }, func(c *Config) { c.ItemTimeout = 601 }, func(c *Config) { c.Limit = 501 }} {
		c := cfg
		change(&c)
		if c.Validate(false, false) == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
	for _, v := range []string{"=SUM(A1)", "\t+evil", " \r-1", "\x00@cmd", "\u2003=foo"} {
		if SafeCSV(v) != "'"+v {
			t.Fatalf("unsafe CSV %q", v)
		}
	}
	if SafeCSV("normal, quoted") != "normal, quoted" {
		t.Fatal("ordinary value changed")
	}
}
func TestV2CompareRejectsMismatchesAndMatchesIDs(t *testing.T) {
	cfg := DefaultConfig()
	run := Run{Protocol: Protocol, State: "completed", Total: 2, Config: cfg, Frozen: Frozen{ItemsSHA: "same", CorpusSHA: "same"}}
	left := []ItemResult{{ID: "a", Query: "a", RuleMatch: false}, {ID: "b", Query: "b", RuleMatch: true}}
	right := []ItemResult{{ID: "b", Query: "b", RuleMatch: true}, {ID: "a", Query: "a", RuleMatch: true}}
	l, r := run, run
	l.Summary = Summarize(left)
	r.Summary = Summarize(right)
	r.Config.Prior = false
	c := CompareRuns(l, r, left, right)
	if !c.Comparable || c.Items[0].Change != "improved" || *c.Deltas["rule_match"] != .5 || len(c.ConfigDifferences) != 1 {
		t.Fatalf("comparison: %+v", c)
	}
	for _, change := range []func(*Run){func(r *Run) { r.State = "failed" }, func(r *Run) { r.Protocol = "legacy" }, func(r *Run) { r.Frozen.ItemsSHA = "different" }, func(r *Run) { r.Frozen.CorpusSHA = "different" }, func(r *Run) { r.Config.Mode = "live" }} {
		r = run
		change(&r)
		c = CompareRuns(l, r, left, right)
		if c.Comparable || len(c.Items) > 0 || c.Deltas["rule_match"] != nil {
			t.Fatalf("misleading comparison: %+v", c)
		}
	}
}

type v2FakeExecutor struct {
	fn     func(context.Context, Item, int64) ItemResult
	closed atomic.Bool
}

func (e *v2FakeExecutor) Execute(ctx context.Context, it Item, n int64) ItemResult {
	return e.fn(ctx, it, n)
}
func (e *v2FakeExecutor) Close() error { e.closed.Store(true); return nil }
func v2Service(t *testing.T, f Factory) (*Service, *cumulite.Engine) {
	t.Helper()
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(context.Background(), c, f, func(cfg Config) string { b, _ := json.Marshal(cfg); return string(b) })
	t.Cleanup(func() { s.Close(); _ = c.Close() })
	return s, c
}
func v2Dataset(t *testing.T, s *Service, n int) Dataset {
	t.Helper()
	d, _, err := s.SaveDataset(context.Background(), "a", "questions", v2Content(n), v2Corpus())
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func v2Start(t *testing.T, s *Service, d Dataset, cfg Config) Run {
	t.Helper()
	r, err := s.Start(context.Background(), "a", d.ID, "test", NewID(), cfg, v2Corpus())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func v2Wait(t *testing.T, s *Service, id string) Record {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		rec, err := s.Get(context.Background(), "a", id)
		if err != nil {
			t.Fatal(err)
		}
		if !Active(rec.Run.State) {
			return rec
		}
		select {
		case <-deadline.C:
			t.Fatalf("run timed out: %+v", rec.Run)
		case <-tick.C:
		}
	}
}
func TestV2ServiceImmutableRetryHistoryIdempotency(t *testing.T) {
	var calls atomic.Int32
	exec := &v2FakeExecutor{fn: func(_ context.Context, it Item, _ int64) ItemResult {
		state := "completed"
		if calls.Add(1) == 1 {
			state = "failed"
		}
		return ItemResult{State: state, Answer: "128", RuleMatch: true, SearchTokens: 3, JudgeTokens: 2, ClosedBookTokens: 1}
	}}
	s, c := v2Service(t, func(context.Context, Record) (Executor, error) { return exec, nil })
	d := v2Dataset(t, s, 2)
	other := v2Dataset(t, s, 2)
	if d.ID == other.ID || d.SHA != other.SHA {
		t.Fatal("dataset versions must be immutable")
	}
	corpus := v2Corpus()
	cfg := DefaultConfig()
	request := NewID()
	r, err := s.Start(context.Background(), "a", d.ID, "test", request, cfg, corpus)
	if err != nil {
		t.Fatal(err)
	}
	corpus[0].Body = "mutated"
	corpus[0].Meta["nested"].(map[string]any)["value"] = "mutated"
	rec := v2Wait(t, s, r.ID)
	if rec.Run.Failed != 1 || rec.Run.Done != 2 || rec.Corpus[0].Body != "Pool maximum 128" || rec.Corpus[0].Meta["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("snapshot/progress: %+v", rec)
	}
	duplicate, err := s.Start(context.Background(), "a", d.ID, "test", request, cfg, nil)
	if err != nil || duplicate.ID != r.ID {
		t.Fatalf("idempotency: %+v %v", duplicate, err)
	}
	if _, err := s.Start(context.Background(), "a", d.ID, "different", request, cfg, nil); err == nil {
		t.Fatal("request_id parameter collision accepted")
	}
	if _, err := s.Get(context.Background(), "b", r.ID); err != ErrNotFound {
		t.Fatalf("cross namespace read: %v", err)
	}
	if _, err := s.Dataset(context.Background(), "b", d.ID); err != ErrNotFound {
		t.Fatalf("cross namespace dataset: %v", err)
	}
	if _, err := s.Retry(context.Background(), "a", r.ID); err != nil {
		t.Fatal(err)
	}
	rec = v2Wait(t, s, r.ID)
	if rec.Run.State != "completed" || calls.Load() != 3 || rec.Results[0].Attempt != 2 || len(rec.Results[0].Attempts) != 1 || rec.Results[1].Attempt != 1 || rec.Run.Summary.Tokens() != 18 {
		t.Fatalf("retry: %+v calls=%d", rec, calls.Load())
	}
	if !exec.closed.Load() {
		t.Fatal("completed experiment not closed")
	}
	// A restart must never automatically execute a formerly active paid run.
	rec.Run.State = "running"
	rec.Owner = "old-process"
	if err := s.put(context.Background(), key("a", "run", r.ID), rec); err != nil {
		t.Fatal(err)
	}
	s2 := NewService(context.Background(), c, func(context.Context, Record) (Executor, error) {
		t.Error("restart launched executor")
		return nil, fmt.Errorf("unexpected")
	}, s.fingerprint)
	defer s2.Close()
	restored, err := s2.Get(context.Background(), "a", r.ID)
	if err != nil || restored.Run.State != "interrupted" {
		t.Fatalf("restart: %+v %v", restored, err)
	}
	if _, err := s2.Retry(context.Background(), "a", r.ID); err == nil || !strings.Contains(err.Error(), "start a new run") {
		t.Fatalf("unsafe cold retry: %v", err)
	}
}
func TestV2ServiceCancellationQueueBudgetAndTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	s, _ := v2Service(t, func(context.Context, Record) (Executor, error) {
		return &v2FakeExecutor{fn: func(ctx context.Context, it Item, n int64) ItemResult {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ItemResult{State: "failed", Error: ctx.Err().Error(), ErrorStage: "search", SearchTokens: 1}
		}}, nil
	})
	d := v2Dataset(t, s, 2)
	r := v2Start(t, s, d, DefaultConfig())
	<-started
	// A single worker holds the first item. Eight waiting jobs are accepted,
	// the ninth is rejected without spawning another executor.
	for i := 0; i < QueueLimit; i++ {
		v2Start(t, s, d, DefaultConfig())
	}
	if _, err := s.Start(context.Background(), "a", d.ID, "overflow", NewID(), DefaultConfig(), v2Corpus()); err == nil {
		t.Fatal("queue unbounded")
	}
	if _, err := s.Cancel(context.Background(), "a", r.ID); err != nil {
		t.Fatal(err)
	}
	rec := v2Wait(t, s, r.ID)
	if rec.Run.State != "cancelled" || rec.Run.Done != 1 || rec.Run.Failed != 1 {
		t.Fatalf("cancellation: %+v", rec.Run)
	}
	again, err := s.Cancel(context.Background(), "a", r.ID)
	if err != nil || again.State != "cancelled" {
		t.Fatal("cancel not idempotent")
	}
	// Independent service with a deterministic spend, no network.
	var calls atomic.Int32
	b, _ := v2Service(t, func(context.Context, Record) (Executor, error) {
		return &v2FakeExecutor{fn: func(context.Context, Item, int64) ItemResult {
			calls.Add(1)
			return ItemResult{State: "completed", SearchTokens: 7, JudgeTokens: 3, ClosedBookTokens: 2}
		}}, nil
	})
	bd := v2Dataset(t, b, 2)
	cfg := DefaultConfig()
	cfg.TokenBudget = 10
	br := v2Start(t, b, bd, cfg)
	brec := v2Wait(t, b, br.ID)
	if brec.Run.State != "failed" || brec.Run.Done != 1 || calls.Load() != 1 || brec.Run.Summary.Tokens() != 12 || !strings.Contains(brec.Run.Error, "budget") {
		t.Fatalf("budget: %+v", brec.Run)
	}
	timeout, _ := v2Service(t, func(context.Context, Record) (Executor, error) {
		return &v2FakeExecutor{fn: func(ctx context.Context, _ Item, _ int64) ItemResult {
			<-ctx.Done()
			return ItemResult{State: "completed"}
		}}, nil
	})
	td := v2Dataset(t, timeout, 1)
	cfg = DefaultConfig()
	cfg.ItemTimeout = 1
	cfg.Timeout = 2
	tr := v2Start(t, timeout, td, cfg)
	trec := v2Wait(t, timeout, tr.ID)
	if trec.Run.Failed != 1 || trec.Results[0].ErrorStage != "timeout" {
		t.Fatalf("item deadline: %+v", trec)
	}
}
func TestV2RetryFingerprintAndSnapshotChecks(t *testing.T) {
	s, _ := v2Service(t, func(context.Context, Record) (Executor, error) {
		return &v2FakeExecutor{fn: func(context.Context, Item, int64) ItemResult { return ItemResult{State: "failed"} }}, nil
	})
	d := v2Dataset(t, s, 1)
	r := v2Start(t, s, d, DefaultConfig())
	rec := v2Wait(t, s, r.ID)
	rec.Corpus[0].Structure[0].Label = "tampered"
	if err := s.put(context.Background(), key("a", "run", r.ID), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), "a", r.ID); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("tampered corpus accepted: %v", err)
	}
}

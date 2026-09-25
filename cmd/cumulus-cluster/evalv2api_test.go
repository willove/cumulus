package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

type evalHTTPHarness struct {
	t       *testing.T
	mux     *http.ServeMux
	c       *cumulite.Engine
	service *eval.Service
	st      *ingest.Store
}

func newEvalHTTPHarness(t *testing.T) *evalHTTPHarness {
	t.Helper()
	ctx := context.Background()
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	buckets := bucket.New(c)
	for _, name := range []string{"eval-a", "eval-b"} {
		if _, err := buckets.Create(ctx, name, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	st := ingest.New(c, "eval-a:clus_sources", "eval-a:clus_evidence", "eval-a:clus_clusters", "eval-a")
	if _, err := st.Ensure(ctx, suiteExtra("eval-a")...); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, source.New("Pool", "text", "", "pool", "en", "Pool maximum connections is 128. Timeout is 30 seconds.", map[string]any{"owner": "frozen"})); err != nil {
		t.Fatal(err)
	}
	service := eval.NewService(ctx, c, newEvalExecutor, evalFingerprint)
	mux := http.NewServeMux()
	registerEvalV2Face(mux, c, buckets, service, "", "")
	t.Cleanup(func() { service.Close(); _ = c.Close() })
	return &evalHTTPHarness{t: t, mux: mux, c: c, service: service, st: st}
}
func (h *evalHTTPHarness) request(method, path string, body any, code int, out any) *httptest.ResponseRecorder {
	h.t.Helper()
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		input = bytes.NewReader(raw)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, httptest.NewRequest(method, path, input))
	if w.Code != code {
		h.t.Fatalf("%s %s = %d want %d: %s", method, path, w.Code, code, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			h.t.Fatal(err)
		}
	}
	return w
}
func (h *evalHTTPHarness) wait(id string) eval.Run {
	h.t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		var run eval.Run
		h.request("GET", "/v1/eval/runs/"+id+"?ns=eval-a", nil, 200, &run)
		if !eval.Active(run.State) {
			return run
		}
		select {
		case <-timer.C:
			h.t.Fatalf("run stuck: %+v", run)
		case <-tick.C:
		}
	}
}
func (h *evalHTTPHarness) dataset(content string) eval.Dataset {
	var d eval.Dataset
	h.request("POST", "/v1/eval/datasets?ns=eval-a", map[string]any{"name": "pool questions", "content": content}, 201, &d)
	return d
}
func (h *evalHTTPHarness) start(d eval.Dataset, request string, cfg eval.Config) eval.Run {
	var r eval.Run
	h.request("POST", "/v1/eval/runs?ns=eval-a", map[string]any{"dataset_id": d.ID, "name": "Pool evaluation", "request_id": request, "config": cfg}, 202, &r)
	return r
}

const evalHTTPContent = `{"id":"q1","query":"Pool maximum connections?","answer":"128","gold_sources":["pool"]}`

func TestEvalV2HTTPNamespaceValidationAndLimits(t *testing.T) {
	h := newEvalHTTPHarness(t)
	for _, route := range []string{"capabilities", "datasets", "datasets/unknown", "runs", "runs/unknown", "runs/unknown/items", "compare?left=x&right=y"} {
		sep := "?"
		if strings.Contains(route, "?") {
			sep = "&"
		}
		h.request("GET", "/v1/eval/"+route, nil, 400, nil)
		h.request("GET", "/v1/eval/"+route+sep+"ns=missing", nil, 400, nil)
		h.request("GET", "/v1/eval/"+route+sep+"ns=bad:ns", nil, 400, nil)
	}
	var cap map[string]any
	h.request("GET", "/v1/eval/capabilities?ns=eval-a", nil, 200, &cap)
	if cap["protocol"] != "eval-v2" || cap["max_items"] != float64(500) || cap["concurrency"] != float64(1) || cap["queue_limit"] != float64(eval.QueueLimit) {
		t.Fatalf("capabilities: %v", cap)
	}
	for _, content := range []string{"{", evalHTTPContent + "\n" + evalHTTPContent, `{"id":"q","query":"x","answer":"a"}`, strings.Repeat("x", eval.MaxBytes+1)} {
		var validation eval.Validation
		h.request("POST", "/v1/eval/datasets/validate?ns=eval-a", map[string]any{"name": "bad", "content": content}, 200, &validation)
		if validation.Valid || len(validation.Errors) == 0 {
			t.Fatal("invalid dataset accepted")
		}
		h.request("POST", "/v1/eval/datasets?ns=eval-a", map[string]any{"name": "bad", "content": content}, 400, nil)
	}
	var datasets struct {
		Datasets []eval.Dataset `json:"datasets"`
	}
	h.request("GET", "/v1/eval/datasets?ns=eval-a", nil, 200, &datasets)
	if len(datasets.Datasets) != 0 {
		t.Fatal("validate wrote storage")
	}
	d := h.dataset(evalHTTPContent)
	h.request("GET", "/v1/eval/datasets/"+d.ID+"?ns=eval-b", nil, 404, nil)
	h.request("POST", "/v1/eval/datasets/"+d.ID+"?ns=eval-a", nil, 405, nil)
	for _, cfg := range []map[string]any{{"mode": "unknown"}, {"mode": "offline", "judge": true}, {"mode": "offline", "closed_book": true}, {"timeout_seconds": 0}, {"token_budget": -1}, {"limit": 501}} {
		h.request("POST", "/v1/eval/runs?ns=eval-a", map[string]any{"dataset_id": d.ID, "request_id": eval.NewID(), "config": cfg}, 400, nil)
	}
	h.request("POST", "/v1/eval/runs?ns=eval-a", map[string]any{"dataset_id": d.ID, "config": map[string]any{}}, 400, nil)
}
func TestEvalV2OfflineFlowExportsCompareAndIsolation(t *testing.T) {
	var network atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { network.Add(1); http.Error(w, "must not call live", 500) }))
	defer trap.Close()
	t.Setenv("CLUS_OFFLINE", "")
	t.Setenv("AIGATE_BASE_URL", trap.URL)
	t.Setenv("AIGATE_API_KEY", "eval-secret-never-export")
	t.Setenv("AIGATE_EMBED_MODEL", "must-not-call")
	h := newEvalHTTPHarness(t)
	ctx := context.Background()
	before := map[string]string{}
	for _, coll := range []string{"clus_sources", "clus_evidence", "clus_clusters", "clus_weak_edges", "clus_cites", "clus_conflicts"} {
		q, err := h.c.Query(ctx, ns.Coll("eval-a", coll), contract.Query{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Documents)
		before[coll] = string(raw)
	}
	d := h.dataset(evalHTTPContent)
	d2 := h.dataset(evalHTTPContent)
	if d.ID == d2.ID || d.SHA != d2.SHA {
		t.Fatal("immutable dataset save failed")
	}
	request := eval.NewID()
	first := h.start(d, request, eval.DefaultConfig())
	run := h.wait(first.ID)
	if run.State != "completed" || run.Done != 1 || run.Total != 1 || run.Failed != 0 || run.Summary.ClosedBookMatch != nil || run.Summary.JudgeCorrect != nil || run.Summary.Tokens() != 0 {
		t.Fatalf("offline run: %+v", run)
	}
	var items struct {
		Items []eval.ItemResult `json:"items"`
	}
	h.request("GET", "/v1/eval/runs/"+run.ID+"/items?ns=eval-a", nil, 200, &items)
	if len(items.Items) != 1 || items.Items[0].Answer == "" || items.Items[0].Attempt != 1 || len(items.Items[0].Citations) == 0 || items.Items[0].Citations[0].Quote == "" {
		t.Fatalf("results: %+v", items)
	}
	duplicate := h.start(d, request, eval.DefaultConfig())
	if duplicate.ID != run.ID {
		t.Fatal("duplicate submission created new run")
	}
	h.request("GET", "/v1/eval/runs/"+run.ID+"?ns=eval-b", nil, 404, nil)
	h.request("GET", "/v1/eval/runs/"+run.ID+"/export?ns=eval-b", nil, 404, nil)
	cfg := eval.DefaultConfig()
	cfg.Prior = false
	second := h.start(d2, eval.NewID(), cfg)
	right := h.wait(second.ID)
	var comparison eval.Comparison
	h.request("GET", "/v1/eval/compare?ns=eval-a&left="+run.ID+"&right="+right.ID, nil, 200, &comparison)
	if !comparison.Comparable || len(comparison.Items) != 1 || len(comparison.ConfigDifferences) != 1 {
		t.Fatalf("comparison: %+v", comparison)
	}
	for _, format := range []string{"json", "jsonl", "csv"} {
		w := h.request("GET", "/v1/eval/runs/"+run.ID+"/export?ns=eval-a&format="+format, nil, 200, nil)
		if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment;") || !strings.Contains(w.Body.String(), run.Frozen.ConfigSHA) || strings.Contains(w.Body.String(), "eval-secret-never-export") {
			t.Fatalf("bad export %s: %s", format, w.Body.String())
		}
		if format == "csv" {
			rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
			if err != nil || len(rows) != 2 {
				t.Fatalf("CSV: %v %v", rows, err)
			}
		}
	}
	h.request("GET", "/v1/eval/runs/"+run.ID+"/export?ns=eval-a&format=exe", nil, 400, nil)
	for coll, want := range before {
		q, err := h.c.Query(ctx, ns.Coll("eval-a", coll), contract.Query{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Documents)
		if string(raw) != want {
			t.Fatalf("business collection mutated: %s", coll)
		}
	}
	if _, err := h.c.KVGet(ctx, ns.KV("eval-a", "clus:lastcluster")); !contract.IsNotFound(err) {
		t.Fatalf("business cursor written: %v", err)
	}
	if network.Load() != 0 {
		t.Fatalf("offline made %d model requests", network.Load())
	}
	// Updating the business source cannot change any already frozen experiment.
	if _, err := h.st.Put(ctx, source.New("Pool", "text", "", "pool", "en", "Pool maximum connections is 256.", nil)); err != nil {
		t.Fatal(err)
	}
	rec, err := h.service.Get(ctx, "eval-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Corpus[0].Body, "128") || rec.Run.Frozen.CorpusSHA != run.Frozen.CorpusSHA {
		t.Fatal("snapshot changed with business corpus")
	}
	third := h.start(d, eval.NewID(), eval.DefaultConfig())
	h.wait(third.ID)
	h.request("GET", "/v1/eval/compare?ns=eval-a&left="+run.ID+"&right="+third.ID, nil, 200, &comparison)
	if comparison.Comparable || len(comparison.Reasons) == 0 || comparison.Deltas["rule_match"] != nil {
		t.Fatalf("mismatched corpus compared: %+v", comparison)
	}
}
func TestEvalV2CSVFormulaProtection(t *testing.T) {
	rec := eval.Record{Run: eval.Run{ID: "safe", Protocol: eval.Protocol, ConfigText: "\t=malicious"}, Results: []eval.ItemResult{{ID: "@id", Query: " \t+cmd", Answer: "\r-1", Reference: "=cmd"}}}
	w := httptest.NewRecorder()
	exportEval(w, httptest.NewRequest("GET", "/v1/eval/runs/safe/export?format=csv", nil), rec)
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []int{2, 6, 7, 8, 9} {
		if !strings.HasPrefix(rows[1][column], "'") {
			t.Fatalf("unsanitized column %d: %q", column, rows[1][column])
		}
	}
}

type evalRoundTripFunc func(*http.Request) (*http.Response, error)

func (f evalRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestEvalV2BudgetTransportNoNetwork(t *testing.T) {
	chat := &llm.ChatClient{}
	calls := 0
	transport := &evalBudgetTransport{chat: chat, base: evalRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("fake request") })}
	if _, err := transport.RoundTrip(httptest.NewRequest("POST", "http://model.invalid", nil)); err == nil || !transport.blocked.Load() || calls != 0 {
		t.Fatal("exhausted budget reached model")
	}
	transport.ceiling.Store(1)
	_, _ = transport.RoundTrip(httptest.NewRequest("POST", "http://model.invalid", nil))
	if calls != 1 {
		t.Fatal("available budget did not reach injected transport")
	}
}

func TestEvalV2L1AndModelStageAccounting(t *testing.T) {
	t.Setenv("CLUS_OFFLINE", "1")
	h := newEvalHTTPHarness(t)
	d := h.dataset(evalHTTPContent)
	cfg := eval.DefaultConfig()
	cfg.L1Pre = true
	run := h.start(d, eval.NewID(), cfg)
	if got := h.wait(run.ID); got.State != "completed" {
		t.Fatalf("L1 failed: %+v", got)
	}
	corpus, err := evalCorpus(context.Background(), h.c, "eval-a:clus_sources")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, judge           string
		budget                int64
		state                 string
		judgeTokens, cbTokens int64
	}{
		{"judge failure", "not JSON", 100, "failed", 7, 11},
		{"negative judge independent of rule", `{"score":1,"reasoning":"incorrect"}`, 100, "completed", 7, 11},
		{"budget includes judge before baseline", `{"score":8,"reasoning":"correct"}`, 7, "failed", 7, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := eval.DefaultConfig()
			factory, err := newEvalExecutor(context.Background(), eval.Record{Run: eval.Run{Config: cfg, ConfigText: evalFingerprint(cfg)}, Corpus: corpus})
			if err != nil {
				t.Fatal(err)
			}
			defer factory.Close()
			e := factory.(*evalExecutor)
			calls := 0
			chat := &llm.ChatClient{BaseURL: "http://injected.invalid"}
			transport := &evalBudgetTransport{chat: chat, base: evalRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				answer, tokens := test.judge, 7
				if calls == 2 {
					answer, tokens = "128", 11
				}
				body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}, "usage": map[string]any{"total_tokens": tokens}})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			chat.HTTPClient = &http.Client{Transport: transport}
			e.stack.chat, e.budget = chat, transport
			e.cfg.Judge, e.cfg.ClosedBook = true, true
			got := e.Execute(context.Background(), d.Items[0], test.budget)
			if got.State != test.state || got.SearchTokens != 0 || got.JudgeTokens != test.judgeTokens || got.ClosedBookTokens != test.cbTokens || !got.RuleMatch {
				t.Fatalf("stage accounting: %+v", got)
			}
			if test.name == "judge failure" && (got.JudgeCorrect != nil || got.JudgeError == "" || got.ClosedBookMatch == nil) {
				t.Fatalf("judge failure hidden: %+v", got)
			}
			if test.name == "negative judge independent of rule" && (got.JudgeCorrect == nil || *got.JudgeCorrect) {
				t.Fatalf("judge overwrote rule: %+v", got)
			}
			if test.cbTokens == 0 && (got.ClosedBookMatch != nil || calls != 1 || got.ErrorStage != "budget") {
				t.Fatalf("budget did not stop baseline: %+v", got)
			}
		})
	}
}

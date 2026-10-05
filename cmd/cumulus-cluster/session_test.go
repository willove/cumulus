package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// Turns on one session are a read-modify-write of one KV document: without the
// face's lock, two concurrent turns both read the old message list and the
// later save drops the other's.
func TestConcurrentTurnsAllLand(t *testing.T) {
	p := newTestPort()
	st := sessionStore{c: p, ns: "alpha"}
	ctx := context.Background()
	const turns = 4
	var wg sync.WaitGroup
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := st.appendTurn(ctx, "shared", "t", fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i)); err != nil {
				t.Errorf("appendTurn %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	d, err := st.load(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Messages) != 2*turns {
		t.Fatalf("messages = %d, want %d — a concurrent turn was lost", len(d.Messages), 2*turns)
	}
}

// A read that fails must never be folded into "no such session": the first
// would silently reset a conversation, the second legitimately starts one.
func TestSessionReadOutcomes(t *testing.T) {
	existing := sessionDoc{
		ID: "shared", Title: "original title", CreatedAt: 10, UpdatedAt: 20,
		Messages: []sessionMessage{
			{Role: "user", Content: "first question", At: 10},
			{Role: "assistant", Content: "first answer", At: 15},
			{Role: "user", Content: "follow-up", At: 20},
		},
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		setup func(p *testPort)
	}{
		{
			name:  "existing",
			setup: func(p *testPort) { p.seed("ns:alpha:clus:session:shared", string(raw)) },
		},
		{name: "absent"},
		{
			name: "read failure",
			setup: func(p *testPort) {
				p.OnKVGet = func(string) error { return errors.New("backend unavailable") }
			},
		},
		{
			name:  "malformed document",
			setup: func(p *testPort) { p.seed("ns:alpha:clus:session:shared", `{"id":"shared","messages":`) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPort()
			if tc.setup != nil {
				tc.setup(p)
			}
			st := sessionStore{c: p, ns: "alpha"}
			ctx := context.Background()
			wantFailure := tc.name == "read failure" || tc.name == "malformed document"
			checkError := func(t *testing.T, err error) {
				t.Helper()
				if !wantFailure {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatal("read failure was treated as a missing session")
				}
				if contract.IsNotFound(err) {
					t.Fatalf("read failure became NotFound: %v", err)
				}
				if tc.name == "read failure" {
					if !strings.Contains(err.Error(), "backend unavailable") {
						t.Fatalf("store error not preserved: %v", err)
					}
					return
				}
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) || !strings.Contains(err.Error(), "session shared:") {
					t.Fatalf("document decode error not preserved: %v", err)
				}
			}

			t.Run("ensure", func(t *testing.T) {
				d, err := st.ensure(ctx, "shared", "new title")
				checkError(t, err)
				if wantFailure {
					if d != nil {
						t.Fatalf("failed ensure returned a session: %+v", d)
					}
				} else if tc.name == "existing" {
					if !reflect.DeepEqual(d, &existing) {
						t.Fatalf("ensure changed existing session: %+v", d)
					}
				} else if d == nil || d.ID != "shared" || d.Title != "new title" || len(d.Messages) != 0 || d.CreatedAt <= 0 || d.UpdatedAt != d.CreatedAt {
					t.Fatalf("unexpected new session: %+v", d)
				}
			})
			t.Run("history", func(t *testing.T) {
				history, d, err := sessionHistory(ctx, st, "shared", 2)
				checkError(t, err)
				if wantFailure {
					if history != nil || d != nil {
						t.Fatalf("failed history returned data: %v, %+v", history, d)
					}
				} else if tc.name == "existing" {
					want := []string{"assistant: first answer", "user: follow-up"}
					if !reflect.DeepEqual(history, want) || !reflect.DeepEqual(d, &existing) {
						t.Fatalf("history = %v, session = %+v", history, d)
					}
				} else if len(history) != 0 || !reflect.DeepEqual(d, &sessionDoc{ID: "shared"}) {
					t.Fatalf("missing history = %v, session = %+v", history, d)
				}
			})
			if _, puts, _, _ := p.calls(); len(puts) != 0 {
				t.Fatalf("ensure/history performed %d writes", len(puts))
			}
			t.Run("append", func(t *testing.T) {
				d, err := st.appendTurn(ctx, "shared", "new title", "new question", "new answer")
				checkError(t, err)
				if wantFailure {
					if d != nil {
						t.Fatalf("failed append returned a session: %+v", d)
					}
					return
				}
				wantTitle, wantMessages := "new title", 2
				if tc.name == "existing" {
					wantTitle = existing.Title
					wantMessages += len(existing.Messages)
				}
				if d == nil || d.ID != "shared" || d.Title != wantTitle || len(d.Messages) != wantMessages {
					t.Fatalf("unexpected appended session: %+v", d)
				}
				if tc.name == "existing" && (d.CreatedAt != existing.CreatedAt || !reflect.DeepEqual(d.Messages[:len(existing.Messages)], existing.Messages)) {
					t.Fatalf("append lost existing session data: %+v", d)
				}
				last := d.Messages[len(d.Messages)-2:]
				if last[0].Role != "user" || last[0].Content != "new question" || last[1].Role != "assistant" || last[1].Content != "new answer" {
					t.Fatalf("unexpected appended messages: %+v", last)
				}
			})
			wantWrites := 1
			if wantFailure {
				wantWrites = 0
				t.Run("POST", func(t *testing.T) {
					mux := http.NewServeMux()
					registerSessionFace(mux, p, "alpha")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"id":"shared","title":"new title"}`)))
					if w.Code != http.StatusInternalServerError {
						t.Fatalf("POST status = %d, want 500; body %s", w.Code, w.Body.String())
					}
				})
			}
			if _, puts, _, _ := p.calls(); len(puts) != wantWrites {
				t.Errorf("writes = %d, want %d", len(puts), wantWrites)
			}
		})
	}
}

// A finished turn must survive the caller walking away. The search writes its turn
// through r.Context(), so a user who navigated to another pane mid-search (or an
// aborted SSE stream) cancelled that context and the KV put failed — the answer was
// delivered but the session stayed behind as an empty shell nobody could open.
// appendTurnDurable detaches the write; this test pins both halves: the detached
// write lands with a cancelled parent, and the raw context still fails (which is
// exactly what made the bug possible).
func TestSessionDurableAppendSurvivesCancelledRequest(t *testing.T) {
	// A real engine, not the in-package fake: the whole point is that the engine
	// honours context cancellation, and a test double that ignored ctx would make
	// the control half of this test vacuous.
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	st := sessionStore{c: c, ns: "alpha"}
	gone, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone

	d, err := st.appendTurnDurable(gone, "kept", "题", "题", "答")
	if err != nil {
		t.Fatalf("detached append must still land: %v", err)
	}
	if len(d.Messages) != 2 || d.Messages[0].Content != "题" || d.Messages[1].Content != "答" {
		t.Fatalf("turn not recorded: %+v", d.Messages)
	}
	if _, err := st.load(context.Background(), "kept"); err != nil {
		t.Fatalf("turn must be readable afterwards: %v", err)
	}

	// Control: the same write through the request context is what failed before,
	// so the fix cannot silently degrade back into the old behaviour.
	if _, err := st.appendTurn(gone, "dropped", "题", "题", "答"); err == nil {
		t.Fatal("a cancelled request context must not be able to persist a turn")
	}
	if _, err := st.load(context.Background(), "dropped"); err == nil {
		t.Fatal("the control session must not exist")
	}
}

// statsFrom 必须吃下 serve 传来的 map[string]int64（曾因只断言
// map[string]any 而静默丢掉整个 stages，刷新恢复后时间轴消失）。
func TestStatsFromKeepsTypedStages(t *testing.T) {
	st := statsFrom(map[string]any{
		"mode": "DEEP", "conf": 0.9, "latency_ms": int64(17473),
		"stages": map[string]int64{"analyze": 1144795, "deep_sample": 22181507},
	})
	if st == nil || len(st.Stages) != 2 {
		t.Fatalf("stages = %v, want two entries", st.Stages)
	}
	if st.Stages["analyze"] != 1144795 {
		t.Fatalf("analyze = %d, want 1144795", st.Stages["analyze"])
	}
	if st.LatencyMS != 17473 {
		t.Fatalf("latency = %d, want 17473", st.LatencyMS)
	}
	// map[string]any（SSE done 反序列化形状）同样要吃下。
	st2 := statsFrom(map[string]any{"stages": map[string]any{"synth": float64(900000)}})
	if st2 == nil || st2.Stages["synth"] != 900000 {
		t.Fatalf("any-map stages = %v", st2.Stages)
	}
}

// The thinking log persists the stream's raw events in arrival order so a
// session switch or refresh can replay the box: contiguous reasoning deltas
// fold into one step, kind boundaries flush the pending buffer, and the
// recorded sequence rides appendTurn → sessionMessage.Think unchanged.
func TestThinkLogMergesReasoningAndPersistsWithTurn(t *testing.T) {
	tl := &thinkLog{}
	tl.stage("analyze", 120*time.Millisecond, map[string]any{"intent": "search"})
	tl.file("src:blk/%E8%AE%B0%E5%88%86.txt%2F000001#1", "记分管理办法.txt", 8.5, 3)
	tl.reasoning("FAST ")
	tl.reasoning("思考。")
	tl.stage("deep_sample", 52*time.Second, nil)
	tl.reasoning("DEEP 思考。")
	got := tl.snapshot()
	if got == nil || len(got.Steps) != 5 {
		t.Fatalf("snapshot = %+v, want 5 steps", got)
	}
	wantKinds := "stage,file,reasoning,stage,reasoning"
	if k := kinds(got.Steps); k != wantKinds {
		t.Fatalf("kinds = %q, want %q (kind boundary must flush pending reasoning)", k, wantKinds)
	}
	if got.Steps[2].Text != "FAST 思考。" || got.Steps[4].Text != "DEEP 思考。" {
		t.Fatalf("reasoning texts = %q / %q", got.Steps[2].Text, got.Steps[4].Text)
	}
	if got.Steps[1].Windows == nil || *got.Steps[1].Windows != 3 {
		t.Fatalf("file windows = %v", got.Steps[1].Windows)
	}
	if got.Steps[1].ID == "" || got.Steps[1].File != "记分管理办法.txt" {
		t.Fatalf("file step must carry both the preview id and the display key: %+v", got.Steps[1])
	}
	// snapshot 深拷贝：后续录制不得渗进已落库的序列。
	tl.reasoning("迟到")
	if n := len(got.Steps); n != 5 {
		t.Fatalf("post-snapshot leakage: %d steps", n)
	}
	if again := tl.snapshot(); len(again.Steps) != 6 {
		t.Fatalf("second snapshot should see 6 steps, got %d", len(again.Steps))
	}

	// 落库往返：appendTurn 带 Think → load 后原样回来（omitempty 不吞）。
	p := newTestPort()
	st := sessionStore{c: p, ns: "alpha"}
	if _, err := st.appendTurn(context.Background(), "think", "t", "q", "a",
		&turnExtras{Think: got}); err != nil {
		t.Fatalf("appendTurn: %v", err)
	}
	d, err := st.load(context.Background(), "think")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var asst *sessionMessage
	for i := range d.Messages {
		if d.Messages[i].Role == "assistant" {
			asst = &d.Messages[i]
		}
	}
	if asst == nil || asst.Think == nil || len(asst.Think.Steps) != 5 {
		t.Fatalf("persisted think = %+v", asst.Think)
	}
	if asst.Think.Steps[0].Detail == nil {
		t.Fatalf("stage detail lost in persist round-trip")
	}
}

func kinds(steps []sessionThinkStep) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return strings.Join(out, ",")
}

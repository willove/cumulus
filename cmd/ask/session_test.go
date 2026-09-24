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
			setup: func(p *testPort) { p.seed("ns:alpha:ask:session:shared", string(raw)) },
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
			setup: func(p *testPort) { p.seed("ns:alpha:ask:session:shared", `{"id":"shared","messages":`) },
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

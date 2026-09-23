package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cumubase/cumudb/pkg/client"
)

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
		name   string
		status int
		body   string
	}{
		{name: "existing", status: http.StatusOK, body: string(raw)},
		{name: "absent", status: http.StatusNotFound, body: `{"error":{"code":"NOT_FOUND","message":"missing"}}`},
		{name: "backend failure", status: http.StatusInternalServerError, body: `{"error":{"code":"INTERNAL","message":"backend unavailable"}}`},
		{name: "malformed document", status: http.StatusOK, body: `{"id":"shared","messages":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/kv/ns:alpha:ask:session:shared" {
					t.Errorf("unexpected backend path: %s", r.URL.Path)
				}
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case http.MethodPut:
					writes.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected backend method: %s", r.Method)
					http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				}
			}))
			defer backend.Close()
			st := sessionStore{c: client.New(backend.URL), ns: "alpha"}
			ctx := context.Background()
			wantFailure := tc.name == "backend failure" || tc.name == "malformed document"
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
				if client.IsNotFound(err) {
					t.Fatalf("read failure became NotFound: %v", err)
				}
				if tc.name == "backend failure" {
					var apiErr *client.APIError
					if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError || apiErr.Message != "backend unavailable" {
						t.Fatalf("backend error not preserved: %v", err)
					}
				} else {
					var syntaxErr *json.SyntaxError
					if !errors.As(err, &syntaxErr) || !strings.Contains(err.Error(), "session shared:") {
						t.Fatalf("document decode error not preserved: %v", err)
					}
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
			if got := writes.Load(); got != 0 {
				t.Fatalf("ensure/history performed %d writes", got)
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
			wantWrites := int32(1)
			if wantFailure {
				wantWrites = 0
				t.Run("POST", func(t *testing.T) {
					mux := http.NewServeMux()
					registerSessionFace(mux, st.c, "alpha")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"id":"shared","title":"new title"}`)))
					if w.Code != http.StatusInternalServerError {
						t.Fatalf("POST status = %d, want 500; body %s", w.Code, w.Body.String())
					}
				})
			}
			if got := writes.Load(); got != wantWrites {
				t.Errorf("writes = %d, want %d", got, wantWrites)
			}
		})
	}
}

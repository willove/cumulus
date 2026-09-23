package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumudb/pkg/client"
)

// P3: the session KV keys are scoped through ns.KV — a tenant's session list
// must never surface another tenant's sessions (same key prefix discipline the
// collections follow via "ns:coll" composite identities).
func TestSessionKeyScopedByNamespace(t *testing.T) {
	if got := (sessionStore{ns: ""}).key("abc"); got != "ask:session:abc" {
		t.Fatalf("default library key = %q", got)
	}
	if got := (sessionStore{ns: "t1"}).key("abc"); got != "ns:t1:ask:session:abc" {
		t.Fatalf("tenant key = %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "serve"); got != "serve" {
		t.Fatalf("empty override: got %q", got)
	}
	if got := firstNonEmpty("req", "serve"); got != "req" {
		t.Fatalf("override wins: got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("both empty: got %q", got)
	}
}

func TestSessionFaceConcurrentNamespaces(t *testing.T) {
	alphaRead := make(chan struct{})
	releaseAlpha := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseAlpha) }) }
	savedKeys := make(chan string, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/kv/ns:alpha:ask:session:shared":
			close(alphaRead)
			select {
			case <-releaseAlpha:
			case <-r.Context().Done():
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/kv/ns:beta:ask:session:shared":
			_, _ = io.WriteString(w, `{"id":"shared","title":"beta","messages":[]}`)
		case r.Method == http.MethodPut:
			savedKeys <- strings.TrimPrefix(r.URL.Path, "/v1/kv/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected backend request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer backend.Close()
	defer unblock()

	mux := http.NewServeMux()
	registerSessionFace(mux, client.New(backend.URL), "serve")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alphaDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"id":"shared","title":"alpha","ns":"alpha"}`))
		mux.ServeHTTP(w, r.WithContext(ctx))
		alphaDone <- w
	}()
	select {
	case <-alphaRead:
	case <-ctx.Done():
		t.Fatal("alpha POST did not reach its backend GET")
	}

	beta := httptest.NewRecorder()
	mux.ServeHTTP(beta, httptest.NewRequest(http.MethodGet, "/v1/sessions/shared?ns=beta", nil).WithContext(ctx))
	if beta.Code != http.StatusOK {
		t.Fatalf("beta GET: status %d, body %s", beta.Code, beta.Body.String())
	}
	unblock()
	select {
	case alpha := <-alphaDone:
		if alpha.Code != http.StatusCreated {
			t.Fatalf("alpha POST: status %d, body %s", alpha.Code, alpha.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("alpha POST did not complete")
	}
	select {
	case key := <-savedKeys:
		if key != "ns:alpha:ask:session:shared" {
			t.Fatalf("alpha POST saved to %q, want alpha namespace", key)
		}
	default:
		t.Fatal("alpha POST did not save a session")
	}
}

func TestSessionFaceNamespaceMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for _, namespace := range []string{"", "tenant", "bad:ns"} {
			paths := []string{"/v1/sessions/shared"}
			if method == http.MethodPost {
				paths = []string{"/v1/sessions"}
			} else if method == http.MethodGet {
				paths = append(paths, "/v1/sessions")
			}
			for _, path := range paths {
				t.Run(method+"/"+namespace+path, func(t *testing.T) {
					wantNS := firstNonEmpty(namespace, "serve")
					requests := make(chan string, 4)
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests <- r.Method + " " + r.URL.String()
						if namespace == "bad:ns" {
							t.Error("invalid namespace reached backend")
						}
						if r.URL.Path == "/v1/kv" {
							if got := r.URL.Query().Get("prefix"); got != "ns:"+wantNS+":ask:session:" {
								t.Errorf("list prefix = %q", got)
							}
							_, _ = io.WriteString(w, `{"keys":[]}`)
							return
						}
						if want := "/v1/kv/ns:" + wantNS + ":ask:session:shared"; r.URL.Path != want {
							t.Errorf("backend path = %q, want %q", r.URL.Path, want)
						}
						if r.Method == http.MethodGet {
							_, _ = io.WriteString(w, `{"id":"shared","messages":[]}`)
						} else {
							w.WriteHeader(http.StatusNoContent)
						}
					}))
					defer backend.Close()
					mux := http.NewServeMux()
					registerSessionFace(mux, client.New(backend.URL), "serve")
					// GET uses the query; POST and DELETE use the body, not the query.
					queryNS, bodyNS := "ignored", namespace
					if method == http.MethodGet {
						queryNS, bodyNS = namespace, "ignored"
					}
					body := `{"id":"shared","ns":"` + bodyNS + `"}`
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(method, path+"?ns="+queryNS, strings.NewReader(body)))
					wantStatus := http.StatusOK
					wantRequests := 1
					if method == http.MethodPost {
						wantStatus = http.StatusCreated
						wantRequests = 2
					}
					if namespace == "bad:ns" {
						wantStatus = http.StatusBadRequest
						wantRequests = 0
					}
					if w.Code != wantStatus {
						t.Errorf("status = %d, want %d; body %s", w.Code, wantStatus, w.Body.String())
					}
					if got := len(requests); got != wantRequests {
						t.Errorf("backend requests = %d, want %d", got, wantRequests)
					}
				})
			}
		}
	}
}

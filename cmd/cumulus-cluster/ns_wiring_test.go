package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/monitor"
)

// The session KV keys are scoped through ns.KV — a tenant's session list
// must never surface another tenant's sessions (same key prefix discipline the
// collections follow via "ns:coll" composite identities).
func TestSessionKeyScopedByNamespace(t *testing.T) {
	if got := (sessionStore{ns: ""}).key("abc"); got != "clus:session:abc" {
		t.Fatalf("default library key = %q", got)
	}
	if got := (sessionStore{ns: "t1"}).key("abc"); got != "ns:t1:clus:session:abc" {
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

// A request whose store read is still in flight must not steer a second
// tenant's request: per-request store scoping is what keeps the two key spaces
// apart, so the interleave is forced rather than hoped for.
func TestSessionFaceConcurrentNamespaces(t *testing.T) {
	p := newTestPort()
	alphaRead := make(chan struct{})
	releaseAlpha := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseAlpha) }) }
	defer unblock()
	p.OnKVGet = func(key string) error {
		if key != "ns:alpha:clus:session:shared" {
			return nil
		}
		close(alphaRead)
		<-releaseAlpha
		return contract.ErrNotFound // alpha legitimately starts its own session
	}
	p.seed("ns:beta:clus:session:shared", `{"id":"shared","title":"beta","messages":[]}`)

	mux := http.NewServeMux()
	registerSessionFace(mux, p, "serve")
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
		t.Fatal("alpha POST did not reach its store read")
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

	_, puts, _, _ := p.calls()
	if len(puts) != 1 || puts[0] != "ns:alpha:clus:session:shared" {
		t.Fatalf("alpha POST wrote %v, want the alpha namespace key", puts)
	}
}

func TestMonitorKnowledgeIsRequestLocal(t *testing.T) {
	tr := monitor.New()
	global := &monitor.Knowledge{Clusters: 99}
	tr.WithKnowledge(global)
	alphaRead := make(chan struct{})
	releaseAlpha := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(releaseAlpha) }) }
	defer unblock()
	mux := http.NewServeMux()
	registerMonitorFace(mux, tr, t.TempDir(), "beta", func(ctx context.Context, name string) *monitor.Knowledge {
		switch name {
		case "alpha":
			close(alphaRead)
			select {
			case <-releaseAlpha:
			case <-ctx.Done():
			}
			return &monitor.Knowledge{Clusters: 1}
		case "beta":
			return &monitor.Knowledge{Clusters: 2}
		default:
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alphaDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/monitor/overview?ns=alpha", nil).WithContext(ctx))
		alphaDone <- w
	}()
	select {
	case <-alphaRead:
	case <-ctx.Done():
		t.Fatal("alpha did not reach knowledge read")
	}
	w, beta := serveJSON(t, mux, http.MethodGet, "/v1/monitor/knowledge?ns=beta", nil)
	if w.Code != http.StatusOK || beta["clusters"] != float64(2) {
		t.Fatalf("beta knowledge: %d %v", w.Code, beta)
	}
	// This invariant also deterministically catches the old mutate-then-read
	// implementation, even when the two snapshot calls happen not to overlap.
	if tr.KnowledgeStats() != global {
		t.Fatal("HTTP knowledge read mutated the shared tracker")
	}
	unblock()
	select {
	case w := <-alphaDone:
		var s monitor.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != http.StatusOK || s.Knowledge == nil || s.Knowledge.Clusters != 1 {
			t.Fatalf("alpha knowledge crossed namespaces: %s (%v)", w.Body.String(), err)
		}
	case <-ctx.Done():
		t.Fatal("alpha did not complete")
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/monitor/knowledge", 2},
		{"/v1/monitor/knowledge?ns=missing", 0},
	} {
		w, out := serveJSON(t, mux, http.MethodGet, tc.path, nil)
		if w.Code != http.StatusOK || (tc.want == 0 && out != nil) || (tc.want != 0 && out["clusters"] != float64(tc.want)) {
			t.Fatalf("knowledge fallback: %d %v", w.Code, out)
		}
	}
	for _, block := range []string{"overview", "knowledge", "system", "llm", "retrieval", "namespaces"} {
		w, _ := serveJSON(t, mux, http.MethodGet, "/v1/monitor/"+block+"?ns=bad:ns", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid namespace accepted for %s", block)
		}
	}
	if tr.KnowledgeStats() != global {
		t.Fatal("request knowledge persisted globally")
	}
}

// Every method resolves the namespace where that method actually reads it
// (GET: query; POST/DELETE: body), touches only that tenant's keys, and refuses
// a malformed namespace before reaching the store at all.
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
					prefix := "ns:" + wantNS + ":clus:session:"
					p := newTestPort()
					// Requests aimed at the "shared" document find one already
					// stored; the list path legitimately sees none.
					if method == http.MethodPost || strings.HasSuffix(path, "/shared") {
						p.seed(prefix+"shared", `{"id":"shared","messages":[]}`)
					}
					mux := http.NewServeMux()
					registerSessionFace(mux, p, "serve")
					// GET uses the query; POST and DELETE use the body, not the query.
					queryNS, bodyNS := "ignored", namespace
					if method == http.MethodGet {
						queryNS, bodyNS = namespace, "ignored"
					}
					body := `{"id":"shared","ns":"` + bodyNS + `"}`
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(method, path+"?ns="+queryNS, strings.NewReader(body)))

					wantStatus := http.StatusOK
					if method == http.MethodPost {
						wantStatus = http.StatusCreated
					}
					if namespace == "bad:ns" {
						wantStatus = http.StatusBadRequest
					}
					if w.Code != wantStatus {
						t.Errorf("status = %d, want %d; body %s", w.Code, wantStatus, w.Body.String())
					}

					gets, puts, dels, lists := p.calls()
					if namespace == "bad:ns" {
						if len(gets)+len(puts)+len(dels)+len(lists) != 0 {
							t.Fatalf("invalid namespace reached the store: gets=%v puts=%v dels=%v lists=%v", gets, puts, dels, lists)
						}
						return
					}
					touched := append(append(append(append([]string{}, gets...), puts...), dels...), lists...)
					for _, key := range touched {
						if !strings.HasPrefix(key, prefix) {
							t.Errorf("store key %q does not carry namespace %q", key, wantNS)
						}
					}
					wantGet, wantPut, wantDel, wantList := 0, 0, 0, 0
					switch method {
					case http.MethodPost:
						wantGet, wantPut = 1, 1
					case http.MethodDelete:
						wantDel = 1
					case http.MethodGet:
						if strings.HasSuffix(path, "/shared") {
							wantGet = 1
						} else {
							wantList = 1
						}
					}
					if len(gets) != wantGet || len(puts) != wantPut || len(dels) != wantDel || len(lists) != wantList {
						t.Errorf("store calls = get:%v put:%v del:%v list:%v, want get:%d put:%d del:%d list:%d",
							gets, puts, dels, lists, wantGet, wantPut, wantDel, wantList)
					}
				})
			}
		}
	}
}

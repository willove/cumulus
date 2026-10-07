package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

func testServer() *Server {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484，变更需值班经理审批。"},
		{ID: "noise-1", Body: "连接池巡检记录：连接池每季度检查一次，记录在案备查。"},
	})
	s := New(idx, func(q string, ws []qaflow.EvidenceWindow) (qaflow.Answer, qaflow.Usage, error) {
		if len(ws) == 0 {
			return qaflow.Answer{}, qaflow.Usage{}, errNoWindows
		}
		ans := qaflow.Answer{}
		for _, w := range ws {
			ans.Citations = append(ans.Citations, w.SourceID+"#"+w.Span)
		}
		ans.Text = ws[0].Text
		return ans, qaflow.Usage{CostKnown: false}, nil
	}, 3, 160)
	return s
}

var errNoWindows = &noWindows{}

type noWindows struct{}

func (*noWindows) Error() string { return "no windows" }

func post(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/qa", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response not json: %v", err)
		}
	}
	return rec, out
}

// 契约：一次问答的响应 = 完整 committed view（答案只是其中一个字段）。
func TestQAReturnsFullRecord(t *testing.T) {
	s := testServer()
	rec, out := post(t, s, `{"question":"连接池最大连接数是多少"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, field := range []string{"question", "answer", "refused", "citations", "route", "escalation", "reuse", "coverage", "eviction", "rerank", "windows", "usage"} {
		if _, ok := out[field]; !ok {
			t.Fatalf("response must carry %q (committed view), got keys %v", field, keysOf(out))
		}
	}
	if out["answer"] == "" {
		t.Fatal("answer must be present")
	}
	cits, _ := out["citations"].([]any)
	if len(cits) == 0 {
		t.Fatal("citations must resolve to windows")
	}
	route, _ := out["route"].(map[string]any)
	if route["action"] != "fast" {
		t.Fatalf("good signals must route fast: %v", route)
	}
	if _, ok := route["signals"]; !ok {
		t.Fatal("route must carry its signals (auditable)")
	}
}

// 契约：没有证据 → 拒答（诚实的不知道），不是 500 也不是硬答。
func TestQARefusesWithoutEvidence(t *testing.T) {
	s := testServer()
	rec, out := post(t, s, `{"question":"量子引力飞船怎么造"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refusal is a valid outcome (200), got %d", rec.Code)
	}
	if out["refused"] != true {
		t.Fatalf("must refuse without evidence: %v", out)
	}
	if out["answer"] != "" {
		t.Fatal("refused answer must be empty")
	}
}

// 契约：同会话同问题 → 第二次复用命中（不重新检索）。
func TestQAReuseWithinSession(t *testing.T) {
	s := testServer()
	post(t, s, `{"question":"连接池最大连接数是多少","session":"s1"}`)
	_, out := post(t, s, `{"question":"连接池最大连接数是多少","session":"s1"}`)
	reuse, _ := out["reuse"].(map[string]any)
	if reuse["hit"] != true {
		t.Fatalf("second ask must reuse: %v", reuse)
	}
}

// 契约：错误输入 → 4xx 带 error 字段（不许 500 糊脸）。
func TestQABadRequests(t *testing.T) {
	s := testServer()
	rec, _ := post(t, s, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json must be 400, got %d", rec.Code)
	}
	rec2, _ := post(t, s, `{"question":""}`)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("empty question must be 400, got %d", rec2.Code)
	}
}

func TestHealth(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health must be 200, got %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["corpus_docs"].(float64) != 3 {
		t.Fatalf("health must report corpus size, got %v", out)
	}
}

// 分类器可见面：可选组件的启停在 HTTP 上看得见。
func TestStatusSurface(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, k := range []string{"synthesis", "embedder", "reuse", "escalate"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("status must list %q", k)
		}
	}
	if out["synthesis"] != true || out["embedder"] != false {
		t.Fatalf("status must reflect assembly: %v", out)
	}
}

// 方法不对 → 405。
func TestQAMethodGuard(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/qa", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on /v1/qa must be 405, got %d", rec.Code)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

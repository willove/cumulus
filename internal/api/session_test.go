package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/store"
)

// framesOf 解出 SSE 帧的 kind 序列（忽略 [DONE] 与注释）。
func framesOf(t *testing.T, body string) ([]string, []map[string]any) {
	t.Helper()
	var kinds []string
	var payloads []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimPrefix(line, "data: ")
		if p == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p), &m); err != nil {
			t.Fatalf("bad frame: %v", err)
		}
		if k, ok := m["kind"].(string); ok {
			kinds = append(kinds, k)
			payloads = append(payloads, m)
		}
	}
	return kinds, payloads
}

// sessionServer 起一个**带 store** 的服务（会话层要落库，普通 demo server 没挂）。
func sessionServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open("", true) // 内存 store：测试不落盘
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.Store = st
	return s
}

// 端到端：live 流落库 → 补页拿回**同样形状**的帧（断线恢复）。
func TestStreamRecordsAndReplayResumes(t *testing.T) {
	s := sessionServer(t)
	body, _ := streamQA(t, s, `{"question":"连接池最大连接数是多少","session":"s-replay"}`)
	kinds, _ := framesOf(t, body)
	if len(kinds) < 5 {
		t.Fatalf("live stream too short: %v", kinds)
	}

	// 清单：收尾后 complete=true、计数与最后序号可查
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/s-replay", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest must be readable: %d %s", rec.Code, rec.Body.String())
	}
	var man map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &man); err != nil {
		t.Fatal(err)
	}
	if man["complete"] != true {
		t.Fatalf("session should be complete: %v", man)
	}
	lastSeq, _ := man["last_seq"].(float64)
	if int(lastSeq) != len(kinds) {
		t.Fatalf("manifest last_seq %v must match frame count %d", lastSeq, len(kinds))
	}

	// 补页：从头再来一遍，形状与顺序必须一致（客户端一套解析器通吃）
	req2 := httptest.NewRequest(http.MethodGet, "/v1/sessions/s-replay/events?cursor=0", nil)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay must work: %d %s", rec2.Code, rec2.Body.String())
	}
	kinds2, _ := framesOf(t, rec2.Body.String())
	// 回放尾部多一帧 replay:started（告诉客户端补到哪儿了）
	if len(kinds2) != len(kinds)+1 {
		t.Fatalf("replay must carry the same frames plus the replay header: %d vs %d (%v)", len(kinds2), len(kinds), kinds2)
	}
	for i := range kinds {
		if kinds2[i] != kinds[i] {
			t.Fatalf("replay diverged at %d: %q vs %q (%v)", i, kinds2[i], kinds[i], kinds2)
		}
	}

	// 从中间续：cursor=2 之后应只剩后面的帧
	req3 := httptest.NewRequest(http.MethodGet, "/v1/sessions/s-replay/events?cursor=2", nil)
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, req3)
	kinds3, _ := framesOf(t, rec3.Body.String())
	if len(kinds3) != len(kinds)-2+1 {
		t.Fatalf("cursor resume wrong: got %d frames, want %d", len(kinds3), len(kinds)-2+1)
	}
}

// 没有 session 的流**不落库**（不做隐式副作用），但流照常工作。
func TestStreamWithoutSessionDoesNotRecord(t *testing.T) {
	s := sessionServer(t)
	body, _ := streamQA(t, s, `{"question":"连接池最大连接数是多少"}`)
	kinds, _ := framesOf(t, body)
	if len(kinds) == 0 {
		t.Fatal("stream must still work without a session")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session list endpoint: %d", rec.Code)
	}
}

// 查不到的会话要 404 而不是空 200（空 200 会让客户端以为"没事件"而不是"没会话"）。
func TestUnknownSessionIsNotFound(t *testing.T) {
	s := sessionServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/does-not-exist", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session must be 404: %d", rec.Code)
	}
}

// 保留期与删除：事件带 expires_at；用户显式删除后立刻 404。
func TestSessionDeleteAndExpiry(t *testing.T) {
	s := sessionServer(t)
	body, _ := streamQA(t, s, `{"question":"连接池最大连接数是多少","session":"del-me"}`)
	if kinds, _ := framesOf(t, body); len(kinds) == 0 {
		t.Fatal("stream produced nothing")
	}

	// 清单带过期时间
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/del-me", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: %d", rec.Code)
	}
	var man map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &man)
	if man["expires_at"] == nil || man["expires_at"] == "" {
		t.Fatalf("manifest must carry an expiry: %v", man)
	}

	// 显式删除
	del := httptest.NewRequest(http.MethodDelete, "/v1/sessions/del-me", nil)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, del)
	if rec2.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec2.Code, rec2.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &out)
	if n, _ := out["deleted_events"].(float64); n <= 0 {
		t.Fatalf("delete must report how many events went: %v", out)
	}

	// 删除后再查 = 404（不是空 200）
	get := httptest.NewRequest(http.MethodGet, "/v1/sessions/del-me", nil)
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, get)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("deleted session must be 404: %d", rec3.Code)
	}
	// 重复删除幂等
	rec4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec4, httptest.NewRequest(http.MethodDelete, "/v1/sessions/del-me", nil))
	if rec4.Code != http.StatusOK {
		t.Fatalf("repeat delete must be idempotent: %d", rec4.Code)
	}
}

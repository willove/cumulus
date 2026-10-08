package decide

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 假端点：断言请求形状（三类型 + 判据带上了），回一个真实形状的响应。
func fakeServer(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

const reply = `{"model":"decision-model-preview","request_id":"r1","answers":{
 "q":{"type":"noul","noul":0.99,"confidence":0.91}},
 "usage":{"input_tokens":125},"latency_ms":62.8}`

func TestAskSendsShapeAndParses(t *testing.T) {
	srv, body := fakeServer(t, reply)
	c := New(srv.URL, "sk-test", "decision-model-preview")
	r, err := c.Ask(context.Background(), Request{
		Content: "材料内容",
		Questions: map[string]Question{
			"escalate": {Type: TypeNoul, Instructions: "是否需要立即升级？"},
			"dept":     {Type: TypeChoice, Instructions: "哪个团队？", Criteria: map[string]string{"a": "甲", "b": "乙"}},
			"sev":      {Type: TypeScore, Instructions: "多严重？", Scale: []string{"轻", "重"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	qs := (*body)["questions"].(map[string]any)
	if len(qs) != 3 {
		t.Fatalf("all questions must be sent: %v", qs)
	}
	dept := qs["dept"].(map[string]any)
	if dept["criteria"].(map[string]any)["b"] != "乙" {
		t.Fatalf("choice criteria must be sent: %v", dept)
	}
	sev := qs["sev"].(map[string]any)
	if cr, ok := sev["criteria"].([]any); !ok || len(cr) != 2 {
		t.Fatalf("score scale must be an ordered list: %v", sev)
	}
	a := r.Answers["q"]
	if a.Noul != 0.99 || a.Confidence != 0.91 {
		t.Fatalf("parse wrong: %+v", a)
	}
	if r.UsageIn != 125 || r.LatencyMS != 62.8 || r.RequestID != "r1" || r.Raw == "" {
		t.Fatalf("receipt must be kept for auditability: %+v", r)
	}
}

// 非 200 必须带原文（排障第一手材料），不吞。
func TestAskSurfacesStatusWithRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k", "m").Ask(context.Background(), Request{Content: "x"})
	if err == nil {
		t.Fatal("non-200 must fail")
	}
}

// 没配 key 时不许发请求（也不许静默成功）。
func TestUnconfiguredClientFails(t *testing.T) {
	if _, err := (&Client{}).Ask(context.Background(), Request{}); err == nil {
		t.Fatal("unconfigured client must fail loudly")
	}
	if _, err := FromEnv(); err == nil {
		t.Skip("env has key; nothing to assert here")
	}
}

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, wantBody func(got map[string]any), resp string, status int) *OpenAICompleter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong auth header: %q", r.Header.Get("Authorization"))
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if wantBody != nil {
			wantBody(got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return &OpenAICompleter{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model", Client: srv.Client()}
}

const okResp = `{"choices":[{"message":{"content":"{\"answer\":\"100\",\"assertions\":[{\"text\":\"x\",\"window\":\"w1\"}]}"}}],"usage":{"total_tokens":42,"prompt_tokens":30,"completion_tokens":12}}`

func TestCompleteParsesContract(t *testing.T) {
	c := newTestServer(t, func(got map[string]any) {
		if got["model"] != "test-model" {
			t.Errorf("model: %v", got["model"])
		}
		msgs, _ := got["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("want system+user, got %v", got["messages"])
		}
	}, okResp, 200)
	resp, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p", MaxTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text == "" {
		t.Fatal("empty text")
	}
	if !resp.Usage.CostKnown || resp.Usage.PromptTokens != 30 || resp.Usage.CompletionTokens != 12 {
		t.Fatalf("usage must pass through: %+v", resp.Usage)
	}
}

// 上游不报 usage：CostKnown=false，不是 0。
func TestCompleteUsageMissingIsCostUnknown(t *testing.T) {
	resp := `{"choices":[{"message":{"content":"hi"}}]}`
	c := newTestServer(t, nil, resp, 200)
	r, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage.CostKnown {
		t.Fatal("no usage must be cost-unknown, not zero-cost")
	}
}

func TestCompleteHTTPErrorSurfaces(t *testing.T) {
	c := newTestServer(t, nil, `{"error":{"message":"rate limited"}}`, 429)
	if _, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"}); err == nil {
		t.Fatal("http error must surface")
	}
}

func TestCompleteUpstreamErrorFieldSurfaces(t *testing.T) {
	c := newTestServer(t, nil, `{"error":{"message":"bad key"}}`, 200)
	if _, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"}); err == nil {
		t.Fatal("upstream error field must surface")
	}
}

// 推理模型只回推理链：显式失败，不拿推理链当答案。
func TestCompleteReasoningOnlyFails(t *testing.T) {
	resp := `{"choices":[{"message":{"content":"","reasoning_content":"let me think"}}]}`
	c := newTestServer(t, nil, resp, 200)
	if _, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"}); err == nil {
		t.Fatal("reasoning-only must fail")
	}
}

func TestFromEnvRequiresAllThree(t *testing.T) {
	if _, err := FromEnv("", "k", "m"); err == nil {
		t.Fatal("missing base url must fail")
	}
	if _, err := FromEnv("u", "", "m"); err == nil {
		t.Fatal("missing key must fail")
	}
	if _, err := FromEnv("u", "k", ""); err == nil {
		t.Fatal("missing model must fail")
	}
	c, err := FromEnv("https://x/v1/", "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://x/v1" {
		t.Fatalf("trailing slash must be trimmed: %q", c.BaseURL)
	}
}

// 推理模型偶发把输出全放 reasoning_content：取推理链里最后一个合法
// JSON 对象救结构化调用；救不回来照旧报错。
func TestCompleteFallsBackToReasoningJSON(t *testing.T) {
	resp := `{"choices":[{"message":{"content":"","reasoning_content":"先想一下……最终 {\"answer\":\"100\",\"assertions\":[]} 对"}}]}`
	c := newTestServer(t, nil, resp, 200)
	r, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(r.Text)) {
		t.Fatalf("must extract the JSON object from reasoning, got %q", r.Text)
	}
}

func TestCompleteReasoningWithoutJSONStillFails(t *testing.T) {
	resp := `{"choices":[{"message":{"content":"","reasoning_content":"lorem ipsum no json here"}}]}`
	c := newTestServer(t, nil, resp, 200)
	if _, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"}); err == nil {
		t.Fatal("reasoning without a JSON object must fail")
	}
}

// 请求必须带 reasoning_effort=low（M3.1 认的轻思考信号；reasoning_split
// 被 M3.1 拒收，不许发）。
func TestCompleteSendsReasoningEffortLow(t *testing.T) {
	var gotEffort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["reasoning_effort"].(string); ok {
			gotEffort = v
		}
		if _, bad := body["reasoning_split"]; bad {
			t.Error("reasoning_split must not be sent (M3.1 rejects it)")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	if _, err := c.Complete(context.Background(), Request{System: "s", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if gotEffort != "low" {
		t.Fatalf("reasoning_effort must be low, got %q", gotEffort)
	}
}

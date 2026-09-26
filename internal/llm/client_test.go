package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAigateSynthesizerRefusalState(t *testing.T) {
	const refusal = `{"summary":"Insufficient evidence.","refuse":true}`
	steps := []struct {
		name        string
		status      int
		content     string
		wantSummary string
		wantRefused bool
		wantErr     string
	}{
		{
			name: "refusal", content: refusal,
			wantSummary: "Insufficient evidence.", wantRefused: true,
		},
		{
			name: "nonrefusal_after_refusal", content: `{"summary":"The answer [1].","refuse":false}`,
			wantSummary: "The answer [1].",
		},
		{
			name: "error_after_nonrefusal", status: http.StatusBadGateway,
			wantErr: "llm: status 502",
		},
		{
			name: "refusal_before_error", content: refusal,
			wantSummary: "Insufficient evidence.", wantRefused: true,
		},
		{
			name: "error_after_refusal", status: http.StatusBadGateway,
			wantErr: "llm: status 502",
		},
		{
			name: "refusal_before_empty_summary", content: refusal,
			wantSummary: "Insufficient evidence.", wantRefused: true,
		},
		{
			name: "empty_summary_after_refusal", content: `{"summary":"  \n","refuse":true}`,
			wantErr: "llm: empty summary",
		},
		{
			name: "refusal_before_empty_response", content: refusal,
			wantSummary: "Insufficient evidence.", wantRefused: true,
		},
		{
			name:    "empty_response_after_refusal",
			wantErr: "llm: no JSON in response",
		},
		{
			name: "refusal_before_parse_error", content: refusal,
			wantSummary: "Insufficient evidence.", wantRefused: true,
		},
		{
			name: "parse_error_after_refusal", content: `{"summary":123,"refuse":true}`,
			wantErr: "json: cannot unmarshal",
		},
	}
	// The synthesizer now runs thinking-disabled first and retries with the
	// thinking pass on failure, so each step implies a request plan: a step
	// whose response succeeds consumes ONE request, a failing step consumes
	// TWO (the retry gets the same response and the same error surfaces).
	// Expanding the steps keeps the per-call assertions identical.
	type req struct {
		status  int
		content string
	}
	var plan []req
	for _, st := range steps {
		ok := st.status == 0 && func() bool {
			var out SynthesizeResult
			return parseJSON(st.content, &out) == nil && strings.TrimSpace(out.Summary) != ""
		}()
		n := 1
		if !ok {
			n = 2
		}
		for i := 0; i < n; i++ {
			plan = append(plan, req{st.status, st.content})
		}
	}
	request := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if request >= len(plan) {
			t.Error("unexpected extra request")
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		step := struct {
			status  int
			content string
		}{plan[request].status, plan[request].content}
		request++
		status := step.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"content": step.content},
			}},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	s := &AigateSynthesizer{Client: &ChatClient{
		BaseURL: server.URL, HTTPClient: server.Client(),
	}}
	if s.Refused() {
		t.Fatal("new synthesizer must not report refusal")
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			summary, err := s.Synthesize(context.Background(), "What is the answer?", nil)
			if step.wantErr == "" {
				if err != nil {
					t.Fatalf("Synthesize: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), step.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, step.wantErr)
			}
			if summary != step.wantSummary {
				t.Errorf("summary = %q, want %q", summary, step.wantSummary)
			}
			if got := s.Refused(); got != step.wantRefused {
				t.Errorf("Refused() = %v, want %v", got, step.wantRefused)
			}
		})
	}
}

func TestParseScoreJSONRubricBands(t *testing.T) {
	cases := []struct {
		raw   string
		score float64
	}{
		{`{"score": 1, "reasoning": "无关"}`, 1},
		{"Here you go:\n{\"score\": 5, \"reasoning\": \"有关键词\"}\n", 5},
		{`{"score": 9, "reasoning": "直接答案"}`, 9},
	}
	for _, c := range cases {
		sc, why, err := ParseScoreJSON(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if sc != c.score {
			t.Fatalf("score %v want %v", sc, c.score)
		}
		if why == "" {
			t.Fatal("reasoning required")
		}
	}
	if _, _, err := ParseScoreJSON("not json"); err == nil {
		t.Fatal("must reject non-JSON")
	}
}

func TestTruncate(t *testing.T) {
	s := truncate(strings.Repeat("a", 50), 10)
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("truncate: %q", s)
	}
}

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 造一个流式假上游：把给定的帧按 SSE 形状吐出来。
func sseServer(t *testing.T, frames []string, usage bool) (*httptest.Server, *bool) {
	t.Helper()
	streamed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		streamed, _ = body["stream"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		if usage {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[],"usage":{"total_tokens":42,"prompt_tokens":30,"completion_tokens":12}}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &streamed
}

func frameOf(reasoning, content string) string {
	d := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}}}}
	delta := d["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if content != "" {
		delta["content"] = content
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// 两个通道分开到达：思考与正文互不串味。
func TestStreamSeparatesReasoningFromContent(t *testing.T) {
	srv, streamed := sseServer(t, []string{
		frameOf("先看覆盖度，", ""),
		frameOf("再看边际。", "最大连接数"),
		frameOf("", "是 100。"),
	}, true)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}

	var reasoning, content strings.Builder
	resp, err := c.Stream(context.Background(), Request{System: "s", Prompt: "p"}, func(ch Chunk) error {
		reasoning.WriteString(ch.Reasoning)
		content.WriteString(ch.Content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !*streamed {
		t.Fatal("request must set stream=true")
	}
	if reasoning.String() != "先看覆盖度，再看边际。" {
		t.Fatalf("reasoning channel wrong: %q", reasoning.String())
	}
	if content.String() != "最大连接数是 100。" {
		t.Fatalf("content channel wrong: %q", content.String())
	}
	if resp.Text != content.String() {
		t.Fatalf("final text must equal the accumulated content: %q vs %q", resp.Text, content.String())
	}
	if !resp.Usage.CostKnown || resp.Usage.CompletionTokens != 12 {
		t.Fatalf("usage must be read when present: %+v", resp.Usage)
	}
}

// 流式端点常常**不回 usage** → 成本未知（不是 0）。
func TestStreamWithoutUsageIsCostUnknown(t *testing.T) {
	srv, _ := sseServer(t, []string{frameOf("", "答案")}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	resp, err := c.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.CostKnown {
		t.Fatal("no usage in stream must mean CostKnown=false, not zero cost")
	}
	if resp.Text != "答案" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
}

// 只有思考、没有正文：能救 JSON 就救，救不回来**报错**（不许拿思考链当答案）。
func TestStreamOnlyReasoningIsNotAnAnswer(t *testing.T) {
	srv, _ := sseServer(t, []string{frameOf("思考里没有可救的 JSON", "")}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	if _, err := c.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return nil }); err == nil {
		t.Fatal("reasoning-only stream must error, not return text")
	}

	srv2, _ := sseServer(t, []string{frameOf(`推理 {"a":1} 然后确认`, "")}, false)
	c2 := &OpenAICompleter{BaseURL: srv2.URL, APIKey: "k", Model: "m", Client: srv2.Client()}
	resp, err := c2.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return nil })
	if err != nil {
		t.Fatalf("rescuable JSON must be rescued: %v", err)
	}
	if !strings.Contains(resp.Text, `"a"`) {
		t.Fatalf("rescued text wrong: %q", resp.Text)
	}
}

// 消费方报错要**立刻中止**（别继续烧 token）。
func TestStreamConsumerErrorAborts(t *testing.T) {
	srv, _ := sseServer(t, []string{frameOf("", "一"), frameOf("", "二")}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	sentinel := fmt.Errorf("下游挂了")
	_, err := c.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return sentinel })
	if err == nil || !strings.Contains(err.Error(), "下游挂了") {
		t.Fatalf("consumer error must surface: %v", err)
	}
}

// 超长单帧不许被静默截断（思考链很容易超 64KB）。
func TestStreamHandlesOversizedLines(t *testing.T) {
	huge := strings.Repeat("思", 300*1024)
	srv, _ := sseServer(t, []string{frameOf("", huge)}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	resp, err := c.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return nil })
	if err != nil {
		t.Fatalf("oversized line must not break the stream: %v", err)
	}
	if len([]rune(resp.Text)) != len([]rune(huge)) {
		t.Fatalf("oversized line truncated: got %d runes, want %d", len([]rune(resp.Text)), len([]rune(huge)))
	}
}

// 上游报错帧要变成 error（不是静默的空答案）。
func TestStreamUpstreamErrorFrame(t *testing.T) {
	srv, _ := sseServer(t, []string{`{"error":{"message":"overloaded"}}`}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	if _, err := c.Stream(context.Background(), Request{Prompt: "p"}, func(Chunk) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("upstream error must surface: %v", err)
	}
}

// 能力询问：实现了 Streamer 就是支持流式（读数可见，不靠猜）。
func TestSupportsStream(t *testing.T) {
	srv, _ := sseServer(t, []string{frameOf("", "x")}, false)
	c := &OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m"}
	if !SupportsStream(c) {
		t.Fatal("OpenAICompleter supports streaming")
	}
	if SupportsStream(offlineStub{}) {
		t.Fatal("a non-streaming completer must report no streaming")
	}
	var _ Streamer = c
}

type offlineStub struct{}

func (offlineStub) Complete(context.Context, Request) (Response, error) {
	return Response{Text: "x"}, nil
}

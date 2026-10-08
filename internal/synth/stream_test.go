package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
)

// streamUpstream 造一个**同时会说两条路**的假上游：不带 stream 时回整条 JSON，
// 带 stream 时回 SSE 分片——这样"两条路必须给出同一个答案"才验得成
// （只造一条路的话，另一条路要么报错、要么测不到同一个请求）。
func streamUpstream(t *testing.T, payload string) (*httptest.Server, *bool) {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		called = true
		if stream, _ := body["stream"].(bool); !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"choices":[{"message":{"content":%q}}],"usage":{"total_tokens":9,"prompt_tokens":5,"completion_tokens":4}}`,
				payload)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// 按 **rune** 分片（真实上游按 token 切，不会把一个汉字切成两半；
		// 按字节切会让分片是非法 UTF-8，json.Marshal 直接替换成 U+FFFD——
		// 那是测试自己制造的损坏，不是被测代码的行为）。
		runes := []rune(payload)
		for i := 0; i < len(runes); i += 3 {
			end := i + 3
			if end > len(runes) {
				end = len(runes)
			}
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": string(runes[i:end])},
			}}})
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

// 流式路径：片段逐段到达，且最终答案与整条路径**逐字段一致**（同一请求两条路
// 必须给同一个答案——否则说不清是哪条路的锅）。
func TestSynthesizeStreamMatchesSynthesize(t *testing.T) {
	var answer = `{"answer":"专利法是为了保护专利权人的合法权益","assertions":[{"text":"保护专利权人的合法权益","window":"w1"}],"refused":false}`
	srv, called := streamUpstream(t, answer)
	c := &llm.OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	l := &LLM{Client: c}

	ws := []qaflow.EvidenceWindow{{SourceID: "d1", Span: "rune[0:1]", Text: "第一条 为了保护专利权人的合法权益，制定本法。"}}
	fx := facts.Report{}

	whole, _, err := l.Synthesize("专利法为了什么", ws, fx)
	if err != nil {
		t.Fatal(err)
	}
	// 重开一条流：走流式
	*called = false
	var pieces []string
	streamed, _, err := l.SynthesizeStream("专利法为了什么", ws, fx, func(p StreamPiece) error {
		if p.Content != "" {
			pieces = append(pieces, p.Content)
		}
		if p.Reasoning != "" {
			pieces = append(pieces, "R:"+p.Reasoning)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !*called {
		t.Fatal("streaming path must actually call the upstream")
	}
	if streamed.Text != whole.Text || streamed.Refused != whole.Refused {
		t.Fatalf("two paths must agree: %+v vs %+v", streamed, whole)
	}
	if len(pieces) < 2 {
		t.Fatalf("streaming must deliver more than one piece: %v", pieces)
	}
	if !SupportsStream(l) {
		t.Fatal("LLM must report streaming capability")
	}
	if SupportsStream(Offline) {
		t.Fatal("offline synthesizer must not claim streaming")
	}
}

// 上游不支持流式 → **退回整条**（诚实降级，不是错误）。
func TestSynthesizeStreamFallsBackWhenClientCannotStream(t *testing.T) {
	l := &LLM{Client: noStreamClient{}}
	ws := []qaflow.EvidenceWindow{{SourceID: "d1", Span: "rune[0:1]", Text: "第一条 为了保护专利权人的合法权益，制定本法。"}}
	ans, _, err := l.SynthesizeStream("专利法为了什么", ws, facts.Report{}, func(StreamPiece) error { return nil })
	if err != nil {
		t.Fatalf("fallback must work: %v", err)
	}
	// 桩返回的是拒答：拒答的 Text 本就为空（诚实结局），所以断言 Refused。
	if !ans.Refused {
		t.Fatalf("fallback must return the non-streaming answer as-is: %+v", ans)
	}
}

type noStreamClient struct{}

func (noStreamClient) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Text: `{"answer":"离线答案","assertions":[],"refused":true}`}, nil
}

// 消费者报错要中止（下游不要了就别烧 token）。
func TestSynthesizeStreamConsumerErrorAborts(t *testing.T) {
	srv, _ := streamUpstream(t, `{"answer":"x","assertions":[],"refused":true}`)
	c := &llm.OpenAICompleter{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
	l := &LLM{Client: c}
	ws := []qaflow.EvidenceWindow{{SourceID: "d1", Span: "rune[0:1]", Text: "第一条 为了保护专利权人的合法权益，制定本法。"}}
	_, _, err := l.SynthesizeStream("q", ws, facts.Report{}, func(StreamPiece) error {
		return fmt.Errorf("下游挂了")
	})
	if err == nil || !strings.Contains(err.Error(), "下游挂了") {
		t.Fatalf("consumer error must surface: %v", err)
	}
}

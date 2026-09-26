package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The live failures these pin: MiniMax-M3 quotes legal text with ASCII
// double quotes inside the JSON string values (教师应当"关心、爱护全体
// 学生…") and sometimes emits raw newlines inside those strings. Both are
// invalid JSON, and before the repair pass each cost the whole answer — the
// synthesizer fell back to its template and the user saw a refusal on a
// question the corpus answers (live probe: scorer 9/10 citing 第三十七条,
// synth failing with "invalid character '关' after object key:value pair"
// on exactly that quote). See parseJSON's LLM repair comment.
func TestParseJSONRepairsCJKContentQuotes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string // must appear in the parsed summary
		wantRef bool   // refuse value
	}{
		{
			name:    "inner ascii quotes from quoting the law",
			raw:     `{"summary": "教师应当"关心、爱护全体学生，尊重学生人格"，并可"制止有害行为" [1]。", "citations": [], "confidence_note": "证据充分", "refuse": false}`,
			wantSub: "关心、爱护全体学生",
		},
		{
			name:    "fenced json with inner quotes",
			raw:     "```json\n{\n  \"summary\": \"第三十七条：体罚学生，经教育不改的\"给予行政处分。\",\n  \"citations\": [{\"index\": 1, \"quote\": \"体罚学生\"}],\n  \"confidence_note\": \"证据充分\",\n  \"refuse\": false\n}\n```",
			wantSub: "经教育不改的",
		},
		{
			name:    "raw newlines inside the summary string",
			raw:     "{\n  \"summary\": \"无法律支持。\n第三十七条：体罚学生，经教育不改的。\",\n  \"citations\": [],\n  \"confidence_note\": \"证据充分\",\n  \"refuse\": false\n}",
			wantSub: "第三十七条",
		},
		{
			name:    "refusal stays a refusal",
			raw:     `{"summary": "无依据。法条写道"体罚"但无解释。", "citations": [], "confidence_note": "证据不足", "refuse": true}`,
			wantSub: "无依据",
			wantRef: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got SynthesizeResult
			if err := parseJSON(tc.raw, &got); err != nil {
				t.Fatalf("parseJSON: %v", err)
			}
			if !strings.Contains(got.Summary, tc.wantSub) {
				t.Fatalf("summary %q missing %q", got.Summary, tc.wantSub)
			}
			if got.Refuse != tc.wantRef {
				t.Fatalf("refuse=%v want %v", got.Refuse, tc.wantRef)
			}
		})
	}
}

// Structurally sound responses must parse untouched — the repair pass is a
// failure-path retry, never a rewrite of what already works (frozen prompt
// regression depends on exact values surviving).
func TestParseJSONLeavesSoundJSONAlone(t *testing.T) {
	raw := `{"summary": "a \"quoted\" word, 中文引号“已对”。", "citations": [{"index": 1, "quote": "x"}], "confidence_note": "证据充分", "refuse": false}`
	var got SynthesizeResult
	if err := parseJSON(raw, &got); err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	if got.Summary != `a "quoted" word, 中文引号“已对”。` {
		t.Fatalf("summary mutated: %q", got.Summary)
	}
	if len(got.Citations) != 1 || got.Citations[0].Index != 1 || got.Citations[0].Quote != "x" {
		t.Fatalf("citations mutated: %+v", got.Citations)
	}
	if got.ConfidenceNote != "证据充分" || got.Refuse {
		t.Fatalf("tail fields mutated: %+v", got)
	}
}

// The scanner's string-state rules: a key's closing quote is followed by ':'
// and must stay structural; an empty string value must survive; a
// backslash-escaped quote must not toggle state; content quotes alternate
// “ ” by parity.
func TestRepairModelJSONRules(t *testing.T) {
	in := `{"a": "他说"你好"就走", "b": "", "c": "she said \"hi\" 中文", "d": "plain"}`
	fixed := repairModelJSON(in)
	var m map[string]string
	if err := json.Unmarshal([]byte(fixed), &m); err != nil {
		t.Fatalf("repaired JSON does not parse: %v\nfixed: %s", err, fixed)
	}
	if m["a"] != "他说“你好”就走" {
		t.Fatalf("a=%q", m["a"])
	}
	if m["b"] != "" {
		t.Fatalf("b=%q", m["b"])
	}
	if m["c"] != `she said "hi" 中文` {
		t.Fatalf("c=%q", m["c"])
	}
	if m["d"] != "plain" {
		t.Fatalf("d=%q", m["d"])
	}
}

// Well-formed input must come back byte-identical (the caller uses that as
// the "no repair needed" signal).
func TestRepairModelJSONNoOp(t *testing.T) {
	in := `{"summary": "没有引号问题", "refuse": false}`
	if got := repairModelJSON(in); got != in {
		t.Fatalf("mutated sound JSON: %q", got)
	}
}

// The no-think wiring, pinned at the wire level (no network): Complete must
// NOT send the thinking field, CompleteStructured must send
// thinking{type:disabled}, and the analyzer/expander must actually use the
// structured variant — the whole latency win disappears if someone swaps
// the call back.
func TestCompleteStructuredWireContract(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"content": `{"summary":"ok","refuse":false}`},
			}},
		})
	}))
	defer server.Close()
	chat := &ChatClient{BaseURL: server.URL, HTTPClient: server.Client(), ReasoningSplit: true}

	if _, err := chat.Complete(context.Background(), "x"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := chat.CompleteStructured(context.Background(), "x"); err != nil {
		t.Fatalf("CompleteStructured: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d bodies", len(bodies))
	}
	if strings.Contains(bodies[0], "thinking") {
		t.Errorf("Complete sent a thinking field: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"thinking":{"type":"disabled"}`) {
		t.Errorf("CompleteStructured missing thinking disabled: %s", bodies[1])
	}

	// The analyzer must route through CompleteStructured.
	bodies = nil
	an := &AigateAnalyzer{Client: chat}
	if _, err := an.Analyze(context.Background(), "查询"); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"thinking":{"type":"disabled"}`) {
		t.Errorf("Analyze did not disable thinking: %v", bodies)
	}
}

// The intent-verification guard, pinned at the wire level: a chat verdict
// for a non-greeting query must trigger exactly one thinking-pass retry,
// and the retry's verdict wins. A greeting-shaped query must NOT retry.
func TestAnalyzeChatVerdictVerifiedForRealQuestions(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		content := `{"intent": "chat"}`
		if strings.Contains(string(b), "disabled") {
			content = `{"intent": "chat"}`
		} else {
			content = `{"intent": "search", "primary": {"关键词": 0.9}}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
		})
	}))
	defer server.Close()
	chat := &ChatClient{BaseURL: server.URL, HTTPClient: server.Client(), ReasoningSplit: true}
	an := &AigateAnalyzer{Client: chat}

	// Real question, fast pass wrongly says chat → thinking pass reclassifies.
	bodies = nil
	got, err := an.Analyze(context.Background(), "什么情况下会被行政拘留")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.Intent != "search" {
		t.Fatalf("intent=%q, want the thinking pass to correct chat→search", got.Intent)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 calls (fast + verify), got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"thinking":{"type":"disabled"}`) {
		t.Error("first call must be thinking-disabled")
	}
	if strings.Contains(bodies[1], "thinking") {
		t.Error("verify call must use the thinking pass")
	}

	// Greeting-shaped query → no retry even though the verdict is chat.
	bodies = nil
	got, err = an.Analyze(context.Background(), "你好")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if got.Intent != "chat" {
		t.Fatalf("intent=%q, want chat", got.Intent)
	}
	if len(bodies) != 1 {
		t.Fatalf("greeting must not pay the verify call, got %d calls", len(bodies))
	}
}

// The synthesizer's latency contract at the wire level: thinking-disabled
// first (the measured 3x win), and a failed fast pass falls back to exactly
// one thinking-pass retry whose response is what the caller gets.
func TestSynthesizerNothinkFirstThinkRetry(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		content := `{"summary":"fast pass garbage` // unparseable on the fast pass
		if len(bodies) == 2 {
			content = `{"summary":"slow pass answer [1].","refuse":false}` // retry succeeds
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
		})
	}))
	defer server.Close()
	chat := &ChatClient{BaseURL: server.URL, HTTPClient: server.Client(), ReasoningSplit: true}
	sy := &AigateSynthesizer{Client: chat}

	sum, err := sy.Synthesize(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if sum != "slow pass answer [1]." {
		t.Fatalf("summary = %q, want the retry's answer", sum)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 calls (fast + retry), got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"thinking":{"type":"disabled"}`) {
		t.Error("first call must be thinking-disabled")
	}
	if strings.Contains(bodies[1], "thinking") {
		t.Error("retry must use the thinking pass")
	}
}

// The JSON field scrubber: streamed deltas are the model's raw envelope
// (```json fence, the "summary" key, the closing envelope). Only the field's
// unescaped body may reach the caller, across delta boundaries too.
func TestJSONFieldScrubberStreamsSummaryBodyOnly(t *testing.T) {
	sc := newJSONFieldScrubber("summary")
	// Delimiter chosen to also exercise escapes split across chunks.
	deltas := []string{
		"```json\n{\n  \"summary\": \"## 标题\n\n第一行 \\\"引用\\\" ",
		"与\\n换行，\\u6587\\u5b57 unicode。\",\n  \"citations\": ",
		"[{\"index\": 1}],\n  \"refuse\": false\n}\n```",
	}
	var got strings.Builder
	for _, d := range deltas {
		for _, out := range scrubFeed(sc, d) {
			got.WriteString(out)
		}
	}
	want := "## 标题\n\n第一行 \"引用\" 与\n换行，文字 unicode。"
	if got.String() != want {
		t.Fatalf("scrubbed = %q, want %q", got.String(), want)
	}
}

// No field at all (a non-JSON reply): nothing is forwarded, so the caller
// falls back to the non-streaming path rather than rendering garbage.
func TestJSONFieldScrubberNoFieldForwardsNothing(t *testing.T) {
	sc := newJSONFieldScrubber("summary")
	var n int
	for _, d := range []string{"plain text answer", " with no json"} {
		for _, out := range scrubFeed(sc, d) {
			if out != "" {
				n++
			}
		}
	}
	if n != 0 {
		t.Fatalf("forwarded %d chunks, want 0", n)
	}
}

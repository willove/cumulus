package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/monitor"
	"github.com/willove/cumulus/internal/ns"
)

// The OpenAI-compatible chat face speaks chat.completion.chunk frames that the
// evoke-chat openai adapter (and generic SDKs) consume. These tests pin the
// wire contract on the offline stub stack: the frame ladder, the cumulus
// extension chunks, the finish/usage echo, the [DONE] sentinel — and the two
// request-side refusals (non-stream, no user message).

func newChatWireTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	t.Setenv("LLM_BASE_URL", "") // offline stubs: no network in tests
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	const nsName = "chatwire"
	srcColl := ns.Coll(nsName, "clus_sources")
	if err := engine.EnsureCollection(ctx, srcColl); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Insert(ctx, srcColl, []map[string]any{{
		"_id": "src:d", "title": "手册", "source_type": "md",
		"body": "连接池最大 128，超时 30 秒。", "business_key": "d",
		"lang": "zh", "digest": "x", "status": "active", "version": 1,
	}}); err != nil {
		t.Fatal(err)
	}
	base := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := base.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatal(err)
	}
	buckets := bucket.New(engine)
	if _, err := buckets.Create(ctx, nsName, "聊天线", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	ens := newNSEnsurer(engine, base, "", "clus_sources")
	registerSearchFace(mux, engine, base, "clus_sources", "", false, ens, buckets, monitor.New())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// chunkOf decodes one `data: {...}` frame's JSON payload.
func chunkOf(t *testing.T, line string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("frame is not a data line: %q", line)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
		t.Fatalf("frame %q: %v", line, err)
	}
	return v
}

func TestChatCompletionsStreamsOpenAIWire(t *testing.T) {
	srv := newChatWireTestServer(t)
	body := `{"model":"cumulus","stream":true,"ns":"chatwire","messages":[` +
		`{"role":"user","content":"连接池最大连接数是多少"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 4 {
		t.Fatalf("want the full frame ladder, got %d lines:\n%s", len(lines), raw)
	}
	// 1) 首帧是 role 声明。
	first := chunkOf(t, lines[0])
	if got := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"]; got != "assistant" {
		t.Fatalf("first frame must declare the assistant role, got %v", got)
	}
	// 2) 中段至少一个 delta.content 增量。
	var sawDelta bool
	for _, l := range lines[1:] {
		if l == "data: [DONE]" {
			break
		}
		v := chunkOf(t, l)
		ch, hasChoices := v["choices"].([]any)
		if !hasChoices || len(ch) == 0 {
			continue // cumulus 扩展块不带 choices：标准解析器零感知，这里也一样跳过
		}
		if d, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok && d["content"] != nil && d["content"] != "" {
			sawDelta = true
		}
	}
	if !sawDelta {
		t.Fatalf("no delta.content frame in stream:\n%s", raw)
	}
	// 3) finish_reason=stop + usage.total_tokens 收尾帧，随后 [DONE]。
	var finish map[string]any
	for _, l := range lines {
		if l == "data: [DONE]" {
			break
		}
		v := chunkOf(t, l)
		if ch, ok := v["choices"].([]any); ok && len(ch) > 0 {
			if ch[0].(map[string]any)["finish_reason"] == "stop" {
				finish = v
			}
		}
	}
	if finish == nil {
		t.Fatalf("no finish_reason=stop frame in stream:\n%s", raw)
	}
	if _, ok := finish["usage"].(map[string]any)["total_tokens"]; !ok {
		t.Fatalf("finish frame must echo usage.total_tokens: %v", finish)
	}
	if lines[len(lines)-1] != "data: [DONE]" {
		t.Fatalf("stream must end with the [DONE] sentinel, got %q", lines[len(lines)-1])
	}
	// 4) cumulus 扩展块：done 载荷带运行卡字段，且不带 choices（标准解析器零感知）。
	var doneExt map[string]any
	for _, l := range lines {
		if l == "data: [DONE]" {
			break
		}
		v := chunkOf(t, l)
		if c, ok := v["cumulus"].(map[string]any); ok && c["kind"] == "done" {
			doneExt = c["payload"].(map[string]any)
		}
	}
	if doneExt == nil {
		t.Fatalf("no cumulus done extension in stream:\n%s", raw)
	}
	if doneExt["mode"] == "" || doneExt["conf"] == nil {
		t.Fatalf("done payload must carry the run card: %v", doneExt)
	}
}

func TestChatCompletionsRequestValidation(t *testing.T) {
	srv := newChatWireTestServer(t)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"non-stream refused", `{"model":"cumulus","stream":false,"messages":[{"role":"user","content":"q"}]}`, "stream"},
		{"no user message", `{"model":"cumulus","stream":true,"messages":[{"role":"system","content":"s"}]}`, "user"},
	} {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", tc.name, resp.StatusCode)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if !strings.Contains(out["error"].(string), tc.want) {
			t.Fatalf("%s: error = %v", tc.name, out["error"])
		}
	}
}

// toSearchIn 的折页规则：query 取最后一条 user，history 是之前的 user 轮（截 6），
// session 缺省回落到 OpenAI 的 user 字段。
func TestChatCompletionsFoldsRequest(t *testing.T) {
	stream := new(bool)
	*stream = true
	in := chatCompletionsIn{
		Model:  "cumulus",
		Stream: stream,
		User:   "sess-1",
		Messages: []chatWireMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "u2"},
			{Role: "user", Content: "u3"},
		},
	}
	si, ok := in.toSearchIn()
	if !ok {
		t.Fatal("fold must succeed with user turns present")
	}
	if si.Query != "u3" {
		t.Fatalf("query = %q, want the LAST user turn", si.Query)
	}
	if strings.Join(si.History, ",") != "u1,u2" {
		t.Fatalf("history = %v (assistant/system turns must be skipped)", si.History)
	}
	if si.Session != "sess-1" {
		t.Fatalf("session fallback to user field: %q", si.Session)
	}
	if !si.Prior {
		t.Fatal("chat face always runs the prior arm")
	}
	long := chatCompletionsIn{Messages: []chatWireMessage{}}
	for i := 0; i < 9; i++ {
		long.Messages = append(long.Messages, chatWireMessage{Role: "user", Content: string(rune('a' + i))})
	}
	si2, _ := long.toSearchIn()
	if len(si2.History) != 6 {
		t.Fatalf("history cap = %d, want 6", len(si2.History))
	}
}

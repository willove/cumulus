package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/harness"
)

// sseFrame 是从 SSE 流里解出的一帧。
type sseFrame struct {
	Event string
	Data  map[string]any
}

// parseSSE 解析 `event: <kind>` + `data: <json>` 帧（旧 cumulus 的形状）。
func parseSSE(t *testing.T, body string) (frames []sseFrame, done bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var cur sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				done = true
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				t.Fatalf("data frame is not json (%s): %v", payload, err)
			}
			cur.Data = m
			frames = append(frames, cur)
			cur = sseFrame{}
		}
	}
	return frames, done
}

func streamQA(t *testing.T, s *Server, body string) (string, []*httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/qa/stream", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Body.String(), []*httptest.ResponseRecorder{rec}
}

// 流式端点必须真的把"进度 / 检索日志 / 引用 / 关联文档 / 答案 / 提交视图"流出去。
func TestQAStreamEmitsTheWholeProcess(t *testing.T) {
	s := testServer()
	body, _ := streamQA(t, s, `{"question":"连接池最大连接数是多少","session":"s1"}`)
	frames, done := parseSSE(t, body)
	if !done {
		t.Fatalf("stream must close politely with [DONE]:\n%s", body)
	}
	kinds := map[string]int{}
	var fileFrames, order []string
	for _, f := range frames {
		kinds[f.Event]++
		order = append(order, f.Event)
		if f.Event == "file" {
			info, _ := f.Data["file"].(map[string]any)
			if info == nil {
				t.Fatalf("file frame must carry its payload: %v", f.Data)
			}
			id, _ := info["doc_id"].(string)
			if id == "" {
				t.Fatalf("file frame must name the document: %v", info)
			}
			if _, ok := info["rank"]; !ok {
				t.Fatalf("file frame must carry rank (retrieval log is ordered): %v", info)
			}
			fileFrames = append(fileFrames, id)
		}
	}
	for _, want := range []string{"started", "stage", "file", "citations", "content", "done"} {
		if kinds[want] == 0 {
			t.Fatalf("stream must carry %q frames; got %v", want, order)
		}
	}
	if len(fileFrames) == 0 {
		t.Fatal("retrieval log must reach the client (file frames)")
	}
	// 顺序即真相：started 最先，done 最后。
	if order[0] != "started" || order[len(order)-1] != "done" {
		t.Fatalf("stream order wrong: %v", order)
	}
	// 每帧都必须带 seq 与 run_id（外部按它对账/回放）。
	for _, f := range frames {
		if _, ok := f.Data["seq"]; !ok {
			t.Fatalf("frame %q must carry seq", f.Event)
		}
		if f.Data["run_id"] != "stream-s1" {
			t.Fatalf("frame %q must carry the run id: %v", f.Event, f.Data["run_id"])
		}
	}
	// done 一帧是流式消费者的唯一收尾：答案 + 提交视图 + 计量。
	var last map[string]any
	for _, f := range frames {
		if f.Event == "done" {
			last = f.Data
		}
	}
	doneInfo, _ := last["done"].(map[string]any)
	if doneInfo == nil {
		t.Fatalf("done frame must carry its payload: %v", last)
	}
	for _, key := range []string{"answer", "committed", "counts", "route_action"} {
		if _, ok := doneInfo[key]; !ok {
			t.Fatalf("done payload must carry %q: %v", key, doneInfo)
		}
	}
	if committed, _ := doneInfo["committed"].(string); !strings.Contains(committed, "corpus=") {
		t.Fatalf("committed view must ride on done: %q", committed)
	}
	// 引用帧要可回溯：docid + 原文
	for _, f := range frames {
		if f.Event != "citations" {
			continue
		}
		cits, _ := f.Data["citations"].([]any)
		if len(cits) == 0 {
			t.Fatalf("citations frame must not be empty: %v", f.Data)
		}
		c0, _ := cits[0].(map[string]any)
		if c0["doc_id"] == "" || c0["resolved"] != true {
			t.Fatalf("citations must resolve to text: %v", c0)
		}
	}
}

// 契约：流式是**可选增强**——`/v1/qa` 的字节输出不受挂载影响。
func TestQAJSONEndpointUnchanged(t *testing.T) {
	s := testServer()
	rec, out := post(t, s, `{"question":"连接池最大连接数是多少"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("json endpoint must keep working: %d", rec.Code)
	}
	if out["answer"] == "" {
		t.Fatal("json endpoint answer must be untouched")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("json endpoint must stay json: %s", ct)
	}
}

// 不能流的服务器要**明说**，不给"看起来在流其实全缓冲"的端点。
func TestQAStreamRejectsNonFlushable(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/qa/stream", strings.NewReader(`{"question":"x"}`))
	rec := newNonFlushRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.rec.Code != http.StatusInternalServerError {
		t.Fatalf("non-flushable server must be told: %d %s", rec.rec.Code, rec.rec.Body.String())
	}
}

// 参数错误要在**开流之前**就报（开了流再报 error 帧，客户端会先收到半截历史）。
func TestQAStreamValidatesBeforeStreaming(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/qa/stream", strings.NewReader(`{"question":"  "}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty question must be rejected before the stream opens: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "event: ") {
		t.Fatalf("no SSE frames may be emitted on a bad request: %q", rec.Body.String())
	}
}

// 拒答也是**一等结局**：有 done 帧、写明原因，而不是沉默或 500。
func TestQAStreamRefusalIsAnEvent(t *testing.T) {
	s := testServer()
	body, _ := streamQA(t, s, `{"question":"今天天气怎么样"}`)
	frames, done := parseSSE(t, body)
	if !done {
		t.Fatal("refusal stream must still close politely")
	}
	var gotDone bool
	for _, f := range frames {
		if f.Event != "done" {
			continue
		}
		gotDone = true
		info, _ := f.Data["done"].(map[string]any)
		if info == nil {
			t.Fatalf("done must carry payload: %v", f.Data)
		}
		if info["refused"] == true {
			if info["answer"] != "" {
				t.Fatalf("a refusal must not smuggle an answer: %v", info)
			}
			return
		}
	}
	if !gotDone {
		t.Fatalf("refusal must still send done: %v", frames)
	}
}

// noFlushRecorder **刻意不实现 http.Flusher**（不能内嵌 ResponseRecorder——
// 嵌入会提升它的 Flush 方法，于是"不能流的服务器"这个前提就假了）。
type noFlushRecorder struct {
	rec *httptest.ResponseRecorder
}

func (n noFlushRecorder) Header() http.Header         { return n.rec.Header() }
func (n noFlushRecorder) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n noFlushRecorder) WriteHeader(code int)        { n.rec.WriteHeader(code) }

func newNonFlushRecorder() noFlushRecorder {
	return noFlushRecorder{rec: httptest.NewRecorder()}
}

var _ = harness.KindStarted

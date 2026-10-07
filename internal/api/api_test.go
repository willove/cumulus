package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	gocontext "context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

func testServer() *Server {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484，变更需值班经理审批。"},
		{ID: "noise-1", Body: "连接池巡检记录：连接池每季度检查一次，记录在案备查。"},
	})
	s := New(idx, func(q string, ws []qaflow.EvidenceWindow, _ facts.Report) (qaflow.Answer, qaflow.Usage, error) {
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
func TestQASecondAskDeepens(t *testing.T) {
	s := testServer()
	post(t, s, `{"question":"连接池最大连接数是多少","session":"s1"}`)
	_, out := post(t, s, `{"question":"连接池最大连接数是多少","session":"s1"}`)
	reuse, _ := out["reuse"].(map[string]any)
	if reuse["hit"] != false {
		t.Fatalf("再问不重放（hit 恒 false），该加深：%v", reuse)
	}
	if reason, _ := reuse["reason"].(string); reason == "" || !strings.Contains(reason, "deepen") {
		t.Fatalf("reason 该说明加深：%v", reuse)
	}
	route, _ := out["route"].(map[string]any)
	if route["action"] != "escalate" {
		t.Fatalf("再问该升级：%v", route)
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

// 摄入契约：粘贴入库（内容寻址）、可核（文档端点）、页面可服务。
func TestIngestAndDocAndPage(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{{ID: "seed", Body: "种子文档"}})
	s := NewWithStore(newFakeStore(), nil, 3, 160)
	s.Synth = offlineQA()
	s.index = idx
	h := s.Handler()

	// 摄入文本
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jsonReq("POST", "/v1/ingest", `{"body":"连接池最大连接数默认为 100。"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ing IngestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &ing)
	if ing.ID == "" || ing.CorpusDocs != 1 {
		t.Fatalf("ingest must report id and corpus size: %+v", ing)
	}

	// 摄入后问答能命中新文档（索引热重建）
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, jsonReq("POST", "/v1/qa", `{"question":"连接池最大连接数是多少"}`))
	var qa QAResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &qa)
	if qa.Answer == "" {
		t.Fatalf("freshly ingested doc must be answerable: %+v", qa)
	}

	// 同内容再摄入：upsert 不加量
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, jsonReq("POST", "/v1/ingest", `{"body":"连接池最大连接数默认为 100。"}`))
	var ing2 IngestResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &ing2)
	if ing2.CorpusDocs != 1 || ing2.ID != ing.ID {
		t.Fatalf("same content must dedupe: %+v vs %+v", ing, ing2)
	}

	// 文档端点：引用可核（span 解析出来）
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, httptest.NewRequest("GET", "/v1/doc/"+ing.ID+"?span=rune[0:3]", nil))
	var doc DocResponse
	_ = json.Unmarshal(rec4.Body.Bytes(), &doc)
	if doc.Body == "" || doc.SpanStart != 0 || doc.SpanEnd != 3 {
		t.Fatalf("doc endpoint must return body and parsed span: %+v", doc)
	}

	// 页面
	rec5 := httptest.NewRecorder()
	h.ServeHTTP(rec5, httptest.NewRequest("GET", "/", nil))
	if rec5.Code != http.StatusOK || !strings.Contains(rec5.Body.String(), "cumulus") {
		t.Fatalf("page must be served, got %d", rec5.Code)
	}
}

// fakeStore 是最小可用 store（ingest 路径用）。
type fakeStore struct{ docs map[string]corpusDocShape }

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string]corpusDocShape{}} }

type corpusDocShape struct {
	ID   string
	Body string
}

func (f *fakeStore) EnsureCollection(gocontext.Context, string) error { return nil }
func (f *fakeStore) PutStruct(_ gocontext.Context, coll, id string, v any) error {
	if d, ok := v.(corpus.Doc); ok {
		f.docs[id] = corpusDocShape{ID: d.ID, Body: d.Body}
	}
	return nil
}
func (f *fakeStore) GetStruct(_ gocontext.Context, coll, id string, out any) error {
	d, ok := f.docs[id]
	if !ok {
		return os.ErrNotExist
	}
	if o, ok := out.(*corpus.Doc); ok {
		o.ID, o.Body = d.ID, d.Body
	}
	return nil
}
func (f *fakeStore) ListIDs(_ gocontext.Context, coll string, _ int) ([]string, error) {
	ids := make([]string, 0, len(f.docs))
	for id := range f.docs {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeStore) Delete(gocontext.Context, string, string) error     { return nil }
func (f *fakeStore) PutValue(gocontext.Context, string, []byte) error   { return nil }
func (f *fakeStore) GetValue(gocontext.Context, string) ([]byte, error) { return nil, os.ErrNotExist }
func (f *fakeStore) Health(gocontext.Context) error                     { return nil }

func offlineQA() qaflow.SynthFunc {
	return func(_ string, ws []qaflow.EvidenceWindow, _ facts.Report) (qaflow.Answer, qaflow.Usage, error) {
		if len(ws) == 0 {
			return qaflow.Answer{}, qaflow.Usage{}, errNoWindows
		}
		ans := qaflow.Answer{Text: ws[0].Text}
		for _, w := range ws {
			ans.Citations = append(ans.Citations, w.SourceID+"#"+w.Span)
		}
		return ans, qaflow.Usage{CostKnown: false}, nil
	}
}

func jsonReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// 窗口必须带文档身份（title）——模型靠它知道"第二十二条"是哪部法律的；
// 前端也显示它。回归：曾经只有内容哈希，模型把两部法律的第一十二条搞混。
func TestQAResponseCarriesWindowTitles(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-a", Body: "中华人民共和国甲法\n第一条 甲法的内容。"},
		{ID: "law-b", Body: "中华人民共和国乙法\n第一条 乙法的内容。"},
	})
	s := New(idx, offlineQA(), 3, 160)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jsonReq("POST", "/v1/qa", `{"question":"甲法的内容"}`))
	var qa QAResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &qa)
	if len(qa.Windows) == 0 {
		t.Fatal("no windows")
	}
	if qa.Windows[0].Title != "中华人民共和国甲法" {
		t.Fatalf("window must carry its document title, got %q", qa.Windows[0].Title)
	}
}

// 引用点击钩子：POST /v1/signal 落库，空 kind 拒收。
func TestSignalEndpoint(t *testing.T) {
	s := NewWithStore(newFakeStore(), nil, 3, 160)
	req := httptest.NewRequest(http.MethodPost, "/v1/signal",
		strings.NewReader(`{"session":"s","kind":"cite","target":"d1#rune[0:9]"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cite 该 204，got %d %s", rec.Code, rec.Body.String())
	}
	if s.Signals.Len() != 1 {
		t.Fatalf("该落一条：%d", s.Signals.Len())
	}
	// 空 kind：没类型的信号聚合不了，拒收
	req2 := httptest.NewRequest(http.MethodPost, "/v1/signal", strings.NewReader(`{"kind":""}`))
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("空 kind 该 400，got %d", rec2.Code)
	}
}

// GET /v1/signals：聚合视图（分布 + top 引用）机器可读。
func TestSignalsAggregate(t *testing.T) {
	s := NewWithStore(newFakeStore(), nil, 3, 160)
	s.Signals.Record(knowledge.Signal{Kind: knowledge.SignalCitationClick, Target: "d1#rune[0:9]"})
	s.Signals.Record(knowledge.Signal{Kind: knowledge.SignalCitationClick, Target: "d1#rune[0:9]"})
	req := httptest.NewRequest(http.MethodGet, "/v1/signals", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("200 该给：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cite") || !strings.Contains(body, "d1#rune[0:9]") {
		t.Fatalf("聚合里该有 cite 族与 target：%s", body)
	}
}

func (f *fakeStore) Query(_ gocontext.Context, _ string, _ map[string]any, _, _ int, _ any) (int, error) {
	return 0, nil
}

// Package api 是 HTTP 面：把已验证的问答流程包成可调用服务。
//
// 设计纪律（承 cumulus 的 searchapi 教训，不重复它的坑）：
//   - 一次问答的响应 = 一次 committed view：答案只是其中一个字段，
//     路由判定、升级执行、复用决定、覆盖度、驱逐账、深循环遥测、用量
//     全部回传——调用方看得见“当时为什么这么答”，不只是答案文本；
//   - 每个可选组件（合成/向量/复用/升级）按装配显式启停，缺席在
//     /v1/status 可见，不静默；
//   - 契约由测试钉死（请求/响应形状），改形状先改测试。
package api

import (
	goembed "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/corpus"
	embedPkg "github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

// Server 是一次装配的服务。零件（合成/向量/复用/升级）装配时定死，
// 运行中不热换——热插拔是注册面的事，HTTP 面只把装配结果暴露出去。
// 语料与索引例外：摄入是运行时事件，索引跟着重建（个人库规模，毫秒级）。
type Server struct {
	Store    corpus.Port // 摄入面：语料活着的地方（索引只是它的投影）
	Synth    qaflow.SynthFunc
	Embedder embedPkg.Embedder // 可空：nil = 语义重排/语义尺缺席
	Reuse    *knowledge.ReuseStore
	Escalate func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error)
	TopK     int
	Width    int
	Realm    string
	Options  qaflow.Options

	mu    sync.RWMutex
	index *retrieval.Index
}

// Index 读当前索引（摄入与问答之间只有这一把锁）。
func (s *Server) Index() *retrieval.Index {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.index
}

// Rebuild 从 store 重建索引。摄入（粘贴/链接/看目录）后调用。
func (s *Server) Rebuild(ctx gocontext.Context) (int, error) {
	docs, err := corpus.Load(ctx, s.Store)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index = retrieval.Build(docs)
	return len(docs), nil
}

// New 装配一台服务（索引由调用方给；摄入面用 NewWithStore）。
func New(idx *retrieval.Index, synth qaflow.SynthFunc, topk, width int) *Server {
	s := NewWithStore(nil, synth, topk, width)
	s.index = idx
	return s
}

// NewWithStore 从 store 装配：索引从 store 建，摄入后热重建。
func NewWithStore(st corpus.Port, synth qaflow.SynthFunc, topk, width int) *Server {
	if topk <= 0 {
		topk = 3
	}
	if width <= 0 {
		width = 160
	}
	return &Server{Store: st, Synth: synth, TopK: topk, Width: width, Realm: "default", Reuse: knowledge.NewReuseStore()}
}

// QARequest 是 POST /v1/qa 的请求。
type QARequest struct {
	Question string `json:"question"`
	Session  string `json:"session"` // 空 = 不问复用（一次性问答）
}

// QAResponse 是一次问答的完整记录（committed view 的 HTTP 形状）。
type QAResponse struct {
	Question  string                  `json:"question"`
	Answer    string                  `json:"answer"`
	Refused   bool                    `json:"refused"`
	Reason    string                  `json:"refusal_reason,omitempty"`
	Citations []string                `json:"citations"`
	Analysis  AnalysisView            `json:"analysis"`
	Prior     []PriorView             `json:"prior,omitempty"` // 空 = 未开多信号重排
	Route     RouteView               `json:"route"`
	Escalate  qaflow.EscalationRecord `json:"escalation"`
	Reuse     qaflow.ReuseState       `json:"reuse"`
	Coverage  CoverageView            `json:"coverage"`
	Eviction  EvictionView            `json:"eviction"`
	Rerank    qaflow.RerankState      `json:"rerank"`
	Windows   []WindowView            `json:"windows"`
	Usage     UsageView               `json:"usage"`
}

// AnalysisView 是查询侧理解的快照：意图、IDF 加权主关键词级（着重/降权
// 的取舍看得见）、语料外词（词汇鸿沟信号）。查询侧做了什么，答案旁边
// 直接可查。
type AnalysisView struct {
	Intent  string             `json:"intent"`
	Primary map[string]float64 `json:"primary"` // 语词 → 权重（2.0 着重/1.0 平权）
	OOV     []string           `json:"out_of_corpus_terms,omitempty"`
	Score   float64            `json:"score"` // 主级总权重（稀薄度代理）
}

// RouteView 是路由判定（含信号——可审计）。
type RouteView struct {
	Action  string              `json:"action"`
	Reason  string              `json:"reason"`
	Signals qaflow.RouteSignals `json:"signals"`
}

// CoverageView 是覆盖度 + 语料外词。
type CoverageView struct {
	Value float64  `json:"value"`
	OOV   []string `json:"out_of_corpus_terms,omitempty"`
}

// EvictionView 是驱逐账。
type EvictionView struct {
	Merged  int `json:"merged"`
	Dropped int `json:"dropped"`
}

// WindowView 是一个证据窗口。
// PriorView 是一篇文档的多信号置信（cumulus prior 移植的可视化：
// lexical 无长度归一 / 标题 / 条文结构，融合后置顶归一）。
type PriorView struct {
	DocID   string             `json:"doc_id"`
	Score   float64            `json:"score"`
	Signals map[string]float64 `json:"signals"`
	Title   string             `json:"title,omitempty"`
}

type WindowView struct {
	SourceID string  `json:"source_id"`
	Title    string  `json:"title"` // 文档身份（法律名）——前端要显示"这是哪份文档的第几条"
	Span     string  `json:"span"`
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
}

// HealthResponse 是 GET /v1/health 的响应（类型化——契约从代码生成，
// map[string]any 生成不出契约）。
type HealthResponse struct {
	Status     string `json:"status"`
	CorpusDocs int    `json:"corpus_docs"`
	Realm      string `json:"realm"`
}

// StatusResponse 是 GET /v1/status 的响应：可选组件的启停。cumulus 的
// MCS 静默不触发，缺的就是这一面。
type StatusResponse struct {
	Synthesis bool `json:"synthesis"`
	Embedder  bool `json:"embedder"`
	Reuse     bool `json:"reuse"`
	Escalate  bool `json:"escalate"`
}

// UsageView 是用量账。CostKnown=false 表示上游不报（成本未知不是 0）。
type UsageView struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	CostKnown        bool `json:"cost_known"`
}

// Handler 返回挂好路由的 http.Handler（测试与 serve 共用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/qa", s.handleQA)
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/ingest", s.handleIngest)
	mux.HandleFunc("/v1/doc/", s.handleDoc)
	mux.HandleFunc("/", s.handlePage)
	return mux
}

func (s *Server) handleQA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req QARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "question is required")
		return
	}
	c := context.New(context.Realm(s.Realm))
	if s.Embedder != nil {
		if err := qaflow.BindEmbedder(c, s.Embedder); err != nil {
			writeErr(w, http.StatusInternalServerError, "bind embedder: "+err.Error())
			return
		}
	}
	opts := s.Options
	opts.Reuse = s.Reuse
	opts.Session = req.Session
	if s.Escalate != nil {
		opts.Escalate = s.Escalate
	}
	runner := qaflow.Runner(req.Question, qaflow.BM25Evidence(s.Index(), s.TopK, s.Width), s.Synth, opts)
	if err := runner.Run(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "flow: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.record(c, req.Question))
}

// record 从 context 采一次问答的完整记录。这是 HTTP 面的核心承诺：
// 答案只是其中一个字段。
func (s *Server) record(c *context.Context, question string) QAResponse {
	resp := QAResponse{Question: question, Citations: []string{}}
	if a, ok := context.Get(c, qaflow.KeyAnswer); ok {
		resp.Answer = a.Text
		resp.Refused = a.Refused
		resp.Reason = a.RefusalReason
		resp.Citations = append(resp.Citations, a.Citations...)
	}
	if an, ok := context.Get(c, qaflow.KeyAnalysis); ok {
		resp.Analysis = AnalysisView{Intent: an.Intent, Primary: an.Primary, OOV: an.OOV, Score: an.Score}
	}
	if ps, ok := context.Get(c, qaflow.KeyPrior); ok {
		for _, p := range ps {
			resp.Prior = append(resp.Prior, PriorView{DocID: p.DocID, Score: p.Score, Signals: p.Signals})
		}
	}
	if rd, ok := context.Get(c, qaflow.KeyRoute); ok {
		resp.Route = RouteView{Action: rd.Action, Reason: rd.Reason, Signals: rd.Signals}
	}
	if es, ok := context.Get(c, qaflow.KeyEscalation); ok {
		resp.Escalate = es
	}
	if rs, ok := context.Get(c, qaflow.KeyReuseState); ok {
		resp.Reuse = rs
	}
	if ci, ok := context.Get(c, qaflow.KeyCoverage); ok {
		resp.Coverage = CoverageView{Value: ci.Value, OOV: ci.OOV}
	}
	if ev, ok := context.Get(c, qaflow.KeyEviction); ok {
		resp.Eviction = EvictionView{Merged: len(ev.Merged), Dropped: len(ev.Dropped)}
	}
	if rr, ok := context.Get(c, qaflow.KeyRerank); ok {
		resp.Rerank = rr
	}
	if ws, ok := context.Get(c, qaflow.KeyWindows); ok {
		for _, w := range ws {
			resp.Windows = append(resp.Windows, WindowView{SourceID: w.SourceID, Title: w.Title, Span: w.Span, Text: w.Text, Score: w.Score})
		}
	}
	if u, ok := context.Get(c, qaflow.KeyUsage); ok {
		resp.Usage = UsageView{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CostKnown: u.CostKnown}
	}
	return resp
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	docs := 0
	if idx := s.Index(); idx != nil {
		docs = idx.N
	}
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", CorpusDocs: docs, Realm: s.Realm})
}

// handleStatus 是分类器可见面：哪些可选组件活着、缺什么。cumulus 的
// MCS 静默不触发，缺的就是这一面。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, StatusResponse{
		Synthesis: s.Synth != nil,
		Embedder:  s.Embedder != nil,
		Reuse:     s.Reuse != nil,
		Escalate:  s.Escalate != nil,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

//go:embed web/index.html
var webPage []byte

var _ goembed.FS

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- 摄入（进） ----------

// IngestRequest 是 POST /v1/ingest 的请求：body 直接存；给了 url 就取了
// 再存。零摩擦：粘贴或链接，一步入库。
type IngestRequest struct {
	Body string `json:"body"`
	URL  string `json:"url"`
}

// IngestResponse 回报内容 id（内容寻址：同内容再来是 upsert）与库容量。
type IngestResponse struct {
	ID         string `json:"id"`
	CorpusDocs int    `json:"corpus_docs"`
	Bytes      int    `json:"bytes"`
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "ingest not wired: server started without a store")
		return
	}
	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx := gocontext.Background()
	body := req.Body
	if req.URL != "" {
		fetched, err := ingest.FetchURL(ctx, req.URL)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "fetch: "+err.Error())
			return
		}
		body = fetched
	}
	id, err := ingest.Text(ctx, s.Store, body, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ingest: "+err.Error())
		return
	}
	n, err := s.Rebuild(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rebuild: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, IngestResponse{ID: id, CorpusDocs: n, Bytes: len(body)})
}

// ---------- 文档（引用可核） ----------

// DocResponse 是一篇文档的全文（点击引用看原文）。Span 把引用的 rune
// 坐标带回来，前端据此高亮——“字符级可核”在界面上兑现。
type DocResponse struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	// 位置数据的 0 是有意义的（区间常从 0 开始）——不许 omitempty，
	// 否则前端 span_end > span_start 的判断直接废掉（真跑踩过）
	SpanStart int `json:"span_start"`
	SpanEnd   int `json:"span_end"`
	// 血缘（摄入面的审计面）：原生 UTF-8 还是转码来的、原始字节的摘要
	// ——产物文本证明不了它来自哪份文件，输入摘要可以
	Encoding  string `json:"encoding,omitempty"`
	SrcDigest string `json:"src_digest,omitempty"`
	SrcBytes  int    `json:"src_bytes,omitempty"`
}

func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/doc/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "doc id required")
		return
	}
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not wired")
		return
	}
	var d corpus.Doc
	if err := s.Store.GetStruct(gocontext.Background(), corpus.Collection, id, &d); err != nil {
		writeErr(w, http.StatusNotFound, "doc not found: "+id)
		return
	}
	resp := DocResponse{ID: id, Body: d.Body, Encoding: d.Encoding, SrcDigest: d.SrcDigest, SrcBytes: d.SrcBytes}
	if span := r.URL.Query().Get("span"); span != "" {
		if start, end, ok := parseSpan(span); ok {
			resp.SpanStart, resp.SpanEnd = start, end
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseSpan 解析 "rune[起:止]"。
func parseSpan(s string) (int, int, bool) {
	if !strings.HasPrefix(s, "rune[") || !strings.HasSuffix(s, "]") {
		return 0, 0, false
	}
	inner := s[5 : len(s)-1]
	i := strings.IndexByte(inner, ':')
	if i < 0 {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(inner[:i])
	end, err2 := strconv.Atoi(inner[i+1:])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return start, end, true
}

// ---------- 页面（看） ----------

// handlePage 服务唯一的页面：一张问题框 + 一个摄入框 + 记录的渲染。
// 页面不含业务逻辑——所有判断在 API 侧做过，这里只画。契约门守着形状。
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(webPage)
}

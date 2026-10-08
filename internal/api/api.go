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
	"fmt"
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
	Store corpus.Port // 摄入面：语料活着的地方（索引只是它的投影）
	Synth qaflow.SynthFunc
	// StreamSynth 是**可选**能力：填了它，流式端点会把思考与正文逐段发出。
	// nil = 只支持整条（那就发整段 content 帧，不假装流式）。
	StreamSynth qaflow.StreamSynthFunc
	Embedder    embedPkg.Embedder // 可空：nil = 语义重排/语义尺缺席
	Reuse       *knowledge.ReuseStore
	// Signals 使用信号库（"长"的地基）：再问族服务端推导，cite 族前端
	// 钩子。nil = 不记（库是可选件，缺了问答照常）。
	Signals  *knowledge.SignalStore
	Escalate func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error)
	TopK     int
	Width    int
	Realm    string
	Options  qaflow.Options
	// StrategyVersion 是检索/路由策略的版本标签（进提交视图）。换策略
	// 实现、换阈值程序都要改它——两次运行的策略不同，数字就不可比。
	StrategyVersion string

	mu    sync.RWMutex
	index *retrieval.Index
}

// DefaultStrategyVersion 是本装配的策略版本。改路由/检索策略时改这里
// （或由调用方覆盖 Server.StrategyVersion）——版本标签是承诺，不是注释。
//
// v0.2：检索分词修了混排文本的脚本边界（"iphone6照片流在哪"整串曾变成一个
// df=0 的词，真实问句 10% 因此零候选）——检索语义变了，策略版本必须跟着
// 变，否则 v0.1 与 v0.2 的答案混在一张表里不可比。
const DefaultStrategyVersion = "qa.v0.2"

// corpusVersion 渲染语料版本：条数 + 内容摘要前 12 位。
func (s *Server) corpusVersion() string {
	idx := s.Index()
	if idx == nil || idx.N == 0 {
		return "empty"
	}
	return fmt.Sprintf("n=%d sha=%s", idx.N, idx.ShortDigest())
}

// configVersion 从**生效的装配**渲染配置版本（不是调用方填的标签）：
// 旋钮、可选件在不在场、阈值的校准程序——这些东西变了，答案就可能变。
func (s *Server) configVersion() string {
	parts := []string{fmt.Sprintf("topk=%d", s.TopK), fmt.Sprintf("width=%d", s.Width)}
	if s.Options.Prior {
		parts = append(parts, "prior")
	}
	if s.Options.Abstain != nil {
		parts = append(parts, "abstain")
	}
	if s.Options.Escalate != nil || s.Escalate != nil {
		parts = append(parts, "escalate")
	}
	if s.Embedder != nil {
		parts = append(parts, "embed")
	}
	if s.Options.GroundingFloor > 0 {
		parts = append(parts, fmt.Sprintf("grounding=%.2f", s.Options.GroundingFloor))
	}
	if s.Options.Route.Program != "" {
		parts = append(parts, "route="+s.Options.Route.ProgramName())
	}
	return strings.Join(parts, ",")
}

// strategyVersion 取策略标签（未设置时用默认）。
func (s *Server) strategyVersion() string {
	if s.StrategyVersion != "" {
		return s.StrategyVersion
	}
	return DefaultStrategyVersion
}

// committedVersions 是提交视图的四版本（问答面每次迁移都要记）。
// 信念版本记 none：belief 已从检索路径退役（真语料 A/B 证实全局声望版
// 有害），退役就要说退役，不许留个版本号假装它还在。
func (s *Server) committedVersions() (corpus, config, strategy, belief string) {
	return s.corpusVersion(), s.configVersion(), s.strategyVersion(), "none(belief-retired)"
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
	return &Server{Store: st, Synth: synth, TopK: topk, Width: width, Realm: "default", Reuse: knowledge.NewReuseStore(), Signals: knowledge.NewSignalStore("")}
}

// POST /v1/signal——外部信号入口。目前唯一发送方是引用点击；拒收空
// kind（没类型的信号聚合不了）。
func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req SignalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Kind) == "" {
		writeErr(w, http.StatusBadRequest, "kind is required")
		return
	}
	if s.Signals == nil {
		writeErr(w, http.StatusNotImplemented, "signal store not configured")
		return
	}
	s.Signals.Record(knowledge.Signal{
		Session:  req.Session,
		Kind:     req.Kind,
		Target:   req.Target,
		Question: knowledge.NormalizeQuestion(req.Question),
	})
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/signals——聚合视图（分布 + 每族 top 问句 + 最常被点引用）。
// 这是"下一轮靶子从哪挑"的那张表，机器可读。
func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.Signals == nil {
		writeErr(w, http.StatusNotImplemented, "signal store not configured")
		return
	}
	out := map[string]any{
		"total":  s.Signals.Len(),
		"counts": s.Signals.Counts(),
		"top": map[string]any{
			knowledge.SignalReaskAfterRefusal: questionCounts(s.Signals.TopQuestions(knowledge.SignalReaskAfterRefusal, 10)),
			knowledge.SignalReaskAfterAnswer:  questionCounts(s.Signals.TopQuestions(knowledge.SignalReaskAfterAnswer, 10)),
			knowledge.SignalCitationClick:     citeCounts(s.Signals.TopCitations(10)),
		},
	}
	writeJSON(w, http.StatusOK, out)
}

func questionCounts(sigs []knowledge.Signal) []map[string]any {
	out := make([]map[string]any, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, map[string]any{"question": s.Question, "count": atoiView(s.Note)})
	}
	return out
}

func citeCounts(sigs []knowledge.Signal) []map[string]any {
	out := make([]map[string]any, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, map[string]any{"target": s.Target, "count": atoiView(s.Note)})
	}
	return out
}

func atoiView(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Handler 返回挂好路由的 http.Handler（测试与 serve 共用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/qa", s.handleQA)
	mux.HandleFunc("/v1/qa/stream", s.handleQAStream)
	mux.HandleFunc("/v1/signal", s.handleSignal)
	mux.HandleFunc("/v1/signals", s.handleSignals)
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
	// 提交视图的四版本在受理时定死（一次性迁移只认开始时那一套环境）：
	// 语料版本是内容摘要，配置版本是生效装配的形状，策略是标签，信念
	// 已退役记 none。语料在长，答案必须能说明"这是针对哪一版给的"。
	opts.CorpusVersion, opts.ConfigVersion, opts.StrategyVersion, opts.BeliefVersion = s.committedVersions()
	runner := qaflow.Runner(req.Question, qaflow.BM25Evidence(s.Index(), s.TopK, s.Width), s.Synth, opts)
	if err := runner.Run(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "flow: "+err.Error())
		return
	}
	resp := s.record(c, req.Question)
	// 使用信号（"长"的地基）：同一 session 同一问句再来一次 = 再问。按
	// 上轮答没答分两族（弃权没解决 / 答案没答全）——服务端推导，前端零
	// 改动。无 session 的一次性问答不记（没有"再问"的上下文）。
	if s.Signals != nil && req.Session != "" {
		s.Signals.Remember(req.Session, req.Question, resp.Refused)
	}
	writeJSON(w, http.StatusOK, resp)
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
	if fx, ok := context.Get(c, qaflow.KeyFactReport); ok {
		for _, f := range fx.Facts {
			fv := FactView{ID: f.ID, Query: f.Query, Covered: f.Covered, NearMiss: f.NearMiss, Judge: f.Judge}
			for _, s := range f.Supports {
				fv.Supports = append(fv.Supports, SupportView{SourceID: s.SourceID, Span: s.Span, Score: s.Score})
			}
			resp.Facts = append(resp.Facts, fv)
		}
	}
	if cs, ok := context.Get(c, qaflow.KeyConflicts); ok {
		for _, x := range cs {
			resp.Conflicts = append(resp.Conflicts, ConflictView{FactID: x.FactID, Values: x.Values, SourceIDs: x.SourceIDs})
		}
	}
	if v, ok := context.Get(c, qaflow.KeyAbstain); ok {
		resp.Abstain = &AbstainView{PFail: v.PFail, Action: v.Action, Reason: v.Reason}
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
	// 提交视图取最后一次迁移（这一问就是最后一次）。没有视图 = 流程没
	// 走完 Commit，此时留零值并在字段上看得出来（四版本全空）。
	if views := c.Views(); len(views) > 0 {
		resp.Committed = views[len(views)-1]
	}
	return resp
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	docs := 0
	if idx := s.Index(); idx != nil {
		docs = idx.N
	}
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", CorpusDocs: docs, Realm: s.Realm, CorpusVersion: s.corpusVersion()})
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

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
	gocontext "context"
	goembed "embed"
	"encoding/json"
	"github.com/willove/cumulus/internal/auth"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/corpus"
	embedPkg "github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/knowledge/affinity"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/usage"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server 是一次装配的服务。零件（合成/向量/复用/升级）装配时定死，
// 运行中不热换——热插拔是注册面的事，HTTP 面只把装配结果暴露出去。
// 语料与索引例外：摄入是运行时事件，索引跟着重建（个人库规模，毫秒级）。
type Server struct {
	Store corpus.Port // 摄入面：语料活着的地方（索引只是它的投影）
	// Meter 是**用量计量与配额**（可选件）：nil = 不计量也不限流。
	Meter *usage.Meter
	// HotDocs 是**热区文档数上限**（0 = 全内存）。分层索引的预算：倒排只放热区，
	// 冷文档留词项指纹、按需取回正文（见 versions.buildIndex）。
	HotDocs int
	// DocGen 是**知识文档生成**（可选件）：把证据整理成一篇可核对的文档写回
	// 语料，于是下一轮问答能引用它。nil = 不提供（端点回 501，/v1/status 可见）。
	DocGen *DocGen
	Synth  qaflow.SynthFunc
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

	// Keys 是凭证表（nil/空 = 默认放行，单机开发的老路径不变）。
	// realm 由**凭证推导**，不信客户端声明——声明就是自己说我是谁。
	Keys *auth.Keyring

	mu sync.RWMutex

	// docLocks 是**按生成文档 id** 的串行化锁（见 lockDoc）。
	//
	// 为什么用 sync.Map 而不是 map+Mutex：map 的**零值不可用**、惰性初始化本身
	// 又是竞争源（我用 map+Mutex 时 race detector 直接报了 docLocks 的竞争——
	// 自己新加的锁自己先出事）。sync.Map 的零值可用，读多写少正是它的场景。
	//
	// 为什么不用一把大锁：不同主题的生成互不相干；大锁会把并行的也串起来。
	// 为什么必须有：更新是 read-modify-write，不锁就会**静默丢更新**（并发测试读到
	// 的版本号是 [2 2 2 2 1 1 2 2]——6 次更新凭空消失，且没有任何报错）。
	docLocks sync.Map // docID → *sync.Mutex
	index    *retrieval.Index
	// indexes 是**按 realm 分开的索引**：多租户共用一个实例时，检索面也必须
	// 分开。只在集合上分开、索引还共用着，等于门锁上了窗户开着。
	indexes map[string]*retrieval.Index
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/qa", s.handleQA)
	mux.HandleFunc("/v1/qa/stream", s.handleQAStream)
	mux.HandleFunc("/v1/sessions", s.handleSession)
	mux.HandleFunc("/v1/sessions/", s.handleSession)
	mux.HandleFunc("/v1/docs", s.handleGenerateDoc)
	mux.HandleFunc("/v1/docs/topics", s.handleSuggestTopics)
	mux.HandleFunc("/v1/usage", s.handleUsage)
	mux.HandleFunc("/v1/signal", s.handleSignal)
	mux.HandleFunc("/v1/signals", s.handleSignals)
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/ingest", s.handleIngest)
	mux.HandleFunc("/v1/doc/", s.handleDoc)
	mux.HandleFunc("/", s.handlePage)
	// 鉴权网关：**health 放行**（探活不该要凭证），其余按凭证表判 realm。
	// 没配 Keys 时整体放行（单机开发的老路径一字不变）。
	return s.authGate(mux)
}

// authGate 是 HTTP 面的鉴权中间层。realm 由**凭证推导**并写进 request context；
// handler 用 realmOf(r) 取，**不用**客户端请求体里的任何自称。
func (s *Server) authGate(next http.Handler) http.Handler {
	if s.Keys.Empty() {
		return next // 没配凭证表 = 宽容（单机/本地开发）
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r) // 探活不鉴权：否则负载均衡会把实例判死
			return
		}
		realm, err := s.Keys.Authenticate(r)
		if err != nil {
			auth.WriteAuthError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithRealm(r.Context(), realm)))
	})
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
	c := context.New(context.Realm(s.realmOf(r)))
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
	// 配额闸门（共享实例的必需品）：超限 → 429，不进流程（不耗 LLM、不写 session）
	if !s.allowQuota(w, s.realmOf(r), usage.KindQuestion) {
		return
	}
	idx, ierr := s.IndexFor(r.Context(), s.realmOf(r))
	if ierr != nil || idx == nil {
		writeErr(w, http.StatusInternalServerError, "index: "+realmErr(ierr))
		return
	}
	runner := qaflow.Runner(req.Question, qaflow.BM25Evidence(idx, s.TopK, s.Width), s.Synth, opts)
	if err := runner.Run(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "flow: "+err.Error())
		return
	}
	resp := s.record(c, req.Question)
	// token 记账：**只有 provider 给了 usage 才记**（CostKnown=false 不记）——
	// 宁可读数说"不知道"，也不要填 0 冒充没用钱（usage 包的纪律 2）。
	if s.Meter != nil && resp.Usage.CostKnown {
		s.Meter.RecordTokens(s.realmOf(r), int64(resp.Usage.PromptTokens), int64(resp.Usage.CompletionTokens), true)
	}
	// 使用信号（"长"的地基）：同一 session 同一问句再来一次 = 再问。按
	// 上轮答没答分两族（弃权没解决 / 答案没答全）——服务端推导，前端零
	// 改动。无 session 的一次性问答不记（没有"再问"的上下文）。
	if s.Signals != nil && req.Session != "" {
		s.Signals.Remember(req.Session, req.Question, resp.Refused)
	}
	// **token×document 账本**（移植自 cumulus 原版 internal/affinity）。
	//
	// 记的是"这次问了这些词，实际上由这些文档当证据"。读的时候按**当前查询的词**
	// 查账本——CJK 二元组天然把"宠物扰邻"与"宠物伤人"连起来，不靠主题聚类对不对。
	//
	// **默认只记不重排**（affinity.RerankEnabled 默认 false）：原版把后验做成全局文档
	// 声望实测 −11pp，宁可不重排也不做错；账本的收益必须先用真实问答量出来。
	// 账本写失败**不许影响这一问的答案**（同 usage/signals 的纪律）。
	if s.Store != nil && !resp.Refused {
		s.recordAffinity(r.Context(), s.realmOf(r), req.Question, resp)
	}
	writeJSON(w, http.StatusOK, resp)
}

// recordAffinity 把"问的词 × 当证据的文档"记进账本。
//
// 权重用**答案置信度**（affinity.OutcomeWeight）：答得确定的教得更多，答得勉强的也留
// 一点痕迹（否则预算耗尽的 DEEP 等于什么都没学到）。
func (s *Server) recordAffinity(ctx gocontext.Context, realm, question string, resp QAResponse) {
	toks := affinity.TrimTokens(retrieval.Fields(question), affinity.DefaultMaxTokensPerQ)
	if len(toks) == 0 {
		return
	}
	// Citations 是 "docID#rune[a:b]" 形态 → 取 docID（账本记的是文档，不是窗口）。
	docs := make([]string, 0, len(resp.Citations))
	for _, ct := range resp.Citations {
		if id := strings.TrimSpace(strings.SplitN(ct, "#", 2)[0]); id != "" {
			docs = append(docs, id)
		}
	}
	if len(docs) == 0 {
		return
	}
	// 权重：**这一问**的可观测强度。API 面没有判定置信度读数（决策头默认关），
	// 所以按"有没有引用"分两档：有引用 = 明确答了（1.0），无引用 = 答了但没落到
	// 证据上（0.4，仍记一条痕迹——预算耗尽的深循环等于是什么都没学到）。
	//
	// **不编一个假的置信度**：账本的长期质量取决于记进去的权重诚实，猜一个数比缺一个
	// 数更坏（它会被当成读数用）。
	w := affinity.OutcomeWeight(0.4)
	if len(docs) > 0 {
		w = affinity.OutcomeWeight(1.0)
	}
	coll := affinity.CollectionFor(realm)
	// 集合**必须先声明**（存储契约：写不存在的集合会失败）。原来这里把 Record 的
	// 错误直接吞掉，于是"账本一条都没写进去"表现为**读数为空**——一个看起来像
	// "还没学到东西"的正常状态，而不是一个能看见的故障。这正是 usage/signals
	// 纪律的反面。
	if err := s.Store.EnsureCollection(ctx, coll); err != nil {
		log.Printf("affinity: ensure collection %s: %v", coll, err)
		return
	}
	if err := affinity.NewLedger(s.Store, coll).Record(ctx, toks, docs, w, time.Now()); err != nil {
		log.Printf("affinity: record %s: %v", coll, err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// **realm 由凭证推导**（与问答面同一口径），篇数也是**该 realm 的**篇数。
	//
	// 验收脚本第一次跑就抓到这里：health 原先读启动参数里的 -realm，而带凭证的
	// 请求落在凭证推出的 realm 上 —— 于是"有 3 篇语料、问答答得上来"的服务，
	// health 却报 realm=""、corpus_docs=0。**说谎的健康报告比没有更坏**：运维
	// 会照着一个空数字去排查。
	realm := s.realmOf(r)
	docs := 0
	if idx, err := s.IndexFor(r.Context(), realm); err == nil && idx != nil {
		docs = idx.N
	}
	writeJSON(w, http.StatusOK, HealthResponse{
		Status: "ok", CorpusDocs: docs, Realm: realm, CorpusVersion: s.corpusVersion(),
	})
}

// handleStatus 是分类器可见面：哪些可选组件活着、缺什么。cumulus 的
// MCS 静默不触发，缺的就是这一面。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sr := StatusResponse{
		Synthesis: s.Synth != nil,
		Embedder:  s.Embedder != nil,
		Reuse:     s.Reuse != nil,
		Escalate:  s.Escalate != nil,
	}
	// 分层读数：**内存里放了多少、升权降权各多少次**——不看这个就不知道"库里有多少
	// 在内存里"，也无法判断要不要调预算。
	if idx, err := s.IndexFor(r.Context(), s.realmOf(r)); err == nil && idx != nil {
		st := idx.TierStats()
		sr.Tier = &st
	}
	writeJSON(w, http.StatusOK, sr)
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
	id, err := ingest.Text(ctx, s.Store, s.realmOf(r), body, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ingest: "+err.Error())
		return
	}
	n, err := s.Rebuild(ctx)
	s.InvalidateRealm(s.realmOf(r)) // 多租户：作废该 realm 的缓存索引（写后作废）
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
	if err := s.Store.GetStruct(gocontext.Background(), corpus.CollectionFor(s.realmOf(r)), id, &d); err != nil {
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

// realmErr 把索引错误压成一句（不泄露路径与内部结构）。
func realmErr(err error) string {
	if err == nil {
		return "empty realm"
	}
	return "realm index unavailable"
}

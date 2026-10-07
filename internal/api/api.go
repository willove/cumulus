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
	"encoding/json"
	"net/http"
	"strings"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

// Server 是一次装配的服务。零件（索引/合成/向量/复用/升级）装配时定死，
// 运行中不热换——热插拔是注册面的事，HTTP 面只把装配结果暴露出去。
type Server struct {
	Index    *retrieval.Index
	Synth    qaflow.SynthFunc
	Embedder embed.Embedder // 可空：nil = 语义重排/语义尺缺席
	Reuse    *knowledge.ReuseStore
	Escalate func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error)
	TopK     int
	Width    int
	Realm    string
	Options  qaflow.Options
}

// New 装配一台服务。topk/width 是检索旋钮（语料不同要重测，见
// evolution-log 的宽度实验）。
func New(idx *retrieval.Index, synth qaflow.SynthFunc, topk, width int) *Server {
	if topk <= 0 {
		topk = 3
	}
	if width <= 0 {
		width = 160
	}
	return &Server{Index: idx, Synth: synth, TopK: topk, Width: width, Realm: "default", Reuse: knowledge.NewReuseStore()}
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
	Route     RouteView               `json:"route"`
	Escalate  qaflow.EscalationRecord `json:"escalation"`
	Reuse     qaflow.ReuseState       `json:"reuse"`
	Coverage  CoverageView            `json:"coverage"`
	Eviction  EvictionView            `json:"eviction"`
	Rerank    qaflow.RerankState      `json:"rerank"`
	Windows   []WindowView            `json:"windows"`
	Usage     UsageView               `json:"usage"`
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
type WindowView struct {
	SourceID string  `json:"source_id"`
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
	runner := qaflow.Runner(req.Question, qaflow.BM25Evidence(s.Index, s.TopK, s.Width), s.Synth, opts)
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
			resp.Windows = append(resp.Windows, WindowView{SourceID: w.SourceID, Span: w.Span, Text: w.Text, Score: w.Score})
		}
	}
	if u, ok := context.Get(c, qaflow.KeyUsage); ok {
		resp.Usage = UsageView{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, CostKnown: u.CostKnown}
	}
	return resp
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	docs := 0
	if s.Index != nil {
		docs = s.Index.N
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

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

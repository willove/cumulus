package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

// versions.go —— 提交视图的四版本、按 realm 的索引、以及**一次问答的完整采样**。
//
// （拆文件的理由：api.go 原本同时装"服务是什么/怎么装配""请求怎么路由""响应怎么
// 采样"三件事。改采样逻辑（新增可观测项）会与路由改动撞在一起，review 时也难说清
// 改的是哪一面。）

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
	if len(s.indexes) > 0 {
		// 启用了多租户：单数索引不再可信（它只装某个 realm 的语料）
		return s.indexes[s.Realm]
	}
	return s.index
}

// IndexFor 取某 realm 的索引（按需重建）。
func (s *Server) IndexFor(ctx gocontext.Context, realm string) (*retrieval.Index, error) {
	// 没挂 store 的服务（`New(idx, …)` 直接喂索引的那种构造）**没有集合可查**：
	// 回落到单数索引，而不是崩在这里——缺席是合法状态。
	if s.Store == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.index, nil
	}
	s.mu.RLock()
	if idx, ok := s.indexes[realm]; ok && idx != nil {
		s.mu.RUnlock()
		return idx, nil
	}
	s.mu.RUnlock()
	docs, err := corpus.LoadRealm(ctx, s.Store, realm)
	if err != nil {
		return nil, err
	}
	// 分层索引：**倒排只放热区**，其余文档留词项指纹 + 按需取回正文。
	//
	// 为什么（实测）：1800 篇 / 1.49 MB 语料 → 全内存索引 26.5 MB，其中倒排占
	// 99.4%。每 MB 语料 ≈ 17 MB 内存且无上限，所以不能全放内存。
	//
	// 预算是 CUMULUS_HOT_DOCS（默认 2000 篇 ≈ 70 MB）。**预算 ≥ 语料规模时行为与
	// 旧的��内存索引逐字节一致**——所以个人小库完全不受影响。
	idx := s.buildIndex(ctx, realm, docs)
	s.mu.Lock()
	if s.indexes == nil {
		s.indexes = map[string]*retrieval.Index{}
	}
	s.indexes[realm] = idx
	s.mu.Unlock()
	return idx, nil
}

// Rebuild 从 store 重建**默认 realm** 的索引。摄入（粘贴/链接/看目录）后调用。
func (s *Server) Rebuild(ctx gocontext.Context) (int, error) {
	// **按 realm 重建**（realm 空 → 服务自身的 Realm，通常是 "default"）。
	// 原先固定读默认集合 `documents`，而摄入写的是 `documents/<realm>`
	// —— 口径不一致时，摄入后重建出来的索引是空的（"刚摄入的文档一问答就查不到"，
	// 同样的坑在 -watch 那次踩过一次）。测试替身原先忽略集合，把这个问题盖住了。
	// realm 为空 → 用服务自身的 Realm。注意 Rebuild 重建的是**单数索引**
	// （`s.index`）：带凭证的请求走的是 IndexFor 的 **per-realm 缓存**，所以
	// 单租户形态下两者一致；多 realm 时 watch/ingest 必须显式用 InvalidateRealm
	// 让对应 realm 失效重建（真跑踩过：watch 写 alpha、rebuild 刷 default，
	// health 读 alpha 的缓存 → 0 篇，"明明导入了 6 篇却一篇查不到"）。
	realm := s.Realm
	docs, err := corpus.LoadRealm(ctx, s.Store, realm)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index = s.buildIndex(ctx, s.Realm, docs)
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
	// 窗口分级（可选）：类别计数 + 分级是否发生过 + 结局。
	// **"未分级"与"分级失败"必须分得开**（前者是没开分类器，后者是开了但没成）。
	resp.Classification = &ClassificationView{Enabled: s.Options.WindowClassifier != nil}
	if res, ok := qaflow.WindowClassOf(c); ok {
		// 三态同址（ran/classes/outcome）："没开分类器""开了但没跑""跑了但失败"
		// 在读数里必须分得开——这是本轮踩了几次的坑。
		resp.Classification.Outcome = res.Outcome
		if res.Ran && len(res.Classes) > 0 {
			resp.Classification.Applied = true
			counts := map[string]int{}
			for _, cl := range res.Classes {
				counts[cl]++
			}
			resp.Classification.Counts = counts
		}
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

// buildIndex 建（可能分层的）索引。hotBudget<=0 或读文档失败时**退���全内存**。
//
// 退���必须诚实：拿不到 loader（比如 store 不支持读回某篇）就老老实实全内存建索引，
// 宁可多占内存，也不能让语料"看起来是有了但检索不到"。
func (s *Server) buildIndex(ctx gocontext.Context, realm string, docs []retrieval.Document) *retrieval.Index {
	budget := s.HotDocs
	if budget <= 0 {
		return retrieval.Build(docs)
	}
	coll := corpus.CollectionFor(realm)
	loader := func(docID string) (string, bool) {
		if s.Store == nil {
			return "", false
		}
		var d corpus.Doc
		if err := s.Store.GetStruct(ctx, coll, docID, &d); err != nil {
			return "", false
		}
		return d.Body, true
	}
	return retrieval.BuildTiered(docs, loader, retrieval.TierPolicy{HotDocs: budget})
}

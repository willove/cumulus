package main

// Search HTTP face (P1): POST /v1/search (JSON, same shape as `ask search
// -raw`) and POST /v1/search/stream (SSE). The SSE event schema maps onto the
// evoke-chat engine callbacks: status → loading, content → appendContent,
// citations → ChatSources, done → completeMessage (P7 对齐).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cumubase/ask/internal/abstain"
	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/ns"
	"github.com/cumubase/ask/internal/source"
	"github.com/willove/cumudb/pkg/client"
	"github.com/willove/cumulite"
)

// firstNonEmpty picks the per-request override, falling back to the serve-level
// namespace (empty both = default library).
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// storeForNS scopes the whole write/read side (L0 sources, L1 evidence, L2
// clusters, KV job cursors) to one namespace: the serve-level store when the
// request carries no override, a freshly composed store otherwise. Without
// this, a per-request "ns" would read evidence from the default library while
// writing clusters into the tenant's — corpus and cluster domains diverging.
// serveSources is the serve-level sources identity the caller already resolved
// (an explicit -sources full identity wins over -ns there) and is returned
// unchanged when the request stays in the serve's namespace.
func storeForNS(c cumulite.Port, st *ingest.Store, serveNS, reqNS, serveSources string) (*ingest.Store, string, error) {
	if err := ns.Validate(reqNS); err != nil {
		return nil, "", err
	}
	nsForReq := firstNonEmpty(reqNS, serveNS)
	if nsForReq == serveNS {
		return st, serveSources, nil
	}
	scoped := ingest.New(c,
		ns.Coll(nsForReq, "ask_sources"), ns.Coll(nsForReq, "ask_evidence"),
		ns.Coll(nsForReq, "ask_clusters"), nsForReq)
	return scoped, ns.Coll(nsForReq, "ask_sources"), nil
}

// SearchOptions carries the CLI flags and HTTP body knobs on one shape.
type SearchOptions struct {
	Prior   bool          `json:"prior"`
	L1Pre   bool          `json:"l1pre"`
	History []string      `json:"history"`
	HopTS   time.Duration `json:"hopts,omitempty"`
	MinHot  float64       `json:"minhot,omitempty"`
	MinConf float64       `json:"minconf,omitempty"`
	// Namespace scopes the L2 collections (P3): clusters, weak edges, cites and
	// conflicts become "<ns>:ask_*" composite identities, so a reuse hit can
	// never come from another tenant's namespace.
	Namespace string `json:"namespace,omitempty"`
}

// searchStack is one fully-wired search pipeline (FAST/KB/DEEP + collaborators),
// shared by the CLI search face and the serve HTTP face.
type searchStack struct {
	fe          *fast.Engine
	kbE         *kb.Engine
	dE          *deep.Engine
	chat        *llm.ChatClient
	st          *ingest.Store
	c           cumulite.Port
	sourcesColl string
	opt         SearchOptions
}

// newSearchStack wires the production stack (aigate when configured, offline
// stubs otherwise) with the ranked admission and widening callbacks.
func newSearchStack(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl string, opt SearchOptions) (*searchStack, error) {
	stack := newProdStack()
	fe := fast.New(stack.scorer)
	fe.UsePrior = opt.Prior
	fe.Analyzer, fe.Synth, fe.Expander = stack.analyzer, stack.synth, stack.expander
	// 1.6: the prior's history arm reads live ask_evidence (needs the active
	// list first — one read per stack build, not per query).
	if opt.Prior {
		if list, lerr := st.ActiveSources(ctx); lerr == nil {
			fe.PriorHist = priorHistFromStore(ctx, st, list)
		}
	}
	kbE := kb.New(fe, cluster.NewCumuStore(c, ns.Coll(opt.Namespace, "ask_clusters")), stack.emb)
	// G2: warm-prior validation reads only the cluster's anchored docs.
	kbE.SourceReader = st
	kbE.Edges = graph.NewCumuStore(c, ns.Coll(opt.Namespace, "ask_weak_edges"))
	kbE.Cites = deep.NewCumuCiteStore(c, ns.Coll(opt.Namespace, "ask_cites"))
	kbE.HopTS = opt.HopTS
	kbE.MinHotness = opt.MinHot
	kbE.MinConfidence = opt.MinConf
	dE := deep.New(kbE, deep.NewCumuStore(c, ns.Coll(opt.Namespace, "ask_conflicts")))
	dE.Scorer = stack.scorer
	dE.Synth = stack.synth
	dE.Widen = widenFunc(fe, st, c, sourcesColl, refinerFor(stack.chat))
	dE.RankAdmission = rankFunc(fe, st, c, sourcesColl)
	// Independent search token budget (3.2): judge never draws from this.
	if stack.chat != nil {
		dE.TokensUsed = stack.chat.TotalTokens
		if v := os.Getenv("ASK_SEARCH_TOKEN_BUDGET"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				dE.TokenBudget = n
			}
		}
	}
	// Opt-in zero-LLM abstention head (3.1): default off so gates stay put.
	if os.Getenv("ASK_ABSTAIN") == "1" {
		dE.Abstain = abstain.Default()
		if os.Getenv("ASK_EARLY_ABSTAIN") != "1" {
			// 早弃权默认关：DEEP 有救回拒答的先例（v1.19 消保法退货题），
			// 运营商显式开才牺牲这段恢复机会换 token。
			dE.Abstain.EarlyAbove = 0
		}
	}
	// Opt-in two-call query simulator (2.4): needs an endpoint.
	if stack.chat != nil && os.Getenv("ASK_QUERY_SIM") == "1" {
		dE.QuerySim = &llm.AigateQuerySimulator{Client: stack.chat}
	}
	if len(opt.History) > 0 {
		dE.History = opt.History
		dE.HistoryRewriter = stack.rewriter
	}
	return &searchStack{fe: fe, kbE: kbE, dE: dE, chat: stack.chat, st: st, c: c, sourcesColl: sourcesColl, opt: opt}, nil
}

func refinerFor(chat *llm.ChatClient) *llm.AigateKeywordRefiner {
	if chat == nil {
		return nil
	}
	return &llm.AigateKeywordRefiner{Client: chat}
}

// loadCandidates materializes the candidate corpus for one query (G2: kept
// behind a loader so a warm reuse never pays the full-corpus read).
func (ss *searchStack) loadCandidates(ctx context.Context, query string) ([]source.Source, error) {
	list, err := ss.st.ActiveSources(ctx)
	if err != nil {
		return nil, err
	}
	if ss.dE.Verbose != nil {
		ss.dE.Verbose("query %q: active sources=%d", query, len(list))
	}
	if ss.opt.L1Pre {
		list = ss.narrowL1Pre(ctx, list, query)
	}
	return list, nil
}

// runSearch executes one query and applies the CLI/HTTP shared side effects
// (evidence marking, B9 token accounting).
func runSearch(ctx context.Context, ss *searchStack, query string) (deep.Result, error) {
	res, err := ss.dE.AskLazy(ctx, query, func(ctx context.Context) ([]source.Source, error) {
		return ss.loadCandidates(ctx, query)
	})
	if err != nil {
		return deep.Result{}, err
	}
	if ss.chat != nil {
		res.Tokens = ss.chat.TotalTokens()
	}
	if ss.dE.Verbose != nil {
		ss.dE.Verbose("done mode=%s conf=%.2f loops=%d widened=%d tokens=%d latency=%dms reused=%v",
			res.Mode, res.Answer.Confidence, res.Loops, res.Widened, res.Tokens, res.LatencyMS, res.Reused)
	}
	ans := res.Answer
	if !res.Reused && len(ans.Samples) > 0 && ans.SourceID != "" && !ans.Skipped {
		top := ans.Samples[0]
		_, _ = ss.st.MarkEvidence(ctx, ans.SourceID, top.Start, top.End, top.Score, top.Reasoning, trim(top.Content, 200))
	}
	return res, nil
}

type searchIn struct {
	Query   string   `json:"query"`
	History []string `json:"history"`
	Session string   `json:"session"` // KV 会话：折叠近几轮进改写，回答回写会话
	Prior   bool     `json:"prior"`
	L1Pre   bool     `json:"l1pre"`
	Stream  bool     `json:"stream"`
	NS      string   `json:"ns"` // per-request namespace override (empty = serve's -ns)
}

// narrowL1Pre narrows candidates via body_embed KNN (D7: a missing index
// materializes once, bounded, then retries; failure → full list, 只慢不错).
func (ss *searchStack) narrowL1Pre(ctx context.Context, list []source.Source, query string) []source.Source {
	embedFn, dims, embedModel := embedderFor()
	knnOnce := func() (*client.KNNResult, error) {
		qv, err := embedFn(ctx, []string{query})
		if err != nil || len(qv) != 1 {
			return nil, fmt.Errorf("embed: %w", err)
		}
		return ss.c.KNN(ctx, ss.sourcesColl, client.KNNRequest{
			Field: "body_embed", Vector: qv[0], K: 8, Metric: "cosine",
			Index:  "ask_body_embed",
			Filter: map[string]any{"status": source.StatusActive},
		})
	}
	knn, err := knnOnce()
	if err != nil {
		if _, berr := ss.st.EnsureEmbed(ctx, embedFn, dims, embedModel, 64); berr == nil {
			knn, err = knnOnce()
		}
	}
	if err != nil || knn == nil || len(knn.Documents) == 0 {
		return list
	}
	if narrowed := orderByKNN(list, knn.Documents); len(narrowed) > 0 {
		return narrowed
	}
	return list
}

// registerSessionFace mounts the session REST endpoints the web UI reads:
// POST /v1/sessions (new), GET /v1/sessions (list), GET/DELETE /v1/sessions/{id}.
// Per-request "ns" scopes the KV keys (P3); empty falls back to serveNS.
func registerSessionFace(mux *http.ServeMux, c cumulite.Port, serveNS string) {
	sessions := func(w http.ResponseWriter, r *http.Request) {
		st := sessionStore{c: c}
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sessions"), "/")
		switch r.Method {
		case http.MethodPost:
			var in struct {
				Title string `json:"title"`
				ID    string `json:"id"`
				NS    string `json:"ns"`
			}
			if err := decode(r, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if err := ns.Validate(in.NS); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			st.ns = firstNonEmpty(in.NS, serveNS)
			if in.ID == "" {
				in.ID = newSessionID()
			}
			d, err := st.ensure(r.Context(), in.ID, in.Title)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if err := st.save(r.Context(), d); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, d)
		case http.MethodGet:
			reqNS := r.URL.Query().Get("ns")
			if err := ns.Validate(reqNS); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			st.ns = firstNonEmpty(reqNS, serveNS)
			if id == "" {
				all, err := st.list(r.Context())
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, all)
				return
			}
			d, err := st.load(r.Context(), id)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
				return
			}
			writeJSON(w, http.StatusOK, d)
		case http.MethodDelete:
			var in struct {
				NS string `json:"ns"`
			}
			_ = decode(r, &in)
			if err := ns.Validate(in.NS); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			st.ns = firstNonEmpty(in.NS, serveNS)
			if id == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session id required"})
				return
			}
			ok, err := st.c.KVDelete(r.Context(), st.key(id))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleted": ok})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET/POST/DELETE only"})
		}
	}
	mux.HandleFunc("/v1/sessions", sessions)
	mux.HandleFunc("/v1/sessions/", sessions)
}

// registerSearchFace mounts POST /v1/search and POST /v1/search/stream.
// Per-request "ns" overrides serveNS for this query only (P3).
func registerSearchFace(mux *http.ServeMux, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, verbose bool) {
	handle := func(stream bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
				return
			}
			var in searchIn
			if err := decode(r, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if in.Query == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "query required"})
				return
			}
			if err := ns.Validate(in.NS); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			// Whole-stack scoping (P3): L0 corpus, L1 evidence, L2 cluster
			// collections and KV keys all move to the request's namespace.
			stForReq, sourcesForReq, serr := storeForNS(c, st, serveNS, in.NS, sourcesColl)
			if serr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": serr.Error()})
				return
			}
			opt := SearchOptions{Prior: in.Prior, L1Pre: in.L1Pre, History: in.History, Namespace: firstNonEmpty(in.NS, serveNS)}
			var sess *sessionStore
			if in.Session != "" {
				sess = &sessionStore{c: c, ns: firstNonEmpty(in.NS, serveNS)}
				hist, _, err := sessionHistory(r.Context(), *sess, in.Session, 6)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				opt.History = hist
			}
			if stream {
				in.Stream = true
			}
			ss, err := newSearchStack(r.Context(), c, stForReq, sourcesForReq, opt)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if verbose || os.Getenv("ASK_VERBOSE") == "1" {
				ss.dE.Verbose = func(f string, a ...any) {
					log.Printf("[search %s] %s", in.Query, fmt.Sprintf(f, a...))
				}
			}
			if !in.Stream {
				res, err := runSearch(r.Context(), ss, in.Query)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				if sess != nil {
					if _, aerr := sess.appendTurn(r.Context(), in.Session, in.Query, in.Query, res.Answer.Summary); aerr == nil {
						res.Session = in.Session
					}
				}
				writeJSON(w, http.StatusOK, res)
				return
			}
			sseSearch(w, r, ss, in.Query, sess, in.Session, verbose || os.Getenv("ASK_VERBOSE") == "1")
		}
	}
	mux.HandleFunc("/v1/search", handle(false))
	mux.HandleFunc("/v1/search/stream", handle(true))
}

// sseSearch streams one search as SSE events mapped onto the evoke-chat
// engine: status → loading/progress, content → appendContent, citations →
// ChatSources, done → completeMessage. A 5s heartbeat keeps proxies and
// browsers from timing out during long DEEP searches.
func sseSearch(w http.ResponseWriter, r *http.Request, ss *searchStack, query string, sess *sessionStore, sessionID string, verbose bool) {
	if verbose {
		ss.dE.Verbose = func(f string, a ...any) {
			log.Printf("[search %s] %s", query, fmt.Sprintf(f, a...))
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	emit := func(event string, data any) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
	emit("status", map[string]any{"stage": "started"})

	// 心跳：长检索期间保活连接；停止与 handler 返回同步，杜绝迟到写。
	heartbeat := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-heartbeat:
				return
			case <-r.Context().Done():
				return
			case <-t.C:
				mu.Lock()
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			}
		}
	}()
	defer func() { close(heartbeat); <-hbDone }()

	ss.dE.OnFile = func(key string, best float64, windows int) {
		emit("status", map[string]any{"stage": "file", "file": key, "score": best})
	}

	res, err := runSearch(r.Context(), ss, query)
	if err != nil {
		emit("error", map[string]any{"error": err.Error()})
		return
	}
	ans := res.Answer
	if ans.Skipped {
		emit("status", map[string]any{"stage": "insufficient-evidence"})
	}
	emit("content", map[string]any{"text": ans.Summary})
	if len(res.Citations.Refs) > 0 {
		emit("citations", res.Citations)
	}
	done := map[string]any{
		"mode": res.Mode, "loops": res.Loops, "conf": ans.Confidence,
		"coverage": ans.Coverage, "reused": res.Reused,
		"cluster_id": res.ClusterID, "tokens": res.Tokens,
		"latency_ms": res.LatencyMS,
	}
	if sess != nil {
		if _, aerr := sess.appendTurn(r.Context(), sessionID, query, query, ans.Summary); aerr == nil {
			done["session"] = sessionID
		}
	}
	emit("done", done)
}

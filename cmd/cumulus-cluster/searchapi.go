package main

// Search HTTP face: POST /v1/search (JSON, same shape as `cumulus-cluster search
// -raw`) and POST /v1/search/stream (SSE). The SSE event schema maps onto the
// evoke-chat engine callbacks: status → loading, content → appendContent,
// citations → ChatSources, done → completeMessage.

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

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/bucket"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/monitor"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
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
		ns.Coll(nsForReq, "clus_sources"), ns.Coll(nsForReq, "clus_evidence"),
		ns.Coll(nsForReq, "clus_clusters"), nsForReq)
	return scoped, ns.Coll(nsForReq, "clus_sources"), nil
}

// SearchOptions carries the CLI flags and HTTP body knobs on one shape.
type SearchOptions struct {
	Prior   bool          `json:"prior"`
	L1Pre   bool          `json:"l1pre"`
	History []string      `json:"history"`
	HopTS   time.Duration `json:"hopts,omitempty"`
	MinHot  float64       `json:"minhot,omitempty"`
	MinConf float64       `json:"minconf,omitempty"`
	// Namespace scopes the L2 collections: clusters, weak edges, cites and
	// conflicts become "<ns>:clus_*" composite identities, so a reuse hit can
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
	// usage holds this request's query-conditioned document weights. The
	// admission ranker closure reads it at call time (after the ledger and
	// session stack have been folded in), so one stack build serves any query.
	usage *usageWeights
}

// usageWeights is the per-request carrier the DEEP admission ranker and the
// FAST cascade both consult.
type usageWeights struct {
	mu sync.Mutex
	m  map[string]float64
}

func (u *usageWeights) set(m map[string]float64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.m = m
}

func (u *usageWeights) get() map[string]float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(u.m))
	for k, v := range u.m {
		out[k] = v
	}
	return out
}

// newSearchStack wires the production stack (aigate when configured, offline
// stubs otherwise) with the ranked admission and widening callbacks.
func newSearchStack(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl string, opt SearchOptions) (*searchStack, error) {
	return newSearchStackWith(ctx, c, st, sourcesColl, opt, newProdStack())
}

// Explicit collaborator injection lets isolated evaluations force offline
// execution without mutating process-wide environment or other requests.
func newSearchStackWith(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl string, opt SearchOptions, stack prodStack) (*searchStack, error) {
	// Strict embedder gate: with -l1pre the corpus-vector seat must be the
	// one the operator asked for — a silent hash fallback (CLUS_MINILM_REQUIRE=1)
	// fails the request instead of quietly degrading the L1 path.
	if opt.L1Pre {
		if _, _, _, eerr := embedderFor(); eerr != nil {
			return nil, eerr
		}
	}
	if stack.embErr != nil {
		return nil, stack.embErr
	}
	fe := fast.New(stack.scorer)
	fe.UsePrior = opt.Prior
	fe.Analyzer, fe.Synth, fe.Expander = degradeAnalyzer{inner: stack.analyzer}, stack.synth, stack.expander
	// 1.6: the prior's history arm reads live clus_evidence (needs the active
	// list first — one read per stack build, not per query).
	if opt.Prior {
		if list, lerr := st.ActiveSources(ctx); lerr == nil {
			fe.PriorHist = priorHistFromStore(ctx, st, list)
		}
	}
	kbE := kb.New(fe, cluster.NewCumuStore(c, ns.Coll(opt.Namespace, "clus_clusters")), stack.emb)
	// The ask-sequence cursor (KV, namespace-scoped): without it the warm
	// reuse path masks the previous cluster and repeated walks never
	// accumulate into pathway edges (P4).
	kbE.Cursor = &kvLastCluster{c: c, key: ns.KV(opt.Namespace, "clus:lastcluster")}
	// warm-prior validation reads only the cluster's anchored docs.
	kbE.SourceReader = st
	kbE.Edges = graph.NewCumuStore(c, ns.Coll(opt.Namespace, "clus_weak_edges"))
	kbE.Cites = deep.NewCumuCiteStore(c, ns.Coll(opt.Namespace, "clus_cites"))
	kbE.HopTS = opt.HopTS
	kbE.MinHotness = opt.MinHot
	kbE.MinConfidence = opt.MinConf
	dE := deep.New(kbE, deep.NewCumuStore(c, ns.Coll(opt.Namespace, "clus_conflicts")))
	// DEEP loop budgets: engine defaults, tightened per deployment. Measured
	// queries spent ~14 rounds (6 admission + 4 widen + 3 correct); the cut
	// targets rounds that re-score files earlier rounds already cleared.
	// Unparseable/non-positive env values fall back to the engine default.
	for _, kv := range []struct {
		env  string
		dest *int
	}{
		{"CLUS_DEEP_LOOPS", &dE.MaxLoops},
		{"CLUS_DEEP_WIDEN", &dE.WidenBudget},
		{"CLUS_DEEP_CORRECT", &dE.CorrectBudget},
	} {
		if v := os.Getenv(kv.env); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*kv.dest = n
			}
		}
	}
	dE.Scorer = stack.scorer
	dE.Synth = stack.synth
	dE.Widen = widenFunc(fe, st, c, sourcesColl, refinerFor(stack.chat))
	ss := &searchStack{fe: fe, kbE: kbE, dE: dE, chat: stack.chat, st: st, c: c, sourcesColl: sourcesColl, opt: opt, usage: &usageWeights{}}
	dE.RankAdmission = rankFunc(fe, st, c, sourcesColl, ss.usage)
	// Independent search token budget (3.2): judge never draws from this.
	if stack.chat != nil {
		// Per-STACK budget: serve builds one stack per request, eval one
		// per run, so the counter must be a delta from stack build. Reading
		// the process-lifetime TotalTokens directly spent the budget
		// cumulatively — on a long-lived serve DEEP was permanently starved
		// once the first few queries crossed the line (the knob had only
		// ever been exercised by eval, whose chat client starts at zero per
		// run). Concurrent requests share the client, so a delta can include
		// a neighbour's spend — that errs toward stopping earlier, the safe
		// direction for a burn cap.
		base := stack.chat.TotalTokens()
		dE.TokensUsed = func() int64 { return stack.chat.TotalTokens() - base }
		if v := os.Getenv("CLUS_SEARCH_TOKEN_BUDGET"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				dE.TokenBudget = n
			}
		}
	}
	// Opt-in zero-LLM abstention head (3.1): default off so gates stay put.
	if os.Getenv("CLUS_ABSTAIN") == "1" {
		dE.Abstain = abstain.Default()
		if os.Getenv("CLUS_EARLY_ABSTAIN") != "1" {
			// 早弃权默认关：DEEP 有救回拒答的真实先例，
			// 运营商显式开才牺牲这段恢复机会换 token。
			dE.Abstain.EarlyAbove = 0
		}
	}
	// Opt-in two-call query simulator (2.4): needs an endpoint.
	if stack.chat != nil && os.Getenv("CLUS_QUERY_SIM") == "1" {
		dE.QuerySim = &llm.AigateQuerySimulator{Client: stack.chat}
	}
	if len(opt.History) > 0 {
		dE.History = opt.History
		dE.HistoryRewriter = stack.rewriter
	}
	return ss, nil
}

func refinerFor(chat *llm.ChatClient) *llm.AigateKeywordRefiner {
	if chat == nil {
		return nil
	}
	return &llm.AigateKeywordRefiner{Client: chat}
}

// kvLastCluster is kb.LastClusterCursor over cumulite KV: the ask sequence's
// previous cluster, scoped per namespace. A failed read is an empty prev
// (one missing walk link), a failed write is ignored — the cursor is an
// accelerator, never a correctness source.
type kvLastCluster struct {
	c   cumulite.Port
	key string
}

func (k *kvLastCluster) LoadLastCluster(ctx context.Context) (string, error) {
	raw, err := k.c.KVGet(ctx, k.key)
	if err != nil || len(raw) == 0 {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), nil
}

func (k *kvLastCluster) SaveLastCluster(ctx context.Context, id string) error {
	return k.c.KVPut(ctx, k.key, []byte(id), 0)
}

// loadCandidates materializes the candidate corpus for one query (kept
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
// (evidence marking, token accounting).
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
	NS      string   `json:"ns"` // explicitly selected, registered bucket
}

// narrowL1Pre narrows candidates via body_embed KNN (D7: a missing index
// materializes once, bounded, then retries; failure → full list, 只慢不错).
func (ss *searchStack) narrowL1Pre(ctx context.Context, list []source.Source, query string) []source.Source {
	embedFn, dims, embedModel, _ := embedderFor() // strict gate already ran at stack build
	knnOnce := func() (*contract.KNNResult, error) {
		qv, err := embedFn(ctx, []string{query})
		if err != nil || len(qv) != 1 {
			return nil, fmt.Errorf("embed: %w", err)
		}
		return ss.c.KNN(ctx, ss.sourcesColl, contract.KNNRequest{
			Field: "body_embed", Vector: qv[0], K: 8, Metric: "cosine",
			Index:  "clus_body_embed",
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
// Per-request "ns" scopes the KV keys; empty falls back to serveNS.
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
			d, err := st.create(r.Context(), in.ID, in.Title)
			if err != nil {
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
// Per-request "ns" IS the bucket and is REQUIRED: a retrieval that names no
// bucket is refused rather than defaulted, because the default library is where
// unrelated corpora accumulate. ensure declares the target namespace's
// collections: the search path persists clusters, so a namespace that was only
// ever searched in must not die on the first write.
func registerSearchFace(mux *http.ServeMux, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, verbose bool, ensure *nsEnsurer, buckets *bucket.Store, tracker *monitor.Tracker) {
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
			// No implicit fallback: ingest and retrieval must select the same
			// explicitly registered bucket.
			if !requireHTTPBucket(w, r, buckets, in.NS) {
				return
			}
			// Whole-stack scoping: L0 corpus, L1 evidence, L2 cluster
			// collections and KV keys all move to the request's namespace.
			// Declaring it here (not only on the ingest faces) keeps the
			// cluster persist from failing on a search-only namespace.
			if err := ensure.declare(r.Context(), firstNonEmpty(in.NS, serveNS)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
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
			// Per-stage latency telemetry: one recorder per request, fed by
			// the FAST/DEEP stage hooks. Pure observability — nil-side
			// behaviour (gates) is unchanged. When the SSE face is active the
			// same hook also pushes a live "stage" status event, so the
			// browser can render a progress timeline as the stages complete
			// instead of one static line of text.
			var stageMu sync.Mutex
			stages := map[string]int64{}
			// streamState carries the SSE face's live sinks (content deltas,
			// stage events) to the engine hooks; both faces share the delta
			// counter so "already on screen" is detectable on either.
			var streamMu sync.Mutex
			streamed := 0
			streamSt := &streamState{mu: &streamMu, streamed: &streamed}
			recStage := func(name string, d time.Duration) {
				stageMu.Lock()
				stages[name] = d.Microseconds()
				stageMu.Unlock()
				if streamSt.onStage != nil {
					streamSt.onStage(name, d)
				}
			}
			emitDelta := func(chunk string) {
				streamMu.Lock()
				streamed++
				n := streamed
				streamMu.Unlock()
				if streamSt.emit != nil && n > 0 {
					streamSt.emit("content", map[string]any{"text": chunk})
				}
			}
			ss, err := newSearchStack(r.Context(), c, stForReq, sourcesForReq, opt)
			if err != nil {
				// Stack failures (e.g. required weights missing) are failed
				// queries too. No embedder actually served this request.
				trackQuery(tracker, in.NS, deep.Result{}, "", err.Error(), nil)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if verbose || os.Getenv("CLUS_VERBOSE") == "1" {
				vlog := func(f string, a ...any) {
					log.Printf("[search %s] %s", in.Query, fmt.Sprintf(f, a...))
				}
				ss.dE.Verbose = vlog
				ss.fe.Verbose = vlog
			}
			ss.fe.Stages = recStage
			ss.dE.Stages = recStage
			// 使用先验：本问词元命中的账本权重 ∪ 本会话证据栈，装进 prior
			// 的 history 臂。失败/关闭都是静默降级，不影响检索本身。
			applyUsagePrior(r.Context(), c, in.NS, in.Query, in.Session, ss.fe, ss.dE, ss.usage)
			// 会话采样上下文（词汇鸿沟回退）：本会话近几问原文。仅当主查询
			// 在选定文档里采不到任何过线窗口时才生效——常见路径零影响。
			if ctxText := sampleContextText(opt.History); ctxText != "" {
				ss.fe.SampleContext = ctxText
				ss.dE.SampleContext = ctxText
			}
			// Streaming synthesis: the SSE face consumes deltas live; the
			// JSON face counts them (no transport) so it can fall back to
			// the whole-summary response.
			ss.fe.SynthDelta = emitDelta
			ss.dE.SynthDelta = emitDelta
			if !stream {
				res, err := runSearch(r.Context(), ss, in.Query)
				if err != nil {
					// The failure is itself a data point: the tracker's contract
					// ("a failed query must be visible as an error, not silently
					// absent") was broken exactly here — every 500 left no trace
					// and the monitor's error count could never leave zero.
					trackQuery(tracker, in.NS, deep.Result{}, embedderLabel(ss), err.Error(), stages)
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				if sess != nil {
					extra := turnExtrasFrom(res.Citations.Refs, statsFromDone(res, stages))
					if _, aerr := sess.appendTurnDurable(r.Context(), in.Session, in.Query, in.Query, res.Answer.Summary, extra); aerr == nil {
						res.Session = in.Session
					} else {
						// A failed session write must be visible, not silent: the
						// query itself succeeded and the caller has no other signal.
						log.Printf("[search] session %s: turn not persisted: %v", in.Session, aerr)
					}
				}
				recordUsage(r.Context(), c, in.NS, in.Query, in.Session, res.Citations.Refs, res.Answer.SourceID, res.Answer.Confidence)
				// Before writing the response: the early return below must not
				// skip the registry bookkeeping.
				bumpBucket(r.Context(), buckets, in.NS, ss)
				trackQuery(tracker, in.NS, res, embedderLabel(ss), "", stages)
				writeJSON(w, http.StatusOK, res)
				return
			}
			bumpBucket(r.Context(), buckets, in.NS, ss)
			sres, serr := sseSearch(w, r, ss, in.Query, sess, in.Session, verbose || os.Getenv("CLUS_VERBOSE") == "1", stages, streamSt)
			if serr == nil {
				recordUsage(r.Context(), c, in.NS, in.Query, in.Session, sres.Citations.Refs, sres.Answer.SourceID, sres.Answer.Confidence)
			}
			errMsg := ""
			if serr != nil {
				errMsg = serr.Error()
			}
			trackQuery(tracker, in.NS, sres, embedderLabel(ss), errMsg, stages)
		}
	}
	mux.HandleFunc("/v1/search", handle(false))
	mux.HandleFunc("/v1/search/stream", handle(true))
}

// turnExtrasFrom builds the persisted turn extras: the answer's evidence
// refs and the run card (the SSE done payload doubles as the card's source,
// so a refreshed session restores exactly what the user saw).
func turnExtrasFrom(refs []deep.Ref, stats *sessionStats) *turnExtras {
	if len(refs) == 0 && stats == nil {
		return nil
	}
	e := &turnExtras{Stats: stats}
	for _, r := range refs {
		resolved := r.Resolved
		e.Sources = append(e.Sources, sessionCite{
			Index: r.Index, Title: r.Title, SourceID: r.SourceID,
			Quote: r.Quote, Resolved: &resolved,
		})
	}
	return e
}

// statsFromDone is the bridge from a finished run (deep.Result + the stage
// map) to the persisted run card.
func statsFromDone(res deep.Result, stages map[string]int64) *sessionStats {
	return statsFrom(map[string]any{
		"mode": res.Mode, "conf": res.Answer.Confidence, "coverage": res.Answer.Coverage,
		"loops": res.Loops, "widened": res.Widened, "tokens": res.Tokens,
		"latency_ms": res.LatencyMS, "reused": res.Reused,
		"cluster_id": res.ClusterID, "stop_reason": res.StopReason,
		"refused": res.Answer.Refused, "stages": stages,
	})
}

// bumpBucket records a query against the bucket that served it. Best effort:
// the registry is telemetry, never a correctness source, so a failure here must
// not fail a search that already succeeded.
func bumpBucket(ctx context.Context, buckets *bucket.Store, name string, ss *searchStack) {
	srcCount, clusterCount := -1, -1
	if ss != nil && ss.st != nil {
		if list, err := ss.st.ActiveSources(ctx); err == nil {
			srcCount = len(list)
		}
		if all, err := cluster.NewCumuStore(ss.c, ns.Coll(name, "clus_clusters")).All(ctx); err == nil {
			clusterCount = len(all)
		}
	}
	_ = buckets.Touch(ctx, name, srcCount, clusterCount)
}

// trackQuery folds one finished retrieval into the monitor. Best effort: the
// tracker is telemetry, never a correctness source.
func trackQuery(tr *monitor.Tracker, ns string, res deep.Result, embedder, errMsg string, stages map[string]int64) {
	if tr == nil {
		return
	}
	a := res.Answer
	tr.Record(monitor.Query{
		Namespace: ns, Mode: res.Mode, Escalated: res.Escalated, Reused: res.Reused,
		Confidence: a.Confidence, Coverage: a.Coverage, Samples: len(a.Samples),
		Loops: res.Loops, Widened: res.Widened, LLMCalls: a.LLMCalls,
		Tokens: res.Tokens, LatencyMS: res.LatencyMS, LatencyUS: res.LatencyUS, Embedder: embedder,
		SelfCorr: res.SelfCorrected, Refused: a.Refused, Error: errMsg,
		StopReason: res.StopReason, Stages: stages,
	})
}

// sseSearch streams one search as SSE events mapped onto the evoke-chat
// engine: status → loading/progress, content → appendContent, citations →
// ChatSources, done → completeMessage. A 5s heartbeat keeps proxies and
// browsers from timing out during long DEEP searches. The error return lets
// the caller record the failed attempt in the monitor — returning a zero
// Result alone made every stream failure look like a successful zero-value
// query and poisoned the averages.
// streamState is the bridge between the SSE face's live sinks and the
// engine hooks: sseSearch installs emit/onStage when the stream opens and
// clears them when it closes.
type streamState struct {
	mu       *sync.Mutex
	streamed *int
	emit     func(event string, data any)
	onStage  func(name string, d time.Duration)
}

func sseSearch(w http.ResponseWriter, r *http.Request, ss *searchStack, query string, sess *sessionStore, sessionID string, verbose bool, stages map[string]int64, st *streamState) (deep.Result, error) {
	if verbose {
		vlog := func(f string, a ...any) {
			log.Printf("[search %s] %s", query, fmt.Sprintf(f, a...))
		}
		ss.dE.Verbose = vlog
		ss.fe.Verbose = vlog
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return deep.Result{}, fmt.Errorf("streaming unsupported")
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
	started := time.Now()
	if st != nil {
		st.mu.Lock()
		st.emit = emit
		st.onStage = func(name string, d time.Duration) {
			emit("status", map[string]any{
				"stage": "stage", "name": name,
				"elapsed_ms": time.Since(started).Milliseconds(),
				"stage_ms":   d.Milliseconds(),
			})
		}
		st.mu.Unlock()
		defer func() {
			st.mu.Lock()
			st.emit = nil
			st.onStage = nil
			st.mu.Unlock()
		}()
	}
	emit("status", map[string]any{"stage": "started"})

	// 心跳：长检索期间保活连接；停止与 handler 返回同步，杜绝迟到写。
	// 它同时是「还活着」的唯一证据——一次 DEEP 检索要串行调用十几次模型（实测
	// 冷启 30s 量级），只发 `: ping` 注释时前端无从区分「在干活」与「卡死了」，
	// 用户看到的就是长时间没响应。所以心跳升级为带 elapsed_ms 的 status 事件。
	heartbeat := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(3 * time.Second)
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
				emit("status", map[string]any{"stage": "working", "elapsed_ms": time.Since(started).Milliseconds()})
			}
		}
	}()
	defer func() { close(heartbeat); <-hbDone }()

	ss.dE.OnFile = func(key string, best float64, windows int) {
		emit("status", map[string]any{"stage": "file", "file": key, "score": best, "elapsed_ms": time.Since(started).Milliseconds()})
	}

	res, err := runSearch(r.Context(), ss, query)
	if err != nil {
		emit("error", map[string]any{"error": err.Error()})
		return deep.Result{}, err
	}
	ans := res.Answer
	if ans.Skipped {
		emit("status", map[string]any{"stage": "insufficient-evidence"})
	} else if ans.Refused {
		// 与「没检索到」不同：检索到了最接近的条文，但语料里没有能回答的依据。
		// 不单独说出来的话，界面会把一段无关引文当成答案展示。
		emit("status", map[string]any{"stage": "refused"})
	}
	// Synthesis deltas already streamed live when the engine hook was wired
	// and the stream survived: the screen has the answer, so this event
	// REPLACES (a re-append would duplicate it). Otherwise the whole summary
	// goes out in one event, as before.
	streamedN := 0
	if st != nil {
		st.mu.Lock()
		streamedN = *st.streamed
		st.mu.Unlock()
	}
	emit("content", map[string]any{"text": ans.Summary, "replace": streamedN > 0})
	if len(res.Citations.Refs) > 0 {
		emit("citations", res.Citations)
	}
	done := map[string]any{
		"stages": stages,
		"mode":   res.Mode, "loops": res.Loops, "conf": ans.Confidence,
		"coverage": ans.Coverage, "reused": res.Reused,
		"cluster_id": res.ClusterID, "tokens": res.Tokens,
		"latency_ms": res.LatencyMS, "widened": res.Widened,
		"stop_reason": res.StopReason, "refused": ans.Refused, "skipped": ans.Skipped,
	}
	if sess != nil {
		extra := turnExtrasFrom(res.Citations.Refs, statsFromDone(res, stages))
		if _, aerr := sess.appendTurnDurable(r.Context(), sessionID, query, query, ans.Summary, extra); aerr == nil {
			done["session"] = sessionID
		} else {
			// The stream already delivered the answer, so the only honest place
			// left to report a lost turn is the terminal event.
			done["session_error"] = evalSafeError(aerr)
			log.Printf("[search] session %s: turn not persisted: %v", sessionID, aerr)
		}
	}
	emit("done", done)
	return res, nil
}

// embedderLabel names the embedder wired into the stack that served the query,
// matching the names the boot log prints. It used to return "" unconditionally,
// so the monitor's embedder headline and every per-query cell stayed blank no
// matter what actually served. Advisory: "" means "nothing wired".
func embedderLabel(ss *searchStack) string {
	if ss == nil || ss.kbE == nil {
		return ""
	}
	return embedderName(ss.kbE.Embedder)
}

func embedderName(emb cluster.Embedder) string {
	if emb == nil {
		return ""
	}
	switch emb := emb.(type) {
	case *minilm.Embedder:
		return "minilm-l12-384"
	case cluster.Local:
		return fmt.Sprintf("local-hash-%d", emb.N)
	case *llm.AigateEmbedder:
		return fmt.Sprintf("aigate-%d", emb.Dims())
	default:
		return fmt.Sprintf("embed-%d", emb.Dims())
	}
}

// degradeAnalyzer keeps a malformed-LLM-reply analyze failure from failing
// the whole query: one bad JSON from the model becomes the deterministic
// rule analyzer (degraded retrieval) instead of a 500. The failure itself is
// logged so the rate stays visible.
type degradeAnalyzer struct{ inner fast.Analyzer }

func (a degradeAnalyzer) Analyze(ctx context.Context, query string) (fast.Analysis, error) {
	if a.inner == nil { // offline gate stacks carry no analyzer at all
		return fast.RuleAnalyzer{}.Analyze(ctx, query)
	}
	res, err := a.inner.Analyze(ctx, query)
	if err == nil {
		return res, nil
	}
	log.Printf("[analyze] LLM analysis failed (%v) — degrading to rule analyzer", err)
	return fast.RuleAnalyzer{}.Analyze(ctx, query)
}

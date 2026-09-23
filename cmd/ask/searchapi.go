package main

// Search HTTP face (P1): POST /v1/search (JSON, same shape as `ask search
// -raw`) and POST /v1/search/stream (SSE). The SSE event schema maps onto the
// evoke-chat engine callbacks: status → loading, content → appendContent,
// citations → ChatSources, done → completeMessage (P7 对齐).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

// SearchOptions carries the CLI flags and HTTP body knobs on one shape.
type SearchOptions struct {
	Prior   bool          `json:"prior"`
	L1Pre   bool          `json:"l1pre"`
	History []string      `json:"history"`
	HopTS   time.Duration `json:"hopts,omitempty"`
	MinHot  float64       `json:"minhot,omitempty"`
	MinConf float64       `json:"minconf,omitempty"`
}

// searchStack is one fully-wired search pipeline (FAST/KB/DEEP + collaborators),
// shared by the CLI search face and the serve HTTP face.
type searchStack struct {
	fe          *fast.Engine
	kbE         *kb.Engine
	dE          *deep.Engine
	chat        *llm.ChatClient
	st          *ingest.Store
	c           *client.Client
	sourcesColl string
	opt         SearchOptions
}

// newSearchStack wires the production stack (aigate when configured, offline
// stubs otherwise) with the ranked admission and widening callbacks.
func newSearchStack(ctx context.Context, c *client.Client, st *ingest.Store, sourcesColl string, opt SearchOptions) (*searchStack, error) {
	stack := newProdStack()
	fe := fast.New(stack.scorer)
	fe.UsePrior = opt.Prior
	fe.Analyzer, fe.Synth, fe.Expander = stack.analyzer, stack.synth, stack.expander
	kbE := kb.New(fe, cluster.NewCumuStore(c, "ask_clusters"), stack.emb)
	kbE.Edges = graph.NewCumuStore(c, "ask_weak_edges")
	kbE.Cites = deep.NewCumuCiteStore(c, "ask_cites")
	kbE.HopTS = opt.HopTS
	kbE.MinHotness = opt.MinHot
	kbE.MinConfidence = opt.MinConf
	dE := deep.New(kbE, deep.NewCumuStore(c, "ask_conflicts"))
	dE.Scorer = stack.scorer
	dE.Synth = stack.synth
	dE.Widen = widenFunc(fe, st, c, sourcesColl, refinerFor(stack.chat))
	dE.RankAdmission = rankFunc(fe, st, c, sourcesColl)
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

// runSearch executes one query and applies the CLI/HTTP shared side effects
// (evidence marking, B9 token accounting).
func runSearch(ctx context.Context, ss *searchStack, query string) (deep.Result, error) {
	list, err := ss.st.ActiveSources(ctx)
	if err != nil {
		return deep.Result{}, err
	}
	if ss.opt.L1Pre {
		list = ss.narrowL1Pre(ctx, list, query)
	}
	res, err := ss.dE.Ask(ctx, query, list)
	if err != nil {
		return deep.Result{}, err
	}
	if ss.chat != nil {
		res.Tokens = ss.chat.TotalTokens()
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
	Prior   bool     `json:"prior"`
	L1Pre   bool     `json:"l1pre"`
	Stream  bool     `json:"stream"`
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

// registerSearchFace mounts POST /v1/search and POST /v1/search/stream.
func registerSearchFace(mux *http.ServeMux, c *client.Client, st *ingest.Store, sourcesColl string) {
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
			opt := SearchOptions{Prior: in.Prior, L1Pre: in.L1Pre, History: in.History}
			if stream {
				in.Stream = true
			}
			ss, err := newSearchStack(r.Context(), c, st, sourcesColl, opt)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if !in.Stream {
				res, err := runSearch(r.Context(), ss, in.Query)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, res)
				return
			}
			sseSearch(w, r, ss, in.Query)
		}
	}
	mux.HandleFunc("/v1/search", handle(false))
	mux.HandleFunc("/v1/search/stream", handle(true))
}

// sseSearch streams one search as SSE events mapped onto the evoke-chat
// engine: status → loading, content → appendContent, citations →
// ChatSources, done → completeMessage.
func sseSearch(w http.ResponseWriter, r *http.Request, ss *searchStack, query string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
	emit("status", map[string]any{"stage": "started"})

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
	emit("done", map[string]any{
		"mode": res.Mode, "loops": res.Loops, "conf": ans.Confidence,
		"coverage": ans.Coverage, "reused": res.Reused,
		"cluster_id": res.ClusterID, "tokens": res.Tokens,
		"latency_ms": res.LatencyMS,
	})
}

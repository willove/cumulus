package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/prompts"
	"github.com/willove/cumulus/internal/source"
)

func evalLiveAvailable() bool {
	u, err := url.Parse(os.Getenv("LLM_BASE_URL"))
	return !offlineForced() && err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
func evalFingerprint(cfg eval.Config) string {
	raw, _ := json.Marshal(cfg)
	// No secret-bearing endpoint paths, credentials, or API keys in artifacts.
	model, endpoint := "offline-keyword", "offline"
	if cfg.Mode == "live" {
		model = envOr("LLM_CHAT_MODEL", "mimo/cascade-pro")
		endpoint = maskHost(os.Getenv("LLM_BASE_URL"))
	}
	return fmt.Sprintf("%s;config=%s;model=%s;endpoint=%s;embed=local-hash-64;l1=local-hash-64;reuse_theta=%g;merge_theta=%g;split_cap=%d;max_loops=%d;widen_budget=%d;correct_budget=%d;abstain=%s;early_abstain=%s;query_sim=%s;reasoning_split=%s;search_budget=%s;scoring=rule-substring-numeric-boundary,exact-active-gold,citations-all-resolved;budget=upstream-reported-tokens,in-flight-overshoot-possible;cold-start=true;warm-order=dataset", eval.Protocol, raw, model, endpoint, kb.DefaultReuseTheta, kb.DefaultMergeTheta, cluster.DefaultSplitCap, deep.MaxLoops, deep.WidenBudget, deep.CorrectBudget, os.Getenv("CLUS_ABSTAIN"), os.Getenv("CLUS_EARLY_ABSTAIN"), os.Getenv("CLUS_QUERY_SIM"), os.Getenv("LLM_REASONING_SPLIT"), os.Getenv("CLUS_SEARCH_TOKEN_BUDGET"))
}

// A transport guard checks before EVERY model request, including internal
// analyzer/scorer/refiner/synth stages. Calls already in flight may overshoot.
type evalBudgetTransport struct {
	chat    *llm.ChatClient
	ceiling atomic.Int64
	blocked atomic.Bool
	failure atomic.Pointer[string]
	base    http.RoundTripper
}

func (t *evalBudgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if message := t.failure.Load(); message != nil {
		return nil, fmt.Errorf("earlier model stage failed: %s", *message)
	}
	if t.chat.TotalTokens() >= t.ceiling.Load() {
		t.blocked.Store(true)
		return nil, fmt.Errorf("evaluation token budget exhausted")
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		message := evalSafeError(err)
		t.failure.Store(&message)
	} else if response.StatusCode != http.StatusOK {
		message := fmt.Sprintf("model HTTP status %d; token usage may be unavailable", response.StatusCode)
		t.failure.Store(&message)
	} else {
		// Do not silently spend an unlimited budget against providers which omit
		// usage. Replay the bounded payload so ChatClient retains its accounting.
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(payload))
		var usage struct {
			Usage struct {
				Total int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if readErr != nil || len(payload) > 1<<20 || json.Unmarshal(payload, &usage) != nil || usage.Usage.Total <= 0 {
			message := "model returned invalid or missing token usage; cost is unknown; further model calls stopped"
			t.failure.Store(&message)
		}
	}
	return response, err
}

type evalExecutor struct {
	engine     *cumulite.Engine
	stack      *searchStack
	corpus     []source.Source
	keys       map[string]string
	corpusKeys map[string]bool
	cfg        eval.Config
	budget     *evalBudgetTransport
}

func newEvalExecutor(ctx context.Context, rec eval.Record) (eval.Executor, error) {
	if rec.Run.ConfigText != evalFingerprint(rec.Run.Config) {
		return nil, fmt.Errorf("effective configuration changed while queued; start a new run")
	}
	c, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = c.Close()
		}
	}()
	st := ingest.New(c, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(ctx, suiteExtra("")...); err != nil {
		return nil, err
	}
	keys := map[string]string{}
	corpusKeys := map[string]bool{}
	for _, src := range rec.Corpus {
		raw, err := json.Marshal(src)
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		// Insert exact revision identities/metadata; ingest.Put would invent new
		// revisions and invalidate the frozen citation identities.
		if _, err := c.Insert(ctx, "clus_sources", []map[string]any{doc}); err != nil {
			return nil, err
		}
		keys[src.ID] = src.BusinessKey
		if src.BusinessKey != "" {
			corpusKeys[src.BusinessKey] = true
		}
	}
	ps := prodStack{scorer: mcs.KeywordScorer{}, emb: cluster.Local{N: 64}}
	var budget *evalBudgetTransport
	if rec.Run.Config.Mode == "live" {
		if !evalLiveAvailable() {
			return nil, fmt.Errorf("live model unavailable")
		}
		ps = newProdStack()
		// Evaluation uses a fixed local cache embedder: embedding tokens cannot be
		// silently omitted from the advertised chat-token budget.
		ps.emb = cluster.Local{N: 64}
		ps.embErr = nil
		budget = &evalBudgetTransport{chat: ps.chat, base: http.DefaultTransport}
		budget.ceiling.Store(rec.Run.Config.TokenBudget)
		ps.chat.HTTPClient = &http.Client{Timeout: 60 * time.Second, Transport: budget}
	}
	ss, err := newSearchStackWith(ctx, c, st, "clus_sources", SearchOptions{Prior: rec.Run.Config.Prior}, ps)
	if err != nil {
		return nil, err
	}
	if rec.Run.Config.L1Pre {
		if _, err := st.EnsureEmbed(ctx, (cluster.Local{N: 64}).Embed, 64, "eval-local-hash-64", 64); err != nil {
			return nil, err
		}
	}
	success = true
	return &evalExecutor{engine: c, stack: ss, corpus: rec.Corpus, keys: keys, corpusKeys: corpusKeys, cfg: rec.Run.Config, budget: budget}, nil
}
func (e *evalExecutor) Close() error { return e.engine.Close() }
func (e *evalExecutor) Execute(ctx context.Context, it eval.Item, remaining int64) (out eval.ItemResult) {
	started := time.Now()
	out = eval.ItemResult{ID: it.ID, Query: it.Query, Reference: it.Answer, Gold: it.Gold, State: "completed", Citations: []eval.Citation{}}
	defer func() { out.LatencyMS = time.Since(started).Milliseconds() }()
	fail := func(stage string, err error) {
		out.State = "failed"
		out.ErrorStage = stage
		out.Error = evalSafeError(err)
	}
	chat := e.stack.chat
	if e.budget != nil {
		e.budget.ceiling.Store(chat.TotalTokens() + remaining)
		e.budget.blocked.Store(false)
		e.budget.failure.Store(nil)
	}
	list := e.corpus
	if e.cfg.L1Pre {
		narrowed, err := narrowByKNN(ctx, e.engine, (cluster.Local{N: 64}).Embed, "clus_sources", list, it.Query)
		if err != nil {
			fail("l1pre", err)
			return
		}
		if len(narrowed) > 0 {
			list = narrowed
		}
	}
	rec, res := evalSearchOne(ctx, e.stack.dE, chat, list, e.keys, e.corpusKeys, it)
	out.SearchTokens = rec.SearchTokens
	out.Answer = res.Answer.Summary
	out.Mode = res.Mode
	// Unlike legacy canonicalKeys, v2 resolves gold to EXACT snapshot IDs. A
	// stale revision or a similarly named business key cannot earn evidence credit.
	goldIDs := map[string]bool{}
	byID := map[string]source.Source{}
	for _, s := range e.corpus {
		byID[s.ID] = s
		for _, g := range it.Gold {
			if g == s.ID || g == s.BusinessKey {
				goldIDs[s.ID] = true
			}
		}
	}
	out.RuleMatch = eval.Correct(it.Answer, out.Answer)
	allResolved := len(res.Citations.Refs) > 0 && strings.TrimSpace(out.Answer) != "" && !res.Answer.Skipped
	for _, ref := range res.Citations.Refs {
		src, exists := byID[ref.SourceID]
		resolved := exists && ref.Resolved
		if !resolved {
			allResolved = false
		}
		if goldIDs[ref.SourceID] {
			out.EvidenceHit = true
		}
		out.Citations = append(out.Citations, eval.Citation{SourceID: ref.SourceID, Title: src.Title, Quote: ref.Quote, Resolved: resolved})
	}
	out.CitationResolved = allResolved
	if rec.Err != "" {
		fail("search", fmt.Errorf("%s", rec.Err))
		return
	}
	if ctx.Err() != nil {
		fail("search", ctx.Err())
		return
	}
	if e.budget != nil && e.budget.blocked.Load() {
		fail("budget", fmt.Errorf("token budget exhausted between search model stages"))
		return
	}
	if e.budget != nil {
		if message := e.budget.failure.Load(); message != nil {
			fail("search", fmt.Errorf("model stage failed: %s", *message))
			return
		}
	}
	available := func(stage string) bool {
		if ctx.Err() != nil {
			fail(stage, ctx.Err())
			return false
		}
		if out.SearchTokens+out.JudgeTokens+out.ClosedBookTokens >= remaining {
			fail("budget", fmt.Errorf("token budget exhausted before %s; in-flight overshoot possible", stage))
			return false
		}
		return true
	}
	if e.cfg.Judge {
		if !available("judge") {
			out.JudgeError = out.Error
			return
		}
		before := chat.TotalTokens()
		ok, reason, err := judgeAnswer(ctx, chat, it.Query, it.Answer, out.Answer)
		out.JudgeTokens = chat.TotalTokens() - before
		if e.budget != nil {
			if message := e.budget.failure.Load(); message != nil {
				err = fmt.Errorf("%s", *message)
			}
		}
		if err != nil {
			out.JudgeError = evalSafeError(err)
			fail("judge", err)
		} else {
			out.JudgeCorrect = &ok
			out.JudgeReason = reason
		}
	}
	if e.cfg.ClosedBook {
		if !available("closed_book") {
			return
		}
		before := chat.TotalTokens()
		answer, err := chat.Complete(ctx, prompts.MustRender(prompts.ClosedBook, map[string]string{"query": it.Query}))
		out.ClosedBookTokens = chat.TotalTokens() - before
		out.ClosedBookAnswer = answer
		if e.budget != nil {
			if message := e.budget.failure.Load(); message != nil {
				err = fmt.Errorf("%s", *message)
			}
		}
		if err != nil {
			fail("closed_book", err)
		} else {
			match := eval.Correct(it.Answer, answer)
			out.ClosedBookMatch = &match
		}
	}
	return
}
func evalSafeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, secret := range []string{os.Getenv("LLM_API_KEY"), os.Getenv("LLM_BASE_URL")} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}

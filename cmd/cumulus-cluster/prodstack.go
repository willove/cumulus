package main

// prodStack bundles the endpoint-backed collaborators shared by the search and
// eval-run faces (D6). Nil interfaces are the offline stubs that keep gates
// deterministic without LLM_BASE_URL.

import (
	"fmt"
	"os"
	"strings"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/minilm"
)

type prodStack struct {
	scorer   mcs.Scorer
	emb      cluster.Embedder
	analyzer fast.Analyzer
	synth    fast.Synthesizer
	expander fast.KeywordExpander
	rewriter deep.HistoryRewriter
	chat     *llm.ChatClient
	// embErr carries an unhonored embedder request (CLUS_EMBED=minilm with the
	// weights absent): the stack still builds so /v1/config can report the
	// cause and the workbench can offer the download, but newSearchStack
	// refuses to serve on it.
	embErr error
	// stageEffort is the per-stage thinking depth (MiniMax M3.1+/OpenAI
	// o1+): keys are stage names (ANALYZE, SCORE, SYNTH, JUDGE, EXPAND),
	// values are ThinkingLevel. Consumers call CompleteWithEffort with the
	// stage's level instead of the binary Complete/CompleteStructured.
	stageEffort map[string]llm.ThinkingLevel
}

func newProdStack() prodStack {
	ps := prodStack{
		scorer: mcs.KeywordScorer{},
		emb:    cluster.Local{N: 64},
	}
	// 簇语义缓存（Sirchmunk 对齐位）：MiniLM 只嵌查询与簇摘要——查询驱动的
	// 复用匹配，从不预嵌语料。语料侧向量仍是 opt-in 加速器（embedderFor）。
	//
	// 要了 minilm 却没有权重就是硬失败（embErr），由 newSearchStack 向外传播。
	// 这里曾经静默退回本地 hash，部署态因此误以为在跑语义模型；现在退回是显式
	// 的（CLUS_EMBED=hash），事故不可能再伪装成选择。
	cacheEmbedder := "local-hash-64"
	if os.Getenv("CLUS_EMBED") == "minilm" {
		emb, err := minilm.Resolve()
		if err != nil {
			ps.embErr = err
			cacheEmbedder = "ERROR: " + err.Error()
		} else {
			ps.emb = emb
			cacheEmbedder = "minilm-l12-384"
		}
	}
	// Still VERBOSE-gated: newProdStack runs per request, so an unconditional
	// line here is stderr spam. The failure path is loud by construction — it
	// rides embErr into a 500 and into /v1/config's embedder_error.
	if os.Getenv("CLUS_VERBOSE") == "1" {
		fmt.Fprintf(os.Stderr, "[stack] 簇语义缓存 embedder=%s\n", cacheEmbedder)
	}
	base := os.Getenv("LLM_BASE_URL")
	if base == "" || offlineForced() {
		// CLUS_OFFLINE=1 pins the offline stubs even with an endpoint
		// configured — gate harnesses must not reach a live model.
		return ps
	}
	split := strings.Contains(strings.ToLower(base), "minimaxi.com")
	if v := os.Getenv("LLM_REASONING_SPLIT"); v != "" {
		split = v == "1" || strings.EqualFold(v, "true")
	}
	chat := &llm.ChatClient{
		BaseURL:        base,
		APIKey:         os.Getenv("LLM_API_KEY"),
		Model:          envOr("LLM_CHAT_MODEL", "mimo/cascade-pro"),
		Caller:         "cumulus-cluster",
		ReasoningSplit: split,
		// One pooled client for the process. ChatClient.http() builds a fresh
		// http.Client on every call when this is nil, so leaving it unset cost
		// a TCP+TLS handshake per LLM call and kept no idle connections —
		// waste when serial, a connection storm once the scorer runs
		// concurrent. MaxIdleConnsPerHost is sized above the scorer's default
		// worker cap so a concurrent round does not serialise on the pool.
		HTTPClient: newPooledHTTPClient(defaultScorerWorkers()),
	}
	ps.chat = chat
	// CLUS_SCORER_NOTHINK is the A/B switch for evaluate_sample: route it
	// through CompleteStructured (private thinking off). Measured by
	// cmd/scoreprobe: 2.28x faster, score_gap 5.70 → 5.52, top_gold_stable
	// 100% either way. The mid-band stability question is now ANSWERED and
	// the answer is negative (var/scoreprobe-trial{,-nothink}.json, 5 items
	// × 3 repeats): decision_stable_rate 0.64 thinking-on vs 0.52 off,
	// mean_stddev 0.64 vs 0.73 — nothink wobbles MORE in the [4,8) band the
	// early-stop arm decides in, so the speed win cannot be banked by waiting
	// for the model to settle. The stop decision is already two-factor
	// (score line × rep.Complete keyword cover), which is what keeps either
	// mode's wobble from fabricating a stop; flipping this default would need
	// a hysteresis/confirm design on the stop line itself, not more patience.
	ps.scorer = &llm.Scorer{Client: chat, NoThink: envFlag("CLUS_SCORER_NOTHINK"), Effort: ps.stageEffort["SCORE"]}
	ps.analyzer = &llm.Analyzer{Client: chat, Effort: ps.stageEffort["ANALYZE"]}
	ps.synth = &llm.Synthesizer{Client: chat, Effort: ps.stageEffort["SYNTH"]}
	ps.expander = &llm.KeywordExpander{Client: chat, Levels: 3}
	ps.rewriter = &llm.HistoryRewriter{Client: chat}
	// Per-stage thinking depth (MiniMax M3.1+/OpenAI o1+ compatible):
	// CLUS_THINK_<STAGE>=low|medium|high|xhigh|max overrides per stage.
	// Defaults: mechanical passes low, quality moments high. Set on the
	// chat client as the per-request effort when CompleteWithEffort is used
	// (the Complete/CompleteStructured binary is the degenerate spectrum).
	ps.stageEffort = map[string]llm.ThinkingLevel{
		"ANALYZE": llm.StageEffort("ANALYZE", llm.ThinkingLow),
		"SCORE":   llm.StageEffort("SCORE", llm.ThinkingLow),
		"SYNTH":   llm.StageEffort("SYNTH", llm.ThinkingMedium),
		"JUDGE":   llm.StageEffort("JUDGE", llm.ThinkingHigh),
		"EXPAND":  llm.StageEffort("EXPAND", llm.ThinkingLow),
	}
	// Atomic-fact decomposer (P1-4). The offline heuristic is measurably bad:
	// K=1 for 124/136 real queries, and all 12 K>1 splits are miscuts inside a
	// title / defined term / enumeration, which become phantom requirements the
	// DEEP loop can never satisfy. Wired ONLY when a live endpoint is present,
	// so every offline gate keeps the deterministic path byte-for-byte (D6).
	// Embeddings switch only on an explicit LLM_EMBED_MODEL: the gateway's
	// chat surface is the proven path, and a silent embed probe against a
	// chat-only gateway would fail every search.
	if os.Getenv("LLM_EMBED_MODEL") != "" {
		ps.emb = &llm.Embedder{
			BaseURL: base,
			APIKey:  os.Getenv("LLM_API_KEY"),
			Model:   os.Getenv("LLM_EMBED_MODEL"),
			N:       64,
		}
		if os.Getenv("CLUS_VERBOSE") == "1" {
			fmt.Fprintf(os.Stderr, "[stack] 簇语义缓存 embedder=remote:%s\n", os.Getenv("LLM_EMBED_MODEL"))
		}
	}
	return ps
}

// maxDeepLoops is the DEEP admission slice: loop budget × a small margin, so
// the ranked head always contains the whole first exploration wave.
const maxDeepLoops = deep.MaxLoops

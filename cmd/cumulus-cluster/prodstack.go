package main

// prodStack bundles the aigate-backed collaborators shared by the search and
// eval-run faces (D6). Nil interfaces are the offline stubs that keep gates
// deterministic without AIGATE_BASE_URL.

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
	// embErr carries a strict-mode failure (CLUS_MINILM_REQUIRE=1 with the
	// weights absent): the stack still builds with the offline fallback, but
	// newSearchStack refuses to serve on it.
	embErr error
}

func newProdStack() prodStack {
	ps := prodStack{
		scorer: mcs.KeywordScorer{},
		emb:    cluster.Local{N: 64},
	}
	// 簇语义缓存（Sirchmunk 对齐位）：MiniLM 只嵌查询与簇摘要——查询驱动的
	// 复用匹配，从不预嵌语料。语料侧向量仍是 opt-in 加速器（embedderFor）。
	//
	// AS_EMBED 忘设或权重缺席时会**静默**退回本地 hash——
	// 部署态因此误以为在跑语义模型。verbose 下必须说清用的是哪一把；
	// CLUS_MINILM_REQUIRE=1 则把「权重缺席」升级为硬失败（embErr），
	// 由 newSearchStack 向外传播——CI 精度门不许静默降级读绿。
	cacheEmbedder := "local-hash-64"
	if os.Getenv("CLUS_EMBED") == "minilm" {
		emb, err := minilm.Resolve()
		if err != nil {
			ps.embErr = err
			cacheEmbedder = "ERROR: " + err.Error()
		} else if emb != nil {
			ps.emb = emb
			cacheEmbedder = "minilm-l12-384"
		} else {
			cacheEmbedder = "local-hash-64 (CLUS_EMBED=minilm 权重缺席，静默降级)"
		}
	}
	if os.Getenv("CLUS_VERBOSE") == "1" {
		fmt.Fprintf(os.Stderr, "[stack] 簇语义缓存 embedder=%s\n", cacheEmbedder)
	}
	base := os.Getenv("AIGATE_BASE_URL")
	if base == "" || offlineForced() {
		// CLUS_OFFLINE=1 pins the offline stubs even with an endpoint
		// configured — gate harnesses must not reach a live model.
		return ps
	}
	split := strings.Contains(strings.ToLower(base), "minimaxi.com")
	if v := os.Getenv("AIGATE_REASONING_SPLIT"); v != "" {
		split = v == "1" || strings.EqualFold(v, "true")
	}
	chat := &llm.ChatClient{
		BaseURL:        base,
		APIKey:         os.Getenv("AIGATE_API_KEY"),
		Model:          envOr("AIGATE_CHAT_MODEL", "mimo/cascade-pro"),
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
	ps.scorer = &llm.AigateScorer{Client: chat, NoThink: envFlag("CLUS_SCORER_NOTHINK")}
	ps.analyzer = &llm.AigateAnalyzer{Client: chat}
	ps.synth = &llm.AigateSynthesizer{Client: chat}
	ps.expander = &llm.AigateKeywordExpander{Client: chat, Levels: 3}
	ps.rewriter = &llm.AigateHistoryRewriter{Client: chat}
	// Atomic-fact decomposer (P1-4). The offline heuristic is measurably bad:
	// K=1 for 124/136 real queries, and all 12 K>1 splits are miscuts inside a
	// title / defined term / enumeration, which become phantom requirements the
	// DEEP loop can never satisfy. Wired ONLY when a live endpoint is present,
	// so every offline gate keeps the deterministic path byte-for-byte (D6).
	// Embeddings switch only on an explicit AIGATE_EMBED_MODEL: the gateway's
	// chat surface is the proven path, and a silent embed probe against a
	// chat-only gateway would fail every search.
	if os.Getenv("AIGATE_EMBED_MODEL") != "" {
		ps.emb = &llm.AigateEmbedder{
			BaseURL: base,
			APIKey:  os.Getenv("AIGATE_API_KEY"),
			Model:   os.Getenv("AIGATE_EMBED_MODEL"),
			N:       64,
		}
		if os.Getenv("CLUS_VERBOSE") == "1" {
			fmt.Fprintf(os.Stderr, "[stack] 簇语义缓存 embedder=aigate:%s\n", os.Getenv("AIGATE_EMBED_MODEL"))
		}
	}
	return ps
}

// maxDeepLoops is the DEEP admission slice: loop budget × a small margin, so
// the ranked head always contains the whole first exploration wave.
const maxDeepLoops = deep.MaxLoops

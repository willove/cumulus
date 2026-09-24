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
	if base == "" {
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
	}
	ps.chat = chat
	ps.scorer = &llm.AigateScorer{Client: chat}
	ps.analyzer = &llm.AigateAnalyzer{Client: chat}
	ps.synth = &llm.AigateSynthesizer{Client: chat}
	ps.expander = &llm.AigateKeywordExpander{Client: chat, Levels: 3}
	ps.rewriter = &llm.AigateHistoryRewriter{Client: chat}
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

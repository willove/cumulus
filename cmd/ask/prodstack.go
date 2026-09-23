package main

// prodStack bundles the aigate-backed collaborators shared by the search and
// eval-run faces (D6). Nil interfaces are the offline stubs that keep gates
// deterministic without AIGATE_BASE_URL.

import (
	"fmt"
	"os"
	"strings"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/minilm"
)

type prodStack struct {
	scorer   mcs.Scorer
	emb      cluster.Embedder
	analyzer fast.Analyzer
	synth    fast.Synthesizer
	expander fast.KeywordExpander
	rewriter deep.HistoryRewriter
	chat     *llm.ChatClient
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
	// 部署态因此误以为在跑语义模型。verbose 下必须说清用的是哪一把。
	cacheEmbedder := "local-hash-64"
	if os.Getenv("ASK_EMBED") == "minilm" {
		if minilm.Available() {
			ps.emb = minilm.New(minilm.DefaultDir())
			cacheEmbedder = "minilm-l12-384"
		} else {
			cacheEmbedder = "local-hash-64 (ASK_EMBED=minilm 权重缺席，静默降级)"
		}
	}
	if os.Getenv("ASK_VERBOSE") == "1" {
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
		Caller:         "ask",
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
		if os.Getenv("ASK_VERBOSE") == "1" {
			fmt.Fprintf(os.Stderr, "[stack] 簇语义缓存 embedder=aigate:%s\n", os.Getenv("AIGATE_EMBED_MODEL"))
		}
	}
	return ps
}

// maxDeepLoops is the DEEP admission slice: loop budget × a small margin, so
// the ranked head always contains the whole first exploration wave.
const maxDeepLoops = deep.MaxLoops

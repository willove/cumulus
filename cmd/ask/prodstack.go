package main

// prodStack bundles the aigate-backed collaborators shared by the search and
// eval-run faces (D6). Nil interfaces are the offline stubs that keep gates
// deterministic without AIGATE_BASE_URL.

import (
	"os"
	"strings"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/mcs"
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
	}
	return ps
}

package main

// The affinity face of the search API: read the usage ledger + session
// evidence stack into the prior before a search, fold a finished answer's
// usage back after it. Both paths are best-effort and env-gated
// (CLUS_AFFINITY=0 turns the whole layer off and restores the previous
// global-binary history behaviour).

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/affinity"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/prior"
)

func sampleContextEnabled() bool {
	return os.Getenv("CLUS_SAMPLE_CONTEXT") != "0"
}

// sampleContextText folds a thread's recent USER questions into a sampler
// fallback. Assistant lines are excluded: the fallback exists to bridge
// wording gaps against statutory text, not to replay the previous answer.
func sampleContextText(histLines []string) string {
	if !sampleContextEnabled() {
		return ""
	}
	var qs []string
	for _, line := range histLines {
		if !strings.HasPrefix(line, "user: ") {
			continue
		}
		if q := strings.TrimSpace(strings.TrimPrefix(line, "user: ")); q != "" {
			qs = append(qs, q)
		}
	}
	if len(qs) == 0 {
		return ""
	}
	if len(qs) > 3 {
		qs = qs[len(qs)-3:]
	}
	joined := strings.Join(qs, " ")
	// Cap by runes: the fallback is a scoring hint, not a second question.
	runes := []rune(joined)
	if len(runes) > 240 {
		joined = string(runes[:240])
	}
	return joined
}

func affinityEnabled() bool {
	return os.Getenv("CLUS_AFFINITY") != "0"
}

// ledgerRankEnabled gates whether the CROSS-SESSION ledger reshapes ranking.
// Default off, and that default is measured, not guessed: the 3-pass A/B showed
// the ledger path has no verified win (the single-query wins were cluster
// reuse, which both arms share) while it actively hurt specific queries
// (traffic Q1 50s→94s, noise query 31s→90s) — the boost reordered candidates
// and stretched the DEEP crawl even when it picked the right document. The
// ledger is still WRITTEN (memory for diagnostics and the mining track); only
// its ranking influence is gated. CLUS_LEDGER_RANK=1 restores the old path.
func ledgerRankEnabled() bool {
	return os.Getenv("CLUS_LEDGER_RANK") == "1"
}

func sessionEvidenceEnabled() bool {
	return os.Getenv("CLUS_SESSION_EVIDENCE") != "0"
}

// queryTokens are the ledger keys for one query: the same CJK-bigram fields
// the prior ranks with, so the two signals speak one vocabulary.
func queryTokens(query string) []string {
	return affinity.TrimTokens(mcs.Fields(query), affinity.DefaultMaxTokensPerQ)
}

// citedDocIDs collects the documents the answer actually leaned on.
func citedDocIDs(refs []deep.Ref) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if r.SourceID == "" || seen[r.SourceID] {
			continue
		}
		seen[r.SourceID] = true
		out = append(out, r.SourceID)
	}
	return out
}

// applyUsagePrior fuses the ledger weights with this session's evidence
// stack (max wins — a document cited one turn ago outranks ledger history)
// and installs them on the fast engine's history arm. A silent no-op when
// the prior is off, the switch is off, or both sources are empty.
func applyUsagePrior(ctx context.Context, c cumulite.Port, namespace, query, sessionID string, fe *fast.Engine, de *deep.Engine, usage *usageWeights) {
	if !affinityEnabled() || fe == nil {
		return
	}
	toks := queryTokens(query)
	if len(toks) == 0 {
		return
	}
	weights, err := affinity.NewCumuStore(c, ns.Coll(namespace, "clus_affinity")).Weights(ctx, toks, time.Now())
	if err != nil {
		return // a ledger read failure must never fail a query
	}
	// 会话栈（本线程的私有证据）优先参与；账本默认只记不重排（见上）。
	sessionW := map[string]float64{}
	if sessionEvidenceEnabled() && sessionID != "" {
		for id, w := range sessEvidence.Weights(namespace, sessionID, time.Now()) {
			sessionW[id] = w
		}
	}
	if !ledgerRankEnabled() {
		weights = sessionW
	} else {
		for id, w := range sessionW {
			if w > weights[id] {
				weights[id] = w
			}
		}
	}
	if len(weights) == 0 {
		return
	}
	// 两条装填路径：FAST 级联的直通项（主路径，权重不再被 prior 稀释）与
	// DEEP 探索前的候选提升。prior 的 history 臂同步保留（离线/历史臂消费者）。
	fe.Usage = weights
	if usage != nil {
		usage.set(weights)
	}
	if de != nil {
		de.DocWeights = weights
	}
	if fe.PriorHist == nil {
		fe.PriorHist = &prior.History{}
	}
	if fe.PriorHist.DocWeights == nil {
		fe.PriorHist.DocWeights = map[string]float64{}
	}
	for id, w := range weights {
		if w > fe.PriorHist.DocWeights[id] {
			fe.PriorHist.DocWeights[id] = w
		}
	}
}

// recordUsage folds one finished answer into the ledger and the session
// stack. Weight scales with the answer's confidence: a weak answer still
// records topical adjacency, a strong one teaches properly.
func recordUsage(ctx context.Context, c cumulite.Port, namespace, query, sessionID string, refs []deep.Ref, anchor string, conf float64) {
	docs := citedDocIDs(refs)
	if len(docs) == 0 && anchor == "" {
		return
	}
	if sessionEvidenceEnabled() && sessionID != "" {
		sessEvidence.Add(namespace, sessionID, docs, anchor, time.Now())
	}
	if !affinityEnabled() {
		return
	}
	// Best effort: the answer is already on screen; a failed ledger write
	// is a lost learning opportunity, not a failed query. Debug-gated so a
	// silently empty ledger (a missing collection, say) is findable.
	if err := affinity.NewCumuStore(c, ns.Coll(namespace, "clus_affinity")).
		Record(ctx, queryTokens(query), docs, affinity.OutcomeWeight(conf), time.Now()); err != nil && os.Getenv("CLUS_AFFINITY_DEBUG") == "1" {
		log.Printf("[affinity] record %s: %v", namespace, err)
	}
}

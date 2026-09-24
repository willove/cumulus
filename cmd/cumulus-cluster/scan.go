package main

// The scan face (P9): `cumulus-cluster scan` discovers ingest candidates in a
// directory (rules; optional LLM topic rank), `ingest-files -candidates`
// feeds the trimmed list through the same job state machine. Scanning never
// opens the store — like env, it is a pure pre-ingest step.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/prompts"
)

// scanRankFunc adapts the chat client to an ingest.Ranker: the prompt ranks
// candidate paths for one query topic; the parse is defensive (unknown paths
// dropped, duplicates collapsed) so a sloppy answer degrades to rule order,
// never to a crash. nil client = no LLM ranking.
func scanRankFunc(chat *llm.ChatClient) ingest.Ranker {
	if chat == nil {
		return nil
	}
	return func(ctx context.Context, query string, cands []ingest.Candidate) ([]string, error) {
		known := make(map[string]bool, len(cands))
		lines := make([]string, 0, len(cands))
		for _, c := range cands {
			known[c.Path] = true
			head := c.Headline
			if head == "" {
				head = "(binary)"
			}
			lines = append(lines, fmt.Sprintf("%s | %s | %dB | %s | %s",
				c.Path, c.Ext, c.Size, c.ModTime, head))
		}
		user := prompts.MustRender(prompts.ScanRank, map[string]string{
			"query":      query,
			"candidates": strings.Join(lines, "\n"),
		})
		raw, err := chat.Complete(ctx, user)
		if err != nil {
			return nil, err
		}
		return parseScanRank(raw, known)
	}
}

// parseScanRank extracts {"ranking":[...]} from a model answer, keeping only
// known paths (first mention wins). The first { / last } slice already copes
// with fenced or prose-wrapped JSON; a missing ranking is an error so
// ApplyRank keeps the rule order.
func parseScanRank(raw string, known map[string]bool) ([]string, error) {
	body := strings.TrimSpace(raw)
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("scan_rank: no JSON object in answer")
	}
	var out struct {
		Ranking []string `json:"ranking"`
	}
	if err := json.Unmarshal([]byte(body[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("scan_rank: %w", err)
	}
	if len(out.Ranking) == 0 {
		return nil, fmt.Errorf("scan_rank: empty ranking")
	}
	seen := map[string]bool{}
	ranked := make([]string, 0, len(out.Ranking))
	for _, p := range out.Ranking {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if known == nil || known[p] {
			ranked = append(ranked, p)
		}
	}
	if len(ranked) == 0 {
		return nil, fmt.Errorf("scan_rank: no known paths in ranking")
	}
	return ranked, nil
}

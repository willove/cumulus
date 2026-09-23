package main

// priorHist builds the LENS B4 history arm from live ask_evidence windows
// (历史成功证据). Read failure degrades to nil — the history arm stays
// silent rather than blocking the search.

import (
	"context"

	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/prior"
	"github.com/cumubase/ask/internal/source"
)

// priorHistFromStore reads live high-score evidence and folds it into a
// prior.History. sources passed in are restricted to the active set so
// retired documents never contribute.
func priorHistFromStore(ctx context.Context, st *ingest.Store, sources []source.Source) *prior.History {
	if st == nil {
		return nil
	}
	active := map[string]bool{}
	for _, s := range sources {
		if s.Status == source.StatusActive {
			active[s.ID] = true
		}
	}
	hits, err := st.ActiveEvidence(ctx, 7, 200)
	if err != nil || len(hits) == 0 {
		return nil
	}
	var ids, snips []string
	for _, h := range hits {
		if !active[h.SourceID] {
			continue
		}
		ids = append(ids, h.SourceID)
		if h.Snippet != "" {
			snips = append(snips, h.Snippet)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return prior.HistoryFrom(ids, snips)
}

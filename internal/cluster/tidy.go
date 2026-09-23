package cluster

// Cluster tidy (P6): the write path only merges within one topic key
// (kb.pickMergeable reads FindByTopic), so near-duplicate clusters asked in
// genuinely different wordings survive across topics — Sirchmunk's "size"
// fracture our R2 probe measured on their side. Tidy is the maintenance sweep
// that folds those survivors: pairwise embed-similar sweep over ONE namespace's
// collection, deterministic (older cluster wins), idempotent, and explicit —
// operators run it; nothing is deleted as a side effect of search.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/cumubase/ask/internal/mcs"
)

// DefaultTidyTheta is the fold line for the sweep — the same cosine bar the
// write path applies within a topic (G-merge), now across topics.
const DefaultTidyTheta = 0.55

// TidyPair records one fold (or, in dry-run, one fold that would happen).
type TidyPair struct {
	Winner string  `json:"winner"` // surviving cluster (older CreatedAt wins)
	Loser  string  `json:"loser"`  // folded cluster, deleted unless dry-run
	Sim    float64 `json:"sim"`    // cosine that triggered the fold
}

// TidyReport summarizes one sweep. Lifecycle counts are the population
// snapshot so operators see drift (emerging/contested/deprecated) even when
// nothing merges.
type TidyReport struct {
	Scanned           int            `json:"scanned"`
	Merged            int            `json:"merged"`
	SkippedContested  int            `json:"skipped_contested"`
	SkippedDeprecated int            `json:"skipped_deprecated"`
	Lifecycle         map[string]int `json:"lifecycle"`
	Pairs             []TidyPair     `json:"pairs,omitempty"`
	DryRun            bool           `json:"dry_run"`
}

// QuerySetEmbed recomputes a cluster embedding from its retained queries
// (D3: query-driven, not content-driven). Mean-pooled; falls back to the
// current embedding when the query set is empty or the embedder fails.
func QuerySetEmbed(ctx context.Context, emb Embedder, queries []string) ([]float64, error) {
	if emb == nil || len(queries) == 0 {
		return nil, fmt.Errorf("cluster: query-set embed needs an embedder and queries")
	}
	vs, err := emb.Embed(ctx, queries)
	if err != nil || len(vs) != len(queries) {
		return nil, fmt.Errorf("cluster: embed %d queries: %w", len(queries), err)
	}
	dims := len(vs[0])
	mean := make([]float64, dims)
	for _, v := range vs {
		if len(v) != dims {
			return nil, fmt.Errorf("cluster: embedder returned dim %d, want %d", len(v), dims)
		}
		for i, x := range v {
			mean[i] += x
		}
	}
	for i := range mean {
		mean[i] /= float64(len(vs))
	}
	return mean, nil
}

// Tidy sweeps the store for cross-topic near-duplicates and folds them.
// Determinism: pairs are processed in (winner, loser) id order, the older
// cluster (CreatedAt; ties by smaller ID) always survives, and the winner's
// embedding is recomputed from the merged query set each round, so a second
// sweep on the same store finds nothing new. Contested and deprecated
// clusters are never folded (a conflict edge or a retire decision outranks a
// cosine); they are counted, not silently skipped. maxMerges bounds the work
// (0 = unlimited).
func Tidy(ctx context.Context, st Store, emb Embedder, theta float64, dryRun bool, maxMerges int) (TidyReport, error) {
	rep := TidyReport{Lifecycle: map[string]int{}, DryRun: dryRun}
	if theta <= 0 {
		theta = DefaultTidyTheta
	}
	all, err := st.All(ctx)
	if err != nil {
		return rep, err
	}
	rep.Scanned = len(all)

	work := make([]Cluster, 0, len(all))
	alive := map[string]bool{}
	for _, c := range all {
		rep.Lifecycle[c.Lifecycle]++
		switch c.Lifecycle {
		case LifecycleContested:
			rep.SkippedContested++
			continue
		case LifecycleDeprecated:
			rep.SkippedDeprecated++
			continue
		}
		work = append(work, c)
		alive[c.ID] = true
	}
	sort.Slice(work, func(i, j int) bool { return work[i].ID < work[j].ID })

	for {
		if maxMerges > 0 && rep.Merged >= maxMerges {
			break
		}
		// Best pair: highest cosine wins; ties broken by the canonical
		// (winner, loser) id pair so the sweep is deterministic.
		var best *TidyPair
		var bestWI, bestLI int
		for i := 0; i < len(work); i++ {
			if !alive[work[i].ID] {
				continue
			}
			for j := i + 1; j < len(work); j++ {
				if !alive[work[j].ID] {
					continue
				}
				sim := Cosine(work[i].Embed, work[j].Embed)
				if sim < theta {
					continue
				}
				w, l := orderByAge(work[i], work[j])
				cand := &TidyPair{Winner: w.ID, Loser: l.ID, Sim: sim}
				if best == nil || betterPair(*cand, *best) {
					best, bestWI, bestLI = cand, i, j
				}
			}
		}
		if best == nil {
			break
		}
		winner, loser := orderByAge(work[bestWI], work[bestLI])
		rep.Pairs = append(rep.Pairs, *best)
		if dryRun {
			break // one exemplar pair is enough to show what would happen
		}
		foldInto(&winner, &loser)
		if emb != nil {
			if qe, eerr := QuerySetEmbed(ctx, emb, winner.Queries); eerr == nil {
				winner.Embed = qe
			}
		}
		if err := st.Save(ctx, winner); err != nil {
			return rep, fmt.Errorf("tidy save %s: %w", winner.ID, err)
		}
		if err := st.Delete(ctx, loser.ID); err != nil {
			return rep, fmt.Errorf("tidy delete %s: %w", loser.ID, err)
		}
		alive[loser.ID] = false
		work[bestWI], work[bestLI] = winner, loser
		rep.Merged++
	}
	return rep, nil
}

// betterPair orders candidates: higher sim first, then smaller winner id,
// then smaller loser id.
func betterPair(a, b TidyPair) bool {
	if a.Sim != b.Sim {
		return a.Sim > b.Sim
	}
	if a.Winner != b.Winner {
		return a.Winner < b.Winner
	}
	return a.Loser < b.Loser
}

// orderByAge picks the survivor: older CreatedAt wins (stable accumulation),
// ties broken by smaller ID.
func orderByAge(a, b Cluster) (winner, loser Cluster) {
	if b.CreatedAt.Before(a.CreatedAt) || (b.CreatedAt.Equal(a.CreatedAt) && b.ID < a.ID) {
		return b, a
	}
	return a, b
}

// foldInto merges loser into winner in place — same fold semantics as the kb
// merge path (content join, averaged confidence, query FIFO), plus: evidence
// deduped by window identity, hotness takes the max (heat transfers, doubles
// do not stack), and flags union.
func foldInto(winner, loser *Cluster) {
	winner.Content = winner.Content + "\n---\n" + loser.Content
	winner.Confidence = (winner.Confidence + loser.Confidence) / 2
	if loser.Hotness > winner.Hotness {
		winner.Hotness = loser.Hotness
	}
	for _, q := range loser.Queries {
		if !containsString(winner.Queries, q) {
			winner.Queries = append(winner.Queries, q)
		}
	}
	if len(winner.Queries) > MaxQueriesPerCluster {
		winner.Queries = winner.Queries[len(winner.Queries)-MaxQueriesPerCluster:]
	}
	// Topic aliases outlive the bounded query FIFO; the survivor keeps its own key.
	var aliases []string
	seenTopics := map[string]bool{winner.TopicKey: true, "": true}
	for _, keys := range [][]string{winner.TopicKeys, {loser.TopicKey}, loser.TopicKeys} {
		for _, key := range keys {
			if !seenTopics[key] {
				seenTopics[key] = true
				aliases = append(aliases, key)
			}
		}
	}
	winner.TopicKeys = aliases

	// Legacy samples carry sampling methods, not document IDs. Normalize both
	// sides before comparing spans, without mutating either input evidence slice.
	winner.Evidence = NormalizeEvidence(winner.SourceID, winner.Evidence)
	loserEvidence := NormalizeEvidence(loser.SourceID, loser.Evidence)
	seen := map[string]bool{}
	for _, ev := range winner.Evidence {
		seen[evidenceKey(ev)] = true
	}
	for _, ev := range loserEvidence {
		k := evidenceKey(ev)
		if seen[k] {
			continue
		}
		seen[k] = true
		winner.Evidence = append(winner.Evidence, ev)
	}
	for f, v := range loser.Flags {
		if v {
			if winner.Flags == nil {
				winner.Flags = map[string]bool{}
			}
			winner.Flags[f] = true
		}
	}
	winner.Version++
	winner.UpdatedAt = time.Now().UTC()
}

// evidenceKey identifies one window: same source + same span = same evidence,
// regardless of score annotations (a re-scored window is not new evidence).
func evidenceKey(ev mcs.Sample) string {
	return fmt.Sprintf("%s|%d|%d", ev.Source, ev.Start, ev.End)
}

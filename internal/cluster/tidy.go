package cluster

// Cluster tidy: the write path only merges within one topic key
// (kb.pickMergeable reads FindByTopic), so near-duplicate clusters asked in
// genuinely different wordings survive across topics — the "size" fracture
// Sirchmunk shows on its side. Tidy is the maintenance sweep
// that folds those survivors: pairwise embed-similar sweep over ONE namespace's
// collection, deterministic (older cluster wins), idempotent, and explicit —
// operators run it; nothing is deleted as a side effect of search.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/willove/cumulus/internal/mcs"
)

// DefaultTidyTheta is the fold line for the sweep — the same cosine bar the
// write path applies within a topic (G-merge), now across topics.
const DefaultTidyTheta = 0.55

// TidyRejection is one refused fold with the A2 verdict that refused it.
type TidyRejection struct {
	Winner string `json:"winner"`
	Loser  string `json:"loser"`
	Reason string `json:"reason"`
}

// TidyPair records one fold (or, in dry-run, one fold that would happen).
type TidyPair struct {
	Winner string  `json:"winner"`       // surviving cluster (older CreatedAt wins)
	Loser  string  `json:"loser"`        // folded cluster, deleted unless dry-run
	Sim    float64 `json:"sim"`          // pure cosine that cleared theta
	Co     float64 `json:"co,omitempty"` // co_occur profile weight (A4; ranking only)
}

// TidyReport summarizes one sweep. Lifecycle counts are the population
// snapshot so operators see drift (emerging/contested/deprecated) even when
// nothing merges. Delta is the before/after snapshot (ir-rag 2.6): a sweep
// whose only effect is fewer clusters is NOT evidence of improvement — the
// delta makes the effect visible and falsifiable. It is never computed over
// evaluation items (论文纪律：不许用评测题选演进停止点).
type TidyReport struct {
	Scanned           int `json:"scanned"`
	Merged            int `json:"merged"`
	Rejected          int `json:"rejected"` // AcceptFold refused (A2)
	SkippedContested  int `json:"skipped_contested"`
	SkippedDeprecated int `json:"skipped_deprecated"`
	// Rejections records WHY each refused fold was refused (A2 verdict), so the
	// sweep's refusals are auditable instead of a bare counter.
	Rejections []TidyRejection `json:"rejections,omitempty"`
	Lifecycle  map[string]int  `json:"lifecycle"`
	Pairs      []TidyPair      `json:"pairs,omitempty"`
	DryRun     bool            `json:"dry_run"`
	Delta      *TidyDelta      `json:"delta,omitempty"`
}

// TidyDelta is the before/after population snapshot of one sweep.
// FoldedClusters = clusters - (merged + deleted-away losers); evidence and
// query counts are sums over the LIVE population, so a fold that loses
// evidence windows (bad) shows up as negative EvidenceDelta.
type TidyDelta struct {
	ClustersBefore int `json:"clusters_before"`
	ClustersAfter  int `json:"clusters_after"`
	QueriesBefore  int `json:"queries_before"`
	QueriesAfter   int `json:"queries_after"`
	EvidenceBefore int `json:"evidence_before"`
	EvidenceAfter  int `json:"evidence_after"`
	// Confidence/Hotness means over the live population (0 decimals).
	ConfidenceBefore float64 `json:"confidence_before"`
	ConfidenceAfter  float64 `json:"confidence_after"`
	HotnessBefore    float64 `json:"hotness_before"`
	HotnessAfter     float64 `json:"hotness_after"`
	// Projected marks a dry-run delta: the numbers describe the fold that
	// WOULD happen, not one that did.
	Projected bool `json:"projected,omitempty"`
}

// snapshotStats aggregates one population snapshot.
func snapshotStats(cs []Cluster) (clusters, queries, evidence int, confMean, hotMean float64) {
	clusters = len(cs)
	for _, c := range cs {
		queries += len(c.Queries)
		evidence += len(c.Evidence)
		confMean += c.Confidence
		hotMean += c.Hotness
	}
	if clusters > 0 {
		confMean /= float64(clusters)
		hotMean /= float64(clusters)
	}
	return
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
// Tidy is the compatibility wrapper (no co_occur preference).
func Tidy(ctx context.Context, st Store, emb Embedder, theta float64, dryRun bool, maxMerges int) (TidyReport, error) {
	return TidyWithCo(ctx, st, emb, theta, dryRun, maxMerges, nil)
}

// TidyWithCo is Tidy plus an optional co_occur weight hook (ir-rag A4):
// pairs with a co-retrieval profile rank above bare cosine ties so diagnosis
// stays comparative, not single-document.
func TidyWithCo(ctx context.Context, st Store, emb Embedder, theta float64, dryRun bool, maxMerges int, coWeight func(a, b string) float64) (TidyReport, error) {
	rep := TidyReport{Lifecycle: map[string]int{}, DryRun: dryRun}
	if theta <= 0 {
		theta = DefaultTidyTheta
	}
	all, err := st.All(ctx)
	if err != nil {
		return rep, err
	}
	rep.Scanned = len(all)
	qBefore, evBefore := 0, 0
	confBefore, hotBefore := 0.0, 0.0
	for _, c := range all {
		qBefore += len(c.Queries)
		evBefore += len(c.Evidence)
		confBefore += c.Confidence
		hotBefore += c.Hotness
	}
	if len(all) > 0 {
		confBefore /= float64(len(all))
		hotBefore /= float64(len(all))
	}
	before := TidyDelta{
		ClustersBefore: len(all), QueriesBefore: qBefore, EvidenceBefore: evBefore,
		ConfidenceBefore: round3(confBefore), HotnessBefore: round3(hotBefore),
	}

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
	rejectedPair := map[string]bool{}

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
				if rejectedPair[pairKey(work[i].ID, work[j].ID)] {
					continue
				}
				sim := Cosine(work[i].Embed, work[j].Embed)
				if sim < theta {
					continue
				}
				w, l := orderByAge(work[i], work[j])
				cand := &TidyPair{Winner: w.ID, Loser: l.ID, Sim: sim}
				if coWeight != nil {
					if cw := coWeight(w.ID, l.ID); cw > 0 {
						// Profile-backed pairs win cosine ties and near-ties.
						cand.Co = cw
					}
				}
				if best == nil || betterPair(*cand, *best) {
					best, bestWI, bestLI = cand, i, j
				}
			}
		}
		if best == nil {
			break
		}
		winner, loser := orderByAge(work[bestWI], work[bestLI])
		// A2: unvalidated evolution is net-negative — refuse the fold.
		if ok, why := AcceptFold(winner, loser, work, 3); !ok {
			// The reason used to be discarded, so a refused fold was invisible
			// in the report the operator reads.
			rejectedPair[pairKey(best.Winner, best.Loser)] = true
			rep.Rejected++
			rep.Rejections = append(rep.Rejections, TidyRejection{
				Winner: winner.ID, Loser: loser.ID, Reason: why,
			})
			continue
		}
		rep.Pairs = append(rep.Pairs, *best)
		if dryRun {
			// 2.6: a dry-run delta that reads "nothing changed" is useless.
			// Project the exemplar fold into a scratch copy (never saved).
			win, lose := orderByAge(work[bestWI], work[bestLI])
			foldInto(&win, &lose)
			projected := make([]Cluster, 0, len(work))
			for _, c := range work {
				if !alive[c.ID] || c.ID == lose.ID {
					continue
				}
				if c.ID == win.ID {
					continue
				}
				projected = append(projected, c)
			}
			projected = append(projected, win)
			rep.Delta = deltaFrom(before, projected, true)
			break // one exemplar pair is enough to show what would happen
		}
		foldInto(&winner, &loser)
		if emb != nil {
			if qe, eerr := QuerySetEmbed(ctx, emb, winner.Queries); eerr == nil {
				winner.Embed = qe
			}
			// 2.5: segment embeddings follow the merged key set.
			if texts := winner.LevelKeyTexts(); len(texts) > 0 {
				if vs, verr := emb.Embed(ctx, texts); verr == nil && len(vs) == len(texts) {
					winner.AttachKeyEmbeds(vs)
				}
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
	// 2.6: before/after snapshot over the LIVE population. A dry-run already
	// set a PROJECTED delta — never overwrite it with the no-op truth.
	if rep.Delta == nil {
		afterAll := make([]Cluster, 0, len(work))
		for _, c := range work {
			if alive[c.ID] {
				afterAll = append(afterAll, c)
			}
		}
		rep.Delta = deltaFrom(before, afterAll, false)
	}
	return rep, nil
}

// deltaFrom fills the "after" half of a delta from a population snapshot.
func deltaFrom(b TidyDelta, after []Cluster, projected bool) *TidyDelta {
	c, q, ev, conf, hot := snapshotStats(after)
	b.ClustersAfter, b.QueriesAfter, b.EvidenceAfter = c, q, ev
	b.ConfidenceAfter, b.HotnessAfter = round3(conf), round3(hot)
	b.Projected = projected
	return &b
}

// round3 keeps the JSON scoreboard readable without pulling in math.
func round3(x float64) float64 { return float64(int(x*1000+0.5)) / 1000 }

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// betterPair orders candidates: higher (cosine + co bonus) first, then pure
// sim, then smaller winner id, then smaller loser id.
func betterPair(a, b TidyPair) bool {
	rank := func(p TidyPair) (float64, float64, string, string) {
		return p.Sim + 0.05*p.Co, p.Sim, p.Winner, p.Loser
	}
	ar, as, aw, al := rank(a)
	br, bs, bw, bl := rank(b)
	if ar != br {
		return ar > br
	}
	if as != bs {
		return as > bs
	}
	if aw != bw {
		return aw < bw
	}
	return al < bl
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

	// Multi-level keys: survivor keeps its own; loser's derived keys are
	// additive (max-over-keys only benefits from a larger set). Identity
	// TopicKey of the loser is already folded into TopicKeys above.
	for _, lk := range loser.LevelKeys {
		winner.addLevelKey(lk.Level, lk.Text)
	}
	winner.KeyEmbeds = nil // recomputed by the embedder-owning caller

	// Legacy samples carry sampling methods, not document IDs. Normalize both
	// sides before comparing spans, without mutating either input evidence slice.
	// The evidence merge comes first: the appended summary's [n] markers have to
	// be renumbered against the merged list, and a window the survivor already
	// carries must map back to that existing copy rather than to a duplicate.
	winner.Evidence = NormalizeEvidence(winner.SourceID, winner.Evidence)
	loserEvidence := NormalizeEvidence(loser.SourceID, loser.Evidence)
	index := make(map[string]int, len(winner.Evidence))
	for i, ev := range winner.Evidence {
		index[evidenceKey(ev)] = i + 1
	}
	remap := make(map[int]int, len(loserEvidence))
	for k, ev := range loserEvidence {
		key := evidenceKey(ev)
		if pos, dup := index[key]; dup {
			remap[k+1] = pos
			continue
		}
		winner.Evidence = append(winner.Evidence, ev)
		index[key] = len(winner.Evidence)
		remap[k+1] = len(winner.Evidence)
	}
	winner.Content = winner.Content + "\n---\n" + RenumberCitations(loser.Content, remap)
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

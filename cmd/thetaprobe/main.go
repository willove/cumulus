// Command thetaprobe measures where the L2 reuse line sits for the currently
// configured embedder (ir-rag / MeanCache-SCALM threshold search).
//
// Labels are PROVENANCE, never a golden set: each seed query's text comes
// from a known statute article (expect_key); the correct reuse target is the
// cluster anchored on that article. The probe never sees judge verdicts or
// eval items, and it does NOT pick θ — it prints score distributions and a
// grid so an operator can decide.
//
// Queries that a cluster already retains verbatim are skipped (self-match is
// a tautology, not evidence).
//
// Usage:
//
//	ASK_EMBED=minilm thetaprobe -data DIR -seeds seeds.jsonl [-ns default]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/minilm"
	"github.com/cumubase/ask/internal/ns"
	"github.com/willove/cumulite"
)

type seed struct {
	Query     string   `json:"query"`
	ExpectKey string   `json:"expect_key"`
	Gold      []string `json:"gold_sources"` // anchorgen shape: expect = gold[0]
}

type row struct {
	query    string
	expect   string
	cluster  string // argmax cluster id
	anchor   string // that cluster's source business_key
	correct  bool
	embedCos float64
	maxSim   float64
	keyRel   float64
	fused    float64
}

func main() {
	lite := flag.String("lite", "", "cumulite store directory")
	namespace := flag.String("ns", "", "namespace (empty = default library)")
	seedsFile := flag.String("seeds", "", "seeds jsonl: {query, expect_key}")
	flag.Parse()
	if *seedsFile == "" {
		fmt.Fprintln(os.Stderr, "thetaprobe: -seeds required")
		os.Exit(2)
	}

	// Same embedder table as the search face.
	var embedFn func(context.Context, []string) ([][]float64, error)
	embedder := "local-hash-64"
	if os.Getenv("ASK_EMBED") == "minilm" && minilm.Available() {
		emb := minilm.New(minilm.DefaultDir())
		embedFn = emb.Embed
		embedder = "minilm-l12-384"
	} else {
		loc := cluster.Local{N: 64}
		embedFn = loc.Embed
	}

	c, err := cumulite.Open(*lite)
	if err != nil {
		fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	st := cluster.NewCumuStore(c, ns.Coll(*namespace, "ask_clusters"))
	clusters, err := st.All(ctx)
	if err != nil {
		fatal(err)
	}
	if len(clusters) < 2 {
		fmt.Fprintf(os.Stderr, "thetaprobe: need ≥2 clusters, got %d\n", len(clusters))
		os.Exit(1)
	}
	sourcesColl := ns.Coll(*namespace, "ask_sources")

	// Skip queries any cluster already retains verbatim (tautological match).
	retained := map[string]bool{}
	// Anchor index: business_key → cluster ids. A seed is only evaluable when
	// its expect_key actually HAS a cluster (otherwise we would be measuring
	// coverage, not reuse ranking).
	anchors := map[string][]string{}
	for i := range clusters {
		for _, q := range clusters[i].Queries {
			retained[trim(q)] = true
		}
		if k := businessKeyOf(ctx, c, sourcesColl, clusters[i].SourceID); k != "" {
			anchors[k] = append(anchors[k], clusters[i].ID)
		}
	}

	seeds, err := readSeeds(*seedsFile)
	if err != nil {
		fatal(err)
	}
	var rows []row
	skippedNoAnchor := 0
	for _, s := range seeds {
		q := trim(s.Query)
		expect := s.ExpectKey
		if expect == "" && len(s.Gold) > 0 {
			expect = s.Gold[0]
		}
		if q == "" || retained[q] || expect == "" {
			continue
		}
		if len(anchors[expect]) == 0 {
			skippedNoAnchor++
			continue
		}
		qv, err := embedFn(ctx, []string{q})
		if err != nil || len(qv) != 1 {
			continue
		}
		best := -1
		bestScore := -2.0
		for i := range clusters {
			sc := cluster.ReuseScore(&clusters[i], q, qv[0])
			if sc > bestScore {
				bestScore = sc
				best = i
			}
		}
		if best < 0 {
			continue
		}
		tgt := &clusters[best]
		anchor := businessKeyOf(ctx, c, sourcesColl, tgt.SourceID)
		rows = append(rows, row{
			query: q, expect: expect, cluster: tgt.ID, anchor: anchor,
			correct:  anchor == expect,
			embedCos: cluster.Cosine(tgt.Embed, qv[0]),
			maxSim:   tgt.MaxKeySim(qv[0]),
			keyRel:   cluster.MaxKeyRel(q, tgt),
			fused:    bestScore,
		})
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "thetaprobe: no evaluable seeds (all retained verbatim?)")
		os.Exit(1)
	}

	correct := filterRows(rows, true)
	wrong := filterRows(rows, false)
	out := map[string]any{
		"embedder":          embedder,
		"clusters":          len(clusters),
		"distinct_anchors":  len(anchors),
		"skipped_no_anchor": skippedNoAnchor,
		"seeds":             len(rows),
		"argmax_correct": map[string]any{
			"n": len(correct), "total": len(rows),
			"rate": round3(float64(len(correct)) / float64(len(rows))),
		},
		"score_when_correct": quantiles(col(correct, func(r row) float64 { return r.fused })),
		"score_when_wrong":   quantiles(col(wrong, func(r row) float64 { return r.fused })),
		"arms_correct": map[string]any{
			"embed_cosine": quantiles(col(correct, func(r row) float64 { return r.embedCos })),
			"max_key_sim":  quantiles(col(correct, func(r row) float64 { return r.maxSim })),
			"max_key_rel":  quantiles(col(correct, func(r row) float64 { return r.keyRel })),
		},
		"grid":   grid(rows),
		"misses": misses(rows, 6),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func readSeeds(path string) ([]seed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []seed
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s seed
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("seed line: %w", err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

func businessKeyOf(ctx context.Context, c cumulite.Port, sourcesColl, id string) string {
	if id == "" {
		return ""
	}
	d, err := c.GetDocument(ctx, sourcesColl, id)
	if err != nil || d == nil {
		return ""
	}
	k, _ := d["business_key"].(string)
	return k
}

func col(rows []row, f func(row) float64) []float64 {
	out := make([]float64, 0, len(rows))
	for _, r := range rows {
		out = append(out, f(r))
	}
	return out
}

func filterRows(rows []row, want bool) []row {
	var out []row
	for _, r := range rows {
		if r.correct == want {
			out = append(out, r)
		}
	}
	return out
}

func quantiles(xs []float64) map[string]float64 {
	if len(xs) == 0 {
		return map[string]float64{}
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	q := func(p float64) float64 { return cp[int(p*float64(len(cp)-1))] }
	return map[string]float64{"min": cp[0], "p50": q(0.5), "p90": q(0.9), "max": cp[len(cp)-1]}
}

// grid reports per-θ behaviour of "reuse the argmax cluster when fused ≥ θ":
// covered = seeds where the argmax was correct, and hits = seeds whose score
// clears θ. Precision = correct∧hit / hit (G-pollute view).
func grid(rows []row) []map[string]any {
	var out []map[string]any
	for _, th := range []float64{0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90} {
		var hit, hitCorrect int
		for _, r := range rows {
			if r.fused >= th {
				hit++
				if r.correct {
					hitCorrect++
				}
			}
		}
		prec, cov := 0.0, 0.0
		if hit > 0 {
			prec = float64(hitCorrect) / float64(hit)
		}
		covered := len(filterRows(rows, true))
		if covered > 0 {
			cov = float64(hitCorrect) / float64(covered)
		}
		out = append(out, map[string]any{
			"theta": th, "hits": hit, "correct_hits": hitCorrect,
			"precision": round3(prec), "recall_on_correct": round3(cov),
		})
	}
	return out
}

// misses lists a few wrong-argmax rows for qualitative reading.
func misses(rows []row, n int) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r.correct || len(out) >= n {
			continue
		}
		out = append(out, map[string]any{
			"query_head": runeHead(r.query, 40), "expect": r.expect, "got_anchor": r.anchor,
			"fused": round3(r.fused), "embed_cos": round3(r.embedCos), "max_key_sim": round3(r.maxSim),
		})
	}
	return out
}

func runeHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func round3(x float64) float64 { return float64(int(x*1000+0.5)) / 1000 }

func trim(s string) string { return strings.TrimSpace(s) }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "thetaprobe:", err)
	os.Exit(1)
}

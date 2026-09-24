// Command knnprobe embeds queries with the suite's configured embedder and
// prints the KNN ranking over a sources collection (title + distance). Dev
// diagnostic for the L1 prefilter; not part of the gates.
// Usage:
//
//	ASK_EMBED=minilm knnprobe -data DIR -q "查询" [-k 40]
//	ASK_EMBED=minilm knnprobe -data DIR -items items.jsonl -out ranks.json [-k 8]
//
// The -items mode writes {"id":…,"rank":…} (0 = gold not in top-K) per item.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/minilm"
	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

type item struct {
	ID    string   `json:"id"`
	Query string   `json:"query"`
	Gold  []string `json:"gold_sources"`
}

type rankOut struct {
	ID   string `json:"id"`
	Rank int    `json:"rank"` // 1-based; 0 = gold absent from top-K
	Dist float64
}

func main() {
	lite := flag.String("lite", "", "cumulite store directory")
	coll := flag.String("coll", "ask_sources", "sources collection")
	query := flag.String("q", "", "query")
	itemsFile := flag.String("items", "", "items jsonl: rank gold per item instead of printing one ranking")
	outFile := flag.String("out", "", "output jsonl for -items mode")
	k := flag.Int("k", 40, "neighbors")
	field := flag.String("field", "body_embed", "vector field")
	flag.Parse()
	if *query == "" && *itemsFile == "" {
		fmt.Fprintln(os.Stderr, "knnprobe: -q or -items required")
		os.Exit(2)
	}
	var embedFn func(ctx context.Context, texts []string) ([][]float64, error)
	if os.Getenv("ASK_EMBED") == "minilm" && minilm.Available() {
		emb := minilm.New(minilm.DefaultDir())
		embedFn = emb.Embed
		fmt.Fprintln(os.Stderr, "embedder: minilm-384")
	} else {
		loc := cluster.Local{N: 64}
		embedFn = loc.Embed
		fmt.Fprintln(os.Stderr, "embedder: local-hash-64")
	}
	if *lite == "" {
		fmt.Fprintln(os.Stderr, "knnprobe: -data DIR required")
		os.Exit(2)
	}
	engine, err := cumulite.Open(*lite)
	if err != nil {
		fmt.Fprintln(os.Stderr, "knnprobe:", err)
		os.Exit(1)
	}
	defer engine.Close()
	var c cumulite.Port = engine

	rankOf := func(q, gold string) (int, float64, error) {
		qv, err := embedFn(context.Background(), []string{q})
		if err != nil || len(qv) != 1 {
			return 0, 0, err
		}
		res, err := c.KNN(context.Background(), *coll, contract.KNNRequest{
			Field: *field, Vector: qv[0], K: *k, Metric: "cosine",
		})
		if err != nil {
			return 0, 0, err
		}
		for i, d := range res.Documents {
			title, _ := d["title"].(string)
			bkey, _ := d["business_key"].(string)
			if title == gold || bkey == gold {
				dist := 0.0
				if i < len(res.Distances) {
					dist = res.Distances[i]
				}
				return i + 1, dist, nil
			}
		}
		return 0, 0, nil
	}

	if *itemsFile != "" {
		raw, err := os.ReadFile(*itemsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "items:", err)
			os.Exit(1)
		}
		var out []rankOut
		for _, line := range splitLines(raw) {
			var it item
			if err := json.Unmarshal(line, &it); err != nil || it.ID == "" {
				continue
			}
			gold := ""
			if len(it.Gold) > 0 {
				gold = it.Gold[0]
			}
			rank, dist, err := rankOf(it.Query, gold)
			if err != nil {
				fmt.Fprintln(os.Stderr, "knn:", err)
				os.Exit(1)
			}
			out = append(out, rankOut{ID: it.ID, Rank: rank, Dist: dist})
			fmt.Printf("%s rank=%d dist=%.4f\n", it.ID, rank, dist)
		}
		if *outFile != "" {
			b, _ := json.MarshalIndent(out, "", "  ")
			if err := os.WriteFile(*outFile, b, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "out:", err)
				os.Exit(1)
			}
		}
		return
	}

	rank, dist, err := rankOf(*query, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "knn:", err)
		os.Exit(1)
	}
	_ = rank
	res, err := knnList(c, *coll, *field, embedFn, *query, *k)
	if err != nil {
		fmt.Fprintln(os.Stderr, "knn:", err)
		os.Exit(1)
	}
	for i, d := range res.Documents {
		title, _ := d["title"].(string)
		id, _ := d["_id"].(string)
		dist := 0.0
		if i < len(res.Distances) {
			dist = res.Distances[i]
		}
		fmt.Printf("%2d. %-10s %-14s dist=%.4f\n", i+1, title, id[:min(12, len(id))], dist)
	}
	_ = dist
}

func knnList(c cumulite.Port, coll, field string, embedFn func(ctx context.Context, texts []string) ([][]float64, error), q string, k int) (*contract.KNNResult, error) {
	qv, err := embedFn(context.Background(), []string{q})
	if err != nil || len(qv) != 1 {
		return nil, err
	}
	return c.KNN(context.Background(), coll, contract.KNNRequest{
		Field: field, Vector: qv[0], K: k, Metric: "cosine",
	})
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				lines = append(lines, raw[start:i])
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

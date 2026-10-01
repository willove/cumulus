// Command agreeprobe discriminates the two candidate causes behind the
// consistency gate's zero conversions on the adversarial set (2026-09-30):
//
//	window-miss  — the divergence signal ("作者：X" at the document head) sat
//	               OUTSIDE the sampled windows, so the checker correctly
//	               judged two poem bodies as "same topic, different matters".
//	model-blind  — even with both author lines in view the model calls the
//	               pair consistent (the same blindness as v3b's conflicts_with).
//
// For each adversarial item it builds TWO variants of the checker input from
// the SAME document pair and calls the live endpoint once per variant:
//
//	body — content past the "。全文：" head mark (what a mid-poem sampled
//	       window looks like; expected agree=true under window-miss)
//	head — the document's first runes, title + author line included
//	       (expected agree=false iff the model can see the divergence)
//
// Record-only like every endpoint-tier probe: prints a table, writes nothing.
//
// Usage: agreeprobe [-set var/poetry-adversarial] [-limit 8]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/willove/cumulus/internal/envcfg"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/mcs"
)

type item struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	GoldSources []string `json:"gold_sources"`
}

type corpusDoc struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

func main() {
	setPath := flag.String("set", "var/poetry-adversarial", "frozen set dir (items.jsonl + corpus.jsonl)")
	limit := flag.Int("limit", 8, "how many pairs to probe")
	flag.Parse()

	if err := envcfg.Resolve(); err != nil {
		fmt.Fprintln(os.Stderr, "agreeprobe:", err)
		os.Exit(1)
	}
	if envcfg.OfflineForced() {
		fmt.Fprintln(os.Stderr, "agreeprobe: CLUS_OFFLINE=1 pins offline stubs — refusing to run (needs a live model)")
		os.Exit(1)
	}
	base := os.Getenv("LLM_BASE_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "agreeprobe: LLM_BASE_URL absent — needs a live model")
		os.Exit(1)
	}

	items, err := readJSONL[item](*setPath + "/items.jsonl")
	if err != nil {
		fatal(err)
	}
	docs, err := readJSONL[corpusDoc](*setPath + "/corpus.jsonl")
	if err != nil {
		fatal(err)
	}
	byKey := map[string]corpusDoc{}
	for _, d := range docs {
		byKey[d.Key] = d
	}
	if *limit > 0 && *limit < len(items) {
		items = items[:*limit]
	}

	chat := &llm.ChatClient{
		BaseURL: base,
		APIKey:  os.Getenv("LLM_API_KEY"),
		Model:   envOr("LLM_CHAT_MODEL", "MiniMax-M3"),
	}
	cons := &llm.Consistency{Client: chat}

	take := func(s string, n int) string {
		r := []rune(s)
		if len(r) > n {
			return string(r[:n])
		}
		return s
	}
	bodyTally, headTally := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fmt.Printf("%-6s %-10s %-6s %s\n", "id", "variant", "agree", "conflict_summary")
	for _, it := range items {
		gk := it.GoldSources[0]
		dk := "dist" + gk[4:]
		gold, dist := byKey[gk], byKey[dk]
		// body: past the self-describing head mark, like a mid-poem window.
		gb := partitionAfter(gold.Text)
		db := partitionAfter(dist.Text)
		// head: the document's first runes, author line included.
		gh := take(gold.Text, 300)
		dh := take(dist.Text, 300)
		for _, v := range []struct {
			name string
			wins []mcs.Sample
		}{
			{"body", []mcs.Sample{{Source: gk, Score: 8, Start: 0, End: 300, Content: gb},
				{Source: dk, Score: 8, Start: 0, End: 300, Content: db}}},
			{"head", []mcs.Sample{{Source: gk, Score: 8, Start: 0, End: 300, Content: gh},
				{Source: dk, Score: 8, Start: 0, End: 300, Content: dh}}},
		} {
			agree, why, err := cons.CheckConsistency(ctx, it.Query, v.wins)
			if err != nil {
				fmt.Printf("%-6s %-10s ERROR %v\n", it.ID, v.name, err)
				continue
			}
			mark := ""
			if !agree {
				mark = "◀"
				if v.name == "head" {
					headTally++
				} else {
					bodyTally++
				}
			}
			fmt.Printf("%-6s %-10s %-6v %s %s\n", it.ID, v.name, agree, mark, take(why, 60))
		}
	}
	fmt.Printf("\ntally: body-flagged %d/%d, head-flagged %d/%d\n",
		bodyTally, len(items), headTally, len(items))
	fmt.Println("reading: head-flagged high + body-flagged ~0 ⇒ WINDOW-MISS (fix gate windows);")
	fmt.Println("         head-flagged ~0 ⇒ MODEL-BLIND (same as v3b, archive the layer).")
}

// partitionAfter returns the text past the first "。全文：" head mark, or the
// tail half when the mark is missing (never panics on odd docs).
func partitionAfter(s string) string {
	const mark = "。全文："
	i := indexOf(s, mark)
	if i < 0 {
		return s
	}
	r := []rune(s)
	// byte offset → rune slice from there; approximate by scanning
	acc := 0
	for j := range r {
		if acc == i+len(mark) {
			return string(r[j:])
		}
		acc += len(string(r[j]))
	}
	return s
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "agreeprobe:", err)
	os.Exit(1)
}

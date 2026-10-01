// Command anchorgen rewrites sampled corpus articles into colloquial user
// questions with the configured chat endpoint, producing an eval items JSONL
// (id/query/answer/gold_sources) for knnprobe -items.
// Corpus rows come from stdin (JSONL: key/title/text), anchors go to stdout.
//
//	ANCHOR_KEY=<key> anchorgen -n 40 < corpus.jsonl > items.jsonl
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/llm"
)

type row struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type item struct {
	ID     string   `json:"id"`
	Query  string   `json:"query"`
	Answer string   `json:"answer"`
	Gold   []string `json:"gold_sources"`
}

const prompt = "把下面这条法律条文改写成一个普通人会问出的口语化问题。\n" +
	"要求：问题里不得出现法律名称、条文编号；不要复述条文原文；只问这件事本身。\n" +
	"只输出 JSON：{\"q\": \"问题\"}\n\n条文：%s"

// envOr mirrors main.envOr: an explicit environment value wins, else the
// default. Local because this is a separate command package.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	n := flag.Int("n", 40, "sample size (deterministic stride)")
	flag.Parse()
	key := os.Getenv("ANCHOR_KEY")
	if key == "" {
		fatal("ANCHOR_KEY not set")
	}
	// Same endpoint convention as the CLI (LLM_* via env.go); a
	// hardcoded URL here meant the tool silently ignored -env switching.
	base := envOr("LLM_BASE_URL", "https://api.minimaxi.com/v1")
	chat := &llm.ChatClient{
		BaseURL:        base,
		APIKey:         key,
		Model:          envOr("LLM_CHAT_MODEL", "MiniMax-M3"),
		Caller:         "anchorgen",
		ReasoningSplit: strings.Contains(strings.ToLower(base), "minimaxi.com"),
	}

	var rows []row
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			fatal("corpus line: %v", err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		fatal("stdin: %v", err)
	}
	step := len(rows) / *n
	if step < 1 {
		step = 1
	}
	sample := []row{}
	for i := 0; i < len(rows) && len(sample) < *n; i += step {
		sample = append(sample, rows[i])
	}

	ctx := context.Background()
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	ok := 0
	for _, r := range sample {
		raw, err := chat.Complete(ctx, fmt.Sprintf(prompt, r.Text))
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", r.Key, err)
			continue
		}
		clean, _ := llm.SplitThink(raw)
		q, err := extractQ(clean)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", r.Key, err)
			continue
		}
		it := item{ID: r.Key, Query: q, Answer: r.Text, Gold: []string{r.Key}}
		b, _ := json.Marshal(it)
		fmt.Fprintln(w, string(b))
		fmt.Fprintf(os.Stderr, "%s | %s\n", r.Key, q)
		ok++
		time.Sleep(200 * time.Millisecond) // be gentle with the endpoint
	}
	fmt.Fprintf(os.Stderr, "anchors: %d/%d\n", ok, len(sample))
}

func extractQ(clean string) (string, error) {
	start := strings.Index(clean, "{")
	end := strings.LastIndex(clean, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("no json object in answer")
	}
	var out struct {
		Q string `json:"q"`
	}
	if err := json.Unmarshal([]byte(clean[start:end+1]), &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Q) == "" {
		return "", fmt.Errorf("empty q")
	}
	return strings.TrimSpace(out.Q), nil
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "anchorgen: "+f+"\n", a...)
	os.Exit(1)
}

// Command scoreprobe measures how RELIABLE the evidence scorer is, which is
// the precondition for reading any A/B that moves the retrieval outcome
// (perf-plan §4 P1-1).
//
// Why this exists: the archived runs show the LLM judge flipping 10/30
// verdicts on CLOSED-BOOK items — a task with no retrieval at all. A scorer
// that unstable at that level cannot adjudicate a mechanism change, and the
// DEEP loop makes hard decisions directly on the scorer's output:
//
//	Score >= 4      keeps a window           (deep.go)
//	bestScore >= 8      ends the loop         (deep.go)
//	bestScore < 6       triggers widening     (deep.go)
//
// So the decisive quantity is not "is the mean score right" but "would the
// same window have been kept or dropped again on a re-score".
//
// No labels are used and none are needed: this is pure re-measurement of the
// same input. It reports, per window:
//
//	repeats, mean, stddev, min, max, spread
//	decision_stable   — every repeat landed on the same side of the nearest
//	                    DEEP threshold (a keep/drop coin flip if false)
//	borderline        — within 1.0 of that threshold, or unstable
//
// and per item:
//
//	top_gold_stable   — the gold document won the argmax on every repeat
//	top_tie_count     — non-winner documents within `tieBand` of the winner,
//	                    i.e. how many re-scores could plausibly change it
//
// Like every other endpoint-tier probe in this repo it is RECORD-ONLY: it
// prints a report and never writes back to config, thresholds, or the store
// (D6 / design-plan §6.1). It needs a live model and refuses to run without.
//
// Every (item, document, repeat) triple is scored exactly once and all
// statistics are derived from that single matrix.
//
// Usage:
//
//	scoreprobe -items var/realeval/items.jsonl -corpus var/realeval/corpus.jsonl \
//	           -repeats 5 [-thinking-off] [-detail] [-json out.json]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/envcfg"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/mcs"
)

// keepThreshold mirrors the DEEP loop's window-keep line; stopThreshold is the
// early-stop line. The probe reports stability against whichever is nearer,
// because that is the one actually acting on a given score.
const (
	keepThreshold = 4.0
	stopThreshold = 8.0
	tieBand       = 1.0
)

type item struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	Answer      string   `json:"answer"`
	GoldSources []string `json:"gold_sources"`
}

type corpusDoc struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type windowReport struct {
	Item           string    `json:"item"`
	Source         string    `json:"source"`
	Gold           bool      `json:"gold"`
	Start          int       `json:"start"`
	Repeats        int       `json:"repeats"`
	Mean           float64   `json:"mean"`
	Stddev         float64   `json:"stddev"`
	Min            float64   `json:"min"`
	Max            float64   `json:"max"`
	Spread         float64   `json:"spread"`
	Scores         []float64 `json:"scores"`
	DecisionStable bool      `json:"decision_stable"`
	Borderline     bool      `json:"borderline"`
	NearestThresh  float64   `json:"nearest_threshold"`
}

type itemReport struct {
	Item          string `json:"item"`
	GoldSources   string `json:"gold_sources"`
	Documents     int    `json:"documents"`
	GoldDocuments int    `json:"gold_documents"`
	TopGoldStable bool   `json:"top_gold_stable"`
	TopTieCount   int    `json:"top_tie_count"`
}

type report struct {
	GeneratedAt string `json:"generated_at"`
	Model       string `json:"model"`
	Thinking    string `json:"thinking"`
	Repeats     int    `json:"repeats"`
	Items       int    `json:"items"`
	Windows     int    `json:"windows"`
	GoldWindows int    `json:"gold_windows"`

	DecisionStableRate float64 `json:"decision_stable_rate"`
	BorderlineRate     float64 `json:"borderline_rate"`
	GoldBorderlineRate float64 `json:"gold_borderline_rate"`
	MeanStddev         float64 `json:"mean_stddev"`
	MeanGoldScore      float64 `json:"mean_gold_score"`
	MeanNonGoldScore   float64 `json:"mean_non_gold_score"`
	ScoreGap           float64 `json:"score_gap"`
	TopGoldStableRate  float64 `json:"top_gold_stable_rate"`
	MeanTopTieCount    float64 `json:"mean_top_tie_count"`

	WindowsDetail []windowReport `json:"windows_detail,omitempty"`
	ItemsDetail   []itemReport   `json:"items_detail,omitempty"`
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var s float64
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

func nearestThreshold(v float64) float64 {
	if math.Abs(v-keepThreshold) <= math.Abs(v-stopThreshold) {
		return keepThreshold
	}
	return stopThreshold
}

func allSameSide(scores []float64, th float64) bool {
	pos := scores[0] >= th
	for _, s := range scores[1:] {
		if (s >= th) != pos {
			return false
		}
	}
	return true
}

func main() {
	var (
		itemsPath  = flag.String("items", "var/realeval/items.jsonl", "item set (jsonl)")
		corpusPath = flag.String("corpus", "var/realeval/corpus.jsonl", "corpus (jsonl)")
		repeats    = flag.Int("repeats", 5, "how many times to score each window")
		negatives  = flag.Int("negatives", 4, "hard negatives per item (0 = every other document)")
		thinkOff   = flag.Bool("thinking-off", false, "use CompleteStructured (no private thinking)")
		limit      = flag.Int("limit", 0, "only the first N items (0 = all)")
		detail     = flag.Bool("detail", false, "include per-window / per-item detail in the JSON output")
		outPath    = flag.String("json", "", "write the JSON report here (default: summary only)")
	)
	flag.Parse()

	// Same endpoint resolution as the search stack (internal/envcfg): the
	// suite .env plus the LLM_* → AIGATE_* alias. A reliability number
	// gathered against a different model is not a number about this suite.
	if err := envcfg.Resolve(); err != nil {
		fmt.Fprintln(os.Stderr, "scoreprobe:", err)
		os.Exit(1)
	}
	if envcfg.OfflineForced() {
		fmt.Fprintln(os.Stderr, "scoreprobe: CLUS_OFFLINE=1 pins the offline stubs — refusing to run (this probe needs a live model)")
		os.Exit(1)
	}

	if *repeats < 2 {
		fmt.Fprintln(os.Stderr, "scoreprobe: -repeats must be >= 2 to measure anything")
		os.Exit(1)
	}
	base := os.Getenv("AIGATE_BASE_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "scoreprobe: AIGATE_BASE_URL absent (and no LLM_BASE_URL in the suite .env) — needs a live model (record-only, never gates)")
		os.Exit(1)
	}

	items, err := readJSONL[item](*itemsPath)
	if err != nil {
		fatal(err)
	}
	docs, err := readJSONL[corpusDoc](*corpusPath)
	if err != nil {
		fatal(err)
	}
	if *limit > 0 && *limit < len(items) {
		items = items[:*limit]
	}
	byKey := map[string]corpusDoc{}
	for _, d := range docs {
		byKey[d.Key] = d
	}

	chat := &llm.ChatClient{
		BaseURL: base,
		APIKey:  os.Getenv("AIGATE_API_KEY"),
		Model:   envOr("AIGATE_CHAT_MODEL", "mimo/cascade-pro"),
		Caller:  "scoreprobe",
	}
	sc := &llm.AigateScorer{Client: chat, NoThink: *thinkOff}

	ctx := context.Background()
	// KeywordScorer is intentional: we only want its WINDOWS, never its
	// scores. The probe measures the LLM scorer against fixed inputs.
	sampler := mcs.New(mcs.EnvConfig(), mcs.KeywordScorer{})

	rep := report{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Model:       chat.Model,
		Thinking:    "on",
		Repeats:     *repeats,
		Items:       len(items),
	}
	if *thinkOff {
		rep.Thinking = "off"
	}

	var stableCount, borderlineCount, goldBorderline int
	var goldSum, nonGoldSum, stddevSum float64
	var goldN, nonGoldN, topStable, topTieSum int
	scoredItems := 0

	for _, it := range items {
		gold := map[string]bool{}
		var order []corpusDoc
		for _, g := range it.GoldSources {
			if _, ok := byKey[g]; ok {
				gold[g] = true
			}
		}
		// Gold documents first (so a mid-run stop still covers them), then a
		// BOUNDED negative sample. Scoring the whole corpus per item costs
		// |corpus| × items × repeats calls and buys nothing extra: the
		// reliability question is about a window's score stability, not about
		// how many negatives exist.
		for _, d := range docs {
			if gold[d.Key] {
				order = append(order, d)
			}
		}
		negSeen := 0
		for _, d := range docs {
			if gold[d.Key] {
				continue
			}
			if *negatives > 0 && negSeen >= *negatives {
				break
			}
			negSeen++
			order = append(order, d)
		}
		if len(order) == 0 {
			continue
		}

		ir := itemReport{Item: it.ID, GoldSources: strings.Join(it.GoldSources, ",")}
		type scored struct {
			key    string
			isGold bool
			scores []float64
		}
		var matrix []scored

		for _, d := range order {
			got, err := sampler.SampleBody(ctx, it.Query, d.Text)
			if err != nil || len(got) == 0 {
				continue
			}
			// One window per document is what the small-file shortcut already
			// produces in production; for a multi-window body the first window
			// is taken so the probe measures the scorer, not arm economics.
			sm := got[0]
			scores := make([]float64, 0, *repeats)
			ok := true
			for r := 0; r < *repeats; r++ {
				v, _, err := sc.Score(ctx, it.Query, sm)
				if err != nil {
					fmt.Fprintf(os.Stderr, "scoreprobe: %s/%s repeat %d: %v\n", it.ID, d.Key, r, err)
					ok = false
					break
				}
				scores = append(scores, v)
			}
			if !ok {
				continue
			}
			ir.Documents++
			if gold[d.Key] {
				ir.GoldDocuments++
			}
			matrix = append(matrix, scored{key: d.Key, isGold: gold[d.Key], scores: scores})

			m := mean(scores)
			sd := stddev(scores)
			near := nearestThreshold(m)
			stable := allSameSide(scores, near)
			mn, mx := scores[0], scores[0]
			for _, s := range scores {
				mn, mx = math.Min(mn, s), math.Max(mx, s)
			}
			wr := windowReport{
				Item: it.ID, Source: d.Key, Gold: gold[d.Key], Start: sm.Start,
				Repeats: len(scores), Mean: m, Stddev: sd, Min: mn, Max: mx,
				Spread: mx - mn, Scores: scores,
				DecisionStable: stable, NearestThresh: near,
			}
			wr.Borderline = !stable || math.Abs(m-near) <= tieBand
			if stable {
				stableCount++
			}
			if wr.Borderline {
				borderlineCount++
			}
			if gold[d.Key] {
				goldSum += m
				goldN++
				rep.GoldWindows++
				if wr.Borderline {
					goldBorderline++
				}
			} else {
				nonGoldSum += m
				nonGoldN++
			}
			rep.Windows++
			stddevSum += sd
			if *detail {
				rep.WindowsDetail = append(rep.WindowsDetail, wr)
			}
		}

		if len(matrix) > 0 {
			scoredItems++
			// Argmax on the mean, plus how many runners-up sit within tieBand
			// of the winner — the population a re-score could plausibly
			// reshuffle.
			sort.SliceStable(matrix, func(i, j int) bool {
				return mean(matrix[i].scores) > mean(matrix[j].scores)
			})
			winner := matrix[0]
			ir.TopGoldStable = winner.isGold
			win := mean(winner.scores)
			for _, m := range matrix[1:] {
				if win-mean(m.scores) <= tieBand {
					ir.TopTieCount++
				}
			}
			if ir.TopGoldStable {
				topStable++
			}
			topTieSum += ir.TopTieCount
		}
		if *detail {
			rep.ItemsDetail = append(rep.ItemsDetail, ir)
		}
		fmt.Fprintf(os.Stderr, "scoreprobe: %s done (docs=%d gold_docs=%d top_gold_stable=%v ties=%d)\n",
			it.ID, ir.Documents, ir.GoldDocuments, ir.TopGoldStable, ir.TopTieCount)
	}

	if rep.Windows > 0 {
		rep.DecisionStableRate = float64(stableCount) / float64(rep.Windows)
		rep.BorderlineRate = float64(borderlineCount) / float64(rep.Windows)
		rep.MeanStddev = stddevSum / float64(rep.Windows)
	}
	if goldN > 0 {
		rep.GoldBorderlineRate = float64(goldBorderline) / float64(goldN)
		rep.MeanGoldScore = goldSum / float64(goldN)
	}
	if nonGoldN > 0 {
		rep.MeanNonGoldScore = nonGoldSum / float64(nonGoldN)
	}
	rep.ScoreGap = rep.MeanGoldScore - rep.MeanNonGoldScore
	if scoredItems > 0 {
		rep.TopGoldStableRate = float64(topStable) / float64(scoredItems)
		rep.MeanTopTieCount = float64(topTieSum) / float64(scoredItems)
	} else {
		rep.TopGoldStableRate = -1
	}

	printSummary(rep)
	if *outPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*outPath, append(b, '\n'), 0o644); err != nil {
			fatal(fmt.Errorf("write: %w", err))
		}
		fmt.Fprintf(os.Stderr, "scoreprobe: wrote %s\n", *outPath)
	}
}

func printSummary(r report) {
	fmt.Println("=== scoreprobe · 打分器可靠性（record-only，不设门）===")
	fmt.Printf("model=%s  thinking=%s  repeats=%d  items=%d  windows=%d (gold=%d)\n\n",
		r.Model, r.Thinking, r.Repeats, r.Items, r.Windows, r.GoldWindows)
	row := func(k, v string) { fmt.Printf("  %-32s %s\n", k, v) }
	row("决策稳定率 decision_stable", pct(r.DecisionStableRate))
	row("模糊带占比 borderline", pct(r.BorderlineRate))
	row("金标窗模糊带 gold_borderline", pct(r.GoldBorderlineRate))
	row("重测标准差 mean_stddev", f2(r.MeanStddev))
	row("金标均分 mean_gold", f2(r.MeanGoldScore))
	row("非金标均分 mean_non_gold", f2(r.MeanNonGoldScore))
	row("分离度 score_gap", f2(r.ScoreGap))
	row("top 金标稳定率", pct(r.TopGoldStableRate))
	row("平均并列数 mean_top_ties", f2(r.MeanTopTieCount))
	fmt.Println()
	fmt.Println("读法：")
	fmt.Println("  · decision_stable 低 ⇒ 同一次检索重跑会得到不同的保留集，任何以")
	fmt.Println("    检索结果为判据的 A/B 都读不出结论（判官 10/30 翻转是同一类问题）。")
	fmt.Println("  · gold_borderline 高 ⇒ 金标窗常骑在 Score>=4 / bestScore>=8 的线上，")
	fmt.Println("    预排（MiniLM top-k）会随机丢或留金标，收益不可预期。")
	fmt.Println("  · score_gap 小 ⇒ 0-10 这把尺子分不开金标与难负例，打分只是噪声。")
	fmt.Println("  · -thinking-off 跑第二遍对比两臂，才回答 P1-2 能否翻默认值。")
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func pct(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "scoreprobe:", err)
	os.Exit(1)
}

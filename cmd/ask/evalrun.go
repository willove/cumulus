package main

// ask eval-run — LENS 式真实语料评测（R-E1，lens-notes §7.4）。
// items JSONL 复用 internal/eval.Item：answer 字段是判官参考文本（检索型
// 数据集没有短答案字符串，如法条原文），gold_sources 是 Ev.Rec 目标文档键。
// 每项跑真实管线（FAST→DEEP 升级）得系统答案，同时产出一个 Closed-Book
// 直答对照（论文 §6：揭穿「凭模型记忆答对」）；-judge 时端点判官按
// judge_correct 资产把「候选回答 vs 参考标准」打成 0–10 分（≥7 判对）。
// 逐项落盘可续跑（中断不丢已完成项），每次运行末尾对**全量**结果文件
// 聚合：EM/Ev.Rec/Ground + 失败四分类 + McNemar 配对 + 档位分布。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/eval"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/ns"
	"github.com/cumubase/ask/internal/prompts"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

// judgePass is the Correct threshold over the judge's 0–10 score.
const judgePass = 7.0

// evalResult is one per-item line in the results JSONL.
type evalResult struct {
	ID      string         `json:"id"`
	Mode    string         `json:"mode,omitempty"`
	Loops   int            `json:"loops,omitempty"`
	Widened int            `json:"widened,omitempty"`
	Conf    float64        `json:"conf,omitempty"`
	Calls   int            `json:"calls,omitempty"`
	Tokens  int64          `json:"tokens,omitempty"`
	Err     string         `json:"err,omitempty"`
	Cites   []string       `json:"cites,omitempty"` // citation business keys — per-item diagnosis
	Eval    eval.ItemScore `json:"eval"`
	Judge   string         `json:"judge,omitempty"`
	CB      eval.ItemScore `json:"closed_book"`
	CBJudge string         `json:"cb_judge,omitempty"`
}

// evalReport is the aggregate scorecard printed after every run.
type evalReport struct {
	N          int            `json:"n"`
	System     eval.Report    `json:"system"`
	ClosedBook eval.Report    `json:"closed_book"`
	McNemar    eval.McNemar   `json:"mcnemar"`
	Modes      map[string]int `json:"modes"`
	Resumed    int            `json:"resumed"`
	Judged     bool           `json:"judged"`
}

// evalRun processes items against the live pipeline and appends one line per
// new item to outPath (resume: ids already present are skipped). The printed
// report aggregates the whole file so the last batch of a chunked run shows
// the full picture.
func evalRun(ctx context.Context, c *client.Client, st *ingest.Store, sourcesColl, namespace, file, outPath string, judgeOn, prior, l1pre bool, limit int) error {
	items, err := readEvalItems(file)
	if err != nil {
		return err
	}
	done, err := readDoneIDs(outPath)
	if err != nil {
		return err
	}
	list, err := st.ActiveSources(ctx)
	if err != nil {
		return err
	}
	keyByID := map[string]string{}
	for _, s := range list {
		keyByID[s.ID] = s.BusinessKey
	}
	// L1 prefilter (D7): materialize the body_embed index once (bounded
	// backfill), then narrow candidates per item by query-vector KNN.
	var embedFn ingest.EmbedderFn
	if l1pre {
		var dims int
		var model string
		embedFn, dims, model = embedderFor()
		if _, err := st.EnsureEmbed(ctx, embedFn, dims, model, 64); err != nil {
			return fmt.Errorf("ensure body_embed: %w", err)
		}
	}
	stack := newProdStack()
	fe := fast.New(stack.scorer)
	fe.UsePrior = prior
	fe.Analyzer, fe.Synth, fe.Expander = stack.analyzer, stack.synth, stack.expander
	kbE := kb.New(fe, cluster.NewCumuStore(c, ns.Coll(namespace, "ask_clusters")), stack.emb)
	kbE.Edges = graph.NewCumuStore(c, ns.Coll(namespace, "ask_weak_edges"))
	kbE.Cites = deep.NewCumuCiteStore(c, ns.Coll(namespace, "ask_cites"))
	dE := deep.New(kbE, deep.NewCumuStore(c, ns.Coll(namespace, "ask_conflicts")))
	dE.Scorer = stack.scorer
	dE.Synth = stack.synth
	// 扩征（Sirchmunk ReAct 对齐）：覆盖未满时用新关键词向全库再征文件。
	var refiner *llm.AigateKeywordRefiner
	if stack.chat != nil {
		refiner = &llm.AigateKeywordRefiner{Client: stack.chat}
	}
	dE.Widen = widenFunc(fe, st, c, sourcesColl, refiner)
	// DEEP 探索前按关键词级联重排候选（10k 规模：ingest 顺序不可用）
	dE.RankAdmission = rankFunc(fe, st, c, sourcesColl)

	// Per-item budget: a batch of items must not share one global deadline —
	// a few slow DEEP escalations would otherwise starve the tail items.
	perItem := 120 * time.Second
	if v := os.Getenv("ASK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			perItem = d
		}
	}

	outF, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer outF.Close()
	w := bufio.NewWriter(outF)
	resumed, processed := 0, 0
	for _, it := range items {
		if done[it.ID] {
			resumed++
			continue
		}
		if limit > 0 && processed >= limit {
			break
		}
		ictx, cancel := context.WithTimeout(ctx, perItem)
		runList := list
		if l1pre {
			if narrowed, err := narrowByKNN(ctx, c, embedFn, sourcesColl, list, it.Query); err != nil {
				fmt.Fprintf(os.Stderr, "eval-run %s: l1pre: %v\n", it.ID, err)
			} else if len(narrowed) > 0 {
				runList = narrowed
			}
		}
		rec := evalOne(ictx, dE, stack.chat, judgeOn, runList, keyByID, it)
		cancel()
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := w.WriteString(string(line) + "\n"); err != nil {
			return err
		}
		processed++
	}
	if err := w.Flush(); err != nil {
		return err
	}
	all, err := readResults(outPath)
	if err != nil {
		return err
	}
	printJSON(aggregateResults(all, judgeOn && stack.chat != nil, resumed))
	return nil
}

// narrowByKNN narrows the active-source list to the body_embed KNN hits for
// one query. Index is deliberately omitted: with no usable ANN structure the
// engine falls back to a filtered scan, which is the right operating point
// for small corpora (索引是缓存：只影响快慢，不影响正确性).
func narrowByKNN(ctx context.Context, c *client.Client, embedFn ingest.EmbedderFn, sourcesColl string, list []source.Source, query string) ([]source.Source, error) {
	qv, err := embedFn(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	knn, err := c.KNN(ctx, sourcesColl, client.KNNRequest{
		Field: "body_embed", Vector: qv[0], K: 8, Metric: "cosine",
		Filter: map[string]any{"status": source.StatusActive},
	})
	if err != nil {
		return nil, err
	}
	return orderByKNN(list, knn.Documents), nil
}

// evalOne runs the system pipeline plus the closed-book contrast for one item.
func evalOne(ctx context.Context, dE *deep.Engine, chat *llm.ChatClient, judgeOn bool, list []source.Source, keyByID map[string]string, it eval.Item) evalResult {
	rec := evalResult{ID: it.ID}
	var tokBefore int64
	if chat != nil {
		tokBefore = chat.TotalTokens()
	}
	res, err := dE.Ask(ctx, it.Query, list)
	if err != nil {
		rec.Mode = "error"
		rec.Err = err.Error()
	} else {
		rec.Mode = res.Mode
		rec.Loops = res.Loops
		rec.Widened = res.Widened
		rec.Conf = res.Answer.Confidence
		rec.Calls = res.Answer.LLMCalls
	}
	pred := predictionOf(res, keyByID)
	rec.Eval = eval.Score(it, pred)
	for _, id := range pred.SourceIDs {
		if !strings.HasPrefix(id, "src:") {
			rec.Cites = append(rec.Cites, id)
		}
	}
	if chat != nil {
		rec.Tokens = chat.TotalTokens() - tokBefore
	}
	if judgeOn && chat != nil {
		if ok, why, jerr := judgeAnswer(ctx, chat, it.Query, it.Answer, res.Answer.Summary); jerr != nil {
			fmt.Fprintf(os.Stderr, "eval-run %s: judge: %v\n", it.ID, jerr)
		} else if ok {
			rec.Eval.Correct = true
			rec.Judge = why
		} else {
			rec.Judge = why
		}
	}
	// Closed-book contrast: answer from model memory only, no retrieval.
	cbAns := ""
	if chat != nil {
		cbAns, _ = chat.Complete(ctx, "仅凭你自己的记忆回答下面的问题，不要编造；不知道就只回答「不知道」。问题："+it.Query)
	}
	rec.CB = eval.ClosedBook(it, cbAns)
	if judgeOn && chat != nil && strings.TrimSpace(cbAns) != "" {
		if ok, why, jerr := judgeAnswer(ctx, chat, it.Query, it.Answer, cbAns); jerr == nil {
			if ok {
				rec.CB.Correct = true
			}
			rec.CBJudge = why
		}
	}
	return rec
}

// predictionOf maps a pipeline result onto the eval protocol's prediction.
// SourceIDs carry both the internal doc id and — when known — the business
// key, since gold_sources reference keys (ingest-jsonl titles).
func predictionOf(res deep.Result, keyByID map[string]string) eval.Prediction {
	p := eval.Prediction{
		Query:   res.Answer.Query,
		Answer:  res.Answer.Summary,
		Refs:    len(res.Citations.Refs),
		Skipped: res.Answer.Skipped,
	}
	seen := map[string]bool{}
	add := func(id, key string) {
		for _, v := range []string{id, key} {
			if v != "" && !seen[v] {
				seen[v] = true
				p.SourceIDs = append(p.SourceIDs, v)
			}
		}
	}
	for _, r := range res.Citations.Refs {
		add(r.SourceID, keyByID[r.SourceID])
		if r.Resolved {
			p.Resolved++
		}
	}
	if res.Answer.SourceID != "" {
		add(res.Answer.SourceID, keyByID[res.Answer.SourceID])
	}
	return p
}

// judgeAnswer scores the candidate against the reference with the judge_correct
// asset; ≥ judgePass counts as correct. Returns the score+reason string for
// the results log.
func judgeAnswer(ctx context.Context, chat *llm.ChatClient, query, reference, answer string) (bool, string, error) {
	tmpl, err := prompts.Load(prompts.JudgeCorrect)
	if err != nil {
		return false, "", err
	}
	prompt := prompts.Render(tmpl, map[string]string{
		"query":     query,
		"reference": reference,
		"answer":    answer,
	})
	raw, err := chat.Complete(ctx, prompt)
	if err != nil {
		return false, "", err
	}
	clean, _ := llm.SplitThink(raw)
	score, reason, err := llm.ParseScoreJSON(clean)
	if err != nil {
		return false, "", err
	}
	return score >= judgePass, fmt.Sprintf("%.0f %s", score, reason), nil
}

func readEvalItems(path string) ([]eval.Item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []eval.Item
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var it eval.Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if it.ID == "" || it.Query == "" {
			return nil, fmt.Errorf("%s: item needs id and query", path)
		}
		items = append(items, it)
	}
	return items, nil
}

// readDoneIDs scans a results JSONL for already-recorded item ids (resume).
func readDoneIDs(path string) (map[string]bool, error) {
	done := map[string]bool{}
	lines, err := readResults(path)
	if err != nil || lines == nil {
		return done, nil
	}
	for _, r := range lines {
		done[r.ID] = true
	}
	return done, nil
}

func readResults(path string) ([]evalResult, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []evalResult
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r evalResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

func aggregateResults(lines []evalResult, judged bool, resumed int) evalReport {
	rep := evalReport{Resumed: resumed, Judged: judged, Modes: map[string]int{}}
	sys := make([]eval.ItemScore, 0, len(lines))
	cb := make([]eval.ItemScore, 0, len(lines))
	for _, r := range lines {
		sys = append(sys, r.Eval)
		cb = append(cb, r.CB)
		rep.Modes[r.Mode]++
	}
	rep.N = len(lines)
	rep.System = eval.Aggregate(sys)
	rep.ClosedBook = eval.Aggregate(cb)
	rep.McNemar = eval.Compare(sys, cb)
	return rep
}

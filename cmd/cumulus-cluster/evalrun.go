package main

// cumulus-cluster eval-run — LENS 式真实语料评测。
// items JSONL 复用 internal/eval.Item：answer 字段是判官参考文本（检索型
// 数据集没有短答案字符串，如法条原文），gold_sources 是 Ev.Rec 目标文档键。
// 每项跑真实管线（FAST→DEEP 升级）得系统答案，同时产出一个 Closed-Book
// 直答对照（揭穿「凭模型记忆答对」）；-judge 时端点判官按
// judge_correct 资产把「候选回答 vs 参考标准」打成 0–10 分（≥7 判对）。
// 逐项落盘可续跑（中断不丢已完成项），每次运行末尾对**全量**结果文件
// 聚合：EM/Ev.Rec/Ground + 失败四分类 + McNemar 配对 + 档位分布。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/prompts"
	"github.com/willove/cumulus/internal/source"
)

// judgePass is the Correct threshold over the judge's 0–10 score.
const judgePass = 7.0

// evalResult is one per-item line in the results JSONL.
// Token fields are split (LENS Remark 2 / ir-rag 3.2): search vs judge are
// independent cost centers; judge never rides on the search budget.
type evalResult struct {
	ID      string  `json:"id"`
	Mode    string  `json:"mode,omitempty"`
	Loops   int     `json:"loops,omitempty"`
	Widened int     `json:"widened,omitempty"`
	Conf    float64 `json:"conf,omitempty"`
	Calls   int     `json:"calls,omitempty"`
	Tokens  int64   `json:"tokens,omitempty"` // search path only (pre-judge)
	// SearchTokens aliases the search-path spend; JudgeTokens is judge_correct
	// (system arm + closed-book arm when both run). Total = Search+Judge.
	SearchTokens int64 `json:"search_tokens,omitempty"`
	JudgeTokens  int64 `json:"judge_tokens,omitempty"`
	// RejectedProposals counts AcceptFold refusals on the write path for this
	// item (Self-Index cost accounting includes rejected proposals).
	RejectedProposals int            `json:"rejected_proposals,omitempty"`
	Err               string         `json:"err,omitempty"`
	Cites             []string       `json:"cites,omitempty"` // citation business keys — per-item diagnosis
	Eval              eval.ItemScore `json:"eval"`
	Judge             string         `json:"judge,omitempty"`
	CB                eval.ItemScore `json:"closed_book"`
	CBJudge           string         `json:"cb_judge,omitempty"`
	// Admission ceiling (ir-rag 1.5 / LENS Remark 1): did gold enter the
	// ranked/widen universe this run actually scored?
	GoldInAdmitted bool `json:"gold_in_admitted,omitempty"`
	// GoldInCorpus: gold business_key exists among active sources at run time.
	GoldInCorpus bool `json:"gold_in_corpus,omitempty"`
	// AbstainP / AbstainAction echo the zero-LLM head when wired (3.1).
	AbstainP      float64 `json:"abstain_p,omitempty"`
	AbstainAction string  `json:"abstain_action,omitempty"`
}

// NRBreakdown splits not_retrieved items into admission-ceiling vs
// post-admission failure (ir-rag 1.5). Fields only count items whose
// taxonomy class is not_retrieved.
type NRBreakdown struct {
	NotRetrieved int `json:"not_retrieved"`
	// GoldMissingFromCorpus: gold key never present in active sources.
	GoldMissingFromCorpus int `json:"gold_missing_from_corpus"`
	// GoldNotAdmitted: gold in corpus but outside ranked/widen scored set
	// (Remark 1 ceiling — more LLM ranking cannot buy it back).
	GoldNotAdmitted int `json:"gold_not_admitted"`
	// GoldAdmittedNoCite: gold was scored but EvRec still false (wrong window
	// kept / citation mapping), i.e. discovery reached it and failed later.
	GoldAdmittedNoCite int `json:"gold_admitted_no_cite"`
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
	Frozen     eval.Frozen    `json:"frozen"`
	ConfigText string         `json:"config_text"` // the human-readable config the hash covers
	// Cost split (3.2): search_tokens / judge_tokens / rejected_proposals.
	SearchTokens      int64 `json:"search_tokens"`
	JudgeTokens       int64 `json:"judge_tokens"`
	RejectedProposals int   `json:"rejected_proposals"`
	// NotRetrieved admission breakdown (1.5).
	NRBreakdown NRBreakdown `json:"nr_breakdown"`
}

// evalRun processes items against the live pipeline and appends one line per
// new item to outPath (resume: ids already present are skipped). The printed
// report aggregates the whole file so the last batch of a chunked run shows
// the full picture.
func evalRun(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl, namespace, file, outPath string, judgeOn, prior, l1pre bool, limit int, tag string) error {
	items, err := readEvalItems(file)
	if err != nil {
		return err
	}
	var itemsRaw []byte
	if raw, rerr := os.ReadFile(file); rerr == nil {
		itemsRaw = raw
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
		var eerr error
		embedFn, dims, model, eerr = embedderFor()
		if eerr != nil {
			return eerr // strict: CLUS_MINILM_REQUIRE=1 fails the run
		}
		if _, err := st.EnsureEmbed(ctx, embedFn, dims, model, 64); err != nil {
			return fmt.Errorf("ensure body_embed: %w", err)
		}
	}
	stack := newProdStack()
	fe := fast.New(stack.scorer)
	fe.UsePrior = prior
	fe.Analyzer, fe.Synth, fe.Expander = stack.analyzer, stack.synth, stack.expander
	// 1.6: prior history arm from live clus_evidence (list is already loaded).
	if prior {
		fe.PriorHist = priorHistFromStore(ctx, st, list)
	}
	kbE := kb.New(fe, cluster.NewCumuStore(c, ns.Coll(namespace, "clus_clusters")), stack.emb)
	kbE.Cursor = &kvLastCluster{c: c, key: ns.KV(namespace, "clus:lastcluster")}
	kbE.Edges = graph.NewCumuStore(c, ns.Coll(namespace, "clus_weak_edges"))
	kbE.Cites = deep.NewCumuCiteStore(c, ns.Coll(namespace, "clus_cites"))
	dE := deep.New(kbE, deep.NewCumuStore(c, ns.Coll(namespace, "clus_conflicts")))
	dE.Scorer = stack.scorer
	dE.Synth = stack.synth
	// Independent search token budget (3.2 / LENS Remark 2).
	if stack.chat != nil {
		dE.TokensUsed = stack.chat.TotalTokens
		if v := os.Getenv("CLUS_SEARCH_TOKEN_BUDGET"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				dE.TokenBudget = n
			}
		}
		if os.Getenv("CLUS_ABSTAIN") == "1" {
			dE.Abstain = abstain.Default()
			if os.Getenv("CLUS_EARLY_ABSTAIN") != "1" {
				// 早弃权默认关：DEEP 有救回拒答的真实先例，
				// 运营商显式开才牺牲这段恢复机会换 token。
				dE.Abstain.EarlyAbove = 0
			}
		}
		if os.Getenv("CLUS_QUERY_SIM") == "1" {
			dE.QuerySim = &llm.AigateQuerySimulator{Client: stack.chat}
		}
	} else if os.Getenv("CLUS_ABSTAIN") == "1" {
		dE.Abstain = abstain.Default()
		if os.Getenv("CLUS_EARLY_ABSTAIN") != "1" {
			// 早弃权默认关：DEEP 有救回拒答的真实先例，
			// 运营商显式开才牺牲这段恢复机会换 token。
			dE.Abstain.EarlyAbove = 0
		}
	}
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
	if v := os.Getenv("CLUS_TIMEOUT"); v != "" {
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
		// Flush after EVERY item. The header promises "中断不丢已完成项" and
		// the resume logic keys off exactly that file; buffering to the end of
		// the loop meant any interrupt, per-item timeout path or mid-loop
		// error silently discarded the whole batch — and a -limit chunked run
		// then re-ran everything from scratch. Items are minutes each, so a
		// per-item fsync-costed flush is the correct trade.
		if err := w.Flush(); err != nil {
			return err
		}
		processed++
	}
	all, err := readResults(outPath)
	if err != nil {
		return err
	}
	{
		r := aggregateResults(all, judgeOn && stack.chat != nil, resumed)
		r.ConfigText = evalConfig(stack, prior, l1pre, judgeOn, namespace)
		// A6: bind the scorecard to items + corpus (active sources) + config
		// so an ablation row cannot silently change the sample set.
		r.Frozen = eval.Freeze(itemsRaw, corpusFingerprint(list), []byte(evalConfig(stack, prior, l1pre, judgeOn, namespace)), 0)
		// B3: the run also lands in clus_evals so the workbench scoreboard
		// can list/detail it. Best-effort: the JSONL stays the primary
		// artifact, and a failed scoreboard write must not fail a 3h run.
		if serr := saveEvalRun(ctx, c, namespace, tag, r); serr != nil {
			fmt.Fprintf(os.Stderr, "eval-run: 记分牌落库失败（JSONL 仍有效）：%v\n", serr)
		}
		printJSON(r)
	}
	return nil
}

// saveEvalRun persists one aggregate report as a scoreboard row. The
// collection is declared lazily (idempotent): eval-run may be the first
// writer, and the engine fail-closes writes to undeclared collections.
func saveEvalRun(ctx context.Context, c cumulite.Port, namespace, tag string, r evalReport) error {
	coll := ns.Coll(namespace, "clus_evals")
	if err := c.EnsureCollection(ctx, coll); err != nil {
		return err
	}
	st := eval.NewCumuStore(c, coll)
	if err := st.SaveRun(ctx, eval.RunDoc{
		Tag: tag, N: r.N, Judged: r.Judged,
		System: r.System, ClosedBook: r.ClosedBook, McNemar: r.McNemar, Modes: r.Modes,
		SearchTokens: r.SearchTokens, JudgeTokens: r.JudgeTokens,
		RejectedProposals: r.RejectedProposals,
		Frozen:            &r.Frozen,
		ConfigText:        r.Frozen.ConfigSHA + " ← " + r.ConfigText,
		Extra:             map[string]any{"nr_breakdown": r.NRBreakdown},
	}); err != nil {
		return err
	}
	return nil
}

// corpusFingerprint is a deterministic sha over sorted source IDs and bodies.
func corpusFingerprint(list []source.Source) []byte {
	cp := append([]source.Source(nil), list...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	var b []byte
	for _, s := range cp {
		b = append(b, s.ID...)
		b = append(b, 0)
		b = append(b, s.Body...)
		b = append(b, 0)
	}
	return b
}

// evalConfig captures knobs that change which path ran (prior/L1/judge/ns).
// evalConfig is the CONFIG half of the A.6 binding, and it must name everything
// that can change a number without changing the items or the corpus. It used to
// carry only the four booleans + ns, so two ablations on DIFFERENT models
// produced byte-identical ConfigSHA — the scoreboard showed them as the same
// configuration, and a model swap looked like a no-op.
func evalConfig(stack prodStack, prior, l1pre, judge bool, namespace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "prior=%v;l1pre=%v;judge=%v;ns=%s", prior, l1pre, judge, namespace)
	// Model identity, masked endpoint, and the embedder that actually served.
	fmt.Fprintf(&b, ";chat_model=%s", envOr("AIGATE_CHAT_MODEL", "<offline-stub>"))
	fmt.Fprintf(&b, ";base_url=%s", maskHost(os.Getenv("AIGATE_BASE_URL")))
	fmt.Fprintf(&b, ";embed_model=%s", os.Getenv("AIGATE_EMBED_MODEL"))
	if os.Getenv("CLUS_EMBED") == "minilm" {
		fmt.Fprintf(&b, ";embed_seat=minilm")
	} else {
		fmt.Fprintf(&b, ";embed_seat=%s", embedSeatLabel())
	}
	// The reuse/merge lines: changing them changes which queries hit a cluster
	// and therefore every reuse-dependent metric.
	fmt.Fprintf(&b, ";reuse_theta=%.4f;merge_theta=%.4f;split_cap=%d",
		kb.DefaultReuseTheta, kb.DefaultMergeTheta, cluster.DefaultSplitCap)
	// The search-path knobs that move tokens and latency.
	fmt.Fprintf(&b, ";max_loops=%d;widen_budget=%d;correct_budget=%d",
		deep.MaxLoops, deep.WidenBudget, deep.CorrectBudget)
	// Which mechanisms were on.
	if os.Getenv("CLUS_ABSTAIN") == "1" {
		b.WriteString(";abstain=1")
	}
	if os.Getenv("CLUS_QUERY_SIM") == "1" {
		b.WriteString(";query_sim=1")
	}
	if v := os.Getenv("CLUS_SEARCH_TOKEN_BUDGET"); v != "" {
		fmt.Fprintf(&b, ";token_budget=%s", v)
	}
	if stack.chat != nil {
		b.WriteString(";endpoint=live")
	} else {
		b.WriteString(";endpoint=offline-stub")
	}
	return b.String()
}

// maskHost keeps the endpoint identifiable across runs without recording
// credentials or the full path.
func maskHost(u string) string {
	if u == "" {
		return "<unset>"
	}
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	// No host means no usable endpoint identity (and "not a url" parses fine
	// as a relative reference), so treat it as unparseable.
	return "<unparseable>"
}

// embedSeatLabel names the embedder the search stack would actually build, so a
// silent hash fallback is visible in the binding.
func embedSeatLabel() string {
	if os.Getenv("AIGATE_EMBED_MODEL") != "" {
		return "aigate"
	}
	return "local-hash-64"
}

// narrowByKNN narrows the active-source list to the body_embed KNN hits for
// one query. Index is deliberately omitted: with no usable ANN structure the
// engine falls back to a filtered scan, which is the right operating point
// for small corpora (索引是缓存：只影响快慢，不影响正确性).
func narrowByKNN(ctx context.Context, c cumulite.Port, embedFn ingest.EmbedderFn, sourcesColl string, list []source.Source, query string) ([]source.Source, error) {
	qv, err := embedFn(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	knn, err := c.KNN(ctx, sourcesColl, contract.KNNRequest{
		Field: "body_embed", Vector: qv[0], K: 8, Metric: "cosine",
		Filter: map[string]any{"status": source.StatusActive},
	})
	if err != nil {
		return nil, err
	}
	return orderByKNN(list, knn.Documents), nil
}

// evalOne runs the system pipeline plus the closed-book contrast for one item.
// Token accounting is split: search spend is sealed before any judge call so
// judge/closed-book tokens never inflate the search budget (3.2).
// keyByID maps internal IDs → business keys for Ev.Rec and admission checks.
// evalSearchOne is the shared in-process per-item search and accounting
// mechanism for CLI and GUI. The legacy wrapper below deliberately keeps the
// historical judge-OR-rule scoring; eval-v2 scores its raw result separately.
func evalSearchOne(ctx context.Context, dE *deep.Engine, chat *llm.ChatClient, list []source.Source, keyByID map[string]string, it eval.Item) (evalResult, deep.Result) {
	rec := evalResult{ID: it.ID}
	var tokBefore int64
	rejBefore := 0
	if chat != nil {
		tokBefore = chat.TotalTokens()
	}
	if dE.KB != nil {
		rejBefore = dE.KB.RejectedProposals
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
		rec.AbstainP = res.AbstainP
		rec.AbstainAction = res.AbstainAction
	}
	// 1.5: was gold in the active corpus, and was it among scored sources?
	corpusKeys := map[string]bool{}
	for _, s := range list {
		if s.BusinessKey != "" {
			corpusKeys[s.BusinessKey] = true
		}
	}
	admittedKeys := map[string]bool{}
	for _, id := range res.Admitted {
		if k := keyByID[id]; k != "" {
			admittedKeys[k] = true
		}
	}
	for _, g := range it.Gold {
		if g == "" {
			continue
		}
		if corpusKeys[g] || corpusKeys[strings.ToLower(g)] {
			rec.GoldInCorpus = true
		}
		if admittedKeys[g] || admittedKeys[strings.ToLower(g)] {
			rec.GoldInAdmitted = true
		}
		// Cites may already carry business keys.
		for _, c := range res.Citations.Refs {
			if k := keyByID[c.SourceID]; k == g {
				rec.GoldInAdmitted = true
			}
		}
	}
	if dE.KB != nil {
		rec.RejectedProposals = dE.KB.RejectedProposals - rejBefore
		if rec.RejectedProposals < 0 {
			rec.RejectedProposals = 0
		}
	}
	pred := predictionOf(res, keyByID)
	rec.Eval = eval.Score(it, pred)
	for _, id := range pred.SourceIDs {
		if !strings.HasPrefix(id, "src:") {
			rec.Cites = append(rec.Cites, id)
		}
	}
	if chat != nil {
		rec.SearchTokens = chat.TotalTokens() - tokBefore
		rec.Tokens = rec.SearchTokens
	}
	return rec, res
}

func evalOne(ctx context.Context, dE *deep.Engine, chat *llm.ChatClient, judgeOn bool, list []source.Source, keyByID map[string]string, it eval.Item) evalResult {
	rec, res := evalSearchOne(ctx, dE, chat, list, keyByID, it)
	if judgeOn && chat != nil {
		j0 := chat.TotalTokens()
		if ok, why, jerr := judgeAnswer(ctx, chat, it.Query, it.Answer, res.Answer.Summary); jerr != nil {
			fmt.Fprintf(os.Stderr, "eval-run %s: judge: %v\n", it.ID, jerr)
		} else if ok {
			rec.Eval.Correct = true
			rec.Judge = why
		} else {
			rec.Judge = why
		}
		rec.JudgeTokens += chat.TotalTokens() - j0
	}
	// Closed-book contrast: answer from model memory only, no retrieval.
	// CB generation tokens are neither search nor judge — left out of the
	// split so SearchTokens stays a pure retrieval cost line.
	cbAns := ""
	if chat != nil {
		cbAns, _ = chat.Complete(ctx, "仅凭你自己的记忆回答下面的问题，不要编造；不知道就只回答「不知道」。问题："+it.Query)
	}
	rec.CB = eval.ClosedBook(it, cbAns)
	if judgeOn && chat != nil && strings.TrimSpace(cbAns) != "" {
		j0 := chat.TotalTokens()
		if ok, why, jerr := judgeAnswer(ctx, chat, it.Query, it.Answer, cbAns); jerr == nil {
			if ok {
				rec.CB.Correct = true
			}
			rec.CBJudge = why
		}
		rec.JudgeTokens += chat.TotalTokens() - j0
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
//
// A parse failure is no longer swallowed. It used to return an empty done-set
// with a nil error, so one malformed or torn line (exactly what an interrupted
// pre-flush run left behind) looked like "nothing done yet": the run
// re-processed every item and appended duplicate ids, and aggregateResults then
// double-counted them into every metric.
func readDoneIDs(path string) (map[string]bool, error) {
	done := map[string]bool{}
	lines, err := readResults(path)
	if err != nil {
		return nil, fmt.Errorf("resume: results file %s is unreadable: %w", path, err)
	}
	for _, r := range lines {
		if r.ID != "" {
			done[r.ID] = true
		}
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
		rep.SearchTokens += r.SearchTokens
		rep.JudgeTokens += r.JudgeTokens
		rep.RejectedProposals += r.RejectedProposals
		// 1.5: only items that land in not_retrieved feed the admission split.
		if eval.Classify(r.Eval) == "not_retrieved" {
			rep.NRBreakdown.NotRetrieved++
			switch {
			case !r.GoldInCorpus:
				rep.NRBreakdown.GoldMissingFromCorpus++
			case !r.GoldInAdmitted:
				rep.NRBreakdown.GoldNotAdmitted++
			default:
				rep.NRBreakdown.GoldAdmittedNoCite++
			}
		}
	}
	rep.N = len(lines)
	rep.System = eval.Aggregate(sys)
	rep.ClosedBook = eval.Aggregate(cb)
	rep.McNemar = eval.Compare(sys, cb)
	return rep
}

// Command ask is the cognitive-search suite CLI: ingest sources and run the
// FAST/DEEP search path against a running cumudb.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/deep"
	"github.com/cumubase/ask/internal/eval"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/minilm"
	"github.com/cumubase/ask/internal/ns"
	"github.com/cumubase/ask/internal/source"
	"github.com/willove/cumudb/pkg/client"
	"github.com/willove/cumulite"
)

const usage = `ask — cognitive search suite (on cumudb)

Usage:
  ask put    -title T [-type md] [-uri U] [-key K] [-lang zh] -body-file F
  ask put    -title T -body "text"
  ask ingest-jsonl -file data.jsonl [-job NAME] [-map map.json]
  ask ingest-files -dir D [-recursive] [-job NAME]
  ask search -q "query" [-session ID] [-raw] [-hopts 168h]
  ask get    <id>
  ask delete <id>
  ask ensure [-embed]           # 声明集合（-embed 兼补内容向量）
  ask reconcile                 # 消费 ask_sources changelog：失效证据+标簇待复核
  ask env                       # 生效的端点配置（脱敏）
  ask reclaim [-stale]          # 物理回收 tombstone（-stale 兼收陈旧修订）
  ask job    [-job NAME]        # 摄取任务状态（queued/running/done/failed）
  ask serve  [-listen ADDR]     # HTTP 面：摄取 /v1/ingest/* + POST /v1/search(JSON) +
                                #   /v1/search/stream(SSE) + 会话 REST + 工作台 /ui/
  ask cluster list | get <id> | tidy [-dry-run] [-theta 0.55] [-max N]
  ask conflicts list | detect <clusterA> <clusterB>
  ask cites  list               # 簇→源证据边（ask_cites）
  ask session new | list | show <id> | rm <id>   # P2 会话（KV）
  ask eval-run -file ITEMS.jsonl -out RESULTS.jsonl [-judge] [-prior] [-limit N]
                                # LENS 式评测：真实管线+Closed-Book 对照+判官，可续跑

Flags:
  -server URL    cumudb base URL (default http://127.0.0.1:8480)
  -lite DIR      嵌入式存储（cumulite，Badger 单文件）：指向目录即完全不连
                 cumudb 服务端，同一套集合与 KV 语义照旧；DIR 不存在则创建
  -sources NAME  sources collection (default ask_sources; full identity wins over -ns)
  -evidence NAME evidence collection (default ask_evidence; full identity wins over -ns)
  -ns NAME       namespace scope: suite collections become ns:ask_* composite
                 identities and job/session KV keys become ns:<name>:ask:* —
                 one tenant's reuse path never sees another's (default library = bare names)

Env:
  AIGATE_BASE_URL    upstream API root INCLUDING /v1 (e.g. https://api.minimaxi.com/v1)
  AIGATE_API_KEY     bearer key for the upstream
  AIGATE_CHAT_MODEL  scorer/synthesis model (e.g. MiniMax-M3 direct, minimax/MiniMax-M3 via gateway)
  AIGATE_EMBED_MODEL embedder model; unset = offline Local embedder even when AIGATE_BASE_URL is set
  AIGATE_REASONING_SPLIT 1/0 force MiniMax reasoning_split (default: auto on minimaxi.com hosts)
  ASK_ENV            path to the suite's .env (default ./.env); LLM_* keys alias onto AIGATE_*
`

func main() {
	args := os.Args[1:]
	// Per-suite endpoint config: ./.env (or $ASK_ENV), operator's LLM_*
	// convention aliased onto AIGATE_*. Already-set env always wins.
	// Per-suite endpoint config: ./.env (or $ASK_ENV), operator's LLM_*
	// convention aliased onto AIGATE_*. Already-set env always wins.
	if err := loadDotEnv(); err != nil {
		fatal(err)
	}
	applyLLMAliases()
	server := "http://127.0.0.1:8480"
	lite := ""
	sources := ""
	evidence := ""
	namespace := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-server" && i+1 < len(args):
			server = args[i+1]
			i++
		case a == "-lite" && i+1 < len(args):
			lite = args[i+1]
			i++
		case a == "-sources" && i+1 < len(args):
			sources = args[i+1]
			i++
		case a == "-evidence" && i+1 < len(args):
			evidence = args[i+1]
			i++
		case a == "-ns" && i+1 < len(args):
			namespace = args[i+1]
			i++
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return
		default:
			rest = append(rest, a)
		}
	}
	if err := ns.Validate(namespace); err != nil {
		fatal(err)
	}
	// Namespace scoping (P3): an explicit -sources/-evidence is a full engine
	// identity and wins verbatim; otherwise the suite's own collections are
	// composed as "ns:coll" so one tenant's reuse path never sees another's.
	if sources == "" {
		sources = ns.Coll(namespace, "ask_sources")
	}
	if evidence == "" {
		evidence = ns.Coll(namespace, "ask_evidence")
	}
	clustersColl := ns.Coll(namespace, "ask_clusters")
	edgesColl := ns.Coll(namespace, "ask_weak_edges")
	citesColl := ns.Coll(namespace, "ask_cites")
	conflictsColl := ns.Coll(namespace, "ask_conflicts")
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, rest := rest[0], rest[1:]
	// Offline gates finish in seconds; the model path makes ~13 sequential
	// calls per query, so production mode gets a wider default. ASK_TIMEOUT
	// (duration, e.g. 10m) overrides either way.
	timeout := 60 * time.Second
	if os.Getenv("AIGATE_BASE_URL") != "" {
		timeout = 300 * time.Second
	}
	if v := os.Getenv("ASK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fatal(fmt.Errorf("ASK_TIMEOUT: %w", err))
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// 存储端口：-lite 指向 cumulite 嵌入式引擎（无服务端），否则连 cumudb。
	// 两者满足同一个 Port 契约，以下的 store 装配对二者逐字节相同。
	var port cumulite.Port
	if lite != "" {
		engine, err := cumulite.Open(lite)
		if err != nil {
			fatal(err)
		}
		defer engine.Close()
		port = engine
	} else {
		port = client.New(server)
	}
	c := port
	st := ingest.New(c, sources, evidence, clustersColl, namespace)

	switch cmd {
	case "put":
		fs := flag.NewFlagSet("put", flag.ExitOnError)
		title := fs.String("title", "", "title")
		typ := fs.String("type", "md", "source_type")
		uri := fs.String("uri", "", "source_uri")
		key := fs.String("key", "", "business key")
		lang := fs.String("lang", "zh", "lang")
		body := fs.String("body", "", "inline body")
		bodyFile := fs.String("body-file", "", "body file")
		_ = fs.Parse(rest)
		text := *body
		if *bodyFile != "" {
			b, err := os.ReadFile(*bodyFile)
			if err != nil {
				fatal(err)
			}
			text = string(b)
		}
		if *typ == "html" {
			text = ingest.ExtractHTML(text)
		}
		if *typ == "docx" {
			docxText, derr := ingest.ExtractDOCX([]byte(text))
			if derr != nil {
				fatal(derr)
			}
			text = docxText
		}
		if *typ == "pdf" {
			text = ingest.ExtractPDF([]byte(text))
		}
		src := source.New(*title, *typ, *uri, *key, *lang, text, nil)
		res, err := st.Put(ctx, src)
		if err != nil {
			fatal(err)
		}
		printJSON(res)
	case "ingest-jsonl":
		fs := flag.NewFlagSet("ingest-jsonl", flag.ExitOnError)
		file := fs.String("file", "", "jsonl file")
		job := fs.String("job", "default", "job key (resumable cursor)")
		mapFile := fs.String("map", "", "Path B map spec (body/title templates)")
		_ = fs.Parse(rest)
		raw, err := os.ReadFile(*file)
		if err != nil {
			fatal(err)
		}
		var spec *ingest.MapSpec
		if *mapFile != "" {
			mraw, err := os.ReadFile(*mapFile)
			if err != nil {
				fatal(err)
			}
			var m ingest.MapSpec
			if err := json.Unmarshal(mraw, &m); err != nil {
				fatal(fmt.Errorf("map spec: %w", err))
			}
			spec = &m
		}
		var recs []map[string]any
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				fatal(fmt.Errorf("jsonl: %w", err))
			}
			recs = append(recs, m)
		}
		mapFn := func(m map[string]any) (source.Source, error) {
			if spec != nil {
				return ingest.RenderMap(*spec, m)
			}
			text, _ := m["text"].(string)
			title, _ := m["title"].(string)
			key, _ := m["key"].(string)
			typ, _ := m["type"].(string)
			if typ == "" {
				typ = "jsonl"
			}
			return source.New(title, typ, "", key, "zh", text, m), nil
		}
		n, err := st.IngestJSONL(ctx, *job, recs, mapFn)
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"processed": n})
	case "ingest-files":
		fs := flag.NewFlagSet("ingest-files", flag.ExitOnError)
		dir := fs.String("dir", "", "directory of .md/.txt files")
		recursive := fs.Bool("recursive", false, "walk subdirectories")
		job := fs.String("job", "files", "job key (resumable cursor)")
		_ = fs.Parse(rest)
		if *dir == "" {
			fatal(fmt.Errorf("ingest-files: -dir required"))
		}
		n, err := st.IngestFiles(ctx, *dir, *recursive, *job)
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"processed": n})
	case "search":
		fs := flag.NewFlagSet("search", flag.ExitOnError)
		q := fs.String("q", "", "query")
		rawOut := fs.Bool("raw", false, "raw JSON")
		hopts := fs.Duration("hopts", 0, "hopTS freshness prune (e.g. 168h; 0 = off)")
		minhot := fs.Float64("minhot", 0, "neighbor min hotness (structured prune)")
		minconf := fs.Float64("minconf", 0, "neighbor min confidence (structured prune)")
		history := fs.String("history", "", "pipe-separated follow-up history (rewrite query)")
		sessionID := fs.String("session", "", "chat session id (KV): folds recent turns, appends this turn")
		priorRank := fs.Bool("prior", false, "rank candidates with the LENS B4 prior")
		l1pre := fs.Bool("l1pre", false, "narrow candidates via body_embed KNN (L1 cache)")
		_ = fs.Parse(rest)
		opt := SearchOptions{Prior: *priorRank, L1Pre: *l1pre, HopTS: *hopts, MinHot: *minhot, MinConf: *minconf, Namespace: namespace}
		if *history != "" {
			for _, h := range strings.Split(*history, "|") {
				if h = strings.TrimSpace(h); h != "" {
					opt.History = append(opt.History, h)
				}
			}
		}
		var sess *sessionStore
		if *sessionID != "" {
			sess = &sessionStore{c: c, ns: namespace}
			hist, _, herr := sessionHistory(ctx, *sess, *sessionID, 6)
			if herr != nil {
				fatal(herr)
			}
			opt.History = hist // -session 优先于 -history
		}
		ss, err := newSearchStack(ctx, c, st, sources, opt)
		if err != nil {
			fatal(err)
		}
		if os.Getenv("ASK_VERBOSE") == "1" {
			ss.dE.Verbose = func(f string, a ...any) {
				fmt.Fprintf(os.Stderr, "[search] "+f+"\n", a...)
			}
		}
		res, err := runSearch(ctx, ss, *q)
		if err != nil {
			fatal(err)
		}
		if sess != nil {
			if _, aerr := sess.appendTurn(ctx, *sessionID, *q, *q, res.Answer.Summary); aerr != nil {
				fmt.Fprintf(os.Stderr, "session append: %v\n", aerr)
			} else {
				res.Session = *sessionID
			}
		}
		if *rawOut {
			printJSON(res)
			return
		}
		ans := res.Answer
		fmt.Printf("mode=%s escalated=%v loops=%d conf=%.3f coverage=%.2f calls=%d skipped=%v reused=%v sampled=%d cluster=%s\n",
			res.Mode, res.Escalated, res.Loops, ans.Confidence, ans.Coverage, ans.LLMCalls, ans.Skipped,
			res.Reused, len(ans.Samples), res.ClusterID)
		fmt.Println(ans.Summary)
		if len(res.Citations.Refs) > 0 {
			fmt.Println("——", res.Citations.Legend)
			for _, r := range res.Citations.Refs {
				mark := ""
				if !r.Resolved {
					mark = " [?]"
				}
				fmt.Printf("  [%d]%s %s (%s) %s\n", r.Index, mark, r.SourceID, r.Span, r.Quote)
			}
		}
	case "get":
		if len(rest) < 1 {
			fatal(fmt.Errorf("get: id required"))
		}
		src, err := st.Get(ctx, rest[0])
		if err != nil {
			fatal(err)
		}
		if src == nil {
			fatal(fmt.Errorf("not found"))
		}
		printJSON(src)
	case "ensure":
		fs := flag.NewFlagSet("ensure", flag.ExitOnError)
		embed := fs.Bool("embed", false, "also backfill body_embed vectors (L1)")
		_ = fs.Parse(rest)
		colls, err := st.Ensure(ctx, edgesColl, citesColl, conflictsColl)
		if err != nil {
			fatal(err)
		}
		out := map[string]any{"collections": colls}
		if *embed {
			// One embedder table for all faces (search -l1pre / eval-run /
			// ensure -embed): ASK_EMBED=minilm wins, then aigate, then hash.
			embedFn, dims, model := embedderFor()
			n, err := st.EnsureEmbed(ctx, embedFn, dims, model, 64)
			if err != nil {
				fatal(err)
			}
			out["embedded"] = n
			out["model"] = model
		}
		printJSON(out)
	case "job":
		fs := flag.NewFlagSet("job", flag.ExitOnError)
		job := fs.String("job", "files", "job key")
		_ = fs.Parse(rest)
		d, err := st.GetJobDoc(ctx, *job)
		if err != nil {
			fatal(err)
		}
		if d.State == "" {
			fatal(fmt.Errorf("job %q not found", *job))
		}
		printJSON(d)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.1:8484", "listen address")
		verbose := fs.Bool("verbose", false, "per-request diagnostic logs (also ASK_VERBOSE=1)")
		_ = fs.Parse(rest)
		runServe(ctx, c, st, *listen, server, sources, namespace, *verbose)
	case "cites":
		sub := "list"
		if len(rest) > 0 {
			sub = rest[0]
		}
		switch sub {
		case "list":
			all, err := deep.NewCumuCiteStore(c, citesColl).List(ctx)
			if err != nil {
				fatal(err)
			}
			printJSON(all)
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	case "env":
		// Resolved endpoint config, masked — the per-suite .env face.
		base := os.Getenv("AIGATE_BASE_URL")
		key := os.Getenv("AIGATE_API_KEY")
		printJSON(map[string]any{
			"env_file":        envFilePath(),
			"env_file_loaded": fileExists(envFilePath()),
			"base_url":        base,
			"chat_model":      os.Getenv("AIGATE_CHAT_MODEL"),
			"embed_model":     os.Getenv("AIGATE_EMBED_MODEL"),
			"api_key_set":     key != "",
			"api_key_len":     len(key),
			"reasoning_split": strings.Contains(strings.ToLower(base), "minimaxi.com"),
		})
	case "reconcile":
		rep, err := st.Reconcile(ctx)
		if err != nil {
			fatal(err)
		}
		printJSON(rep)
	case "reclaim":
		fs := flag.NewFlagSet("reclaim", flag.ExitOnError)
		stale := fs.Bool("stale", false, "also reclaim stale revisions")
		_ = fs.Parse(rest)
		n, err := st.Reclaim(ctx, *stale)
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"reclaimed": n})
	case "cluster":
		store := cluster.NewCumuStore(c, clustersColl)
		sub := "list"
		if len(rest) > 0 {
			sub = rest[0]
		}
		switch sub {
		case "list":
			all, err := store.All(ctx)
			if err != nil {
				fatal(err)
			}
			printJSON(all)
		case "get":
			if len(rest) < 2 {
				fatal(fmt.Errorf("cluster get: id required"))
			}
			cl, err := store.Get(ctx, rest[1])
			if err != nil {
				fatal(err)
			}
			if cl == nil {
				fatal(fmt.Errorf("not found"))
			}
			printJSON(cl)
		case "tidy":
			// P6: cross-topic near-duplicate sweep. The write path only
			// merges within a topic key, so paraphrases asked in genuinely
			// different wordings survive as siblings; this folds them.
			fs := flag.NewFlagSet("tidy", flag.ExitOnError)
			dry := fs.Bool("dry-run", false, "report what would fold, change nothing")
			theta := fs.Float64("theta", cluster.DefaultTidyTheta, "fold line (cosine)")
			maxMerges := fs.Int("max", 0, "cap on folds this run (0 = unlimited)")
			_ = fs.Parse(rest[1:])
			// The winner's embed is recomputed from its merged query set —
			// with the SAME embedder the search stack uses (minilm/aigate in
			// production), never a different one, or reuse geometry drifts.
			edgeStore := graph.NewCumuStore(c, edgesColl)
			co := func(a, b string) float64 { return graph.CoOccurWeight(ctx, edgeStore, a, b) }
			rep, err := cluster.TidyWithCo(ctx, store, newProdStack().emb, *theta, *dry, *maxMerges, co)
			if err != nil {
				fatal(err)
			}
			printJSON(rep)
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	case "conflicts":
		store := deep.NewCumuStore(c, conflictsColl)
		sub := "list"
		if len(rest) > 0 {
			sub = rest[0]
		}
		switch sub {
		case "list":
			all, err := store.All(ctx)
			if err != nil {
				fatal(err)
			}
			printJSON(all)
		case "detect":
			if len(rest) < 3 {
				fatal(fmt.Errorf("conflicts detect: two cluster ids required"))
			}
			cs := cluster.NewCumuStore(c, clustersColl)
			a, err := cs.Get(ctx, rest[1])
			if err != nil || a == nil {
				fatal(fmt.Errorf("cluster %s: %v", rest[1], err))
			}
			b, err := cs.Get(ctx, rest[2])
			if err != nil || b == nil {
				fatal(fmt.Errorf("cluster %s: %v", rest[2], err))
			}
			cf, err := deep.DetectConflict(ctx, store, *a, *b)
			if err != nil {
				fatal(err)
			}
			printJSON(cf)
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	case "eval-demo":
		// LENS B3 evidence-quality protocol demo (offline, deterministic).
		items := []eval.Item{
			{ID: "q1", Query: "连接池最大连接数", Answer: "128", Gold: []string{"handbook"}},
			{ID: "q2", Query: "部署机房", Answer: "广州", Gold: []string{"handbook"}},
			{ID: "q3", Query: "无关问题", Answer: "42", Gold: []string{"nowhere"}},
		}
		preds := []eval.Prediction{
			{Query: items[0].Query, Answer: "连接池最大 128", SourceIDs: []string{"src:handbook"}, Resolved: 1, Refs: 1},
			{Query: items[1].Query, Answer: "广州机房", SourceIDs: []string{"src:handbook"}, Resolved: 1, Refs: 1},
			{Query: items[2].Query, Answer: "凭记忆 42", SourceIDs: []string{}, Refs: 0},
		}
		var scores []eval.ItemScore
		var cb []eval.ItemScore
		for i := range items {
			scores = append(scores, eval.Score(items[i], preds[i]))
			cb = append(cb, eval.ClosedBook(items[i], preds[i].Answer))
		}
		rep := eval.Aggregate(scores)
		cbRep := eval.Aggregate(cb)
		out := map[string]any{
			"em": rep.EM, "ev_rec": rep.EvRec, "ground": rep.Ground, "n": rep.N,
			"taxonomy":    rep.Taxonomy,
			"closed_book": map[string]any{"em": cbRep.EM, "ev_rec": cbRep.EvRec, "ground": cbRep.Ground},
			"mcnemar":     eval.Compare(scores, cb),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return

	case "session":
		runSessionCLI(ctx, c, rest, namespace)
		return
	case "eval-run":
		// LENS 式真实语料评测（R-E1）：真实管线 + Closed-Book 对照 + 判官。
		fs := flag.NewFlagSet("eval-run", flag.ExitOnError)
		file := fs.String("file", "", "items jsonl (eval.Item: id/query/answer=reference/gold_sources)")
		out := fs.String("out", "", "results jsonl (resumable; report aggregates the whole file)")
		judgeOn := fs.Bool("judge", false, "LLM judges Correct against the reference (needs endpoint)")
		priorRank := fs.Bool("prior", false, "rank candidates with the LENS B4 prior")
		l1pre := fs.Bool("l1pre", false, "narrow candidates per item via body_embed KNN (L1 cache)")
		limit := fs.Int("limit", 0, "max new items this run (0 = all remaining)")
		_ = fs.Parse(rest)
		if *file == "" || *out == "" {
			fatal(fmt.Errorf("eval-run: -file and -out required"))
		}
		// Per-item deadlines inside evalRun; the shared ctx's whole-process
		// budget must not cap a multi-item batch.
		if err := evalRun(context.Background(), c, st, sources, namespace, *file, *out, *judgeOn, *priorRank, *l1pre, *limit); err != nil {
			fatal(err)
		}

	case "delete":
		if len(rest) < 1 {
			fatal(fmt.Errorf("delete: id required"))
		}
		if err := st.Delete(ctx, rest[0]); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"deleted": rest[0]})
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// embedderFor resolves the content-vector embedder by the same rule as
// search: explicit AIGATE_EMBED_MODEL over AIGATE_BASE_URL, else the offline
// Local hash embedder.
func embedderFor() (ingest.EmbedderFn, int, string) {
	// 纯 Go MiniLM（embed-notes §8）：ASK_EMBED=minilm 显式开启；权重直接
	// 复用 Sirchmunk 的模型缓存，向量空间与其语义缓存索引一致（384 维）。
	if os.Getenv("ASK_EMBED") == "minilm" {
		if minilm.Available() {
			emb := minilm.New(minilm.DefaultDir())
			return emb.Embed, emb.Dims(), "minilm-l12-384"
		}
		if os.Getenv("ASK_VERBOSE") == "1" {
			fmt.Fprintln(os.Stderr, "[embedderFor] ASK_EMBED=minilm 但权重缺席——退回 local-hash-64（语料向量降级）")
		}
	}
	if base := os.Getenv("AIGATE_BASE_URL"); base != "" && os.Getenv("AIGATE_EMBED_MODEL") != "" {
		fe := &llm.AigateEmbedder{
			BaseURL: base,
			APIKey:  os.Getenv("AIGATE_API_KEY"),
			Model:   os.Getenv("AIGATE_EMBED_MODEL"),
			N:       64,
		}
		return fe.Embed, fe.Dims(), os.Getenv("AIGATE_EMBED_MODEL")
	}
	loc := cluster.Local{N: 64}
	return loc.Embed, loc.Dims(), "local-hash-64"
}

// orderByKNN narrows the active-source list to the KNN hits (distance order).
func orderByKNN(list []source.Source, knn []map[string]any) []source.Source {
	byID := map[string]source.Source{}
	for _, s := range list {
		byID[s.ID] = s
	}
	var out []source.Source
	for _, d := range knn {
		id, _ := d["_id"].(string)
		if s, ok := byID[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ask:", err)
	os.Exit(1)
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

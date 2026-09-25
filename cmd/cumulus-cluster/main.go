// Command cumulus-cluster is the cognitive-search suite CLI: ingest sources and run the
// FAST/DEEP search path against a cumulite store directory.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/willove/cumulite"

	"github.com/willove/cumulus/internal/adapt"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/eval"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/ns"
	"github.com/willove/cumulus/internal/source"
)

const usage = `cumulus-cluster — cognitive search suite (on cumulite)

Usage:
  cumulus-cluster put    -title T [-type md] [-uri U] [-key K] [-lang zh] -body-file F
  cumulus-cluster put    -title T -body "text"
  cumulus-cluster ingest-jsonl -file data.jsonl [-job NAME] [-map map.json]
  cumulus-cluster ingest-adapt -dir DIR [-recursive] [-job NAME] [-id F] [-title F] [-body F] [-extra a,b]
                                               # 异构语料适配：json/jsonl/csv/txt 自动判别+字段映射
  cumulus-cluster ingest-files -dir D [-recursive] [-job NAME] [-candidates scan.json]  # P9：-candidates 只吃扫描清单
  cumulus-cluster scan -dir D [-recursive] [-limit N] [-newer-than 168h] [-q "主题"] [-out scan.json]
                                            # 摄取候选发现（P9）：规则清单 + LLM 主题排名（opt-in），不开库
  cumulus-cluster search -q "query" [-session ID] [-raw] [-hopts 168h]
  cumulus-cluster get    <id>
  cumulus-cluster delete <id>
  cumulus-cluster ensure [-embed]             # 声明集合（-embed 兼补内容向量）
  cumulus-cluster reconcile                   # 消费 clus_sources changelog：失效证据+标簇待复核
  cumulus-cluster env                       # 生效的端点配置（脱敏）
  cumulus-cluster model [status|install|verify]   # MiniLM 权重：状态/下载（魔搭，~464MB）/加载验证；
                                            #   权重缺席且 CLUS_EMBED=minilm 时启动即提示安装
  cumulus-cluster mcp [-url ADDR]           # MCP stdio 代理 → serve 的 POST /mcp
                                            #   （search/list_clusters/get_cluster 三工具；代理不开库）
  cumulus-cluster reclaim [-stale]            # 物理回收 tombstone（-stale 兼收陈旧修订）
  cumulus-cluster job    [-job NAME]          # 摄取任务状态（queued/running/done/failed）
  cumulus-cluster serve  [-listen ADDR]       # HTTP 面：摄取 /v1/ingest/* + POST /v1/search(JSON) +
                                              #   /v1/search/stream(SSE) + 会话 REST + 工作台 /ui/
  cumulus-cluster bucket list | new <name> [label] [note] | rm <name> | show <name>
  cumulus-cluster cluster list | get <id> | tidy [-dry-run] [-theta 0.55] [-max N]
  cumulus-cluster conflicts list | detect <clusterA> <clusterB>
  cumulus-cluster cites  list                 # 簇→源证据边（clus_cites）
  cumulus-cluster session new | list | show <id> | rm <id>   # P2 会话（KV）
  cumulus-cluster eval-run -file ITEMS.jsonl -out RESULTS.jsonl [-judge] [-prior] [-limit N]
                                              # LENS 式评测：真实管线+Closed-Book 对照+判官，可续跑

Flags:
  -data DIR      存储目录（cumulite 嵌入式引擎，Badger 单文件；默认 var/cumulus-cluster，
                 不存在则创建）。Badger 对该目录取排他锁：同一 store 同时只能
                 有一个进程打开，serve 与 CLI 不能指向同一目录并跑
  -sources NAME  sources collection (default clus_sources; full identity wins over -ns)
  -evidence NAME evidence collection (default clus_evidence; full identity wins over -ns)
  -ns NAME       bucket selector (same thing): suite collections become ns:clus_* composite
                 identities and job/session KV keys become ns:<name>:clus:* —
                 one tenant's reuse path never sees another's (default library = bare names)

Env:
  AIGATE_BASE_URL    upstream API root INCLUDING /v1 (e.g. https://api.minimaxi.com/v1)
  AIGATE_API_KEY     bearer key for the upstream
  AIGATE_CHAT_MODEL  scorer/synthesis model (e.g. MiniMax-M3 direct, minimax/MiniMax-M3 via gateway)
  AIGATE_EMBED_MODEL embedder model; unset = offline Local embedder even when AIGATE_BASE_URL is set
  AIGATE_REASONING_SPLIT 1/0 force MiniMax reasoning_split (default: auto on minimaxi.com hosts)
  CLUS_ENV            path to the suite's .env (default ./.env); LLM_* keys alias onto AIGATE_*
  CLUS_OFFLINE        1 = pin the offline stubs (no chat client, no remote embedder) even when an
                      endpoint is configured — gate harnesses use this so a developer's ambient
                      LLM_BASE_URL/AIGATE_* cannot route deterministic gates at a live model
  CLUS_MCS_WINDOW / _SAMPLES_PER_ROUND / _ROUNDS / _TOP_SEEDS / _SIGMA /
  CLUS_MCS_SMALL_FILE / _MAX_EVIDENCE
                      Monte-Carlo sampler overrides (SSOT 3.3 参数配置驱动)；缺省用内置默认值
  CLUS_FSYNC          1 = fsync every transaction (default off; measured +0.05 ms/txn —
                     only machine crash needs it, process death does not)
`

func main() {
	args := os.Args[1:]
	// Per-suite endpoint config: ./.env (or $CLUS_ENV), operator's LLM_*
	// convention aliased onto AIGATE_*. Already-set env always wins.
	if err := loadDotEnv(); err != nil {
		fatal(err)
	}
	applyLLMAliases()
	data := "var/cumulus-cluster"
	sources := ""
	evidence := ""
	namespace := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-data" && i+1 < len(args):
			data = args[i+1]
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
	// Namespace scoping: an explicit -sources/-evidence is a full engine
	// identity and wins verbatim; otherwise the suite's own collections are
	// composed as "ns:coll" so one tenant's reuse path never sees another's.
	if sources == "" {
		sources = ns.Coll(namespace, "clus_sources")
	}
	if evidence == "" {
		evidence = ns.Coll(namespace, "clus_evidence")
	}
	clustersColl := ns.Coll(namespace, "clus_clusters")
	edgesColl := ns.Coll(namespace, "clus_weak_edges")
	citesColl := ns.Coll(namespace, "clus_cites")
	conflictsColl := ns.Coll(namespace, "clus_conflicts")
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, rest := rest[0], rest[1:]
	// Offline gates finish in seconds; the model path makes ~13 sequential
	// calls per query, so production mode gets a wider default. CLUS_TIMEOUT
	// (duration, e.g. 10m) overrides either way.
	timeout := 60 * time.Second
	if os.Getenv("AIGATE_BASE_URL") != "" {
		timeout = 300 * time.Second
	}
	if v := os.Getenv("CLUS_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fatal(fmt.Errorf("CLUS_TIMEOUT: %w", err))
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// First-run weight check: the semantic embedder is requested but the
	// weights are absent → say so, and offer the download on a TTY. The
	// model command drives its own flow; env never needs weights.
	if cmd != "model" && cmd != "env" {
		maybeOfferModelInstall(ctx)
	}
	// 存储：cumulite 嵌入式引擎（Badger 单文件）。env/mcp/scan/model 只打印
	// 配置、做 HTTP 代理、扫文件系统或装模型权重，不碰存储；其余命令都用
	// -data 给的目录（默认 var/cumulus-cluster）。
	var c cumulite.Port
	var st *ingest.Store
	if cmd != "env" && cmd != "mcp" && cmd != "scan" && cmd != "model" {
		var opts []cumulite.Option
		if os.Getenv("CLUS_FSYNC") == "1" {
			// Durability opt-in: fsync每笔事务。实测代价 ≈0.05 ms/事务
			// （cumulite engine_durability_test.go），1.4 万篇回填约 +1s。
			opts = append(opts, cumulite.WithSyncWrites())
		}
		engine, err := cumulite.Open(data, opts...)
		if err != nil {
			fatal(err)
		}
		defer engine.Close()
		c = engine
		st = ingest.New(c, sources, evidence, clustersColl, namespace)
	}

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
	case "ingest-adapt":
		// Heterogeneous corpus files: JSON / JSON-lines / CSV / text are
		// autodetected and field-mapped (adapt package). Flags come before the
		// subcommand's positional list, like the other subcommands.
		fs := flag.NewFlagSet("ingest-adapt", flag.ExitOnError)
		dir := fs.String("dir", "", "directory of corpus files")
		recursive := fs.Bool("recursive", false, "walk subdirectories")
		job := fs.String("job", "adapt", "job key (resumable cursor)")
		fID := fs.String("id", "", "record field holding the business key (default autodetect)")
		fTitle := fs.String("title", "", "record field holding the title (default autodetect)")
		fBody := fs.String("body", "", "record field holding the text (default autodetect)")
		fExtra := fs.String("extra", "", "comma-separated record fields copied into meta")
		// Flags may follow the file list; reorder so the flag package sees them.
		_ = fs.Parse(flagsFirst(rest, "recursive"))
		var files []string
		if *dir != "" {
			var werr error
			files, werr = filepath.Glob(filepath.Join(*dir, "*"))
			if werr != nil {
				fatal(werr)
			}
			if !*recursive {
				kept := files[:0]
				for _, f := range files {
					if st, serr := os.Stat(f); serr == nil && !st.IsDir() {
						kept = append(kept, f)
					}
				}
				files = kept
			}
		}
		// Explicit files after the flags win over -dir.
		for _, a := range fs.Args() {
			files = append(files, a)
		}
		if len(files) == 0 {
			fatal(fmt.Errorf("ingest-adapt: -dir DIR or explicit files required"))
		}
		sort.Strings(files)
		var extra []string
		for _, e := range strings.Split(*fExtra, ",") {
			if e = strings.TrimSpace(e); e != "" {
				extra = append(extra, e)
			}
		}
		n, err := st.IngestAdapted(ctx, files, adapt.Fields{
			ID: *fID, Title: *fTitle, Body: *fBody, Extra: extra,
		}, *job)
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"ingested": n, "files": len(files)})
	case "ingest-files":
		fs := flag.NewFlagSet("ingest-files", flag.ExitOnError)
		dir := fs.String("dir", "", "directory of .md/.txt files")
		recursive := fs.Bool("recursive", false, "walk subdirectories")
		job := fs.String("job", "files", "job key (resumable cursor)")
		candidates := fs.String("candidates", "", "ingest only the files in this scan report (JSON; P9)")
		_ = fs.Parse(rest)
		var n int
		var err error
		if *candidates != "" {
			cf, lerr := ingest.LoadCandidateFile(*candidates)
			if lerr != nil {
				fatal(lerr)
			}
			n, err = st.IngestCandidates(ctx, cf.Paths(), *job)
		} else {
			if *dir == "" {
				fatal(fmt.Errorf("ingest-files: -dir (or -candidates) required"))
			}
			n, err = st.IngestFiles(ctx, *dir, *recursive, *job)
		}
		if err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"processed": n})
	case "scan":
		// P9 candidate discovery: rules over a directory, optional LLM topic
		// rank. Touches no store — a pure pre-ingest step (like env).
		fs := flag.NewFlagSet("scan", flag.ExitOnError)
		dir := fs.String("dir", "", "directory to discover")
		recursive := fs.Bool("recursive", false, "walk subdirectories")
		limit := fs.Int("limit", 0, "cap candidates (0 = no cap; stratified by extension)")
		maxSize := fs.Int64("max-size", 0, "per-file cap in bytes (0 = 8 MiB default)")
		newerThan := fs.Duration("newer-than", 0, "only files newer than this age (e.g. 168h; 0 = off)")
		rankQuery := fs.String("q", "", "rank candidates for this topic via the LLM (opt-in)")
		out := fs.String("out", "", "write the trimmed candidate report (JSON) for ingest-files -candidates")
		_ = fs.Parse(rest)
		if *dir == "" {
			fatal(fmt.Errorf("scan: -dir required"))
		}
		rep, serr := ingest.ScanDir(*dir, ingest.ScanOptions{
			Recursive: *recursive, Limit: *limit, MaxSize: *maxSize, NewerThan: *newerThan,
		})
		if serr != nil {
			fatal(serr)
		}
		if *rankQuery != "" {
			if rerr := ingest.ApplyRank(ctx, &rep, *rankQuery, scanRankFunc(newProdStack().chat)); rerr != nil {
				fmt.Fprintf(os.Stderr, "scan: LLM 排名失败，保留规则序：%v\n", rerr)
			}
		}
		if *out != "" {
			cf := ingest.CandidateFile{Dir: rep.Dir, Candidates: rep.Candidates}
			b, merr := json.MarshalIndent(cf, "", "  ")
			if merr != nil {
				fatal(merr)
			}
			if werr := os.WriteFile(*out, b, 0o644); werr != nil {
				fatal(werr)
			}
		}
		printJSON(rep)
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
		if os.Getenv("CLUS_VERBOSE") == "1" {
			ss.dE.Verbose = func(f string, a ...any) {
				fmt.Fprintf(os.Stderr, "[search] "+f+"\n", a...)
			}
		}
		res, err := runSearch(ctx, ss, *q)
		if err != nil {
			fatal(err)
		}
		if sess != nil {
			if _, aerr := sess.appendTurnDurable(ctx, *sessionID, *q, *q, res.Answer.Summary); aerr != nil {
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
			// ensure -embed): CLUS_EMBED=minilm wins, then aigate, then hash.
			embedFn, dims, model, eerr := embedderFor()
			if eerr != nil {
				fatal(eerr)
			}
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
	case "model":
		// The embedder seat: status / install (ModelScope) / verify (run the
		// weights). Touches no store. Subcommand first, flags after it — the
		// flag package stops at the first positional.
		fs := flag.NewFlagSet("model", flag.ExitOnError)
		yes := fs.Bool("y", false, "skip the download confirmation (non-interactive/CI)")
		dir := fs.String("dir", minilm.DefaultDir(), "model directory (default $CLUS_MODEL_DIR or ~/.cumulus/models/<model>)")
		sub := "status"
		args := rest
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			sub = args[0]
			args = args[1:]
		}
		_ = fs.Parse(args)
		switch sub {
		case "status":
			printJSON(collectModelStatus(*dir))
		case "install":
			if minilm.Available() && *dir == minilm.DefaultDir() {
				printJSON(map[string]any{"installed": true, "dir": *dir, "note": "already installed; delete the dir to reinstall"})
				return
			}
			if !*yes && !isInteractive(os.Stdin) {
				fatal(fmt.Errorf("model install: refusing a ~464MB download without -y (non-interactive)"))
			}
			if !*yes && !confirmDownload(os.Stdin, os.Stderr, fmt.Sprintf("model install: download ~464MB from ModelScope into %s?", *dir)) {
				fmt.Fprintln(os.Stderr, "model install: cancelled")
				return
			}
			m, err := installModel(ctx, *dir, func(p minilm.Progress) {
				fmt.Fprintf(os.Stderr, "\rmodel install: %s %d/%d MB", p.File, p.Done>>20, p.Total>>20)
			})
			if err != nil {
				fatal(err)
			}
			fmt.Fprintln(os.Stderr, "\nmodel install: verifying weights……")
			rep, verr := verifyModel(ctx, *dir)
			if verr != nil {
				fatal(verr)
			}
			rep["files"] = m.Files
			printJSON(rep)
		case "verify":
			rep, err := verifyModel(ctx, *dir)
			if err != nil {
				fatal(err)
			}
			printJSON(rep)
		default:
			fatal(fmt.Errorf("model: unknown subcommand %q (status|install|verify)", sub))
		}
	case "mcp":
		// MCP stdio proxy: forwards JSON-RPC to a running serve's POST /mcp.
		// Owns no store (Badger 目录排他锁——代理必须是第二个进程也不开库）。
		fs := flag.NewFlagSet("mcp", flag.ExitOnError)
		url := fs.String("url", defaultMCPURL(), "serve 的 MCP 端点（含 /mcp）")
		_ = fs.Parse(rest)
		// Long-running like serve: the whole-process CLI budget must not cap
		// the proxy — under it, every in-flight JSON-RPC was cancelled and the
		// proxy fatal'd when the timeout fired (an MCP client would see its
		// server die mid-session after 60s/300s). Signals are the lifecycle.
		pctx, pcancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer pcancel()
		if err := runMCPProxy(pctx, *url, os.Stdin, os.Stdout, os.Stderr); err != nil {
			fatal(err)
		}
	case "bucket":
		runBucketCLI(ctx, c, rest)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		listen := fs.String("listen", "127.0.0.1:8484", "listen address")
		verbose := fs.Bool("verbose", false, "per-request diagnostic logs (also CLUS_VERBOSE=1)")
		_ = fs.Parse(rest)
		// serve is long-running: the whole-process CLI budget above (60s, or
		// 300s with an endpoint) must not cap it — runServe's graceful-shutdown
		// goroutine waits on this ctx's Done, so under the budget the server
		// silently self-Shutdown and the process exited 0 mid-serving. Its
		// lifecycle is signals instead: Ctrl-C/SIGTERM take the graceful path
		// (same reasoning as eval-run's Background ctx further down).
		sctx, scancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer scancel()
		runServe(sctx, c, st, *listen, sources, namespace, *verbose, data)
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
			"store":           data, // resolved only; env never opens it
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
			// Cross-topic near-duplicate sweep. The write path only
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
			// P4: the contested pair also bars traversal between the two
			// clusters (bidirectional barrier edges) — expansion then
			// refuses to serve one side as evidence for the other.
			edgeStore := graph.NewCumuStore(c, edgesColl)
			if berr := graph.LinkBarrier(ctx, edgeStore, a.ID, b.ID, cf.Reason); berr != nil {
				fatal(berr)
			}
			printJSON(cf)
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	case "eval-demo":
		// Evidence-quality protocol demo (offline, deterministic).
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
		// LENS 式真实语料评测：真实管线 + Closed-Book 对照 + 判官。
		fs := flag.NewFlagSet("eval-run", flag.ExitOnError)
		file := fs.String("file", "", "items jsonl (eval.Item: id/query/answer=reference/gold_sources)")
		out := fs.String("out", "", "results jsonl (resumable; report aggregates the whole file)")
		judgeOn := fs.Bool("judge", false, "LLM judges Correct against the reference (needs endpoint)")
		priorRank := fs.Bool("prior", false, "rank candidates with the LENS B4 prior")
		l1pre := fs.Bool("l1pre", false, "narrow candidates per item via body_embed KNN (L1 cache)")
		limit := fs.Int("limit", 0, "max new items this run (0 = all remaining)")
		tag := fs.String("tag", "", "scoreboard label for this run (default: items file name)")
		_ = fs.Parse(rest)
		if *file == "" || *out == "" {
			fatal(fmt.Errorf("eval-run: -file and -out required"))
		}
		if *tag == "" {
			*tag = filepath.Base(*file)
		}
		// Per-item deadlines inside evalRun; the shared ctx's whole-process
		// budget must not cap a multi-item batch.
		if err := evalRun(context.Background(), c, st, sources, namespace, *file, *out, *judgeOn, *priorRank, *l1pre, *limit, *tag); err != nil {
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
func embedderFor() (ingest.EmbedderFn, int, string, error) {
	// 纯 Go MiniLM：CLUS_EMBED=minilm 显式开启；权重直接
	// 复用 Sirchmunk 的模型缓存，向量空间与其语义缓存索引一致（384 维）。
	// CLUS_OFFLINE does NOT suppress this seat: the weights are a local file,
	// so an offline gate can still exercise (and strictly fail on) them — only
	// the remote aigate embedder below is pinned off.
	if os.Getenv("CLUS_EMBED") == "minilm" {
		emb, err := minilm.Resolve()
		if err != nil {
			return nil, 0, "", err // strict: CLUS_MINILM_REQUIRE=1 fails hard
		}
		if emb != nil {
			return emb.Embed, emb.Dims(), "minilm-l12-384", nil
		}
		if os.Getenv("CLUS_VERBOSE") == "1" {
			fmt.Fprintln(os.Stderr, "[embedderFor] CLUS_EMBED=minilm 但权重缺席——退回 local-hash-64（语料向量降级）")
		}
	}
	if base := os.Getenv("AIGATE_BASE_URL"); base != "" && os.Getenv("AIGATE_EMBED_MODEL") != "" && !offlineForced() {
		fe := &llm.AigateEmbedder{
			BaseURL: base,
			APIKey:  os.Getenv("AIGATE_API_KEY"),
			Model:   os.Getenv("AIGATE_EMBED_MODEL"),
			N:       64,
		}
		return fe.Embed, fe.Dims(), os.Getenv("AIGATE_EMBED_MODEL"), nil
	}
	loc := cluster.Local{N: 64}
	return loc.Embed, loc.Dims(), "local-hash-64", nil
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
	fmt.Fprintln(os.Stderr, "cumulus-cluster:", err)
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

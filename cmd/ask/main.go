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
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/ingest"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/llm"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
	"github.com/cumubase/cumudb/pkg/client"
)

const usage = `ask — cognitive search suite (on cumudb)

Usage:
  ask put    -title T [-type md] [-uri U] [-key K] [-lang zh] -body-file F
  ask put    -title T -body "text"
  ask ingest-jsonl -file data.jsonl [-job NAME] [-map map.json]
  ask ingest-files -dir D [-recursive] [-job NAME]
  ask search -q "query" [-raw] [-hopts 168h]
  ask get    <id>
  ask delete <id>
  ask ensure [-embed]           # 声明集合（-embed 兼补内容向量）
  ask reconcile                 # 消费 ask_sources changelog：失效证据+标簇待复核
  ask env                       # 生效的端点配置（脱敏）
  ask reclaim [-stale]          # 物理回收 tombstone（-stale 兼收陈旧修订）
  ask job    [-job NAME]        # 摄取任务状态（queued/running/done/failed）
  ask serve  [-listen ADDR]     # HTTP 摄取面（/health · /v1/ingest/*）
  ask cluster list | get <id>
  ask conflicts list | detect <clusterA> <clusterB>
  ask cites  list               # 簇→源证据边（ask_cites）

Flags:
  -server URL    cumudb base URL (default http://127.0.0.1:8480)
  -sources NAME  sources collection (default ask_sources)
  -evidence NAME evidence collection (default ask_evidence)

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
	envFile := os.Getenv("ASK_ENV")
	if envFile == "" {
		envFile = ".env"
	}
	if err := loadDotEnv(envFile); err != nil {
		fatal(err)
	}
	applyLLMAliases()
	server := "http://127.0.0.1:8480"
	sources := "ask_sources"
	evidence := "ask_evidence"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-server" && i+1 < len(args):
			server = args[i+1]
			i++
		case a == "-sources" && i+1 < len(args):
			sources = args[i+1]
			i++
		case a == "-evidence" && i+1 < len(args):
			evidence = args[i+1]
			i++
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return
		default:
			rest = append(rest, a)
		}
	}
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
	c := client.New(server)
	st := ingest.New(c, sources, evidence)

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
		history := fs.String("history", "", "pipe-separated follow-up history (rewrite query)")
		priorRank := fs.Bool("prior", false, "rank candidates with the LENS B4 prior")
		l1pre := fs.Bool("l1pre", false, "narrow candidates via body_embed KNN (L1 cache)")
		_ = fs.Parse(rest)
		list, err := st.ActiveSources(ctx)
		if err != nil {
			fatal(err)
		}
		// Production path via aigate (D6): scorer/embedder/analyze/synthesize.
		// Offline stubs keep the gates deterministic without AIGATE_BASE_URL.
		var scorer mcs.Scorer = mcs.KeywordScorer{}
		var emb cluster.Embedder = cluster.Local{N: 64}
		var analyzer fast.Analyzer
		var synth fast.Synthesizer
		var expander fast.KeywordExpander
		var rewriter deep.HistoryRewriter
		var chatClient *llm.ChatClient
		if base := os.Getenv("AIGATE_BASE_URL"); base != "" {
			split := strings.Contains(strings.ToLower(base), "minimaxi.com")
			if v := os.Getenv("AIGATE_REASONING_SPLIT"); v != "" {
				split = v == "1" || strings.EqualFold(v, "true")
			}
			chat := &llm.ChatClient{
				BaseURL:        base,
				APIKey:         os.Getenv("AIGATE_API_KEY"),
				Model:          envOr("AIGATE_CHAT_MODEL", "mimo/cascade-pro"),
				Caller:         "ask",
				ReasoningSplit: split,
			}
			chatClient = chat
			scorer = &llm.AigateScorer{Client: chat}
			analyzer = &llm.AigateAnalyzer{Client: chat}
			synth = &llm.AigateSynthesizer{Client: chat}
			expander = &llm.AigateKeywordExpander{Client: chat, Levels: 3}
			rewriter = &llm.AigateHistoryRewriter{Client: chat}
			// Embeddings switch only on an explicit AIGATE_EMBED_MODEL: the
			// gateway's chat surface is the proven path, and a silent embed
			// probe against a chat-only gateway would fail every search.
			if os.Getenv("AIGATE_EMBED_MODEL") != "" {
				emb = &llm.AigateEmbedder{
					BaseURL: base,
					APIKey:  os.Getenv("AIGATE_API_KEY"),
					Model:   os.Getenv("AIGATE_EMBED_MODEL"),
					N:       64,
				}
			}
		}
		_ = rewriter
		// L1 candidate prefilter: KNN over body_embed (opt-in; empty/error
		// falls back to the full L0 set — 索引是缓存，不是契约). D7 lazy
		// path: a missing index materializes once, bounded, then retries.
		if *l1pre {
			embedFn, dims, embedModel := embedderFor()
			knnOnce := func() (*client.KNNResult, error) {
				qv, err := embedFn(ctx, []string{*q})
				if err != nil || len(qv) != 1 {
					return nil, fmt.Errorf("embed: %w", err)
				}
				return c.KNN(ctx, sources, client.KNNRequest{
					Field: "body_embed", Vector: qv[0], K: 8, Metric: "cosine",
					Index:  "ask_body_embed",
					Filter: map[string]any{"status": source.StatusActive},
				})
			}
			knn, err := knnOnce()
			if err != nil {
				if _, berr := st.EnsureEmbed(ctx, embedFn, dims, embedModel, 64); berr == nil {
					knn, err = knnOnce()
				}
			}
			if err == nil && knn != nil && len(knn.Documents) > 0 {
				if narrowed := orderByKNN(list, knn.Documents); len(narrowed) > 0 {
					list = narrowed
				}
			}
		}
		fe := fast.New(scorer)
		fe.UsePrior = *priorRank
		fe.Analyzer, fe.Synth, fe.Expander = analyzer, synth, expander
		kbE := kb.New(fe, cluster.NewCumuStore(c, "ask_clusters"), emb)
		kbE.Edges = graph.NewCumuStore(c, "ask_weak_edges")
		kbE.Cites = deep.NewCumuCiteStore(c, "ask_cites")
		kbE.HopTS = *hopts
		dE := deep.New(kbE, deep.NewCumuStore(c, "ask_conflicts"))
		dE.Scorer = scorer
		dE.Synth = synth
		if *history != "" {
			var hist []string
			for _, h := range strings.Split(*history, "|") {
				if h = strings.TrimSpace(h); h != "" {
					hist = append(hist, h)
				}
			}
			dE.History = hist
			dE.HistoryRewriter = rewriter
		}
		res, err := dE.Ask(ctx, *q, list)
		if err != nil {
			fatal(err)
		}
		if chatClient != nil {
			res.Tokens = chatClient.TotalTokens() // B9 per-query accounting
		}
		ans := res.Answer
		if !res.Reused && len(ans.Samples) > 0 && ans.SourceID != "" && !ans.Skipped {
			top := ans.Samples[0]
			_, _ = st.MarkEvidence(ctx, ans.SourceID, top.Start, top.End, top.Score, top.Reasoning, trim(top.Content, 200))
		}
		if *rawOut {
			printJSON(res)
			return
		}
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
		colls, err := st.Ensure(ctx)
		if err != nil {
			fatal(err)
		}
		out := map[string]any{"collections": colls}
		if *embed {
			dims := 64
			var embedFn ingest.EmbedderFn
			// Same rule as search: embeddings need the explicit model env.
			if os.Getenv("AIGATE_EMBED_MODEL") != "" && os.Getenv("AIGATE_BASE_URL") != "" {
				fe := &llm.AigateEmbedder{
					BaseURL: os.Getenv("AIGATE_BASE_URL"),
					APIKey:  os.Getenv("AIGATE_API_KEY"),
					Model:   os.Getenv("AIGATE_EMBED_MODEL"),
					N:       64,
				}
				embedFn, dims = fe.Embed, fe.Dims()
			} else {
				loc := cluster.Local{N: 64}
				embedFn = loc.Embed
			}
			n, err := st.EnsureEmbed(ctx, embedFn, dims, envOr("AIGATE_EMBED_MODEL", "local-hash-64"), 64)
			if err != nil {
				fatal(err)
			}
			out["embedded"] = n
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
		_ = fs.Parse(rest)
		runServe(ctx, c, st, *listen, server)
	case "cites":
		sub := "list"
		if len(rest) > 0 {
			sub = rest[0]
		}
		switch sub {
		case "list":
			all, err := deep.NewCumuCiteStore(c, "ask_cites").List(ctx)
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
			"env_file":        envFile,
			"env_file_loaded": fileExists(envFile),
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
		store := cluster.NewCumuStore(c, "ask_clusters")
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
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	case "conflicts":
		store := deep.NewCumuStore(c, "ask_conflicts")
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
			cs := cluster.NewCumuStore(c, "ask_clusters")
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
			"closed_book": map[string]any{"em": cbRep.EM, "ev_rec": cbRep.EvRec, "ground": cbRep.Ground},
			"mcnemar":     eval.Compare(scores, cb),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return

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

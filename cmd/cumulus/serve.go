package main

import (
	gocontext "context"
	"flag"
	"fmt"
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/api"
	"github.com/willove/cumulus/internal/auth"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/docgen"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/store"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// serve.go —— 自托管服务的**装配**（语料、索引、可选件、路由、监听）。
//
// （拆文件的理由：main.go 原本同时装"子命令分发"与"服务怎么装配"。装配是最容易
// 静默失手的地方（某可选件没装 → 读数里看着像在工作），单独成文件才好对着读。）

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8485", "listen address")
	data := fs.String("data", "", "store directory (empty = in-memory)")
	corpusDir := fs.String("corpus", "", "directory with .jsonl files to import when the store is empty")
	synthFlag := fs.String("synth", "offline", "synthesis backend: offline | llm")
	embedFlag := fs.String("embed", "off", "embedding backend: off | minilm")
	watchDir := fs.String("watch", "", "directory to watch for new files (txt/md/jsonl)")
	priorOn := fs.Bool("prior", false, "document-level multi-signal rerank (cumulus prior: lexical without length norm + title + article struct)")
	abstainOn := fs.Bool("abstain", false, "zero-LLM fail-prediction head (cumulus abstain: early refusal / forced escalation)")
	topk := fs.Int("topk", 9, "retrieval top-k; 实测依据见 defaultKnobs")
	width := fs.Int("width", 400, "evidence window width (runes); 实测下限见 defaultKnobs")
	// realm = 语料集合的**物理分区**（集合名 documents/<realm>）。多项目共用一个
	// 实例时用它隔开；空 = 默认集合（单机老路径不变）。
	realmFlag := fs.String("realm", "", "corpus realm (collection namespace: documents/<realm>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*data, *data == "")
	if err != nil {
		return err
	}
	ctx := gocontext.Background()
	docs, err := corpus.Load(ctx, st)
	if err != nil {
		return err
	}
	// -corpus 给了就导，**不看出不出空**：导入是幂等的（内容寻址 + 规范
	// 化后同内容同 id，重导即去重 upsert）。曾经"仅空库才导"的守卫造的
	// 孽：库里躺一篇残留文档，整个语料导入被静默跳过，服务拿一篇文档
	// 回答"证据不足"（真跑踩过，用户当场抓住退化）。
	if *corpusDir != "" {
		n, err := corpus.ImportDir(ctx, st, *corpusDir)
		if err != nil {
			return err
		}
		fmt.Printf("corpus: imported %d docs from %s (idempotent re-scan)\n", n, *corpusDir)
		docs, err = corpus.Load(ctx, st)
		if err != nil {
			return err
		}
	}
	if len(docs) == 0 && *corpusDir == "" && *watchDir == "" {
		return fmt.Errorf("serve: store is empty and neither -corpus nor -watch given; nothing to answer from")
	}
	if len(docs) == 0 {
		fmt.Println("serve: starting with an empty store (watch/ingest will fill it)")
	}
	synthFn, streamFn, synthLabel, err := pickSynth(*synthFlag)
	if err != nil {
		return err
	}
	// 摄入面装配：store 是语料的家，索引是它的投影（摄入后热重建）
	srv := api.NewWithStore(st, synthFn, *topk, *width)
	srv.Realm = *realmFlag
	srv.StreamSynth = streamFn
	// 凭证表：`CUMULUS_KEYS=realm=key,realm2=key2`（realm 由**凭证推导**，
	// 不信请求体里的自称）。没配 = 宽容（本地开发），但会打印醒目提示——
	// 多租户部署忘了配 key 是危险状态，不该静默。
	keys, kerr := auth.ParseKeyringSpec(os.Getenv("CUMULUS_KEYS"))
	if kerr != nil {
		return fmt.Errorf("CUMULUS_KEYS: %w", kerr)
	}
	srv.Keys = keys
	// 会话事件保留期：启动清一次 + 每小时清一次（落库的是提问原文与思考过程，
	// 不是该永久保存的东西；到期就删，不等谁来点）。
	if st != nil {
		api.PruneSessions(ctx, st)
		go func() {
			t := time.NewTicker(pruneEvery())
			defer t.Stop()
			for range t.C {
				api.PruneSessions(gocontext.Background(), st)
			}
		}()
	}
	if keys.Empty() {
		fmt.Println("serve: 未配 CUMULUS_KEYS —— 所有请求放行（仅限本地/单人使用）")
	} else {
		fmt.Printf("serve: 凭证表已启用，realm=%v（key 不回显）\n", keys.Realms())
	}
	if _, err := srv.Rebuild(ctx); err != nil {
		return err
	}
	// 查询侧三件（cumulus 的 IDF 加权关键词级 + 多级 fallback + LLM 词汇
	// 鸿沟桥）：分析用索引事实，桥用 LLM，加权重取用加权检索。LLM 不在
	// 时桥缺席——鸿沟时退化普通贵路（ 遥测可见）。
	idx := srv.Index()
	srv.Options.Analyzer = analyzerFor(idx)
	srv.Options.Prior = *priorOn
	// **serve 也要读这个开关**（原来只有 eval 读）：首程零窗口时先升级再判不知道。
	// 少了它，鸿沟问句在路由阶段就拒答，词汇桥永远没机会上场——而桥就是为这种情况
	// 造的。（验收脚本第一次跑就撞上：设了开关、bridge 读数仍恒空。）
	srv.Options.Route = routeWithEnvToggles(srv.Options.Route)
	if *abstainOn {
		srv.Options.Abstain = abstain.Default() // 保守启发式权重（cumulus 同款取值）
	}
	if *synthFlag == "llm" {
		client, err := llm.FromEnv(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_CHAT_MODEL"))
		if err != nil {
			return err
		}
		// 桥的结果按归一化问句缓存：桥是链上最后一个非确定源，缓存后同
		// 一问题的第二次起行为完全一致（破局后 12 跑时对时不对，根因就
		// 是模型每次给的扩展词不同）
		// CUMULUS_BRIDGE=0 关桥（消融）：桥的收益/代价要能单独量。
		if os.Getenv("CUMULUS_BRIDGE") != "0" {
			srv.Options.Expander = &query.Cached{Inner: &query.LLM{Client: client}}
		}
		// 事实覆盖判官：词面判据认不出改写（"专利期" vs "专利权的期
		// 限"），未盖的事实让模型判一次——只升级不降级，失败不阻塞
		srv.Options.FactScorer = &facts.LLMScorer{Client: client}
		// 使用信号落数据目录（与语料同盘，同生共死）：再问族服务端推
		// 导，cite 族前端钩子，cumulus signals 看聚合
		srv.Signals = knowledge.NewSignalStore(filepath.Join(*data, "signals.json"))
		// 知识文档生成：把证据整理成一篇可核对的文档写回语料（下一轮能引用它）
		srv.DocGen = &docgen.Generator{Client: client}
		// 窗口分级（GaRAGe 四类）：**只影响可解释性**（file 事件带类别），
		// 不参与检索排序与路由判据。CUMULUS_CLASSIFY=1 开。
		if os.Getenv("CUMULUS_CLASSIFY") == "1" {
			srv.Options.WindowClassifier = &qaflow.WindowClassifier{Client: client}
		}
		// 桥的加权重取**同样要每次现取索引**——上面那段注释（"不能钉死"）
		// 说的就是这件事，我只把它用在了升级贵路上，**桥这条腿漏了**。
		// 症状：桥扩词完全正确（"犬只户外排便"…），但加权检索恒 0 命中 →
		// rejected-empty → 退回朴素路 → 口语问句被拒答。真跑（验收脚本）抓到。
		srv.Options.WeightedRetrieve = func(fc *context.Context, weights map[string]float64) ([]qaflow.EvidenceWindow, error) {
			realm := string(fc.Realm())
			cur, err := srv.IndexFor(gocontext.Background(), realm)
			if err != nil || cur == nil {
				return nil, fmt.Errorf("bridge: index unavailable: %w", err)
			}
			return weightedRetrieveFor(cur, *topk, *width)(fc, weights)
		}
	}
	// 升级贵路无条件装配（深循环不要 embedder；embedder 只服务语义重排
	// 与语义接地尺）——升级判了却没有执行处，等于级联半条腿
	// 升级取数：**每次调用现取索引**，不能在启动时把 srv.Index() 的指针钉死——
	// 那样摄入进来的新文档不在这个旧索引里，升级检索恒返回 0 窗口，于是
	// "刚摄入的文档一问答就拒答"（真跑踩过：facts 显示答案有据、支撑得分 12.8，
	// 但 escalation.windows=0 把好窗口换掉了）。
	srv.Escalate = func(ctx *context.Context, rw qaflow.Rewrite) ([]qaflow.EvidenceWindow, error) {
		idx, err := srv.EscalateIndexFor(ctx)
		if err != nil || idx == nil {
			return nil, fmt.Errorf("escalate: index unavailable")
		}
		return qaflow.BM25DeepEvidence(idx, *width, qaflow.DefaultDeep())(ctx, rw)
	}
	if *embedFlag == "minilm" {
		embFn, _, err := pickEmbed(*embedFlag)
		if err != nil {
			return err
		}
		srv.Embedder = embFn()
	}
	// 看目录：文件落进去即入库（零摩擦摄入的第三条路）
	if *watchDir != "" {
		go func() {
			// 看目录摄入的 realm **必须与请求侧一致**：带凭证时请求都落在密钥推出的
			// realm 上，而这里用空 realm 会写进 `documents/`——于是"watch 说导入了
			// 3 篇，问答/生成却 0 窗口"（真跑踩到）。
			//
			// 口径：**有密钥时取第一个密钥的 realm**（单 realm 部署的常见形态），
			// 其次 -realm，最后空（无凭证的单租户开发形态）。多个 realm 时 watch
			// 只能写一个——所以多 realm 部署不该用 watch 摄入（用 /v1/docs 或逐 realm
			// 的目录），这一点在启动提示里说明。
			watchRealm := *realmFlag
			if keys != nil {
				if rs := keys.Realms(); len(rs) == 1 {
					watchRealm = rs[0]
				} else if len(rs) > 1 {
					fmt.Printf("watch: 警告：配了 %d 个 realm，目录摄入只写一个（%s）；多 realm 请逐 realm 摄入\n", len(rs), rs[0])
					watchRealm = rs[0]
				}
			}
			_ = ingest.WatchDir(ctx, st, watchRealm, *watchDir, 2*time.Second, func(n int) {
				// **失效的是 watchRealm 的缓存索引**，不是单数索引：多 realm 时
				// 请求走 per-realm 缓存（刷单数索引等于刷了一个没人读的）。
				srv.InvalidateRealm(watchRealm)
				fmt.Printf("watch: +%d docs, index invalidated (realm=%s)\n", n, watchRealm)
			})
		}()
		fmt.Printf("serve: watching %s for .txt/.md/.jsonl\n", *watchDir)
	}
	fmt.Printf("serve: corpus=%d docs synth=%s embed=%s listen=%s\n", len(docs), synthLabel, *embedFlag, *listen)
	fmt.Printf("serve: POST /v1/qa {question, session?} · GET /v1/health · GET /v1/status\n")
	return http.ListenAndServe(*listen, srv.Handler())
}

// runSignals 打印使用信号聚合（"下一轮靶子从哪挑"的那张表）。信号是
// 本地资产：只读本机文件，不出网。
func runSignals(args []string) error {
	fs := flag.NewFlagSet("signals", flag.ContinueOnError)
	data := fs.String("data", "", "store directory (signals.json 在里面)")
	top := fs.Int("top", 10, "每族列几条")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return fmt.Errorf("signals: -data is required")
	}
	st := knowledge.NewSignalStore(filepath.Join(*data, "signals.json"))
	if st.Len() == 0 {
		fmt.Println("signals: 还没有信号（先跑服务、真问几个问题）")
		return nil
	}
	fmt.Printf("signals: 共 %d 条\n", st.Len())
	counts := st.Counts()
	for _, kind := range []string{knowledge.SignalReaskAfterRefusal, knowledge.SignalReaskAfterAnswer, knowledge.SignalCitationClick} {
		fmt.Printf("\n[%s] %d 条\n", kind, counts[kind])
		if kind == knowledge.SignalCitationClick {
			for _, s := range st.TopCitations(*top) {
				fmt.Printf("  %6s 次  %s\n", s.Note, s.Target)
			}
			continue
		}
		for _, s := range st.TopQuestions(kind, *top) {
			fmt.Printf("  %6s 次  %s\n", s.Note, s.Question)
		}
	}
	return nil
}

// pruneEvery 是会话清扫周期（CUMULUS_PRUNE_EVERY，默认 1h；0/配错 = 默认）。
//
// 周期与 TTL **故意分开**：TTL 决定"多久算过期"，周期决定"多久才真的删"。
// 把周期调到比 TTL 还密是部署者的选择（要更硬的删除保证）——但不该由代码替他们定。

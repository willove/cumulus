# 认知检索套件（cumulus-cluster）

> CumuBase 功能套件：cumulite 嵌入式存储之上的认知检索——证据可核、多跳可串、同类问题越问越快。
> 文档主场（含设计 SSOT 与 LENS 论文对照）：[`docs/`](docs/)（**本地私有**——`/docs/` 已登记 .gitignore，不提交不外发）；设计单一事实源 [`design-plan.md`](docs/design-plan.md)；Sirchmunk 核心算法研究论文对照 [`lens-notes.md`](docs/lens-notes.md)。

## 定位

对持续增长的本地语料做自然语言检索：**原文是契约（L0）、索引是缓存（L1）、知识图是加速（L2）**。
本套件贡献摄取形状、**BM25 倒排检索与词汇鸿沟改写**（`internal/index`）、FAST/DEEP 分层、知识簇生命周期与图/时序剪枝。蒙特卡洛证据窗口采样也在设计里，但**它在生产语料上基本不触发**——正文 < `SmallFileRunes = 100_000` runes 即整体读入，而中文法条语料 p50 ≈ 3.5K 字符（`internal/mcs/mcs.go:147-161` 注释自陈；详见 `docs/baseline-2026-10-02.md` §三）。全文排序由本套件自建的倒排索引在进程内承担（**不是** cumulite 的全文能力），图遍历同样在进程内；模型流量走 OpenAI 兼容直连端点（`LLM_*` 配置）。

当前进度：**P0–P5 + LENS B1–B10 + 六模协同 + 产品闭环 + 簇整理 + UI v1 簇浏览 + UI v2 目录摄取 + UI v3（evoke-business-ui 壳重构：证据卡/簇证据星图/监控 hero/深色令牌化，见 docs/ui-v3-design.md）+ P8 MCP 工具面 + P9 摄取候选发现 + P4 富边认知层（pathway/barrier）+ B3 评测记分牌 + 评测工作台 v2（题集向导/进度/逐题冻结证据/对比/导出 + 后台隔离执行与持久化，见「评测工作台（eval-v2）」）全部落地**（27 个门标 B–AC，其中无门 A、无门 E；**175** 断言，退出判据零容错 `[ "$FAIL" -eq 0 ]`——不存在「下限」这种部分通过）：P1 搜索 HTTP/SSE 面 → P2 KV 会话 → P3 命名空间作用域 → P6 cluster tidy → UI v1 簇浏览（GET /v1/clusters + 工作台知识簇面板）→ UI v2 摄取面板（POST /v1/ingest/jobs 指定服务器本地目录异步摄取 + 任务状态轮询）→ P8 MCP（POST /mcp 三工具 + stdio 代理）→ P9 候选发现（`scan` 目录扫描 + `ingest-files -candidates` 清单摄取 + 工作台「摄取」面板扫描→勾选→提交，POST /v1/scan）。设计 SSOT 见 design-plan.md。

## 快速开始

```bash
make check                 # fmt + vet + test
make build                 # bin/cumulus-cluster
make e2e                   # 27 个门标（B–AC，无 A 无 E）· 真 cumulite 嵌入库 · 175 断言零容错
                           # 注意：它 source scripts/offline-gate.sh —— LLM 打分与语义嵌入都被换成
                           # 离线 stub / hash-64，所以全绿**不构成任何语义质量保证**
make browser-check         # 浏览器联调门（可选：自起离线 serve，打生产内嵌 /ui/）
bash scenarios/run.sh      # 案例语料（manual-qa / project-kb，离线门，隔离见 scripts/offline-gate.sh）
bash scripts/realdata-probe.sh  # 真实语料对抗基线（~/datasets/cn-law-rag，缺则跳过）

CLUS="./bin/cumulus-cluster"                 # 默认存储 ./var/cumulus-cluster；-data DIR 覆盖；cumulus-cluster env / cumulus-cluster -h 不开库

# 多租户：一切集合/会话按 -ns 分域（ns:clus_* 复合身份；缺省 = 默认库裸名）
$CLUS -ns tenant_a put -title "部署手册" -key handbook -body-file doc.md
$CLUS -ns tenant_a search -q "连接池最大连接数"

$CLUS ensure                          # 声明集合（幂等；serve 启动会自动声明，此命令供 CLI 直用）
$CLUS model status                    # MiniLM 权重状态（目录/文件/清单）
$CLUS model install -y                # 缺席时从魔搭社区下载（约 485MB）并自动验证运行
$CLUS put -title "部署手册" -key handbook -body-file doc.md
$CLUS put -title "页面" -type html -key page -body-file page.html   # HTML 抽取为正文
$CLUS put -title "附件" -type docx -key spec -body-file spec.docx   # DOCX 段落抽取
$CLUS ingest-jsonl -file batch.jsonl -job batch1 [-map map.json]   # 报 written/unchanged/dropped_empty；有空 body 即退出非 0（-allow-empty 豁免）
$CLUS ingest-files -dir ./docs -recursive -job docs1
$CLUS scan -dir ./docs -recursive -limit 200 -out scan.json   # P9 候选发现：规则清单（可 -q 主题 LLM 排名），不开库
$CLUS ingest-files -candidates scan.json -job docs1   # 只吃清单内的文件（同一 Job 状态机，可续）
$CLUS job -job docs1                  # 摄取任务状态（queued/running/done/failed）
$CLUS search -q "连接池最大连接数" [-hopts 168h] [-prior]
$CLUS search -q "那它最大是多少" -history "连接池最大连接数是多少|端口是多少"  # 多轮改写
$CLUS search -q "那它最大是多少" -session SID   # KV 会话（P2，优先于 -history）
$CLUS get <id>
$CLUS delete <id>
$CLUS reclaim -stale                  # 物理回收 tombstone/陈旧修订
$CLUS ensure [-embed]                 # 声明集合（-embed 兼补内容向量）
$CLUS reconcile                       # 消费 changelog：带外退役源→失效证据+标簇待复核
$CLUS cluster list                    # 知识簇（clus_clusters）
$CLUS cluster tidy [-dry-run] [-theta 0.55] [-max N]
                                 # 簇整理：跨题近邻 sibling 的显式折叠（older 存活，幂等）
$CLUS conflicts list                  # 冲突边（clus_conflicts）
$CLUS cites  list                     # 簇→源证据边（clus_cites）
$CLUS session new | list | show <id> | rm <id>   # 会话（P2 KV）
$CLUS eval-run -file items.jsonl -out results.jsonl [-judge] [-l1pre]  # LENS 式评测（R-E1，可续跑）
                                  # ⚠ search 面没有 -l1pre：传入即被拒绝（它曾经被接受却什么都不做）

# 以下 9 个子命令此前既不在本文、其中 6 个也不在 `cumulus-cluster -h` 的 usage 里
$CLUS ingest-adapt -dir DIR [-recursive] [-id F] [-title F] [-body F] [-extra a,b]
                                  # 异构语料适配：json/jsonl/csv/txt 自动判别 + 字段映射
$CLUS env                         # 生效的端点配置（脱敏）；只解析不打开库
$CLUS bucket list | new <name> [label] [note] | rm <name> | show <name>
                                  # 桶/命名空间登记——多租户与 -ns 的入口
$CLUS calib -rows R.jsonl [-usage] [-auto] [-apply] | -guardrail baseline|check -rows R.jsonl
                                  # 阈值自调参：挖档→提线→配对自检→受控应用；不加 -apply 只记录
                                  # -guardrail 是金题护栏基线（见「质量门」）
$CLUS learn [-dry] [-budget N]    # 自进化单周期：零 LLM 挖异常 → 一次假设调用（白名单恰三枚旋钮）
                                  # → 委托 calib -auto 自跑对 → 应用后由护栏校验、掉线即回滚
                                  # → clus_learning 全程落盘；默认 40 万 token 硬顶；无护栏基线拒跑
$CLUS learning                    # 学习状态计数（「现在是干净的吗」「这轮学到了什么」）
$CLUS reset learned [-ns NS] [-yes] [-dry-run]
                                  # 清空某命名空间学过的东西（簇/证据/账本/边/会话），语料不动
                                  # ——复验前的必要动作：旧簇会掩蔽新代码路径
$CLUS vocab -build [-top N]       # 语料自描述词表（纯统计挖词 + 本地嵌入，零 LLM）
                                  # ⚠ 只建表：查询期的桥接钩子已于 3e29a09 撤除，当前不进检索路径
$CLUS affinity                    # 诊断：查账本里某词元的文档权重（衰减后按分排序）
$CLUS serve -listen 127.0.0.1:8484    # HTTP 面：摄取 + POST /v1/search(JSON) + /v1/search/stream(SSE)
                                  #   + 会话 REST + 内嵌工作台 /ui/（会话/知识簇/目录摄取/评测四面板）
                                  #   + POST /mcp（MCP：search/list_clusters/get_cluster）
                                  #   + POST /v1/clusters/{id}/review（簇复核：证据窗口对当前原文
                                  #     逐窗校验，通过则 emerging→stable；工作台「待复核」入口）
                                  #   + /v1/eval/*（评测工作台：题集/运行/逐题证据/对比/导出，见下节）
                                  #   serve 启动即声明集合，新库无需先 ensure；持库期间 CLI 勿指同一目录
$CLUS mcp                             # MCP stdio 代理 → serve 的 /mcp（CLUS_MCP_URL 或 -url 指向端点；
                                  #   代理不开库——Badger 目录排他锁，第二个进程不能再开同一目录）
$CLUS eval-demo                       # 证据质量评测协议演示（B3，离线确定性）
```

## 评测工作台（eval-v2）

工作台「评测」面板可以**直接发起**评测，不再只能读 CLI 落下的记分牌。协议与 CLI 的 `eval-run` 并存：`eval-v2` 是 GUI 面，`ItemScore`/`RunDoc` 保留 CLI 的历史语义（判官结论不覆盖规则分）。

```bash
# 全部端点都要求显式 ns（注册过的库），否则 400——请求体里的 ns 是作用域不是授权
GET  /v1/eval/capabilities?ns=LIB                  # 协议/模型/上限/默认配置/live_available/queue_limit
POST /v1/eval/datasets/validate?ns=LIB             # 只校验不落盘（逐行诊断：重复题号/金标不解析…）
POST /v1/eval/datasets?ns=LIB                      # 保存**不可变版本**（同内容两次保存 = 两个 id、同一 sha）
GET  /v1/eval/datasets?ns=LIB | /datasets/{id}
POST /v1/eval/runs?ns=LIB                          # 202 受理；request_id 重放返回同一实验
GET  /v1/eval/runs?ns=LIB | /runs/{id} | /runs/{id}/items
POST /v1/eval/runs/{id}/cancel | /runs/{id}/retry
GET  /v1/eval/runs/{id}/export?format=json|jsonl|csv
GET  /v1/eval/compare?ns=LIB&left=A&right=B        # 不可比时只给理由，不给差值
```

口径与硬约束（每条都有门断言）：

- **冻结三指纹**：题集 `items_sha`、语料 `corpus_sha`、配置 `config_sha`（含生效模型与掩码后的端点主机名，不含密钥与路径）。运行期间改业务语料不影响已冻结实验；对比时三指纹不一致即判**不可比**，只列原因不列差值。
- **后台隔离执行**：每次运行一个**内存引擎**，把冻结语料原样灌入后跑真实检索/合成路径；业务集合、`clus:lastcluster` 游标一律不写。单 worker 串行，队列上限见 `capabilities.queue_limit`（溢出 409），并发上限 1。
- **持久化与中断语义**：运行状态与逐题结果**同一个 KV 值原子落盘**——进度不可能领先于未持久化的结果。进程被 SIGKILL 后，已完成的实验原样还在；当时在飞的运行只报 `interrupted`（**绝不冷启动重放**，那是没被同意的二次计费）；重试要求热态仍在，否则提示「start a new run」。
- **计费诚实**：`mode=live` 才会调模型；判官与闭卷基线各自计费，工作台在提交前要求勾选**费用确认**（未勾选按钮保持禁用）。Token 记账取自上游 `usage.total_tokens`；上游不报 usage 即判为成本未知并停止后续模型调用（传输层每次请求前复核预算闸门，在飞请求可能小幅超额）。
- **分数口径**：规则匹配（非 EM）· 证据命中（金标解析为**快照内的精确 id**，陈旧修订与同名 business key 都不给分）· 引用可解析（全部引用回溯成功才算）· 判官正确率（分母 `judge_n`，未判分显示 **N/A 而不是 0**）· 闭卷基线对照。延迟与 token 含重试历史。
- **协议边界（照实）**：规则臂是**短参考答案**的子串/数值边界匹配。把 LENS 式整段引用（整篇法条）当 `answer`，规则列会**恒 0**——那是协议不匹配，不是检索失败。校验器对超过 200 字的参考答案给出**一次聚合警告**（工作台直接在向导里显示，数据集仍可保存），把误读挡在运行之前；段落级质量（EM / Ev.Rec / McNemar）仍走 CLI `eval-run` / `scripts/realeval.sh` 的 LENS 协议，两套口径不要混说。
- **记录预算与清理（照实）**：每个库的**运行**与**题集**各上限 200 条；到顶后新工作被拒（错误直说 `storage limit`，`request_id` 重放仍可用，且两个预算互不占用、按库各计）。目前**没有删除端点**：清理是操作者动作——先停 serve（Badger 目录排他锁，CLI 与 serve 不能同开一个目录），然后
  ```bash
  cumulite kv ls  -data <store> -limit 500 "ns:<库名>:clus:eval-v2:run:"     # 题集把 run: 换成 dataset:
  cumulite kv del -data <store> "ns:<库名>:clus:eval-v2:run:<id>"
  ```
  运行记录**自带题集与语料快照**（导出直接读该记录），所以删掉某个题集版本不影响已完成运行与导出，只是那个版本不再可选。
- **验证（三层）**：
  1. **门 BB（`make e2e`，离线确定性）**：校验/不可变版本/幂等/导出/对比/`limit` 取子集/L1 预筛/跨库 404/方法门/有界队列 409/取消（运行中与排队中，后者重启后**不得**被改写成 interrupted）/SIGKILL 重启后的持久化与 interrupted 语义/冷重试拒绝。
  2. **浏览器联调门（`make browser-check`，两段）**：
     - **运行生命周期契约**（`scripts/browser/eval-run-states.mjs`）：静态托管已构建的 `dist` 并拦截 `/v1/**`，确定性断言费用确认闸门（`live` 起跑与 `live` 重试各需一次勾选）、`运行中 → 正在取消（禁用）→ cancelled`、只有「可重试」的运行才出现 `重试失败题目`、干净完成的运行既不给重试也不给冷启动、`interrupted` 必须给出原因 + 重试 + `新建冷启动评测`。状态用 mock 才能按需复现，真的一遍在下一段。
     - **真端到端**（`scripts/browser/eval-workbench.mjs`）：自起一个离线 `serve`，在真浏览器里打**生产内嵌 `/ui/`**（不是 vite dev）走完向导→运行→进度→冻结逐题证据→导出→刷新持久化→对比→深色移动端，含段落式参考答案的警告上屏；任一条浏览器错误即失败。截图落在 `var/browser-eval-check/`。刻意打生产包，是因为内嵌 `dist` 曾经落后于 `web/src`，只有打生产包才看得见。
  3. **真实模型付费面（`make browser-live EVAL_BASE=... EVAL_NS=...`）**：只读一次已完成的 `mode=live` 运行，核对判官结论/理由、闭卷基线、真实 token 计数是否上屏，并确认费用确认闸门在勾选前不可提交；**不提交任何运行、不产生费用**。
- **可选门的依赖边界（照实）**：浏览器门需要 `node` 与 `@playwright/test`，本仓**不**把它写进 `web/package.json`（否则每次 `npm ci` 都要下浏览器）。驱动从 `PLAYWRIGHT_ROOT`（显式指定则只用它）或默认候选（本仓、同级 evoke-ui 检出、`$PWD`）借用；缺席时**显式 SKIP 并以退出码 2 结束**，不会伪装成通过。

## 存储：cumulite 嵌入式库

存储是一个进程内的 Badger 单文件目录，由 `-data DIR` 指定，**默认 `./var/cumulus-cluster`**（不存在则创建）：**不连任何服务端、零网络**。

```bash
./bin/cumulus-cluster ensure                                        # 声明集合（幂等）
./bin/cumulus-cluster put -title "部署手册" -key handbook -body-file doc.md
./bin/cumulus-cluster search -q "连接池最大连接数"                    # 集合/KV/向量/changelog 同一引擎
./bin/cumulus-cluster serve -listen 127.0.0.1:8484                  # HTTP 面同样落在该目录
./bin/cumulus-cluster -data ~/stores/law ensure                     # 换目录（-data 覆盖默认）
```

- **一条 Port 契约**：存储面收敛为 17 方法接口 `cumulite.Port`（16 + `Subscribe`），本仓只经它消费——不为引擎改一行存储代码；
- **cumulite 是独立仓库**（`../db-works/cumulite`，契约类型在其 `contract/` 包维护）；
- **单进程独占**：Badger 对目录取排他锁，同一 store 同时只能有一个进程打开——`serve` 与 CLI **不能**指向同一目录并跑，写入侧要走 serve 的 `/v1/ingest/*`；
- **边界**：KNN 为精确扫描（无 ANN），无 CAS/自增，collection 是声明标记而非 schema；运维注意（fsync、压缩、命名空间合成）见 cumulite README；
- **证据**：hermetic 回归 `cmd/cumulus-cluster/lite_test.go`——put/ensure/ensure-embed/reconcile/session/cluster/weak-edge 全链 + 一次真实 FAST search，进程内没有任何服务端。

## 架构

```
put / ingest-jsonl / ingest-adapt / ingest-files / HTTP ingest ─► internal/ingest
        │   批量写面共享 NewBatchIngester（集合在那里懒声明）
        │   存储身份是 source.RevisionID = src:<业务身份>#<版本号>
        │   ——业务身份取 key→title→摘要，正文改动产生新版本，
        │     不再是内容摘要本身（A→B→A 与「删除后重导判 unchanged」都源于旧绑法）
        ▼
search ─► internal/deep  AskLazy 编排，真实顺序是：
        ①effectiveQuery（会话/历史改写）
        ②decompose + thresholdFor
        ③internal/kb TryReuseNarrow（L2 簇复用尝试，命中可不碰语料）
        ④loadCandidates ─► **internal/index**：ActiveSources 全量读 → BM25 倒排
        │   Build/Score → RewriteWhenEmpty（词汇鸿沟一次 LLM 改写）→ 条件 Rerank
        │   （倒排索引每请求重建，非进程级缓存；Rerank 只认语义座位）
        ⑤askEffective：fast.MatchFilename → FAST → mcs → 置信不足升 DEEP
           （defer-synth 默认开：低置信直接升级、不先合成）
              ├─ L2 graph.Expand（weak_edges 1..2 跳 + hopKNN）
              ├─ internal/mcs（证据窗口；**生产语料走整文件捷径**，见上）
              ├─ internal/facts（多跳覆盖 B1/B2）· internal/eval（记分牌 B3）
              └─ internal/calib + internal/learn（自动调参；由金题护栏否决回滚）

        internal/vocab 在树但**未接线**（钩子已于 3e29a09 撤除）；
        internal/index 的 v3 扩展链（NarrowWithExpansion 等）同样**未接线**。
```

- **摄取单元 = 源文档**（`body` + `structure` 定位映射），**不是 chunk**；
- **put 不依赖 embedder**——L0 即可搜；内容向量属 L1（`ensure -embed` 补建；KNN 收窄候选是**评测对照臂** `eval-run -l1pre`，它索引不可用时直接报错、不回退全列表，所以归档的 eval 数字不等于 serve 在同语料上的行为；**检索主路径没有这条预筛**，`search -l1pre` 传入即被拒绝——它曾经被接受却什么都不做）；
- ~~**语义臂的排序默认生效**~~（**该机制已退役，开关 `CLUS_ADMIT_SEMANTIC_HEAD` 已从代码移除**）：曾发现 DEEP 准入顺序把 KNN 臂**追加**在 500 篇词面扫之后，再被 `out[:maxDeepLoops]` 截掉——embedder 排第 1 的文档落在第 ~500 位、**算了付费却从未被循环看到**。当时端点档配对 A/B（12 道 cn-law 锚点）测得 `Ev.Rec` 4/12→6/12、discordant **0:2 零回退**。**该收益后来被 D0 判定为短文档假象并撤回**（见下条）——短文档语料上 KNN 有效，真实长度文档上 R@4≈1%。历史读数见 `docs/perf-plan.md` §4.8；
- ~~**原子事实拆解**~~（**该机制已退役，开关 `CLUS_DECOMPOSE` 与 `llm.FactBuilder` 已从代码移除**）：启发式 `facts.Build` 在真实题集上 124/136 题不拆、12 次 K>1 拆分**全部**误切（书名/术语/枚举中间）；端点档配对 A/B 测出的是回退（`Ev.Rec` 7/12→6/12），两次失败同型，**都是 0 轮早停 + 引到错文档 + token 减半**。且 30 题里只有 2 题（7%）含协调标记，双闸门下机制几乎完全惰性——**该题集评估不了它**。替代方案（`query_abstract` / `query_from_abstract` 两 call 隔离）资产已就位未接线。历史读数见 `docs/perf-plan.md` §4.9；
- **摄取有编码门**（2026-09-27，`internal/charset`）：实测 1251 本中文网络小说语料——**纯 UTF-8 5.4% / GB18030 87.0% / 拒结 7.6%**。原本 `ingest-files` 零编码处理，GBK 书会以乱码入库且**无任何环节报告**，rune 计数错还会连带破坏块边界与引用偏移。三档门：UTF-8 原样（去 BOM）/ GB18030 转码并记 `charset`+`src_digest`+`src_bytes` / **都不行则拒绝并报错，绝不替换字符**。注意 `x/text` 的 GB18030 解码器对无法映射的字节**静默吐 U+FFFD 且 `err=nil`**（三种调用方式都验过），所以校验用两级：扫 U+FFFD 快路径 + **重新编码回 GB18030 与原字节比对**（"转换无损"这个不变量本身）。磁盘上的源文件从不写入；
- **超长文档按块入库是默认路径**（2026-09-28，`internal/source.SplitBlocks` + `CLUS_INGEST_BLOCKS=0` 关闭）：实测一本 1859 万 rune 的书，走窗口采样时**只读进 0.031%**。块切分**结构无关**（只看长度，不含任何体裁规则），默认 8000 rune 块 + 1000 重叠 ⇒ 块永远整块直读、零采样损失；孤儿退役判据是"块序号 ≥ 当前块数"。`Structure` 对纯 txt 无效（只产 1 个 doc span），所以这是兜底路径；带层级的文档仍走原路径。**默认开启是安全的**：`MinRunes` 默认 16,000 rune，低于它的文档逐字不变——实测 40 条法条锚点 0/40 受影响、chinalaw 1548 篇 60 篇（3.9%）受影响，而那 60 篇正是此前"每查询只读全书 0.031%"的那批；
- **嵌入单元必须 ≤ 嵌入模型窗口**（2026-09-28 写入 `design-plan.md` §D0）：MiniLM 的 SentencePiece 模板有 **128-token 上限**，所以文档级向量只表示前 ~300 汉字。实测一本 1548 万 rune 的书切成 2212 块后，**MiniLM 的块级召回 R@4=1.1%（随机 0.18%），而词法 BM25 同一批块 R@4=99.0%、零嵌入**。因此**长文档由词法倒排承担召回，向量层降级为短文档加速器**（法条/百科条目千字级时它仍然有效）。语义臂的准入排序开关（`CLUS_ADMIT_SEMANTIC_HEAD`）据此**连同机制一并退役**——不是因为排序改错了，而是因为该场景下语义臂本身不成立；
- **打分器可靠性可测**（2026-09-27，`make score-probe` / `cmd/scoreprobe`）：同一窗口重复打分 N 次，报告决策稳定性、阈值模糊带、金标分离度。实测（live MiniMax-M3，25 窗 × 3 轮）：`score_gap` 5.70、`top_gold_stable` 100%、队首零并列，但**中段判决 36% 会翻转**（thinking 关时 48%）——所以逐窗指标比端到端指标吵得多，而队首选择是稳的。record-only，不设门；
- **MiniLM 权重是本套件自有资产**：装在 `~/.cumulus/models/<model>`（`CLUS_MODEL_DIR` 可覆盖，**不读其他项目的缓存**）；`CLUS_EMBED=minilm` 时首启检查，缺席则 CLI 交互提示下载 / 工作台「配置」页一键下载（魔搭社区，约 485MB，sha256 校验，下载后自动加载验证）；**要了 `CLUS_EMBED=minilm` 却没有权重就是硬失败**（2026-10-02 起；此前是静默退回 `local-hash-64`，而 hash-64 的向量无语义，Rerank 拿它会把 BM25 序随机打乱——serve 曾连着数日跑在这个状态上）。显式退出口是 `CLUS_EMBED=hash`；`CLUS_MINILM_REQUIRE=1` 保留为兼容别名，不再是开关；
- **生产路径全走远端端点**（D6）：打分 `evaluate_sample`、意图/级联关键词 `fast_analyze`、合成 `synthesize_roi`、级联降级 `keywords_multilevel`、多轮改写 `history_rewrite`（`search -history` 传入历史，失败降级原查询）、embedder——设 `LLM_BASE_URL` 即切换（`LLM_CHAT_MODEL` 选模型，`LLM_EMBED_MODEL` 显式指定才启用远程嵌入）；离线 `RuleAnalyzer`/`KeywordScorer`/模板是门的载体；
- **档位判定**：FILENAME_ONLY（文件/标题名匹配，0 LLM）/ 闲聊 / 整文档意图 / FAST→DEEP（置信不足必升级）；
- **B4 先验已接线**：`search -prior` 走五信号融合排序（默认 IDF 级联不变）；`search -hopts <时长>` 开 hopTS 新鲜度剪枝；
- **抽取**：md/txt/html 树内；DOCX 树内（stdlib zip+xml 段落）；PDF 尽力而为（未压缩/Flate 文本流——加密/CID 字体走外挂 worker 形态）；二进制仍不进引擎。

## 边界（照实）

- v1 DEEP 为**多源重采样 + 确定性合成**（ReAct 形），生成段质量门按 R 轨收口；冲突边首版是数值主张分歧启发式；
- **离线 KeywordScorer 不是语义评分**——定位与门可断言，质量声明要换真实端点后再测；
- **摘要模板是确定性拼接**，不是生成式合成；生成段质量门按 R 轨收口，不设确定性线；
- **引用 `[?]`** = 未能精确回溯原文窗口（源已更新或定位越界）；多源样本逐源定位回原文；
- **v1 未接线**（设计已声明、触发再做）：PDF 外挂 worker（加密/CID 字体件——树内 best-effort 覆盖未压缩与 Flate 文本流）、写入侧 LLM 自评质量门（端点档，只记录不设线）；tidy 维护动作仍走 CLI；UI v2 已提供目录摄取面板——路径是 serve 所在机器的本地目录（可信本机工具口径，见部署边界）；
- **Prompt 五类资产已全部进生产路径**（打分/意图/合成/级联降级/多轮改写）；离线桩与冻结回归是门的载体；
- **端点配置在各套件内**（D6 决策更新 2026-09-22：统一网关暂锁、直连端点即用；命名 2026-10-01 起收敛为 `LLM_*` canonical，旧 `AIGATE_*`/`LLM_MODEL_NAME` 作为别名兼容）：套件读 `./.env`（或 `$CLUS_ENV`），`LLM_BASE_URL`/`LLM_API_KEY`/`LLM_CHAT_MODEL` 为准（见 `.env.example`；已设环境变量优先，`cumulus-cluster env` 脱敏查看）。MiniMax 直连已适配：`reasoning_split` 自动（minimaxi 域，`LLM_REASONING_SPLIT` 强制）+ `<think>` 内联思维链剥离。实测（2026-09-25，177 篇法律语料 + MiniMax-M3）：**FAST 冷查询 ≈6–10s（受控 A/B；线上采样受端点负载影响 10–15s）/ 簇复用 0.04s / tokens ≈6–10K 每查询**；本会话把同端点冷查询从 67.4s 压到此，三步都有门禁与受控测量背书：① 小文件全文路径（对标 sirchmunk `_FAST_SMALL_FILE_THRESHOLD=100_000`，见 `internal/mcs` DefaultConfig）把一次查询的串行 LLM 调用从 ≈21 次压到 3 次（分析+整文评分+合成），置信度不降反升（0.31→0.85 均值档）；② 修复全文证据被字节截断喂给模型的 bug（scorer/synth 的 2000/800 字节帽 → 15000 字位，`internal/llm` maxSampleRunes）与模型坏 JSON 的修复通道（`repairModelJSON`：法条引用把引号写成未转义 ASCII 引号曾让合成整条落模板拒答）；③ 意图/关键词分析走无思维链变体（`CompleteStructured`）——受控 A/B 8.66s→6.17s，关键词质量不减。**该变体的实现已随模型换代改写**：M3.1+ 拒绝 `thinking{type:disabled}`（报 "requires adaptive thinking"），现改发 `reasoning_effort=low` + `output_config.effort=low`（低思考替代禁思考；旧模型忽略新字段，无破坏），且 M3.1+ 同时抑制 `reasoning_split`；其偶发把真问题误判为 chat/doc_summary（不检索直接答"闲聊"）由确定性门复核兜底（`fast.LooksLikeChat/LooksLikeDocSummary`，非问候句式即用带思维链调用复核一次，真问候零成本）；④ 合成提示词要求结构化 Markdown 简报（`internal/prompts/synthesize_roi.md` 规则：列举类内容逐条带条款号与关键参数、三项以上用表格）——对标 sirchmunk 的 Markdown Briefing 契约（其 `llm/prompts.py:ROI_RESULT_SUMMARY`），引用与拒答契约不变；远程 embedder 仅在 `LLM_EMBED_MODEL` 显式指定时切换（本地 minilm 权重在即默认启用，见 `embedderFor`）；
- **`internal/prior`（LENS B4）已接线**：`search -prior` 五信号排序，默认 IDF 级联；
- **hopTS 新鲜度剪枝**已接线（`search -hopts 168h`），默认关；
- **R3 实测**（`go test ./internal/mcs -run TestR3AnchorProbe -v`）：CJK bigram 锚点命中率 **0.404**（23/57，噪声大→阶段①分层撒网臂必须保留）；答案入窗率 **0.714**（5/7）；
- **R2 跨系统实测**（`bash scripts/r2-cluster-probe.sh`，指向外部 Sirchmunk）：同主题 10 措辞 Sirchmunk 裂 **4 簇** vs Gate B 改写族 ≤1；R5 阴性（无跨主题串台，G-pollute 素材用套内构造 fixture）；
- 状态在 store：`clus_sources` / `clus_evidence` / `clus_clusters` / `clus_weak_edges` / job 游标 KV；
- **部署边界（照实）**：HTTP 面（`cumulus-cluster serve`）按**可信本机工具**部署——没有应用层鉴权，请求体里的 `ns` 是**作用域**不是授权（租户隔离靠不把端口暴露给不受信方），`POST /v1/ingest/jobs` 的 `dir` 可读本机任意目录。要对外提供服务需另行加鉴权与目录白名单；
- 不做查询期扫本地文件树、不把二进制原件写入引擎、模型不进库；
- 规模假设：源 ≤10⁴、簇 ≤10³（见计划 §7）。

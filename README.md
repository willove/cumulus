# 认知检索套件（cumulus-cluster）

> CumuBase 功能套件：cumulite 嵌入式存储之上的认知检索——证据可核、多跳可串、同类问题越问越快。
> 文档主场（含设计 SSOT 与 LENS 论文对照）：[`docs/`](docs/)（**本地私有**——`/docs/` 已登记 .gitignore，不提交不外发）；设计单一事实源 [`design-plan.md`](docs/design-plan.md)；Sirchmunk 核心算法研究论文对照 [`lens-notes.md`](docs/lens-notes.md)。

## 定位

对持续增长的本地语料做自然语言检索：**原文是契约（L0）、索引是缓存（L1）、知识图是加速（L2）**。
本套件贡献摄取形状、蒙特卡洛证据采样、FAST/DEEP 分层、知识簇生命周期与图/时序剪枝；存储是 cumulite 的文档/KV/向量三面（全文排序与图遍历在进程内完成），模型流量一律经 aigate。

当前进度：**P0–P5 + LENS B1–B10 + 六模协同 + 产品闭环 + 簇整理 + UI v1 簇浏览 + UI v2 目录摄取 + P8 MCP 工具面 + P9 摄取候选发现 + P4 富边认知层（pathway/barrier）+ B3 评测记分牌全部落地**（门 A–BB，门限 118、现 153 断言）：P1 搜索 HTTP/SSE 面 → P2 KV 会话 → P3 命名空间作用域 → P6 cluster tidy → UI v1 簇浏览（GET /v1/clusters + 工作台知识簇面板）→ UI v2 摄取面板（POST /v1/ingest/jobs 指定服务器本地目录异步摄取 + 任务状态轮询）→ P8 MCP（POST /mcp 三工具 + stdio 代理）→ P9 候选发现（`scan` 目录扫描 + `ingest-files -candidates` 清单摄取 + 工作台「摄取」面板扫描→勾选→提交，POST /v1/scan）。设计 SSOT 见 design-plan.md。

## 快速开始

```bash
make check                 # fmt + vet + test
make build                 # bin/cumulus-cluster
make e2e                   # 门 A–BB（真 cumulite 嵌入库，门限 118、现 153 断言）
bash scenarios/run.sh      # 案例语料（manual-qa / project-kb）
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
$CLUS ingest-jsonl -file batch.jsonl -job batch1 [-map map.json]
$CLUS ingest-files -dir ./docs -recursive -job docs1
$CLUS scan -dir ./docs -recursive -limit 200 -out scan.json   # P9 候选发现：规则清单（可 -q 主题 LLM 排名），不开库
$CLUS ingest-files -candidates scan.json -job docs1   # 只吃清单内的文件（同一 Job 状态机，可续）
$CLUS job -job docs1                  # 摄取任务状态（queued/running/done/failed）
$CLUS search -q "连接池最大连接数" [-hopts 168h] [-prior] [-l1pre]
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
$CLUS serve -listen 127.0.0.1:8484    # HTTP 面：摄取 + POST /v1/search(JSON) + /v1/search/stream(SSE)
                                  #   + 会话 REST + 内嵌工作台 /ui/（会话/知识簇/目录摄取三面板）
                                  #   + POST /mcp（MCP：search/list_clusters/get_cluster）
                                  #   serve 启动即声明集合，新库无需先 ensure；持库期间 CLI 勿指同一目录
$CLUS mcp                             # MCP stdio 代理 → serve 的 /mcp（CLUS_MCP_URL 或 -url 指向端点；
                                  #   代理不开库——Badger 目录排他锁，第二个进程不能再开同一目录）
$CLUS eval-demo                       # 证据质量评测协议演示（B3，离线确定性）
```

## 存储：cumulite 嵌入式库

存储是一个进程内的 Badger 单文件目录，由 `-data DIR` 指定，**默认 `./var/cumulus-cluster`**（不存在则创建）：**不连任何服务端、零网络**。

```bash
./bin/cumulus-cluster ensure                                        # 声明集合（幂等）
./bin/cumulus-cluster put -title "部署手册" -key handbook -body-file doc.md
./bin/cumulus-cluster search -q "连接池最大连接数"                    # 集合/KV/向量/changelog 同一引擎
./bin/cumulus-cluster serve -listen 127.0.0.1:8484                  # HTTP 面同样落在该目录
./bin/cumulus-cluster -data ~/stores/law ensure                     # 换目录（-data 覆盖默认）
```

- **一条 Port 契约**：存储面收敛为 16 方法接口 `cumulite.Port`，本仓只经它消费——不为引擎改一行存储代码；
- **cumulite 是独立仓库**（`../db-works/cumulite`，契约类型在其 `contract/` 包维护）；
- **单进程独占**：Badger 对目录取排他锁，同一 store 同时只能有一个进程打开——`serve` 与 CLI **不能**指向同一目录并跑，写入侧要走 serve 的 `/v1/ingest/*`；
- **边界**：KNN 为精确扫描（无 ANN），无 CAS/自增，collection 是声明标记而非 schema；运维注意（fsync、压缩、命名空间合成）见 cumulite README；
- **证据**：hermetic 回归 `cmd/cumulus-cluster/lite_test.go`——put/ensure/ensure-embed/reconcile/session/cluster/weak-edge 全链 + 一次真实 FAST search，进程内没有任何服务端。

## 架构

```
put / ingest-jsonl ─► internal/ingest ─► clus_sources（内容寻址 src:<digest16>）
                           │                 clus_evidence（窗口+失效）
                           ▼
search ─► internal/kb（复用-or-检索 · 簇演化 · query_seq 边）
              ├─ L2 graph.Expand（weak_edges 1..2 跳 + hopKNN）
              └─ internal/fast ─► internal/mcs（分层/锚点/高斯采样）
                     └─ internal/facts（多跳覆盖 B1/B2）· internal/eval（证据质量 B3）
```

- **摄取单元 = 源文档**（`body` + `structure` 定位映射），**不是 chunk**；
- **put 不依赖 embedder**——L0 即可搜；内容向量属 L1（`ensure -embed` 补建，`search -l1pre` 经 KNN 收窄候选；索引挂了只慢不错）；
- **MiniLM 权重是本套件自有资产**：装在 `~/.cumulus/models/<model>`（`CLUS_MODEL_DIR` 可覆盖，**不读其他项目的缓存**）；`CLUS_EMBED=minilm` 时首启检查，缺席则 CLI 交互提示下载 / 工作台「配置」页一键下载（魔搭社区，约 485MB，sha256 校验，下载后自动加载验证）；`CLUS_MINILM_REQUIRE=1` 把缺席升级为硬失败（CI 精度门不空转）；
- **生产路径全走 aigate**（D6）：打分 `evaluate_sample`、意图/级联关键词 `fast_analyze`、合成 `synthesize_roi`、级联降级 `keywords_multilevel`、多轮改写 `history_rewrite`（`search -history` 传入历史，失败降级原查询）、embedder——设 `AIGATE_BASE_URL` 即切换（`AIGATE_CHAT_MODEL` 默认 `mimo/cascade-pro`，`AIGATE_EMBED_MODEL` 默认 `text-embedding-3-small`）；离线 `RuleAnalyzer`/`KeywordScorer`/模板是门的载体；
- **档位判定**：FILENAME_ONLY（文件/标题名匹配，0 LLM）/ 闲聊 / 整文档意图 / FAST→DEEP（置信不足必升级）；
- **B4 先验已接线**：`search -prior` 走五信号融合排序（默认 IDF 级联不变）；`search -hopts <时长>` 开 hopTS 新鲜度剪枝；
- **抽取**：md/txt/html 树内；DOCX 树内（stdlib zip+xml 段落）；PDF 尽力而为（未压缩/Flate 文本流——加密/CID 字体走外挂 worker 形态）；二进制仍不进引擎。

## 边界（照实）

- v1 DEEP 为**多源重采样 + 确定性合成**（ReAct 形），生成段质量门按 R 轨收口；冲突边首版是数值主张分歧启发式；
- **离线 KeywordScorer 不是语义评分**——定位与门可断言，质量声明要换 aigate 端点后再测；
- **摘要模板是确定性拼接**，不是生成式合成；生成段质量门按 R 轨收口，不设确定性线；
- **引用 `[?]`** = 未能精确回溯原文窗口（源已更新或定位越界）；多源样本逐源定位回原文；
- **v1 未接线**（设计已声明、触发再做）：PDF 外挂 worker（加密/CID 字体件——树内 best-effort 覆盖未压缩与 Flate 文本流）、写入侧 LLM 自评质量门（端点档，只记录不设线）；Web UI 暂无 ns 选择器与评测记分牌页（沿用 serve 级 `-ns`；tidy 维护动作仍走 CLI）；UI v2 已提供目录摄取面板——路径是 serve 所在机器的本地目录（可信本机工具口径，见部署边界）；
- **Prompt 五类资产已全部进生产路径**（打分/意图/合成/级联降级/多轮改写）；离线桩与冻结回归是门的载体；
- **端点配置在各套件内**（D6 决策更新 2026-09-22，aigate 暂锁、统一网关后期规划）：套件读 `./.env`（或 `$CLUS_ENV`），沿用操作者 `LLM_BASE_URL`/`LLM_API_KEY`/`LLM_MODEL_NAME` 约定（见 `.env.example`；已设环境变量优先，`cumulus-cluster env` 脱敏查看）。MiniMax 直连已适配：`reasoning_split` 自动（minimaxi 域，`AIGATE_REASONING_SPLIT` 强制）+ `<think>` 内联思维链剥离。实测：FAST 67.4s / 复用 0.0s / DEEP 53.7s，≈904 tokens/窗、≈7186/合成（`scripts/endpoint-probe.sh`）；embedder 仅在 `AIGATE_EMBED_MODEL` 显式指定时切换；
- **`internal/prior`（LENS B4）已接线**：`search -prior` 五信号排序，默认 IDF 级联；
- **hopTS 新鲜度剪枝**已接线（`search -hopts 168h`），默认关；
- **R3 实测**（`go test ./internal/mcs -run TestR3AnchorProbe -v`）：CJK bigram 锚点命中率 **0.404**（23/57，噪声大→阶段①分层撒网臂必须保留）；答案入窗率 **0.714**（5/7）；
- **R2 跨系统实测**（`bash scripts/r2-cluster-probe.sh`，指向外部 Sirchmunk）：同主题 10 措辞 Sirchmunk 裂 **4 簇** vs Gate B 改写族 ≤1；R5 阴性（无跨主题串台，G-pollute 素材用套内构造 fixture）；
- 状态在 store：`clus_sources` / `clus_evidence` / `clus_clusters` / `clus_weak_edges` / job 游标 KV；
- **部署边界（照实）**：HTTP 面（`cumulus-cluster serve`）按**可信本机工具**部署——没有应用层鉴权，请求体里的 `ns` 是**作用域**不是授权（租户隔离靠不把端口暴露给不受信方），`POST /v1/ingest/jobs` 的 `dir` 可读本机任意目录。要对外提供服务需另行加鉴权与目录白名单；
- 不做查询期扫本地文件树、不把二进制原件写入引擎、模型不进库；
- 规模假设：源 ≤10⁴、簇 ≤10³（见计划 §7）。

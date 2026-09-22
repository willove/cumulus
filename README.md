# 认知检索套件（ask 套件）

> CumuBase 功能套件：cumudb 基座之上的认知检索——证据可核、多跳可串、同类问题越问越快。
> 文档主场（含设计 SSOT 与 LENS 论文对照）：[`docs/suites/ask/`](../../docs/suites/ask/)；设计单一事实源 [`design-plan.md`](../../docs/suites/ask/design-plan.md)（v1.4）；Sirchmunk 核心算法研究论文对照 [`lens-notes.md`](../../docs/suites/ask/lens-notes.md)。

## 定位

对持续增长的本地语料做自然语言检索：**原文是契约（L0）、索引是缓存（L1）、知识图是加速（L2）**。
本套件贡献摄取形状、蒙特卡洛证据采样、FAST/DEEP 分层与知识簇生命周期；向量/全文/图/时序能力全部来自基座，模型流量一律经 aigate。

当前进度：**P0–P5 + LENS B1–B3 完成，B4 已接线**（门 A–I）。设计 SSOT 见计划 v1.8。

## 快速开始

```bash
make check                 # fmt + vet + test
make build                 # bin/ask
make e2e                   # 门 A–I（真 cumudb，62 断言）
bash scenarios/run.sh      # 案例语料（manual-qa / project-kb）

./bin/ask ensure                          # 声明集合（幂等）
./bin/ask put -title "部署手册" -key handbook -body-file doc.md
./bin/ask put -title "页面" -type html -key page -body-file page.html   # HTML 抽取为正文
./bin/ask put -title "附件" -type docx -key spec -body-file spec.docx   # DOCX 段落抽取
./bin/ask ingest-jsonl -file batch.jsonl -job batch1 [-map map.json]
./bin/ask ingest-files -dir ./docs -recursive -job docs1
./bin/ask job -job docs1                  # 摄取任务状态（queued/running/done/failed）
./bin/ask search -q "连接池最大连接数" [-hopts 168h] [-prior] [-l1pre]
./bin/ask search -q "那它最大是多少" -history "连接池最大连接数是多少|端口是多少"  # 多轮改写
./bin/ask get <id>
./bin/ask delete <id>
./bin/ask reclaim -stale                  # 物理回收 tombstone/陈旧修订
./bin/ask ensure -embed                   # 兼补 body_embed 内容向量（L1 缓存，search -l1pre 读）
./bin/ask cluster list                    # 知识簇（ask_clusters）
./bin/ask conflicts list                  # 冲突边（ask_conflicts）
./bin/ask cites  list                     # 簇→源证据边（ask_cites）
./bin/ask serve -listen 127.0.0.1:8484    # HTTP 摄取面（/health · /v1/ingest/*）
./bin/ask eval-demo                       # 证据质量评测协议演示（B3，离线确定性）
```

## 架构

```
put / ingest-jsonl ─► internal/ingest ─► ask_sources（内容寻址 src:<digest16>）
                           │                 ask_evidence（窗口+失效）
                           ▼
search ─► internal/kb（复用-or-检索 · 簇演化 · query_seq 边）
              ├─ L2 graph.Expand（weak_edges 1..2 跳 + hopKNN）
              └─ internal/fast ─► internal/mcs（分层/锚点/高斯采样）
                     └─ internal/facts（多跳覆盖 B1/B2）· internal/eval（证据质量 B3）
```

- **摄取单元 = 源文档**（`body` + `structure` 定位映射），**不是 chunk**；
- **put 不依赖 embedder**——L0 即可搜；内容向量属 L1（`ensure -embed` 补建，`search -l1pre` 经 KNN 收窄候选；索引挂了只慢不错）；
- **生产路径全走 aigate**（D6）：打分 `evaluate_sample`、意图/级联关键词 `fast_analyze`、合成 `synthesize_roi`、级联降级 `keywords_multilevel`、多轮改写 `history_rewrite`（`search -history` 传入历史，失败降级原查询）、embedder——设 `AIGATE_BASE_URL` 即切换（`AIGATE_CHAT_MODEL` 默认 `mimo/cascade-pro`，`AIGATE_EMBED_MODEL` 默认 `text-embedding-3-small`）；离线 `RuleAnalyzer`/`KeywordScorer`/模板是门的载体；
- **档位判定**：FILENAME_ONLY（文件/标题名匹配，0 LLM）/ 闲聊 / 整文档意图 / FAST→DEEP（置信不足必升级）；
- **B4 先验已接线**：`search -prior` 走五信号融合排序（默认 IDF 级联不变）；`search -hopts <时长>` 开 hopTS 新鲜度剪枝；
- **抽取**：md/txt/html 树内；DOCX 树内（stdlib zip+xml 段落）；PDF 尽力而为（未压缩/Flate 文本流——加密/CID 字体走外挂 worker 形态）；二进制仍不进引擎。

## 边界（照实）

- v1 DEEP 为**多源重采样 + 确定性合成**（ReAct 形），生成段质量门按 R 轨收口；冲突边首版是数值主张分歧启发式；
- **离线 KeywordScorer 不是语义评分**——定位与门可断言，质量声明要换 aigate 端点后再测；
- **摘要模板是确定性拼接**，不是生成式合成；生成段质量门按 R 轨收口，不设确定性线；
- **引用 `[?]`** = 未能精确回溯原文窗口（源已更新或定位越界）；多源样本逐源定位回原文；
- **v1 未接线**（设计已声明、触发再做）：PDF 外挂 worker（加密/CID 字体件——树内 best-effort 覆盖未压缩与 Flate 文本流）、`history_rewrite` 的多轮会话载体（现为 CLI `-history` 显式传入）、六模协同 scenario（可选，未建）；
- **Prompt 五类资产已全部进生产路径**（打分/意图/合成/级联降级/多轮改写）；离线桩与冻结回归是门的载体；
- **`internal/prior`（LENS B4）已接线**：`search -prior` 五信号排序，默认 IDF 级联；
- **hopTS 新鲜度剪枝**已接线（`search -hopts 168h`），默认关；
- **R3 实测**（`go test ./internal/mcs -run TestR3AnchorProbe -v`）：CJK bigram 锚点命中率 **0.404**（23/57，噪声大→阶段①分层撒网臂必须保留）；答案入窗率 **0.714**（5/7）。
- 状态在 cumudb：`ask_sources` / `ask_evidence` / `ask_clusters` / `ask_weak_edges` / job 游标 KV；
- 不做查询期扫本地文件树、不把二进制原件写入引擎、模型不进库；
- 规模假设：源 ≤10⁴、簇 ≤10³（见计划 §7）。

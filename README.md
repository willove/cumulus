# cumulus-next

对持续增长的本地语料做自然语言检索与问答：证据可核、策略可回滚、系统越用越准。

cumulus 的下一版。区别不在功能，在结构：一切共享状态经过一个 context、一切修改可逆、一切依赖显式、每次处理留提交视图。

## 读文档的顺序

1. [`docs/architecture.md`](docs/architecture.md)——架构总图 + 从 cumulus 继承的教训（问题→对策→执行处）
2. [`docs/flow-grammar.md`](docs/flow-grammar.md)——流程文法 SSOT：三个流程的 stage 契约、六条不变量、失败六分类
3. [`docs/evolution-log.md`](docs/evolution-log.md)——演进记录：门禁抓到过什么、立下了哪些规矩、还没验证什么
4. [`docs/web-audit.md`](docs/web-audit.md)——web 重建前的审计（内容去留与重建纪律）
5. [`docs/adr/`](docs/adr/)——决策记录（含 ADR-001：为什么不做代码级热替换）

## 现状（v0.2 · 2026-10-08）

已落地并能跑（25 个包、八道门禁全绿；逐项进度见 `docs/flow-grammar.md` 正文的 ✅/⏳）：

```
internal/context/   typed key、realm、注册/释放、提交视图（含校准档位）、stage 禁闭
internal/flow/      stage 契约 + runner（禁闭、失败反卷、记视图、ViewHook）
internal/qaflow/    问答流程（复用了这批条件件：复写/事实/驱逐/路由/升级/弃权/复用）+ 哑臂装配
internal/synth/     合成面两种实现：离线确定性 + LLM（严格 JSON 契约，引用编号化）
internal/evalfcore/ 评测 episode（三指纹/原子落盘/失败六分类/臂注册表 + 哑臂不变量 + 校准读数）
internal/learncore/ 受管变更五阶段（旋钮白名单、率地板护栏、提升=注册、掉线回滚）
internal/retrieval/ 倒排索引 + BM25 + CJK 二元组分词 + 证据窗口坐标 + 窗口内容摘要
internal/facts/     事实分解 / 覆盖判定 / 一致性门（同事实不同值）
internal/query/     IDF 加权主关键词级、停用/胶水降权、词汇鸿沟桥（按问句缓存）
internal/prior/     文档级多信号重排（lexical 无长度归一 + 标题 + 条文结构）
internal/ctxmgmt/   合成前上下文管理（按源配额 / 语义近重合并 0.92 / 窗口预算，决定留痕）
internal/abstain/   零 LLM 失败预测头（早弃权 / 强升级）
internal/corpus/    摄入管：解码→规范化→内容寻址→入库（charset 三纪律 + 血缘）
internal/ingest/    三通道摄入：粘贴 / 链接 / 看目录
internal/api/       HTTP 面（qa/ingest/doc/signal(s)/health/status）+ 内嵌单页 + 契约生成
internal/store/     存储端口 + cumulite 适配器（线上版 github.com/willove/cumulite v0.2.1）
cmd/cumulus/        selftest / eval / learn / serve / signals
```

```bash
scripts/gates.sh                    # 八道门禁（零容错）
CUMULUS_AB=1 CUMULUS_DEEP=1 CUMULUS_REALDATA=cnlaw CUMULUS_SAMPLE=40 \
  go run ./cmd/cumulus eval         # 对照评测：哑臂 + 各臂 diff + 校准读数
go run ./cmd/cumulus selftest       # 一次问答：提交视图 + 路由校准档位

# 外部小语料评测（拉数据 → 裁剪小文档 → 跑分）：见 scripts/prep_cmrc.py
python3 scripts/prep_cmrc.py --download --out ~/datasets/cmrc-small --docs 400 --max-items 200
CUMULUS_REALDATA=local CUMULUS_LOCAL_DIR=~/datasets/cmrc-small go run ./cmd/cumulus eval
```

评测的对照运行**必须有哑臂**（`bm25-bare`：纯 BM25 top-3、零改写、零管理）——缺了
直接红（`dumb-arm-invariant` 门）。

## 下一步（按序）

1. ~~BM25 检索接进 `qaflow` 的 evidence stage~~（已落地：`internal/retrieval`，selftest 可见窗口坐标与原文还原）
2. ~~`evalfcore`：评测 episode~~（已落地：`cumulus eval` 可见三指纹/逐题归因/摘要；判官臂 N/A≠0、成本未知显式计数）
3. ~~`learncore`：受管变更五阶段~~（已落地：`cumulus learn` 可见 diagnose→propose→evaluate→promote 全链路）
4. ~~evalfcore 判官臂接 LLM~~（已落地：`CUMULUS_JUDGE=llm`，判词原文留档供校准）
5. ~~belief 接进候选排序~~（曾接线，后按真语料 A/B 退役——全局声望版 −11pp；`none(belief-retired)` 进提交视图）
6. ~~store 端口补 KV 与查询 + retrieval 从 store 装语料~~（已落地：`internal/corpus` + 摄入后热重建）
7. ~~HTTP 面 + 契约生成~~（已落地：`internal/api` + contract-gen 门）
8. ~~反应式依赖分类器~~（已落地：`context.Activator` + `SemanticRerank`）
9. ~~web 重建~~（部分：单页零构建，问答 + 摄入 + 点引用看原文；六页 IA 其余五页未建）

**下一批（研究线 v0.2 的 ⏳ 项，见 [`docs/flow-grammar.md`](docs/flow-grammar.md) §七）**：

1. 三级信号档位的**优先档**（provider logprobs）+ 用真实校准集定 τ₀（CAUC）与拒答阈值（A-CRC-QA）；
2. 带符号接地（SLC）+ 证据窗口四级分类（GaRAGe 的 ANSWER/RELATED/OUTDATED/UNKNOWN）；
3. 学习护栏量化阈值（EvoC2F：成功率 −2% / P95 +20% / 重试率 +50%）与影子期；
4. 使用信号 → learncore.Observe 的闭环（采集端已就位，消费端未接）。

## 已定的决定（2026-10-07）

- 模块名 `github.com/willove/cumulus`，分支策略：本目录先走 next 分支，稳定后逐步切 main；
- 存储用线上 cumulite（`github.com/willove/cumulite` v0.2.1），经 `internal/store` 端口接入，业务代码不直接依赖它；
- web 维持 Vite + Vue 系，内容按 [`docs/web-audit.md`](docs/web-audit.md) 逐块审计后重建，契约先于页面。

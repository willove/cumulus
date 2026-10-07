# 演进记录（给协作者）

> 状态：v0.1 · 2026-10-07 · 本文记录 cumulus-next 从空目录到三流程落地的过程，
> 重点是**门禁抓到过什么**——那些问题是这个架构和 cumulus 老路的分界线。
> 读法：先读 `architecture.md` 和 `flow-grammar.md` 懂设计，再读本文懂来历。

## 一、时间线

| 轮次 | 内容 | 产出 |
|---|---|---|
| 0 | 空目录搭骨架：两份 SSOT 文档先行，再写执行处 | context / flow / qaflow 骨架 / failure / gates |
| 1 | 接 cumulite（线上版），模块改名 `github.com/willove/cumulus` | store 端口 + 适配器 + KV |
| 2 | web 内容审计（不建页面，先定去留与纪律） | `web-audit.md` |
| 3 | BM25 进 evidence stage（cumulus 最强零件） | retrieval / qaflow.BM25Evidence |
| 4 | 评测 episode（cumulus 评测工作台协议迁移） | evalfcore |
| 5 | 受管变更五阶段 + belief | learncore / knowledge/belief |

## 二、门禁抓到过什么（这才是本文的重点）

搭建过程中门禁（gofmt/vet/test/grammar-conformance）和 demo 一共抓到 9 个真问题。
按“老路会怎样”分组：

### 老 cumulus 会放过的（架构不变拦不住）

| # | 问题 | 怎么被抓住的 | 修法 |
|---|---|---|---|
| 1 | 合成桩产出**无引用的答案** | `SynthesizeStage.Verify` 直接拦下，整个流程反卷 | 桩至少带一条可解析引用，并标注 TODO |
| 2 | store 适配器 upsert **丢 id**：`InsertStructs` 的 id 由引擎生成，我们手里的 id 丢了 | 真存储往返测试（put→get 读到 not-found） | 改走 `Insert` + 显式 `_id`；坑写进注释 |
| 3 | 失败标签挂在**全对的题**上：`unclassified` 出现在 rule=1、evidence=true 的题上，学习周期把“全对”当成“有可学” | `cumulus learn` demo 的 observe 阶段打印出 `failures=map[recall-miss:1 unclassified:2]`——数字对但语义错 | 失败标签只出现在真失败的题（答错/未命中/引用核不掉/拒答，四占一） |
| 4 | 拒答路径在真实查询下才现形：语料加了三个“成本结构”干扰文档后，topk=3 时前三名全是干扰、金标排第四——召回不足是真的，不是编的 | learn demo 的 baseline | 这条不是 bug，是**故意构造的真失败**，让五阶段有真东西可看 |

### 新脚手架自己会犯的（编译期/基础门）

| # | 问题 | 修法 |
|---|---|---|
| 5 | 分词循环写错（`for i := 0+1 < len(runes)`） | 编译期发现，逻辑写对 |
| 6 | 无命中查询返回**非 nil 空切片**，调用方用 nil 判“没有”失效 | 显式返回 nil；测试钉死 |
| 7 | `ruleSum` 类型不匹配（int 累 float） | 编译期 |
| 8 | 未用 import / 未格式化文件 | gofmt/vet 门 |
| 9 | selftest 参数切片越界（`args[1:]` 在已剥离的 slice 上） | 修 Parse 参数 |

### 规律

9 个里 6 个是被**测试或 demo 的真实数据**抓到的，不是人眼。尤其 #3：如果按老路写（失败分类只管分类，不管该不该贴标签），这个问题会潜伏到学习周期真的开跑那天——而 cumulus 的 `learn` 已经在生产跑类似的逻辑。

## 三、立下的规矩（协作者必读，每条都有出处）

1. **端口先行**：业务代码只认 `store.Port`，cumulite 在适配器后面。换存储是换适配器，不动流程。
2. **端口按流程需求长方法**：KV 是 evalfcore 要原子落盘时才加的；不是“引擎有什么就暴露什么”。
3. **不静默夹取**：旋钮提议越界整组拒绝；契约不符整组拒绝。夹取 = 静默改意图。
4. **率地板不用逐题 slack**：同一条护栏规则不随切片大小改含义（cumulus 的教训）。
5. **N/A ≠ 0，成本未知 ≠ 0**：判官没判是 N/A；上游不报 usage 是“成本未知”并停止后续调用。
6. **失败标签只贴真失败**：通过项不许挂失败标签，否则诊断与学习全被污染。
7. **executor 签名里没有金标**：闭卷与防泄漏是结构性的，不靠自觉。
8. **内容寻址**：题集同内容同 ID；指纹三缺一即不可比，只给理由不给数字。
9. **提升 = 注册**：任何受管改动都带逆（旧值），掉线即回滚；回滚不许静默失败。
10. **每轮先写门禁再写功能**：新包的第一件事是让它可被测，第二件才是实现。
11. **工具链两条**（协作者会踩）：命令行 `gofmt -w` / `sed` 改过的文件，编辑器要求重读再改；macOS 的 `sed -i` 语句链易断，批量替换用 Python。

## 三·补、第一轮复盘后补的明显问题（2026-10-07 晚）

复盘（对照最初设计）列出三个明显问题，本轮处理：

| 问题 | 处理 | 证据 |
|---|---|---|
| belief 数学齐了但没接线，学习闭环断半圈 | `qaflow.KeyBelief` 挂点 + `BM25Evidence` 从 context 读信念做候选重排（观测过上浮/没产出下沉/没观测不动，系数有界 [0.5,1.5]）；`learncore.ObserveBelief` 把已完成的评测运行折成观测（只看被引用的文档，没试过没有发言权） | `cumulus learn` 末尾：topk 调回 3、只留信念，证据命中仍 100%——旋钮与信念是两条独立且可叠加的路 |
| usage 写了没人读 | 观测的原料就是运行结果（CitedDocs 进 ItemResult）；候选旋钮花费在 Cycle 里对 Budget 对账 | eval 摘要的 tokens/cost_unknown 一直在读 |
| 可选组件不可见 | 信念绑 context 即声明：`DeclareDependency("belief", ...)`，selftest 打印 `dep: belief -> knowledge.belief available=true`；没绑就是不可见，不是静默换算法 | selftest 输出 |

过程中抓到一个自己的 Patch 漏填（`CitedDocs` 字段加了没填，观测数为 0）——字段在、值不在，demo 一出就露馅。

## 三·补二、隐疾：反应式依赖分类器（2026-10-07 深夜）

复盘指出最大的隐疾：cumulus 的 vocab 桥接说撤就撤、MCS 采样悄悄不触发，
根因是可选组件没有“依赖没了就该停”的运行时判定。本轮补上：

- `context.Activator`：组件登记时声明 `Requires()`；每次绑定/解绑后
  context 按当前状态把组件分类为激活/停用/中性，转换由分类驱动；
  重入（激活回调里再 Set）用深度计数延到最外层一次收敛；
  激活失败保持未激活且错误进状态，停用失败也照常停用（清理不讨价还价）；
- 第一个真实用户 `qaflow.BeliefBooster`：只要 knowledge.belief 绑着；
- selftest 的 status 行从手工声明换成分类器输出：
  `component: belief-booster active`。

顺带删掉上一版的 `DeclareDependency` 手工 API——声明变成了规格 +
转换，不是一行注释。

## 三·补三、合成步从桩到契约（2026-10-07）

问答流程最后一个桩（合成）落地。做法是接口先行：

- `llm.Completer`：模型调用面只认这个接口；Usage 带 CostKnown——
  上游不报就是成本未知，不是 0；
- `qaflow.SynthFunc`：流程级契约（问题 + 窗口 → 带引用的答案 + 用量）；
- `internal/synth` 两个实现：**离线确定性合成**（无 key 环境全链路可跑）
  和 **LLM 合成**（窗口编号化 w1/wN——模型只碰编号碰不到原文坐标；
  输出必须是严格 JSON；断言引用编号解析时换成坐标，挂不上的整体失败）；
- SynthesizeStage 重写：路由 refuse 时不调合成面；非拒答而没有合成面
  是配置错误，直接失败——上一版那个“返回 TODO 文本”的桩删掉了；
- AccountStage 改为读合成步的发生额（KeySynthUsage）——账目和发生额
  同一个来源，不许两处各写一份。

抓到的真问题：批量替换把 eval.go 的函数头吞了（工具失误，不是设计
问题）——重写该文件收场；另有测试替身 usage 零值导致 CostKnown 断言
失败（假件的默认值要和场景匹配）。

## 三·补四、接真模型：MiniMax-M3.1-Flash-Preview（2026-10-07）

第一次把真模型接进来验证（.env：LLM_BASE_URL/LLM_API_KEY/LLM_CHAT_MODEL，
与 cumulus 同款约定）。抓到两个真问题，都不是单元测试能发现的：

1. **证据原文没进提示词**：窗口只带了坐标（docID#rune[a:b]），合成提示词
   列的是“文档/位置/得分”，模型看不到原文——它答“给定证据未提供连接池
   最大连接数”，是对的，它真没看到。修法：检索侧就地解析坐标，
   Hit.SpanText/EvidenceWindow.Text 一路带到提示词。教训：**坐标是引用
   凭据，不是内容；合成面要的是内容**。
2. **判官白嫖账单**：判官 3 次调用的 token 不进账。Judge 接口改为返回
   带用量的 Verdict，ItemResult.JudgeTokens、Summary.TotalJudgeTokens
   一路记账。计费诚实这条纪律，漏一次就是真的在漏钱。

定版（llm.OpenAICompleter，OpenAI 兼容 /chat/completions，httptest 覆盖）：
- 契约解析对齐 cumulus：choices[].message.content + usage；
- 只回推理链不回答案 → 显式失败（不拿 reasoning 当答案）；
- 上游不报 usage → CostKnown=false（成本未知不是 0）；
- 缺 base_url/key/model → ErrNotConfigured，不许静默回落桩。

CLI：selftest -synth offline|llm；eval 走 CUMULUS_SYNTH / CUMULUS_JUDGE。

真跑结果（3 题，全链路）：rule 100%、evidence 100%、citations 100%、
judge 100% (n=3)、tokens(p/c/j)=1003/141/798、cost_unknown=0。

## 三·补五、向量面：端口 + 语义重排组件（2026-10-07）

对齐 harness×RAG 研究后落地（MiniLM 的正确位置：验证层组件，不是检索
底物——文档级 KNN 检索臂 cumulus 已退役，不捡回来）：

- `internal/embed`：Embedder 端口（384 维 L2 归一）+ Cosine/L2Norm。
  唯一实现将是本地 MiniLM（vendor），**没有线上实现**——语料不出边界；
- `internal/retrieval.RerankByCosine`：BM25 收窄后的段落级语义重排。
  两条 cumulus 实测约束写进注释：只比小集合（top-50，MiniLM 甜蜜点）、
  同分保原序（防向量抖动随机化词法序）；
- `internal/qaflow`：`KeyEmbedder` + `SemanticRerank` Activator +
  `KeyRerank` 审计态。三条纪律：embedder 缺席/失败/向量不全 → 保序并
  记 skipped 原因（degraded, not dropped, and visible——cumulus 的
  hash-64 假向量败局的根治）；成功记 applied。

抓到的两个真问题：
1. **Go 泛型推断坑**：`Set(c, Key[接口], 具体实现)` 推断冲突（T 从实参
   推断成具体类型）。接口型 key 的调用方必须显式 `Set[embed.Embedder]`。
   已写进 KeyEmbedder 注释——belief 那个 key 是指针具体型所以没暴露；
2. **禁闭第二次抓到漏声明**：重排写 KeyRerank 但 stage 没声明，测试直接
   红。补声明收场。

MiniLM vendor（权重）与语义接地尺（SynthesizeStage.Verify 的语义版）是
接下来的两步。

## 三·补六、MiniLM 真权重 A/B（2026-10-07 深夜）

向量面全链路接通到真权重（`CUMULUS_AB=1 CUMULUS_EMBED=minilm`，权重在
`~/.cumulus/models/sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2`，
463MB，vendored forward + tokenizer + 归一化表，`internal/minilm/VENDOR.md`）。

**接线过程中抓到的三个真问题**：

1. **执行面自建 context，外层绑定传不进去**：eval 的执行面每题新建
   context（隔离设计），第一版把 embedder 绑在外层 context——rerank 永远
   "embedder not bound"。修法：embedder 作为执行面字段，Answer 里绑；
2. **rerank 静默失败不可见**：一开始 rerank=false 但看不出为什么。加
   `RerankReason` 遥测（too few candidates / embedder not bound / …），
   degraded 必须带原因留痕；
3. **A/B diff 方向标签错**：Compare 给 a−b，打印却标 (rerank−bm25)，取负修。

**真结果（构造语料，3 题含 3 个“成本结构”干扰文档）**：

```
bm25        evidence 66.7%（q3 recall-miss：干扰文档词频压过金标）
bm25+rerank evidence 66.7%  evidence=+0.000  latency=+103ms
```

**诚实的阴性结果**：段落级语义重排没有救回 q3——查询与干扰文档、金标的
 embedding 全都挤在“成本结构”附近，MiniLM-L12 分不开。这与 cumulus 的旧
发现同向：相似文本间区分度差（他们因此退役了 KNN 检索臂）。延迟代价
+103ms/查询是真的（3 次 embed 推理）。

**这个阴性结果的价值**：它把“上向量”从信仰变成问题——
- 玩具语料上下结论不够，下一步必须上真实语料（cn-law-rag 1781 篇）跑
  同一个 A/B；
- 这类失败（同词面近重复干扰）上轮已验证 **belief 路径能救**（观测过
  的零产出文档下沉），可能比语义重排更对症——两个部件治的是不同病：
  belief 治“这个文档历史上没产出”，rerank 治“这个文档内容不相关”，本
  例是前者。

## 三·补七、真实语料 A/B：两个部件的真实成绩（2026-10-08 凌晨）

语料：cn-law-rag finetune_dataset.jsonl（65,783 条“口语问句→法条”，
去重后 22,132 篇法条作检索宇宙），题集：前 300 条 anchor，指标：
证据命中（金标 id 进 top-3 窗口）。三臂同指纹对照。

```
bm25        evidence 74.0%  latency 1ms
bm25+rerank evidence 74.0%  flips 0/0   +142ms
bm25+belief evidence 63.0%  flips 51/18 −11pp
```

### 结论一：段落级语义重排在法律语料上是死重

0 翻转——不是“效果差”，是**动不了**。原因可解释：法条段落的
embedding 彼此几乎相同（同类“第 X 条 …”文本），query 与所有候选的
cosine 挤在一起 → 同分 → 我们的保序规则生效，BM25 序原样返回。
cumulus 退役 KNN 检索臂的旧发现（法条相似文本区分度差）在段落级
重排上重现。**成本是真实的：+142ms/查询。**

### 结论二：belief 有害，而且能指出为什么

51 丢 / 18 赚。根因不在数据，在**我们把 belief 移植错了**：
cumulus 的 belief-update-design 是**单次查询的候选区后验**（DEEP 循环内
“哪些候选文件有产出”，随查询开始重置）；我们的端口把它做成了
**全局文档声望**（跨查询累积）。法律数据集里一篇文章回答很多问题、
竞争文档各不同——全局声望把“常常被错引的热门法条”沉底，轮到它真是
金标时也捞不回来了。

正确形态两种，都要重做：
1. 按查询重置的候选区后验（原设计本尊）：一次查询多轮取证时记住
   哪些候选有产出——治“同一轮内重复翻错文件”；
2. 按会话/历史的复用（“同类问题越问越快”的本意）：同一问题再问时
   直接复用上轮窗口——治“重复问”， toy 场景生效的其实是这个。

### 结论三：BM25 基线本身很能打

74% @ top-3，与 cumulus 当年“BM25 远胜向量检索”的发现一致。向量面
在这个语料上两个用法（检索/重排）都没证明价值——这本身就是对
positioning.md 适用边界的实证补充。

### 下一步

- belief 重做成按查询候选区（对齐原设计），扔掉全局声望版；
- 会话复用路径（同一问题重问直接复用窗口）单独做，那才是“越问越快”；
- rerank 在此语料默认关，留开关等你来翻案；
- 把 flip 统计留在 A/B 输出里（总量持平但翻转不为零 = 部件在重新
  分配风险，这是比均值更细的判据）。

## 三·补八、按诊断修：belief 退役、复用上台（2026-10-08）

按上一轮的诊断执行（不是感觉，是 51 丢 / 18 赚的数字指的路）：

1. **全局声望版 belief 从检索路径上摘掉**：BM25Evidence 不再读信念加权，
   BeliefBoost 删除，BeliefBooster 组件退役（文件留作分类器纪律的注释
   档案），learn 的 belief 演示路退役。belief 包本身留着——朴素后验
   数学没错，错的是把它用成跨查询声望；
2. **按会话复用上台**（belief 原设计的正确形态第一版）：
   `knowledge.ReuseStore` 按会话记"问题→窗口+yield"；qaflow 加
   ReuseStage（evidence 前查，命中直接短路——EvidenceStage 入口处
   让路，不调检索不覆盖窗口）与 ReuseRecordStage（account 后记，
   拒答不记）。归一化只去空白小写——宁可漏命中，不错复用（错复用
   是把错答案当经验）；
3. **复用窗口过同一把契约尺**：SourceID+Span 非空才让复用（Verify）。

验收（selftest 两问并排）：

```
--- first ask (cold)   reuse: hit=false  answer=…citations=[law-1#rune[0:32]]
--- second ask (reuse) reuse: hit=true   answer=…citations=[law-1#rune[0:32]]
reuse store: 1 entries after two asks
```

同会话、同问题：第二问不检索、窗口同上轮。flip 判据也留在了 A/B 输出
里（本语料 belief 臂已退役，两臂：bm25 74.0% / +rerank 74.0% 0 翻转
+118ms——回归确认）。

过程中的真问题：短路第一版没实现（ReuseStage 命中后 EvidenceStage 照
跑覆盖窗口），单元测试的"检索调用次数"判据当场抓住；selftest 的打印
块读错了 context（第一问的状态），并排打印改造时才发现。

## 三·补九、深循环落地 + 一次真实事故：评测把机器打满（2026-10-08）

按"先看流程能不能跑全"做 DEEP：deepcore 包（多轮取证循环：取页→抽窗→
算覆盖→不够展开→死路只记本问）+ qaflow.BM25DeepEvidence + KeyDeep 遥测
（轮次/取样/死路/覆盖度轨迹/停止原因）。

**跑通了**：selftest 演示四族事实问句（"连接数/服务端口/收入/审计周期"），
k=3 首轮只覆盖三族 → 覆盖 0.75 不达标 → 第二轮取进第四篇 → 覆盖 1.0
停；遥测 rounds=2 stop=target。单元测试钉死：单轮回退等价、死路不重复
抽、不可回溯候选丢弃、遥测键禁闭声明。

**真实语料三臂**（预算对齐才可比——窗口数不同，命中高是预算差异不是
部件差异）：

```
bm25     evidence 74.0%   1ms
bm25-k9  evidence 88.3%   1ms     （同预算单轮，纯预算差异）
deep     evidence 88.0%   367ms   （覆盖驱动多轮 + 收敛后一次重排）
```

诚实的结论：**深循环 ≈ 同预算单轮**（88.0 vs 88.3，差异在噪声内）。
原因是覆盖目标 1.0 在口语问句 × 法条文本上几乎不可达——大多数问句
跑满预算停下，最终窗口数 ≈ k=9。深循环的价值不在这个语料上，在
"覆盖可达"的场景（结构化文档、短事实问答）——记录在案，不夸大。

### 事故记录（这是本轮最重要的教训）

我造成了系统异常：`CUMULUS_DEEP=1 CUMULUS_EMBED=minilm` 的评测把
**循环内每一页的候选全部**送进 MiniLM 重排——300 题 × 3 轮 × 最多
50 候选 ≈ 几万次纯 Go CPU 推理，负载打到 29，满跑三四分钟。

修法与纪律（都已落测试）：
1. 循环内不 embed：每页只过 BM25（覆盖信号要的是便宜可重复的词法
   候选）；语义重排只发生在**收敛之后、对最终窗口做一次**；
2. 回归测试 `TestDeepEmbedsOnlyOnceAfterLoop`：多轮跑完 embed 调用
   必须恰好 1 次——违反这条的改动直接红；
3. 修复后延迟 681ms → 367ms/题。

用户的负载告警是对的，我的实现错了。教训：**把重活放进循环之前，先算
总调用量**（题数 × 轮数 × 每页候选数），这个乘法没人替你做。

## 三·补十、重新对齐研究后的第 1 步：充足性路由接真信号（2026-10-08）

按 05/04 对账后重新排的序，第 1 步：把 RouteStage 从"调用方手填置信度"
换成可观测信号（04 落点 1：草稿置信度 + 引用接地）：

- 信号三件套，全部来自流程内状态，没有手填数：
  - 覆盖度（evidence.coverage：查询词的语料内可达比例，语料外词单独上报）
  - 区分度（窗口打分 top1-top2 归一化分差）
  - 死路率（deep 遥测：死路/取样）
- 权重公开可审：0.5 覆盖 + 0.3 区分 + 0.2 死路率；权重是默认值，改它走
  评测对照
- 决策留痕带全部信号值：事后能回答"这次为什么 escalate/refuse"
- 两条检索路（单轮/深循环）都记覆盖度；复用命中回放记录里的覆盖度
  ——第一版没回放，复用的问句覆盖度缺省成 0，被误判 escalate（真 bug，
  已修并留测试）

验收（selftest）：

```
route: fast — draft confidence 0.946 (coverage=1.000 margin=0.819)
route: fast — draft confidence 0.946 (coverage=1.000 margin=0.819)  ← 复用命中，信号回放一致
```

下一步：LLM 回路上真实语料（真合成 + 真判官 + 真语义尺，三个"真"第一次
同时在场）。

## 三·补十一、LLM 回路上真实语料：三个“真”第一次同时在场（2026-10-08）

真 MiniMax 合成 + 真判官 + 真 token 账，cn-law-rag 50 题：

```
items=50 evidence=68.0% citations=100.0%
judge=57.1% (n=28) refused=16 judge_errs=6
tokens(p/c/j)=27735/5014/9833 cost_unknown=0
```

拆开看：28 题作答（16 题判等价）、16 题诚实拒答、6 题判官未判上
（原因逐题留痕）。拒答不是故障——那是 BM25 没召回或证据真不足时
系统的正确出口。

**这一轮真实运行改掉的六个件**（每个都是被真模型/真数据逼出来的，
单元测试发现不了）：

1. **契约没有拒答协议**：模型用自然语言（"现有证据未提供……无法作答"）
   + 空断言表达拒答，被契约判成错误、把整题失败掉。契约加第三字段
   refused:true（answer 与 assertions 必须双空；夹带答案按错误处理），
   提示词明教。真实运行学到的第一条：**拒答是一等结局，不是失败**；
2. **推理模型把输出全放 reasoning_content**：content 空手回来。取推理
   链里最后一个合法 JSON 对象救结构化调用（提取不放松校验）；请求带
   reasoning_effort=low（M3.1 认的轻思考信号，reasoning_split 被
   M3.1 拒收不能发）；
3. **判官只认 YES/NO，中文模型答“是/否”**：50 题只判上 2 题。双语
   判定 + max_tokens 8→64（预算被推理链吃光也是判不上的原因之一）；
4. **金标字段选错**：cn-law 的金标“答案”原来是法条标题——判官拿答案
   对标题，永远不等价（judge 一度 6%）。改成金标段落正文首段；
5. **窗口宽度 60 把法条拦腰截断**：真实运行里模型对碎片证据全部正确
   拒答（judge 6% 的根因）。默认宽度提到 160（法条长度决定下限，换
   语料重测——这正是旋钮该干的事）；
6. **失败标签口径**：规则分是词法基线口径，LLM 答案天然不逐字含金标，
   拿规则分判失败等于把口径当事实（50 题 LLM 臂规则分全 0）。判官
   说对（JudgeOK=true）的题不算失败；拒答不调判官（省 token 且不污染
   判官分母）；拒答数/判官错误数进汇总。

## 三·补十二、重新对齐第 3、4 步：上下文驱逐 + 改写防漂移（2026-10-08）

**第 3 步 上下文驱逐**（04 第 4 条，MiniLM 在研究处方的位置）：
- `internal/ctxmgmt`：按源配额 / 语义近重合并（cosine ≥ 0.92，Volt 阈值）
  / 窗口预算，三步都可关；每个驱逐决定留痕（合并了谁、余弦多少；
  丢了谁、per-source 还是 budget）——裁剪不可见就是 cumulus 的 MCS
  静默不触发同类事故；
- EvictStage 插在证据与路由之间（合成前最后一道上下文管理）；驱逐后
  **覆盖度重算**（路由读驱逐后的事实，拿旧覆盖度做路由是把旧状态当
  新状态）；
- embedder 缺席 → 语义合并不做，配额/预算照做（降级可见不失败）；
- 血泪：复用回放的覆盖度第一版没带词表，驱逐后重算把词表当空、覆盖度
  归零、复用问句被误判 escalate。修：复用记录带完整覆盖度（值+词表+
  语料外词）。

**第 4 步 改写防漂移**（BioHarness：改写管召回，原问管接地）：
- Rewrite 带漂移账：Hypothetical / DriftRejected / DroppedTerms；
- 漂移闸：注入的改写若丢掉原问的**语料内内容词** → 拦下，检索退回原问，
  丢词清单入账；没索引可判时不拦（放行）；
- 检索实际文本走 `Effective()`（过闸才用改写）；覆盖度始终按原问算；
- 生成器（HyDE 式）留作注入件——现在由调用方给，将来接 LLM。

验收：selftest 三问并排（冷/深循环/复用）+ 驱逐账 + 路由信号；
真实语料无回归（bm25 arm 63.3%@30、LLM arm evidence 63.3% /
judge 46.7% (n=15) / refused 12 / judge_errs 3 / 真 token 账在场）。

## 三·补十三、新指标全量跑分（2026-10-08）

重新对齐四步落地后，cn-law-rag 300 题全量：

```
LLM 臂（真 MiniMax 合成 + 真判官 + 真账）：
  evidence=74.0%  citations=100.0%  judge=50.6% (n=160)
  refused=104  judge_errs=36  latency=1778ms
  tokens(p/c/j)=165082/29746/56000  cost_unknown=0
拆解：300 题 = 196 作答 + 104 诚实拒答；196 作答里 160 判上分
（81 题等价 = 全量 27.0% / 作答 41.3%），36 题判官未判上。

检索臂（无模型，宽度 160）：
  bm25   74.0%   0ms
  bm25-k9 88.3%  1ms   （同预算单轮，+43/0 翻转）
  deep   87.7%   3ms   （覆盖驱动多轮，+41/0 翻转）
```

诚实读法：
1. **检索真损失 26%**（k=3）：74.0% 的题金标不在前三——这是合成侧
   一切损失的源头（拒答与判否大多从这来）；
2. **同预算下单轮即可**：k9 88.3% vs deep 87.7%，深循环没有额外价值
   （口语问句×法条的词面覆盖几乎不可达，多数轮跑满预算）；
3. **拒答率 34.7%**：BM25 miss + 模型保守各占一半——拒答是正确出口，
   但它同时说明**提升空间在检索**（88.3% 检索若配上，拒答应大幅下降）；
4. **判官未判上 12%**（36/196）：双语+预算修完仍有一成判不出——判官
   覆盖率本身是可追踪指标；
5. 真金白银：16.5 万 prompt + 2.97 万 completion + 5.6 万判官 token
   /300 题，cost_unknown=0（计费口径干净）。

## 四、现在的样子（2026-10-08）

```
cumulus selftest   一次问答：BM25 → 路由 → 证据窗口坐标 → 引用还原 → 提交视图
cumulus eval       一次评测：三指纹冻结 → 逐题归因 → 摘要（judge=N/A, cost_unknown 诚实计数）
cumulus learn      一次学习：召回不足诊断 → topk 3→4 → 护栏放行 → 提升落盘
```

- 6 个 internal 包 + cmd，3645 行 Go，39 个测试，四道门禁零容错；
- 文档：architecture（架构 SSOT）/ flow-grammar（流程文法 SSOT）/ web-audit（重建前裁判）/ adr-001（不做代码级 HMR）/ 本文。

## 五、接下来（按序，别跳）

1. belief 接进 qaflow 候选排序（Observe 输入已定义，纯接线）；
2. 合成步接 LLM（`SynthesizeStage` 的 TODO 处；答案无证据断言的纪律已由 Verify 把守）；
3. evalfcore 判官臂接 LLM（Judge 接口已在）；
4. HTTP 面 + contract-gen 门（Go 结构体 → OpenAPI → TS 类型）；
5. web 按 `web-audit.md` 重建，第一个页面“问答”；
6. retrieval 从 store 装语料（LoadFromStore）。

## 六、还没验证的（诚实清单）

- 12 类上下文的配额与驱逐参数：抄自 Volt 论文，未在本项目语料实测；
- 失败六分类：迁移自三篇论文，标注方式是新的，未用真实失败集验证过分布；
- learncore 的离线假设规则只有两条，旋钮只有两枚——真跑之前不知道够不够；
- 抽取式基线的 rule 分有意义，但不代表生成式合成的水平。

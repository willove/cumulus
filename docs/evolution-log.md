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

## 四、现在的样子（2026-10-07）

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

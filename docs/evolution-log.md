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

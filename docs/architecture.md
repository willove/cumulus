# cumulus-next 架构

> 状态：草案 v0.1 · 2026-10-07 · 本文是架构的单一事实源，与 `docs/flow-grammar.md`（流程文法）、`docs/evolution-log.md`（来历与规矩）配套
> 前身是 cumulus（同目录 `../cumulus`）。本仓库的存在理由：把 cumulus 五年级到的教训固化成架构，而不是再靠记性。

## 一、这个项目是什么

对持续增长的本地语料做自然语言检索与问答，证据可核、策略可回滚、系统越用越准。继承 cumulus 已验证的能力：BM25 倒排 + 词汇鸿沟改写、FAST/DEEP 分层、证据窗口、知识簇、评测工作台、护栏自学习。

本仓库与 cumulus 的区别不在功能，在**结构**：一切共享状态经过一个 context；一切修改可逆；一切依赖显式；每次处理留提交视图。这四条来自 Cordis 论文（arXiv:2608.25512）的形式化，细节见 `docs/flow-grammar.md`。

## 二、一层地图

```
cmd/cumulus-next/        CLI 与 HTTP 面（面按 registerXFace 登记，契约由 Go 结构体生成）
internal/
  context/               核心：typed key、realm、注册/释放、提交视图、stage 期禁闭
  flow/                  流程框架：stage 契约、runner（禁闭 + 失败反卷 + 记视图）
  failure/               失败模式六分类（评测归因用）
  qaflow/                流程一：一次问答的六个 stage
  evalfcore/             流程二：一次评测运行（episode：冻结/隔离/登记化/原子落盘）
  learncore/             流程三：一次学习周期（受管变更五阶段：白名单旋钮/护栏/提升即注册）
  knowledge/             簇、冲突边、候选区信念（belief 后验）
  evidence/              证据供给：FAST/DEEP、充足性路由、证据窗口、接地检查
  retrieval/             检索后端：BM25、CJK 二元组分词、证据窗口坐标（第一个真零件）
  knowledge/             簇、冲突边、候选区信念
  ingest/                适配器 + 任务状态机
  usage/                 用量台账（红字冲销）
  store/                 存储端口 + cumulite 适配器（业务代码只认 Port）
docs/                    SSOT：架构、流程文法、ADR
scripts/                 门禁
web/                     前端（契约稳定后才动，令牌单一来源）
```

依赖方向单向：`flows → flow → context`；`evidence/retrieval/knowledge` 是被 flow 调度的零件，不许反向依赖 flow。

## 三、六条不变量，以及每条在哪里强制执行

| 不变量 | 执行位置 |
|---|---|
| 一切共享状态绑 key，经过 context | `context.Context` 是唯一可变状态入口；`Set/Get` 之外没有后门 |
| 注册必有逆，卸载 = 逆按 LIFO 回放 | `context.Registration.Release` 非空才收；`Unwind()` 统一反卷 |
| 依赖显式，不可用即停用可见 | stage 的 `Reads()` 即声明；读未绑定的 key 返回类型化错误并进 status 面 |
| 每次迁移记提交视图 | `flow.Runner` 跑完写 `CommittedView`（语料/配置/策略/信念四版本） |
| 出界数据管不了 | 答案与上游调用走 `emission` 记录点；回滚不覆盖，文档明示 |
| 便宜信号优先 | 路由/升级/早停只用 `RouteSignal`（置信度、接地、struggle），贵模型只做合成 |

## 四、从 cumulus 继承的教训（问题 → 对策 → 执行处）

| cumulus 的坑 | 本仓库的对策 |
|---|---|
| 功能逐个灌入，缝越加越多（vocab 建表不接检索、MCS 生产不触发） | 新能力必须是 context 上的注册 + flow 里的 stage；加机制不改 runner。禁闭机制让“接入一半”直接报错 |
| 上下文阈值散落三处（deep/abstain/earlystop 各一份升级判断） | 充足性路由收拢成一个函数 + 配置，decision 留痕进提交视图 |
| 前后端字段漂移（`answer.conf` vs `confidence`，置信度永远显示 0%） | Go 结构体是唯一契约源；门禁生成 OpenAPI 与 TS 类型，漂移即红 |
| 文档不在版本控制内、过期无报警（positioning.md 自陈锚点失效） | docs 全部进 git；文档只引符号路径不引 file:line；新鲜度门禁 |
| UI 四天六版，每版作废上一版 | web/ 后置；启动时先定令牌单一来源；改布局必须先出参考图与要素表 |
| 单文件两千行（deep.go 2212 行事后拆五） | 门禁设单文件行数上限；stage 天然是小单元 |
| 两套评测口径混用 | 评测只有 evalfcore 一条路径；规则臂/判官臂是不同判官注册，不是两套系统 |
| 静默降级（fail-open） | 降级必须显式：状态进 status 面 + 提交视图记版本；不许静默 |

 traditions worth keeping（cumulus 的好东西，原样继承）：门禁文化（断言零容错）、先量再改（插桩读数再动手）、诚实的“不知道”出口、按结果计费的口径、`-ns` 命名空间分域。

## 五、语言栈决策与边界

- **继续用 Go**：cumulite 是 Go、单二进制零服务端是已验证优势、团队熟悉。
- **Go 卸不了代码**：Cordis 的热模块替换在 Go 里没有对应物，本仓库**明确不做代码级 HMR**。可逆性作用于状态注册（缓存、连接、窗口、策略实例）；热插拔 = 换接口实现 + 配置版本号，提交视图记录用的是哪一版。这一条写进 ADR-001，免得以后按论文硬凑。
- **类型化 key**：Go 泛型提供 `Key[T]`，比动态语言的名字约定安全；代价是 key 要集中登记（`internal/context/keys.go`）。
- **禁闭是真检查**：stage 运行期间 context 进入守卫模式，写未声明的 key 直接 error——论文里“禁闭”在 Go 里落成运行期断言 + 测试。

## 六、契约与门禁

```
scripts/gates.sh
  gofmt / vet / test ./...          基础门（零容错）
  grammar-conformance                每个 flow 的 stage 声明与实际读写一致（禁闭测试）
  contract-gen                       Go 结构体 → OpenAPI + TS 类型；产物过期即红
  doc-fresh                          docs 引用的符号路径存在；ADR 编号连续
```

## 七、已定决定与下一步

已定（2026-10-07）：模块名 `github.com/willove/cumulus`，本目录走 next 分支、稳定后切 main；存储用线上 cumulite v0.2.1 经 store 端口接入；web 维持 Vite + Vue，内容按 `docs/web-audit.md` 审计后重建。

下一步（按序）：

1. ~~BM25 检索接进 qaflow 的 evidence stage~~（已落地，含窗口坐标与引用还原）；
2. ~~evalfcore：把 cumulus 的评测工作台协议（冻结三指纹、隔离引擎、原子落盘）迁成 episode~~（已落地）；
3. ~~learncore：受管变更五阶段~~（已落地）；
4. retrieval 从 store 装语料（LoadFromStore），store 补 KV 与查询；
5. 反应式依赖分类器：上下文变化时按声明把组件分类为激活/停用/中性——
   这是 cumulus“vocab 桥接撤除、MCS 悄悄不触发”的直接对治；
6. HTTP 面与契约生成（contract-gen 门）；
7. web 按审计重建，第一个页面是“问答”。

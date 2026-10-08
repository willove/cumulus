# cumulus-next 架构图（评审版）

两图一分层静态结构、二一次问答的数据流；之后是诚实边界。文字描述以
docs/flow-grammar.md（流程）与 docs/architecture.md（分层）为 SSOT，本
文件只画图。

## 对接 invoke-chat（@wil-works/evoke-chat）：adapter，不是 UI

**harness 不做 UI**，界面已经有了：`@wil-works/evoke-chat`（v0.4.1，Vite/Vue）。
harness 一旦自带界面，就会退化成一个内部耦合点；而且层次纪律会直接拦下
（harness 归 capabilities，长出 UI 依赖 = capabilities → apps 反向依赖）。

我们做的是**协议适配**：`internal/harness/evokechat` —— 纯翻译，**零依赖**
（只认 harness 的事件类型 + 标准库），输出"宿主该调用哪些引擎方法、带什么参数"。

**它对齐的契约**（docs/chat/ai-contract.md 的硬规则）：

| 契约要求 | 怎么落 |
|---|---|
| `appendContent` / `appendThinkContent` **分开两个 API**（混写会串行渲染） | `reasoning` → `OpAppendThink`；`content` → `OpAppendContent`（各有测试钉住） |
| 结构化进度走瞬时事件，**别把阶段进度拼进 think 文本** | `stage`/`file` → `OpProgress`，`Transient=true` |
| 事件信封 `{type, seq, time, data}`，**seq 必须连续** | `Envelope()`；瞬时事件（started/stage）不推进游标也不造成缺口 |
| 拿不到窗口容量就不画环 | 只给 `used`，**绝不伪造 `capacity`** |
| 引用面板 | `citations` → `refs[{sourceId,title,text,url,resolved}]` |

**补的两个真缺口**（从契约反查出来的，不是猜的）：
1. `stage` 事件加 **`label`（中文标签）/ `detail`（这一步干了什么）/ `percent`**——
   标签表在 harness（`StageLabel`，未登记的原样返回、**不猜不编**），每个消费方
   都要它，两边各写一份迟早说法不一致；
2. **阶段进度要有分母**：分母取**本次实际注册的阶段数**（装了哪些可选件步数就不同），
   写死常数会让进度条说谎。

**真跑读数**（真模型 + 真语料）：
```
 9.1%  理解问题与检索意图
18.2%  复用上轮证据
27.3%  检索证据窗口        · 取回 1 条窗口
36.4%  核对事实覆盖        · 1/1 条事实已覆盖
54.5%  判断证据够不够      · 路由 escalate · 覆盖 1.00 · 边际 0.00
81.8%  合成答案            · 答案 62 字
100.0%  记录可复用证据
```
详情只写**这一步刚做完时可读到**的东西——不许用后面才产生的数据解释这一步。

**file 事件的两种口径**（`Options.FilesAsToolCalls`）：默认走**进度行**（轻）；
可切成**工具调用卡**（重、可折叠回看原文）。默认轻，是因为契约明确说结构化进度别堆
进 think，而工具卡是结构化呈现、不是文本。

## 思考与正文的真增量（llm.Streamer / StreamSynthFunc）

**已完成**：思考过程与答案正文**双通道实时可看**（此前只有整段）。

**llm 层**（`internal/llm/stream.go`）：`Streamer` 是**可选**接口（`Complete` 不动）。
`Chunk{Reasoning, Content}` **两个通道分开**——混在一个流里，调用方只能靠猜哪段是
思考（真跑踩过：reasoning 混进正文，前端把思考当答案念出来）。三条纪律：

1. **同一请求两条路一致**：流式只是取答案的方式不同，最终 Text 必须与 `Complete`
   逐字段一致（推理链兜底 JSON 提取复用同一份逻辑）；
2. **成本未知不是 0**：流式端点常不回 usage → `CostKnown=false`；
3. **超长单帧不许静默截断**：自己按字节读 SSE 行（不用 `bufio.Scanner` 默认 64KB
   ——300KB 的思考链会直接断在中间且报错难懂）。

**能力必须显式声明**（本轮最大的坑）：`l.Synthesize` 是**方法值**，传进
`Options.Synth` 之后它就是普通函数类型，`SynthesizeStream` 这个方法**在类型层面
已经不存在**，断言永远失败、能力静默消失——真跑表现：端点正常、事件正常、正文仍是
整段 replace，**一眼看不出来**。所以 `Options.StreamSynth` 是显式字段，由接线处
（`pickSynth`）连同 `llm.SupportsStream(c)` 一起声明；上游不支持流式时**不接这条腿**
（免得"接了但每步都退回调"，读数却显示流式开着）。

**读数**（真模型 + 真语料，`-synth llm`）：`content` **13 帧**逐段到达
（`{` → `"answer":"专利法是为了保护` → …），38 帧全部落地、`dropped=0`。
`reasoning` 0 帧——该模型不吐 `reasoning_content`（**能力在，模型没给**，
这件事读数可见，不假装"思考已显示"）。

## 埋点与流式端点（/v1/qa/stream）

事件词表落地成**两处接点**：

**1. 阶段观测（kernel 的洞，不认识上层词表）**
`flow.TraceFunc` = `func(c *Context, name string, phase TracePhase, durMS int64)`，phase ∈
`start/done/fail`。flow 是 kernel，**不许依赖 harness**（反向依赖被 boundary 拦），所以
它只给一个"你可以观测我"的**纯函数洞**；由 `qaflow.Trace`（pipeline→capabilities，合法边）
把相位翻译成事件，并在**证据阶段结束时把窗集发成 `file` 事件**。

为什么窗口挂在 evidence 上：检索是"取到一个就能讲一个"的动作（玩家想看的就是检索
日志）。但 BM25 是一次性返回有序列表，所以事件是**有序批量**发出，**不假装成流式**。

**2. HTTP 端点（可选增强）**
`POST /v1/qa/stream` → `text/event-stream`，帧形状继承旧 cumulus。`/v1/qa` **逐字节
不变**（既有脚本/服务读的是一次性 JSON，改形状等于破坏兼容）。

终态四类事件的顺序有意义：**先引用（证据面）再答案（交付面）**——消费者据此知道
"答案里每条断言都能对上前面那几条引用"。空引用**不发帧**（半截事件比不发更坏，
"有没有引用"在 `done.counts` 里可查）。

**读数（真模型 + 真语料，`/v1/qa/stream`）**：
```
file      #1 6859a9806bda score=5.65 「# 专利法第一条 …」
citations → 6859a9806bda resolved=True 「# 专利法第一条 …」
content   「# 专利法第一条 …制定本法。」
done      route=fast cov=1 counts={'citations':1,'delivered':26,'dropped':0,'windows':1}
data: [DONE]
```
26 帧全部落地（阶段时间线 → 检索日志 → 引用 → 答案 → 收尾），`dropped=0`。

**降级与诚实**：sink 写失败不阻断问答（`Health.Dropped` 记在 `done.counts` 里）；
不能流的服务器（无 `http.Flusher`）**明说 500**，不给"看起来在流其实全缓冲"的端点；
参数错误在**开流之前**就报（否则客户端先收到半截历史）；阶段失败也**收尾**（只有
start 的阶段会让 UI 永远转圈）。

**还没做**：`reasoning`/`content` 的**真增量**需要 `internal/llm` 加 `Stream()`
（provider 层当前是非流式 `Complete()`）。当前 `content` 是整段 `replace=true`——
**诚实的形状**：伪增量比整段更坏。

## 对外输出面：internal/harness（事件流）

harness 的职责是"让人看见系统在干什么"。旧 cumulus 有过一套输出流（OpenAI chat 帧 +
`cumulus` 扩展帧），能力搬到了新骨架，**这个面没搬过来**——现在只有一次性 JSON
响应，思考、引用、进度全压在跑完之后。`internal/harness` 把它补回来并按新纪律重做。

**事件词表**（继承旧 cumulus 的形状，老前端可直接吃；新增 `related`）：

| 事件 | 载荷 | 对应你的需求 |
|---|---|---|
| `started` | 问题、run id | — |
| `stage` | 阶段名 + 相位 + 耗时 | **输出进度**（阶段时间线） |
| `file` | rank / docid / 标题 / 分数 / 坐标 / 预览 / 是否被引用 | **引用的文章**（检索到一个就发一个）+ 检索日志 |
| `reasoning` | 思考片段 | **全部思考过程** |
| `content` | 答案片段（可 `replace` 整段覆盖） | 答案流 |
| `citations` | docid + 坐标 + 原文 + 可回溯标记 | **引用的文章**（终态、可核） |
| `related` | 取回未引用 / 同文档其他片段（why 字段说明理由） | **关联文档** |
| `done` | 答案 + 路由 + 校准 + 计量 + **提交视图** + 决策留痕 | 收尾对账 |
| `error` | 错误文本 | 失败也是事件（然后礼貌收尾） |

**四条契约**（各有测试钉死，不靠人记）：

1. **缺席不改行为**：没挂 sink 时 Emit 是空操作（连 Emitter 都没有也不炸）。
   可选增强不许变成依赖——外部服务缺席不许改变问答结果。
2. **顺序即真相**：`seq` 从 1 单调、`at_ms` 相对起点；事件顺序 = 流程顺序，
   回放能重建时间线（stage 在窗口之前、引用在答案之后）。
3. **降级必须可见**：sink 写失败**不阻断**问答，但计数并可查（`Health.Dropped` +
   原因）；"没接上"与"接上了但丢了事件"分得开。
4. **事件即数据**：每个事件 JSON 往返不变，可单独落库重放（离线回放/评测复用
   同一份）。

**出口**：`SSEStream`（`event: <kind>` + `data: <json>`，带心跳，不依赖 net/http
→ 任何宿主可用）、`Recorder`（内存回放/测试）、`SinkFunc`（普通函数即出口）。
构造器拒绝半截事件——**事件流里出现半截事件比不出事件更坏**。

**分层**：`harness` 归 capabilities，**只依赖 kernel**。它**不许依赖 qaflow / api**
（boundary 门禁会拦），代价是载荷用朴素结构体而不复用 qaflow 类型——这是有意的
解耦成本：harness 必须是"出口"，不能是又一个内部耦合点。

## 分层与依赖方向（internal/arch 门禁，第九道）

`internal/` 下二十几个包此前**没有任何地方声明过"哪个包属于哪层、谁可以依赖谁"**——依赖方向靠人记着，而人记不住（已发生过的漂移：evalfcore 反向依赖 context、synth 依赖 qaflow、judge 依赖 evalfcore）。现在分层写成数据、方向写成规则，由 `internal/arch` 对着**真实导入图**跑一遍（`go list`），违反即红——与 grammar-conformance 同一套路：**不靠评审靠门禁**。

| 层 | 包 | 只能依赖 |
|---|---|---|
| **kernel** | `context` `flow` `marks` `arch` | kernel（自己） |
| **ports** | `store` `decide` | kernel |
| **capabilities** | `abstain` `calib` `corpus` `ctxmgmt` `deepcore` `embed` `evaldata` `evalfcore` `facts` `failure` `ingest` `knowledge`(+`/belief`) `learncore` `llm` `minilm` `prior` `query` `rerank` `retrieval` | kernel / ports / capabilities |
| **pipeline** | `qaflow` `synth` `judge` | 下面三层 |
| **apps** | `api` `cmd/**` | 任何层（但不被任何层依赖） |

**规则**：同层允许（cmd→api、能力件互依）；下层被上层依赖是正常的；**反向禁止**。被这套规则防住的正是三件坏事：底座被业务流程反向依赖、能力件长出对流程的依赖（那样它就不能单独复用）、流程去依赖应用面。

**新增包必须登记归属**（不登记 = 没有约束 = 门禁形同虚设 → `TestEveryInternalPackageIsRegistered` 会红）；表里也不能留幽灵包（`TestNoGhostPackagesInTable`）。`cmd/**` 按前缀归层，新增 cmd 包不必改表。

**为什么不做 Turborepo 之类的编排器**：本项目是**纯 Go 单 module**（无 go.work、无 package.json），Go 自带的构建缓存已经按包/编译单元做得更细；Turborepo 的价值在 JS workspace 的多包任务图，这里**没有多语言边界可编排**，接入只会给 Go 仓库加一条 Node 工具链。要做的是**边界**（本节）和**出口**（`internal/harness` 事件流），不是构建编排。

**分层的直接回报**：`internal/harness`（事件词表 + 问答入口）会成为唯一对外出口，后续项目（UI/别的 agent/CLI）只依赖它，不直接碰 `qaflow`/`context`——整合是"加一个 adapter"，而不是"接进二十几个包"。

## 一、分层结构（静态）

```mermaid
flowchart TB
    subgraph 入口层
        HTTP["HTTP /v1<br/>qa · signal · signals · ingest · doc · health · status"]
        CLI["CLI<br/>serve · ingest · selftest · eval · learn · signals"]
        PAGE["静态页<br/>点引用 → /v1/signal"]
    end

    subgraph 流程层["流程层 qaflow（Cordis 四原语）"]
        CTX["单一中介 context<br/>键的申报驱动（Writes/Reads，漏申报=静默失效）<br/>提交视图四版本：corpus/config/strategy/belief"]
        STAGES["stages：rewrite → evidence → facts → evict → route<br/>→ escalate →〔facts 二次〕→ synthesize → account<br/>条件插件按名字插：abstain(route后) · reuse(evidence前) · learn(account后)"]
    end

    subgraph 支撑层["支撑层（都是注册件，可换底物不动流程）"]
        QUERY["query 查询理解<br/>IDF 主关键词 · 停用/胶水降权<br/>词汇桥 Cached（归一化问句缓存）"]
        RET["retrieval 检索<br/>BM25+CJK 二元组 · 窗口定心<br/>条文吸附 · 事实锚定窗"]
        FACTS["facts 事实层<br/>分解+覆盖判定 · 一致性门 · NearMiss"]
        SYNTH["synth 合成<br/>分层提示词（事实骨架+逐事实窗）<br/>JSON 契约：断言必挂窗"]
        CTXM["ctxmgmt 上下文预算<br/>语义合并（Volt 0.92）+ 事实配额"]
        DEEP["deepcore/prior/abstain<br/>升级取数 · 多信号重排 · 零 LLM 弃权头"]
    end

    subgraph 基础层["基础层（不依赖流程）"]
        CORPUS["corpus 语料<br/>Prepare：解码→规范化→血缘→入库<br/>charset UTF8/GB18030 三纪律"]
        STORE["store 持久化<br/>Port：Doc 带血缘（Encoding/SrcDigest）"]
        KNOW["knowledge 知识<br/>ReuseStore（再问检测）<br/>SignalStore（使用信号）· belief"]
        LLM["llm 上游<br/>MiniMax-M3.1-Flash ·严格 JSON 契约"]
        EMB["embed/minilm<br/>本地 MiniLM 384 维（vendor 权重）"]
    end

    HTTP --> CTX
    CLI --> CTX
    PAGE --> HTTP
    CTX --> STAGES
    STAGES --> QUERY & RET & FACTS & SYNTH & CTXM & DEEP
    STAGES --> CORPUS --> STORE
    STAGES --> KNOW
    SYNTH --> LLM
    CTXM --> EMB
```

## 二、一次问答的数据流（动态）

```mermaid
flowchart LR
    Q["问句"] --> RW["1 rewrite<br/>分析+（条件）假设文档<br/>KeyRewrite/KeyAnalysis"]
    RW --> EV["2 evidence<br/>BM25 取窗（topk·width）<br/>单事实：一查"]
    EV --> F1["3 facts 首判<br/>分解→覆盖→一致性门<br/>KeyFactReport"]
    F1 --> EJ["4 evict<br/>预算裁剪+**事实配额**<br/>KeyWindows（最终窗集）"]
    EJ --> RT["5 route<br/>充足性判据：fast/escalate/refuse<br/>KeyRoute<br/>（条件）abstain 头旁听"]
    RT -->|escalate| ESC["6 escalate<br/>贵路重取（BM25 deep/加权）<br/>KeyEscalation"]
    RT -->|fast| F2["7 facts 二判<br/>按最终窗集重算<br/>（escalate 也走这里）"]
    ESC --> F2
    F2 --> SY["8 synthesize<br/>多事实：提示词=事实骨架+逐事实分组<br/>JSON 契约：断言必挂窗，违例→拒答<br/>（逐事实 fan-out 检索发生在 stage 2：bm25.go 的 retrievePerFact）"]
    SY --> AC["9 account<br/>用量账进 committed view"]
    AC --> RESP["响应=完整记录<br/>答案只是其中一个字段"]
```

## 三、诚实边界（评审时请盯这些）

1. **合成面是 LLM 的**：流水线保证"证据取对、取全、摆对结构"，最终措辞
   与数值提取的稳定性由 MiniMax 决定（Noesis 判定：7B 以下瓶颈在上下
   文利用）。破局链已把可控部分做到头，残余方差是模型固有。
2. **再问即加深（唯一由使用直接驱动的行为）**：同一会话原样再问 =
   用户说"上次不够"——本轮 topk×2、窗宽×1.5、强制升级（词汇桥+贵路
   重取）。复用库从"重放器"退成"再问检测器"：个人工具重放同一个不够
   好的答案是伪需求（一次检索毫秒级，省机器时间赔用户答案）。
3. **停车三件**（小时级、有界收益，未做）：冲突门子情形分流（醉酒
   "五年vs十年"是不同子情形非真冲突）、fact 视图进前端、GBK 语料
   清洗。
4. **无 session 不问复用**：一次性问答（不带 session）不记信号、不进再问
   ——这是选择不是遗漏：没有"再问"的上下文就没有那一族信号。
5. **词面判据的边界已由判官兜底**：覆盖判据（内容词占比+3 字核心）认
   不出改写，未盖事实问一次模型（只升级不降级、失败不阻塞、裁决可
   见——judge 字段）。 Patent 两连问全链因此通了：锚点落第四十二条 →
   判官救回 f1 → 分组带判官裁决 → 模型给出"二十年/十年/十五年"。

## 四、数字（2026-10-08）

25 个包（23 个有测试；无测试的两个是 minilm 的 vendored forward 与 flow 的 runner）·
**八门禁全绿**（gofmt/vet/test/grammar-conformance/dumb-arm-invariant/contract-gen/
doc-fresh/file-size）· 唯一外部依赖：MiniMax API（合成/桥）与 ~/.cumulus/models
的 MiniLM 权重。

提交视图（v0.2 §五.4）已上服务面：`/v1/qa` 的 `committed` 字段带四版本 +
路由校准档位（语料版本 = 内容摘要，改一个字就变）；路由信号档位记
`retrieval`（provider 不返回 logprobs，优先档未接）；评测的对照运行强制
含哑臂 `bm25-bare`（零改写零管理）。

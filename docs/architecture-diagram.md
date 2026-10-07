# cumulus-next 架构图（评审版）

两图一分层静态结构、二一次问答的数据流；之后是诚实边界。文字描述以
docs/flow-grammar.md（流程）与 docs/architecture.md（分层）为 SSOT，本
文件只画图。

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

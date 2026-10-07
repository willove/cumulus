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
        KNOW["knowledge 知识<br/>ReuseStore（按会话复用）<br/>SignalStore（使用信号）· belief"]
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
    F2 --> SY["8 synthesize<br/>多事实：逐事实 fan-out 检索+事实锚定窗<br/>提示词=事实骨架+逐事实分组<br/>JSON 契约：断言必挂窗，违例→拒答"]
    SY --> AC["9 account<br/>用量账进 committed view"]
    AC --> RESP["响应=完整记录<br/>答案只是其中一个字段"]
```

## 三、诚实边界（评审时请盯这些）

1. **合成面是 LLM 的**：流水线保证"证据取对、取全、摆对结构"，最终措辞
   与数值提取的稳定性由 MiniMax 决定（Noesis 判定：7B 以下瓶颈在上下
   文利用）。破局链已把可控部分做到头，残余方差是模型固有。
2. **信号面只观测不改行为**：reask 族/cite 族落盘可聚合，但"用信号做
   重排"必须是 learncore 的受管变更（白名单旋钮+冻结评测对照），不在
   这里自动接。
3. **停车三件**（小时级、有界收益，未做）：冲突门子情形分流（醉酒
   "五年vs十年"是不同子情形非真冲突）、fact 视图进前端、GBK 语料
   清洗。
4. **无 session 不问复用**：一次性问答（不带 session）不记信号不复用
   ——这是选择不是遗漏：没有"再问"的上下文就没有那一族信号。
5. **词面判据的边界**：覆盖判据是内容词占比+3 字核心，认不出深度改写
   （"期限是多少年"vs"期限为二十年"已用内容词齐聚锚绕过）；彻底解法
   是 LLM 事实感知评分（cumulus 之路），未搬。

## 四、数字（2026-10-08）

25 个包（22 个有测试）· 七门禁全绿 · 37 提交 · 唯一外部依赖：
MiniMax API（合成/桥）与 ~/.cumulus/models 的 MiniLM 权重。

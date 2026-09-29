// 会话、请求和 SSE 的生命周期都在此管理；各输入入口只需调用 onSend。
//
// 传输层走组件规范：后端 /v1/chat/completions 说 OpenAI Chat Completions 流
// （evoke-chat 的 openai 适配器直接可吃），本文件按包的推荐接法用
// useChatSession + 自定义 transport 组装——openai 适配器的纯函数负责标准块
// （delta.content / finish / usage），cumulus 扩展块（stage/citations/done）
// 由 foldExtension 折进消息附属数据。
//
// 每条 assistant 消息自带四份随答案持久化的附属数据（刷新后从服务端会话
// 文档恢复）：sources（引用窗口）、stats（运行卡）、stages（分步时间轴）、
// createdAt（时间戳）。
import { ref, onMounted, onUnmounted, nextTick, watch } from "vue";
import { useChatEngine, useChatSession, openai, readSseFrames } from "@wil-works/evoke-chat";
import { nsSel, pane, withNS } from "../state.js";
import { api, requestJSON, jsonPost } from "../api.js";

// 服务端 stage 键 → 中文名。顺序即流水线顺序（时间轴按它排序）。
export const STAGE_ORDER = ["analyze", "cascade", "sample", "synth", "deep_sample", "deep_synth"];
const STAGE_TEXT = {
  started: "正在分析问题与检索意图",
  working: "",
  sampling: "正在采样证据窗口",
  synthesize: "正在合成答案",
  refused: "语料中没有能回答这个问题的依据，正在整理最接近的条文",
  analyze: "分析问题与检索意图",
  cascade: "关键词级联排序候选文档",
  sample: "采样并评分证据窗口",
  synth: "合成答案",
  deep_sample: "深度采样：逐篇收容评分",
  deep_synth: "深度合成答案",
};
export function stageText(name) { return STAGE_TEXT[name] || name; }
export function fmtMS(ms) { return ms >= 1000 ? (ms / 1000).toFixed(1) + "s" : ms + "ms"; }

// token 去向的分段名 → 人话（引擎侧键：rewrite/fast/decompose/rank/score/
// widen/synth）。只列出现的段；这段是注脚级数据，标签必须自解释。
const TOKEN_STAGE_TEXT = {
  rewrite: "改写", fast: "快答", decompose: "拆解", rank: "排序",
  score: "评分", widen: "扩展", synth: "合成",
};
export function fmtTokens(n) { return n >= 10000 ? (n / 1000).toFixed(1) + "K" : String(n || 0); }
export function tokenSegmentsOf(stats) {
  const st = stats?.stages_tokens;
  if (!st || typeof st !== "object") return undefined;
  const segments = [];
  for (const [key, label] of Object.entries(TOKEN_STAGE_TEXT)) {
    if (st[key] > 0) segments.push({ label, tokens: st[key] });
  }
  return segments.length ? segments : undefined;
}

// usageOf 把运行卡折成 EbChatUsage 的形状：总量 + 分段（披露阶梯），并给
// 输入/输出一个诚实映射——合成是输出侧，其余（检索/评分/排序…）是输入侧。
// 没有分段数据时输入/输出留 0（只显示总量），不编造拆分。
export function usageOf(stats) {
  if (!stats) return null;
  const total = Number(stats.tokens) || 0;
  const st = stats.stages_tokens;
  const completion = st && typeof st === "object" ? Math.min(Number(st.synth) || 0, total) : 0;
  return {
    totalTokens: total,
    promptTokens: st ? total - completion : 0,
    completionTokens: st ? completion : 0,
    segments: tokenSegmentsOf(stats),
  };
}

// SSE citations 事件与会话恢复共用一条引用映射：EbChatSources 吃
// {index, title, snippet, source}；未定位的窗口在标题上明说，不藏在交互里。
export function mapRef(r) {
  return {
    index: r.index,
    title: (r.title || r.source_id || "") + (r.span ? " · " + r.span : ""),
    snippet: r.quote, source: r.source_id, resolved: r.resolved,
    status: r.resolved === false ? "未定位" : undefined,
  };
}

// 一条消息的时间轴视图：live 阶段（正在跑）以 status=streaming 的消息为准，
// 用实时事件累积；完成后用 done 事件里的权威分段（微秒 → 毫秒）重算。
export function timelineFor(message) {
  const live = message.status !== "done" && message.status !== "error";
  if (!live && message.stats?.stages) {
    const ms = Object.entries(message.stats.stages).map(([name, us]) => ({ name, ms: Math.round(us / 1000) }));
    ms.sort((a, b) => STAGE_ORDER.indexOf(a.name) - STAGE_ORDER.indexOf(b.name));
    return ms;
  }
  return message.stages || [];
}

export function useChatPane() {
  const sessions = ref([]);
  const sessionsBusy = ref(false);
  const current = ref("");
  const loading = ref(false);
  const error = ref("");
  const elapsed = ref(0); // 秒；来自服务端 status 事件，不本地计时（连接断了就该停）
  // stats 是最后一次完成的运行卡：消息级 stats 负责展示与历史恢复，这份全局
  // 副本供程序化消费（门测试与将来的面板）——契约是「后端指标一条不丢」。
  const stats = ref(null);
  // meta 是当前阶段文案：消息级时间轴接管了展示，这份保留给门测试契约
  // （失败/取消时必须回到空串）。
  const meta = ref("");
  // sources 是最后一次回答的引用全集：消息级 sources 负责展示与历史恢复，
  // 这份全局副本供程序化消费（门测试契约：引用一条不丢）。
  const sources = ref([]);
  // 实时检索进度：stage 事件一边完成一边推到这里，悬浮进度条直接读——
  // 消息本体在 pending 阶段只渲染 loading（内容槽没挂载），进度必须活在这。
  const liveStages = ref([]);
  const liveStage = ref(""); // 正在进行的阶段（上一段完成后的推论）
  const engine = useChatEngine();
  const { messages, addUserMessage, createAssistantMessage, appendContent, appendThinkContent,
    updateMessage, completeMessage, stopThinking, setMessageError, cancelMessage } = engine;
  const box = ref(null);
  let disposed = false;
  let epoch = 0;
  let viewRequest = null;
  const sessionRequests = new Set();

  function scroll() {
    nextTick(() => { if (!disposed && box.value) box.value.scrollTop = box.value.scrollHeight; });
  }
  async function sessionJSON(op, url, options = {}) {
    sessionRequests.add(op);
    sessionsBusy.value = true;
    try {
      return await requestJSON(url, { ...options, signal: op.controller.signal });
    } finally {
      sessionRequests.delete(op);
      sessionsBusy.value = sessionRequests.size > 0;
    }
  }
  function operation() {
    return { epoch, ns: nsSel.value, controller: new AbortController() };
  }
  function valid(op) {
    return !disposed && op.epoch === epoch && op.ns === nsSel.value && !op.controller.signal.aborted;
  }
  let sending = false;
  function stop() {
    if (!sending) return; // 切视图/开旧会话时无在飞请求：静默清场，不写「已取消」
    void chatSession.stop();
    elapsed.value = 0; meta.value = ""; stats.value = null; sources.value = [];
    liveStages.value = []; liveStage.value = "";
    loading.value = false;
    error.value = "已取消";
  }
  function switchView() {
    stop();
    epoch++;
    for (const op of sessionRequests) op.controller.abort();
    sessionRequests.clear();
    sessionsBusy.value = false;
    listRequest = viewRequest = null;
  }
  function clearConversation() {
    current.value = "";
    messages.value = [];
  }
  function showError(e) { error.value = "出错：" + e.message; }
  // 会话 id 由前端生成，服务端在第一次成功落库时才真正建会话（appendTurn 的
  // ensure）。原来「先 POST 建会话再检索」会留下空壳：检索中断/失败时，列表里
  // 多出一个 messages 为空的会话，点进去什么都没有——这正是用户报的
  // 「历史会话点进去无法打开会话内容」。惰性建会话把这一类空壳整体消掉。
  function newClientSessionID() {
    const bytes = new Uint8Array(6);
    (globalThis.crypto || {}).getRandomValues?.(bytes);
    if (!bytes.some(Boolean)) for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
    return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
  }
  function backendError(d, fallback) {
    return d?.error ? String(d.error) + (d.hint ? "（" + d.hint + "）" : "") : fallback;
  }
  // 扩展块落点：cumulus wire 扩展（stage/citations/replace/done）不是会话层
  // 的事件类型，直接折进消息附属数据与全局副本。返回 error 字符串时由
  // transport 抛给会话层的错误路径。
  function ensureWireMessage(id) {
    if (messages.value.some((m) => m.id === id)) return id;
    const created = createAssistantMessage();
    updateMessage(created.id, { id });
    return id;
  }
  function foldExtension(ext, messageId) {
    const p = ext?.payload || {};
    const m = () => messages.value.find((x) => x.id === messageId);
    switch (ext?.kind) {
      case "stage": {
        const row = { name: p.name || "", ms: p.ms || p.stage_ms || 0, elapsed_ms: p.elapsed_ms || 0 };
        liveStages.value = [...liveStages.value, row];
        const idx = STAGE_ORDER.indexOf(row.name);
        liveStage.value = idx >= 0 && idx + 1 < STAGE_ORDER.length ? STAGE_ORDER[idx + 1] : "";
        ensureWireMessage(messageId);
        const msg = m();
        if (msg) {
          msg.stages = [...(msg.stages || []), row];
          if (msg.thinking || msg.status === "pending") {
            updateMessage(msg.id, { thinkContent: stageText(row.name) + " 完成" + (row.ms ? " · " + fmtMS(row.ms) : "") });
          }
        }
        if (p.elapsed_ms) elapsed.value = Math.round(p.elapsed_ms / 1000);
        return "";
      }
      case "citations": {
        const refs = (p.refs || []).map(mapRef);
        sources.value = refs;
        ensureWireMessage(messageId);
        const msg = m();
        if (msg) msg.sources = refs;
        return "";
      }
      case "replace": {
        updateMessage(ensureWireMessage(messageId), { content: p.text || "" });
        scroll();
        return "";
      }
      case "done":
        return p; // 运行卡延迟落：[DONE] 哨兵到达前流仍可能截断，截断不留半套数据
      case "flag":
        if (p.insufficient) insufficientSeen = true;
        return "";
      case "error":
        return backendError(p, "流式检索失败");
      default:
        return "";
    }
  }
  function foldDone(p, messageId) {
    const card = {
      mode: p.mode || "", conf: p.conf ?? 0, coverage: p.coverage ?? 0,
      loops: p.loops || 0, widened: p.widened || 0, tokens: p.tokens || 0,
      latency: p.latency_ms || 0, reused: !!p.reused,
      cluster_id: p.cluster_id || "", stop_reason: p.stop_reason || "",
      refused: !!p.refused, insufficient: !!insufficientSeen,
      stages: p.stages || null,
      stages_tokens: p.stages_tokens || null,
    };
    ensureWireMessage(messageId);
    const msg = messages.value.find((x) => x.id === messageId);
    if (msg) msg.stats = card;
    stats.value = card;
    if (p.session_error) error.value = "答案已生成，但这一轮未写入会话历史：" + p.session_error;
  }
  let insufficientSeen = false;
  // 兼容旧 wire 的标志（insufficient-evidence 在 chat wire 上并成 done 前置位）
  // ——chatwire 的 done payload 不带它时保持 false。

  // 组件规范 transport：openai 适配器纯函数吃标准块，cumulus 扩展块走
  // foldExtension；abort 统一发 turn/end(aborted)，让会话层收干净尾。
  let wireAbort = null;
  let wireMessageId = "";
  function makeTransport() {
    let sink = null;
    return {
      open({ onEvent } = {}) {
        sink = onEvent || null;
        return () => { sink = null; };
      },
      async page() { return []; },
      async send({ requestId, content }) {
        const text = (content || []).map((part) => part.text || "").join("");
        const messageId = wireMessageId || "m-" + requestId;
        insufficientSeen = false;
        const ctl = wireAbort = new AbortController();
        // 轮次守卫：发送一刻的 epoch/库。切视图、换库、停止之后，迟到的
        // 响应与帧一律按中止处理——它们属于已经翻篇的一问。
        const myEpoch = epoch, myNs = nsSel.value;
        const stale = () => myEpoch !== epoch || myNs !== nsSel.value || ctl.signal.aborted;
        // 用户主动停止（同轮次内 abort）要广播 aborted 让气泡收尾；切视图/
        // 换库导致的迟到轮次静默丢弃——消息列表已被替换，事件无处安放。
        const bail = () => {
          if (ctl.signal.aborted && myEpoch === epoch && myNs === nsSel.value) {
            sink?.({ type: "turn/end", transient: true, data: { messageId, reason: { kind: "aborted" } } });
          }
        };
        // wire 上的历史：本会话此前的 user 轮（不含刚追加的本问）。
        const priorUsers = messages.value.filter((m2) => m2.role === "user" && m2.status === "done").map((m2) => m2.content);
        const history = priorUsers.slice(0, -1).slice(-6);
        let resp;
        try {
          resp = await api.chatCompletions({
            model: "cumulus",
            messages: [...history.map((c) => ({ role: "user", content: c })), { role: "user", content: text }],
            stream: true,
            session: current.value || undefined,
            ns: nsSel.value || undefined,
          }, wireAbort.signal);
        } catch (e) {
          if (e?.name === "AbortError" || wireAbort?.signal.aborted) {
            sink?.({ type: "turn/end", transient: true, data: { messageId, reason: { kind: "aborted" } } });
            return;
          }
          throw e;
        }
        if (!resp.ok || !resp.body || /\bjson\b/i.test(resp.headers.get("content-type") || "")) {
          let d;
          try { d = await resp.json(); } catch {}
          throw new Error(backendError(d, !resp.ok ? "HTTP " + resp.status : "响应不是 SSE 流"));
        }
        // 中止必须同时解开阻塞中的 reader.read()：abort 信号叫不醒它，
        // cancel 要走自持的 reader（流一旦 getReader 即锁定，body.cancel
        // 会抛 Invalid state）。监听器晚于中止挂上的竞态用即时分支兜住。
        // 打捞体：截断且一帧未收时，网关可能整包回了 JSON 错误对象。克隆
        // 必须在流被读取之前做（getReader 即锁定），但只在截断（流已尽）
        // 时才读——流式响应会一直等到流结束。tee 的源 cancel 要两条分支都
        // 取消：readSseFrames 经 signal 管主分支，克隆分支在这里补。
        const salvageClone = typeof resp.clone === "function" ? resp.clone() : null;
        const cancelClone = () => { try { salvageClone?.body?.cancel?.()?.catch?.(() => {}); } catch {} };
        if (ctl.signal.aborted) cancelClone();
        else ctl.signal.addEventListener("abort", cancelClone);
        let sawDone = false; // 终结哨兵只认 [DONE]：done 扩展块是数据，不是终止符
        let rawText = "";
        let pendingDone = null;
        try {
          const state = openai.createState();
          for await (const frame of readSseFrames(resp.body, { signal: ctl.signal })) {
            if (stale()) { bail(); return; }
            if (frame.done) { sawDone = true; break; }
            rawText += frame.data + "\n";
            let chunk = null;
            try { chunk = JSON.parse(frame.data); } catch { throw new Error("响应帧不是合法 JSON"); }
            for (const ev of openai.frameToEvents(frame, state, { messageId })) sink?.(ev);
            if (chunk?.cumulus) {
              const out = foldExtension(chunk.cumulus, messageId);
              if (typeof out === "string" && out) throw new Error(out);
              if (out && typeof out === "object") pendingDone = out;
            }
            scroll();
          }
          // [DONE] 早退：生成器归还后取消剩余响应体（tee 源 cancel 需两条
          // 分支——reader 一条已随生成器结束，克隆分支在 abort 监听里，这里
          // 再兜主 body；已锁定/已结束时是静默空操作）。
          try { await resp.body.cancel?.(); } catch {}
          if (stale()) { bail(); return; }
          if (!sawDone) {
            // 网关有时不带 JSON content-type、也没有帧结构，直接回错误对象：
            // 截断报错前先打捞（帧内残文与克隆体两条路都试）。
            let salvageText = "";
            if (salvageClone) { try { salvageText = await salvageClone.text(); } catch {} }
            for (const salvage of [rawText.trim(), salvageText.trim()]) {
              if (!salvage || !salvage.startsWith("{")) continue;
              try {
                const d = JSON.parse(salvage);
                if (d?.error) throw new Error(backendError(d, "响应流已截断"));
              } catch (e) { if (!(e instanceof SyntaxError) && e?.message && !e.message.includes("截断")) throw e; }
            }
            throw new Error("响应流已截断：未收到 done 哨兵（[DONE]）");
          }
          // 成功收尾才取消克隆分支（tee 源要双分支取消）；截断路径上方
          // 还要读它打捞错误体，先读后弃。
          cancelClone();
          if (pendingDone) foldDone(pendingDone, messageId);
          for (const ev of openai.finalize(state, { messageId })) sink?.(ev);
        } catch (e) {
          if (e?.name === "AbortError" || wireAbort?.signal.aborted) {
            sink?.({ type: "turn/end", transient: true, data: { messageId, reason: { kind: "aborted" } } });
            return;
          }
          throw e;
        } finally {
          if (wireAbort === ctl) wireAbort = null; // 迟到的旧请求不得摘掉新请求的控制器
        }
      },
      async cancel() { wireAbort?.abort(); },
    };
  }
  const chatSession = useChatSession({ engine, transport: makeTransport() });
  // open() 把会话层的事件出口接进 transport——不调它，sink 永远是 null，
  // 标准事件（增量/收尾）全部落空，只有直改引擎的扩展折叠会生效。
  chatSession.open?.();

  let listRequest = null;

  async function loadSessions() {
    if (disposed) return;
    if (!nsSel.value) { sessions.value = []; return; }
    if (listRequest) {
      listRequest.controller.abort();
      sessionRequests.delete(listRequest);
    }
    const op = operation();
    listRequest = op;
    try {
      const d = await sessionJSON(op, withNS("/v1/sessions", op.ns));
      if (!valid(op) || listRequest !== op) return;
      if (d !== null && !Array.isArray(d)) throw new Error("会话列表响应格式无效");
      sessions.value = d || [];
    } catch (e) {
      // 后台列表刷新不能覆盖本轮检索的错误或取消说明。
      if (valid(op) && listRequest === op && !error.value) showError(e);
    } finally {
      if (listRequest === op) listRequest = null;
    }
  }

  async function openSession(s) {
    if (disposed) return;
    switchView();
    clearConversation();
    const op = operation();
    viewRequest = op;
    try {
      const d = await sessionJSON(op, withNS("/v1/sessions/" + encodeURIComponent(s.id), op.ns));
      if (!valid(op)) return;
      if (!d || (d.messages != null && !Array.isArray(d.messages))) throw new Error("会话响应格式无效");
      current.value = s.id;
      // 恢复每条消息的附属数据：引用、运行卡、时间轴、时间戳——服务端会话
      // 文档现在随答案一起存了这些（sessionMessage 的 sources/stats），
      // 刷新后重新打开会话，答案下面的引用卡和分段耗时原样回来。
      messages.value = (d.messages || []).map((m, i) => ({
        id: s.id + "-" + i, role: m.role, content: m.content, status: "done",
        createdAt: m.at || undefined,
        sources: Array.isArray(m.sources) ? m.sources.map(mapRef) : [],
        // 持久化字段是 latency_ms，界面运行卡读 latency：归一化，别让刷新后
        // 总耗时变成 0.0s。
        stats: m.stats ? { ...m.stats, latency: m.stats.latency ?? m.stats.latency_ms ?? 0 } : null,
        stages: m.stats?.stages
          ? Object.entries(m.stats.stages).map(([name, us]) => ({ name, ms: Math.round(us / 1000) }))
          : [],
      }));
    } catch (e) {
      if (valid(op)) showError(e);
    } finally {
      if (viewRequest === op) viewRequest = null;
    }
  }

  // 新会话同样是惰性的：清空视图并给一个本地 id，第一次成功提问后服务端才落库。
  // 否则点一下「新会话」就在列表里多一个空会话。
  async function newSession() {
    if (disposed) return;
    switchView();
    clearConversation();
    current.value = newClientSessionID();
    void loadSessions();
  }

  async function delSession(id, e) {
    e?.stopPropagation();
    if (disposed) return;
    const deletingCurrent = current.value === id;
    if (deletingCurrent) switchView();
    error.value = "";
    const op = operation();
    if (deletingCurrent) viewRequest = op;
    try {
      await sessionJSON(op, "/v1/sessions/" + encodeURIComponent(id), {
        ...jsonPost({ ns: op.ns || undefined }), method: "DELETE",
      });
      if (!valid(op)) return;
      if (current.value === id) clearConversation();
      await loadSessions();
    } catch (err) {
      if (valid(op)) showError(err);
    } finally {
      if (viewRequest === op) viewRequest = null;
    }
  }

  async function onSend(text) {
    text = String(text ?? "").trim();
    if (!text || loading.value || viewRequest || disposed) return;
    if (!nsSel.value) { error.value = "请先创建或选择知识库"; return; }
    error.value = "";
    elapsed.value = 0; meta.value = ""; stats.value = null; sources.value = [];
    liveStages.value = [];
    liveStage.value = "analyze"; // 首段完成事件到达前也要有进度
    // eb-chatbot 已经写入用户消息；首页/示例按钮则没有。只去重末条同文 user。
    const last = messages.value[messages.value.length - 1];
    if (last?.role !== "user" || last.content !== text) addUserMessage(text);
    // 惰性会话 id：检索中断不会留下谁也打不开的空会话。
    if (!current.value) current.value = newClientSessionID();
    loading.value = true;
    sending = true;
    try {
      // 预建本问的 assistant 消息：wire 事件（stage/增量）都指向它，思考占位
      // 让首帧到达前就有可见进度；附属数据随答案走（刷新后可恢复）。
      wireMessageId = createAssistantMessage().id;
      const m0 = messages.value.find((m) => m.id === wireMessageId);
      if (m0) { m0.sources = []; m0.stats = null; m0.stages = []; }
      appendThinkContent(wireMessageId, "检索私域语料并评分证据窗口……");
      await chatSession.submit(text);
      // 会话层把错误记在消息上而不抛出（幂等重发的契约）——从消息态回捞，
      // 让全局 error 说明与消息状态一致。
      const m = messages.value.find((x) => x.id === wireMessageId);
      if (m?.status === "error" && m.error) showError({ message: m.error });
      if (m?.status !== "error") void loadSessions();
    } catch (e) {
      showError(e);
      meta.value = ""; stats.value = null; sources.value = [];
      if (wireMessageId) {
        stopThinking(wireMessageId);
        if (e.name === "AbortError") cancelMessage(wireMessageId);
        else setMessageError(wireMessageId, e.message);
      }
    } finally {
      sending = false;
      loading.value = false;
      wireMessageId = "";
      scroll();
    }
  }

  function resetForNs() {
    switchView();
    clearConversation();
    sessions.value = [];
    void loadSessions();
  }
  watch(nsSel, resetForNs, { flush: "sync" });
  onMounted(loadSessions);
  onUnmounted(() => {
    switchView();
    chatSession.dispose?.();
    disposed = true;
  });

  return {
    sessions, sessionsBusy, current, loading, error, elapsed, stats, meta, sources,
    liveStages, liveStage,
    messages, onSend, stop, loadSessions, openSession, newSession, delSession, resetForNs,
    pane, nsSel,
  };
}

// 会话、请求和 SSE 的生命周期都在此管理；各输入入口只需调用 onSend。
//
// 每条 assistant 消息自带四份随答案持久化的附属数据（刷新后从服务端会话
// 文档恢复）：sources（引用窗口）、stats（运行卡：模式/置信度/token/耗时）、
// stages（分步时间轴：实时事件流 + done 的权威分段）、createdAt（时间戳）。
// 这些曾经只活在浏览器内存里——刷新即丢，正是「历史问答的引用和数据看不见」。
import { ref, onMounted, onUnmounted, nextTick, watch } from "vue";
import { useChatEngine } from "@wil-works/evoke-chat";
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
  // 实时检索进度：stage 事件一边完成一边推到这里，头部进度面板直接读——
  // 消息本体在 pending 阶段只渲染 loading（内容槽没挂载），进度必须活在这。
  const liveStages = ref([]);
  const liveStage = ref(""); // 正在进行的阶段（上一段完成后的推论）
  const { messages, addUserMessage, createAssistantMessage, appendContent, appendThinkContent,
    updateMessage, completeMessage, stopThinking, setMessageError, cancelMessage } = useChatEngine();
  const box = ref(null);
  let disposed = false;
  let epoch = 0;
  let active = null;
  let listRequest = null;
  let viewRequest = null;
  const sessionRequests = new Set();

  function scroll() {
    nextTick(() => { if (!disposed && box.value) box.value.scrollTop = box.value.scrollHeight; });
  }
  function valid(op) {
    return !disposed && op.epoch === epoch && op.ns === nsSel.value && !op.controller.signal.aborted;
  }
  function operation() {
    return { epoch, ns: nsSel.value, controller: new AbortController() };
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
  function closeReader(op) {
    if (op.reader) {
      // cancel also releases a pending read when the server stops sending bytes.
      void op.reader.cancel().catch(() => {});
    }
  }
  function stop() {
    const op = active;
    if (!op) return;
    active = null;
    op.controller.abort();
    closeReader(op);
    sessionRequests.delete(op);
    sessionsBusy.value = sessionRequests.size > 0;
    if (op.message) {
      stopThinking(op.message.id);
      cancelMessage(op.message.id);
    }
    loading.value = false;
    elapsed.value = 0; meta.value = ""; stats.value = null; sources.value = [];
    liveStages.value = []; liveStage.value = "";
    error.value = "已取消";
  }
  function switchView() {
    stop();
    epoch++;
    for (const op of sessionRequests) op.controller.abort();
    sessionRequests.clear();
    sessionsBusy.value = false;
    listRequest = viewRequest = null;
    elapsed.value = 0; meta.value = ""; stats.value = null; sources.value = [];
    liveStages.value = []; liveStage.value = "";
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
        sources: Array.isArray(m.sources) ? m.sources.map((r) => ({
          index: r.index, title: r.title || r.source_id, snippet: r.quote,
          source: r.source_id, resolved: r.resolved,
        })) : [],
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
    } catch (e) {
      if (valid(op)) showError(e);
    } finally {
      if (viewRequest === op) viewRequest = null;
    }
  }
  function assertActive(op) {
    if (!valid(op) || active !== op) {
      const e = new Error("已取消");
      e.name = "AbortError";
      throw e;
    }
  }
  function backendError(d, fallback) {
    return d?.error ? String(d.error) + (d.hint ? "（" + d.hint + "）" : "") : fallback;
  }
  async function readAnswer(op, resp) {
    if (!resp.ok || !resp.body || /\bjson\b/i.test(resp.headers.get("content-type") || "")) {
      let d;
      try { d = await resp.json(); } catch {}
      throw new Error(backendError(d, !resp.ok ? "HTTP " + resp.status : "响应不是 SSE 流"));
    }
    const reader = op.reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "", lines = [], finished = false;
    function frame() {
      let event = "message";
      const data = [];
      for (const line of lines) {
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
        if (field === "event") event = value;
        else if (field === "data") data.push(value);
      }
      lines = [];
      if (!data.length) return;
      const m = JSON.parse(data.join("\n"));
      if (event === "error") throw new Error(backendError(m, "流式检索失败"));
      if (event === "content") {
        // 合成token是流式增量；replace=true 表示这是权威全文（流式失败回退
        // 到非流式重合成时出现），整段替换而非追加，避免屏幕上出现残段+全文。
        if (m.replace) updateMessage(op.message.id, { content: m.text || "" });
        else appendContent(op.message.id, m.text || "");
        scroll();
      } else if (event === "citations") {
        // 引用挂到本条消息上（历史恢复靠它），全局副本同步维护。
        const refs = (m.refs || []).map((r) => ({
          index: r.index, title: (r.title || r.source_id) + (r.span ? " · " + r.span : ""),
          snippet: r.quote, source: r.source_id, resolved: r.resolved,
        }));
        sources.value = refs;
        if (op.message) op.message.sources = refs;
      } else if (event === "status" && m.stage === "stage") {
        // 实时时间轴：每个 stage 完成时推一行（名 + 本段耗时 + 累计 elapsed）。
        const row = { name: m.name || "", ms: m.stage_ms || 0, elapsed_ms: m.elapsed_ms || 0 };
        liveStages.value = [...liveStages.value, row];
        // 下一段（规范序）推论为进行中；思考块文案跟着它走，不再静止占位。
        const idx = STAGE_ORDER.indexOf(row.name);
        liveStage.value = idx >= 0 && idx + 1 < STAGE_ORDER.length ? STAGE_ORDER[idx + 1] : "";
        if (op.message) {
          op.message.stages = [...(op.message.stages || []), row];
          if (op.message.thinking || op.message.status === "pending") {
            updateMessage(op.message.id, { thinkContent: stageText(row.name) + " 完成" + (row.ms ? " · " + fmtMS(row.ms) : "") });
          }
        }
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage === "file") {
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage === "working") {
        // 心跳：只推进计时，不冲掉当前阶段文案
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage === "insufficient-evidence") {
        op.insufficient = true;
        if (op.message) op.message.insufficient = true;
      } else if (event === "status" && m.stage === "refused") {
        if (op.message) op.message.refused = true;
      } else if (event === "done") {
        finished = true;
        // done 携带权威分段（微秒）与完整运行卡，落到本条消息上。
        const card = {
          mode: m.mode || "", conf: m.conf ?? 0, coverage: m.coverage ?? 0,
          loops: m.loops || 0, widened: m.widened || 0, tokens: m.tokens || 0,
          latency: m.latency_ms || 0, reused: !!m.reused,
          cluster_id: m.cluster_id || "", stop_reason: m.stop_reason || "",
          refused: !!m.refused, insufficient: !!op.insufficient,
          stages: m.stages || null,
        };
        if (op.message) op.message.stats = card;
        stats.value = card;
        // 服务端明确说了这一轮没写进会话：必须让用户看见，而不是下次点开才发现
        // 历史里少了这一问。
        if (m.session_error) error.value = "答案已生成，但这一轮未写入会话历史：" + m.session_error;
      }
    }
    try {
      while (!finished) {
        const { value, done } = await reader.read();
        assertActive(op);
        buffer += done ? decoder.decode() : decoder.decode(value, { stream: true });
        let i;
        while (!finished && (i = buffer.indexOf("\n")) >= 0) {
          const line = buffer.slice(0, i).replace(/\r$/, "");
          buffer = buffer.slice(i + 1);
          if (line === "") frame(); else lines.push(line);
        }
        if (done) {
          if (!finished) {
            // Some gateways return JSON without a JSON content-type.
            let d;
            try { d = JSON.parse([...lines, buffer].join("\n")); } catch {}
            throw new Error(backendError(d, "响应流已截断：未收到 done 事件"));
          }
          break;
        }
      }
    } finally {
      closeReader(op);
      reader.releaseLock();
      op.reader = null;
    }
  }

  async function onSend(text) {
    text = String(text ?? "").trim();
    if (!text || loading.value || viewRequest || disposed) return;
    if (!nsSel.value) { error.value = "请先创建或选择知识库"; return; }
    const op = operation();
    active = op;
    loading.value = true;
    elapsed.value = 0; meta.value = ""; stats.value = null; sources.value = [];
    liveStages.value = [];
    // 请求一发出去，第一段（分析）就已在进行——首段完成事件到达前也要有进度。
    liveStage.value = "analyze";
    error.value = "";
    try {
      // eb-chatbot 已经写入用户消息；首页/示例按钮则没有。只去重末条同文 user。
      const last = messages.value[messages.value.length - 1];
      if (last?.role !== "user" || last.content !== text) addUserMessage(text);
      op.message = createAssistantMessage();
      // 附属数据挂在本条消息上：引用/运行卡/时间轴随答案走（刷新后可恢复）。
      op.message.sources = [];
      op.message.stats = null;
      op.message.stages = [];
      // 思考占位：时间轴建起来之前的气泡内容；失败路径也要看见它。
      appendThinkContent(op.message.id, "检索私域语料并评分证据窗口……");
      sources.value = [];
      scroll();
      // 会话 id 本地生成：服务端在第一次成功落库时 ensure 建会话。这样中断的
      // 提问不会留下谁也打不开的空会话。
      if (!current.value) current.value = newClientSessionID();
      const resp = await api.searchStream({ query: text, session: current.value, prior: true, ns: op.ns || undefined }, op.controller.signal);
      assertActive(op);
      await readAnswer(op, resp);
      assertActive(op);
      completeMessage(op.message.id);
      void loadSessions();
    } catch (e) {
      if (active === op && valid(op)) {
        showError(e);
        meta.value = ""; stats.value = null; sources.value = [];
        if (op.message) {
          stopThinking(op.message.id);
          if (e.name === "AbortError") cancelMessage(op.message.id);
          else setMessageError(op.message.id, e.message);
        }
      }
    } finally {
      if (active === op) {
        active = null;
        loading.value = false;
        liveStages.value = []; liveStage.value = "";
        scroll();
      }
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
    disposed = true;
  });

  return {
    sessions, sessionsBusy, current, loading, error, elapsed, stats, meta, sources,
    liveStages, liveStage,
    messages, onSend, stop, loadSessions, openSession, newSession, delSession, resetForNs,
    pane, nsSel,
  };
}

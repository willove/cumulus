// 会话、请求和 SSE 的生命周期都在此管理；各输入入口只需调用 onSend。
import { ref, onMounted, onUnmounted, nextTick, watch } from "vue";
import { useChatEngine } from "@wil-works/evoke-chat";
import { nsSel, pane, withNS } from "../state.js";
import { api, requestJSON, jsonPost } from "../api.js";

export function useChatPane() {
  const sessions = ref([]);
  const sessionsBusy = ref(false);
  const current = ref("");
  const loading = ref(false);
  const sources = ref([]);
  const meta = ref("");
  const elapsed = ref(0); // 秒；来自服务端 status 事件，不本地计时（连接断了就该停）
  const error = ref("");
  const stats = ref(null);
  const { messages, addUserMessage, createAssistantMessage, appendContent, appendThinkContent,
    completeMessage, stopThinking, setMessageError, cancelMessage } = useChatEngine();
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
    meta.value = "";
    elapsed.value = 0;
    stats.value = null;
    error.value = "已取消";
  }
  function switchView() {
    stop();
    epoch++;
    for (const op of sessionRequests) op.controller.abort();
    sessionRequests.clear();
    sessionsBusy.value = false;
    listRequest = viewRequest = null;
    sources.value = []; meta.value = ""; elapsed.value = 0; stats.value = null; error.value = "";
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
  // 服务端 stage → 中文文案。以前除 file/insufficient 外直接把原始 stage 显示给
  // 用户（进度行上出现英文 "started"），既难看也说明不了在做什么。
  const STAGE_TEXT = {
    started: "正在分析问题与检索意图",
    working: "",
    sampling: "正在采样证据窗口",
    synthesize: "正在合成答案",
  };

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
      messages.value = (d.messages || []).map((m, i) => ({
        id: s.id + "-" + i, role: m.role, content: m.content, status: "done",
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
        stopThinking(op.message.id); appendContent(op.message.id, m.text || ""); scroll();
      } else if (event === "citations") {
        sources.value = (m.refs || []).map((r) => ({
          index: r.index, title: (r.title || r.source_id) + (r.span ? " · " + r.span : ""),
          snippet: r.quote, source: r.source_id, resolved: r.resolved,
        }));
      } else if (event === "status" && m.stage === "file") {
        meta.value = "已采样 " + (m.file || "") + "（" + (m.score ?? 0) + " 分）";
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage === "working") {
        // 心跳：只推进计时，不冲掉当前阶段文案
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage !== "started") {
        if (m.stage === "insufficient-evidence") op.insufficient = true;
        const text = STAGE_TEXT[m.stage] || m.stage;
        if (text) meta.value = text;
        if (m.elapsed_ms) elapsed.value = Math.round(m.elapsed_ms / 1000);
      } else if (event === "status" && m.stage === "started") {
        meta.value = STAGE_TEXT.started;
      } else if (event === "done") {
        finished = true;
        stats.value = {
          mode: m.mode || "", conf: m.conf ?? 0, loops: m.loops || 0,
          widened: m.widened || 0, tokens: m.tokens || 0,
          latency: m.latency_ms || 0, session: m.session || "",
          coverage: m.coverage ?? 0, reused: !!m.reused,
          cluster_id: m.cluster_id || "", stop_reason: m.stop_reason || "",
          insufficient: !!op.insufficient,
        };
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
    sources.value = []; meta.value = "正在分析问题与检索意图"; elapsed.value = 0; stats.value = null; error.value = "";
    try {
      // eb-chatbot 已经写入用户消息；首页/示例按钮则没有。只去重末条同文 user。
      const last = messages.value[messages.value.length - 1];
      if (last?.role !== "user" || last.content !== text) addUserMessage(text);
      op.message = createAssistantMessage();
      appendThinkContent(op.message.id, "检索私域语料并评分证据窗口……");
      scroll();
      // 会话 id 本地生成：服务端在第一次成功落库时 ensure 建会话。这样中断的
      // 提问不会留下谁也打不开的空会话。
      if (!current.value) current.value = newClientSessionID();
      const resp = await api.searchStream({ query: text, session: current.value, prior: true, ns: op.ns || undefined }, op.controller.signal);
      assertActive(op);
      await readAnswer(op, resp);
      assertActive(op);
      completeMessage(op.message.id);
      meta.value = "";
      void loadSessions();
    } catch (e) {
      if (active === op && valid(op)) {
        meta.value = "";
        stats.value = null;
        showError(e);
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
    sessions, sessionsBusy, current, loading, sources, meta, error, stats, box,
    messages, onSend, stop, loadSessions, openSession, newSession, delSession, resetForNs, elapsed,
    pane, nsSel,
  };
}

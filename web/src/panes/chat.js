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
    sources.value = []; meta.value = ""; stats.value = null; error.value = "";
  }
  function clearConversation() {
    current.value = "";
    messages.value = [];
  }
  function showError(e) { error.value = "出错：" + e.message; }
  async function createSession(op, title) {
    const d = await sessionJSON(op, "/v1/sessions", jsonPost({ ns: op.ns || undefined, title }));
    if (!d || typeof d.id !== "string" || !d.id) throw new Error("创建会话失败：响应缺少 session id");
    return d.id;
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
      messages.value = (d.messages || []).map((m, i) => ({
        id: s.id + "-" + i, role: m.role, content: m.content, status: "done",
      }));
    } catch (e) {
      if (valid(op)) showError(e);
    } finally {
      if (viewRequest === op) viewRequest = null;
    }
  }

  async function newSession() {
    if (disposed) return;
    switchView();
    clearConversation();
    const op = operation();
    viewRequest = op;
    try {
      const id = await createSession(op);
      if (!valid(op)) return;
      current.value = id;
      void loadSessions();
    } catch (e) {
      if (valid(op)) showError(e);
    } finally {
      if (viewRequest === op) viewRequest = null;
    }
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
      } else if (event === "status" && m.stage !== "started") {
        if (m.stage === "insufficient-evidence") op.insufficient = true;
        meta.value = m.stage === "insufficient-evidence" ? "证据不足，正在整理检索结果" : m.stage;
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
    sources.value = []; meta.value = "检索中……"; stats.value = null; error.value = "";
    try {
      // eb-chatbot 已经写入用户消息；首页/示例按钮则没有。只去重末条同文 user。
      const last = messages.value[messages.value.length - 1];
      if (last?.role !== "user" || last.content !== text) addUserMessage(text);
      op.message = createAssistantMessage();
      appendThinkContent(op.message.id, "检索私域语料并评分证据窗口……");
      scroll();
      if (!current.value) {
        const id = await createSession(op, text);
        assertActive(op);
        current.value = id;
      }
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
    messages, onSend, stop, loadSessions, openSession, newSession, delSession, resetForNs,
    pane, nsSel,
  };
}

<script setup>
// ask 认知检索工作台（P7 UI v0.5）：evoke-chat 组件族 + /v1/search/stream。
import { ref, onMounted, nextTick } from "vue";
import { useChatEngine } from "@wil-works/evoke-chat";

const sessions = ref([]);
const current = ref("");
const loading = ref(false);
const sources = ref([]);
const meta = ref("");
const { messages, createAssistantMessage, appendContent, completeMessage, stopThinking } =
  useChatEngine();

const box = ref(null);
function scroll() {
  nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight; });
}

async function loadSessions() {
  try { sessions.value = await (await fetch("/v1/sessions")).json() || []; } catch {}
}

async function openSession(s) {
  current.value = s.id;
  sources.value = []; meta.value = "";
  const d = await (await fetch("/v1/sessions/" + s.id)).json();
  messages.value = (d.messages || []).map((m, i) => ({
    id: s.id + "-" + i, role: m.role, content: m.content, status: "done",
  }));
}

async function newSession() {
  const d = await (await fetch("/v1/sessions", {
    method: "POST", headers: { "Content-Type": "application/json" }, body: "{}",
  })).json();
  current.value = d.id;
  messages.value = [];
  sources.value = []; meta.value = "";
  loadSessions();
}

async function delSession(id, e) {
  e.stopPropagation();
  await fetch("/v1/sessions/" + id, { method: "DELETE" });
  if (current.value === id) { current.value = ""; messages.value = []; }
  loadSessions();
}

async function onSend(text) {
  loading.value = true;
  sources.value = []; meta.value = "检索中……";
  const am = createAssistantMessage();
  appendThinkContent(am.id, "检索私域语料并评分证据窗口……");
  scroll();
  try {
    const resp = await fetch("/v1/search/stream", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: text, session: current.value || undefined, prior: true }),
    });
    if (!resp.ok || !resp.body) throw new Error("HTTP " + resp.status);
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const frame = buf.slice(0, i); buf = buf.slice(i + 2);
        let ev = "message", data = "";
        for (const line of frame.split("\n")) {
          if (line.startsWith("event: ")) ev = line.slice(7).trim();
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (!data) continue;
        const m = JSON.parse(data);
        if (ev === "content") { stopThinking(am.id); appendContent(am.id, m.text); scroll(); }
        else if (ev === "citations") { sources.value = (m.refs || []).map((r, i) => ({ index: r.index, title: (r.title || r.source_id) + (r.span ? " · " + r.span : ""), snippet: r.quote, source: r.source_id })); }
        else if (ev === "status" && m.stage === "file") { meta.value = "已采样 " + (m.file || "") + "（" + (m.score ?? 0) + " 分）"; }
        else if (ev === "status" && m.stage !== "started") { meta.value = m.text || m.stage; }
        else if (ev === "done") {
          meta.value = "mode=" + m.mode + " · conf=" + (m.conf ?? 0).toFixed(2)
            + " · loops=" + (m.loops || 0) + (m.widened ? " · 扩征" + m.widened : "")
            + " · tokens=" + (m.tokens || 0) + " · " + (m.latency_ms || 0) + "ms"
            + (m.session ? " · 会话 " + m.session : "");
        }
        else if (ev === "error") { meta.value = "出错：" + m.error; }
      }
    }
  } catch (e) {
    meta.value = "出错：" + e.message;
  }
  completeMessage(am.id);
  loading.value = false;
  scroll();
  if (current.value) loadSessions(); else current.value = "";
}

onMounted(loadSessions);
</script>

<template>
  <div class="shell">
    <aside class="side">
      <header>
        <b>ask 认知检索</b>
        <small>原文即契约 · 索引是缓存</small>
      </header>
      <button class="new" @click="newSession">＋ 新会话</button>
      <div class="list">
        <div v-for="s in sessions" :key="s.id" class="sess" :class="{ cur: s.id === current }"
             @click="openSession(s)">
          <span class="t">{{ s.title || s.id }}</span>
          <span class="x" @click="delSession(s.id, $event)">✕</span>
        </div>
      </div>
    </aside>
    <main>
      <div class="head">认知检索工作台<span v-if="current" class="sid">会话 {{ current }}</span></div>
      <div class="body" ref="box">
        <eb-chatbot v-model="messages" :loading="loading" height="100%"
                    :show-tip="false" stoppable @send="onSend" />
        <div v-if="meta" class="meta">{{ meta }}</div>
        <eb-chat-sources v-if="sources.length" :items="sources" class="srcs" />
      </div>
    </main>
  </div>
</template>

<style>
html, body, #app { height: 100%; margin: 0; }
.shell { display: flex; height: 100vh; font: 14px/1.6 -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; }
.side { width: 250px; border-right: 1px solid #e3e6ea; padding: 14px 12px; box-sizing: border-box; display: flex; flex-direction: column; gap: 10px; background: #fff; }
.side header small { color: #6b7684; font-weight: 400; display: block; }
.new { padding: 8px; border: 1px solid #e3e6ea; border-radius: 8px; background: #e8eefc; color: #2456d6; cursor: pointer; }
.list { overflow: auto; }
.sess { display: flex; justify-content: space-between; padding: 8px 10px; border-radius: 8px; cursor: pointer; }
.sess:hover { background: #f6f7f9; }
.sess.cur { background: #e8eefc; }
.sess .t { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sess .x { color: #98a2b0; }
main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.head { padding: 12px 20px; border-bottom: 1px solid #e3e6ea; font-weight: 700; }
.head .sid { color: #6b7684; font-weight: 400; font-size: 12px; margin-left: 10px; }
.body { flex: 1; overflow: auto; padding: 8px 20px 16px; }
.meta { color: #6b7684; font-size: 12px; padding: 4px 8px; }
.srcs { margin: 0 auto 12px; max-width: 780px; }
</style>

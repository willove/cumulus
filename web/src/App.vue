<script setup>
// ask 认知检索工作台（P7 UI v0.5 + UI v1 簇浏览）：evoke-chat 组件族 +
// /v1/search/stream；知识簇面板读 /v1/clusters（簇内容/query 集/cites 边）。
import { ref, onMounted, nextTick } from "vue";
import { useChatEngine } from "@wil-works/evoke-chat";

const pane = ref("chat"); // chat | clusters
const sessions = ref([]);
const current = ref("");
const loading = ref(false);
const sources = ref([]);
const meta = ref("");
const clusters = ref([]);
const clusterCur = ref(null);
const clusterLoading = ref(false);
const { messages, createAssistantMessage, appendContent, appendThinkContent, completeMessage, stopThinking } =
  useChatEngine();

const box = ref(null);
function scroll() {
  nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight; });
}

async function loadSessions() {
  try { sessions.value = await (await fetch("/v1/sessions")).json() || []; } catch {}
}

async function loadClusters() {
  clusterLoading.value = true;
  try {
    const r = await (await fetch("/v1/clusters?limit=200")).json();
    clusters.value = r.clusters || [];
  } catch { clusters.value = []; }
  clusterLoading.value = false;
}

async function openCluster(id) {
  try {
    const r = await (await fetch("/v1/clusters/" + id)).json();
    clusterCur.value = r;
  } catch { clusterCur.value = null; }
}

const lifecycleLabel = { emerging: "待复核", stable: "稳定", contested: "有争议", deprecated: "已退役" };

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
  try {
    appendThinkContent(am.id, "检索私域语料并评分证据窗口……");
    scroll();
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

onMounted(() => { loadSessions(); loadClusters(); });
</script>

<template>
  <div class="shell">
    <aside class="side">
      <header>
        <b>ask 认知检索</b>
        <small>原文即契约 · 索引是缓存</small>
      </header>
      <div class="tabs">
        <button :class="{ on: pane === 'chat' }" @click="pane = 'chat'">会话</button>
        <button :class="{ on: pane === 'clusters' }" @click="pane = 'clusters'">知识簇</button>
      </div>
      <template v-if="pane === 'chat'">
        <button class="new" @click="newSession">＋ 新会话</button>
        <div class="list">
          <div v-for="s in sessions" :key="s.id" class="sess" :class="{ cur: s.id === current }"
               @click="openSession(s)">
            <span class="t">{{ s.title || s.id }}</span>
            <span class="x" @click="delSession(s.id, $event)">✕</span>
          </div>
        </div>
      </template>
      <div v-else class="list">
        <div v-for="c in clusters" :key="c._id" class="sess"
             :class="{ cur: clusterCur && clusterCur.cluster._id === c._id }"
             @click="openCluster(c._id)">
          <span class="t">{{ (c.queries && c.queries[0]) || c.name || c._id }}</span>
          <span class="lc" :class="c.lifecycle">{{ lifecycleLabel[c.lifecycle] || c.lifecycle }}</span>
        </div>
        <div v-if="clusterLoading" class="hint">加载中……</div>
        <div v-else-if="!clusters.length" class="hint">暂无知识簇</div>
      </div>
    </aside>
    <main>
      <div class="head" v-if="pane === 'chat'">认知检索工作台<span v-if="current" class="sid">会话 {{ current }}</span></div>
      <div class="head" v-else>知识簇浏览<span class="sid">簇内容 · query 集 · cites 证据边</span></div>
      <div class="body" ref="box" v-if="pane === 'chat'">
        <eb-chatbot v-model="messages" :loading="loading" height="100%"
                    :show-tip="false" stoppable @send="onSend" />
        <div v-if="meta" class="meta">{{ meta }}</div>
        <eb-chat-sources v-if="sources.length" :items="sources" class="srcs" />
      </div>
      <div class="body clu" v-else>
        <div v-if="!clusterCur" class="hint">从左栏选一个知识簇查看内容与证据边。</div>
        <template v-else>
          <div class="ch">
            <div class="row">
              <span class="lc" :class="clusterCur.cluster.lifecycle">{{ lifecycleLabel[clusterCur.cluster.lifecycle] || clusterCur.cluster.lifecycle }}</span>
              <b>{{ clusterCur.cluster.name || clusterCur.cluster._id }}</b>
              <span class="sid">v{{ clusterCur.cluster.version }} · conf {{ (clusterCur.cluster.confidence ?? 0).toFixed(2) }} · 热度 {{ (clusterCur.cluster.hotness ?? 0).toFixed(1) }}</span>
            </div>
            <div class="qs">问法：<span v-for="q in (clusterCur.cluster.queries || [])" :key="q" class="q">{{ q }}</span></div>
            <pre class="content">{{ clusterCur.cluster.content }}</pre>
            <div class="cites">
              <div class="ct">证据边（cites）</div>
              <div v-for="(e, i) in clusterCur.cites" :key="i" class="cite">
                → {{ e._to }} <span class="sid">{{ e.start }}–{{ e.end }} · {{ (e.score ?? 0).toFixed(1) }} 分</span>
              </div>
              <div v-if="!clusterCur.cites.length" class="hint">该簇暂无 cites 边（复用簇可能不带窗口）</div>
            </div>
          </div>
        </template>
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
.tabs { display: flex; gap: 6px; }
.tabs button { flex: 1; padding: 6px; border: 1px solid #e3e6ea; border-radius: 8px; background: #fff; color: #6b7684; cursor: pointer; }
.tabs button.on { background: #e8eefc; color: #2456d6; border-color: #c7d7f5; }
.hint { color: #98a2b0; font-size: 12px; padding: 8px 10px; }
.lc { font-size: 11px; padding: 1px 6px; border-radius: 999px; background: #eef1f5; color: #6b7684; white-space: nowrap; }
.lc.stable { background: #e7f6ec; color: #1a7f37; }
.lc.emerging { background: #fff4e0; color: #b25e09; }
.lc.contested { background: #fdeaea; color: #c0392b; }
.lc.deprecated { background: #eef1f5; color: #98a2b0; }
.clu { padding: 16px 24px; }
.ch { max-width: 860px; margin: 0 auto; }
.ch .row { display: flex; align-items: center; gap: 10px; }
.ch .sid { color: #6b7684; font-size: 12px; }
.qs { margin: 8px 0; font-size: 12px; color: #6b7684; }
.qs .q { display: inline-block; background: #f2f5fa; border-radius: 6px; padding: 1px 8px; margin: 0 4px 4px 0; }
.content { white-space: pre-wrap; background: #f8fafc; border: 1px solid #eef1f5; border-radius: 10px; padding: 12px 14px; font: 13px/1.7 -apple-system, "PingFang SC", sans-serif; }
.cites { margin-top: 12px; }
.cites .ct { font-weight: 700; font-size: 13px; margin-bottom: 6px; }
.cites .cite { font-size: 12px; color: #44505f; padding: 3px 0; }
</style>

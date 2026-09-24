<script setup>
// cumulus-cluster 认知检索工作台（P7 UI：evoke-chat 组件族 + /v1/search/stream；
// UI v1 知识簇面板读 /v1/clusters；UI v2 摄取面板 POST /v1/ingest/jobs 指定
// 服务器本地目录异步摄取，轮询 GET /v1/ingest/jobs/{id} 跟踪状态机；B1 命名
// 空间选择器把四个面（搜索/会话/簇/摄取）按 ns 分域，空 = serve 级 -ns）。
import { ref, onMounted, nextTick } from "vue";
import { useChatEngine } from "@wil-works/evoke-chat";

const pane = ref("chat"); // chat | clusters | ingest | settings | evals | monitor
const sessions = ref([]);
const current = ref("");
const loading = ref(false);
const sources = ref([]);
const meta = ref("");
const clusters = ref([]);
const clusterCur = ref(null);
const clusterLoading = ref(false);
// B1: per-request namespace selector — empty means the serve-level -ns.
const nsSel = ref("");
// Settings pane: weight status/install/verify + masked endpoint config.
const settingsModel = ref(null);
const settingsConfig = ref(null);
const settingsBusy = ref(false);
const settingsMsg = ref("");
// B3 scoreboard pane: past eval runs (written by `eval-run` into clus_evals).
const evalRuns = ref([]);
const evalCur = ref(null);
const evalLoading = ref(false);
const { messages, createAssistantMessage, appendContent, appendThinkContent, completeMessage, stopThinking } =
  useChatEngine();

const box = ref(null);
function scroll() {
  nextTick(() => { if (box.value) box.value.scrollTop = box.value.scrollHeight; });
}

// withNS scopes a GET to the selected namespace (empty = default library),
// joining onto an existing query string when there is one.
function withNS(url) {
  if (!nsSel.value) return url;
  return url + (url.includes("?") ? "&" : "?") + "ns=" + encodeURIComponent(nsSel.value);
}

async function loadSessions() {
  try { sessions.value = await (await fetch(withNS("/v1/sessions"))).json() || []; } catch {}
}

async function loadClusters() {
  clusterLoading.value = true;
  try {
    const r = await (await fetch(withNS("/v1/clusters?limit=200"))).json();
    clusters.value = r.clusters || [];
  } catch { clusters.value = []; }
  clusterLoading.value = false;
}

async function openCluster(id) {
  try {
    const r = await (await fetch(withNS("/v1/clusters/" + id))).json();
    clusterCur.value = r;
  } catch { clusterCur.value = null; }
}

const lifecycleLabel = { emerging: "待复核", stable: "稳定", contested: "有争议", deprecated: "已退役" };

// --- settings pane (model weights + endpoint config) -------------------------
async function loadSettings() {
  try {
    settingsModel.value = await (await fetch("/v1/model")).json();
    settingsConfig.value = await (await fetch("/v1/config")).json();
    if (settingsModel.value && settingsModel.value.installing) pollSettings();
  } catch {}
}

async function installWeights() {
  settingsBusy.value = true; settingsMsg.value = "提交下载……";
  try {
    const r = await fetch("/v1/model", { method: "POST" });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
    settingsMsg.value = d.installed ? "权重已安装" : "下载中（约 464MB，魔搭社区）……";
    pollSettings();
  } catch (e) { settingsMsg.value = "出错：" + e.message; }
  settingsBusy.value = false;
}

async function verifyWeights() {
  settingsBusy.value = true; settingsMsg.value = "加载并运行权重……";
  try {
    const r = await fetch("/v1/model/verify", { method: "POST" });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
    settingsMsg.value = "验证通过：" + d.dims + " 维 · " + d.ms + "ms";
  } catch (e) { settingsMsg.value = "验证失败：" + e.message; }
  settingsBusy.value = false;
}

let settingsTimer = null;
function pollSettings() {
  if (settingsTimer) return;
  settingsTimer = setInterval(async () => {
    try { settingsModel.value = await (await fetch("/v1/model")).json(); } catch {}
    if (settingsModel.value && !settingsModel.value.installing) {
      clearInterval(settingsTimer); settingsTimer = null;
      const m = settingsModel.value;
      settingsMsg.value = m.error ? ("下载失败：" + m.error) : (m.installed ? "权重已安装" : settingsMsg.value);
    }
  }, 1500);
}

function fmtMB(b) { return (b / 1e6).toFixed(1) + " MB"; }

// --- B3 scoreboard pane -------------------------------------------------------
async function loadEvals() {
  evalLoading.value = true;
  try {
    const r = await (await fetch(withNS("/v1/evals?limit=50"))).json();
    evalRuns.value = r.runs || [];
  } catch { evalRuns.value = []; }
  evalLoading.value = false;
}

async function openEval(id) {
  try { evalCur.value = await (await fetch(withNS("/v1/evals/" + encodeURIComponent(id)))).json(); }
  catch { evalCur.value = null; }
}

function pct(x) { return ((x || 0) * 100).toFixed(1) + "%"; }
function shortSha(s) { return s ? s.slice(0, 12) : "—"; }
function txRatio(tx, k) {
  const t = tx || {};
  const total = (t.correct || 0) + (t.retrieved_but_unanswered || 0) + (t.answered_but_wrong || 0) + (t.not_retrieved || 0);
  return total > 0 ? (t[k] || 0) / total : 0;
}

// 摄取面板：目录表单提交异步 job，左栏跟踪状态机（queued/running/done/failed）。
const ingDir = ref("");
const ingRec = ref(false);
const ingName = ref("");
const ingBusy = ref(false);
const ingMeta = ref("");
const jobs = ref([]);
const jobCur = ref("");
// P9/B2 候选发现：先扫描出清单（规则分层），勾选后只吃选中的文件。
const scanDir = ref("");
const scanLimit = ref("");
const scanNewer = ref("");
const scanBusy = ref(false);
const scanReport = ref(null);
const scanPicked = ref([]);
const scanMeta = ref("");
let pollHandle = null;

const jobLabel = { queued: "排队中", running: "进行中", done: "已完成", failed: "已失败" };

function jobDone(j) { return j.state === "done" || j.state === "failed"; }
function curJob() { return jobs.value.find((j) => j.id === jobCur.value) || null; }

const mon = ref(null);
const monBusy = ref(false);
let monTimer = null;

async function loadMonitor() {
  monBusy.value = true;
  try { mon.value = await (await fetch("/v1/monitor/overview")).json(); }
  catch { mon.value = null; }
  monBusy.value = false;
}

// 适配器摄取（json/jsonl/csv/parquet）：探测容器与字段 → 选映射 → 提交。
const adPaths = ref("");
const adBusy = ref(false);
const adProbes = ref([]);
const adMeta = ref("");
const adMap = ref({ id: "", title: "", body: "", extra: "" });
const adJob = ref("");

function adFileList() {
  return adPaths.value.split("\n").map((x) => x.trim()).filter((x) => x);
}

async function runAdaptProbe() {
  const paths = adFileList();
  if (!paths.length) { adMeta.value = "请先填写文件路径（每行一个）"; return; }
  adBusy.value = true; adMeta.value = "探测中……";
  try {
    const r = await fetch("/v1/adapt/probe", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ paths }),
    });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
    adProbes.value = d.probes || [];
    const bad = d.failed || [];
    // 预填建议映射：取第一个 probe 的 bodyish/titleish/idish 首项。
    const p = adProbes.value[0];
    if (p) {
      adMap.value = {
        id: (p.idish || [])[0] || "",
        title: (p.titleish || [])[0] || "",
        body: (p.bodyish || [])[0] || "",
        extra: "",
      };
    }
    adMeta.value = "探测到 " + adProbes.value.length + " 个文件"
      + (bad.length ? " · " + bad.length + " 个读取失败" : "")
      + (p && p.note ? " · " + p.note : "");
  } catch (e) { adMeta.value = "出错：" + e.message; adProbes.value = []; }
  adBusy.value = false;
}

async function submitAdapt() {
  const paths = adFileList();
  if (!paths.length) { adMeta.value = "请先填写文件路径"; return; }
  if (!nsSel.value) { adMeta.value = "请先选择 bucket（右上角命名空间）"; return; }
  adBusy.value = true; adMeta.value = "提交摄取……";
  try {
    const r = await fetch("/v1/adapt/ingest", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        paths, ns: nsSel.value, job: adJob.value.trim() || undefined,
        id: adMap.value.id || undefined, title: adMap.value.title || undefined,
        body: adMap.value.body || undefined,
      }),
    });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || "HTTP " + r.status);
    jobs.value.unshift({ id: d.job, state: d.state || "queued", phase: "",
      total: d.total || 0, done: 0, failed: 0, error: "", updated: "" });
    jobCur.value = d.job;
    adMeta.value = "已提交任务 " + d.job;
    schedulePoll();
  } catch (e) { adMeta.value = "出错：" + e.message; }
  adBusy.value = false;
}

function openPane(p) {
  pane.value = p;
  if (p === "ingest") schedulePoll();
  if (p === "settings") return loadSettings();
  if (p === "evals") return loadEvals();
  if (p === "monitor") { loadMonitor(); startMonPoll(); } else { stopMonPoll(); }
}

// The monitor auto-refreshes like an ops dashboard, but ONLY while its pane is
// open — a hidden pane must not keep polling.
function startMonPoll() {
  if (monTimer) return;
  monTimer = setInterval(() => { if (pane.value === "monitor") loadMonitor(); }, 5000);
}
function stopMonPoll() {
  if (monTimer) { clearInterval(monTimer); monTimer = null; }
}

// Switching namespace reloads every scoped list (sessions, clusters) — the
// per-request ns travels with each call from here on.
function onNsChange() {
  current.value = "";
  messages.value = []; sources.value = []; meta.value = "";
  loadSessions();
  loadClusters();
}

function selectJob(id) { jobCur.value = id; }

async function startIngest() {
  const dir = ingDir.value.trim();
  if (!dir) { ingMeta.value = "请先填写目录（服务器本地路径）"; return; }
  ingBusy.value = true; ingMeta.value = "提交摄取任务……";
  try {
    const resp = await fetch("/v1/ingest/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ dir, recursive: ingRec.value, job: ingName.value.trim() || undefined, ns: nsSel.value || undefined }),
    });
    const d = await resp.json();
    if (!resp.ok) throw new Error(d.error || "HTTP " + resp.status);
    const id = d.job || "";
    jobs.value.unshift({
      id, state: d.state || "queued", phase: "",
      total: d.total || 0, done: 0, failed: 0, error: "", updated: "",
    });
    jobCur.value = id;
    ingMeta.value = "已提交任务 " + id + "（候选 " + (d.total || 0) + " 个文件）";
  } catch (e) {
    ingMeta.value = "出错：" + e.message;
  }
  ingBusy.value = false;
  schedulePoll();
}

// --- P9/B2 candidate discovery ------------------------------------------------
async function runScan() {
  const dir = scanDir.value.trim();
  if (!dir) { scanMeta.value = "请先填写要扫描的目录"; return; }
  scanBusy.value = true; scanMeta.value = "扫描中……";
  try {
    const resp = await fetch("/v1/scan", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        dir, recursive: ingRec.value,
        limit: parseInt(scanLimit.value) || 0,
        newer_than: scanNewer.value.trim() || undefined,
        ns: nsSel.value || undefined,
      }),
    });
    const d = await resp.json();
    if (!resp.ok) throw new Error(d.error || "HTTP " + resp.status);
    scanReport.value = d;
    scanPicked.value = (d.candidates || []).map(() => true);
    const sk = Object.entries(d.skipped || {}).map(([k, v]) => k + "×" + v).join(" ");
    scanMeta.value = "候选 " + (d.candidates || []).length + " 个" + (sk ? " · 跳过 " + sk : "")
      + (d.rank_error ? " · 排名失败保留规则序" : "");
  } catch (e) {
    scanMeta.value = "出错：" + e.message;
  }
  scanBusy.value = false;
}

function pickedCount() { return scanPicked.value.filter(Boolean).length; }
function toggleAllScan(v) { scanPicked.value = (scanReport.value?.candidates || []).map(() => v); }

async function ingestPicked() {
  const paths = (scanReport.value?.candidates || [])
    .filter((_, i) => scanPicked.value[i]).map((c) => c.path);
  if (!paths.length) { scanMeta.value = "没有选中的候选"; return; }
  ingBusy.value = true; scanMeta.value = "提交 " + paths.length + " 个选中文件……";
  try {
    const resp = await fetch("/v1/ingest/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ candidates: paths, job: ingName.value.trim() || undefined, ns: nsSel.value || undefined }),
    });
    const d = await resp.json();
    if (!resp.ok) throw new Error(d.error || "HTTP " + resp.status);
    const id = d.job || "";
    jobs.value.unshift({
      id, state: d.state || "queued", phase: "",
      total: d.total || paths.length, done: 0, failed: 0, error: "", updated: "",
    });
    jobCur.value = id;
    scanMeta.value = "已提交任务 " + id + "（" + paths.length + " 个文件）";
  } catch (e) {
    scanMeta.value = "出错：" + e.message;
  }
  ingBusy.value = false;
  schedulePoll();
}

async function pollJobs() {
  pollHandle = null;
  const active = jobs.value.filter((j) => !jobDone(j));
  for (const j of active) {
    try {
      const resp = await fetch(withNS("/v1/ingest/jobs/" + encodeURIComponent(j.id)));
      if (!resp.ok) continue;
      const d = await resp.json();
      j.state = d.state || j.state;
      j.phase = d.phase || "";
      j.total = d.total ?? j.total;
      j.done = d.done ?? j.done;
      j.failed = d.failed ?? j.failed;
      j.error = d.error || "";
      j.updated = d.updated || "";
    } catch {}
  }
  schedulePoll();
}

function schedulePoll() {
  if (pollHandle || pane.value !== "ingest") return;
  if (!jobs.value.some((j) => !jobDone(j))) return;
  pollHandle = setTimeout(pollJobs, 1000);
}

async function openSession(s) {
  current.value = s.id;
  sources.value = []; meta.value = "";
  // ns-scoped like the list above: the session KV keys are namespaced, so a
  // request without ?ns= reads the serve-level library instead of the tenant's.
  const d = await (await fetch(withNS("/v1/sessions/" + encodeURIComponent(s.id)))).json();
  messages.value = (d.messages || []).map((m, i) => ({
    id: s.id + "-" + i, role: m.role, content: m.content, status: "done",
  }));
}

async function newSession() {
  const d = await (await fetch("/v1/sessions", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ns: nsSel.value || undefined }),
  })).json();
  current.value = d.id;
  messages.value = [];
  sources.value = []; meta.value = "";
  loadSessions();
}

async function delSession(id, e) {
  e.stopPropagation();
  await fetch("/v1/sessions/" + encodeURIComponent(id), {
    method: "DELETE", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ns: nsSel.value || undefined }),
  });
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
      body: JSON.stringify({ query: text, session: current.value || undefined, prior: true, ns: nsSel.value || undefined }),
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
        else if (ev === "status" && m.stage !== "started") { meta.value = m.stage; }
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
        <b>cumulus-cluster 认知检索</b>
        <small>原文即契约 · 索引是缓存</small>
      </header>
      <div class="tabs">
        <button :class="{ on: pane === 'chat' }" @click="openPane('chat')">会话</button>
        <button :class="{ on: pane === 'clusters' }" @click="openPane('clusters')">知识簇</button>
        <button :class="{ on: pane === 'ingest' }" @click="openPane('ingest')">摄取</button>
        <button :class="{ on: pane === 'settings' }" @click="openPane('settings')">配置</button>
        <button :class="{ on: pane === 'evals' }" @click="openPane('evals')">评测</button>
        <button :class="{ on: pane === 'monitor' }" @click="openPane('monitor')">监控</button>
      </div>
      <div class="nssel">
        <input v-model="nsSel" placeholder="命名空间（空 = 默认库）" @change="onNsChange" />
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
      <template v-else-if="pane === 'clusters'">
        <div class="list">
          <div v-for="c in clusters" :key="c._id" class="sess"
               :class="{ cur: clusterCur && clusterCur.cluster._id === c._id }"
               @click="openCluster(c._id)">
            <span class="t">{{ (c.queries && c.queries[0]) || c.name || c._id }}</span>
            <span class="lc" :class="c.lifecycle">{{ lifecycleLabel[c.lifecycle] || c.lifecycle }}</span>
          </div>
          <div v-if="clusterLoading" class="hint">加载中……</div>
          <div v-else-if="!clusters.length" class="hint">暂无知识簇</div>
        </div>
      </template>
      <template v-else-if="pane === 'ingest'">
        <div v-if="!jobs.length" class="hint">暂无摄取任务</div>
        <div v-for="j in jobs" :key="j.id" class="sess" :class="{ cur: jobCur === j.id }"
             @click="selectJob(j.id)">
          <span class="t">{{ j.id }}</span>
          <span class="lc" :class="'st-' + j.state">{{ jobLabel[j.state] || j.state }}</span>
        </div>
      </template>
      <template v-else>
        <div v-if="!evalRuns.length" class="hint">{{ evalLoading ? "加载中……" : "暂无评测记录（CLI：eval-run -file items.jsonl -out results.jsonl）" }}</div>
        <div v-for="r in evalRuns" :key="r._id" class="sess" :class="{ cur: evalCur && evalCur._id === r._id }"
             @click="openEval(r._id)">
          <span class="t">{{ r.tag || r._id }}</span>
          <span class="lc st-done">{{ pct((r.system || {}).em) }}</span>
        </div>
      </template>
    </aside>
    <main>
      <div class="head" v-if="pane === 'chat'">认知检索工作台<span v-if="current" class="sid">会话 {{ current }}</span></div>
      <div class="head" v-else-if="pane === 'clusters'">知识簇浏览<span class="sid">簇内容 · query 集 · cites 证据边</span></div>
      <div class="head" v-else-if="pane === 'ingest'">目录摄取<span class="sid">异步任务 · 进度跟踪 · 可续跑（同名 job）</span></div>
      <div class="head" v-else-if="pane === 'settings'">配置<span class="sid">模型权重 · 端点</span></div>
      <div class="head" v-else-if="pane === 'evals'">评测记分牌<span class="sid">LENS 协议 · 系统 vs Closed-Book</span></div>
      <div class="head" v-else-if="pane === 'monitor'">运行监控<span class="sid">系统 · LLM · 检索路径 · 按 bucket</span></div>
      <div class="head" v-else>评测记分牌<span class="sid">LENS 协议 · 系统 vs Closed-Book</span></div>
      <div class="body" ref="box" v-if="pane === 'chat'">
        <eb-chatbot v-model="messages" :loading="loading" height="100%"
                    :show-tip="false" stoppable @send="onSend" />
        <div v-if="meta" class="meta">{{ meta }}</div>
        <eb-chat-sources v-if="sources.length" :items="sources" class="srcs" />
      </div>
      <div class="body mon" v-else-if="pane === 'monitor'">
        <div v-if="!mon" class="hint">{{ monBusy ? "加载中……" : "暂无监控数据" }}</div>
        <template v-else>
          <div class="mgrid">
            <div class="mcard"><div class="mt">系统</div>
              <div class="mrow"><span>Heap</span><b>{{ mon.system.heap_mb.toFixed(1) }} MB</b></div>
              <div class="mrow"><span>RSS/Sys</span><b>{{ mon.system.rss_mb.toFixed(1) }} MB</b></div>
              <div class="mrow"><span>Goroutines</span><b>{{ mon.system.goroutines }}</b></div>
              <div class="mrow"><span>GC</span><b>{{ mon.system.num_gc }}</b></div>
              <div class="mrow"><span>存储目录</span><b>{{ mon.system.store_dir }}</b></div>
              <div class="mrow"><span>存储文件</span><b>{{ (mon.system.store_files_bytes/1e6).toFixed(0) }} MB（上限，含预分配）</b></div>
              <div class="mrow"><span>运行</span><b>{{ mon.uptime_sec }}s</b></div>
            </div>
            <div class="mcard"><div class="mt">LLM</div>
              <div class="mrow"><span>调用数</span><b>{{ mon.llm.calls }}</b></div>
              <div class="mrow"><span>Tokens</span><b>{{ mon.llm.tokens }}</b></div>
              <div class="mrow"><span>每问 tokens</span><b>{{ mon.llm.tokens_per_query.toFixed(1) }}</b></div>
              <div class="mrow"><span>每分钟调用</span><b>{{ mon.llm.calls_per_min.toFixed(2) }}</b></div>
            </div>
            <div class="mcard"><div class="mt">检索路径</div>
              <div class="mrow"><span>簇复用命中</span><b>{{ mon.retrieval.reuse_hits }}/{{ mon.queries }}（{{ (mon.retrieval.reuse_rate*100).toFixed(0) }}%）</b></div>
              <div class="mrow"><span>温命中 p50</span><b>{{ mon.retrieval.warm_p50_us }} µs</b></div>
              <div class="mrow"><span>冷查询 p50</span><b>{{ mon.retrieval.cold_p50_us }} µs</b></div>
              <div class="mrow"><span>升级 DEEP</span><b>{{ mon.retrieval.escalations }}</b></div>
              <div class="mrow"><span>自纠错</span><b>{{ mon.retrieval.self_corrected }}</b></div>
              <div class="mrow"><span>拒答</span><b>{{ mon.retrieval.refused }}</b></div>
              <div class="mrow"><span>错误</span><b>{{ mon.retrieval.errors }}</b></div>
              <div class="mrow"><span>平均置信</span><b>{{ mon.retrieval.avg_confidence.toFixed(3) }}</b></div>
              <div class="mrow"><span>平均覆盖</span><b>{{ mon.retrieval.avg_coverage.toFixed(3) }}</b></div>
              <div class="mrow"><span>档位分布</span><b>{{ JSON.stringify(mon.retrieval.by_mode) }}</b></div>
            </div>
            <div class="mcard"><div class="mt">按 bucket</div>
              <table class="mtab">
                <tr><th>bucket</th><th>查询</th><th>复用</th><th>p50</th></tr>
                <tr v-for="n in mon.namespaces" :key="n.namespace">
                  <td>{{ n.namespace || "（默认）" }}</td><td>{{ n.queries }}</td>
                  <td>{{ n.reuse_hits }}</td><td>{{ n.avg_p50_us }} µs</td>
                </tr>
              </table>
              <div v-if="!mon.namespaces.length" class="hint">暂无查询记录</div>
            </div>
          </div>
          <div class="ct">最近查询</div>
          <table class="mtab">
            <tr><th>时间</th><th>bucket</th><th>档位</th><th>复用</th><th>置信</th><th>覆盖</th><th>采样</th><th>延迟</th></tr>
            <tr v-for="(q, i) in mon.recent" :key="i">
              <td>{{ new Date(q.at).toLocaleTimeString() }}</td>
              <td>{{ q.namespace || "（默认）" }}</td>
              <td>{{ q.mode }}</td>
              <td>{{ q.reused ? "✓" : "" }}</td>
              <td>{{ q.confidence.toFixed(2) }}</td>
              <td>{{ q.coverage.toFixed(2) }}</td>
              <td>{{ q.samples }}</td>
              <td>{{ q.latency_us }} µs</td>
            </tr>
          </table>
        </template>
      </div>
      <div class="body clu" v-else-if="pane === 'clusters'">
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
      <div class="body ing" v-else-if="pane === 'ingest'">
        <div class="scan">
          <div class="ct">候选发现（大目录先扫后吃）</div>
          <div class="srow">
            <input v-model="scanDir" placeholder="要扫描的目录（服务器本地路径）" />
            <input v-model="scanLimit" class="num" placeholder="上限 N" />
            <input v-model="scanNewer" class="num" placeholder="新鲜度 168h" />
            <button class="new" :disabled="scanBusy || !scanDir.trim()" @click="runScan">
              {{ scanBusy ? "扫描中……" : "扫描" }}
            </button>
          </div>
          <div class="hint">规则：可摄取扩展名 · 单文件 ≤8MiB · 新鲜度窗口；超限按扩展名分层取样。扫描不读写存储。</div>
          <div v-if="scanReport" class="sres">
            <div class="srow between">
              <span>候选 {{ (scanReport.candidates || []).length }} 个</span>
              <span class="lk">
                <a href="javascript:void(0)" @click="toggleAllScan(true)">全选</a> ·
                <a href="javascript:void(0)" @click="toggleAllScan(false)">全不选</a>
              </span>
            </div>
            <div v-for="(c, i) in (scanReport.candidates || [])" :key="c.path" class="cand">
              <label><input type="checkbox" v-model="scanPicked[i]" /> {{ c.path.split("/").pop() }}</label>
              <span class="sid">{{ fmtMB(c.size) }} · {{ c.age_days }} 天 · {{ c.headline ? c.headline.slice(0, 48) : "(二进制)" }}</span>
            </div>
            <button class="new" :disabled="ingBusy || pickedCount() === 0" @click="ingestPicked">
              摄取所选（{{ pickedCount() }}）
            </button>
          </div>
          <div v-if="scanMeta" class="meta">{{ scanMeta }}</div>
        </div>
        <div class="form">
          <div class="ct">异构语料适配（json / jsonl / csv / parquet）</div>
          <label>文件路径（服务器本地，每行一个）</label>
          <textarea v-model="adPaths" rows="3" placeholder="/data/mmarco/corpus.parquet&#10;/data/news.json&#10;/data/titles.csv"></textarea>
          <div class="srow">
            <button class="new" :disabled="adBusy || !adPaths.trim()" @click="runAdaptProbe">
              {{ adBusy ? "处理中……" : "① 探测字段" }}
            </button>
            <button class="new" :disabled="adBusy || !adProbes.length" @click="submitAdapt">
              ② 按映射摄取
            </button>
          </div>
          <div v-if="adProbes.length" class="pres">
            <div v-for="p in adProbes" :key="p.path" class="prow">
              <b>{{ p.path.split("/").pop() }}</b>
              <span class="lc" :class="'st-' + p.kind">{{ p.kind }}</span>
              <span class="sid">{{ p.records ? p.records + " 条" : "" }} · {{ fmtMB(p.bytes) }}</span>
              <div class="flds">
                <span v-for="f in p.fields" :key="f"
                      :class="{ b: (p.bodyish || []).includes(f), t: (p.titleish || []).includes(f), i: (p.idish || []).includes(f) }">{{ f }}</span>
              </div>
              <div v-if="p.note" class="hint">{{ p.note }}</div>
            </div>
            <div class="maprow">
              <label>ID 字段<input v-model="adMap.id" placeholder="自动识别" /></label>
              <label>标题字段<input v-model="adMap.title" placeholder="自动识别" /></label>
              <label>正文字段<input v-model="adMap.body" placeholder="自动识别（取最宽列）" /></label>
            </div>
            <div class="hint">蓝=正文候选 · 绿=标题候选 · 紫=身份候选。留空即用自动识别。</div>
          </div>
          <div class="srow">
            <input v-model="adJob" class="num" placeholder="任务名（可选）" />
          </div>
          <div v-if="adMeta" class="meta">{{ adMeta }}</div>
          <div class="hint">容器与字段自动判别（parquet 读 schema，不假设列名）；摄取走同一 Job 状态机，可断点续跑；必须指定 bucket。</div>
        </div>
        <div class="form">
          <label>目录（服务器本地路径）</label>
          <input v-model="ingDir" placeholder="/Users/you/documents" />
          <label class="chk"><input type="checkbox" v-model="ingRec" /> 包含子目录</label>
          <label>任务名（可选，缺省自动生成）</label>
          <input v-model="ingName" placeholder="docs1" />
          <button class="new" :disabled="ingBusy || !ingDir.trim()" @click="startIngest">
            {{ ingBusy ? "提交中……" : "开始摄取" }}
          </button>
          <div v-if="ingMeta" class="meta">{{ ingMeta }}</div>
          <div class="hint">路径为运行 serve 所在机器的本地目录；摄取异步执行，状态在左栏跟踪。支持 .md / .txt / .html / .htm。</div>
        </div>
        <div class="jd" v-if="curJob()">
          <div class="row">
            <span class="lc" :class="'st-' + curJob().state">{{ jobLabel[curJob().state] || curJob().state }}</span>
            <b>{{ curJob().id }}</b>
            <span class="sid">{{ curJob().phase || "—" }} · {{ curJob().done }}/{{ curJob().total }} 完成 · {{ curJob().failed }} 失败</span>
          </div>
          <div class="err" v-if="curJob().error">{{ curJob().error }}</div>
          <div class="sid" v-if="curJob().updated">{{ curJob().updated }}</div>
        </div>
      </div>
      <div class="body cfg" v-else-if="pane === 'settings'">
        <div class="card">
          <div class="ct">模型权重（MiniLM-L12 · 384 维）</div>
          <div class="row">
            <span class="lc" :class="settingsModel && settingsModel.installed ? 'st-done' : 'st-failed'">
              {{ settingsModel && settingsModel.installed ? "已安装" : "未安装" }}
            </span>
            <span class="sid">目录：{{ settingsModel ? settingsModel.dir : "—" }}</span>
          </div>
          <div class="hint" v-if="settingsModel && settingsModel.installed">
            来源：{{ settingsModel.source }}（{{ settingsModel.model_id }}）· 语义嵌入与簇复用使用这份权重
          </div>
          <div class="hint" v-else>
            未检测到权重。下载约 485MB（魔搭社区）后自动验证；CLI 亦可 <code>cumulus-cluster model install</code>。
          </div>
          <div class="row btns">
            <button class="new" :disabled="settingsBusy || (settingsModel && settingsModel.installing)" @click="installWeights">
              {{ settingsModel && settingsModel.installing ? "下载中……" : "下载权重" }}
            </button>
            <button class="new" :disabled="settingsBusy" @click="verifyWeights">验证并运行</button>
          </div>
          <div class="prog" v-if="settingsModel && settingsModel.installing && settingsModel.progress">
            {{ settingsModel.progress.file }} · {{ fmtMB(settingsModel.progress.done) }} / {{ fmtMB(settingsModel.progress.total) }}
          </div>
          <div class="meta" v-if="settingsMsg">{{ settingsMsg }}</div>
          <div class="files" v-if="settingsModel && settingsModel.files && settingsModel.files.length">
            <div class="ct2">已下载文件</div>
            <div v-for="f in settingsModel.files" :key="f.name" class="frow">
              <span>{{ f.name }}</span><span class="sid">{{ fmtMB(f.size) }}</span>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="ct">端点配置（脱敏）</div>
          <div class="frow"><span>Base URL</span><span class="sid">{{ (settingsConfig && settingsConfig.base_url) || "未设置（离线桩）" }}</span></div>
          <div class="frow"><span>Chat 模型</span><span class="sid">{{ (settingsConfig && settingsConfig.chat_model) || "—" }}</span></div>
          <div class="frow"><span>Embed 模型</span><span class="sid">{{ (settingsConfig && settingsConfig.embed_model) || "未设置（本地 embedder）" }}</span></div>
          <div class="frow"><span>API Key</span><span class="sid">{{ settingsConfig && settingsConfig.api_key_set ? "已设置（" + settingsConfig.api_key_len + " 字符）" : "未设置" }}</span></div>
          <div class="frow"><span>reasoning_split</span><span class="sid">{{ settingsConfig && settingsConfig.reasoning_split ? "自动（MiniMax 域）" : "关" }}</span></div>
          <div class="frow"><span>MiniLM 严格门</span><span class="sid">{{ settingsConfig && settingsConfig.minilm_required ? "开（CLUS_MINILM_REQUIRE=1）" : "关" }}</span></div>
        </div>
      </div>
      <div class="body ev" v-else>
        <div v-if="!evalCur" class="hint">从左栏选一次评测运行查看记分牌。</div>
        <template v-else>
          <div class="card">
            <div class="row">
              <b>{{ evalCur.tag || evalCur._id }}</b>
              <span class="lc" :class="evalCur.judged ? 'st-done' : 'st-queued'">{{ evalCur.judged ? "判官评分" : "无判官" }}</span>
              <span class="sid">n={{ evalCur.n }} · {{ evalCur.at }}</span>
            </div>
            <div class="grid">
              <div class="cell"><span class="k">判官 EM</span><span class="v">{{ pct((evalCur.system || {}).em) }}</span></div>
              <div class="cell"><span class="k">Ev.Rec</span><span class="v">{{ pct((evalCur.system || {}).ev_rec) }}</span></div>
              <div class="cell"><span class="k">Ground</span><span class="v">{{ pct((evalCur.system || {}).ground) }}</span></div>
              <div class="cell cb"><span class="k">Closed-Book EM</span><span class="v">{{ pct((evalCur.closed_book || {}).em) }}</span></div>
            </div>
            <div class="ct2">失败四分类（系统）</div>
            <div class="tax">
              <div class="trow"><span>答对</span><div class="bar"><i :style="{width: pct(txRatio((evalCur.system||{}).taxonomy,'correct'))}"></i></div><span>{{ ((evalCur.system || {}).taxonomy || {}).correct || 0 }}</span></div>
              <div class="trow"><span>找到未答好</span><div class="bar warn"><i :style="{width: pct(txRatio((evalCur.system||{}).taxonomy,'retrieved_but_unanswered'))}"></i></div><span>{{ ((evalCur.system || {}).taxonomy || {}).retrieved_but_unanswered || 0 }}</span></div>
              <div class="trow"><span>答错</span><div class="bar bad"><i :style="{width: pct(txRatio((evalCur.system||{}).taxonomy,'answered_but_wrong'))}"></i></div><span>{{ ((evalCur.system || {}).taxonomy || {}).answered_but_wrong || 0 }}</span></div>
              <div class="trow"><span>未定位</span><div class="bar gray"><i :style="{width: pct(txRatio((evalCur.system||{}).taxonomy,'not_retrieved'))}"></i></div><span>{{ ((evalCur.system || {}).taxonomy || {}).not_retrieved || 0 }}</span></div>
            </div>
            <div class="ct2">成本与显著性</div>
            <div class="frow"><span>search tokens</span><span class="sid">{{ evalCur.search_tokens || 0 }}</span></div>
            <div class="frow"><span>judge tokens</span><span class="sid">{{ evalCur.judge_tokens || 0 }}</span></div>
            <div class="frow"><span>拒绝的复用提案</span><span class="sid">{{ evalCur.rejected_proposals || 0 }}</span></div>
            <div class="frow"><span>McNemar</span><span class="sid">b={{ (evalCur.mcnemar || {}).b_only || 0 }} · c={{ (evalCur.mcnemar || {}).c_only || 0 }} · p={{ ((evalCur.mcnemar || {}).p || 0).toFixed(3) }}</span></div>
            <div class="frow"><span>档位</span><span class="sid">{{ Object.entries(evalCur.modes || {}).map(([k, v]) => k + "×" + v).join(" ") || "—" }}</span></div>
            <div class="frow"><span>题集指纹</span><span class="sid">{{ shortSha(((evalCur.extra || {}).frozen || {}).items_sha) }}</span></div>
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
.nssel input { width: 100%; box-sizing: border-box; padding: 6px 8px; border: 1px solid #e3e6ea; border-radius: 8px; font: 12px/1.4 -apple-system, "PingFang SC", sans-serif; color: #44505f; }
.hint { color: #98a2b0; font-size: 12px; padding: 8px 10px; }
.lc { font-size: 11px; padding: 1px 6px; border-radius: 999px; background: #eef1f5; color: #6b7684; white-space: nowrap; }
.lc.stable { background: #e7f6ec; color: #1a7f37; }
.lc.emerging { background: #fff4e0; color: #b25e09; }
.lc.contested { background: #fdeaea; color: #c0392b; }
.lc.deprecated { background: #eef1f5; color: #98a2b0; }
.clu { padding: 16px 24px; }
.cfg { padding: 16px 24px; display: flex; flex-direction: column; gap: 14px; }
.card { max-width: 720px; border: 1px solid #eef1f5; border-radius: 10px; padding: 12px 14px; }
.card .ct { font-weight: 700; font-size: 13px; margin-bottom: 8px; }
.card .ct2 { font-weight: 600; font-size: 12px; color: #44505f; margin: 10px 0 4px; }
.card .row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.card .row.btns { margin-top: 10px; }
.card .sid { color: #6b7684; font-size: 12px; }
.card .prog { font-size: 12px; color: #2456d6; margin-top: 8px; }
.card .files .frow { display: flex; justify-content: space-between; font-size: 12px; color: #44505f; padding: 2px 0; }
.card button.new { padding: 6px 14px; font-size: 12px; }
.card button.new:disabled { opacity: 0.5; cursor: not-allowed; }
.card code { background: #f2f5fa; border-radius: 4px; padding: 0 4px; font-size: 11px; }
.ev { padding: 16px 24px; }
.ev .grid { display: flex; gap: 10px; flex-wrap: wrap; margin: 10px 0 4px; }
.ev .cell { border: 1px solid #eef1f5; border-radius: 8px; padding: 8px 14px; display: flex; flex-direction: column; gap: 2px; min-width: 110px; }
.ev .cell.cb { border-style: dashed; }
.ev .cell .k { font-size: 11px; color: #6b7684; }
.ev .cell .v { font-size: 18px; font-weight: 700; color: #1f2937; }
.ev .tax { margin: 6px 0 10px; }
.ev .trow { display: flex; align-items: center; gap: 10px; font-size: 12px; color: #44505f; padding: 2px 0; }
.ev .trow > span:first-child { width: 80px; }
.ev .trow > span:last-child { width: 36px; text-align: right; color: #6b7684; }
.ev .bar { flex: 1; height: 8px; background: #f2f5fa; border-radius: 999px; overflow: hidden; }
.ev .bar i { display: block; height: 100%; background: #1a7f37; }
.ev .bar.warn i { background: #b25e09; }
.ev .bar.bad i { background: #c0392b; }
.ev .bar.gray i { background: #98a2b0; }
.ing { padding: 16px 24px; }
.form { max-width: 560px; display: flex; flex-direction: column; gap: 8px; }
.form label { font-size: 12px; color: #44505f; font-weight: 600; }
.form label.chk { font-weight: 400; display: flex; align-items: center; gap: 6px; }
.scan { max-width: 720px; border: 1px solid #eef1f5; border-radius: 10px; padding: 12px 14px; margin-bottom: 14px; }
.scan .ct { font-weight: 700; font-size: 13px; margin-bottom: 8px; }
.scan .srow { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.scan .srow.between { justify-content: space-between; margin: 8px 0 4px; }
.scan input:not([type]) { flex: 1; min-width: 180px; padding: 6px 8px; border: 1px solid #e3e6ea; border-radius: 8px; font: 12px/1.4 -apple-system, "PingFang SC", sans-serif; }
.scan input.num { width: 90px; flex: none; }
.scan .lk a { color: #2456d6; text-decoration: none; font-size: 12px; }
.scan .sres { margin-top: 8px; }
.scan .cand { display: flex; justify-content: space-between; gap: 10px; padding: 3px 0; font-size: 12px; }
.scan .cand label { display: flex; align-items: center; gap: 6px; color: #44505f; }
.scan .cand .sid { color: #6b7684; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 55%; }
.scan button.new { padding: 6px 14px; font-size: 12px; }
.scan button.new:disabled { opacity: 0.5; cursor: not-allowed; }
.form input[type="text"], .form input:not([type]) { padding: 8px 10px; border: 1px solid #e3e6ea; border-radius: 8px; font: 13px/1.5 -apple-system, "PingFang SC", sans-serif; }
.form button.new { align-self: flex-start; padding: 8px 18px; }
.form button.new:disabled { opacity: 0.5; cursor: not-allowed; }
.jd { max-width: 860px; margin: 16px auto 0; border: 1px solid #eef1f5; border-radius: 10px; padding: 12px 14px; }
.jd .row { display: flex; align-items: center; gap: 10px; }
.jd .sid { color: #6b7684; font-size: 12px; }
.jd .err { color: #c0392b; font-size: 12px; margin-top: 6px; word-break: break-all; }
.lc.st-queued { background: #eef1f5; color: #6b7684; }
.lc.st-running { background: #e8eefc; color: #2456d6; }
.lc.st-done { background: #e7f6ec; color: #1a7f37; }
.lc.st-failed { background: #fdeaea; color: #c0392b; }
.ch { max-width: 860px; margin: 0 auto; }
.ch .row { display: flex; align-items: center; gap: 10px; }
.ch .sid { color: #6b7684; font-size: 12px; }
.qs { margin: 8px 0; font-size: 12px; color: #6b7684; }
.qs .q { display: inline-block; background: #f2f5fa; border-radius: 6px; padding: 1px 8px; margin: 0 4px 4px 0; }
.content { white-space: pre-wrap; background: #f8fafc; border: 1px solid #eef1f5; border-radius: 10px; padding: 12px 14px; font: 13px/1.7 -apple-system, "PingFang SC", sans-serif; }
.cites { margin-top: 12px; }
.cites .ct { font-weight: 700; font-size: 13px; margin-bottom: 6px; }
.cites .cite { font-size: 12px; color: #44505f; padding: 3px 0; }
  .mgrid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 12px; }
  .mcard { border: 1px solid var(--border, #2a2f3a); border-radius: 10px; padding: 12px 14px; }
  .mcard .mt { font-weight: 700; margin-bottom: 8px; }
  .mrow { display: flex; justify-content: space-between; gap: 12px; font-size: 12px; padding: 2px 0; }
  .mrow span { opacity: .65; }
  .mtab { width: 100%; border-collapse: collapse; font-size: 12px; margin: 6px 0 12px; }
  .mtab th, .mtab td { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--border, #2a2f3a); }
  .mtab th { opacity: .6; font-weight: 600; }
  .body.mon { overflow: auto; }
  .body.ing textarea { width: 100%; background: transparent; border: 1px solid var(--border, #2a2f3a); border-radius: 8px; color: inherit; font-family: ui-monospace, monospace; font-size: 12px; padding: 6px 8px; }
  .pres { margin: 8px 0; }
  .prow { border: 1px solid var(--border, #2a2f3a); border-radius: 8px; padding: 8px 10px; margin-bottom: 6px; font-size: 12px; }
  .flds { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 4px; }
  .flds span { border: 1px solid var(--border, #2a2f3a); border-radius: 999px; padding: 1px 8px; font-size: 11px; opacity: .75; }
  .flds span.b { border-color: #3a86ff; color: #4895ef; opacity: 1; }
  .flds span.t { border-color: #2ea043; color: #3fb950; opacity: 1; }
  .flds span.i { border-color: #8957e2; color: #a371f7; opacity: 1; }
  .maprow { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 6px; }
  .maprow label { font-size: 11px; display: flex; flex-direction: column; gap: 2px; }
  .maprow input { width: 130px; background: transparent; border: 1px solid var(--border, #2a2f3a); border-radius: 6px; color: inherit; font-size: 12px; padding: 3px 6px; }
</style>

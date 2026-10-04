<template>
  <div class="workspace" :class="{ 'history-open': historyOpen }">
    <aside class="session-rail" aria-label="历史会话">
      <!-- 会话栏走库件 EbChatThreads：日期分组/悬停更多菜单/归档折叠都是它的本职。
           置顶与归档是视图态（后端会话无此字段），按库存 localStorage；
           后端没有改名接口，renamable 关掉，避免出现提交无效果的死菜单项。 -->
      <EbChatThreads :threads="threadItems" :active="current" :searchable="false" :renamable="false"
                     :time-format="fmtSessionTime"
                     @select="pickSession" @create="startSession" @remove="delSession"
                     @pin="(id, pinned) => setFlag(id, { pinned })" @archive="(id, archived) => setFlag(id, { archived })">
        <template #empty><p class="tiny rail-note">提问后自动保存会话。</p></template>
      </EbChatThreads>
    </aside>
    <section class="conversation">
      <header class="conversation-head">
        <div class="conversation-heading"><span class="status-dot" :class="{ ready: hasBucket && documents.length }"></span><strong>{{ libraryLabel }}</strong><span class="tiny">{{ documents.length }} 篇文档</span></div>
        <div class="form-actions">
          <eb-button size="small" @click="pane = 'corpus'">语料</eb-button>
          <eb-button v-if="hasBucket && resetting !== 'done'" size="small" :loading="resetting === 'busy'" @click="clearCache">清除缓存</eb-button>
          <eb-button v-else-if="resetting === 'done'" size="small" type="success" text @click="resetting = ''">✓ 已清除</eb-button>
          <eb-button class="history-toggle" type="primary" link size="small" :aria-expanded="historyOpen" @click="historyOpen = !historyOpen">历史会话</eb-button>
        </div>
      </header>
      <eb-alert v-if="error" class="conversation-error" type="error" :title="error" :closable="false" show-icon />
      <div v-if="!hasBucket" class="welcome">
        <span class="welcome-eyebrow">开始使用 Cumulus</span>
        <h2>让文档成为可追溯的答案</h2>
        <eb-button type="primary" @click="$emit('create-library')">创建第一个知识库</eb-button>
      </div>
      <div v-else-if="documentsError && !messages.length" class="welcome">
        <h2>无法读取文档</h2><p role="alert">{{ documentsError }}</p><eb-button @click="loadDocuments">重试</eb-button>
      </div>
      <div v-else-if="documentsBusy && !documents.length && !messages.length" class="welcome" role="status">正在读取知识库…</div>
      <div v-else-if="!documents.length && !messages.length" class="welcome">
        <span class="welcome-eyebrow">知识库已就绪</span>
        <h2>添加文档，开始提问</h2>
        <eb-button type="primary" @click="pane = 'corpus'">添加文档</eb-button>
      </div>
      <template v-else>
        <div class="conversation-stream">
          <!-- 会话区与输入台分开组合（组件规范方案）：消息列表用 EbChatList，
               输入用编排层 EbAiPromptBox——eb-chatbot 的整体封装不透传
               message-content，引用卡/运行卡没有渲染位，故走组合式。 -->
          <div v-if="!messages.length" class="welcome">
            <span class="welcome-eyebrow">基于 {{ documents.length }} 篇文档</span>
            <h2>你想从文档中了解什么？</h2>
            <EbChatSuggestion class="sample-questions" layout="column" :items="samples" @pick="(s) => onSend(s.prompt)" />
          </div>
          <EbChatList v-show="messages.length" class="conv-list" :messages="messages"
                      assistant-name="Cumulus" :show-avatar="false"
                      @regenerate="regenerate">
            <!-- 只接管正文渲染：消息外壳（思考/动作条）仍是组件默认的。
                 答案正文 + 分步时间轴 + 引用卡 + 运行卡都挂在本条消息下面，
                 随消息一起进历史、一起刷新恢复。 -->
            <template #message-content="{ message }">
              <div v-if="message.role === 'user'" class="user-body">{{ message.content }}</div>
              <template v-else>
                <EbChatMarkdown :content="message.content || ''" :streaming="message.status === 'streaming'" />
                <!-- 分步时间轴：进行中常驻（到哪了、每段多久），完成后收成一行
                     可展开的注脚——检索过程是佐证材料，不是答案的一部分。 -->
                <details v-if="!message.error && (message.stages?.length || Object.keys(message.stats?.stages || {}).length)" class="run-timeline" :open="message.status !== 'done' && message.status !== 'error'">
                  <summary class="tl-summary">检索过程 · {{ timelineFor(message).length }} 段<template v-if="message.status === 'done'"> · {{ totalMsOf(message) }}</template></summary>
                  <div v-for="(st, i) in timelineFor(message)" :key="st.name + i" class="tl-step">
                    <span class="tl-dot" :class="{ live: st.ms === 0 && message.status !== 'done' }" />
                    <span class="tl-name">{{ stageText(st.name) }}</span>
                    <span class="tl-ms">{{ st.ms ? fmtMS(st.ms) : (message.status === 'done' ? '' : '进行中…') }}</span>
                    <div v-if="stageDetailLines(st.name, st.detail).length" class="tl-detail">
                      <span v-for="(line, j) in stageDetailLines(st.name, st.detail)" :key="j">{{ line }}</span>
                    </div>
                  </div>
                </details>
                <!-- 引用：编号出处列表（EbChatSources），点条目开原文抽屉。
                     未定位窗口在标题上标注，不藏进交互。 -->
                <EbChatSources v-if="message.sources?.length" :items="message.sources"
                               @item-click="(item) => (item.raw || item.source) && preview(item.raw || item.source, item.snippet)" />
                <!-- 运行卡：icon + 指标 + 数据的统计行，不用 tag。簇 id 是内部
                     机制标识，不上可见文案——挂在整行 title 上供排查悬停查看。 -->
                <div v-if="message.stats" class="run-card" :title="message.stats.cluster_id ? '知识簇 ' + message.stats.cluster_id : undefined">
                  <span class="run-item"><eb-icon name="speed-line" /><b>{{ tierLabel(message.stats.mode) }}</b></span>
                  <span class="run-item"><eb-icon name="check-line" />置信度 <b>{{ Math.round((message.stats.conf || 0) * 100) }}%</b></span>
                  <span class="run-item"><eb-icon name="pie-chart-line" />覆盖率 <b>{{ Math.round((message.stats.coverage || 0) * 100) }}%</b></span>
                  <span class="run-item"><eb-icon name="stack-line" /><b>{{ message.stats.loops }}</b> 轮</span>
                  <span class="run-item"><eb-icon name="time-line" />总耗时 <b>{{ ((message.stats.latency || 0) / 1000).toFixed(1) }}s</b></span>
                  <span v-if="message.stats.reused" class="run-item"><eb-icon name="links-line" />复用已有知识</span>
                  <!-- token 总量 + 去向：库件的披露阶梯（segments 跟在总量后，
                       点击展开分段），顶替原先的手写 tokens 项与 token 行。 -->
                  <EbChatUsage class="run-item" :usage="usageOf(message.stats)" size="compact" bare />
                </div>
                <eb-alert v-else-if="message.insufficient" type="warning" title="当前证据不足，建议补充文档或缩小问题范围。" :closable="false" show-icon />
                <eb-alert v-else-if="message.stats?.refused || message.refused" type="warning" :closable="false" show-icon
                          title="这份语料里没有能直接回答这个问题的依据">
                  下面给出的是本库最接近的条文，<b>不等于答案</b>。请补充相关法规或文档后再问（例如工伤认定需要《工伤保险条例》）。
                </eb-alert>
              </template>
            </template>
          </EbChatList>
          <!-- 悬浮进度条：贴在输入台上方，一行交代「第几段/本段多久/总共多久」，
               加载图标常转。完整分段仍留在答案下面的时间轴里。 -->
          <div v-if="loading && (liveStages.length || liveStage)" class="float-progress" role="status">
            <eb-icon name="loader-line" class="fp-spin" />
            <span class="fp-stage">{{ stageText(liveStage || liveStages[liveStages.length-1]?.name) }}</span>
            <span class="fp-num">{{ fpIndex }}/{{ STAGE_ORDER.length }}</span>
            <span class="fp-sep">·</span>
            <span class="fp-item">本段 <b>{{ fpSeg }}</b></span>
            <span class="fp-sep">·</span>
            <span class="fp-item">总耗时 <b>{{ fpTotal }}</b></span>
          </div>
          <!-- 输入台：编排层组件（场景/能力/附件等开关面我们不开，保持
               检索问答的单一动作）。send 载荷取 text；loading 时发钮即停钮。 -->
          <EbAiPromptBox v-model="draft" :loading="loading" stoppable :allow-attachments="false"
                         placeholder="向当前知识库提问，Enter 发送，Shift + Enter 换行"
                         @send="(p) => onSend(p.text)" @stop="stop" />
        </div>
      </template>
    </section>
    <!-- 原文预览抽屉：引用卡的「查看」在此打开，右侧滑入，不打断会话。 -->
    <SourcePreview v-if="previewSource" :source-id="previewSource" :highlight="previewQuote" @close="previewSource = ''" />
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { EbChatThreads, EbChatList, EbChatMarkdown, EbChatSources, EbChatSuggestion, EbChatUsage, EbAiPromptBox } from "@wil-works/evoke-chat";
import { useChatPane, timelineFor, stageText, stageDetailLines, fmtMS, usageOf, STAGE_ORDER } from "../panes/chat.js";
import { documents, documentsBusy, documentsError, hasBucket, libraryLabel, loadDocuments, nsSel, pane } from "../state.js";
import { requestJSON } from "../api.js";
import SourcePreview from "./SourcePreview.vue";

defineEmits(["create-library"]);
const { sessions, current, loading, error, messages, onSend, stop, openSession, newSession, delSession,
  liveStages, liveStage, elapsed } = useChatPane();
const historyOpen = ref(false);
const resetting = ref("");
const draft = ref("");
const previewSource = ref("");
const previewQuote = ref("");
const samples = ["有哪些关键要求？", "有哪些例外情形？", "总结文档中的注意事项"];
function preview(sourceId, quote) { previewSource.value = sourceId; previewQuote.value = quote || ""; }
function tierLabel(mode) { return mode === "DEEP" ? "深度检索" : mode === "FAST" ? "快速回答" : mode || "检索"; }
function totalMsOf(message) {
  const total = timelineFor(message).reduce((sum, st) => sum + (st.ms || 0), 0);
  return fmtMS(total);
}
// 悬浮条的本地计时：服务端 elapsed 只在 stage 事件到达时跳变，条上要平滑走字。
const startTs = ref(0), segTs = ref(0), nowTs = ref(0);
watch(loading, (v) => {
  if (v) { startTs.value = segTs.value = nowTs.value = Date.now(); }
});
watch(() => liveStages.value.length, () => { segTs.value = Date.now(); });
const fpIndex = computed(() => {
  const idx = STAGE_ORDER.indexOf(liveStage.value || "");
  return idx < 0 ? liveStages.value.length + 1 : idx + 1;
});
const fpSeg = computed(() => ((nowTs.value - segTs.value) / 1000).toFixed(1) + "s");
const fpTotal = computed(() => ((nowTs.value - startTs.value) / 1000).toFixed(1) + "s");
let fpTimer = 0;
watch(loading, (v) => {
  if (fpTimer) { clearInterval(fpTimer); fpTimer = 0; }
  if (v) fpTimer = setInterval(() => { nowTs.value = Date.now(); }, 500);
});
onUnmounted(() => { if (fpTimer) clearInterval(fpTimer); });
// 会话 → 线程条目：updated_at 归一成毫秒；置顶/归档是本机视图态，按库隔离存。
const threadFlags = ref({});
const flagsKey = computed(() => "cumulus-threads-" + (nsSel.value || ""));
function loadFlags() {
  try { threadFlags.value = JSON.parse(localStorage.getItem(flagsKey.value) || "{}"); }
  catch { threadFlags.value = {}; }
}
function setFlag(id, patch) {
  threadFlags.value = { ...threadFlags.value, [id]: { ...threadFlags.value[id], ...patch } };
  try { localStorage.setItem(flagsKey.value, JSON.stringify(threadFlags.value)); } catch { /* 私密模式等存不了就算了 */ }
}
watch(flagsKey, loadFlags, { immediate: true });
const threadItems = computed(() => sessions.value.map(session => {
  const ts = Number(session.updated_at) || 0;
  return { id: session.id, title: session.title || "", updatedAt: ts > 0 && ts < 1e12 ? ts * 1000 : ts, ...(threadFlags.value[session.id] || {}) };
}));
function pickSession(id) { const session = sessions.value.find(item => item.id === id); if (session) selectSession(session); }
function startSession() { newSession(); historyOpen.value = false; }

async function clearCache() {
  const ns = nsSel.value;
  if (!ns || resetting.value === "busy") return;
  resetting.value = "busy";
  try {
    const nsParam = ns ? `?ns=${encodeURIComponent(ns)}` : "";
    await requestJSON(`/v1/learning/reset${nsParam}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ confirm: ns }),
    });
    resetting.value = "done";
    setTimeout(() => { resetting.value = ""; }, 2000);
  } catch (e) {
    resetting.value = "";
  }
}
// 会话时间列：固定 YYYY-MM-DD HH:mm:ss，不跟浏览器 locale（en-US 会出 9/28/2026）。
function fmtSessionTime(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
function selectSession(session) { openSession(session); historyOpen.value = false; }
function regenerate(message) {
  const index = messages.value.findIndex(item => item.id === message.id);
  const question = messages.value.slice(0, index).findLast(item => item.role === "user");
  if (question && !loading.value) onSend(question.content);
}
onMounted(loadDocuments);
</script>

<style scoped>
.workspace { display: grid; grid-template-columns: 216px minmax(0, 1fr); min-height: 0; background: var(--eb-bg-color); }
.session-rail { min-width: 0; overflow: hidden; border-right: 1px solid var(--eb-border-color-light); }
.rail-note { padding: 12px; line-height: 1.8; }
.conversation { display: flex; flex-direction: column; min-width: 0; min-height: 0; }
.conversation-head { padding: 18px 24px; border-bottom: 1px solid var(--eb-border-color-light); display: flex; gap: 12px; align-items: center; justify-content: space-between; flex-wrap: wrap; }
.conversation-heading { display: flex; align-items: center; gap: 9px; font-size: 14px; min-width: 0; }
.conversation-heading strong { overflow-wrap: anywhere; }
.history-toggle { display: none; }
/* stream 自身是 flex 列：conv-list 占满滚动、输入台钉底；横向 24px 与头部对齐
   （EbAiPromptBox 是 width:100%，留白由宿主给，不给就贴视口边）。 */
.conversation-stream { flex: 1; min-height: 0; display: flex; flex-direction: column; padding: 0 24px 12px; }
.conv-list { flex: 1; min-height: 0; overflow: auto; }
.welcome { flex: 1; max-width: 1000px; width: 100%; margin: auto; padding: 40px 24px; display: flex; flex-direction: column; align-items: center; justify-content: center; text-align: center; }
.welcome-eyebrow { color: var(--eb-color-primary); font-size: 12px; font-weight: 600; letter-spacing: .08em; }
.welcome h2 { margin: 16px 0 12px; font-size: clamp(22px, 2.5vw, 30px); line-height: 1.4; letter-spacing: -.7px; }
.welcome p { margin: 0 0 24px; max-width: 440px; color: var(--eb-text-color-secondary); font-size: 14px; line-height: 1.9; }
.sample-questions { display: flex; justify-content: center; }
/* 引用卡的来源行：解码后的路径最多两行，超出截断（组件自身的单行省略
   对长路径太狠，整段路径值得两行）。 */
.conv-list :deep(.eb-chat-sources__meta) {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  overflow: hidden;
  word-break: break-all;
}
.user-body { white-space: pre-wrap; overflow-wrap: anywhere; }
/* 悬浮进度条：贴在输入区上方的一行胶囊——当前段 + 第几段/共几段 + 本段/
     总耗时，加载图标常转。 */
.conversation-stream { position: relative; }
.float-progress { position: absolute; left: 50%; transform: translateX(-50%); bottom: 140px; z-index: 5; display: flex; align-items: center; gap: 7px; max-width: min(92%, 640px); padding: 7px 14px; border: 1px solid var(--eb-border-color-lighter); border-radius: 999px; background: var(--eb-bg-color); box-shadow: 0 6px 24px rgba(0, 0, 0, .12); font-size: 12px; color: var(--eb-text-color-secondary); white-space: nowrap; overflow: hidden; animation: fp-in var(--eb-duration-base, .2s) var(--eb-ease-out); }
@keyframes fp-in { from { opacity: 0; transform: translate(-50%, 6px); } to { opacity: 1; transform: translate(-50%, 0); } }
.float-progress .fp-spin { color: var(--eb-color-primary); font-size: 14px; animation: lp-rotate 1.1s linear infinite; }
@keyframes lp-rotate { to { transform: rotate(360deg); } }
.fp-stage { font-weight: 600; color: var(--eb-text-color-regular); overflow: hidden; text-overflow: ellipsis; }
.fp-num { font-variant-numeric: tabular-nums; color: var(--eb-color-primary); font-weight: 600; }
.fp-sep { color: var(--eb-text-color-placeholder); }
.fp-item b { font-variant-numeric: tabular-nums; color: var(--eb-text-color-regular); font-weight: 600; }
/* 分步时间轴（消息内）：完成后是 <details> 折叠注脚——一行摘要常驻，
     展开是圆点竖列；进行中默认展开，live 段呼吸闪烁。 */
.run-timeline { margin: 10px 0 4px; padding: 6px 12px; border: 1px solid var(--eb-border-color-lighter); border-radius: 8px; background: var(--eb-fill-color-light); display: flex; flex-direction: column; gap: 4px; }
.run-timeline[open] { padding-bottom: 10px; }
.tl-summary { cursor: pointer; font-size: 12px; color: var(--eb-text-color-secondary); list-style: none; display: flex; align-items: center; gap: 6px; user-select: none; }
.tl-summary::-webkit-details-marker { display: none; }
.tl-summary::before { content: ""; width: 0; height: 0; border-left: 4px solid var(--eb-text-color-placeholder); border-top: 3px solid transparent; border-bottom: 3px solid transparent; transition: transform .15s ease; }
.run-timeline[open] .tl-summary::before { transform: rotate(90deg); }
.tl-step { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--eb-text-color-secondary); padding-top: 4px; flex-wrap: wrap; }
.tl-detail { flex-basis: 100%; padding-left: 15px; display: flex; flex-direction: column; gap: 2px; font-size: 12px; color: var(--eb-text-color-placeholder); line-height: 1.6; }
.tl-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--eb-color-primary); flex: none; }
.tl-dot.live { background: var(--eb-text-color-placeholder); animation: tl-pulse 1.2s ease-in-out infinite; }
@keyframes tl-pulse { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }
.tl-name { flex: 1; min-width: 0; }
.tl-ms { font-variant-numeric: tabular-nums; color: var(--eb-text-color-regular); }
/* 运行卡：icon + 指标 + 数据 的一行统计（不用 tag）。 */
.run-card { margin: 8px 0 2px; display: flex; flex-wrap: wrap; align-items: center; gap: 4px 14px; font-size: 12px; color: var(--eb-text-color-secondary); }
.run-item { display: inline-flex; align-items: center; gap: 4px; white-space: nowrap; }
.run-item b { font-variant-numeric: tabular-nums; color: var(--eb-text-color-regular); font-weight: 600; }
.run-item.dim { color: var(--eb-text-color-placeholder); }
.conversation-error { margin: var(--eb-space-3) var(--eb-space-6) 0; width: auto; flex: none; }
@media (max-width: 1000px) {
  .workspace { grid-template-columns: minmax(0, 1fr); }
  .session-rail { display: none; }
  .history-toggle { display: inline-flex; }
  .workspace.history-open { grid-template-rows: minmax(100px, 28%) minmax(0, 1fr); }
  .history-open .session-rail { display: block; border-bottom: 1px solid var(--eb-border-color-light); padding: 12px; }
  .conversation-head { padding: 14px 16px; }
  .live-progress { margin: 8px 12px 0; }
}
</style>

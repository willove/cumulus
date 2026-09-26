<template>
  <div class="workspace" :class="{ 'history-open': historyOpen }">
    <aside class="session-rail" aria-label="历史会话">
      <div class="rail-head"><span class="rail-title">历史会话</span><span class="tiny">{{ sessions.length }}</span></div>
      <div v-if="sessionsBusy" class="tiny rail-note" role="status">正在加载会话…</div>
      <div v-for="session in sessions" :key="session.id" class="session-item" :class="{ cur: session.id === current }">
        <eb-button class="session-open" text :type="session.id === current ? 'primary' : 'default'" @click="selectSession(session)">
          <span class="session-title">{{ session.title || '未命名会话' }}</span>
          <span v-if="session.updated_at" class="tiny session-time">{{ fmtTime(session.updated_at) }}</span>
        </eb-button>
        <eb-popconfirm title="删除这段会话？文档不受影响。" confirm-button-text="删除" confirm-button-type="danger" @confirm="delSession(session.id)">
          <eb-button text type="danger" size="small" icon="delete" :aria-label="'删除会话 ' + (session.title || session.id)" />
        </eb-popconfirm>
      </div>
      <p v-if="!sessions.length && !sessionsBusy" class="tiny rail-note">提问后会自动保存会话，方便继续追问。</p>
    </aside>
    <section class="conversation">
      <header class="conversation-head">
        <div class="conversation-heading"><span class="status-dot" :class="{ ready: hasBucket && documents.length }"></span><strong>{{ libraryLabel }}</strong><span class="tiny">{{ documents.length }} 篇文档</span></div>
        <div class="form-actions">
          <eb-button class="history-toggle" type="primary" link size="small" :aria-expanded="historyOpen" @click="historyOpen = !historyOpen">历史会话</eb-button>
          <eb-button size="small" :disabled="!hasBucket" @click="newSession">新会话</eb-button>
          <eb-button size="small" @click="pane = 'documents'">管理文档</eb-button>
        </div>
      </header>
      <eb-alert v-if="error" class="conversation-error" type="error" :title="error" :closable="false" show-icon />
      <div v-if="!hasBucket" class="welcome">
        <span class="welcome-eyebrow">开始使用 Cumulus</span>
        <h2>让文档成为可追溯的答案</h2>
        <p>先创建一个知识库，再导入文档。每次提问都只检索当前库，并保留可核对的原文证据。</p>
        <eb-button type="primary" @click="$emit('create-library')">创建第一个知识库</eb-button>
      </div>
      <div v-else-if="documentsError && !messages.length" class="welcome">
        <h2>无法读取文档</h2><p role="alert">{{ documentsError }}</p><eb-button @click="loadDocuments">重试</eb-button>
      </div>
      <div v-else-if="documentsBusy && !documents.length && !messages.length" class="welcome" role="status">正在读取知识库…</div>
      <div v-else-if="!documents.length && !messages.length" class="welcome">
        <span class="welcome-eyebrow">知识库已就绪</span>
        <h2>添加文档，开始提问</h2>
        <p>当前知识库还没有文档。支持本地目录、候选扫描，以及 JSON、CSV、Parquet 等结构化文件。</p>
        <eb-button type="primary" @click="pane = 'documents'">添加文档</eb-button>
      </div>
      <template v-else>
        <div class="conversation-stream">
          <eb-chatbot v-model="messages" :loading="loading" height="100%" :show-tip="false"
                      :allow-attachments="false" :allow-drop="false" :show-avatar="false"
                      assistant-name="Cumulus" placeholder="向当前知识库提问，Enter 发送，Shift + Enter 换行"
                      stoppable @send="onSend" @stop="stop" @regenerate="regenerate">
            <template #empty>
              <div class="welcome">
                <span class="welcome-eyebrow">基于 {{ documents.length }} 篇文档</span>
                <h2>你想从文档中了解什么？</h2>
                <p>回答会附上证据窗口、分步耗时和可核对的原文引用。</p>
                <div class="sample-questions"><eb-button v-for="question in samples" :key="question" size="small" @click="onSend(question)">{{ question }}</eb-button></div>
              </div>
            </template>
            <!-- 只接管正文渲染：消息外壳（头像/思考/动作条）仍是组件默认的。
                 答案正文 + 分步时间轴 + 引用卡 + 运行卡都挂在本条消息下面，
                 随消息一起进历史、一起刷新恢复。 -->
            <template #message-content="{ message }">
              <div v-if="message.role === 'user'" class="user-body">{{ message.content }}</div>
              <template v-else>
                <EbChatMarkdown :content="message.content || ''" :streaming="message.status === 'streaming'" />
                <!-- 分步时间轴：实时阶段用事件流（每完成一段推一行），完成后用
                     done 的权威分段（含每段耗时与累计）。用户看见的是"到哪了、
                     每段多久"，而不是一个静止的转圈。 -->
                <div v-if="!message.error && (message.stages?.length || Object.keys(message.stats?.stages || {}).length)" class="run-timeline" role="status">
                  <div v-for="(st, i) in timelineFor(message)" :key="st.name + i" class="tl-step">
                    <span class="tl-dot" :class="{ live: st.ms === 0 && message.status !== 'done' }" />
                    <span class="tl-name">{{ stageText(st.name) }}</span>
                    <span class="tl-ms">{{ st.ms ? fmtMS(st.ms) : (message.status === 'done' ? '' : '进行中…') }}</span>
                  </div>
                </div>
                <!-- 引用卡：文件卡片式自适应网格——单张占满一行，多张一行两个，
                     容器过窄（<~640px）自动回落单列。右侧「查看」开抽屉看原文。 -->
                <div v-if="message.sources?.length" class="cite-block">
                  <div class="cite-head tiny"><eb-icon name="links-line" /> 引用 {{ message.sources.length }} 个原文窗口</div>
                  <div class="cite-grid" :class="{ single: message.sources.length === 1 }">
                    <article v-for="source in message.sources" :key="source.index" class="cite-card">
                      <span class="cite-glyph"><eb-icon name="file-text-line" /></span>
                      <div class="cite-main">
                        <span class="cite-title" :title="source.title">{{ source.title }}</span>
                        <span v-if="source.resolved === false" class="tiny warn">未定位</span>
                        <p class="quote" :title="source.snippet">{{ source.snippet }}</p>
                      </div>
                      <eb-button size="small" text type="primary" :disabled="!source.source" @click="preview(source.source, source.snippet)">查看</eb-button>
                    </article>
                  </div>
                </div>
                <!-- 运行卡：icon + 指标 + 数据的统计行，不用 tag。 -->
                <div v-if="message.stats" class="run-card">
                  <span class="run-item"><eb-icon name="speed-line" /><b>{{ tierLabel(message.stats.mode) }}</b></span>
                  <span class="run-item"><eb-icon name="check-line" />置信度 <b>{{ Math.round((message.stats.conf || 0) * 100) }}%</b></span>
                  <span class="run-item"><eb-icon name="pie-chart-line" />覆盖率 <b>{{ Math.round((message.stats.coverage || 0) * 100) }}%</b></span>
                  <span class="run-item"><eb-icon name="stack-line" /><b>{{ message.stats.loops }}</b> 轮</span>
                  <span class="run-item"><eb-icon name="database-2-line" /><b>{{ fmtTokens(message.stats.tokens) }}</b> tokens</span>
                  <span class="run-item"><eb-icon name="time-line" />总耗时 <b>{{ ((message.stats.latency || 0) / 1000).toFixed(1) }}s</b></span>
                  <span v-if="message.stats.reused" class="run-item"><eb-icon name="links-line" />复用已有知识</span>
                  <span v-if="message.stats.cluster_id" class="run-item dim">簇 {{ message.stats.cluster_id }}</span>
                </div>
                <eb-alert v-else-if="message.insufficient" type="warning" title="当前证据不足，建议补充文档或缩小问题范围。" :closable="false" show-icon />
                <eb-alert v-else-if="message.stats?.refused || message.refused" type="warning" :closable="false" show-icon
                          title="这份语料里没有能直接回答这个问题的依据">
                  下面给出的是本库最接近的条文，<b>不等于答案</b>。请补充相关法规或文档后再问（例如工伤认定需要《工伤保险条例》）。
                </eb-alert>
              </template>
            </template>
          </eb-chatbot>
          <!-- 悬浮进度条：贴在输入框上方，一行交代「第几段/本段多久/总共多久」，
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
        </div>
      </template>
    </section>
    <!-- 原文预览抽屉：引用卡的「查看」在此打开，右侧滑入，不打断会话。 -->
    <SourcePreview v-if="previewSource" :source-id="previewSource" :highlight="previewQuote" @close="previewSource = ''" />
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { EbChatMarkdown } from "@wil-works/evoke-chat";
import { useChatPane, timelineFor, stageText, fmtMS, STAGE_ORDER } from "../panes/chat.js";
import { documents, documentsBusy, documentsError, hasBucket, libraryLabel, loadDocuments, pane } from "../state.js";
import SourcePreview from "./SourcePreview.vue";

defineEmits(["create-library"]);
const { sessions, sessionsBusy, current, loading, error, messages, onSend, stop, openSession, newSession, delSession,
  liveStages, liveStage, elapsed } = useChatPane();
const historyOpen = ref(false);
const previewSource = ref("");
const previewQuote = ref("");
const samples = ["有哪些关键要求？", "有哪些例外情形？", "总结文档中的注意事项"];
function preview(sourceId, quote) { previewSource.value = sourceId; previewQuote.value = quote || ""; }
function tierLabel(mode) { return mode === "DEEP" ? "深度检索" : mode === "FAST" ? "快速回答" : mode || "检索"; }
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
function fmtTokens(n) { return n >= 10000 ? (n / 1000).toFixed(1) + "K" : String(n || 0); }
function fmtTime(at) {
  if (!at) return "";
  const d = new Date(typeof at === "number" && at < 1e12 ? at * 1000 : at);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
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
.session-rail { padding: 24px 12px; overflow: auto; border-right: 1px solid var(--eb-border-color-light); background: var(--eb-fill-color-light); }
.rail-note { padding: 12px; line-height: 1.8; }
.session-item { display: flex; align-items: center; gap: var(--eb-space-1); margin: var(--eb-space-1) 0; }
.session-open { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; justify-content: flex-start; flex-direction: column; align-items: flex-start; gap: 0; height: auto; padding: 6px 8px; }
.session-title { max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.session-time { color: var(--eb-text-color-placeholder); }
.conversation { display: flex; flex-direction: column; min-width: 0; min-height: 0; }
.conversation-head { padding: 18px 24px; border-bottom: 1px solid var(--eb-border-color-light); display: flex; gap: 12px; align-items: center; justify-content: space-between; flex-wrap: wrap; }
.conversation-heading { display: flex; align-items: center; gap: 9px; font-size: 14px; min-width: 0; }
.conversation-heading strong { overflow-wrap: anywhere; }
.history-toggle { display: none; }
.conversation-stream { flex: 1; min-height: 0; overflow: hidden; }
.welcome { flex: 1; max-width: 1000px; width: 100%; margin: auto; padding: 40px 24px; display: flex; flex-direction: column; align-items: center; justify-content: center; text-align: center; }
.welcome-eyebrow { color: var(--eb-color-primary); font-size: 12px; font-weight: 600; letter-spacing: .08em; }
.welcome h2 { margin: 16px 0 12px; font-size: clamp(22px, 2.5vw, 30px); line-height: 1.4; letter-spacing: -.7px; }
.welcome p { margin: 0 0 24px; max-width: 440px; color: var(--eb-text-color-secondary); font-size: 14px; line-height: 1.9; }
.sample-questions { display: flex; flex-wrap: wrap; justify-content: center; gap: 8px; }
.user-body { white-space: pre-wrap; overflow-wrap: anywhere; }
/* 悬浮进度条：贴在输入区上方的一行胶囊——当前段 + 第几段/共几段 + 本段/
     总耗时，加载图标常转。 */
.conversation-stream { position: relative; }
.float-progress { position: absolute; left: 50%; transform: translateX(-50%); bottom: 140px; z-index: 5; display: flex; align-items: center; gap: 7px; max-width: min(92%, 640px); padding: 7px 14px; border: 1px solid var(--eb-border-color-lighter); border-radius: 999px; background: var(--eb-bg-color); box-shadow: 0 6px 24px rgba(0, 0, 0, .12); font-size: 12px; color: var(--eb-text-color-secondary); white-space: nowrap; overflow: hidden; }
.float-progress .fp-spin { color: var(--eb-color-primary); font-size: 14px; animation: lp-rotate 1.1s linear infinite; }
@keyframes lp-rotate { to { transform: rotate(360deg); } }
.fp-stage { font-weight: 600; color: var(--eb-text-color-regular); overflow: hidden; text-overflow: ellipsis; }
.fp-num { font-variant-numeric: tabular-nums; color: var(--eb-color-primary); font-weight: 600; }
.fp-sep { color: var(--eb-text-color-placeholder); }
.fp-item b { font-variant-numeric: tabular-nums; color: var(--eb-text-color-regular); font-weight: 600; }
/* 分步时间轴（消息内）：左侧竖线 + 圆点，完成段显示耗时，进行中的段呼吸闪烁。 */
.run-timeline { margin: 10px 0 4px; padding: 8px 12px; border: 1px solid var(--eb-border-color-lighter); border-radius: 8px; background: var(--eb-fill-color-light); display: flex; flex-direction: column; gap: 4px; }
.tl-step { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--eb-text-color-secondary); }
.tl-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--eb-color-primary); flex: none; }
.tl-dot.live { background: var(--eb-text-color-placeholder); animation: tl-pulse 1.2s ease-in-out infinite; }
@keyframes tl-pulse { 0%, 100% { opacity: .35; } 50% { opacity: 1; } }
.tl-name { flex: 1; min-width: 0; }
.tl-ms { font-variant-numeric: tabular-nums; color: var(--eb-text-color-regular); }
/* 引用卡：文件卡片式。auto-fill 网格天然满足三态——单张占满一行、多张
     一行两个、容器 <~640px 回落单列，无需断点。 */
.cite-block { margin: 8px 0 4px; display: flex; flex-direction: column; gap: 6px; }
.cite-head { display: flex; align-items: center; gap: 5px; color: var(--eb-text-color-placeholder); }
.cite-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 8px; }
/* 单张引用：铺满一行，不留半行空格。 */
.cite-grid.single { grid-template-columns: 1fr; }
.cite-card { display: flex; gap: 10px; align-items: center; padding: 10px 12px; border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; background: var(--eb-fill-color-light); min-width: 0; }
.cite-glyph { flex: none; width: 34px; height: 34px; border-radius: 8px; display: flex; align-items: center; justify-content: center; background: var(--eb-color-primary-light, rgba(64, 158, 255, .12)); color: var(--eb-color-primary); font-size: 18px; }
.cite-main { flex: 1; min-width: 0; }
.cite-title { font-size: 13px; font-weight: 600; color: var(--eb-text-color-regular); display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.cite-card .quote { margin: 2px 0 0; font-size: 12px; line-height: 1.6; color: var(--eb-text-color-placeholder); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.warn { color: var(--eb-color-warning); margin-left: 6px; }
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

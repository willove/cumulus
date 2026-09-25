<template>
  <div class="workspace" :class="{ 'history-open': historyOpen }">
    <aside class="session-rail" aria-label="历史会话">
      <div class="rail-head"><span class="rail-title">历史会话</span><span class="tiny">{{ sessions.length }}</span></div>
      <div v-if="sessionsBusy" class="tiny rail-note" role="status">正在加载会话…</div>
      <div v-for="session in sessions" :key="session.id" class="session-item" :class="{ cur: session.id === current }">
        <eb-button class="session-open" text :type="session.id === current ? 'primary' : 'default'" @click="selectSession(session)">{{ session.title || '未命名会话' }}</eb-button>
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
        <div v-if="loading && meta" class="search-progress" role="status"><span class="status-dot ready"></span>{{ meta }}<span v-if="elapsed > 1" class="tiny"> · 已 {{ elapsed }} 秒</span></div>
        <div class="conversation-stream">
          <eb-chatbot v-model="messages" :loading="loading" height="100%" :show-tip="false"
                      :allow-attachments="false" :allow-drop="false" :show-avatar="false" :show-time="false"
                      assistant-name="Cumulus" placeholder="向当前知识库提问，Enter 发送，Shift + Enter 换行"
                      stoppable @send="onSend" @stop="stop" @regenerate="regenerate">
            <template #empty>
              <div class="welcome">
                <span class="welcome-eyebrow">基于 {{ documents.length }} 篇文档</span>
                <h2>你想从文档中了解什么？</h2>
                <p>回答会附上证据窗口，你可以打开原文核对。</p>
                <div class="sample-questions"><eb-button v-for="question in samples" :key="question" size="small" @click="onSend(question)">{{ question }}</eb-button></div>
              </div>
            </template>
          </eb-chatbot>
        </div>
        <eb-alert v-if="stats?.insufficient" type="warning" title="当前证据不足，建议补充文档或缩小问题范围。" :closable="false" show-icon />
        <details v-if="stats || sources.length" class="evidence-tray">
          <summary>查看本次回答的证据 <eb-tag size="small">{{ sources.length }} 个窗口</eb-tag><span v-if="stats" class="tiny">{{ tierLabel(stats.mode) }} · {{ (stats.latency / 1000).toFixed(1) }} 秒</span></summary>
          <div class="evidence-list">
            <div v-if="stats" class="form-actions">
              <eb-tag size="small">置信度 {{ Math.round(stats.conf * 100) }}%</eb-tag>
              <eb-tag size="small">覆盖率 {{ Math.round(stats.coverage * 100) }}%</eb-tag>
              <eb-tag size="small">{{ stats.reused ? '复用已有知识' : '本次检索' }}</eb-tag>
              <span class="tiny">{{ stats.loops }} 轮 · {{ stats.tokens }} tokens<span v-if="stats.stop_reason"> · 结束原因：{{ stats.stop_reason }}</span><span v-if="stats.cluster_id"> · 知识簇 {{ stats.cluster_id }}</span></span>
            </div>
            <p v-if="!sources.length" class="tiny">本次回答没有可回溯的证据窗口。</p>
            <article v-for="source in sources" :key="source.index" class="cite-card">
              <eb-tag class="idx" size="small">{{ source.index }}</eb-tag>
              <div><eb-button type="primary" link class="source-link" :disabled="!source.source" @click="previewSource = source.source">{{ source.title }}</eb-button><span v-if="source.resolved === false" class="tiny"> · 未定位</span><p class="quote">{{ source.snippet }}</p></div>
            </article>
          </div>
        </details>
      </template>
    </section>
    <SourcePreview v-if="previewSource" :source-id="previewSource" @close="previewSource = ''" />
  </div>
</template>

<script setup>
import { ref, onMounted } from "vue";
import { useChatPane } from "../panes/chat.js";
import { documents, documentsBusy, documentsError, hasBucket, libraryLabel, loadDocuments, pane } from "../state.js";
import SourcePreview from "./SourcePreview.vue";

defineEmits(["create-library"]);
const { sessions, sessionsBusy, current, loading, sources, meta, elapsed, stats, error, messages, onSend, stop, openSession, newSession, delSession } = useChatPane();
const historyOpen = ref(false);
const previewSource = ref("");
const samples = ["有哪些关键要求？", "有哪些例外情形？", "总结文档中的注意事项"];
function tierLabel(mode) { return mode === "DEEP" ? "深度检索" : mode === "FAST" ? "快速回答" : mode || "检索"; }
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
.session-open { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; justify-content: flex-start; }
.conversation { display: flex; flex-direction: column; min-width: 0; min-height: 0; }
.conversation-head { padding: 18px 24px; border-bottom: 1px solid var(--eb-border-color-light); display: flex; gap: 12px; align-items: center; justify-content: space-between; flex-wrap: wrap; }
.conversation-heading { display: flex; align-items: center; gap: 9px; font-size: 14px; min-width: 0; }
.conversation-heading strong { overflow-wrap: anywhere; }
.history-toggle { display: none; }
.conversation-stream { flex: 1; min-height: 0; overflow: hidden; }
.welcome { flex: 1; max-width: 600px; width: 100%; margin: auto; padding: 40px 24px; display: flex; flex-direction: column; align-items: center; justify-content: center; text-align: center; }
.welcome-eyebrow { color: var(--eb-color-primary); font-size: 12px; font-weight: 600; letter-spacing: .08em; }
.welcome h2 { margin: 16px 0 12px; font-size: clamp(22px, 2.5vw, 30px); line-height: 1.4; letter-spacing: -.7px; }
.welcome p { margin: 0 0 24px; max-width: 440px; color: var(--eb-text-color-secondary); font-size: 14px; line-height: 1.9; }
.sample-questions { display: flex; flex-wrap: wrap; justify-content: center; gap: 8px; }
.search-progress { padding: 10px 24px; font-size: 12px; color: var(--eb-text-color-secondary); display: flex; align-items: center; gap: 8px; }
.evidence-tray { flex: none; border-top: 1px solid var(--eb-border-color-light); padding: 12px 24px; background: var(--eb-fill-color-light); }
.evidence-tray summary { cursor: pointer; font-size: 13px; }
.evidence-list { max-height: 180px; overflow: auto; padding-top: 12px; }
.evidence-list .quote { margin: 6px 0 0; white-space: pre-wrap; overflow-wrap: anywhere; }
.conversation-error { margin: var(--eb-space-3) var(--eb-space-6) 0; width: auto; flex: none; }
@media (max-width: 1000px) {
  .workspace { grid-template-columns: minmax(0, 1fr); }
  .session-rail { display: none; }
  .history-toggle { display: inline-block; }
  .workspace.history-open { grid-template-rows: minmax(100px, 28%) minmax(0, 1fr); }
  .history-open .session-rail { display: block; border-bottom: 1px solid var(--eb-border-color-light); padding: 12px; }
  .conversation-head { padding: 14px 16px; }
  .evidence-tray { padding: 10px 16px; }
  .welcome { padding: 24px 20px; }
}
</style>

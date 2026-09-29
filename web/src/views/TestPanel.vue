<template>
  <div class="test-panel">
    <!-- 测试一问（Dify 式命中测试）：验证检索质量的最短路径——输入问题，
         直接看被召回的证据窗口、得分与来源，不合成答案、不另起会话。 -->
    <div class="test-bar">
      <eb-input v-model="question" :clearable="false" class="test-input" placeholder="输入一个测试问题，看它能召回什么证据"
                @keydown.enter="run" />
      <eb-button type="primary" :loading="busy" :disabled="!question.trim()" @click="run">测一下</eb-button>
    </div>
    <eb-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
    <div v-if="busy" class="tiny" role="status">检索中…（走完整检索管线，但不落会话）</div>

    <template v-if="result">
      <div class="test-summary">
        <eb-tag size="small">{{ result.mode || '检索' }}</eb-tag>
        <span class="tiny">置信度 {{ Math.round((result.answer?.conf ?? 0) * 100) }}% · 证据窗口 {{ refs.length }} 个 · {{ result.latency_ms }}ms<template v-if="result.model"> · {{ result.model }}</template><template v-if="result.reused"> · 命中已有簇</template></span>
      </div>

      <div v-if="!refs.length" class="empty-guide">
        <h3>没有召回任何证据</h3>
        <p class="sub">换个问法，或先在「文档」里确认语料覆盖这个主题。</p>
      </div>
      <div v-else class="evidence-list">
        <p class="tiny section-label">被召回的证据窗口（按引用顺序）</p>
        <article v-for="(r, i) in refs" :key="i" class="evidence-card">
          <div class="evidence-head">
            <b class="num">{{ i + 1 }}</b>
            <span class="evidence-src" :title="r.source_id">{{ r.title || r.source_id }}</span>
            <span class="tiny num">{{ r.start }}–{{ r.end }}</span>
            <span v-if="r.score != null" class="tiny num score">得分 {{ Number(r.score).toFixed(1) }}</span>
            <eb-tag v-if="r.resolved === false" type="warning" size="small">未定位</eb-tag>
          </div>
          <p class="evidence-quote">{{ r.quote }}</p>
        </article>
      </div>

      <!-- 答案收成一行可展开——测试的主语是证据，不是答案。 -->
      <details v-if="result.answer?.summary" class="answer-fold">
        <summary>这次检索给出的答案（折叠查看）</summary>
        <p class="answer-body">{{ result.answer.summary }}</p>
      </details>
    </template>
    <div v-else-if="!busy && !error" class="empty-guide">
      <h3>测一问，看召回</h3>
      <p class="sub">同一条检索管线：问题 → 证据窗口与得分。答案质量先看证据对不对。</p>
    </div>
  </div>
</template>

<script setup>
import { ref, computed } from "vue";
import { nsSel } from "../state.js";
import { requestJSON, jsonPost } from "../api.js";

const question = ref("");
const busy = ref(false);
const error = ref("");
const result = ref(null);

const refs = computed(() => {
  const raw = result.value?.citations?.refs || [];
  return raw.map((r, i) => ({
    index: r.index ?? i + 1, title: r.title, source_id: r.source_id,
    quote: r.quote, start: r.start, end: r.end, score: r.score, resolved: r.resolved,
  }));
});

async function run() {
  const q = question.value.trim();
  if (!q || busy.value) return;
  busy.value = true;
  error.value = "";
  result.value = null;
  const ns = nsSel.value;
  try {
    // 走一次性 JSON 检索（不传 session → 不落会话、不进历史）。Prior 关掉，
    // 测试要的是干净基线而不是会话语境的加成。ns 在请求体（query 参数不作数）。
    result.value = await requestJSON("/v1/search", jsonPost({ query: q, prior: false, ns }));
  } catch (e) {
    error.value = e.message;
  } finally { busy.value = false; }
}
</script>

<style src="./common.css"></style>
<style scoped>
.test-panel { display: flex; flex-direction: column; gap: var(--eb-space-4); padding-top: var(--eb-space-2); }
.test-bar { display: flex; gap: var(--eb-space-3); }
.test-input { flex: 1; min-width: 0; }
.test-summary { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
.evidence-list { display: flex; flex-direction: column; gap: var(--eb-space-2); }
.section-label { margin: 0; }
.evidence-card { border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; padding: 10px 12px; }
.evidence-head { display: flex; align-items: center; gap: var(--eb-space-3); }
.evidence-src { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; font-size: 13px; }
.score { color: var(--eb-color-primary); font-weight: 600; }
.evidence-quote { margin: 8px 0 0; font-size: 13px; line-height: 1.8; color: var(--eb-text-color-regular); }
.answer-fold { border: 1px dashed var(--eb-border-color-lighter); border-radius: 8px; padding: 8px 12px; font-size: 13px; }
.answer-fold summary { cursor: pointer; color: var(--eb-text-color-secondary); user-select: none; }
.answer-body { margin: 8px 0 0; line-height: 1.8; white-space: pre-wrap; }
</style>

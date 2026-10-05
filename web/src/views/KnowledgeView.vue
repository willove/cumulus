<template>
  <section class="pane knowledge-page" aria-label="知识">
    <!-- 页名顶栏已有：页内不重复页头，簇列表即刻起排。 -->
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先创建一个知识库</h3>
      <eb-button type="primary" @click="$emit('create-library')">新建知识库</eb-button>
    </div>
    <template v-else>
      <eb-alert v-if="overviewError" type="error" :title="overviewError" :closable="false" show-icon>
        <eb-button link type="primary" size="small" @click="loadOverview">重试</eb-button>
      </eb-alert>

      <!-- 库读数一行注脚：簇数 + 待复核 + 证据窗口。 -->
      <div class="ov-line">
        <b class="num ov-count">{{ stats.total }}</b><span>个答案簇</span>
        <template v-if="stats.emerging">
          <span class="ov-sep">·</span>
          <b class="num warn">{{ stats.emerging }}</b><span>待复核</span>
        </template>
        <span class="ov-sep">·</span>
        <span class="tiny">证据窗口 {{ evidenceCount == null ? "—" : evidenceCount }} 个被簇引用</span>
        <eb-button link size="small" @click="pane = 'corpus'">看文档</eb-button>
      </div>

      <!-- 主体：簇列表 / 详情 / 证据星图 / 复核。 -->
      <ClustersView />

      <!-- 词档关联：接口一直在（GET /v1/affinity），但从来没有界面。
           它解释的是「为什么这篇排在前头」——检索在词面命中之外，
           还用这份账本把同族文档提前（widen.go 的 affinityFirst）。 -->
      <section class="affinity" aria-labelledby="affinity-heading">
        <div class="affinity-head">
          <h3 id="affinity-heading">词档关联</h3>
        </div>
        <div class="affinity-bar">
          <eb-input v-model="token" :clearable="false" aria-label="要查询的词" placeholder="输入一个词，例如 连接池" @keydown.enter="lookup()" />
          <eb-button type="primary" :loading="busy" :disabled="!token.trim()" @click="lookup()">查关联</eb-button>
        </div>
        <eb-alert v-if="affError" type="warning" :title="affError" :closable="false" show-icon />
        <template v-else-if="searched">
          <div v-if="!docs.length" class="empty-guide">
            <h3>「{{ searched }}」没有关联记录</h3>
            <p class="tiny">提问里用到这个词后才会入账。</p>
          </div>
          <ul v-else class="aff-list">
            <li v-for="d in docs" :key="d.source_id" class="aff-row">
              <span class="aff-title" :title="displaySourceId(d.source_id)">{{ titleOf(d.source_id) }}</span>
              <span class="aff-bar"><i :style="{ width: barWidth(d.weight) }" /></span>
              <b class="num aff-weight">{{ fmtWeight(d.weight) }}</b>
            </li>
          </ul>
        </template>
      </section>

      <!-- 清空学得物删的是簇、证据窗口、引用边、账本与会话——都是这一页的东西。
           破坏性动作常驻页脚行（说明在左、动作在右），不再折进手风琴。 -->
      <eb-alert v-if="resetReport" type="success" :closable="false" :title="`已清空 ${resetReport.total} 项学得物`">{{ resetSummary }}</eb-alert>
      <eb-alert v-if="resetError" type="error" :title="resetError" :closable="false" show-icon />
      <div class="page-foot">
        <span class="tiny">删除答案簇、证据窗口、引用边、关联账本与会话，语料不动，下次提问重新学习。</span>
        <eb-button text type="danger" :loading="resetBusy" @click="resetDialog = true">清空学得物并重学</eb-button>
      </div>
    </template>

    <eb-dialog v-model="resetDialog" title="清空学得物" width="min(92%, 440px)" align-center>
      <p class="sub">将删除 {{ libraryLabel }} 的答案簇、证据窗口、账本与全部会话——<b>语料不动</b>，之后提问会重新学习。</p>
      <p class="tiny">输入库名 <b class="num">{{ nsSel }}</b> 确认：</p>
      <eb-input v-model="resetConfirmInput" :clearable="false" :placeholder="nsSel" @keydown.enter="confirmReset" />
      <template #footer>
        <eb-button @click="resetDialog = false">取消</eb-button>
        <eb-button type="danger" :loading="resetBusy" :disabled="resetConfirmInput.trim() !== nsSel" @click="confirmReset">清空并重学</eb-button>
      </template>
    </eb-dialog>
  </section>
</template>

<script setup>
import { ref, computed, onMounted, watch } from "vue";
import { useLibraryPane } from "../panes/library.js";
import { useClustersPane, clusterStats } from "../panes/clusters.js";
import { useAffinityPane } from "../panes/affinity.js";
import { displaySourceId } from "../panes/chat.js";
import { documents, loadDocuments, hasBucket, nsSel, pane } from "../state.js";
import ClustersView from "./ClustersView.vue";

defineEmits(["create-library"]);

const { learning, overviewError, resetBusy, resetReport, resetError, resetLearned,
  loadOverview, clearResetFeedback } = useLibraryPane();
const { clusters } = useClustersPane();
const { token, docs, busy, error: affError, searched, lookup } = useAffinityPane();

const stats = computed(() => clusterStats(clusters.value));
// /v1/affinity 只回 source_id 与 weight，而 id 形如 "src:部署手册#1" 不可读。
// 标题从已加载的语料列表里查，查不到就退回 id——不猜、也不留空。
const titleById = computed(() => {
  const map = new Map();
  for (const d of documents.value) if (d && d.id) map.set(d.id, d.title || d.id);
  return map;
});
function titleOf(id) { return titleById.value.get(id) || id; }
function fmtWeight(w) { return Number.isFinite(w) ? (Number.isInteger(w) ? String(w) : w.toFixed(3)) : "—"; }
const maxWeight = computed(() => docs.value.reduce((m, d) => Math.max(m, Number(d.weight) || 0), 0));
function barWidth(w) {
  const max = maxWeight.value;
  const v = Number(w) || 0;
  return (max > 0 ? Math.max(2, Math.round((v / max) * 100)) : 0) + "%";
}
const evidenceCount = computed(() => {
  const docs = learning.value?.docs || {};
  const hit = Object.keys(docs).find(key => key.endsWith(":clus_evidence") || key === "clus_evidence");
  return hit ? docs[hit] : null;
});
const resetSummary = computed(() => {
  const docs = resetReport.value?.docs || {};
  const pick = suffix => { const key = Object.keys(docs).find(k => k.endsWith(suffix)); return key ? docs[key] : 0; };
  return `簇 ${pick(":clus_clusters")} · 证据窗口 ${pick("clus_evidence")} · 引用边 ${pick(":clus_cites")} · 账本 ${pick(":clus_affinity")} · 会话与游标 ${resetReport.value?.kv_keys ?? 0}。语料未动。`;
});

const resetDialog = ref(false);
const resetConfirmInput = ref("");

async function confirmReset() {
  if (resetConfirmInput.value.trim() !== nsSel.value) return;
  const ok = await resetLearned(resetConfirmInput.value.trim());
  if (ok) { resetDialog.value = false; resetConfirmInput.value = ""; }
}
watch(resetDialog, open => { if (open) { resetConfirmInput.value = ""; clearResetFeedback(); } });

// 语料列表是为了把 affinity 的 source_id 翻成标题；概览是为了证据窗口计数。
onMounted(() => {
  loadDocuments();
  loadOverview();
});
</script>

<style src="./common.css"></style>
<style scoped>
.knowledge-page { display: flex; flex-direction: column; gap: var(--eb-space-4); }
.ov-line { display: flex; align-items: baseline; gap: var(--eb-space-2); flex-wrap: wrap; }
.ov-count { font-size: var(--eb-font-size-md); font-weight: var(--eb-font-weight-semibold); }
.ov-line .warn { color: var(--eb-color-warning); }
.ov-sep { color: var(--eb-text-color-placeholder); }
/* 词档关联：一行一个文档，权重画成条+数字两个通道（长度易比、数字可核）。 */
.affinity { display: flex; flex-direction: column; gap: var(--eb-space-3); border-top: 1px solid var(--eb-border-color-lighter); padding-top: var(--eb-space-4); }
.affinity-head h3 { margin: 0; font-size: var(--eb-font-size-md); }
.affinity-head .tiny { margin: 4px 0 0; color: var(--eb-text-color-secondary); line-height: 1.7; max-width: 720px; }
.affinity-bar { display: flex; gap: var(--eb-space-3); max-width: 560px; }
.aff-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 2px; max-width: 860px; }
.aff-row { display: flex; align-items: center; gap: var(--eb-space-3); padding: 4px 0; border-bottom: 1px solid var(--eb-border-color-lighter); }
.aff-title { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
.aff-bar { flex: none; width: 160px; height: 6px; border-radius: 3px; background: var(--eb-fill-color-light); overflow: hidden; }
.aff-bar i { display: block; height: 100%; background: var(--eb-color-primary); }
.aff-weight { flex: none; width: 56px; text-align: right; font-size: 12px; color: var(--eb-text-color-secondary); }
</style>

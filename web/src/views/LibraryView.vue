<template>
  <section class="pane library-page" aria-labelledby="library-heading">
    <header class="page-heading">
      <div><span class="eyebrow">知识库 / {{ libraryLabel }}</span><h2 id="library-heading">知识库</h2></div>
    </header>
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先创建一个知识库</h3>
      <p class="sub">每个知识库独立保存文档、学得的答案簇和会话，互不干扰。</p>
      <eb-button type="primary" @click="$emit('create-library')">新建知识库</eb-button>
    </div>
    <template v-else>
      <!-- 一个库的四段视图（segmented 胶囊）：概览 / 文档 / 测试一问 / 学得知识。 -->
      <eb-segmented v-model="tab" :options="segments" />
      <div :key="tab" class="tab-swap tab-body">
        <div v-if="tab === 'overview'" class="overview">
        <eb-alert v-if="overviewError" type="error" :title="overviewError" :closable="false" show-icon>
          <eb-button link type="primary" size="small" @click="refreshOverview">重试</eb-button>
        </eb-alert>
        <div v-if="overviewBusy && !learning" class="tiny" role="status">正在读取库状态…</div>
        <template v-else>
          <!-- 语料与学得物：一行主读数。整段唯一的视觉重心，其余全是它的注脚。 -->
          <div class="ov-hero">
            <b class="num">{{ documents.length }}</b>
            <span class="ov-hero-label">篇文档<template v-if="clusterStatsOf.total"> · {{ clusterStatsOf.total }} 个答案簇<template v-if="clusterStatsOf.emerging">（{{ clusterStatsOf.emerging }} 待复核）</template></template></span>
          </div>

          <!-- 这个库最近在发生什么：评测与会话两条，各一行。 -->
          <div class="ov-activity">
            <div class="ov-line">
              <span class="ov-line-label">最近评测</span>
              <template v-if="lastRun">
                <eb-status-tag :value="lastRun.state" :statuses="runStatuses" size="small" />
                <b>{{ lastRun.name || lastRun.id }}</b>
                <span class="tiny num">{{ fmtPct(lastRun.summary?.rule_match) }} 规则匹配 · {{ fmtPct(lastRun.summary?.evidence_hit) }} 证据命中 · {{ lastRun.done ?? 0 }}/{{ lastRun.total ?? 0 }} 题</span>
                <eb-button link type="primary" size="small" @click="pane = 'evals'">查看</eb-button>
              </template>
              <template v-else>
                <span class="tiny dim">还没跑过——用一组固定问题验证这个库的检索质量。</span>
                <eb-button link type="primary" size="small" @click="pane = 'evals'">去评测</eb-button>
              </template>
            </div>
            <div class="ov-line">
              <span class="ov-line-label">回答方式</span>
              <span class="tiny">同类问题命中簇时毫秒级返回、不再调模型；没命中才走完整检索。</span>
              <eb-button link size="small" @click="tab = 'learned'">看学得了什么</eb-button>
            </div>
          </div>

          <!-- 库管理：破坏性操作默认折叠——低频且重，不该常驻视野。 -->
          <details class="danger-fold">
            <summary>库管理（清空学得物 / 注销）</summary>
            <div class="danger-body">
              <div class="ov-actions">
                <eb-button :loading="resetBusy" @click="resetDialog = true">清空学得物并重学</eb-button>
                <eb-button type="danger" plain :loading="deleteBusy" @click="deleteDialog = true">注销这个知识库</eb-button>
              </div>
              <p class="tiny">清空学得物会删除答案簇、证据窗口、账本与会话，语料不动，下次提问重新学习。注销仅移除该库的注册（数据仍在存储中，可重新注册同名库找回）。</p>
              <eb-alert v-if="resetReport" type="success" :closable="false" :title="`已清空 ${resetReport.total} 项学得物`">
                {{ resetSummary }}
              </eb-alert>
              <eb-alert v-if="resetError" type="error" :title="resetError" :closable="false" show-icon />
              <eb-alert v-if="deleteError" type="error" :title="deleteError" :closable="false" show-icon />
            </div>
          </details>
        </template>
      </div>

      <DocumentsPanel v-else-if="tab === 'docs'" />
      <TestPanel v-else-if="tab === 'test'" />
      <ClustersView v-else />
      </div>
    </template>

    <!-- 重学确认：输入库名逐字确认（服务端同一门禁），防的就是手快。 -->
    <eb-dialog v-model="resetDialog" title="清空学得物" width="min(92%, 440px)" align-center>
      <p class="sub">将删除 {{ libraryLabel }} 的答案簇、证据窗口、账本与全部会话——<b>语料不动</b>，之后提问会重新学习。</p>
      <p class="tiny">输入库名 <b class="num">{{ nsSel }}</b> 确认：</p>
      <eb-input v-model="resetConfirmInput" :clearable="false" :placeholder="nsSel" @keydown.enter="confirmReset" />
      <template #footer>
        <eb-button @click="resetDialog = false">取消</eb-button>
        <eb-button type="danger" :loading="resetBusy" :disabled="resetConfirmInput.trim() !== nsSel" @click="confirmReset">清空并重学</eb-button>
      </template>
    </eb-dialog>

    <!-- 注销确认：不删数据，但选中的库会从选择器消失，值得一次明确确认。 -->
    <eb-dialog v-model="deleteDialog" title="注销知识库" width="min(92%, 440px)" align-center>
      <p class="sub">注销 {{ libraryLabel }}（{{ nsSel }}）后：顶部选择器不再列出该库，检索与导入不可用。</p>
      <p class="tiny">已导入的数据仍保留在存储中；重新注册同名库可恢复访问。输入库名确认：</p>
      <eb-input v-model="deleteConfirmInput" :clearable="false" :placeholder="nsSel" @keydown.enter="confirmDelete" />
      <template #footer>
        <eb-button @click="deleteDialog = false">取消</eb-button>
        <eb-button type="danger" :loading="deleteBusy" :disabled="deleteConfirmInput.trim() !== nsSel" @click="confirmDelete">注销</eb-button>
      </template>
    </eb-dialog>
  </section>
</template>

<script setup>
import { ref, computed, onMounted, watch } from "vue";
import { useLibraryPane } from "../panes/library.js";
import { useClustersPane, clusterStats } from "../panes/clusters.js";
import { documents, loadDocuments, hasBucket, libraryLabel, nsSel, pane } from "../state.js";
import DocumentsPanel from "./DocumentsPanel.vue";
import ClustersView from "./ClustersView.vue";
import TestPanel from "./TestPanel.vue";

defineEmits(["create-library"]);

const tab = ref("overview");
// 分段选项：待复核 > 0 时在「学得知识」标签上带计数（文本并入，segmented 无插槽）。
const segments = computed(() => [
  { label: "概览", value: "overview" },
  { label: "文档", value: "docs" },
  { label: "测试一问", value: "test" },
  { label: clusterStatsOf.value.emerging > 0 ? `学得知识 · ${clusterStatsOf.value.emerging}` : "学得知识", value: "learned" },
]);
const { learning, learningBusy, lastRun, overviewError, resetBusy, resetReport, resetError, resetLearned,
  deleteBusy, deleteError, removeBucket, loadOverview, clearResetFeedback } = useLibraryPane();
const { clusters } = useClustersPane();

const overviewBusy = computed(() => learningBusy.value);
const clusterStatsOf = computed(() => clusterStats(clusters.value));
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
const runStatuses = [
  { value: "queued", label: "排队中", type: "info" }, { value: "running", label: "评测中", type: "primary" },
  { value: "cancelling", label: "正在取消", type: "warning" }, { value: "cancelled", label: "已取消", type: "info" },
  { value: "completed", label: "已完成", type: "success" }, { value: "failed", label: "执行失败", type: "danger" },
  { value: "interrupted", label: "已中断", type: "warning" },
];

const resetDialog = ref(false);
const resetConfirmInput = ref("");
const deleteDialog = ref(false);
const deleteConfirmInput = ref("");

function refreshOverview() { loadOverview(); }
async function confirmReset() {
  if (resetConfirmInput.value.trim() !== nsSel.value) return;
  const ok = await resetLearned(resetConfirmInput.value.trim());
  if (ok) { resetDialog.value = false; resetConfirmInput.value = ""; }
}
async function confirmDelete() {
  if (deleteConfirmInput.value.trim() !== nsSel.value) return;
  const ok = await removeBucket();
  if (ok) { deleteDialog.value = false; deleteConfirmInput.value = ""; }
}
watch(resetDialog, open => { if (open) { resetConfirmInput.value = ""; clearResetFeedback(); } });
watch(deleteDialog, open => { if (open) deleteConfirmInput.value = ""; });

function fmtPct(value) { return value == null ? "—" : (Number(value) * 100).toFixed(0) + "%"; }

onMounted(() => {
  loadDocuments();
  loadOverview();
});
</script>

<style src="./common.css"></style>
<style scoped>
.library-page { display: flex; flex-direction: column; gap: var(--eb-space-4); }
.overview { display: flex; flex-direction: column; gap: var(--eb-space-5); padding-top: var(--eb-space-4); max-width: 860px; }
/* 主读数：这一段唯一的视觉重心。 */
.ov-hero { display: flex; align-items: baseline; gap: var(--eb-space-3); }
.ov-hero b { font-size: 34px; font-weight: 650; font-variant-numeric: tabular-nums; line-height: 1; }
.ov-hero-label { color: var(--eb-text-color-secondary); font-size: 14px; }
/* 库活动：标签列对齐的两行注脚。 */
.ov-activity { display: flex; flex-direction: column; border-top: 1px solid var(--eb-border-color-lighter); }
.ov-line { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; padding: var(--eb-space-3) 0; border-bottom: 1px solid var(--eb-border-color-lighter); }
.ov-line:last-child { border-bottom: 0; }
.ov-line-label { flex: none; width: 64px; font-size: 12px; color: var(--eb-text-color-placeholder); }
.ov-line .tiny { line-height: 1.7; }
.dim { color: var(--eb-text-color-placeholder); }
/* 破坏性操作：默认收起，低频且重。 */
.danger-fold { border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; padding: 8px 12px; }
.danger-fold summary { cursor: pointer; font-size: 13px; color: var(--eb-text-color-secondary); user-select: none; }
.danger-body { padding-top: var(--eb-space-3); display: flex; flex-direction: column; gap: var(--eb-space-3); }
.danger-body .tiny { color: var(--eb-text-color-placeholder); line-height: 1.8; margin: 0; }
.ov-actions { display: flex; flex-wrap: wrap; gap: var(--eb-space-3); }
</style>

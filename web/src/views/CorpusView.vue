<template>
  <section class="pane corpus-page" aria-labelledby="corpus-heading">
    <header class="page-heading">
      <div><span class="eyebrow">语料 / {{ libraryLabel }}</span><h2 id="corpus-heading">语料</h2>
        <p class="sub">这一页只做一件事：把文档变成可检索的语料。学过什么在「知识」，跑得怎么样在「评测」。</p></div>
    </header>
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先创建一个知识库</h3>
      <p class="sub">每个知识库独立保存文档、学得的答案簇和会话，互不干扰。</p>
      <eb-button type="primary" @click="$emit('create-library')">新建知识库</eb-button>
    </div>
    <template v-else>
      <eb-alert v-if="overviewError" type="error" :title="overviewError" :closable="false" show-icon>
        <eb-button link type="primary" size="small" @click="refreshOverview">重试</eb-button>
      </eb-alert>

      <!-- 主读数一行 + 最近评测一行，都是这一页的注脚；主体是下面的入库流水线。 -->
      <div v-if="overviewBusy && !learning" class="tiny" role="status">正在读取库状态…</div>
      <div v-else class="ov-strip">
        <div class="ov-hero">
          <b class="num">{{ documents.length }}</b>
          <span class="ov-hero-label">篇文档在库</span>
        </div>
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
      </div>

      <!-- 主操作：开始摄取。扫描→勾选→字段映射→上传/目录 都在这一个面板里。 -->
      <DocumentsPanel />

      <!-- 注销是库级破坏性操作，低频且重，默认收起。
           「清空学得物」不在这页——它删的是簇与证据，属于「知识」。 -->
      <details class="danger-fold">
        <summary>库管理（注销这个知识库）</summary>
        <div class="danger-body">
          <eb-button type="danger" plain :loading="deleteBusy" @click="deleteDialog = true">注销这个知识库</eb-button>
          <p class="tiny">注销仅移除该库的注册，语料与学得物都仍在存储中，重新注册同名库即可找回。</p>
          <eb-alert v-if="deleteError" type="error" :title="deleteError" :closable="false" show-icon />
        </div>
      </details>
    </template>

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
import { documents, loadDocuments, hasBucket, libraryLabel, nsSel, pane } from "../state.js";
import DocumentsPanel from "./DocumentsPanel.vue";

defineEmits(["create-library"]);

const { learning, learningBusy, lastRun, overviewError, deleteBusy, deleteError,
  removeBucket, loadOverview } = useLibraryPane();

const overviewBusy = computed(() => learningBusy.value);
const runStatuses = [
  { value: "queued", label: "排队中", type: "info" }, { value: "running", label: "评测中", type: "primary" },
  { value: "cancelling", label: "正在取消", type: "warning" }, { value: "cancelled", label: "已取消", type: "info" },
  { value: "completed", label: "已完成", type: "success" }, { value: "failed", label: "执行失败", type: "danger" },
  { value: "interrupted", label: "已中断", type: "warning" },
];

const deleteDialog = ref(false);
const deleteConfirmInput = ref("");

function refreshOverview() { loadOverview(); }
async function confirmDelete() {
  if (deleteConfirmInput.value.trim() !== nsSel.value) return;
  const ok = await removeBucket();
  if (ok) { deleteDialog.value = false; deleteConfirmInput.value = ""; }
}
watch(deleteDialog, open => { if (open) deleteConfirmInput.value = ""; });

function fmtPct(value) { return value == null ? "—" : (Number(value) * 100).toFixed(0) + "%"; }

onMounted(() => {
  loadDocuments();
  loadOverview();
});
</script>

<style src="./common.css"></style>
<style scoped>
.corpus-page { display: flex; flex-direction: column; gap: var(--eb-space-4); }
.ov-strip { display: flex; flex-direction: column; gap: var(--eb-space-3); max-width: 860px; }
.ov-hero { display: flex; align-items: baseline; gap: var(--eb-space-3); }
.ov-hero b { font-size: 34px; font-weight: 650; font-variant-numeric: tabular-nums; line-height: 1; }
.ov-hero-label { color: var(--eb-text-color-secondary); font-size: 14px; }
.ov-line { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; padding: var(--eb-space-3) 0; border-top: 1px solid var(--eb-border-color-lighter); }
.ov-line-label { flex: none; width: 64px; font-size: 12px; color: var(--eb-text-color-placeholder); }
.ov-line .tiny { line-height: 1.7; }
.dim { color: var(--eb-text-color-placeholder); }
.danger-fold { border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; padding: 8px 12px; }
.danger-fold summary { cursor: pointer; font-size: 13px; color: var(--eb-text-color-secondary); user-select: none; }
.danger-body { padding-top: var(--eb-space-3); display: flex; flex-direction: column; gap: var(--eb-space-3); align-items: flex-start; }
.danger-body .tiny { color: var(--eb-text-color-placeholder); line-height: 1.8; margin: 0; }
</style>

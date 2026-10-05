<template>
  <section class="pane corpus-page" aria-label="文档">
    <!-- 页名顶栏已有、库选择器也在顶栏：页内不再重复页头，主体（文档表）直接起排。
         路由 id 叫 corpus、导航已改「文档」——这页就是文档列表管理。 -->
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先创建一个知识库</h3>
      <eb-button type="primary" @click="$emit('create-library')">新建知识库</eb-button>
    </div>
    <template v-else>
      <eb-alert v-if="overviewError" type="error" :title="overviewError" :closable="false" show-icon>
        <eb-button link type="primary" size="small" @click="refreshOverview">重试</eb-button>
      </eb-alert>

      <!-- 库读数压成一行注脚：篇数 + 最近评测。大数字块曾把文档表推到首屏之外。 -->
      <div v-if="overviewBusy && !learning" class="tiny" role="status">正在读取库状态…</div>
      <div v-else class="ov-line">
        <b class="num ov-count">{{ documents.length }}</b><span>篇在库</span>
        <span class="ov-sep">·</span>
        <span class="ov-label">最近评测</span>
        <template v-if="lastRun">
          <eb-status-tag :value="lastRun.state" :statuses="runStatuses" size="small" />
          <b>{{ lastRun.name || lastRun.id }}</b>
          <span class="tiny num">{{ fmtPct(lastRun.summary?.rule_match) }} 规则匹配 · {{ fmtPct(lastRun.summary?.evidence_hit) }} 证据命中 · {{ lastRun.done ?? 0 }}/{{ lastRun.total ?? 0 }} 题</span>
          <eb-button link type="primary" size="small" @click="pane = 'evals'">查看</eb-button>
        </template>
        <template v-else>
          <span class="tiny dim">还没跑过评测</span>
          <eb-button link type="primary" size="small" @click="pane = 'evals'">去评测</eb-button>
        </template>
      </div>

      <!-- 主操作：开始摄取。扫描→勾选→字段映射→上传/目录 都在这一个面板里。 -->
      <DocumentsPanel />

      <!-- 注销是库级破坏性操作，低频且重：页脚行常驻（说明在左、动作在右），
           不再折进手风琴。「清空学得物」不在这页——它删的是簇与证据，属于「知识」。 -->
      <eb-alert v-if="deleteError" type="error" :title="deleteError" :closable="false" show-icon />
      <div class="page-foot">
        <span class="tiny">注销仅移除该库的注册，语料与学得物仍在存储中，重新注册同名库即可找回。</span>
        <eb-button text type="danger" :loading="deleteBusy" @click="deleteDialog = true">注销这个知识库</eb-button>
      </div>
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
import { documents, loadDocuments, hasBucket, nsSel, pane } from "../state.js";
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
.ov-line { display: flex; align-items: baseline; gap: var(--eb-space-2); flex-wrap: wrap; }
.ov-count { font-size: var(--eb-font-size-md); font-weight: var(--eb-font-weight-semibold); }
.ov-sep { color: var(--eb-text-color-placeholder); }
.ov-label { font-size: var(--eb-font-size-xs); color: var(--eb-text-color-placeholder); }
.dim { color: var(--eb-text-color-placeholder); }
</style>

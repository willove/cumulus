<template>
  <section class="pane eval-workbench" aria-labelledby="eval-heading">
    <header class="page-heading">
      <div><span class="eyebrow">知识库 / {{ libraryLabel }}<span v-if="nsSel"> · {{ nsSel }}</span></span><h2 id="eval-heading">评测</h2><p class="sub">用固定题集检验当前库的检索与回答质量：跑一次、看逐题、比上次。</p></div>
      <div class="form-actions">
        <eb-button :disabled="!hasBucket || busy.runs" @click="refresh">刷新</eb-button>
      </div>
    </header>
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先选择一个知识库</h3><p class="sub">在顶部选择或创建知识库并导入文档后，再回来跑评测。</p>
    </div>
    <LegacyEvalsView v-else-if="section === 'legacy'" :key="nsSel" />
    <template v-else>
      <eb-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
      <eb-alert v-if="formError" type="error" :title="formError" :closable="false" show-icon />
      <eb-alert v-if="pollError" type="warning" title="自动刷新已暂停" :closable="false" show-icon>{{ pollError }}<eb-button link type="primary" @click="refresh">恢复刷新</eb-button></eb-alert>

      <!-- 运行一行：题集 → 模式 → 跑。没有向导，没有步骤。 -->
      <div class="run-bar">
        <eb-select ref="datasetPick" :model-value="datasetID" class="dataset-pick" aria-label="题集" placeholder="选择题集" :disabled="busy.dataset" @update:model-value="chooseDataset">
          <eb-option v-for="dataset in datasets" :key="dataset.id" :value="dataset.id" :label="`${dataset.name} · ${dataset.count} 题`" />
        </eb-select>
        <eb-segmented v-model="mode" :options="modeOptions" size="small" />
        <eb-button type="primary" :disabled="!datasetPreview || busy.submit" :loading="busy.submit" @click="startRunClick">运行</eb-button>
        <eb-button link type="primary" @click="uploadOpen = !uploadOpen">{{ uploadOpen ? '收起上传' : '上传题集' }}</eb-button>
      </div>

      <!-- 上传/校验折叠面板：JSONL 上传或粘贴 → 校验 → 存为题集。 -->
      <div v-if="uploadOpen" class="upload-fold">
        <eb-form label-position="top" @submit.prevent>
          <div class="upload-grid">
            <eb-form-item label="上传 JSONL">
              <eb-upload :key="nsSel" v-model:file-list="uploadFiles" :auto-upload="false" :limit="1" accept=".jsonl,.ndjson,application/x-ndjson" :disabled="busy.file" @change="readDataset" @remove="clearUpload">
                <template #trigger><span>选择 JSONL 文件</span></template>
                <p class="tiny">每行一题：id / query / answer / gold_sources</p>
              </eb-upload>
            </eb-form-item>
            <eb-form-item label="或直接粘贴 JSONL 内容">
              <eb-input v-model="datasetContent" type="textarea" :rows="6" :clearable="false" placeholder='{"id":"q1","query":"…","answer":"…","gold_sources":["…"]}' @update:model-value="clearSavedSelection" />
            </eb-form-item>
            <eb-form-item label="题集名称"><eb-input v-model="datasetName" :clearable="false" placeholder="例如：回归题集" @update:model-value="clearSavedSelection" /></eb-form-item>
          </div>
          <div class="form-actions">
            <eb-button :disabled="!datasetContent.trim() || busy.file" :loading="busy.validate" @click="validateDataset">校验</eb-button>
            <eb-button type="primary" :disabled="!validation?.valid || busy.file" :loading="busy.save" @click="saveDataset">存为题集</eb-button>
            <eb-button link @click="downloadTemplate">下载模板</eb-button>
          </div>
        </eb-form>
        <section v-if="validation" aria-label="题集校验结果" class="form-stack">
          <eb-alert :type="validation.valid ? 'success' : 'error'" :title="validation.valid ? `校验通过 · ${validation.items?.length || 0} 题` : '校验未通过，请修正以下问题'" :closable="false" />
          <ul v-if="validation.errors?.length" class="tiny validate-list"><li v-for="(issue, index) in validation.errors" :key="index">第 {{ issue.line || '?' }} 行：{{ issue.message }}</li></ul>
          <!-- 段落式参考答案：能存，但规则臂必然恒 0。向导退役时这条提醒跟着没了，
               而 /v1/eval/datasets/validate 一直在返回 warnings（eval/v2.go:141）——接口有、脸上没有。 -->
          <ul v-if="validation.warnings?.length" class="tiny validate-list"><li v-for="(issue, index) in validation.warnings" :key="index">警告 · 第 {{ issue.line || '?' }} 行：{{ issue.message }}</li></ul>
        </section>
      </div>

      <!-- 空态：没跑过评测时先回答“这是干嘛的”。题集已选或上传面板已展开时不再出现——
           否则它会和刚校验通过的题集同屏，一边显示「校验通过 · 2 题」一边让你上传第一个题集。 -->
      <div v-if="!runs.length && !busy.runs && !run && !datasetID && !uploadOpen" class="empty-guide eval-empty">
        <h3>用题集检验回答质量</h3>
        <p class="sub">题集是固定的一组「问题 + 参考答案 + 证据位置」（JSONL）。运行后系统在当前库上作答并对照评分，给出正确率、证据命中率等指标。默认离线模式只验证流程，不调用真实模型。</p>
        <div class="form-actions"><eb-button type="primary" @click="uploadOpen = true">上传第一个题集</eb-button></div>
      </div>
      <template v-else>
        <div v-if="busy.runs" role="status" class="tiny">正在读取运行…</div>
        <div v-else-if="runs.length" class="table-scroll">
          <eb-table :data="runs" row-key="id" :scroll-x="720" aria-label="评测运行列表" @row-click="(row) => selectRun(row.id)">
            <eb-table-column prop="name" label="运行" min-width="220"><template #default="{ row }">{{ row.name || row.id }}</template></eb-table-column>
            <eb-table-column prop="state" label="状态" width="110"><template #default="{ row }"><eb-status-tag :value="row.state" :statuses="runStatuses" size="small" /></template></eb-table-column>
            <eb-table-column label="规则匹配" width="110"><template #default="{ row }"><b class="num">{{ pct(row.summary?.rule_match) }}</b></template></eb-table-column>
            <eb-table-column label="证据命中" width="110"><template #default="{ row }"><b class="num">{{ pct(row.summary?.evidence_hit) }}</b></template></eb-table-column>
            <eb-table-column label="进度" width="100"><template #default="{ row }"><span class="num">{{ row.done ?? 0 }}/{{ row.total ?? 0 }}</span></template></eb-table-column>
            <eb-table-column prop="created_at" label="时间" min-width="160"><template #default="{ row }"><span class="num">{{ fmtWhen(row.created_at) }}</span></template></eb-table-column>
          </eb-table>
        </div>
        <p v-if="busy.detail && !run" role="status" class="tiny">正在读取运行详情…</p>
        <EvalRunDetail v-if="run" :key="run.id" :run="run" :items="filteredItems" :runs="runs" :progress="progress" :active="isActive(run)" :busy="busy" :item-filter="itemFilter" :live-confirmed="retryConfirmed" :compare-id="compareID" :comparison="comparison" :compare-error="compareError" @update:item-filter="itemFilter = $event" @update:live-confirmed="retryConfirmed = $event" @update:compare-id="compareID = $event" @action="runAction" @export="exportRun" @compare="compareRuns" @select-item="selectedItem = $event" @new-run="backToRunBar" />
        <EvalItemDetail v-if="selectedItem" :item="selectedItem" @close="selectedItem = null" />
      </template>

      <div class="legacy-entry"><eb-button link @click="section = 'legacy'">旧版历史报告</eb-button></div>
    </template>

    <!-- 真实模式确认：计费是事实，不是吓唬。 -->
    <eb-dialog v-model="liveConfirmOpen" title="使用真实模型运行" width="min(92%, 440px)" align-center>
      <p class="sub">真实模式会调用当前启用的模型端点：检索、判官、闭卷基线都按供应商价格计费。离线模式只验证流程，不花钱。</p>
      <template #footer>
        <eb-button @click="liveConfirmOpen = false">回到离线</eb-button>
        <eb-button type="primary" @click="confirmLive">确认用真实模型跑</eb-button>
      </template>
    </eb-dialog>
  </section>
</template>

<script setup>
import { computed, ref, watch } from 'vue';
import { useEvaluationWorkbench } from '../panes/evals.js';
import { nsSel, hasBucket, libraryLabel } from '../state.js';
import EvalRunDetail from './EvalRunDetail.vue';
import EvalItemDetail from './EvalItemDetail.vue';
import LegacyEvalsView from './LegacyEvalsView.vue';
const { capabilities, datasets, runs, run, selectedItem, filteredItems, itemFilter, progress, isActive,
  error, formError, pollError, busy, datasetName, datasetContent, validation, datasetID,
  datasetPreview, config, liveConfirmed, retryConfirmed, canStart, compareID, comparison, compareError,
  refresh, openRun, readDataset, validateDataset, saveDataset, chooseDataset, startRun,
  runAction, compareRuns, exportRun } = useEvaluationWorkbench();
const section = ref('workbench');
const uploadOpen = ref(false);
const liveConfirmOpen = ref(false);
const mode = ref('offline');
const modeOptions = [
  { label: '离线', value: 'offline' },
  { label: '真实模型', value: 'live' },
];
const runStatuses = [
  { value: 'queued', label: '排队中', type: 'info' }, { value: 'running', label: '评测中', type: 'primary' },
  { value: 'cancelling', label: '正在取消', type: 'warning' }, { value: 'cancelled', label: '已取消', type: 'info' },
  { value: 'completed', label: '已完成', type: 'success' }, { value: 'failed', label: '失败', type: 'danger' },
  { value: 'interrupted', label: '已中断', type: 'warning' },
];
const uploadFiles = ref([]);
// 线性流只露模式这一个开关，其余沿用工作台默认值；高级项 CLI 仍在。
watch(mode, value => {
  config.value.mode = value;
  config.value.judge = value === 'live';
  config.value.closed_book = false;
});
function startRunClick() {
  if (!datasetPreview.value) return;
  if (mode.value === 'live' && !capabilities.value?.live_available) {
    // 没有可用模型时不能把人支使回「请选择题集」——题集就在旁边且已经选好了。
    // 这句原本是向导第二步里的，随向导一起失效过一段时间。
    formError.value = '服务尚未配置可用的真实模型：切回离线，或到「引擎 → 模型与配置」启用一个';
    return;
  }
  if (mode.value === 'live') { liveConfirmOpen.value = true; return; }
  void startRun();
}
// 「新建冷启动评测」没有对应的模式：evalexecutor.go:40 的冻结签名恒定含
// cold-start=true，每次运行本来就是冷的。所以这个按钮做的是「回到运行条」，
// 而不是打开一个不存在的第二套流程。
const datasetPick = ref(null);
function backToRunBar() {
  const el = datasetPick.value?.$el?.querySelector('[role="combobox"]');
  if (!el) return;
  el.scrollIntoView({ block: 'center' });
  el.focus();
}
function confirmLive() {
  liveConfirmOpen.value = false;
  liveConfirmed.value = true; // canStart 在 live 模式查这个标志；对话框确认就是它的唯一写入点
  void startRun();
}
function selectRun(id) { liveConfirmed.value = false; openRun(id); }
function clearSavedSelection() { if (datasetID.value) chooseDataset(''); }
function clearUpload() { datasetName.value = ''; datasetContent.value = ''; chooseDataset(''); }
watch(capabilities, value => { if (!value?.l1_available) config.value.l1pre = false; });
watch(nsSel, () => { uploadFiles.value = []; });
function pct(value) { return value == null ? '—' : (Number(value) * 100).toFixed(0) + '%'; }
function fmtWhen(value) { return value ? new Date(value).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '—'; }
function downloadTemplate() {
  const sample = { id: 'example-1', query: '请替换为业务问题', answer: '请替换为参考答案', gold_sources: [] };
  const url = URL.createObjectURL(new Blob([JSON.stringify(sample) + '\n'], { type: 'application/x-ndjson;charset=utf-8' }));
  const link = document.createElement('a');
  link.href = url; link.download = 'evaluation-template.jsonl';
  document.body.appendChild(link); link.click(); link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
</script>

<style src="./common.css"></style>
<style scoped>
.eval-workbench { display: flex; flex-direction: column; gap: var(--eb-space-5); }
.eval-workbench > * { flex-shrink: 0; min-width: 0; }
.run-bar { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
.dataset-pick { width: min(100%, 300px); }
.upload-fold { border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; padding: var(--eb-space-4); display: flex; flex-direction: column; gap: var(--eb-space-4); }
.upload-grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1.4fr); gap: 0 var(--eb-space-5); }
.upload-grid > :last-child { grid-column: 1 / -1; }
.validate-list { margin: 0; padding-left: 18px; line-height: 1.8; }
.eval-empty { max-width: 640px; margin: var(--eb-space-10) auto var(--eb-space-4); }
.eval-empty .form-actions { justify-content: center; margin-top: var(--eb-space-5); }
.legacy-entry { display: flex; justify-content: flex-end; }
@media (max-width: 760px) { .upload-grid { grid-template-columns: minmax(0, 1fr); } }
</style>

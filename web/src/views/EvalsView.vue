<template>
  <section class="pane eval-workbench" aria-labelledby="eval-heading">
    <header class="page-heading">
      <div><span class="eyebrow">知识库 / {{ libraryLabel }}<span v-if="nsSel"> · {{ nsSel }}</span></span><h2 id="eval-heading">评测工作台</h2><p class="sub">固定题集与语料快照，验证检索、回答与引用。</p></div>
      <div class="form-actions">
        <eb-button :disabled="!hasBucket || busy.runs" @click="refresh">刷新评测</eb-button>
        <eb-button type="primary" :disabled="!hasBucket" @click="beginRun">新建评测</eb-button>
      </div>
    </header>
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先选择一个知识库</h3><p class="sub">在顶部选择或创建知识库，导入业务文档后，即可在这里上传题集、校验并运行评测。</p>
    </div>
    <template v-else>
      <nav class="form-actions" aria-label="评测版本">
        <eb-button :type="section === 'workbench' ? 'primary' : 'default'" :aria-pressed="section === 'workbench'" @click="section = 'workbench'">评测工作台</eb-button>
        <eb-button :type="section === 'legacy' ? 'primary' : 'default'" :aria-pressed="section === 'legacy'" @click="section = 'legacy'">旧版历史报告</eb-button>
      </nav>
      <LegacyEvalsView v-if="section === 'legacy'" :key="nsSel" />
      <template v-else>
        <eb-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
        <eb-alert v-if="pollError" type="warning" title="自动刷新已暂停" :closable="false" show-icon>{{ pollError }}<eb-button link type="primary" @click="refresh">恢复刷新</eb-button></eb-alert>
        <eb-section-card title="运行环境与协议">
          <div v-if="capabilities" class="form-stack">
            <div class="form-actions"><eb-tag :type="capabilities.live_available ? 'success' : 'info'">真实模型：{{ capabilities.live_available ? '可用' : '未配置 / 不可用' }}</eb-tag><eb-tag>L1：{{ capabilities.l1_available ? '可用' : '不可用' }}</eb-tag><span class="tiny">题集上限 {{ capabilities.max_items ?? 'N/A' }} 题 · {{ capabilities.max_bytes ?? 'N/A' }} bytes</span></div>
            <p class="tiny">服务模型：{{ capabilities.model || 'N/A' }} · 并发上限：{{ capabilities.concurrency ?? 'N/A' }} · 排队上限：{{ capabilities.queue_limit ?? 'N/A' }} · 协议：{{ capabilities.protocol || 'N/A' }}</p>
            <p class="sub">默认离线：仅验证功能流程，不代表真实模型质量。每次运行冷启动；运行内按固定题序积累状态，业务语料隔离，不写回生产学习状态。</p>
            <eb-button link @click="showCapabilities = !showCapabilities">{{ showCapabilities ? '收起' : '查看' }}服务能力、模型与限制</eb-button>
            <pre v-if="showCapabilities" class="pre-block">{{ json(capabilities) }}</pre>
          </div>
          <p v-else class="sub" role="status">{{ busy.capabilities ? '正在读取服务能力…' : '服务能力尚未就绪，请刷新后重试。' }}</p>
        </eb-section-card>

        <eb-section-card v-if="createOpen" role="region" aria-labelledby="eval-create-heading">
          <template #header><h3 id="eval-create-heading">新建评测 · {{ libraryLabel }}</h3></template>
          <template #extra><eb-button :disabled="busy.submit" @click="createOpen = false">收起创建</eb-button></template>
          <div class="wizard form-stack">
            <eb-steps :active="step" simple><eb-step title="1 题集" /><eb-step title="2 配置" /><eb-step title="3 确认" /></eb-steps>
            <section v-if="step === 0" aria-labelledby="eval-dataset-heading" class="form-stack">
              <h3 id="eval-dataset-heading">选择或上传题集</h3>
              <eb-form label-position="top">
                <eb-form-item label="已保存的不可变题集版本">
                  <eb-select :model-value="datasetID" aria-label="已保存题集" clearable :disabled="busy.dataset || busy.save" @update:model-value="chooseDataset">
                    <eb-option v-for="dataset in datasets" :key="dataset.id" :value="dataset.id" :label="`${dataset.name} · ${dataset.count} 题 · ${dataset.sha?.slice(0, 12) || dataset.id}`" />
                  </eb-select>
                </eb-form-item>
              </eb-form>
              <div v-if="datasetPreview" class="form-stack">
                <p>已选：<strong>{{ datasetPreview.name }}</strong> · {{ datasetPreview.count }} 题</p>
                <p class="tiny fingerprint">SHA：{{ datasetPreview.sha }}</p>
              </div>
              <eb-form label-position="top" :disabled="busy.file || busy.save">
                <eb-form-item label="上传 JSONL（仅本地读取，校验后保存）">
                  <eb-upload :key="nsSel" v-model:file-list="uploadFiles" :auto-upload="false" :limit="1" accept=".jsonl,.ndjson,application/x-ndjson" :disabled="busy.file || busy.save" @change="readDataset" @remove="clearUpload" @exceed="formError = '每次只能选择一个题集，请先移除已选文件。'">
                    <template #trigger><span>选择 JSONL 文件</span></template>
                  </eb-upload>
                </eb-form-item>
                <div class="form-actions"><eb-button link type="primary" @click="downloadTemplate">下载 JSONL 模板</eb-button><eb-button link @click="editJSONL = !editJSONL">{{ editJSONL ? '收起' : '展开' }} JSONL 编辑</eb-button></div>
                <p class="sub">每行一题，包含 id、query、answer、gold_sources。模板不含可直接使用的证据键；请将 gold_sources 填为当前库真实业务键或有效文档 ID，再校验。</p>
                <eb-form-item label="新题集名称（可编辑）"><eb-input v-model="datasetName" aria-label="新题集名称" :clearable="false" placeholder="例如：客服回归题集" @update:model-value="clearSavedSelection" /></eb-form-item>
                <eb-form-item v-if="editJSONL" label="JSONL 内容"><eb-input v-model="datasetContent" aria-label="JSONL 内容" type="textarea" :rows="8" :clearable="false" placeholder="每行一个 JSON 对象" @update:model-value="clearSavedSelection" /></eb-form-item>
                <div class="form-actions"><eb-button :disabled="!datasetContent.trim() || busy.file" :loading="busy.validate" @click="validateDataset">校验题集</eb-button><eb-button type="primary" :disabled="!validation?.valid || busy.file" :loading="busy.save" @click="saveDataset">保存不可变版本并配置</eb-button></div>
              </eb-form>
              <section v-if="validation" aria-label="题集校验结果" class="form-stack">
                <eb-alert :type="validation.valid ? 'success' : 'error'" :title="validation.valid ? `校验通过 · ${validation.items?.length || 0} 题` : '校验未通过，请修正以下问题'" :closable="false" />
                <ul v-if="validation.errors?.length"><li v-for="(issue, index) in validation.errors" :key="index">错误 · {{ issue.line ? `第 ${issue.line} 行` : '题集整体' }}：{{ issue.message }}</li></ul>
                <ul v-if="validation.warnings?.length"><li v-for="(issue, index) in validation.warnings" :key="index">警告 · {{ issue.line ? `第 ${issue.line} 行` : '题集整体' }}：{{ issue.message }}</li></ul>
              </section>
              <section v-if="previewItems.length" aria-label="题集前五题预览">
                <h3>前 {{ previewItems.length }} 题预览</h3>
                <div class="table-scroll"><eb-table :data="previewItems" row-key="id" :scroll-x="650"><eb-table-column prop="id" label="ID" width="110" /><eb-table-column prop="query" label="问题" min-width="200" /><eb-table-column prop="answer" label="参考答案" min-width="200" /><eb-table-column prop="gold_sources" label="证据业务键" :formatter="row => (row.gold_sources || []).join('、')" min-width="140" /></eb-table></div>
              </section>
            </section>
            <section v-else-if="step === 1" aria-labelledby="eval-config-heading">
              <h3 id="eval-config-heading">配置运行</h3>
              <eb-form :model="config" label-position="top">
                <eb-form-item label="运行名称（可选）"><eb-input v-model="runName" aria-label="运行名称" :clearable="false" :placeholder="datasetPreview?.name" /></eb-form-item>
                <eb-form-item label="运行模式" prop="mode"><eb-select v-model="config.mode" aria-label="运行模式"><eb-option value="offline" label="离线 · 功能验证（默认）" /><eb-option value="live" label="真实模型 · 产生调用费用" :disabled="!capabilities?.live_available" /></eb-select></eb-form-item>
                <p class="sub">离线结果不能用于模型质量结论。真实模型的检索、判官和闭卷基线均可能计费，具体价格由服务端模型供应商决定。</p>
                <div class="config-checks">
                  <eb-checkbox v-model="config.judge" :disabled="config.mode !== 'live'" tabindex="0" @keydown.space.prevent="config.mode === 'live' && (config.judge = !config.judge)">启用判官（独立于规则匹配）</eb-checkbox>
                  <eb-checkbox v-model="config.closed_book" :disabled="config.mode !== 'live'" tabindex="0" @keydown.space.prevent="config.mode === 'live' && (config.closed_book = !config.closed_book)">运行闭卷基线</eb-checkbox>
                  <eb-checkbox v-model="config.prior" tabindex="0" @keydown.space.prevent="config.prior = !config.prior">启用 prior</eb-checkbox>
                  <eb-checkbox v-model="config.l1pre" :disabled="!capabilities?.l1_available" tabindex="0" @keydown.space.prevent="capabilities?.l1_available && (config.l1pre = !config.l1pre)">启用 L1 预筛</eb-checkbox>
                </div>
                <div class="config-grid">
                  <eb-form-item label="单题时限（秒，1–600）" prop="item_timeout_seconds"><eb-input v-model="config.item_timeout_seconds" type="number" aria-label="单题时限" min="1" max="600" step="1" :clearable="false" /></eb-form-item>
                  <eb-form-item label="运行时限（秒，1–7200）" prop="timeout_seconds"><eb-input v-model="config.timeout_seconds" type="number" aria-label="运行时限" min="1" max="7200" step="1" :clearable="false" /></eb-form-item>
                  <eb-form-item label="Token 总预算（1–10000000）" prop="token_budget"><eb-input v-model="config.token_budget" type="number" aria-label="Token 总预算" min="1" max="10000000" step="1" :clearable="false" /></eb-form-item>
                  <eb-form-item :label="`题数上限（0 = 全部，最多 ${capabilities?.max_items ?? 500}）`" prop="limit"><eb-input v-model="config.limit" type="number" aria-label="题数上限" min="0" :max="capabilities?.max_items || 500" step="1" :clearable="false" /></eb-form-item>
                </div>
                <eb-alert type="info" title="预算覆盖检索 + 判官 + 闭卷基线" :closable="false">预算是停止条件，不是费用硬上限；正在进行的调用仍可能超出预算。</eb-alert>
              </eb-form>
            </section>
            <section v-else aria-labelledby="eval-confirm-heading" class="form-stack">
              <h3 id="eval-confirm-heading">确认评测</h3>
              <p>知识库：<strong>{{ libraryLabel }}（{{ nsSel }}）</strong></p>
              <p>题集：<strong>{{ datasetPreview?.name }}</strong> · {{ datasetPreview?.count }} 题 · 本次最多 {{ config.limit || datasetPreview?.count }} 题</p>
              <p class="tiny fingerprint">题集 SHA：{{ datasetPreview?.sha }}</p>
              <p>模式：{{ config.mode === 'live' ? '真实模型（按供应商实际调用计费）' : '离线（功能验证，不调用真实模型）' }}；判官 {{ config.judge ? '开启' : '关闭' }}；闭卷基线 {{ config.closed_book ? '开启' : '关闭' }}。</p>
              <p class="sub">冷启动、固定题序、运行内积累；冻结当前库语料，隔离业务状态。Token 预算 {{ config.token_budget }} 覆盖检索、判官及闭卷调用，进行中的调用可能超出。界面不估算未知模型单价。</p>
              <pre class="pre-block">{{ json(config) }}</pre>
              <eb-checkbox v-if="config.mode === 'live'" v-model="liveConfirmed" tabindex="0" @keydown.space.prevent="liveConfirmed = !liveConfirmed">我确认对 {{ libraryLabel }} / {{ datasetPreview?.name }} 调用真实模型，并承担检索、判官、闭卷基线的实际费用</eb-checkbox>
            </section>
            <eb-alert v-if="formError" type="error" :title="formError" :closable="false" show-icon />
            <div class="form-actions">
              <eb-button v-if="step > 0" :disabled="busy.submit" @click="step--; liveConfirmed = false">上一步</eb-button>
              <eb-button v-if="step < 2" type="primary" :disabled="busy.dataset || busy.save || busy.file" @click="nextStep">{{ step === 0 ? '下一步：配置' : '下一步：确认' }}</eb-button>
              <eb-button v-else type="primary" :loading="busy.submit" :disabled="!canStart" @click="startRun">确认并开始评测</eb-button>
            </div>
          </div>
        </eb-section-card>

        <eb-section-card role="region" aria-labelledby="eval-runs-heading">
          <template #header><h3 id="eval-runs-heading">评测运行 · {{ runs.length }}</h3></template>
          <div v-if="busy.runs" role="status" class="tiny">正在读取评测运行…</div>
          <div v-if="!runs.length && !busy.runs" class="empty-guide"><h3>当前库还没有评测运行</h3><p class="sub">上传 JSONL 或选择已保存题集，从默认离线验证开始。</p><eb-button type="primary" @click="beginRun">创建第一次评测</eb-button></div>
          <div v-else class="table-scroll"><eb-table :data="runs" row-key="id" :scroll-x="760" aria-label="评测运行列表">
            <eb-table-column prop="name" label="运行名称" min-width="220"><template #default="{ row }"><eb-button link type="primary" :aria-label="`查看评测 ${row.name || row.id}`" @click="selectRun(row.id)">{{ row.name || row.id }}</eb-button></template></eb-table-column>
            <eb-table-column prop="state" label="状态" width="120"><template #default="{ row }"><eb-status-tag :value="row.state" :statuses="runStatuses" /></template></eb-table-column>
            <eb-table-column label="进度" width="110" :formatter="row => `${row.done ?? 0}/${row.total ?? 0}`" />
            <eb-table-column label="模式" width="110" :formatter="row => row.config?.mode || 'N/A'" />
            <eb-table-column prop="created_at" label="创建时间" min-width="200" :formatter="row => date(row.created_at)" />
          </eb-table></div>
        </eb-section-card>
        <p v-if="busy.detail && !run" role="status">正在读取运行详情…</p>
        <EvalRunDetail v-if="run" :key="run.id" :run="run" :items="filteredItems" :runs="runs" :progress="progress" :active="isActive(run)" :busy="busy" :item-filter="itemFilter" :live-confirmed="retryConfirmed" :compare-id="compareID" :comparison="comparison" :compare-error="compareError" @update:item-filter="itemFilter = $event" @update:live-confirmed="retryConfirmed = $event" @update:compare-id="compareID = $event" @action="runAction" @export="exportRun" @compare="compareRuns" @select-item="selectedItem = $event" @new-run="beginRun" />
        <EvalItemDetail v-if="selectedItem" :item="selectedItem" @close="selectedItem = null" />
      </template>
    </template>
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
  error, formError, pollError, busy, createOpen, step, datasetName, datasetContent, validation, datasetID,
  datasetPreview, runName, config, liveConfirmed, retryConfirmed, canStart, compareID, comparison, compareError,
  refresh, openRun, newRun, readDataset, validateDataset, saveDataset, chooseDataset, nextStep, startRun,
  runAction, compareRuns, exportRun } = useEvaluationWorkbench();
const section = ref('workbench');
const runStatuses = [
  { value: 'queued', label: '排队中', type: 'info' }, { value: 'running', label: '评测中', type: 'primary' },
  { value: 'cancelling', label: '正在取消', type: 'warning' }, { value: 'cancelled', label: '已取消', type: 'info' },
  { value: 'completed', label: '已完成', type: 'success' }, { value: 'failed', label: '执行失败', type: 'danger' },
  { value: 'interrupted', label: '已中断', type: 'warning' },
];
const uploadFiles = ref([]);
const editJSONL = ref(false);
const showCapabilities = ref(false);
const previewItems = computed(() => (datasetPreview.value?.items || validation.value?.items || []).slice(0, 5));
const json = value => JSON.stringify(value, null, 2);
const date = value => value ? new Date(value).toLocaleString() : 'N/A';
function beginRun() {
  section.value = 'workbench';
  if (!createOpen.value) { config.value.mode = 'offline'; config.value.judge = false; config.value.closed_book = false; }
  newRun();
}
function selectRun(id) { liveConfirmed.value = false; openRun(id); }
function clearSavedSelection() { if (datasetID.value) chooseDataset(''); }
function clearUpload() { datasetName.value = ''; datasetContent.value = ''; chooseDataset(''); }
watch(capabilities, value => { if (!value?.l1_available) config.value.l1pre = false; });
watch(nsSel, () => { uploadFiles.value = []; editJSONL.value = false; });
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
.wizard { width: min(100%, 960px); }
.config-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--eb-space-4); margin-top: var(--eb-space-4); }
.config-checks { display: grid; gap: var(--eb-space-3); margin: var(--eb-space-4) 0; }
.fingerprint { overflow-wrap: anywhere; }
@media (max-width: 640px) { .config-grid { grid-template-columns: minmax(0, 1fr); } }
</style>

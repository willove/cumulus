<template>
  <section class="pane documents-page">
    <header class="page-heading">
      <div><span class="eyebrow">知识库 / {{ libraryLabel }}</span><h2>文档与导入</h2><p class="sub">管理可检索的原文，查看每一次导入的结果。</p></div>
      <div class="form-actions"><eb-button :disabled="!hasBucket || documentsBusy" @click="loadDocuments">刷新</eb-button><eb-button type="primary" :disabled="!hasBucket" @click="addOpen = !addOpen">{{ addOpen ? '收起导入' : '添加文档' }}</eb-button></div>
    </header>
    <div v-if="!hasBucket" class="empty-guide">
      <h3>先创建一个知识库</h3><p class="sub">每个知识库独立保存文档、任务和会话，避免不同主题互相干扰。</p><eb-button type="primary" @click="$emit('create-library')">新建知识库</eb-button>
    </div>
    <div v-else class="documents-layout" :class="{ 'with-import': addOpen }">
      <aside v-if="addOpen" class="import-panel" aria-label="导入文档">
        <eb-section-card :title="'添加到 ' + libraryLabel" class="import-card">
          <p class="tiny import-hint">先选择来源并确认文件，再提交后台任务；完成后即可检索。</p>
          <eb-select v-model="addWay" class="import-tabs" aria-label="导入方式">
            <eb-option v-for="way in addWays" :key="way.value" :value="way.value" :label="way.label" />
          </eb-select>
          <eb-form v-if="addWay === 'upload'" label-position="top" @finish="startUpload">
            <eb-form-item label="选择本机文件或目录">
              <div class="form-stack">
                <!-- EbUpload 未暴露 directory 属性，仅用原生 input 调起系统选择器。 -->
                <input ref="directoryInput" type="file" webkitdirectory multiple hidden aria-label="选择本机目录" :disabled="uploadBusy" @change="onDirectoryChange" />
                <eb-button icon="folder-open" :disabled="uploadBusy" @click="directoryInput?.click()">选择目录</eb-button>
                <div class="upload-list">
                  <eb-upload :file-list="uploadFiles" :auto-upload="false" :accept="uploadAccept" multiple :disabled="uploadBusy"
                             @change="file => updateUploadFiles([...uploadFiles, file])" @remove="(_, list) => updateUploadFiles(list)">
                    <template #trigger><span>选择文件</span></template>
                  </eb-upload>
                </div>
              </div>
            </eb-form-item>
            <p class="tiny">所选文件会上传到服务端，不读取本机绝对路径。支持 Markdown、文本、HTML、PDF、DOCX；隐藏文件和其他格式不上传。</p>
            <p class="tiny">每批最多 500 个文件、64 MiB，单文件不超过 16 MiB。空目录不包含可上传文件。</p>
            <div v-if="uploadFiles.length" class="form-actions">
              <eb-tag>{{ uploadFiles.length }} 个文件 · {{ fmtSize(uploadBytes) }}</eb-tag>
              <eb-button link :disabled="uploadBusy" @click="updateUploadFiles([])">清空</eb-button>
            </div>
            <eb-button native-type="submit" type="primary" :loading="uploadBusy" :disabled="!uploadFiles.length">确认上传并导入</eb-button>
          </eb-form>
          <eb-form v-else-if="addWay === 'dir'" :model="{ dir: ingDir, name: ingName }" label-position="top" @finish="startIngest">
            <p class="tiny">读取服务所在机器的目录；大批量文件建议先扫描再确认。</p>
            <eb-form-item label="目录路径" prop="dir" :rules="{ required: true, whitespace: true, message: '请填写服务端目录' }">
              <eb-input id="dir" v-model="ingDir" :clearable="false" placeholder="/data/documents" />
            </eb-form-item>
            <eb-form-item>
              <eb-checkbox v-model="ingRec" tabindex="0" @keydown.space.prevent="ingRec = !ingRec">包含子目录</eb-checkbox>
            </eb-form-item>
            <eb-form-item label="任务名称（可选）" prop="name">
              <eb-input id="name" v-model="ingName" :clearable="false" placeholder="留空自动生成" />
            </eb-form-item>
            <p class="tiny">支持 Markdown、文本、HTML、PDF、DOCX。</p>
            <eb-button native-type="submit" type="primary" :loading="ingBusy" :disabled="!ingDir.trim()">开始导入</eb-button>
          </eb-form>
          <div v-else-if="addWay === 'scan'" class="form-stack">
            <div class="document-field">
              <label for="documents-scan-dir">扫描目录</label>
              <eb-input id="documents-scan-dir" v-model="scanDir" :clearable="false" placeholder="/data/documents" />
            </div>
            <eb-checkbox v-model="ingRec" tabindex="0" @keydown.space.prevent="ingRec = !ingRec">包含子目录</eb-checkbox>
            <div class="field-pair">
              <div class="document-field">
                <label for="documents-scan-limit">文件上限</label>
                <eb-input id="documents-scan-limit" v-model="scanLimit" type="number" min="1" :clearable="false" placeholder="不限" />
              </div>
              <div class="document-field">
                <label for="documents-scan-newer">最近修改</label>
                <eb-input id="documents-scan-newer" v-model="scanNewer" :clearable="false" placeholder="例如 168h" />
              </div>
            </div>
            <eb-button :loading="scanBusy" :disabled="!scanDir.trim()" @click="runScan">扫描候选文件</eb-button>
            <template v-if="scanReport">
              <div class="page-heading">
                <span class="sub">候选 {{ (scanReport.candidates || []).length }} 个</span>
                <eb-button link type="primary" size="small" @click="toggleAllScan(pickedCount() !== (scanReport.candidates || []).length)">{{ pickedCount() === (scanReport.candidates || []).length ? '清空选择' : '全选' }}</eb-button>
              </div>
              <div class="scan-list">
                <eb-checkbox v-for="(candidate, i) in (scanReport.candidates || [])" :key="candidate.path" v-model="scanPicked[i]" class="scan-choice" size="small" tabindex="0" :aria-label="candidate.path" :title="candidate.path" @keydown.space.prevent="scanPicked[i] = !scanPicked[i]">
                  <span class="scan-candidate"><span class="candidate-name">{{ candidate.path.split('/').pop() }}</span><span class="tiny candidate-size">{{ fmtSize(candidate.size) }}</span></span>
                </eb-checkbox>
              </div>
              <eb-button type="primary" :loading="ingBusy" :disabled="!pickedCount()" @click="ingestPicked">导入所选（{{ pickedCount() }}）</eb-button>
            </template>
          </div>
          <div v-else class="form-stack">
            <div class="document-field">
              <label for="documents-adapt-paths">文件路径（每行一个）</label>
              <eb-input id="documents-adapt-paths" v-model="adPaths" type="textarea" :rows="4" :clearable="false" placeholder="/data/corpus.parquet&#10;/data/news.json&#10;/data/titles.csv" />
            </div>
            <p class="tiny">JSON / JSONL / CSV / Parquet：先识别字段，再确认映射。</p>
            <eb-button :loading="adBusy" :disabled="!adPaths.trim()" @click="runAdaptProbe">探测文件与字段</eb-button>
            <template v-if="adProbes.length">
              <eb-card v-for="probe in adProbes" :key="probe.path" class="probe-card" :hoverable="false">
                <div class="probe-content">
                  <strong>{{ probe.path.split('/').pop() }}</strong>
                  <p class="tiny">{{ probe.kind }} · {{ fmtSize(probe.bytes) }}</p>
                  <div class="probe-fields"><eb-tag v-for="field in probe.fields" :key="field" :title="field">{{ field }}</eb-tag></div>
                </div>
              </eb-card>
              <div class="document-field">
                <label for="documents-adapt-id">ID 字段</label>
                <eb-input id="documents-adapt-id" v-model="adMap.id" :clearable="false" placeholder="自动识别" />
              </div>
              <div class="document-field">
                <label for="documents-adapt-title">标题字段</label>
                <eb-input id="documents-adapt-title" v-model="adMap.title" :clearable="false" placeholder="自动识别" />
              </div>
              <div class="document-field">
                <label for="documents-adapt-body">正文字段</label>
                <eb-input id="documents-adapt-body" v-model="adMap.body" :clearable="false" placeholder="自动识别" />
              </div>
              <p class="tiny">上述映射应用于本次所有文件；不同结构请分批导入。</p>
              <eb-button type="primary" :loading="adBusy" @click="submitAdapt">确认映射并导入</eb-button>
            </template>
          </div>
          <eb-alert v-if="activeMeta" class="feedback" :title="activeMeta" :type="activeMeta.startsWith('出错') ? 'error' : 'info'" :closable="false" show-icon />
        </eb-section-card>
      </aside>
      <div class="document-content">
        <eb-section-card v-if="jobs.length" class="jobs-section" role="region" aria-labelledby="documents-jobs-heading">
          <template #header><h3 id="documents-jobs-heading">导入任务</h3></template>
          <template #extra><eb-button link type="primary" size="small" @click="pollJobs">刷新任务</eb-button></template>
          <div class="job-list">
            <eb-button v-for="job in jobs" :key="job.id" class="job-row" size="small" plain :type="job.id === jobCur ? 'primary' : 'default'" :aria-pressed="job.id === jobCur" @click="selectJob(job.id)">
              <span class="job-name">{{ job.id }}</span>
              <eb-tag :type="job.state === 'failed' ? 'danger' : job.skipped ? 'warning' : jobTypes[job.state] || 'info'">{{ job.skipped && job.state === 'done' ? '完成，有跳过' : jobLabels[job.state] }}</eb-tag>
              <span class="tiny">处理 {{ job.done }}/{{ job.total }} 个文件</span>
            </eb-button>
          </div>
          <div v-if="selectedJob" class="job-detail" aria-live="polite">
            <div class="tiny">{{ selectedJob.id }}<span v-if="selectedJob.phase"> · {{ selectedJob.phase }}</span></div>
            <div class="job-counts"><span>已处理 <b>{{ selectedJob.done || 0 }}</b> 文件</span><span>跳过 <b>{{ selectedJob.skipped || 0 }}</b></span><span>失败 <b>{{ selectedJob.failed || 0 }}</b></span><span v-if="selectedJob.records != null">写入 <b>{{ selectedJob.records }}</b> 条记录</span></div>
            <p class="tiny">文件处理进度不等于入库文档数；实际可检索文档以列表为准。</p>
            <eb-alert v-if="selectedJob.error" type="error" :title="selectedJob.error" :closable="false" show-icon />
            <eb-alert v-if="selectedJob.pollError" type="error" :title="'状态读取失败：' + selectedJob.pollError" :closable="false" show-icon><eb-button link type="primary" size="small" @click="pollJobs">重试</eb-button></eb-alert>
            <eb-alert v-if="selectedJob.skipped" type="warning" title="部分文件已跳过" :closable="false" show-icon>{{ Object.entries(selectedJob.skip_reasons || {}).map(([key, count]) => key + ' × ' + count).join('，') || '请查看任务详情' }}</eb-alert>
            <ul v-if="Object.keys(selectedJob.skip_errors || {}).length" class="skip-errors"><li v-for="(reason, path) in selectedJob.skip_errors" :key="path"><b>{{ path }}</b>：{{ reason }}</li></ul>
          </div>
        </eb-section-card>
        <eb-section-card class="document-section" role="region" aria-labelledby="documents-list-heading">
          <template #header><h3 id="documents-list-heading">文档 <span class="tiny">{{ documents.length }} 篇</span></h3></template>
          <template #extra><eb-input v-model="filter" class="document-filter" :clearable="false" aria-label="搜索已导入文档" placeholder="搜索文档名称" /></template>
          <eb-alert v-if="documentsError" type="error" :title="documentsError" :closable="false" show-icon><eb-button link type="primary" size="small" @click="loadDocuments">重试</eb-button></eb-alert>
          <div v-if="documentsBusy" class="tiny" role="status">正在加载文档…</div>
          <eb-table v-if="filteredDocuments.length" :data="filteredDocuments" row-key="id" class="document-table" aria-label="已导入文档" :scroll-x="600">
            <eb-table-column prop="title" label="文档名称" min-width="220">
              <template #default="{ row }"><eb-button link type="primary" class="source-link" @click="previewSource = row.id">{{ row.title || row.id }}</eb-button></template>
            </eb-table-column>
            <eb-table-column prop="type" label="类型" width="90">
              <template #default="{ row }"><eb-tag>{{ row.type }}</eb-tag></template>
            </eb-table-column>
            <eb-table-column prop="bytes" label="大小" width="100" :formatter="row => fmtSize(row.bytes)" />
            <eb-table-column prop="ingested_at" label="导入时间" width="170" :formatter="row => fmtWhen(row.ingested_at)" />
          </eb-table>
          <div v-else-if="!documentsBusy && !documentsError" class="docs-empty"><h3>{{ documents.length ? '没有匹配的文档' : '知识库里还没有文档' }}</h3><p class="sub">{{ documents.length ? '试试其他关键词。' : '使用导入面板添加文件，完成后文档会自动出现在这里。' }}</p><eb-button v-if="!documents.length && !addOpen" @click="addOpen = true">添加文档</eb-button></div>
          <div v-if="documents.length" class="document-footer"><span class="tiny">点击文档名称查看原文</span><eb-button type="primary" size="small" @click="pane = 'chat'">去提问</eb-button></div>
        </eb-section-card>
      </div>
    </div>
    <SourcePreview v-if="previewSource" :source-id="previewSource" @close="previewSource = ''" />
  </section>
</template>

<script setup>
import { ref, computed, watch, onMounted } from "vue";
import { useIngestPane } from "../panes/ingest.js";
import { documents, documentsBusy, documentsError, hasBucket, libraryLabel, loadDocuments, pane } from "../state.js";
import SourcePreview from "./SourcePreview.vue";
defineEmits(["create-library"]);
const { ingDir, ingRec, ingName, ingBusy, ingMeta, jobs, jobCur, curJob,
  uploadFiles, uploadMeta, uploadBusy, uploadBytes, uploadAccept, selectDirectory, updateUploadFiles, startUpload,
  scanDir, scanLimit, scanNewer, scanBusy, scanReport, scanPicked, scanMeta,
  adPaths, adBusy, adProbes, adMeta, adMap, selectJob, startIngest, runScan,
  pickedCount, toggleAllScan, ingestPicked, runAdaptProbe, submitAdapt, pollJobs } = useIngestPane();
const directoryInput = ref(null);
function onDirectoryChange(event) {
  selectDirectory(event.target.files);
  event.target.value = "";
}
const addOpen = ref(true);
const addWay = ref("upload");
const filter = ref("");
const previewSource = ref("");
const addWays = [{ value: "upload", label: "本机文件 / 目录" }, { value: "scan", label: "服务端扫描（先预览）" }, { value: "dir", label: "服务端目录（直接导入）" }, { value: "adapt", label: "结构化数据（字段映射）" }];
const jobLabels = { queued: "排队中", running: "导入中", done: "处理完成", failed: "导入失败" };
const jobTypes = { queued: "info", running: "primary", done: "success", failed: "danger" };
const selectedJob = computed(curJob);
const activeMeta = computed(() => addWay.value === "upload" ? uploadMeta.value : addWay.value === "dir" ? ingMeta.value : addWay.value === "scan" ? scanMeta.value : adMeta.value);
const filteredDocuments = computed(() => documents.value.filter(doc => (doc.title || doc.id).toLowerCase().includes(filter.value.trim().toLowerCase())));
watch(() => jobs.value.filter(job => job.state === "done" || job.state === "failed").map(job => job.id + job.state).join("|"), loadDocuments);
onMounted(loadDocuments);
function fmtSize(bytes) { return bytes < 1024 ? bytes + " B" : bytes < 1048576 ? (bytes / 1024).toFixed(1) + " KB" : (bytes / 1048576).toFixed(1) + " MB"; }
function fmtWhen(value) { return value ? new Date(value).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }) : "—"; }
</script>

<style scoped>
.documents-page { background: var(--eb-bg-color-page); }
.documents-layout { display: grid; grid-template-columns: minmax(0, 1fr); align-items: start; gap: var(--eb-space-6); margin-top: var(--eb-space-6); }
.documents-layout.with-import { grid-template-columns: 310px minmax(0, 1fr); }
.import-panel, .jobs-section, .document-section { min-width: 0; }
.documents-page :deep(.eb-section-card__header) { flex-wrap: wrap; gap: var(--eb-space-4); }
.documents-page :deep(.eb-section-card__header-left) { min-width: 0; overflow-wrap: anywhere; }
.documents-page :deep(.eb-section-card__extra) { max-width: 100%; }
.import-hint { line-height: 1.8; margin: 0 0 var(--eb-space-4); }
.import-tabs { width: 100%; margin-bottom: var(--eb-space-5); }
.upload-list { max-height: 240px; overflow: auto; }
.import-card .feedback { margin-top: var(--eb-space-4); }
.document-field { display: grid; gap: var(--eb-space-2); min-width: 0; }
.document-field > label { font-size: var(--eb-font-size-xs); color: var(--eb-text-color-regular); }
.field-pair { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--eb-space-3); }
.scan-list { max-height: 240px; overflow: auto; display: grid; gap: var(--eb-space-2); padding: var(--eb-space-1); }
.documents-page .scan-choice { min-width: 0; height: auto; min-height: var(--eb-component-size-small); white-space: normal; }
.scan-list .scan-choice :deep(.eb-checkbox__label) { flex: 1; min-width: 0; }
.scan-candidate { display: flex; align-items: center; gap: var(--eb-space-2); }
.candidate-name { flex: 1; min-width: 0; overflow-wrap: anywhere; }
.candidate-size { flex: none; }
.probe-content { display: grid; gap: var(--eb-space-2); min-width: 0; font-size: var(--eb-font-size-xs); overflow-wrap: anywhere; }
.probe-fields { display: flex; flex-wrap: wrap; gap: var(--eb-space-1); min-width: 0; }
.probe-fields > * { max-width: 100%; }
.document-content { display: grid; gap: var(--eb-space-6); min-width: 0; }
.job-list { margin-bottom: var(--eb-space-4); padding: var(--eb-space-1); display: grid; gap: var(--eb-space-2); max-height: 220px; overflow: auto; }
.documents-page .job-row { flex-wrap: wrap; gap: var(--eb-space-2); justify-content: flex-start; width: 100%; height: auto; text-align: left; white-space: normal; }
.job-name { flex: 1; min-width: 80px; overflow-wrap: anywhere; }
.job-detail { border-top: 1px solid var(--eb-border-color-light); padding-top: var(--eb-space-4); overflow-wrap: anywhere; }
.job-counts { display: flex; flex-wrap: wrap; gap: var(--eb-space-4); font-size: var(--eb-font-size-xs); margin: var(--eb-space-3) 0; color: var(--eb-text-color-secondary); }
.job-counts b { color: var(--eb-text-color-primary); }
.skip-errors { font-size: var(--eb-font-size-xs); line-height: 1.8; padding-left: var(--eb-space-5); }
.document-filter { width: 200px; max-width: 100%; min-width: 0; }
.document-table .source-link { height: auto; max-width: 100%; white-space: normal; text-align: left; overflow-wrap: anywhere; }
.docs-empty { padding: var(--eb-space-12) var(--eb-space-4); text-align: center; }
.docs-empty p { line-height: 1.8; }
.document-footer { display: flex; justify-content: space-between; align-items: center; gap: var(--eb-space-3); margin-top: var(--eb-space-5); }
@media (max-width: 1000px) { .documents-layout.with-import { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 600px) { .document-filter { width: 160px; } }
</style>

// 三路导入共享持久任务队列；任务身份是 (namespace, id)，而非裸 id。
import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { nsSel, pane, withNS } from "../state.js";
import { api, requestJSON } from "../api.js";

// 表单及任务跨视图保留，轮询器则必须归属当前挂载实例。
const ingDir = ref("");
const ingRec = ref(false);
const ingName = ref("");
const ingPending = ref(Object.create(null));
const ingBusy = computed(() => !!ingPending.value[nsSel.value]);
const ingMeta = ref("");
const uploadFiles = ref([]);
const uploadMeta = ref("");
const uploadPending = ref(Object.create(null));
const uploadBusy = computed(() => !!uploadPending.value[nsSel.value]);
const uploadBytes = computed(() => uploadFiles.value.reduce((sum, file) => sum + file.size, 0));
const uploadAccept = ".md,.txt,.html,.htm,.pdf,.docx";
const allJobs = ref([]);
const selectedJobs = ref(Object.create(null));
const jobs = computed(() => allJobs.value.filter((j) => j.ns === nsSel.value));
const jobCur = computed({
  get: () => selectedJobs.value[nsSel.value] || "",
  set: (id) => { selectedJobs.value[nsSel.value] = id; },
});
const scanDir = ref("");
const scanLimit = ref("");
const scanNewer = ref("");
const scanBusy = ref(false);
const scanReport = ref(null);
const scanPicked = ref([]);
const scanMeta = ref("");
const tab = ref("scan");
const adPaths = ref("");
const adPending = ref(Object.create(null));
const probeBusy = ref(false);
const adBusy = computed(() => probeBusy.value || !!adPending.value[nsSel.value]);
const adProbes = ref([]);
const adMeta = ref("");
const emptyMapping = () => ({ id: "", title: "", body: "", extra: "" });
const adMap = ref(emptyMapping());
const adJob = ref("");
let scanVersion = 0, probeVersion = 0;
let scanKey = "", probeKey = "";
let formNamespace = nsSel.value;

export function useIngestPane() {
  let alive = true, mounted = false;
  let pollHandle = null, pollPromise = null, pollController = null;
  const jobLabel = { queued: "排队中", running: "进行中", done: "已完成", failed: "已失败" };
  const pollError = computed(() => jobs.value.filter((j) => j.pollError)
    .map((j) => j.id + "：" + j.pollError).join("；"));
  function jobDone(j) { return j.state === "done" || j.state === "failed"; }
  function curJob() { return jobs.value.find((j) => j.id === jobCur.value) || null; }
  function fmtMB(b) { return (b / 1e6).toFixed(1) + " MB"; }
  function requireNS(namespace, target) {
    if (namespace) return true;
    target.value = "请先在右上角创建或选择知识库";
    return false;
  }
  function showResult(namespace, target, text) {
    if (alive && namespace === nsSel.value) target.value = text;
  }
  function queueJob(d, namespace, total = 0) {
    if (!d || typeof d.job !== "string" || !d.job) throw new Error("任务响应缺少 job id");
    const job = {
      state: "queued", phase: "", total, done: 0, failed: 0, skipped: 0,
      skip_reasons: {}, skip_errors: {}, records: null, error: "", updated: "",
      ...d, id: d.job, ns: namespace, pollError: "",
    };
    // 同库同名任务刷新，异库同名任务并存。替换对象也使旧轮询响应失效。
    allJobs.value = [job, ...allJobs.value.filter((j) => j.ns !== namespace || j.id !== job.id)];
    selectedJobs.value[namespace] = job.id;
    return job;
  }
  function clearPoll() {
    if (pollHandle !== null) clearTimeout(pollHandle);
    pollHandle = null;
  }
  // 轮询可见性：文档页签挂在知识库页（library）下；documents/ingest 是旧 hash 的兜底。
  function visible() { return alive && mounted && pane.value === "corpus"; }
  function schedulePoll() {
    if (!visible()) { clearPoll(); return; }
    if (pollHandle !== null || pollPromise) return;
    // 读取失败保留真实 job 状态；暂停该任务的自动重试，交给手动 pollJobs。
    if (!allJobs.value.some((j) => !jobDone(j) && !j.pollError)) return;
    pollHandle = setTimeout(() => {
      pollHandle = null;
      if (visible()) void pollJobs(false);
    }, 1000);
  }
  function pollJobs(retryErrors = true) {
    if (!alive) return Promise.resolve();
    if (pollPromise) return pollPromise;
    clearPoll();
    pollPromise = (async () => {
      const active = allJobs.value.filter((j) => !jobDone(j) && (retryErrors || !j.pollError));
      for (const j of active) {
        if (!alive) break;
        const controller = pollController = new AbortController();
        try {
          const d = await requestJSON(withNS("/v1/ingest/jobs/" + encodeURIComponent(j.id), j.ns), {
            signal: controller.signal,
          });
          if (!alive || controller.signal.aborted || !allJobs.value.includes(j)) continue;
          if (!d || typeof d.state !== "string") throw new Error("任务状态响应格式无效");
          // 保留后端完整统计（包括 skipped/skip_reasons/skip_errors/records）。
          Object.assign(j, d, { id: j.id, ns: j.ns, pollError: "" });
        } catch (e) {
          if (alive && !controller.signal.aborted && allJobs.value.includes(j)) j.pollError = e.message;
        } finally {
          if (pollController === controller) pollController = null;
        }
      }
    })().finally(() => { pollPromise = null; schedulePoll(); });
    return pollPromise;
  }

  function selectDirectory(files) {
    if (!files?.length || uploadBusy.value) return;
    const entries = Array.from(files).map((raw, i) => ({
      raw, uid: "directory-" + i, name: raw.webkitRelativePath || raw.name, size: raw.size, status: "ready",
    }));
    updateUploadFiles(entries);
  }
  function updateUploadFiles(entries) {
    if (uploadBusy.value) return;
    const seen = new Set();
    uploadFiles.value = entries.filter(file => {
      const name = file.raw.webkitRelativePath || file.raw.name;
      file.name = name;
      if (!file.size || !/\.(md|txt|html?|pdf|docx)$/i.test(name) || name.split("/").some(p => p.startsWith(".")) || name.endsWith("~") || seen.has(name)) return false;
      seen.add(name);
      return true;
    });
    const skipped = entries.length - uploadFiles.value.length;
    uploadMeta.value = skipped ? skipped + " 个文件已跳过（空文件、隐藏文件、不支持的格式或重复路径）" : "";
  }
  async function startUpload() {
    const namespace = nsSel.value;
    if (!requireNS(namespace, uploadMeta) || uploadPending.value[namespace]) return;
    const files = uploadFiles.value;
    if (!files.length) { uploadMeta.value = "请先选择目录或文件"; return; }
    if (files.length > 500) { uploadMeta.value = "出错：每批最多 500 个文件，请分批选择"; return; }
    if (files.some(file => file.size > 16 * 1024 * 1024)) { uploadMeta.value = "出错：单文件不能超过 16 MiB，请移除超限文件"; return; }
    if (uploadBytes.value > 64 * 1024 * 1024) { uploadMeta.value = "出错：每批不能超过 64 MiB，请分批选择"; return; }
    uploadPending.value[namespace] = true;
    uploadMeta.value = "正在上传，服务接收完成后开始摄取……";
    try {
      const d = await api.upload(namespace, files);
      const job = queueJob(d, namespace, files.length);
      if (namespace === nsSel.value && uploadFiles.value === files) {
        uploadFiles.value = [];
        uploadMeta.value = "已提交任务（" + job.total + " 个文件），可在任务列表查看进度。";
      }
    } catch (e) {
      if (namespace === nsSel.value && uploadFiles.value === files) uploadMeta.value = "出错：" + e.message;
    } finally {
      uploadPending.value[namespace] = false;
      schedulePoll();
    }
  }

  async function startIngest() {
    const namespace = nsSel.value;
    const dir = ingDir.value.trim();
    if (!requireNS(namespace, ingMeta)) return;
    if (!dir) { ingMeta.value = "请先填写目录（服务器本地路径）"; return; }
    if (ingPending.value[namespace]) return;
    ingPending.value[namespace] = true;
    ingMeta.value = "提交摄取任务……";
    try {
      const d = await api.ingest({
        dir, recursive: ingRec.value, job: ingName.value.trim() || undefined, ns: namespace,
      });
      const job = queueJob(d, namespace);
      showResult(namespace, ingMeta, "已提交任务 " + job.id + "（候选 " + job.total + " 个文件）");
    } catch (e) {
      showResult(namespace, ingMeta, "出错：" + e.message);
    } finally {
      ingPending.value[namespace] = false;
      schedulePoll();
    }
  }

  function scanSignature() {
    return JSON.stringify([nsSel.value, scanDir.value.trim(), ingRec.value, scanLimit.value, scanNewer.value]);
  }
  function clearScan() {
    scanVersion++;
    scanKey = "";
    scanReport.value = null; scanPicked.value = []; scanMeta.value = ""; scanBusy.value = false;
  }
  async function runScan() {
    const namespace = nsSel.value;
    const dir = scanDir.value.trim();
    clearScan();
    if (!requireNS(namespace, scanMeta)) return;
    if (!dir) { scanMeta.value = "请先填写要扫描的目录"; return; }
    const version = scanVersion;
    const key = scanSignature();
    scanBusy.value = true; scanMeta.value = "扫描中……";
    try {
      const d = await api.scan({
        dir, recursive: ingRec.value, limit: parseInt(scanLimit.value) || 0,
        newer_than: scanNewer.value.trim() || undefined, ns: namespace,
      });
      if (!alive || version !== scanVersion || key !== scanSignature()) return;
      if (!d || !Array.isArray(d.candidates)) throw new Error("扫描响应格式无效");
      scanKey = key;
      scanReport.value = d;
      scanPicked.value = d.candidates.map(() => true);
      const sk = Object.entries(d.skipped || {}).map(([k, v]) => k + "×" + v).join(" ");
      scanMeta.value = "候选 " + d.candidates.length + " 个" + (sk ? " · 跳过 " + sk : "")
        + (d.rank_error ? " · 排名失败保留规则序" : "");
    } catch (e) {
      if (alive && version === scanVersion) scanMeta.value = "出错：" + e.message;
    } finally {
      if (version === scanVersion) scanBusy.value = false;
    }
  }
  function pickedCount() { return scanPicked.value.filter(Boolean).length; }
  function toggleAllScan(v) { scanPicked.value = (scanReport.value?.candidates || []).map(() => v); }
  async function ingestPicked() {
    const namespace = nsSel.value;
    if (!requireNS(namespace, scanMeta)) return;
    if (!scanKey || scanKey !== scanSignature()) { clearScan(); scanMeta.value = "请先重新扫描目录"; return; }
    const paths = (scanReport.value?.candidates || []).filter((_, i) => scanPicked.value[i]).map((c) => c.path);
    if (!paths.length) { scanMeta.value = "没有选中的候选"; return; }
    if (ingPending.value[namespace]) return;
    ingPending.value[namespace] = true; scanMeta.value = "提交 " + paths.length + " 个选中文件……";
    try {
      const d = await api.ingest({
        candidates: paths, job: ingName.value.trim() || undefined, ns: namespace,
      });
      const job = queueJob(d, namespace, paths.length);
      showResult(namespace, scanMeta, "已提交任务 " + job.id + "（" + paths.length + " 个文件）");
    } catch (e) {
      showResult(namespace, scanMeta, "出错：" + e.message);
    } finally {
      ingPending.value[namespace] = false;
      schedulePoll();
    }
  }

  function adFileList() { return adPaths.value.split("\n").map((x) => x.trim()).filter(Boolean); }
  function clearProbe() {
    probeVersion++;
    probeKey = "";
    adProbes.value = []; adMap.value = emptyMapping(); adMeta.value = ""; probeBusy.value = false;
  }
  async function runAdaptProbe() {
    const paths = adFileList();
    clearProbe();
    if (!paths.length) { adMeta.value = "请先填写文件路径（每行一个）"; return; }
    const version = probeVersion;
    const key = JSON.stringify(paths);
    probeBusy.value = true; adMeta.value = "探测中……";
    try {
      const d = await api.probe(paths);
      if (!alive || version !== probeVersion || key !== JSON.stringify(adFileList())) return;
      if (!d || !Array.isArray(d.probes)) throw new Error("探测响应格式无效");
      adProbes.value = d.probes;
      probeKey = key;
      const bad = d.failed || [];
      const p = d.probes[0];
      if (p) adMap.value = {
        id: (p.idish || [])[0] || "", title: (p.titleish || [])[0] || "",
        body: (p.bodyish || [])[0] || "", extra: "",
      };
      adMeta.value = "探测到 " + d.probes.length + " 个文件"
        + (bad.length ? " · " + bad.length + " 个读取失败" : "") + (p?.note ? " · " + p.note : "");
    } catch (e) {
      if (alive && version === probeVersion) adMeta.value = "出错：" + e.message;
    } finally {
      if (version === probeVersion) probeBusy.value = false;
    }
  }
  async function submitAdapt() {
    const namespace = nsSel.value;
    const paths = adFileList();
    if (!requireNS(namespace, adMeta)) return;
    if (!paths.length) { adMeta.value = "请先填写文件路径"; return; }
    if (!probeKey || probeKey !== JSON.stringify(paths) || !adProbes.value.length) {
      clearProbe(); adMeta.value = "路径已变化或尚未探测，请先重新探测字段"; return;
    }
    if (adPending.value[namespace]) return;
    adPending.value[namespace] = true; adMeta.value = "提交摄取……";
    try {
      const d = await api.adapt({
        paths, ns: namespace, job: adJob.value.trim() || undefined,
        id: adMap.value.id || undefined, title: adMap.value.title || undefined, body: adMap.value.body || undefined,
      });
      const job = queueJob(d, namespace);
      showResult(namespace, adMeta, "已提交任务 " + job.id);
    } catch (e) {
      showResult(namespace, adMeta, "出错：" + e.message);
    } finally {
      adPending.value[namespace] = false;
      schedulePoll();
    }
  }

  function syncNamespace() {
    if (formNamespace === nsSel.value) return;
    formNamespace = nsSel.value;
    clearScan(); clearProbe(); ingMeta.value = "";
    uploadFiles.value = []; uploadMeta.value = "";
  }
  // namespace 可能在面板卸载期间改变，此时旧实例的 watch 已停止。
  syncNamespace();
  if (scanKey && scanKey !== scanSignature()) clearScan();
  if (probeKey && probeKey !== JSON.stringify(adFileList())) clearProbe();
  watch([scanDir, scanLimit, scanNewer, ingRec], clearScan, { flush: "sync" });
  watch(adPaths, clearProbe, { flush: "sync" });
  watch(nsSel, syncNamespace, { flush: "sync" });
  watch(pane, schedulePoll, { flush: "sync" });
  // 旧实例的迟到提交仍入正确库；新实例观察队列变化后接管轮询。
  watch(allJobs, schedulePoll, { deep: true, flush: "sync" });
  onMounted(() => { mounted = true; schedulePoll(); });
  onUnmounted(() => {
    alive = false;
    clearPoll();
    pollController?.abort();
    if (scanBusy.value) clearScan();
    if (probeBusy.value) clearProbe();
  });

  return {
    ingDir, ingRec, ingName, ingBusy, ingMeta, jobs, jobCur, pollError,
    uploadFiles, uploadMeta, uploadBusy, uploadBytes, uploadAccept, selectDirectory, updateUploadFiles, startUpload,
    scanDir, scanLimit, scanNewer, scanBusy, scanReport, scanPicked, scanMeta,
    tab, jobLabel, jobDone, curJob, fmtMB, selectJob: (id) => { jobCur.value = id; },
    startIngest, runScan, pickedCount, toggleAllScan, ingestPicked, pollJobs,
    adPaths, adBusy, adProbes, adMeta, adMap, adJob, adFileList, runAdaptProbe, submitAdapt,
  };
}

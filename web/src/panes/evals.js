import { ref, computed, watch, onMounted, onUnmounted } from "vue";
import { withNS, nsSel, pane } from "../state.js";
import { requestJSON, evaluationAPI } from "../api.js";

export function useEvalsPane() {
  const evalRuns = ref([]);
  const evalCur = ref(null);
  const evalLoading = ref(false);
  const evalError = ref("");
  let detailRequest = 0, listRequest = 0, alive = true;

  async function loadEvals() {
    const namespace = nsSel.value, request = ++listRequest;
    if (!namespace) { evalRuns.value = []; evalLoading.value = false; return; }
    evalLoading.value = true;
    evalError.value = "";
    try {
      const r = await requestJSON(withNS("/v1/evals?limit=50", namespace));
      if (alive && request === listRequest && namespace === nsSel.value) evalRuns.value = r.runs || [];
    } catch (error) {
      if (alive && request === listRequest && namespace === nsSel.value) evalError.value = error.message;
    } finally { if (alive && request === listRequest) evalLoading.value = false; }
  }

  async function openEval(id) {
    const namespace = nsSel.value, request = ++detailRequest;
    evalCur.value = null;
    evalError.value = "";
    if (!namespace) return;
    try {
      const data = await requestJSON(withNS("/v1/evals/" + encodeURIComponent(id), namespace));
      if (alive && request === detailRequest && namespace === nsSel.value) evalCur.value = data;
    } catch (error) {
      if (alive && request === detailRequest && namespace === nsSel.value) evalError.value = error.message;
    }
  }

  function resetForNs() { detailRequest++; evalCur.value = null; evalRuns.value = []; return loadEvals(); }

  onMounted(loadEvals);
  onUnmounted(() => { alive = false; detailRequest++; listRequest++; });

  return {
    evalRuns, evalCur, evalLoading, evalError, loadEvals, openEval, resetForNs,
    pct: (x) => ((x || 0) * 100).toFixed(1) + "%",
    shortSha: (s) => (s ? s.slice(0, 12) : "—"),
    txRatio(tx, k) {
      const t = tx || {};
      const total = (t.correct || 0) + (t.retrieved_but_unanswered || 0) + (t.answered_but_wrong || 0) + (t.not_retrieved || 0);
      return total > 0 ? (t[k] || 0) / total : 0;
    },
  };
}

export function useEvaluationWorkbench() {
  const capabilities = ref(null);
  const datasets = ref([]);
  const runs = ref([]);
  const run = ref(null);
  const items = ref([]);
  const selectedItem = ref(null);
  const error = ref("");
  const formError = ref("");
  const busy = ref({});
  const datasetName = ref("");
  const datasetContent = ref("");
  const validation = ref(null);
  const datasetID = ref("");
  const datasetPreview = ref(null);
  const config = ref({ mode: "offline", judge: false, closed_book: false, prior: true, l1pre: false,
    item_timeout_seconds: 120, timeout_seconds: 1800, token_budget: 100000, limit: 0 });
  const liveConfirmed = ref(false);
  const retryConfirmed = ref(false);
  const itemFilter = ref("all");
  const compareID = ref("");
  const comparison = ref(null);
  const compareError = ref("");
  const pollError = ref("");
  const activeStates = ["queued", "running", "cancelling"];
  const isActive = value => activeStates.includes(value?.state);
  const filteredItems = computed(() => items.value.filter(item => itemFilter.value === "all" ||
    (itemFilter.value === "failed" ? item.state === "failed" : itemFilter.value === "wrong" ? !item.rule_match : !item.evidence_hit)));
  const progress = computed(() => run.value?.total ? Math.min(100, Math.round(run.value.done / run.value.total * 100)) : 0);
  const canStart = computed(() => !!datasetID.value && !!capabilities.value && !busy.value.submit &&
    (config.value.mode === "offline" || (capabilities.value.live_available && liveConfirmed.value)));
  let alive = true, mounted = false, epoch = 0, timer = null, polling = false;
  let selectedRunID = "", submissionKey = "", submissionSignature = "";
  const requests = new Map();
  function cancelRequest(key) {
    requests.get(key)?.controller.abort();
    requests.delete(key);
    busy.value[key] = false;
  }
  function contextFor(key) {
    cancelRequest(key);
    const op = { controller: new AbortController(), ns: nsSel.value, epoch };
    requests.set(key, op);
    busy.value[key] = true;
    return op;
  }
  function valid(key, op) {
    return alive && op.epoch === epoch && op.ns === nsSel.value && requests.get(key) === op && !op.controller.signal.aborted;
  }
  async function request(key, work, apply, target = error) {
    if (!nsSel.value || !alive) return false;
    const op = contextFor(key);
    target.value = "";
    try {
      const result = await work(op.ns, op.controller.signal);
      if (!valid(key, op)) return false;
      apply(result);
      return true;
    } catch (err) {
      if (valid(key, op)) target.value = err.message;
      return false;
    } finally {
      if (requests.get(key) === op) { requests.delete(key); busy.value[key] = false; }
    }
  }
  function clearPoll() { if (timer !== null) clearTimeout(timer); timer = null; }
  function schedule() {
    clearPoll();
    if (!alive || !mounted || pane.value !== "evals" || polling || pollError.value || !(runs.value.some(isActive) || isActive(run.value))) return;
    timer = setTimeout(() => { timer = null; void poll(); }, 1500);
  }
  async function loadRuns(target = error) {
    return request("runs", (ns, signal) => evaluationAPI.get("runs", ns, signal), data => { runs.value = data.runs || []; }, target);
  }
  async function loadDatasets() {
    return request("datasets", (ns, signal) => evaluationAPI.get("datasets", ns, signal), data => { datasets.value = data.datasets || []; });
  }
  async function refresh() {
    pollError.value = "";
    error.value = "";
    if (!await request("capabilities", (ns, signal) => evaluationAPI.get("capabilities", ns, signal), data => { capabilities.value = data; })) return;
    if (!await loadDatasets()) return;
    if (!await loadRuns()) return;
    if (selectedRunID) await openRun(selectedRunID, false);
    schedule();
  }
  async function openRun(id, reset = true, target = error) {
    selectedRunID = id;
    if (reset) {
      retryConfirmed.value = false;
      run.value = null; items.value = []; selectedItem.value = null;
      comparison.value = null; compareID.value = ""; compareError.value = "";
      cancelRequest("compare");
    }
    const ok = await request("detail", async (ns, signal) => {
      const [detail, results] = await Promise.all([
        evaluationAPI.get("runs/" + encodeURIComponent(id), ns, signal),
        evaluationAPI.get("runs/" + encodeURIComponent(id) + "/items", ns, signal),
      ]);
      return { detail, results };
    }, ({ detail, results }) => {
      run.value = detail;
      items.value = results.items || [];
      if (selectedItem.value) selectedItem.value = items.value.find(item => item.id === selectedItem.value.id) || null;
    }, target);
    schedule();
    return ok;
  }
  async function poll() {
    if (polling || !alive || !mounted || pane.value !== "evals") return;
    polling = true;
    const currentEpoch = epoch;
    try {
      if (!await loadRuns(pollError) || currentEpoch !== epoch) return;
      if (selectedRunID) await openRun(selectedRunID, false, pollError);
    } finally { polling = false; schedule(); }
  }
  function invalidateValidation() {
    cancelRequest("validate"); cancelRequest("save"); cancelRequest("file"); cancelRequest("dataset");
    datasetID.value = ""; datasetPreview.value = null;
    validation.value = null; formError.value = ""; liveConfirmed.value = false;
  }
  async function readDataset(file) {
    const raw = file?.raw || file;
    if (!raw || busy.value.submit) return;
    cancelRequest("dataset");
    datasetID.value = ""; datasetPreview.value = null;
    validation.value = null;
    await request("file", async () => {
      if (raw.size > (capabilities.value?.max_bytes || 2097152)) throw new Error("题集文件不能超过 2 MiB");
      return { name: raw.name, content: await raw.text() };
    }, data => { datasetName.value = data.name; datasetContent.value = data.content; datasetID.value = ""; datasetPreview.value = null; }, formError);
  }
  async function validateDataset() {
    if (!datasetContent.value.trim()) { formError.value = "请先选择题集文件或填写 JSONL"; return; }
    const body = { name: datasetName.value.trim(), content: datasetContent.value };
    await request("validate", (ns, signal) => evaluationAPI.post("datasets/validate", ns, body, signal), data => { validation.value = data; }, formError);
  }
  async function saveDataset() {
    if (busy.value.save || !validation.value?.valid) return;
    const body = { name: datasetName.value.trim(), content: datasetContent.value };
    const ok = await request("save", (ns, signal) => evaluationAPI.post("datasets", ns, body, signal), data => {
      datasetID.value = data.id; datasetPreview.value = data;
    }, formError);
    if (ok) await loadDatasets();
  }
  async function chooseDataset(id) {
    cancelRequest("file"); cancelRequest("validate"); cancelRequest("save");
    datasetID.value = id; datasetPreview.value = null;
    validation.value = null; liveConfirmed.value = false;
    cancelRequest("dataset");
    if (!id) return;
    await request("dataset", (ns, signal) => evaluationAPI.get("datasets/" + encodeURIComponent(id), ns, signal), data => { datasetPreview.value = data; }, formError);
  }
  async function startRun() {
    if (busy.value.submit) return;
    if (!nsSel.value) { formError.value = "请先选择知识库"; return; }
    // canStart 即全部前置（线性流：题集 + 模式 + 运行，没有 step 门槛）。
    // live 模式缺模型那条不在这里报——startRunClick 会在开对话框之前就说清楚，
    // 走到这一步还失败只剩「题集没选/正在提交」，那句误导性的话就没有机会出现了。
    if (!canStart.value) { formError.value = "题集未就绪或正在提交中"; return; }
    const body = { dataset_id: datasetID.value, name: datasetPreview.value?.name, config: { ...config.value } };
    const signature = JSON.stringify([nsSel.value, body]);
    if (signature !== submissionSignature) { submissionKey = crypto.randomUUID(); submissionSignature = signature; }
    body.request_id = submissionKey;
    let id;
    const origin = epoch;
    const ok = await request("submit", (ns, signal) => evaluationAPI.post("runs", ns, body, signal), data => {
      id = data.id; submissionSignature = ""; liveConfirmed.value = false;
    }, formError);
    if (ok && alive && origin === epoch) {
      const listed = await loadRuns(pollError);
      if (alive && origin === epoch) await openRun(id, true, listed ? error : formError);
    }
    schedule();
  }
  async function runAction(action) {
    if (!run.value || busy.value.action) return;
    const id = run.value.id;
    if (action === "retry" && run.value.config?.mode === "live" && !retryConfirmed.value) {
      error.value = "重试会产生模型调用费用，请先确认真实模型调用"; return;
    }
    retryConfirmed.value = false;
    const origin = epoch;
    const ok = await request("action", (ns, signal) => evaluationAPI.post("runs/" + encodeURIComponent(id) + "/" + action, ns, {}, signal), data => {
      if (selectedRunID === id) run.value = data;
    });
    if (ok && alive && origin === epoch) {
      await loadRuns(pollError);
      if (alive && origin === epoch && selectedRunID === id) await openRun(id, false);
    }
    schedule();
  }
  async function compareRuns() {
    if (!run.value || !compareID.value || run.value.id === compareID.value) { compareError.value = "请选择另一次运行"; return; }
    comparison.value = null;
    await request("compare", (ns, signal) => evaluationAPI.get("compare?left=" + encodeURIComponent(run.value.id) + "&right=" + encodeURIComponent(compareID.value), ns, signal), data => { comparison.value = data; }, compareError);
  }
  async function exportRun(format) {
    if (!run.value || busy.value.export) return;
    const id = run.value.id;
    await request("export", (ns, signal) => evaluationAPI.download(id, format, ns, signal), blob => {
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url; link.download = "evaluation-" + id + "." + format;
      document.body.appendChild(link); link.click(); link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    });
  }
  function reset() {
    epoch++; clearPoll();
    for (const key of requests.keys()) cancelRequest(key);
    capabilities.value = null; datasets.value = []; runs.value = []; run.value = null; items.value = [];
    selectedItem.value = null; selectedRunID = "";
    datasetID.value = ""; datasetPreview.value = null; datasetName.value = ""; datasetContent.value = "";
    validation.value = null; comparison.value = null; compareID.value = ""; liveConfirmed.value = false; retryConfirmed.value = false;
    error.value = ""; formError.value = ""; pollError.value = ""; compareError.value = "";
    submissionKey = ""; submissionSignature = "";
    config.value.mode = "offline"; config.value.judge = false; config.value.closed_book = false;
  }
  watch([datasetName, datasetContent], invalidateValidation, { flush: "sync" });
  watch(() => config.value.mode, mode => {
    liveConfirmed.value = false;
    if (mode === "offline") { config.value.judge = false; config.value.closed_book = false; }
  }, { flush: "sync" });
  watch(compareID, () => { cancelRequest("compare"); comparison.value = null; }, { flush: "sync" });
  watch(nsSel, () => { reset(); if (mounted) void refresh(); }, { flush: "sync" });
  watch(pane, schedule, { flush: "sync" });
  onMounted(() => { mounted = true; return refresh(); });
  onUnmounted(() => { alive = false; mounted = false; reset(); });
  return {
    capabilities, datasets, runs, run, items, selectedItem, filteredItems, itemFilter, progress, isActive,
    error, formError, pollError, busy, datasetName, datasetContent, validation, datasetID,
    datasetPreview, config, liveConfirmed, retryConfirmed, canStart, compareID, comparison, compareError,
    refresh, openRun, readDataset, validateDataset, saveDataset, chooseDataset, startRun,
    runAction, compareRuns, exportRun,
  };
}

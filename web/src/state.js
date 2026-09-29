import { ref, watch, computed } from "vue";
import { requestJSON, jsonPost } from "./api.js";

export const pane = ref("chat");
export const PANES = ["chat", "library", "evals", "engine"];
// 旧六导航 hash 归并到新四页：文档/知识簇是知识库的页签，监控/配置是引擎的页签。
// 旧链不断——外部脚本与收藏里可能还带着 #/documents、#/monitor。
const HASH_FOLD = { documents: "library", ingest: "library", clusters: "library", monitor: "engine", settings: "engine" };
export function paneFromHash(id) {
  const mapped = HASH_FOLD[id] || id;
  return PANES.includes(mapped) ? mapped : "chat";
}
export const nsSel = ref("");
export const buckets = ref([]);
export const bucketsBusy = ref(false);
export const bucketsError = ref("");
export const documents = ref([]);
export const documentsBusy = ref(false);
export const documentsError = ref("");
export const hasBucket = computed(() => buckets.value.some(b => b.value === nsSel.value));
export const libraryLabel = computed(() => buckets.value.find(b => b.value === nsSel.value)?.label || "未选择知识库");
let documentRequest = 0;

export function withNS(url, namespace = nsSel.value) {
  if (!namespace) return url;
  return url + (url.includes("?") ? "&" : "?") + "ns=" + encodeURIComponent(namespace);
}

export async function loadBuckets() {
  bucketsBusy.value = true;
  bucketsError.value = "";
  try {
    const data = await requestJSON("/v1/buckets");
    buckets.value = (data.buckets || []).map(b => ({
      value: b.name, label: b.label || b.name,
      sources: b.sources || 0, clusters: b.clusters || 0, queries: b.queries || 0,
    }));
    if (!buckets.value.some(b => b.value === nsSel.value)) {
      nsSel.value = buckets.value.find(b => b.value === data.default_ns)?.value || buckets.value[0]?.value || "";
    }
  } catch (error) { bucketsError.value = error.message; }
  finally { bucketsBusy.value = false; }
}

export async function createBucket(name, label) {
  const created = await requestJSON("/v1/buckets", jsonPost({ name: name.trim(), label: label.trim() }));
  await loadBuckets();
  if (bucketsError.value) throw new Error("知识库已创建，但刷新失败：" + bucketsError.value);
  nsSel.value = created.name;
  pane.value = "library";
}

export async function loadDocuments() {
  const namespace = nsSel.value;
  const request = ++documentRequest;
  documentsError.value = "";
  if (!namespace) { documents.value = []; documentsBusy.value = false; return; }
  documentsBusy.value = true;
  try {
    const data = await requestJSON(withNS("/v1/sources", namespace));
    if (request === documentRequest && namespace === nsSel.value) documents.value = data.sources || [];
  } catch (error) {
    if (request === documentRequest && namespace === nsSel.value) documentsError.value = error.message;
  } finally {
    if (request === documentRequest && namespace === nsSel.value) documentsBusy.value = false;
  }
}

watch(nsSel, () => {
  documentRequest++;
  documents.value = [];
  documentsError.value = "";
  documentsBusy.value = false;
}, { flush: "sync" });

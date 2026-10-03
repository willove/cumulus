import { ref, watch, computed } from "vue";
import { requestJSON, jsonPost } from "./api.js";

export const pane = ref("chat");
export const PANES = ["chat", "corpus", "knowledge", "evals", "engine"];
// 旧 hash 一律折叠到新五页，外链与收藏不断。
// v3 是四页：library 一页扛着 概览/文档/测试一问/学得知识 四个页签，也就是四个主操作，
// 而 ui-v3-design.md 自己写的是「每屏主操作 ≤1」。v4 按心智模型拆开——
// 语料（入库流水线）、知识（簇与复核）、原始检索归到引擎（它和「测试端点」同类：
// 拿真实接口打一枪看回什么）。
const HASH_FOLD = {
  documents: "corpus", ingest: "corpus", library: "corpus",
  clusters: "knowledge",
  monitor: "engine", settings: "engine", test: "engine",
};
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
  pane.value = "corpus";
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

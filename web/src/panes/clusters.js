// 知识簇面板逻辑：列表 + 详情（cites 证据边）+ 复核动作。
//
// 「知识簇是什么」是这个面板最大的困惑点，所以 expose 一层解释文案和数据
// 口径：簇 = 已答问题的主题归档，命中复用时是毫秒级、零 token 的；emerging
// （待复核）= 簇的证据窗口还没对当前语料复核过。复核入口 POST review 会把
// 每个窗口对当前原文逐一核对，通过则升 stable。
import { ref, onMounted } from "vue";
import { withNS } from "../state.js";
import { requestJSON, jsonPost } from "../api.js";

export function useClustersPane() {
  const clusters = ref([]);
  const clusterCur = ref(null);
  const clusterLoading = ref(false);
  const clusterError = ref("");
  const reviewBusy = ref(false);
  const reviewResult = ref(null);
  let detailRequest = 0;

  async function loadClusters() {
    clusterLoading.value = true;
    clusterError.value = "";
    try {
      const r = await requestJSON(withNS("/v1/clusters?limit=200"));
      clusters.value = r.clusters || [];
    } catch (error) { clusterError.value = error.message; }
    finally { clusterLoading.value = false; }
  }

  async function openCluster(id) {
    const request = ++detailRequest;
    clusterCur.value = null;
    clusterError.value = "";
    reviewResult.value = null;
    try {
      const data = await requestJSON(withNS("/v1/clusters/" + encodeURIComponent(id)));
      if (request === detailRequest) clusterCur.value = data;
    } catch (error) { if (request === detailRequest) clusterError.value = error.message; }
  }

  // 复核：把簇的每个证据窗口对当前原文核对一遍。valid → 升 stable；
  // 否则留在 emerging 并带回逐窗原因（哪个源变了/没了）。
  async function reviewCluster(id) {
    if (!id || reviewBusy.value) return null;
    reviewBusy.value = true;
    reviewResult.value = null;
    try {
      const d = await requestJSON(withNS("/v1/clusters/" + encodeURIComponent(id) + "/review"),
        jsonPost({}));
      reviewResult.value = d;
      // 复核通过会改生命周期：本地同步，避免列表标签和详情不一致。
      const cid = (c) => c._id || c.id;
      const hit = clusters.value.find((c) => cid(c) === id);
      if (hit && d.lifecycle) hit.lifecycle = d.lifecycle;
      if (clusterCur.value?.cluster && d.lifecycle) clusterCur.value.cluster.lifecycle = d.lifecycle;
      return d;
    } catch (error) {
      reviewResult.value = { error: error.message };
      return null;
    } finally { reviewBusy.value = false; }
  }

  function resetForNs() {
    clusterCur.value = null;
    reviewResult.value = null;
    loadClusters();
  }

  onMounted(loadClusters);

  return {
    clusters, clusterCur, clusterLoading, clusterError,
    reviewBusy, reviewResult, loadClusters, openCluster, reviewCluster, resetForNs,
  };
}

export const lifecycleLabel = { emerging: "待复核", stable: "稳定", contested: "有争议", deprecated: "已退役" };

// 面板口径：总数 / 各生命周期计数，顶部说明条直接用它。
export function clusterStats(list) {
  const stats = { total: list.length, emerging: 0, stable: 0, contested: 0, deprecated: 0 };
  for (const c of list) {
    if (c.lifecycle in stats) stats[c.lifecycle] += 1;
  }
  return stats;
}

// 知识簇面板逻辑：列表 + 详情（cites 证据边）。逻辑从 v2 原样迁出。
import { ref, onMounted } from "vue";
import { withNS } from "../state.js";
import { requestJSON } from "../api.js";

export function useClustersPane() {
  const clusters = ref([]);
  const clusterCur = ref(null);
  const clusterLoading = ref(false);
  const clusterError = ref("");
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
    try {
      const data = await requestJSON(withNS("/v1/clusters/" + encodeURIComponent(id)));
      if (request === detailRequest) clusterCur.value = data;
    } catch (error) { if (request === detailRequest) clusterError.value = error.message; }
  }

  function resetForNs() {
    clusterCur.value = null;
    loadClusters();
  }

  onMounted(loadClusters);

  return { clusters, clusterCur, clusterLoading, clusterError, loadClusters, openCluster, resetForNs };
}

export const lifecycleLabel = { emerging: "待复核", stable: "稳定", contested: "有争议", deprecated: "已退役" };

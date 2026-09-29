// 知识库页的概览聚合与库管理动作。一个库的完整生命周期状态在这里一屏读出：
// 语料规模（documents 来自 state.js）、学得物计数（/v1/learning）、最近一次评测
// （/v1/eval/runs 首条）。管理动作只有两个破坏性写：清空学得物重学
// （confirm 必须逐字等于库名，服务端同门禁）与注销知识库。
import { ref } from "vue";
import { nsSel, buckets, loadBuckets, withNS } from "../state.js";
import { requestJSON } from "../api.js";

export function useLibraryPane() {
  const learning = ref(null);
  const learningBusy = ref(false);
  const lastRun = ref(null);
  const overviewError = ref("");
  const resetBusy = ref(false);
  const resetReport = ref(null);
  const resetError = ref("");
  const deleteBusy = ref(false);
  const deleteError = ref("");

  async function loadOverview() {
    if (!nsSel.value) { learning.value = null; lastRun.value = null; return; }
    learningBusy.value = true;
    overviewError.value = "";
    const ns = nsSel.value;
    try {
      const [learningData, runsData] = await Promise.all([
        requestJSON(withNS("/v1/learning", ns)),
        requestJSON(withNS("/v1/eval/runs", ns)).catch(() => null),
      ]);
      if (nsSel.value !== ns) return; // 切库竞态：晚到的旧库数据不上屏
      learning.value = learningData;
      const runs = runsData?.runs || [];
      lastRun.value = runs.length ? runs[0] : null;
    } catch (error) {
      if (nsSel.value === ns) {
        overviewError.value = error.message;
        learning.value = null;
        lastRun.value = null;
      }
    } finally { learningBusy.value = false; }
  }

  // 重学：清空该库学得物（簇/证据/账本/会话），语料不动。服务端要求
  // confirm 逐字等于库名——这里不代填、不默认，调用方必须拿到用户输入。
  async function resetLearned(confirmName) {
    if (!nsSel.value || resetBusy.value) return false;
    resetBusy.value = true;
    resetError.value = "";
    resetReport.value = null;
    const ns = nsSel.value;
    try {
      const report = await requestJSON(withNS("/v1/learning/reset", ns), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ confirm: confirmName }),
      });
      if (nsSel.value !== ns) return true;
      resetReport.value = report;
      await loadOverview();
      return true;
    } catch (error) {
      resetError.value = error.message;
      return false;
    } finally { resetBusy.value = false; }
  }

  // 注销知识库：成功后切到剩下的第一个库（没有则清空选择，各页回落空态）。
  async function removeBucket() {
    if (!nsSel.value || deleteBusy.value) return false;
    deleteBusy.value = true;
    deleteError.value = "";
    const ns = nsSel.value;
    try {
      await requestJSON(`/v1/buckets/${encodeURIComponent(ns)}`, { method: "DELETE" });
      await loadBuckets();
      if (nsSel.value === ns) {
        nsSel.value = buckets.value[0]?.value || "";
      }
      return true;
    } catch (error) {
      deleteError.value = error.message;
      return false;
    } finally { deleteBusy.value = false; }
  }

  function clearResetFeedback() { resetReport.value = null; resetError.value = ""; }

  return { learning, learningBusy, lastRun, overviewError,
    resetBusy, resetReport, resetError, resetLearned, clearResetFeedback,
    deleteBusy, deleteError, removeBucket, loadOverview };
}

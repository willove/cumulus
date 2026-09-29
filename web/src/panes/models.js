// 模型配置档案 + 消费台账的前端状态。多条端点配置，激活一条（物化 .env 热生效），
// 每次检索的 tokens 按模型落持久台账。key 只上行不下行——编辑时空 = 不改。
import { ref, onMounted } from "vue";
import { requestJSON, jsonPost } from "../api.js";

const profiles = ref([]);
const activeProfile = ref("");
const modelsBusy = ref(false);
const modelsError = ref("");
const saveBusy = ref(false);
const activateBusy = ref("");
const testReport = ref(null);   // { ok, model, latency_ms, tokens, answer } 或 { error }
const testBusy = ref(false);

const usageRecords = ref([]);
const usageScanned = ref(0);
const usageBusy = ref(false);
const usageError = ref("");

export function useModelsPane() {

  async function loadModels() {
    modelsBusy.value = true;
    modelsError.value = "";
    try {
      const d = await requestJSON("/v1/models");
      profiles.value = d.profiles || [];
      activeProfile.value = d.active || "";
    } catch (error) { modelsError.value = error.message; }
    finally { modelsBusy.value = false; }
  }

  async function saveProfile(p) {
    saveBusy.value = true;
    try {
      await requestJSON("/v1/models", jsonPost(p));
      await loadModels();
      return true;
    } catch (error) {
      modelsError.value = error.message;
      return false;
    } finally { saveBusy.value = false; }
  }

  async function activateProfile(id) {
    activateBusy.value = id;
    modelsError.value = "";
    try {
      await requestJSON(`/v1/models/${encodeURIComponent(id)}/activate`, { method: "POST" });
      await loadModels();
      return true;
    } catch (error) {
      modelsError.value = error.message;
      return false;
    } finally { activateBusy.value = ""; }
  }

  async function removeProfile(id) {
    modelsError.value = "";
    try {
      await requestJSON(`/v1/models/${encodeURIComponent(id)}`, { method: "DELETE" });
      await loadModels();
      return true;
    } catch (error) {
      modelsError.value = error.message;
      return false;
    }
  }

  // 按档案试连（激活前验证）：未激活档案也测得到。
  async function testProfile(id) {
    testBusy.value = true;
    testReport.value = null;
    try {
      testReport.value = await requestJSON("/v1/config/test", jsonPost({ profile_id: id }));
    } catch (error) {
      testReport.value = { ok: false, error: error.message };
    } finally { testBusy.value = false; }
  }

  async function loadUsage(model = "") {
    usageBusy.value = true;
    usageError.value = "";
    try {
      const q = model ? `?model=${encodeURIComponent(model)}&limit=200` : "?limit=200";
      const d = await requestJSON("/v1/usage" + q);
      usageRecords.value = d.records || [];
      usageScanned.value = d.scanned || 0;
    } catch (error) { usageError.value = error.message; }
    finally { usageBusy.value = false; }
  }

  onMounted(() => { loadModels(); loadUsage(); });

  return { profiles, activeProfile, modelsBusy, modelsError, saveBusy, activateBusy,
    testReport, testBusy, loadModels, saveProfile, activateProfile, removeProfile, testProfile,
    usageRecords, usageScanned, usageBusy, usageError, loadUsage };
}

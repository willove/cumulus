// 配置面板逻辑：权重状态/下载/验证 + 脱敏端点配置。逻辑从 v2 原样迁出。
import { ref, onMounted, onUnmounted } from "vue";
import { requestJSON } from "../api.js";

export function useSettingsPane() {
  const settingsModel = ref(null);
  const settingsConfig = ref(null);
  const settingsBusy = ref(false);
  const settingsMsg = ref("");
  let timer = null;

  async function loadSettings() {
    try {
      settingsModel.value = await requestJSON("/v1/model");
      settingsConfig.value = await requestJSON("/v1/config");
      if (settingsModel.value && settingsModel.value.installing) pollSettings();
    } catch (error) { settingsMsg.value = "加载失败：" + error.message; }
  }

  async function installWeights() {
    settingsBusy.value = true; settingsMsg.value = "提交下载……";
    try {
      const d = await requestJSON("/v1/model", { method: "POST" });
      settingsMsg.value = d.installed ? "权重已安装" : "下载中（约 464MB，魔搭社区）……";
      pollSettings();
    } catch (e) { settingsMsg.value = "出错：" + e.message; }
    settingsBusy.value = false;
  }

  async function verifyWeights() {
    settingsBusy.value = true; settingsMsg.value = "加载并运行权重……";
    try {
      const d = await requestJSON("/v1/model/verify", { method: "POST" });
      settingsMsg.value = "验证通过：" + d.dims + " 维 · " + d.ms + "ms";
    } catch (e) { settingsMsg.value = "验证失败：" + e.message; }
    settingsBusy.value = false;
  }

  function pollSettings() {
    if (timer) return;
    timer = setInterval(async () => {
      try { settingsModel.value = await requestJSON("/v1/model"); }
      catch (error) { settingsMsg.value = "状态读取失败：" + error.message; }
      if (settingsModel.value && !settingsModel.value.installing) {
        clearInterval(timer); timer = null;
        const m = settingsModel.value;
        settingsMsg.value = m.error ? ("下载失败：" + m.error) : (m.installed ? "权重已安装" : settingsMsg.value);
      }
    }, 1500);
  }

  onUnmounted(() => { if (timer) { clearInterval(timer); timer = null; } });
  onMounted(loadSettings);

  return { settingsModel, settingsConfig, settingsBusy, settingsMsg, installWeights, verifyWeights, fmtMB: (b) => (b / 1e6).toFixed(1) + " MB" };
}

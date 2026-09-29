// 配置面板逻辑：权重状态/下载/验证 + 端点与模型的读写配置。
//
// 两条硬规矩：
//   1. 密钥只上行不下行——GET /v1/config 只回 api_key_set/api_key_len，表单里
//      留空即「不修改」，所以前端永不持有密钥明文（Sirchmunk 把真 key 回显到
//      password 输入框，只是视觉掩码；这里不照抄）。
//   2. 验证报告不再压成一行字符串：dims/ms/norm/probe 分开呈现，并写清这次验证
//      证明了什么、没证明什么。
import { ref, onMounted } from "vue";
import { requestJSON, jsonPost } from "../api.js";

// 状态为模块级单例：引擎页把配置与本地权重拆成了两个页签，两处必须共享
// 同一份表单/权重状态——否则在「服务配置」里保存后切到「本地模型」又读旧值。
const settingsModel = ref(null);
const settingsConfig = ref(null);
const settingsBusy = ref(false);
const settingsMsg = ref("");
const settingsVerify = ref(null); // { ok, dims, ms, norm, probe } 或 { error }
const settingsTest = ref(null);   // { ok, model, latency_ms, tokens, answer } 或 { error }
const settingsForm = ref({ base_url: "", chat_model: "", embed_model: "", api_key: "", reasoning_split: false });
const settingsSaving = ref(false);
let timer = null;

export function useSettingsPane() {
  function fillForm(config) {
    if (!config) return;
    settingsForm.value = {
      base_url: config.base_url || "",
      chat_model: config.chat_model || "",
      embed_model: config.embed_model || "",
      api_key: "", // 永不自服务端回填
      reasoning_split: !!config.reasoning_split,
    };
  }

  async function loadSettings() {
    try {
      settingsModel.value = await requestJSON("/v1/model");
      settingsConfig.value = await requestJSON("/v1/config");
      fillForm(settingsConfig.value);
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
    settingsBusy.value = true;
    settingsVerify.value = null;
    settingsMsg.value = "正在加载并运行权重（首次加载约需加载 470MB 分片）……";
    try {
      const d = await requestJSON("/v1/model/verify", { method: "POST" });
      settingsVerify.value = d;
      settingsMsg.value = "";
    } catch (e) {
      settingsVerify.value = { ok: false, error: e.message };
      settingsMsg.value = "";
    }
    settingsBusy.value = false;
  }

  // 只提交改动过的字段：未改动的字段留空/缺省即不动，避免把当前值原样写一遍。
  function changedFields() {
    const config = settingsConfig.value || {};
    const form = settingsForm.value;
    const body = {};
    if (form.base_url.trim() !== (config.base_url || "")) body.base_url = form.base_url.trim();
    if (form.chat_model.trim() !== (config.chat_model || "")) body.chat_model = form.chat_model.trim();
    if (form.embed_model.trim() !== (config.embed_model || "")) body.embed_model = form.embed_model.trim();
    if (form.api_key.trim() !== "") body.api_key = form.api_key.trim();
    if (!!form.reasoning_split !== !!config.reasoning_split) body.reasoning_split = !!form.reasoning_split;
    return body;
  }

  async function saveConfig() {
    const body = changedFields();
    if (!Object.keys(body).length) { settingsMsg.value = "没有改动需要保存"; return; }
    settingsSaving.value = true;
    settingsMsg.value = "保存中……";
    try {
      const d = await requestJSON("/v1/config", jsonPost(body));
      settingsConfig.value = await requestJSON("/v1/config");
      fillForm(settingsConfig.value);
      settingsTest.value = null;
      settingsMsg.value = "已保存 " + (d.saved || []).length + " 项到 " + d.env_file + "；对当前进程立即生效，下次检索即用新配置。";
    } catch (e) { settingsMsg.value = "保存失败：" + e.message; }
    settingsSaving.value = false;
  }

  async function testConnection() {
    settingsBusy.value = true;
    settingsTest.value = null;
    settingsMsg.value = "正在用当前配置发起一次最小模型调用……";
    try {
      settingsTest.value = await requestJSON("/v1/config/test", { method: "POST" });
      settingsMsg.value = "";
    } catch (e) {
      // 502 也带结构化报告（ok:false + error），优先展示它而不是丢掉的错误串。
      settingsTest.value = { ok: false, error: e.message };
      settingsMsg.value = "";
    }
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

  onMounted(loadSettings);

  return {
    settingsModel, settingsConfig, settingsBusy, settingsMsg, settingsForm, settingsSaving,
    settingsVerify, settingsTest, saveConfig, testConnection,
    installWeights, verifyWeights, fmtMB: (b) => (b / 1e6).toFixed(1) + " MB",
  };
}
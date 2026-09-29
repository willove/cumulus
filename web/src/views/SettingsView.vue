<template>
  <div class="pane settings-panel">
    <eb-alert v-if="settingsMsg" :title="settingsMsg"
              :type="settingsMsg.includes('失败') || settingsMsg.startsWith('出错') ? 'error' : 'info'"
              :closable="false" show-icon />

    <eb-section-card title="模型端点与密钥">
      <template #extra>
        <eb-status-tag :value="settingsConfig && settingsConfig.offline ? 'offline' : 'online'"
                       :statuses="[{ value: 'online', label: '在线模型', type: 'success' },
                                   { value: 'offline', label: '离线规则模式', type: 'info' }]" />
      </template>
      <eb-form label-position="top">
        <div class="cfg-grid">
          <eb-form-item label="Base URL（OpenAI 兼容端点，含 /v1）">
            <eb-input v-model="settingsForm.base_url" :clearable="false" placeholder="https://api.minimaxi.com/v1" />
          </eb-form-item>
          <eb-form-item :label="'API Key' + (settingsConfig && settingsConfig.api_key_set ? '（已设置 ' + settingsConfig.api_key_len + ' 字符，留空不修改）' : '（未设置）')">
            <eb-input v-model="settingsForm.api_key" type="password" :clearable="false"
                      :placeholder="settingsConfig && settingsConfig.api_key_set ? '留空 = 保持现有密钥' : '粘贴供应商密钥'" />
          </eb-form-item>
          <eb-form-item label="Chat 模型（打分/意图/合成/判官）">
            <eb-input v-model="settingsForm.chat_model" :clearable="false" placeholder="MiniMax-M3" />
          </eb-form-item>
          <eb-form-item label="Embed 模型（留空 = 本地哈希嵌入）">
            <eb-input v-model="settingsForm.embed_model" :clearable="false" placeholder="留空使用本地 embedder" />
          </eb-form-item>
        </div>
        <eb-form-item label="reasoning_split（MiniMax 思维链分离；minimaxi 域自动开启）">
          <eb-switch v-model="settingsForm.reasoning_split" />
        </eb-form-item>
      </eb-form>
      <div class="cfg-actions">
        <eb-button type="primary" :loading="settingsSaving" @click="saveConfig">保存并热生效</eb-button>
        <eb-button :loading="settingsBusy" @click="testConnection">测试连接</eb-button>
        <span class="tiny">密钥只上行不下行：页面永远拿不到已保存的明文；留空即不修改。</span>
      </div>
      <eb-alert v-if="settingsTest" :type="settingsTest.ok ? 'success' : 'error'" :closable="false" show-icon
                :title="settingsTest.ok ? '连接正常' : '连接失败'">
        <div v-if="settingsTest.ok" class="tiny">
          模型 {{ settingsTest.model || 'N/A' }} · 往返 {{ settingsTest.latency_ms }} ms · {{ settingsTest.tokens }} tokens ·
          回复「{{ settingsTest.answer }}」（这一次调用会计费）
        </div>
        <div v-else class="tiny">{{ settingsTest.error }}</div>
      </eb-alert>
    </eb-section-card>

    <eb-section-card title="生效配置">
      <eb-alert v-if="settingsConfig && settingsConfig.embedder_error" type="error"
                :title="'嵌入模型不可用：' + settingsConfig.embedder_error" :closable="false" show-icon />
      <eb-detail-descriptions :column="2" :data="settingsConfig || {}" :items="configItems" border />
    </eb-section-card>
  </div>
</template>

<script setup>
import { useSettingsPane } from "../panes/settings.js";

const { settingsConfig, settingsBusy, settingsMsg, settingsForm, settingsSaving,
  settingsTest, saveConfig, testConnection } = useSettingsPane();

const configItems = [
  { prop: "offline", label: "回答模式", formatter: (v) => (v ? "离线规则模式（不调用远端模型）" : "在线模型") },
  { prop: "effective_embedder", label: "实际嵌入模型", formatter: (v) => v || "—" },
  { prop: "base_url", label: "Base URL", formatter: (v) => v || "未设置（离线桩）" },
  { prop: "chat_model", label: "Chat 模型", formatter: (v) => v || "—" },
  { prop: "embed_model", label: "Embed 模型", formatter: (v) => v || "未设置（本地 embedder）" },
  { prop: "api_key_set", label: "API Key", formatter: (v, row) => (v ? "已设置（" + row.api_key_len + " 字符，不回显）" : "未设置") },
  { prop: "reasoning_split", label: "reasoning_split", formatter: (v) => (v ? "开" : "关") },
  { prop: "minilm_required", label: "MiniLM 严格门", formatter: (v) => (v ? "开（CLUS_MINILM_REQUIRE=1）" : "关") },
];
</script>

<style src="./common.css"></style>
<style>
.settings-panel { display: flex; flex-direction: column; gap: 20px; max-width: 1000px; }
.cfg-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 0 var(--eb-space-4); }
.cfg-actions { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; margin-top: var(--eb-space-2); }
</style>

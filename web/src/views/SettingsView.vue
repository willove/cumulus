<template>
  <div class="pane" style="display: flex; flex-direction: column; gap: 20px; max-width: 960px">
    <header class="page-heading"><div><h2>系统配置</h2><p class="sub">以下为服务启动时的只读配置。修改环境变量后重启服务生效。</p></div></header>
    <eb-alert v-if="settingsMsg" :title="settingsMsg" :type="settingsMsg.includes('失败') || settingsMsg.startsWith('出错') ? 'error' : 'info'" :closable="false" show-icon />
    <eb-section-card title="本地模型权重（MiniLM-L12 · 384 维）">
      <template #extra>
        <eb-status-tag :value="settingsModel && settingsModel.installed ? 'yes' : 'no'"
                       :statuses="[{ value: 'yes', label: '已安装', type: 'success' }, { value: 'no', label: '未安装', type: 'warning' }]" />
      </template>
      <div class="krow"><span>目录</span><b class="num">{{ settingsModel ? settingsModel.dir : "—" }}</b></div>
      <div class="krow" v-if="settingsModel && settingsModel.installed">
        <span>来源</span><b>{{ settingsModel.source }}（{{ settingsModel.model_id }}）</b>
      </div>
      <div class="tiny" style="margin: 8px 0">
        {{ settingsModel && settingsModel.installed
          ? ""
          : "未检测到权重。下载约 485MB（魔搭社区）后自动验证。" }}
      </div>
      <div class="frow2">
        <eb-button v-if="!(settingsModel && settingsModel.installed)" type="primary"
                   :loading="settingsBusy || (settingsModel && settingsModel.installing)" @click="installWeights">下载权重</eb-button>
        <eb-button :loading="settingsBusy" @click="verifyWeights">验证运行</eb-button>
      </div>
      <div class="tiny num" v-if="settingsModel && settingsModel.installing && settingsModel.progress" style="margin-top: 8px">
        {{ settingsModel.progress.file }} · {{ fmtMB(settingsModel.progress.done) }} / {{ fmtMB(settingsModel.progress.total) }}
      </div>
      <p class="tiny">已安装只表示文件可用；是否用于检索取决于服务的嵌入模型配置。</p>
      <template v-if="settingsModel && settingsModel.files && settingsModel.files.length">
        <div class="sub" style="margin: 10px 0 4px">已下载文件</div>
        <div v-for="f in settingsModel.files" :key="f.name" class="krow">
          <span class="num">{{ f.name }}</span><b class="num">{{ fmtMB(f.size) }}</b>
        </div>
      </template>
    </eb-section-card>

    <eb-section-card title="生效配置（只读）">
      <div class="krow"><span>回答模式</span><b>{{ settingsConfig ? (settingsConfig.offline ? '离线规则模式（不调用远端模型）' : '在线模型') : '—' }}</b></div>
      <div class="krow"><span>实际嵌入模型</span><b>{{ settingsConfig?.effective_embedder || '—' }}</b></div>
      <eb-alert v-if="settingsConfig?.embedder_error" type="error" :title="settingsConfig.embedder_error" :closable="false" show-icon />
      <div class="krow"><span>Base URL</span><b class="num">{{ (settingsConfig && settingsConfig.base_url) || "未设置（离线桩）" }}</b></div>
      <div class="krow"><span>Chat 模型</span><b>{{ (settingsConfig && settingsConfig.chat_model) || "—" }}</b></div>
      <div class="krow"><span>Embed 模型</span><b>{{ (settingsConfig && settingsConfig.embed_model) || "未设置（本地 embedder）" }}</b></div>
      <div class="krow"><span>API Key</span><b>{{ settingsConfig && settingsConfig.api_key_set ? "已设置（" + settingsConfig.api_key_len + " 字符）" : "未设置" }}</b></div>
      <div class="krow"><span>reasoning_split</span><b>{{ settingsConfig && settingsConfig.reasoning_split ? "开" : "关" }}</b></div>
      <div class="krow"><span>MiniLM 严格门</span><b>{{ settingsConfig && settingsConfig.minilm_required ? "开（CLUS_MINILM_REQUIRE=1）" : "关" }}</b></div>
    </eb-section-card>
    <eb-section-card title="机制视图">
      <div class="tiny" style="margin-bottom: 10px">检索系统的内部读数，日常检索不需要看。</div>
      <div class="adv-links">
        <eb-button v-for="a in advanced" :key="a.pane" @click="pane = a.pane">{{ a.label }}</eb-button>
      </div>
    </eb-section-card>
  </div>
</template>

<script setup>
import { useSettingsPane } from "../panes/settings.js";
import { pane } from "../state.js";

const { settingsModel, settingsConfig, settingsBusy, settingsMsg, installWeights, verifyWeights, fmtMB } = useSettingsPane();

const advanced = [
  { pane: "clusters", label: "知识簇" },
  { pane: "monitor", label: "监控读数" },
  { pane: "evals", label: "评测记录" },
];
</script>

<style src="./common.css"></style>
<style>
.adv-links { display: flex; gap: var(--eb-space-2); flex-wrap: wrap; }
</style>

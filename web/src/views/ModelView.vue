<template>
  <div class="pane model-panel">
    <eb-alert v-if="settingsMsg" :title="settingsMsg"
              :type="settingsMsg.includes('失败') || settingsMsg.startsWith('出错') ? 'error' : 'info'"
              :closable="false" show-icon />
    <eb-section-card title="本地模型权重（MiniLM-L12 · 384 维）">
      <template #extra>
        <eb-status-tag :value="weightState.value" :statuses="weightState.statuses" />
      </template>
      <div class="krow"><span>目录</span><b class="num">{{ settingsModel ? settingsModel.dir : "—" }}</b></div>
      <div class="krow" v-if="settingsModel && settingsModel.installed">
        <span>来源</span><b>{{ settingsModel.source }}（{{ settingsModel.model_id }}）</b>
      </div>
      <div class="tiny" style="margin: 8px 0">
        {{ settingsModel && settingsModel.installed
          ? "已安装只表示文件可用；是否用于检索取决于「服务配置」里的「实际嵌入模型」。"
          : "未检测到权重。下载约 485MB（魔搭社区）后自动验证。" }}
      </div>
      <div class="frow2">
        <eb-button v-if="!(settingsModel && settingsModel.installed)" type="primary"
                   :loading="settingsBusy || (settingsModel && settingsModel.installing)" @click="installWeights">下载权重</eb-button>
        <eb-button :loading="settingsBusy" @click="verifyWeights">验证运行</eb-button>
      </div>

      <!-- 验证报告：分项给出这次验证证明了什么、没证明什么。 -->
      <div v-if="settingsVerify" class="verify-block" :class="settingsVerify.ok ? 'ok' : 'bad'">
        <div class="verify-head">
          <eb-status-tag :value="settingsVerify.ok ? 'pass' : 'fail'"
                         :statuses="[{ value: 'pass', label: '验证通过', type: 'success' }, { value: 'fail', label: '验证失败', type: 'danger' }]" />
          <span class="tiny" v-if="settingsVerify.ok">
            {{ settingsVerify.dims }} 维 · 用时 {{ settingsVerify.ms }} ms · 向量范数 {{ Number(settingsVerify.norm).toFixed(4) }}
          </span>
        </div>
        <template v-if="settingsVerify.ok">
          <p class="tiny" style="margin: 6px 0 0">
            证明：权重文件能加载、能编码、维度与规范一致（范数贴近 1）。
            不证明：它与远端 embedder 同处一个向量空间，也不代表它正在被检索使用——那取决于「服务配置」里的「实际嵌入模型」。
          </p>
          <details class="tiny" style="margin-top: 6px">
            <summary>探针向量前 4 维（技术细节）</summary>
            <code class="num">[{{ (settingsVerify.probe || []).map((x) => Number(x).toFixed(6)).join(", ") }}]</code>
          </details>
        </template>
        <p v-else class="tiny" style="margin: 6px 0 0">{{ settingsVerify.error }}</p>
      </div>

      <div class="tiny num" v-if="settingsModel && settingsModel.installing && settingsModel.progress" style="margin-top: 8px">
        {{ settingsModel.progress.file }} · {{ fmtMB(settingsModel.progress.done) }} / {{ fmtMB(settingsModel.progress.total) }}
      </div>
      <eb-progress v-if="settingsModel && settingsModel.installing && settingsModel.progress && settingsModel.progress.total"
                   :percentage="Math.min(100, Math.round(settingsModel.progress.done / settingsModel.progress.total * 100))" />
      <template v-if="settingsModel && settingsModel.files && settingsModel.files.length">
        <div class="sub" style="margin: 10px 0 4px">已下载文件</div>
        <div v-for="f in settingsModel.files" :key="f.name" class="krow">
          <span class="num">{{ f.name }}</span><b class="num">{{ fmtMB(f.size) }}</b>
        </div>
      </template>
    </eb-section-card>
  </div>
</template>

<script setup>
import { computed } from "vue";
import { useSettingsPane } from "../panes/settings.js";

const { settingsModel, settingsBusy, settingsMsg, settingsVerify,
  installWeights, verifyWeights, fmtMB } = useSettingsPane();

const weightState = computed(() => {
  const m = settingsModel.value;
  if (!m) return { value: "unknown", statuses: [{ value: "unknown", label: "状态未知", type: "info" }] };
  if (m.installing) return { value: "installing", statuses: [{ value: "installing", label: "下载中", type: "primary" }] };
  if (m.error) return { value: "error", statuses: [{ value: "error", label: "下载失败", type: "danger" }] };
  return m.installed
    ? { value: "yes", statuses: [{ value: "yes", label: "已安装", type: "success" }] }
    : { value: "no", statuses: [{ value: "no", label: "未安装", type: "warning" }] };
});
</script>

<style src="./common.css"></style>
<style>
.model-panel { display: flex; flex-direction: column; gap: 20px; max-width: 1000px; }
.verify-block { margin-top: var(--eb-space-3); padding: var(--eb-space-3); border-radius: var(--eb-radius-base); border: 1px solid var(--eb-border-color-light); }
.verify-block.ok { border-color: var(--eb-color-success-light-5); background: var(--eb-color-success-light-9); }
.verify-block.bad { border-color: var(--eb-color-danger-light-5); background: var(--eb-color-danger-light-9); }
.verify-head { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
</style>

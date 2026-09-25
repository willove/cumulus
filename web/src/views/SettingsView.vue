<template>
  <div class="pane" style="display: flex; flex-direction: column; gap: 20px; max-width: 1000px">
    <header class="page-heading">
      <div>
        <h2>系统配置</h2>
        <p class="sub">查询端点、模型与本地权重。保存即写入 <code>.env</code> 并对当前进程生效——下一次检索就用新配置，不必重启。</p>
      </div>
    </header>
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
          ? "已安装只表示文件可用；是否用于检索取决于上面的「实际嵌入模型」。"
          : "未检测到权重。下载约 485MB（魔搭社区）后自动验证。" }}
      </div>
      <div class="frow2">
        <eb-button v-if="!(settingsModel && settingsModel.installed)" type="primary"
                   :loading="settingsBusy || (settingsModel && settingsModel.installing)" @click="installWeights">下载权重</eb-button>
        <eb-button :loading="settingsBusy" @click="verifyWeights">验证运行</eb-button>
      </div>

      <!-- 验证报告：以前只有页顶一行「验证通过：384 维 · 197ms」，既离按钮远又
           看不出这次验证到底证明了什么。现在就地展开、分项给出。 -->
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
            不证明：它与远端 embedder 同处一个向量空间，也不代表它正在被检索使用——那取决于上面的「实际嵌入模型」。
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

    <eb-section-card title="机制视图">
      <div class="tiny" style="margin-bottom: 10px">检索系统的内部读数，日常检索不需要看。</div>
      <div class="adv-links">
        <eb-button v-for="a in advanced" :key="a.pane" @click="pane = a.pane">
          <eb-icon :name="a.icon" :size="14" style="margin-right: 4px" />{{ a.label }}
        </eb-button>
      </div>
    </eb-section-card>
  </div>
</template>

<script setup>
import { computed } from "vue";
import { useSettingsPane } from "../panes/settings.js";
import { pane } from "../state.js";

const { settingsModel, settingsConfig, settingsBusy, settingsMsg, settingsForm, settingsSaving,
  settingsVerify, settingsTest, saveConfig, testConnection,
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

const advanced = [
  { pane: "clusters", label: "知识簇", icon: "database" },
  { pane: "monitor", label: "监控读数", icon: "dashboard" },
  { pane: "evals", label: "评测记录", icon: "list-check" },
];
</script>

<style src="./common.css"></style>
<style>
.adv-links { display: flex; gap: var(--eb-space-2); flex-wrap: wrap; }
.cfg-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 0 var(--eb-space-4); }
.cfg-actions { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; margin-top: var(--eb-space-2); }
.verify-block { margin-top: var(--eb-space-3); padding: var(--eb-space-3); border-radius: var(--eb-radius-base); border: 1px solid var(--eb-border-color-light); }
.verify-block.ok { border-color: var(--eb-color-success-light-5); background: var(--eb-color-success-light-9); }
.verify-block.bad { border-color: var(--eb-color-danger-light-5); background: var(--eb-color-danger-light-9); }
.verify-head { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
</style>
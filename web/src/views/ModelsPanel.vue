<template>
  <div class="pane models-panel">
    <eb-alert v-if="modelsError" type="error" :title="modelsError" :closable="false" show-icon />
    <eb-alert v-if="settingsMsg && !modelsError" :title="settingsMsg"
              :type="settingsMsg.includes('失败') || settingsMsg.startsWith('出错') ? 'error' : 'info'"
              :closable="false" show-icon />

    <!-- 生效状态一行：当前是谁在服务（激活档案或 env 直配）。 -->
    <div class="eff-row">
      <eb-status-tag :value="settingsConfig && settingsConfig.offline ? 'offline' : 'online'"
                     :statuses="[{ value: 'online', label: '在线模型', type: 'success' },
                                 { value: 'offline', label: '离线规则模式', type: 'info' }]" />
      <span v-if="settingsConfig" class="tiny">
        {{ activeLabel }} · 嵌入 {{ settingsConfig.effective_embedder || '本地' }}<template v-if="settingsConfig.chat_model"> · {{ settingsConfig.chat_model }}</template>
      </span>
      <eb-button link size="small" @click="refreshAll">刷新</eb-button>
    </div>
    <eb-alert v-if="settingsConfig && settingsConfig.embedder_error" type="error"
              :title="'嵌入模型不可用：' + settingsConfig.embedder_error" :closable="false" show-icon />

    <!-- 档案列表：一行一个端点配置，激活徽标 + 动作。 -->
    <div class="profile-list" role="list" aria-label="模型配置档案">
      <div v-if="!profiles.length && !modelsBusy" class="tiny empty-note">还没有模型配置——新建一条，填入端点与密钥。</div>
      <div v-for="p in profiles" :key="p.id" class="profile-row" role="listitem" :class="{ active: p.id === activeProfile }">
        <div class="profile-main">
          <b>{{ p.label || p.id }}</b>
          <eb-tag v-if="p.id === activeProfile" type="success" size="small">使用中</eb-tag>
          <span class="tiny num profile-url">{{ hostOf(p.base_url) }}<template v-if="p.chat_model"> · {{ p.chat_model }}</template></span>
        </div>
        <span class="tiny dim">{{ usedLabel(p) }}</span>
        <div class="profile-actions">
          <eb-button v-if="p.id !== activeProfile" size="small" type="primary" plain :loading="activateBusy === p.id" @click="activateProfile(p.id)">启用</eb-button>
          <eb-button size="small" :loading="testBusy && testReport?.model === p.chat_model" @click="testProfile(p.id)">测试</eb-button>
          <eb-button size="small" @click="startEdit(p)">编辑</eb-button>
          <eb-popconfirm v-if="p.id !== activeProfile" title="删除这条模型配置？" confirm-button-text="删除" confirm-button-type="danger" @confirm="removeProfile(p.id)">
            <eb-button size="small" text type="danger" aria-label="删除配置">删除</eb-button>
          </eb-popconfirm>
        </div>
      </div>
    </div>

    <!-- 试连报告：就近展开，说明这次测试证明了什么。 -->
    <div v-if="testReport" class="verify-block" :class="testReport.ok ? 'ok' : 'bad'">
      <div class="verify-head">
        <eb-status-tag :value="testReport.ok ? 'pass' : 'fail'"
                       :statuses="[{ value: 'pass', label: '连接正常', type: 'success' }, { value: 'fail', label: '连接失败', type: 'danger' }]" />
        <span class="tiny" v-if="testReport.ok">模型 {{ testReport.model || '—' }} · 往返 {{ testReport.latency_ms }} ms · {{ testReport.tokens }} tokens · 回复「{{ testReport.answer }}」（这一次调用会计费）</span>
        <span class="tiny" v-else>{{ testReport.error }}</span>
      </div>
    </div>

    <!-- 编辑/新建：通用配置表单（label 左置，无卡片堆）。 -->
    <eb-form v-if="editing" label-position="left" :label-width="150" class="profile-form" @submit.prevent>
      <h3 class="form-heading">{{ editing.created_at ? '编辑模型配置' : '新建模型配置' }}</h3>
      <eb-form-item label="标识（不可改）">
        <eb-input v-if="!editing.created_at" v-model="editing.id" :clearable="false" placeholder="例如 minimax" />
        <b v-else class="num">{{ editing.id }}</b>
      </eb-form-item>
      <eb-form-item label="名称"><eb-input v-model="editing.label" :clearable="false" placeholder="例如 MiniMax 生产" /></eb-form-item>
      <eb-form-item label="Base URL（含 /v1）"><eb-input v-model="editing.base_url" :clearable="false" placeholder="https://api.example.com/v1" /></eb-form-item>
      <eb-form-item :label="'API Key' + (editing.api_key_set ? '（已设置，留空不修改）' : '（未设置）')">
        <eb-input v-model="editing.api_key" type="password" :clearable="false" :placeholder="editing.api_key_set ? '留空 = 保持现有密钥' : '粘贴供应商密钥'" />
      </eb-form-item>
      <eb-form-item label="Chat 模型"><eb-input v-model="editing.chat_model" :clearable="false" placeholder="例如 MiniMax-M3" /></eb-form-item>
      <eb-form-item label="Embed 模型（空 = 本地）"><eb-input v-model="editing.embed_model" :clearable="false" placeholder="留空使用本地 embedder" /></eb-form-item>
      <eb-form-item label="思维链分离（MiniMax 系）"><eb-switch v-model="editing.reasoning_split" /></eb-form-item>
      <div class="cfg-actions">
        <eb-button type="primary" :loading="saveBusy" :disabled="!editing.id.trim() || !editing.base_url.trim()" @click="save">保存</eb-button>
        <eb-button @click="editing = null">取消</eb-button>
        <span class="tiny">保存后先「测试」再「启用」；启用即写入 .env 并热生效，下次检索就用它。</span>
      </div>
    </eb-form>
    <div v-else class="cfg-actions">
      <eb-button type="primary" plain icon="plus" @click="startEdit(null)">新建模型配置</eb-button>
    </div>

    <!-- 本地模型权重：嵌入侧的本地选项，与端点配置同页收纳。 -->
    <div class="weights-section">
      <div class="form-heading-row">
        <h3 class="form-heading">本地模型权重（MiniLM-L12 · 384 维）</h3>
        <eb-status-tag :value="weightState.value" :statuses="weightState.statuses" />
      </div>
      <div class="krow"><span>目录</span><b class="num">{{ settingsModel ? settingsModel.dir : '—' }}</b></div>
      <div class="tiny" style="margin: 8px 0">
        {{ settingsModel && settingsModel.installed
          ? '已安装只表示文件可用；是否用于检索取决于上面的嵌入配置。'
          : '未检测到权重。下载约 485MB（魔搭社区）后自动验证。' }}
      </div>
      <div class="cfg-actions">
        <eb-button v-if="!(settingsModel && settingsModel.installed)" :loading="settingsBusy || (settingsModel && settingsModel.installing)" @click="installWeights">下载权重</eb-button>
        <eb-button :loading="settingsBusy" @click="verifyWeights">验证运行</eb-button>
      </div>
      <div v-if="settingsVerify" class="verify-block" :class="settingsVerify.ok ? 'ok' : 'bad'">
        <div class="verify-head">
          <eb-status-tag :value="settingsVerify.ok ? 'pass' : 'fail'"
                         :statuses="[{ value: 'pass', label: '验证通过', type: 'success' }, { value: 'fail', label: '验证失败', type: 'danger' }]" />
          <span class="tiny" v-if="settingsVerify.ok">{{ settingsVerify.dims }} 维 · 用时 {{ settingsVerify.ms }} ms · 范数 {{ Number(settingsVerify.norm).toFixed(4) }}</span>
        </div>
        <p class="tiny" style="margin: 6px 0 0" v-if="settingsVerify.ok">证明权重能加载、能编码、维度一致；不证明它正在被检索使用。</p>
        <p class="tiny" style="margin: 6px 0 0" v-else>{{ settingsVerify.error }}</p>
      </div>
      <div class="tiny num" v-if="settingsModel && settingsModel.installing && settingsModel.progress" style="margin-top: 8px">
        {{ settingsModel.progress.file }} · {{ fmtMB(settingsModel.progress.done) }} / {{ fmtMB(settingsModel.progress.total) }}
      </div>
      <eb-progress v-if="settingsModel && settingsModel.installing && settingsModel.progress && settingsModel.progress.total"
                   :percentage="Math.min(100, Math.round(settingsModel.progress.done / settingsModel.progress.total * 100))" />
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from "vue";
import { useModelsPane } from "../panes/models.js";
import { useSettingsPane } from "../panes/settings.js";

const { profiles, activeProfile, modelsBusy, modelsError, saveBusy, activateBusy,
  testReport, testBusy, loadModels, saveProfile, activateProfile, removeProfile, testProfile } = useModelsPane();
const { settingsModel, settingsConfig, settingsBusy, settingsMsg, settingsVerify,
  installWeights, verifyWeights, fmtMB } = useSettingsPane();

const editing = ref(null);

const activeLabel = computed(() => {
  const hit = profiles.value.find(p => p.id === activeProfile.value);
  if (hit) return `使用「${hit.label || hit.id}」`;
  if (settingsConfig.value && settingsConfig.base_url) return "env 直配（未走档案）";
  return "离线";
});

const weightState = computed(() => {
  const m = settingsModel.value;
  if (!m) return { value: "unknown", statuses: [{ value: "unknown", label: "状态未知", type: "info" }] };
  if (m.installing) return { value: "installing", statuses: [{ value: "installing", label: "下载中", type: "primary" }] };
  if (m.error) return { value: "error", statuses: [{ value: "error", label: "下载失败", type: "danger" }] };
  return m.installed
    ? { value: "yes", statuses: [{ value: "yes", label: "已安装", type: "success" }] }
    : { value: "no", statuses: [{ value: "no", label: "未安装", type: "warning" }] };
});

function startEdit(p) {
  if (p) {
    editing.value = { ...p, api_key: "", api_key_set: !!p.api_key_set };
  } else {
    editing.value = { id: "", label: "", base_url: "", chat_model: "", embed_model: "", api_key: "", api_key_set: false, reasoning_split: false };
  }
}
async function save() {
  const e = editing.value;
  const ok = await saveProfile({
    id: e.id.trim(), label: e.label.trim(), base_url: e.base_url.trim(),
    chat_model: e.chat_model.trim(), embed_model: e.embed_model.trim(),
    api_key: e.api_key.trim(), reasoning_split: !!e.reasoning_split,
  });
  if (ok) editing.value = null;
}
function refreshAll() { loadModels(); }
function hostOf(url) { try { return new URL(url).host; } catch { return url; } }
// 零值时间（0001-01-01）不是「启用过」——后端 omitempty 挡不住 Go 零值序列化。
function usedLabel(p) {
  const d = p.last_used_at ? new Date(p.last_used_at) : null;
  if (!d || Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "未启用过";
  return "启用于 " + d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}
</script>

<style src="./common.css"></style>
<style>
.models-panel { display: flex; flex-direction: column; gap: 18px; max-width: 1000px; }
.eff-row { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
.profile-list { display: flex; flex-direction: column; gap: 2px; }
.empty-note { padding: var(--eb-space-4) 0; }
.profile-row { display: flex; align-items: center; gap: var(--eb-space-4); padding: 9px 10px; border-radius: 8px; transition: background-color var(--eb-duration-fast) var(--eb-ease-out); }
.profile-row:hover { background: var(--eb-fill-color-light); }
.profile-row.active { background: var(--eb-fill-color); }
.profile-main { display: flex; align-items: center; gap: var(--eb-space-2); flex: 1; min-width: 0; }
.profile-url { color: var(--eb-text-color-placeholder); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.profile-actions { display: flex; gap: var(--eb-space-2); flex: none; }
.dim { color: var(--eb-text-color-placeholder); flex: none; }
.profile-form { border-top: 1px solid var(--eb-border-color-lighter); padding-top: var(--eb-space-4); }
.form-heading { margin: 0 0 var(--eb-space-3); font-size: 14px; font-weight: 600; }
.cfg-actions { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; margin-top: var(--eb-space-2); }
.weights-section { border-top: 1px solid var(--eb-border-color-lighter); padding-top: var(--eb-space-4); display: flex; flex-direction: column; gap: 4px; }
.form-heading-row { display: flex; align-items: center; justify-content: space-between; gap: var(--eb-space-3); }
.verify-block { margin-top: var(--eb-space-3); padding: var(--eb-space-3); border-radius: var(--eb-radius-base); border: 1px solid var(--eb-border-color-light); }
.verify-block.ok { border-color: var(--eb-color-success-light-5); background: var(--eb-color-success-light-9); }
.verify-block.bad { border-color: var(--eb-color-danger-light-5); background: var(--eb-color-danger-light-9); }
.verify-head { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; }
</style>

<template>
  <eb-config-provider :theme-color="primary" :semantic="semantic">
  <eb-app-layout title="Cumulus" logo-text="Cu" :active-menu="pane" :active-title="activeTitle"
                 :is-dark="isDark" :collapsed="collapsed" :style="isDark ? darkNavigation : undefined"
                 @toggle="toggleDark" @update:collapsed="v => collapsed = v">
    <template #menu>
      <eb-menu-item v-for="item in navigation" :key="item.id" :index="item.id">
        <eb-icon :name="item.icon" :size="18" /><span>{{ item.title }}</span>
      </eb-menu-item>
    </template>
    <template #topbar-left><h1 class="topbar-title">{{ activeTitle }}</h1></template>
    <template #topbar-right>
      <div class="topbar-actions">
        <div class="library-selector">
          <span>当前知识库</span>
          <eb-select v-model="nsSel" aria-label="当前知识库" :disabled="bucketsBusy" size="small"
                     :placeholder="bucketsBusy ? '正在加载…' : '请先创建知识库'">
            <eb-option v-for="bucket in buckets" :key="bucket.value" :value="bucket.value" :label="bucket.label" />
          </eb-select>
          <eb-button type="primary" link size="small" @click="createOpen = true">新建库</eb-button>
        </div>
        <ThemePicker :primary="primary" :is-dark="isDark" @change="selectPrimary" @dark-change="setDark" />
      </div>
    </template>
    <div class="app-content">
      <eb-alert v-if="bucketsError" type="error" :closable="false" :title="'无法加载知识库：' + bucketsError" show-icon>
        <eb-button type="primary" link @click="loadBuckets">重试</eb-button>
      </eb-alert>
      <!-- 面板切换用 keyed 容器 + 纯 CSS 入场动画：Vue Transition 的离场推进
           依赖 requestAnimationFrame，webview 被遮挡时 rAF 暂停会让 out-in
           切换永久死锁（实测：后台页签里 hash 切换后主区卡在旧面板）。 -->
      <div :key="pane + '|' + nsSel" class="pane-swap">
        <component :is="views[pane]" @create-library="createOpen = true" />
      </div>
    </div>
  </eb-app-layout>
  <eb-dialog v-model="createOpen" title="新建知识库" :width="480" align-center
             :before-close="beforeCloseCreate" :show-close="!creating"
             :close-on-click-modal="!creating" :close-on-press-escape="!creating"
             @open="createError = ''" @opened="nameInput?.focus()">
    <p class="sub">按主题组织文档。导入、会话和检索只在选中的知识库内进行。</p>
    <form class="form-stack" @submit.prevent="submitBucket">
      <label>知识库标识<eb-input ref="nameInput" v-model="bucketName" aria-label="知识库标识" required placeholder="例如 project-docs" autocomplete="off" /></label>
      <label>显示名称（可选）<eb-input v-model="bucketLabel" aria-label="显示名称（可选）" placeholder="例如 项目文档" /></label>
      <eb-alert v-if="createError" type="error" :title="createError" :closable="false" show-icon />
      <div class="form-actions"><eb-button native-type="submit" type="primary" :loading="creating" :disabled="!bucketName.trim()">创建并添加文档</eb-button><eb-button :disabled="creating" @click="createOpen = false">取消</eb-button></div>
    </form>
  </eb-dialog>
  </eb-config-provider>
</template>

<script setup>
import { computed, watch, ref, onMounted, onUnmounted, provide } from "vue";
import { useAppearance } from "./theme.js";
import ThemePicker from "./views/ThemePicker.vue";
import { pane, paneFromHash, nsSel, buckets, bucketsBusy, bucketsError, loadBuckets, createBucket } from "./state.js";
import "./views/common.css";
import ChatView from "./views/ChatView.vue";
import CorpusView from "./views/CorpusView.vue";
import KnowledgeView from "./views/KnowledgeView.vue";
import EvalsView from "./views/EvalsView.vue";
import EngineView from "./views/EngineView.vue";

// 五页，每页一个主操作（ui-v3-design.md「每屏主操作 ≤1」，v3 的 library 一页
// 扛四个页签时是违反的）：问 / 喂 / 管知识 / 验 / 看引擎。
const views = { chat: ChatView, corpus: CorpusView, knowledge: KnowledgeView, evals: EvalsView, engine: EngineView };
const navigation = [
  { id: "chat", title: "检索问答", icon: "question-answer" },
  // 这页实质是文档列表管理，导航跟内容叫「文档」；pane id 仍是 corpus（旧收藏链接不断）。
  { id: "corpus", title: "文档", icon: "database" },
  { id: "knowledge", title: "知识", icon: "book-open" },
  { id: "evals", title: "评测", icon: "bar-chart-h" },
  { id: "engine", title: "引擎", icon: "dashboard" },
];
const { primary, semantic, isDark, setDark, toggleDark, selectPrimary } = useAppearance();
// 库的深色导航固定为蓝色，改为关联同一套运行时主色阶。
const darkNavigation = { "--eb-sidebar-text-active": "var(--eb-color-primary-dark-2)", "--eb-sidebar-active-bg": "var(--eb-color-primary-light-9)" };
const collapsed = ref(false);
const createOpen = ref(false);
const creating = ref(false);
const createError = ref("");
const bucketName = ref("");
const bucketLabel = ref("");
const nameInput = ref(null);
function beforeCloseCreate(done) { if (!creating.value) done(); }
async function submitBucket() {
  if (creating.value || !bucketName.value.trim()) return;
  creating.value = true;
  createError.value = "";
  try {
    await createBucket(bucketName.value, bucketLabel.value);
    createOpen.value = false;
    bucketName.value = "";
    bucketLabel.value = "";
  } catch (error) { createError.value = error.message; }
  finally { creating.value = false; }
}
const activeTitle = computed(() => navigation.find(item => item.id === pane.value)?.title || "检索问答");
provide("router", { push(id) { if (Object.hasOwn(views, id)) pane.value = id; } });

function fromHash() {
  const id = location.hash.replace(/^#\//, "").split("/")[0];
  const next = paneFromHash(id);
  pane.value = next;
  // 折叠之后把地址栏也改成规范 id。否则停在语料页时打开 #/documents：面板对了，
  // 地址栏却留着一个已退役的名字，而收藏它就是收藏一个下一版可能不再折叠的 id。
  // 写回会再触发一次 hashchange，那时 id === next，不会成环。
  if (id !== next && location.hash !== "#/" + next) location.hash = "/" + next;
}
watch(pane, id => { if (location.hash !== "#/" + id) location.hash = "/" + id; });
onMounted(() => {
  fromHash();
  window.addEventListener("hashchange", fromHash);
  loadBuckets();
});
onUnmounted(() => window.removeEventListener("hashchange", fromHash));
</script>

<style>
html, body, #app { height: 100%; margin: 0; }
* { box-sizing: border-box; }
.eb-layout__content { padding: 0; overflow: hidden; min-width: 0; }
.eb-layout__content > * { height: 100%; }
/* 侧栏默认宽度收窄一档（库默认 224px）；折叠宽 64 保持库默认。
   theme.css 有"不得定义 --eb-* 令牌值"的门禁，宿主覆盖放这里。 */
:root { --eb-sidebar-width: 200px; }
.app-content .pane-swap { flex: 1; min-height: 0; display: flex; flex-direction: column; }
.app-content .pane-swap > * { flex: 1; min-height: 0; }
.eb-layout__sidebar .eb-menu-item { display: flex; align-items: center; gap: 12px; }
.topbar-title { margin: 0; font-size: 16px; font-weight: 600; color: var(--eb-text-color-primary); white-space: nowrap; }
.topbar-actions, .library-selector { display: flex; align-items: center; gap: var(--eb-space-3); }
.library-selector { color: var(--eb-text-color-secondary); font-size: var(--eb-font-size-xs); }
.library-selector .eb-select { width: 168px; min-width: 0; }
@media (max-width: 760px) {
  .library-selector > span { display: none; }
  .topbar-actions, .library-selector { gap: var(--eb-space-2); }
  .library-selector .eb-select { width: 116px; }
  .eb-layout__topbar { flex-wrap: wrap; height: auto; min-height: var(--eb-header-height); gap: var(--eb-space-2); padding-block: var(--eb-space-2); }
}
</style>

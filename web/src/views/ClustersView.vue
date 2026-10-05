<template>
  <div class="pane">
    <eb-alert v-if="clusterError" type="error" :title="clusterError" :closable="false" show-icon><eb-button type="primary" link @click="loadClusters">重试列表</eb-button></eb-alert>

    <!-- 这个面板过去最大的问题是「不知道它是干什么的」——但解释条/帮助折叠
         本身又成了要收的东西。现在页面自明：列表+生命周期筛选+复核动作，
         「待复核」的含义由复核结果消息就地解释，不再放说明书。 -->
    <div class="split">
      <aside class="rail">
        <div class="rail-head"><span class="rail-title">知识簇 · {{ clusters.length }}</span></div>
        <!-- 生命周期筛选：segmented small+block（等分撑满单行）。原文字钮行
             在 264px 窄栏里会折成两行（"已退役"掉行）。 -->
        <eb-segmented v-model="activeFilter" :options="filterOptions" size="small" block />
        <!-- 列表项此前是无 role 的 div @click：鼠标能用，键盘到不了，读屏也念不出这是
             一个可选项。簇列表是这一页的主入口，所以补 role/tabindex/Enter+Space。 -->
        <div v-for="c in visibleClusters" :key="c._id" class="rail-item" role="button" tabindex="0"
             :aria-pressed="clusterCur && clusterCur.cluster._id === c._id"
             :class="{ cur: clusterCur && clusterCur.cluster._id === c._id }"
             @click="openCluster(c._id)" @keydown.enter.prevent="openCluster(c._id)" @keydown.space.prevent="openCluster(c._id)">
          <span class="t">{{ (c.queries && c.queries[0]) || c.name || c._id }}</span>
          <eb-status-tag :value="c.lifecycle" :statuses="lifecycleStatuses" size="small" />
        </div>
        <div v-if="clusterLoading" class="tiny" style="padding: 0 12px">加载中……</div>
        <div v-else-if="!visibleClusters.length" class="tiny" style="padding: 0 12px">{{ clusters.length ? "该分类下没有簇" : "当前库还没有簇——问过问题后自动生成" }}</div>
      </aside>

      <div>
        <div v-if="!clusterCur" class="empty-guide">
          <h3>{{ clusters.length ? "从左侧选一个知识簇" : "当前库还没有知识簇" }}</h3>
          <eb-button v-if="!clusters.length" type="primary" @click="gotoChat">去检索会话</eb-button>
        </div>
        <template v-else>
          <div class="frow2" style="margin-bottom: 8px">
            <eb-status-tag :value="clusterCur.cluster.lifecycle" :statuses="lifecycleStatuses" />
            <b style="font-size: var(--eb-font-size-md)">{{ clusterCur.cluster.name || clusterCur.cluster._id }}</b>
            <span class="tiny num">v{{ clusterCur.cluster.version }} · 置信度 {{ (clusterCur.cluster.confidence ?? 0).toFixed(2) }} · 热度 {{ (clusterCur.cluster.hotness ?? 0).toFixed(1) }}</span>
          </div>

          <div style="margin: 10px 0">
            <span class="sub">问法：</span>
            <eb-tag v-for="q in (clusterCur.cluster.queries || [])" :key="q" size="small">{{ q }}</eb-tag>
          </div>
          <pre class="pre-block">{{ clusterCur.cluster.content }}</pre>

          <!-- 复核区：待复核的簇给按钮和结论；稳定的簇给「重新复核」（语料会变）。 -->
          <div class="review-row">
            <eb-button size="small" :loading="reviewBusy" @click="reviewCluster(clusterCur.cluster._id)">
              {{ clusterCur.cluster.lifecycle === "emerging" ? "复核这个簇" : "重新复核" }}
            </eb-button>
            <span v-if="reviewResult" class="tiny" :class="reviewResult.valid ? 'ok' : 'warn'">
              {{ reviewResult.error ? "复核失败：" + reviewResult.error
                : reviewResult.valid ? `复核通过：${reviewResult.checked} 个证据窗口与当前原文一致，已转为稳定。`
                : `发现 ${reviewResult.reasons.length} 处对不上的窗口（语料已变）；下次同类问题会自动重新检索自愈。` }}
            </span>
          </div>
          <ul v-if="reviewResult && reviewResult.reasons?.length" class="reasons tiny">
            <li v-for="(r, i) in reviewResult.reasons" :key="i">{{ r }}</li>
          </ul>

          <div style="margin-top: 14px">
            <div class="sub" style="margin-bottom: 6px">依据的原文窗口 · {{ clusterCur.cites.length }}</div>
            <div v-if="!clusterCur.cites.length" class="tiny">暂无 cites 边——复用簇可能不带窗口。</div>
            <div v-else class="cite-cards">
              <article v-for="(e, i) in clusterCur.cites" :key="i" class="evi-card">
                <div class="evi-head">
                  <span class="evi-src num" :title="displaySourceId(e._to)">{{ displaySourceId(e._to) }}</span>
                  <span class="tiny num">{{ e.start }}–{{ e.end }}</span>
                  <span class="evi-score" :style="{ width: scorePct(e.score) }" :title="'证据得分 ' + (e.score ?? 0).toFixed(1)"></span>
                  <span class="tiny num">{{ (e.score ?? 0).toFixed(1) }}</span>
                </div>
              </article>
            </div>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref, watch } from "vue";
import { useClustersPane, lifecycleLabel, clusterStats } from "../panes/clusters.js";
import { displaySourceId } from "../panes/chat.js";
import { pane } from "../state.js";

const { clusters, clusterCur, clusterLoading, clusterError, reviewBusy, reviewResult, loadClusters, openCluster, reviewCluster } = useClustersPane();

function gotoChat() { pane.value = "chat"; }

const lifecycleStatuses = [
  { value: "stable", label: lifecycleLabel.stable, type: "success" },
  { value: "emerging", label: lifecycleLabel.emerging, type: "warning" },
  { value: "contested", label: lifecycleLabel.contested, type: "danger" },
  { value: "deprecated", label: lifecycleLabel.deprecated, type: "info" },
];

// 生命周期口径给「知识」页的概览段（KnowledgeView）复用；本段自身不再渲染统计卡。
const stats = computed(() => clusterStats(clusters.value));
const filterOptions = [
  { label: "全部", value: "" },
  { label: "待复核", value: "emerging" },
  { label: "稳定", value: "stable" },
  { label: "有争议", value: "contested" },
  { label: "已退役", value: "deprecated" },
];
const activeFilter = ref("");
const visibleClusters = computed(() =>
  activeFilter.value ? clusters.value.filter((c) => c.lifecycle === activeFilter.value) : clusters.value);

// 首次加载自动选中一个簇（优先待复核）：右栏详情是这页的主体，空着的
// 「从左侧选一个」等于把最重要的内容挡在第一次点击之后。只补一次，之后
// 选不选、选哪个都归用户。
let autoPicked = false;
watch([clusters, clusterLoading], () => {
  if (autoPicked || clusterLoading.value || clusterCur.value) return;
  const first = visibleClusters.value.find((c) => c.lifecycle === "emerging") || visibleClusters.value[0];
  if (first) { autoPicked = true; openCluster(first._id); }
}, { immediate: true });

// 证据得分 → 强度条宽度百分比（0–10 分制）。
function scorePct(score) { return Math.max(4, Math.min(100, ((score ?? 0) / 10) * 100)) + "%"; }
</script>

<style scoped src="./common.css"></style>
<style scoped>
/* segmented 筛选行贴栏宽，条目留呼吸位后即列表。 */
.rail :deep(.eb-segmented) { width: 100%; margin: 0 0 8px; }
.review-row { display: flex; align-items: center; gap: 10px; margin: 12px 0 4px; }
.review-row .ok { color: var(--eb-color-success); }
.review-row .warn { color: var(--eb-color-warning); }
.reasons { margin: 4px 0 0; padding-left: 18px; color: var(--eb-text-color-secondary); line-height: 1.7; }
.cite-cards { display: flex; flex-direction: column; gap: 6px; max-height: 260px; overflow: auto; }
.evi-card { padding: 8px 10px; border: 1px solid var(--eb-border-color-lighter); border-radius: 8px; }
.evi-head { display: flex; align-items: center; gap: 10px; }
.evi-src { flex: 1; min-width: 0; display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; line-clamp: 2; overflow: hidden; word-break: break-all; }
.evi-score { height: 6px; border-radius: 3px; background: var(--eb-color-primary); opacity: .75; flex: none; max-width: 120px; }
</style>

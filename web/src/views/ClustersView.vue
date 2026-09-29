<template>
  <div class="pane">
    <eb-alert v-if="clusterError" type="error" :title="clusterError" :closable="false" show-icon><eb-button type="primary" link @click="loadClusters">重试列表</eb-button></eb-alert>

    <!-- 这个面板过去最大的问题是「不知道它是干什么的」：口径解释收进
         帮助折叠（默认收起，占屏的是数据不是说明书）。统计数字只在右上
         一行，与概览段口径一致。知识簇 = 问过的问题按主题归档的答案缓存。 -->
    <details class="intro-help">
      <summary>知识簇是什么</summary>
      <div class="intro-help-body">
        <p>系统把问过的问题按主题自动归并：同一个主题再问，直接复用已合成好的答案——<b>毫秒级返回、不再调用模型、不烧 token</b>。每个簇记着它依据的原文窗口，可逐条核对。</p>
        <p class="tiny">待复核 = 该簇的证据窗口还没对当前语料验证过（语料可能已更新）；选中后点「复核」即可以当前原文逐窗校验，通过则转为稳定。</p>
      </div>
    </details>

    <div class="split">
      <aside class="rail">
        <div class="rail-head"><span class="rail-title">知识簇 · {{ clusters.length }}</span></div>
        <!-- 生命周期筛选：segmented small+block（等分撑满单行）。原文字钮行
             在 264px 窄栏里会折成两行（"已退役"掉行）。 -->
        <eb-segmented v-model="activeFilter" :options="filterOptions" size="small" block />
        <div v-for="c in visibleClusters" :key="c._id" class="rail-item" :class="{ cur: clusterCur && clusterCur.cluster._id === c._id }"
             @click="openCluster(c._id)">
          <span class="t">{{ (c.queries && c.queries[0]) || c.name || c._id }}</span>
          <eb-status-tag :value="c.lifecycle" :statuses="lifecycleStatuses" size="small" />
        </div>
        <div v-if="clusterLoading" class="tiny" style="padding: 0 12px">加载中……</div>
        <div v-else-if="!visibleClusters.length" class="tiny" style="padding: 0 12px">{{ clusters.length ? "该分类下没有簇" : "当前库还没有簇——问过问题后自动生成" }}</div>
      </aside>

      <div>
        <div v-if="!clusterCur" class="empty-guide">
          <h3>{{ clusters.length ? "从左侧选一个知识簇" : "当前库还没有知识簇" }}</h3>
          <p v-if="!clusters.length" class="tiny">在会话里提过的问题会被归并成簇；下次同类问题命中簇时毫秒级返回。</p>
          <eb-button v-if="!clusters.length" type="primary" @click="gotoChat">去检索会话</eb-button>
        </div>
        <template v-else>
          <div class="frow2" style="margin-bottom: 8px">
            <eb-status-tag :value="clusterCur.cluster.lifecycle" :statuses="lifecycleStatuses" />
            <b style="font-size: var(--eb-font-size-md)">{{ clusterCur.cluster.name || clusterCur.cluster._id }}</b>
            <span class="tiny num">v{{ clusterCur.cluster.version }} · 置信度 {{ (clusterCur.cluster.confidence ?? 0).toFixed(2) }} · 热度 {{ (clusterCur.cluster.hotness ?? 0).toFixed(1) }}</span>
          </div>

          <div style="margin: 10px 0">
            <span class="sub">问法（命中这些问题时直接复用）：</span>
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
            <div class="sub" style="margin-bottom: 6px">依据的原文窗口（{{ clusterCur.cites.length }}）</div>
            <div v-if="!clusterCur.cites.length" class="tiny">暂无 cites 边——复用簇可能不带窗口。</div>
            <div v-else class="cite-cards">
              <article v-for="(e, i) in clusterCur.cites" :key="i" class="evi-card">
                <div class="evi-head">
                  <span class="evi-src num">{{ e._to }}</span>
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
import { computed, ref } from "vue";
import { useClustersPane, lifecycleLabel, clusterStats } from "../panes/clusters.js";
import { pane } from "../state.js";

const { clusters, clusterCur, clusterLoading, clusterError, reviewBusy, reviewResult, loadClusters, openCluster, reviewCluster } = useClustersPane();

function gotoChat() { pane.value = "chat"; }

const lifecycleStatuses = [
  { value: "stable", label: lifecycleLabel.stable, type: "success" },
  { value: "emerging", label: lifecycleLabel.emerging, type: "warning" },
  { value: "contested", label: lifecycleLabel.contested, type: "danger" },
  { value: "deprecated", label: lifecycleLabel.deprecated, type: "info" },
];

// 生命周期口径给概览段（LibraryView）复用；本段自身不再渲染统计卡。
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

// 证据得分 → 强度条宽度百分比（0–10 分制）。
function scorePct(score) { return Math.max(4, Math.min(100, ((score ?? 0) / 10) * 100)) + "%"; }
</script>

<style scoped src="./common.css"></style>
<style scoped>
.intro-help { border: 1px dashed var(--eb-border-color-lighter); border-radius: 10px; padding: 8px 12px; margin-bottom: 12px; }
.intro-help summary { cursor: pointer; font-size: 13px; font-weight: 600; color: var(--eb-text-color-regular); user-select: none; }
.intro-help-body p { margin: 8px 0 0; font-size: 12.5px; line-height: 1.8; color: var(--eb-text-color-secondary); }
/* segmented 筛选行贴栏宽，条目留呼吸位后即列表。 */
.rail :deep(.eb-segmented) { width: 100%; margin: 0 0 8px; }
.review-row { display: flex; align-items: center; gap: 10px; margin: 12px 0 4px; }
.review-row .ok { color: var(--eb-color-success); }
.review-row .warn { color: var(--eb-color-warning); }
.reasons { margin: 4px 0 0; padding-left: 18px; color: var(--eb-text-color-secondary); line-height: 1.7; }
.cite-cards { display: flex; flex-direction: column; gap: 6px; max-height: 260px; overflow: auto; }
.evi-card { padding: 8px 10px; border: 1px solid var(--eb-border-color-lighter); border-radius: 8px; }
.evi-head { display: flex; align-items: center; gap: 10px; }
.evi-src { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.evi-score { height: 6px; border-radius: 3px; background: var(--eb-color-primary); opacity: .75; flex: none; max-width: 120px; }
</style>

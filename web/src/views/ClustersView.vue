<template>
  <div class="pane">
    <eb-alert v-if="clusterError" type="error" :title="clusterError" :closable="false" show-icon><eb-button type="primary" link @click="loadClusters">重试列表</eb-button></eb-alert>
    <div class="split">
      <aside class="rail">
        <div class="rail-head"><span class="rail-title">知识簇 · {{ clusters.length }}</span></div>
        <div v-for="c in clusters" :key="c._id" class="rail-item" :class="{ cur: clusterCur && clusterCur.cluster._id === c._id }"
             @click="openCluster(c._id)">
          <span class="t">{{ (c.queries && c.queries[0]) || c.name || c._id }}</span>
          <eb-status-tag :value="c.lifecycle" :statuses="lifecycleStatuses" size="small" />
        </div>
        <div v-if="clusterLoading" class="tiny" style="padding: 0 12px">加载中……</div>
        <div v-else-if="!clusters.length" class="tiny" style="padding: 0 12px">当前库还没有簇</div>
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
            <span class="tiny num">v{{ clusterCur.cluster.version }} · conf {{ (clusterCur.cluster.confidence ?? 0).toFixed(2) }} · 热度 {{ (clusterCur.cluster.hotness ?? 0).toFixed(1) }}</span>
          </div>

          <!-- 证据星图：Sirchmunk 没有任何图视图；这里把簇→原文窗口的 cites 边画出来，
               边宽按得分，越粗越强。读图不交互，诚实呈现当前数据形态。 -->
          <div class="graph-wrap" v-if="clusterCur.cites && clusterCur.cites.length">
            <svg viewBox="0 0 340 240" width="100%" height="220" role="img" aria-label="簇到证据窗口的星图">
              <g v-for="(e, i) in graphEdges" :key="i">
                <line :x1="170" :y1="120" :x2="e.x" :y2="e.y"
                      stroke="var(--eb-color-primary)"
                      :stroke-width="e.w" stroke-opacity="0.55" stroke-linecap="round" />
                <circle :cx="e.x" :cy="e.y" r="5" fill="var(--eb-color-primary)" />
                <text :x="e.x + (e.x >= 170 ? 9 : -9)" :y="e.y + 4" :text-anchor="e.x >= 170 ? 'start' : 'end'"
                      font-size="10" fill="var(--eb-text-color-secondary)" class="num">{{ e.label }}</text>
              </g>
              <circle cx="170" cy="120" r="26" fill="var(--eb-color-primary)" fill-opacity="0.12"
                      stroke="var(--eb-color-primary)" stroke-width="1.5" />
              <text x="170" y="116" text-anchor="middle" font-size="10" fill="var(--eb-text-color-secondary)">簇</text>
              <text x="170" y="130" text-anchor="middle" font-size="11" font-weight="600" fill="var(--eb-text-color-primary)">
                {{ (clusterCur.cluster.queries || [clusterCur.cluster._id])[0].slice(0, 8) }}
              </text>
            </svg>
            <div class="tiny" style="padding: 0 6px 4px">边宽 = 证据得分（{{ clusterCur.cites.length }} 条 cites 边）</div>
          </div>

          <div style="margin: 10px 0">
            <span class="sub">问法：</span>
            <eb-tag v-for="q in (clusterCur.cluster.queries || [])" :key="q" size="small">{{ q }}</eb-tag>
          </div>
          <pre class="pre-block">{{ clusterCur.cluster.content }}</pre>

          <div style="margin-top: 14px">
            <div class="sub" style="margin-bottom: 6px">证据边（cites，{{ clusterCur.cites.length }}）</div>
            <div v-if="!clusterCur.cites.length" class="tiny">暂无 cites 边——复用簇可能不带窗口。</div>
            <table v-else class="readtab">
              <tr><th>源</th><th>窗口</th><th>得分</th></tr>
              <tr v-for="(e, i) in clusterCur.cites" :key="i">
                <td class="num" style="max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap">{{ e._to }}</td>
                <td class="num">{{ e.start }}–{{ e.end }}</td>
                <td class="num">{{ (e.score ?? 0).toFixed(1) }}</td>
              </tr>
            </table>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed } from "vue";
import { useClustersPane, lifecycleLabel } from "../panes/clusters.js";
import { pane } from "../state.js";

const { clusters, clusterCur, clusterLoading, clusterError, loadClusters, openCluster } = useClustersPane();

function gotoChat() { pane.value = "chat"; }

const lifecycleStatuses = [
  { value: "stable", label: lifecycleLabel.stable, type: "success" },
  { value: "emerging", label: lifecycleLabel.emerging, type: "warning" },
  { value: "contested", label: lifecycleLabel.contested, type: "danger" },
  { value: "deprecated", label: lifecycleLabel.deprecated, type: "info" },
];

// 星图几何：cites 均匀铺开在圆周上，边宽随得分 1–5px。
const graphEdges = computed(() => {
  const cites = clusterCur.value?.cites || [];
  const n = cites.length;
  return cites.map((e, i) => {
    const ang = (2 * Math.PI * i) / n - Math.PI / 2;
    const R = 96;
    return {
      x: 170 + R * Math.cos(ang),
      y: 120 + R * Math.sin(ang),
      w: Math.max(1, Math.min(5, (e.score ?? 0) / 2)),
      label: (e.start ?? 0) + "–" + (e.end ?? 0),
    };
  });
});
</script>

<style src="./common.css"></style>

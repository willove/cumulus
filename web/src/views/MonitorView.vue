<template>
  <div class="pane">
    <div class="page-heading" style="margin-bottom: 20px">
      <div>
        <h2>运行监控</h2>
        <p class="sub">检索与系统指标为整个服务的累计值；知识层为当前知识库。每 5 秒自动刷新。</p>
      </div>
      <eb-button :loading="monBusy" @click="loadMonitor">
        <eb-icon name="refresh" :size="14" style="margin-right: 4px" />刷新
      </eb-button>
    </div>
    <eb-alert v-if="monError" type="error" :title="monError" :closable="false" show-icon />
    <eb-skeleton v-if="!mon" :rows="4" animated />
    <template v-else>
      <!-- hero：一行统计卡，图标 + 大数字，替掉原先的文本行 -->
      <div class="stat-grid" style="margin-bottom: 14px">
        <eb-stat-card v-for="s in heroStats" :key="s.label" v-bind="s" />
      </div>

      <eb-section-card title="检索趋势" style="margin-bottom: 14px">
        <div class="chart-grid">
          <div>
            <div class="chart-cap">最近 {{ recentQueries.length }} 次查询耗时（毫秒，右端最新）</div>
            <eb-chart v-if="recentQueries.length >= 2" :options="latencyOptions" :height="240" aria-label="最近查询耗时" />
            <eb-empty-state v-else icon="line-chart" size="compact" title="还没有查询样本"
                            description="在工作台问一句，这里就会开始画趋势。" />
          </div>
          <div>
            <div class="chart-cap">档位分布（累计）</div>
            <eb-chart v-if="modeData.length" :options="modeOptions" :height="240" aria-label="检索档位分布" />
            <eb-empty-state v-else icon="dashboard" size="compact" title="还没有档位记录" />
          </div>
        </div>
      </eb-section-card>

      <eb-section-card title="命中与质量" style="margin-bottom: 14px">
        <div class="ratio-grid">
          <div class="ratio">
            <div class="ratio-head"><span>重复问题命中缓存</span><b class="num">{{ fmtPct(mon.retrieval.reuse_rate) }}</b></div>
            <eb-progress :percentage="pct(mon.retrieval.reuse_rate)" />
            <div class="tiny">{{ mon.retrieval.reuse_hits }} / {{ mon.queries }} 次查询走了「越问越快」的复用路径</div>
          </div>
          <div class="ratio">
            <div class="ratio-head"><span>平均置信度</span><b class="num">{{ (mon.retrieval.avg_confidence || 0).toFixed(3) }}</b></div>
            <eb-progress :percentage="pct(mon.retrieval.avg_confidence)" />
            <div class="tiny">回答置信度的均值（0–1）</div>
          </div>
          <div class="ratio">
            <div class="ratio-head"><span>平均事实覆盖</span><b class="num">{{ (mon.retrieval.avg_coverage || 0).toFixed(3) }}</b></div>
            <eb-progress :percentage="pct(mon.retrieval.avg_coverage)" />
            <div class="tiny">查询分解出的原子事实被证据覆盖的比例</div>
          </div>
          <div class="ratio">
            <div class="ratio-head"><span>错误率</span><b class="num">{{ fmtPct(errorRate) }}</b></div>
            <eb-progress :percentage="pct(errorRate)" :status="errorRate > 0 ? 'exception' : undefined" />
            <div class="tiny">{{ mon.retrieval.errors }} 次失败 · 拒答 {{ mon.retrieval.refused }} 次</div>
          </div>
        </div>
        <div class="krow" style="margin-top: 10px">
          <span>延迟分位（复用 / 全量）</span>
          <b class="num">{{ fmtUS(mon.retrieval.warm_p50_us) }} / {{ fmtUS(mon.retrieval.cold_p50_us) }} p50</b>
        </div>
        <div class="krow">
          <span>嵌入器</span>
          <b class="num">{{ mon.retrieval.embedder || "未接线" }}</b>
        </div>
        <div class="chips">
          <eb-status-tag v-for="(cnt, m) in (mon.retrieval.by_mode || {})" :key="m" size="small"
                         :value="m" :statuses="[{ value: m, label: tierLabel(m) + ' · ' + cnt, type: 'info' }]" />
        </div>
      </eb-section-card>

      <eb-section-card title="知识层" style="margin-bottom: 14px">
        <div class="stat-grid">
          <eb-stat-card v-for="s in knowledgeStats" :key="s.label" v-bind="s" />
        </div>
        <div class="chart-grid" style="margin-top: 12px">
          <div>
            <div class="chart-cap">簇生命周期</div>
            <eb-chart v-if="lifecycleData.length" :options="lifecycleOptions" :height="220" aria-label="簇生命周期分布" />
            <eb-empty-state v-else icon="database" size="compact" title="当前库还没有知识簇"
                            description="检索命中同主题问题后会自动成簇，同类问题越问越快。" />
          </div>
          <div>
            <div class="chart-cap">平均置信 / 热度</div>
            <div class="ratio" style="margin-top: 12px">
              <div class="ratio-head"><span>平均置信</span><b class="num">{{ (mon.knowledge?.avg_confidence || 0).toFixed(3) }}</b></div>
              <eb-progress :percentage="pct(mon.knowledge?.avg_confidence)" />
            </div>
            <div class="ratio" style="margin-top: 12px">
              <div class="ratio-head"><span>平均热度</span><b class="num">{{ (mon.knowledge?.avg_hotness || 0).toFixed(3) }}</b></div>
              <eb-progress :percentage="pct(mon.knowledge?.avg_hotness)" />
            </div>
            <div class="krow" style="margin-top: 10px">
              <span>待复核 / 争议</span>
              <b class="num">{{ mon.knowledge?.needing_review ?? 0 }} / {{ mon.knowledge?.contested ?? 0 }}</b>
            </div>
          </div>
        </div>
      </eb-section-card>

      <eb-section-card title="系统与存储" style="margin-bottom: 14px">
        <div class="stat-grid">
          <eb-stat-card v-for="s in systemStats" :key="s.label" v-bind="s" />
        </div>
        <div class="krow" style="margin-top: 10px">
          <span>存储目录</span><b class="num tiny">{{ mon.system.store_dir }}</b>
        </div>
        <div class="krow">
          <span>内存占用（Heap / RSS）</span>
          <b class="num">{{ (mon.system?.heap_mb || 0).toFixed(1) }} / {{ (mon.system?.rss_mb || 0).toFixed(1) }} MB</b>
        </div>
      </eb-section-card>

      <eb-section-card title="按知识库" style="margin-bottom: 14px">
        <eb-table v-if="(mon.namespaces || []).length" :data="mon.namespaces" row-key="namespace" aria-label="按知识库统计">
          <eb-table-column prop="namespace" label="知识库" :min-width="140" />
          <eb-table-column prop="queries" label="查询" :width="90" />
          <eb-table-column prop="reuse_hits" label="复用命中" :width="110" />
          <eb-table-column prop="avg_p50_us" label="p50 (µs)" :width="120" />
        </eb-table>
        <eb-empty-state v-else icon="database" size="compact" title="还没有按库的查询记录" />
      </eb-section-card>

      <eb-section-card :title="'最近查询 · ' + recentQueries.length">
        <eb-table v-if="recentQueries.length" :data="recentQueries" row-key="at" :scroll-x="880" aria-label="最近查询">
          <eb-table-column prop="at" label="时间" :width="130">
            <template #default="{ row }"><span class="num">{{ new Date(row.at).toLocaleTimeString() }}</span></template>
          </eb-table-column>
          <eb-table-column prop="namespace" label="知识库" :min-width="120">
            <template #default="{ row }">{{ row.namespace || "（默认）" }}</template>
          </eb-table-column>
          <eb-table-column prop="mode" label="档位" :width="110">
            <template #default="{ row }">
              <eb-status-tag size="small" :value="row.mode"
                             :statuses="[{ value: row.mode, label: tierLabel(row.mode), type: row.mode === 'DEEP' ? 'warning' : 'info' }]" />
            </template>
          </eb-table-column>
          <eb-table-column prop="reused" label="复用" :width="80">
            <template #default="{ row }">
              <eb-icon v-if="row.reused" name="circle-check-filled" :size="14" color="var(--eb-color-success)" />
              <span v-else class="tiny">—</span>
            </template>
          </eb-table-column>
          <eb-table-column prop="confidence" label="置信" :width="90" />
          <eb-table-column prop="coverage" label="覆盖" :width="90" />
          <eb-table-column prop="stop_reason" label="停止原因" :width="130">
            <template #default="{ row }">
              <eb-status-tag v-if="row.stop_reason" size="small" :value="row.stop_reason" :statuses="stopStatuses" />
              <span v-else class="tiny">—</span>
            </template>
          </eb-table-column>
          <eb-table-column prop="latency_ms" label="延迟" :width="110">
            <template #default="{ row }"><span class="num">{{ row.latency_ms }} ms</span></template>
          </eb-table-column>
        </eb-table>
        <eb-empty-state v-else icon="search" size="compact" title="还没有查询" description="每次检索都会在这里留一行。" />
      </eb-section-card>
    </template>
  </div>
</template>

<script setup>
import { computed } from "vue";
import { useMonitorPane } from "../panes/monitor.js";

const { mon, monBusy, monError, loadMonitor } = useMonitorPane();

// 档位与生命周期说人话：FAST/DEEP 与 stable/emerging 是内部名。
function tierLabel(m) {
  return { FAST: "快答", DEEP: "深挖", FILENAME_ONLY: "文件名", CHAT: "闲聊", DOC_SUMMARY: "整篇" }[m] || m || "—";
}
function lifecycleLabel(lc) {
  return { stable: "稳定", emerging: "待复核", contested: "有争议", deprecated: "已退役" }[lc] || lc;
}
const stopStatuses = [
  { value: "sufficient", label: "充分", type: "success" },
  { value: "utility", label: "收益耗尽", type: "warning" },
  { value: "budget", label: "预算", type: "info" },
];

const pct = (v) => Math.max(0, Math.min(100, Math.round((Number(v) || 0) * 100)));
const fmtPct = (v) => pct(v) + "%";
const fmtUS = (us) => (us >= 1000 ? (us / 1000).toFixed(0) + " ms" : (us || 0) + " µs");
const compact = (n) => {
  const v = Number(n) || 0;
  if (v >= 1e9) return (v / 1e9).toFixed(1) + "B";
  if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
  if (v >= 1e3) return (v / 1e3).toFixed(1) + "k";
  return String(v);
};

const recentQueries = computed(() => (mon.value?.recent || []).slice(-20));
const errorRate = computed(() => {
  const total = mon.value?.queries || 0;
  return total ? (mon.value.retrieval.errors || 0) / total : 0;
});

// eb-stat-row 只透传 label/value/icon/type/trend，所以单位拼进 value。
// 数值一律给数字 + suffix：stat-card 的 countUp 会从字符串里抽数字做动画，
// 传 "26%"、"1.3M" 这种拼好的字符串会显示成动画中间值（26% 显示成 21）。
const heroStats = computed(() => {
  const r = mon.value?.retrieval || {};
  const llm = mon.value?.llm || {};
  return [
    { label: "检索请求", value: mon.value?.queries || 0, icon: "search", type: "primary" },
    { label: "复用命中率", value: pct(r.reuse_rate), suffix: "%", icon: "database", type: "success" },
    { label: "全量检索 p50", value: Math.round((r.cold_p50_us || 0) / 1000), suffix: " ms", countUp: false, icon: "clock", type: "warning" },
    { label: "累计 Token", value: Number(llm.tokens) || 0, countUp: false, icon: "line-chart", type: "info" },
  ];
});
const knowledgeStats = computed(() => {
  const k = mon.value?.knowledge || {};
  return [
    { label: "知识簇", value: k.clusters ?? 0, icon: "database", type: "primary" },
    { label: "证据窗口", value: k.evidence_windows ?? 0, countUp: false, icon: "file-text", type: "info" },
    { label: "待复核", value: k.needing_review ?? 0, icon: "list-check", type: k.needing_review ? "warning" : "success" },
    { label: "争议边", value: k.contested ?? 0, icon: "target", type: k.contested ? "danger" : "success" },
  ];
});
const systemStats = computed(() => {
  const s = mon.value?.system || {};
  return [
    { label: "运行时长", value: mon.value?.uptime_sec ?? 0, suffix: " s", countUp: false, icon: "time", type: "info" },
    { label: "Goroutines", value: s.goroutines ?? 0, icon: "cpu", type: "primary" },
    { label: "Heap", value: Math.round(s.heap_mb || 0), suffix: " MB", countUp: false, icon: "dashboard", type: "info" },
    { label: "存储占用", value: Math.round((s.store_files_bytes || 0) / 1e6), suffix: " MB", countUp: false, icon: "hard-drive", type: "warning" },
  ];
});

const modeData = computed(() => Object.entries(mon.value?.retrieval?.by_mode || {})
  .map(([name, value]) => ({ name: tierLabel(name), value })));
const lifecycleData = computed(() => Object.entries(mon.value?.knowledge?.by_lifecycle || {})
  .map(([name, value]) => ({ name: lifecycleLabel(name), value })));

const latencyOptions = computed(() => ({
  type: "line",
  labels: recentQueries.value.map((q) => new Date(q.at).toLocaleTimeString().slice(3)),
  series: [{ name: "延迟 ms", data: recentQueries.value.map((q) => q.latency_ms || Math.round((q.latency_us || 0) / 1000)) }],
  smooth: true,
}));
const modeOptions = computed(() => ({ type: "doughnut", pieData: modeData.value }));
const lifecycleOptions = computed(() => ({ type: "doughnut", pieData: lifecycleData.value }));
</script>

<style src="./common.css"></style>
<style>
.chart-grid { display: grid; grid-template-columns: minmax(0, 1.6fr) minmax(0, 1fr); gap: var(--eb-space-5); }
.chart-cap { font-size: var(--eb-font-size-xs); color: var(--eb-text-color-secondary); margin-bottom: var(--eb-space-2); }
.ratio-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: var(--eb-space-4); }
.ratio-head { display: flex; justify-content: space-between; align-items: baseline; gap: var(--eb-space-2);
  font-size: var(--eb-font-size-sm); color: var(--eb-text-color-regular); margin-bottom: 4px; }
.stat-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: var(--eb-space-3); }
.chips { display: flex; gap: var(--eb-space-2); flex-wrap: wrap; margin-top: var(--eb-space-3); }
@media (max-width: 900px) { .chart-grid { grid-template-columns: 1fr; } }
</style>
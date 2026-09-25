<template>
  <div class="pane">
    <div class="page-heading" style="margin-bottom: 20px"><div><h2>运行监控</h2><p class="sub">检索和系统指标为整个服务的累计值；知识层为当前知识库。每 5 秒刷新。</p></div><eb-button :loading="monBusy" @click="loadMonitor">刷新</eb-button></div>
    <eb-alert v-if="monError" type="error" :title="monError" :closable="false" show-icon />
    <div v-if="!mon" class="tiny">{{ monBusy ? "加载中……" : "暂无监控数据" }}</div>
    <template v-else>
      <!-- hero：重复问题命中（「同类问题越问越快」的 Plain-language 口径） -->
      <eb-section-card title="检索" style="margin-bottom: 14px">
        <div class="hero-band">
          <div class="hero-num">
            <div class="hero-v num">{{ (mon.retrieval.reuse_rate * 100).toFixed(0) }}<span class="hero-u">%</span></div>
            <div class="hero-l">重复问题命中缓存 · {{ mon.retrieval.reuse_hits }}/{{ mon.queries }} 次查询</div>
          </div>
          <div class="hero-rest">
            <div class="krow"><span>命中时延 p50</span><b class="num">{{ mon.retrieval.warm_p50_us }} µs</b></div>
            <div class="krow"><span>全量检索 p50</span><b class="num">{{ mon.retrieval.cold_p50_us }} µs</b></div>
            <div class="brow" v-for="(cnt, m) in (mon.retrieval.by_mode || {})" :key="m">
              <span>{{ tierLabel(m) }}</span>
              <div class="bar"><i :style="{ width: modeBarWidth(cnt) }"></i></div>
              <span>{{ cnt }}</span>
            </div>
          </div>
          <div class="hero-spark">
            <svg viewBox="0 0 240 40" width="240" height="40" role="img" aria-label="最近查询延迟">
              <polyline :points="sparkPoints" fill="none" stroke="var(--eb-color-primary)"
                        stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" />
            </svg>
            <div class="tiny" style="text-align: center">最近查询耗时（µs，右新左旧）</div>
          </div>
        </div>
      </eb-section-card>

      <!-- 机制读数：内部状态机计数器，普通用户无需看，默认收起 -->
      <eb-collapse v-model="mechOpen" class="fold-detail" style="margin-bottom: 14px">
        <eb-collapse-item name="mech" title="机制读数">
          <div class="krow"><span>置信 / 覆盖（均值）</span><b class="num">{{ (mon.retrieval?.avg_confidence || 0).toFixed(3) }} / {{ (mon.retrieval?.avg_coverage || 0).toFixed(3) }}</b></div>
          <div class="krow"><span>升级 DEEP / 自纠错 / 拒答 / 错误</span><b class="num">{{ mon.retrieval.escalations }} / {{ mon.retrieval.self_corrected }} / {{ mon.retrieval.refused }} / {{ mon.retrieval.errors }}</b></div>
          <div class="krow"><span>簇总数 / 证据窗</span><b class="num">{{ mon.knowledge?.clusters ?? 0 }} / {{ mon.knowledge?.evidence_windows ?? 0 }}</b></div>
          <div class="krow"><span>平均置信 / 热度</span><b class="num">{{ (mon.knowledge?.avg_confidence || 0).toFixed(3) }} / {{ (mon.knowledge?.avg_hotness || 0).toFixed(3) }}</b></div>
          <div class="krow"><span>待复核 / 争议</span><b class="num">{{ mon.knowledge?.needing_review ?? 0 }} / {{ mon.knowledge?.contested ?? 0 }}</b></div>
        </eb-collapse-item>
      </eb-collapse>

      <!-- 三块读数合成一个面：发丝线分栏，不再三只等宽箱子 -->
      <eb-section-card title="运行读数" style="margin-bottom: 14px; max-width: 960px">
        <div class="trio">
          <div class="trio-col">
            <div class="trio-h">LLM</div>
            <div class="krow"><span>调用数</span><b class="num">{{ mon.llm.calls }}</b></div>
            <div class="krow"><span>Tokens</span><b class="num">{{ mon.llm.tokens }}</b></div>
            <div class="krow"><span>每问 tokens</span><b class="num">{{ (mon.llm?.tokens_per_query || 0).toFixed(1) }}</b></div>
            <div class="krow"><span>每分钟调用</span><b class="num">{{ (mon.llm?.calls_per_min || 0).toFixed(2) }}</b></div>
          </div>
          <div class="trio-col">
            <div class="trio-h">系统</div>
            <div class="krow"><span>Heap / RSS</span><b class="num">{{ (mon.system?.heap_mb || 0).toFixed(1) }} / {{ (mon.system?.rss_mb || 0).toFixed(1) }} MB</b></div>
            <div class="krow"><span>Goroutines / GC</span><b class="num">{{ mon.system.goroutines }} / {{ mon.system.num_gc }}</b></div>
            <div class="krow"><span>存储（上限含预分配）</span><b class="num">{{ ((mon.system?.store_files_bytes || 0) / 1e6).toFixed(0) }} MB</b></div>
            <div class="krow"><span>运行</span><b class="num">{{ mon.uptime_sec }}s</b></div>
            <div class="krow"><span>存储目录</span><b class="num tiny">{{ mon.system.store_dir }}</b></div>
          </div>
          <div class="trio-col">
            <div class="trio-h">知识层</div>
            <div class="brow" v-for="(cnt, lc) in (mon.knowledge?.by_lifecycle || {})" :key="lc">
              <span>{{ lifecycleLabel(lc) }}</span>
              <div class="bar"><i :style="{ width: lcBarWidth(cnt) }"></i></div>
              <span>{{ cnt }}</span>
            </div>
            <div v-if="!Object.keys(mon.knowledge?.by_lifecycle || {}).length" class="tiny">暂无簇</div>
          </div>
        </div>
      </eb-section-card>

      <eb-section-card title="按 bucket" style="margin-top: 14px; max-width: 960px">
        <table class="readtab">
          <tr><th>bucket</th><th>查询</th><th>复用</th><th>p50</th></tr>
          <tr v-for="n in (mon.namespaces || [])" :key="n.namespace">
            <td>{{ n.namespace || "（默认）" }}</td>
            <td class="num">{{ n.queries }}</td>
            <td class="num">{{ n.reuse_hits }}</td>
            <td class="num">{{ n.avg_p50_us }} µs</td>
          </tr>
        </table>
        <div v-if="!(mon.namespaces || []).length" class="tiny">暂无查询记录</div>
      </eb-section-card>

      <!-- 明细默认折叠：首屏只留 hero 指标 + 运行读数，最近查询是排查时才看的东西 -->
      <eb-collapse v-model="recentOpen" class="fold-detail" style="margin-top: 14px; max-width: 960px">
        <eb-collapse-item name="recent" :title="'最近查询 · ' + ((mon.recent || []).length)">
          <table class="readtab">
            <tr><th>时间</th><th>bucket</th><th>档位</th><th>复用</th><th>置信</th><th>覆盖</th><th>采样</th><th>停止</th><th>延迟</th></tr>
            <tr v-for="(q, i) in mon.recent" :key="i">
              <td class="num">{{ new Date(q.at).toLocaleTimeString() }}</td>
              <td>{{ q.namespace || "（默认）" }}</td>
              <td>{{ q.mode }}</td>
              <td>{{ q.reused ? "✓" : "" }}</td>
              <td class="num">{{ (q.confidence || 0).toFixed(2) }}</td>
              <td class="num">{{ (q.coverage || 0).toFixed(2) }}</td>
              <td class="num">{{ q.samples }}</td>
              <td><eb-status-tag v-if="q.stop_reason" :value="q.stop_reason" :statuses="stopStatuses" size="small" /></td>
              <td class="num">{{ q.latency_us }} µs</td>
            </tr>
          </table>
        </eb-collapse-item>
      </eb-collapse>
    </template>
  </div>
</template>

<script setup>
import { computed, ref } from "vue";
import { useMonitorPane } from "../panes/monitor.js";

const { mon, monBusy, monError, loadMonitor } = useMonitorPane();

const recentOpen = ref([]);
const mechOpen = ref([]);

// 档位与生命周期说人话：FAST/DEEP 与 stable/emerging 是内部名。
function tierLabel(m) { return m === "FAST" ? "快答" : m === "DEEP" ? "深挖" : m || "—"; }
function lifecycleLabel(lc) {
  return { stable: "稳定", emerging: "待复核", contested: "有争议", deprecated: "已退役" }[lc] || lc;
}

const stopStatuses = [
  { value: "sufficient", label: "充分", type: "success" },
  { value: "utility", label: "收益耗尽", type: "warning" },
  { value: "budget", label: "预算", type: "info" },
];

const maxMode = computed(() => Math.max(1, ...Object.values(mon.value?.retrieval?.by_mode || {})));
function modeBarWidth(c) { return ((c / maxMode.value) * 100).toFixed(0) + "%"; }
const maxLc = computed(() => Math.max(1, ...Object.values(mon.value?.knowledge?.by_lifecycle || {})));
function lcBarWidth(c) { return ((c / maxLc.value) * 100).toFixed(0) + "%"; }

// 火花线：最近 ≤20 条的 latency_us，右端最新；数量少时不画。
const sparkPoints = computed(() => {
  const xs = (mon.value?.recent || []).map((q) => q.latency_us || 0);
  if (xs.length < 2) return "";
  const max = Math.max(...xs, 1);
  return xs.map((v, i) => {
    const x = (i / (xs.length - 1)) * 236 + 2;
    const y = 36 - (v / max) * 30;
    return x.toFixed(1) + "," + y.toFixed(1);
  }).join(" ");
});
</script>

<style src="./common.css"></style>
<style>
.hero-band { display: flex; gap: 28px; align-items: stretch; flex-wrap: wrap; }
.hero-num { flex: none; min-width: 200px; border-right: 1px solid var(--eb-border-color-extra-light); padding-right: 28px; }
.hero-v { font-size: 64px; font-weight: var(--eb-font-weight-semibold); color: var(--eb-text-color-primary); line-height: 1; letter-spacing: -0.02em; }
.hero-u { font-size: 24px; color: var(--eb-text-color-secondary); margin-left: 4px; font-weight: var(--eb-font-weight-regular); }
.hero-l { font-size: var(--eb-font-size-sm); color: var(--eb-text-color-secondary); margin-top: 8px; }
.hero-rest { flex: 1; min-width: 280px; }
.hero-spark { flex: none; align-self: center; }
.trio { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 0; }
.trio-col { padding: 0 var(--eb-space-5); border-left: 1px solid var(--eb-border-color-extra-light); min-width: 0; }
.trio-col:first-child { border-left: 0; padding-left: 0; }
.trio-h { font-size: var(--eb-font-size-xs); color: var(--eb-text-color-secondary); letter-spacing: .08em;
  margin-bottom: var(--eb-space-2); font-weight: var(--eb-font-weight-medium); }
@media (max-width: 900px) { .trio { grid-template-columns: 1fr; } .trio-col { border-left: 0; padding: 0; } }
</style>

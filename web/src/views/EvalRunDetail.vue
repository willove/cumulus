<template>
  <eb-section-card role="region" aria-labelledby="eval-run-detail-heading">
    <template #header><h3 id="eval-run-detail-heading">运行详情 · {{ run.name || run.id }}</h3></template>
    <div class="run-detail">
      <div class="form-actions"><eb-tag :type="run.state === 'failed' ? 'danger' : run.state === 'completed' ? 'success' : 'info'">{{ run.state }}</eb-tag><eb-tag>{{ run.config?.mode }}</eb-tag><span class="tiny">{{ run.id }} · {{ run.dataset_name }}</span></div>
      <eb-progress :percentage="progress" :status="run.state === 'failed' ? 'exception' : run.state === 'completed' ? 'success' : undefined" aria-label="评测运行进度" />
      <p role="status">总题数 {{ run.total ?? 'N/A' }} · 已处理 {{ run.done ?? 'N/A' }} · 失败 {{ run.failed ?? 'N/A' }} · 当前题 {{ run.current_item || '—' }}</p>
      <eb-alert v-if="run.error" type="error" :title="run.error" :closable="false" show-icon />
      <div class="form-actions">
        <eb-button v-if="active" type="danger" :disabled="run.state === 'cancelling'" :loading="busy.action" @click="$emit('action', 'cancel')">{{ run.state === 'cancelling' ? '正在取消' : '取消运行' }}</eb-button>
        <eb-button v-if="retryable" :loading="busy.action" :disabled="run.config?.mode === 'live' && !liveConfirmed" @click="$emit('action', 'retry')">重试失败题目</eb-button>
        <eb-button @click="protocolOpen = true">查看协议、配置与冻结指纹</eb-button>
        <eb-button v-for="format in ['json', 'jsonl', 'csv']" :key="format" :disabled="busy.export" @click="$emit('export', format)">导出 {{ format.toUpperCase() }}</eb-button>
      </div>
      <eb-checkbox v-if="retryable && run.config?.mode === 'live'" :model-value="liveConfirmed" tabindex="0" @update:model-value="$emit('update:live-confirmed', $event)" @keydown.space.prevent="$emit('update:live-confirmed', !liveConfirmed)">我确认重试真实模型调用会产生额外费用（检索、判官及闭卷基线）</eb-checkbox>
      <div v-if="!active && (retryable || run.state === 'interrupted')" class="form-stack">
        <p class="sub">重试保留已有结果及尝试记录。中断后若服务端拒绝不安全的续跑，请新建冷启动运行，不能绕过快照或运行内状态限制。</p>
        <eb-button @click="$emit('new-run')">新建冷启动评测</eb-button>
      </div>
      <section aria-labelledby="eval-metrics-heading">
        <h3 id="eval-metrics-heading">指标</h3>
        <eb-alert v-if="active" type="info" title="阶段性指标：运行结束前不代表完整题集表现。" :closable="false" />
        <dl class="metrics"><div v-for="metric in metrics" :key="metric.key"><dt class="tiny">{{ metric.label }}</dt><dd class="num">{{ formatMetric(run.summary?.[metric.key], metric.ratio) }}</dd></div></dl>
        <details class="tiny metric-notes">
          <summary>指标口径说明</summary>
          规则匹配不是 EM，也不是判官评分。判官正确率仅以成功判分的 judge_n 为分母；证据命中与引用可解析不等于答案被证据蕴含。延迟为累计毫秒，Token 与延迟包含历史尝试；金额以供应商账单为准。
        </details>
      </section>
      <section aria-labelledby="eval-results-heading" class="run-detail">
        <div class="page-heading"><h3 id="eval-results-heading">逐题结果 · {{ items.length }}</h3><eb-select class="filter-picker" :model-value="itemFilter" aria-label="逐题结果筛选" @update:model-value="$emit('update:item-filter', $event)"><eb-option value="all" label="全部题目" /><eb-option value="failed" label="执行失败" /><eb-option value="wrong" label="规则未匹配" /><eb-option value="missed" label="证据未命中" /></eb-select></div>
        <div class="table-scroll"><eb-table :data="items" row-key="id" :scroll-x="940" aria-label="逐题结果表">
          <eb-table-column prop="query" label="问题 / 详情" min-width="240"><template #default="{ row }"><eb-button link type="primary" class="source-link" :aria-label="`查看题目 ${row.id}`" @click="$emit('select-item', row)">{{ row.id }} · {{ row.query }}</eb-button></template></eb-table-column>
          <eb-table-column prop="state" label="执行状态" width="110" />
          <eb-table-column label="规则匹配" width="110" :formatter="row => verdict(row.rule_match)" />
          <eb-table-column label="证据命中" width="110" :formatter="row => verdict(row.evidence_hit)" />
          <eb-table-column label="引用可解析" width="120" :formatter="row => verdict(row.citation_resolved)" />
          <eb-table-column label="判官正确" width="120" :formatter="row => row.judge_error ? '判分失败' : verdict(row.judge_correct)" />
          <eb-table-column prop="latency_ms" label="延迟 (ms)" width="120" />
        </eb-table></div>
        <p v-if="!items.length" class="sub">当前筛选暂无结果；运行中题目完成后会自动更新。</p>
      </section>
      <!-- 对比收进折叠：回归对比是评测的本职，但读数的主语是本次运行。 -->
      <details class="compare-fold" aria-labelledby="eval-compare-heading">
        <summary id="eval-compare-heading">对比另一运行（回归检验）</summary>
        <div class="run-detail compare-body">
        <p class="sub">左侧为当前运行，右侧为所选运行；差值 = 右 − 左。只展示协议允许的对比，不推断统计显著性。</p>
        <div class="form-actions"><eb-select class="compare-picker" :model-value="compareId" aria-label="选择对比运行" @update:model-value="$emit('update:compare-id', $event)"><eb-option v-for="other in runs.filter(value => value.id !== run.id)" :key="other.id" :value="other.id" :label="`${other.name || other.id} · ${other.state}`" /></eb-select><eb-button size="small" :loading="busy.compare" :disabled="!compareId" @click="$emit('compare')">比较</eb-button></div>
        <eb-alert v-if="compareError" type="error" :title="compareError" :closable="false" />
        <template v-if="comparison">
          <eb-alert :type="comparison.comparable ? 'success' : 'warning'" :title="comparison.comparable ? '符合当前协议的可比条件' : '不可直接比较'" :closable="false" />
          <ul v-if="comparison.reasons?.length"><li v-for="reason in comparison.reasons" :key="reason">{{ reason }}</li></ul>
          <p>左：{{ comparison.left?.name || comparison.left?.id }} · 右：{{ comparison.right?.name || comparison.right?.id }}</p>
          <h4>配置差异</h4>
          <div class="table-scroll"><eb-table :data="configDifferences" row-key="key" :scroll-x="500"><eb-table-column prop="key" label="配置项" min-width="180" /><eb-table-column prop="left" label="左 / 当前" min-width="160" /><eb-table-column prop="right" label="右 / 对比" min-width="160" /></eb-table></div>
          <p v-if="!configDifferences.length" class="sub">运行配置相同；仍需检查题集、语料及协议是否一致。</p>
          <eb-button link @click="compareProtocolOpen = !compareProtocolOpen">{{ compareProtocolOpen ? '收起' : '查看' }}双方协议与冻结配置</eb-button>
          <pre v-if="compareProtocolOpen" class="pre-block">{{ json({ left: protocol(comparison.left), right: protocol(comparison.right) }) }}</pre>
          <div class="table-scroll"><eb-table :data="comparisonMetrics" row-key="key" :scroll-x="680" aria-label="对比指标差值"><eb-table-column prop="label" label="指标" min-width="200" /><eb-table-column prop="left" label="左" width="140" /><eb-table-column prop="right" label="右" width="140" /><eb-table-column prop="delta" label="差值（右 − 左）" min-width="180" /></eb-table></div>
          <p class="sub">比例差值以百分点显示，Token / 延迟为绝对差值。服务端未提供或不可比的差值显示 N/A；不补造缺失指标。</p>
          <h4>逐题改善与退步（规则匹配）</h4>
          <div class="table-scroll"><eb-table :data="comparison.items || []" row-key="id" :scroll-x="650" aria-label="逐题改善退步"><eb-table-column prop="id" label="ID" width="100" /><eb-table-column prop="query" label="问题" min-width="240" /><eb-table-column label="左" width="90" :formatter="row => verdict(row.left_correct)" /><eb-table-column label="右" width="90" :formatter="row => verdict(row.right_correct)" /><eb-table-column label="变化" width="120" :formatter="row => ({ improved: '改善', regressed: '退步', unchanged: '不变' }[row.change] || row.change)" /></eb-table></div>
        </template>
        </div>
      </details>
    </div>
    <eb-dialog v-model="protocolOpen" title="运行协议、配置与冻结指纹" width="min(100%, 800px)" top="5vh"><pre class="pre-block">{{ json(protocol(run)) }}</pre><template #footer><eb-button @click="protocolOpen = false">关闭协议详情</eb-button></template></eb-dialog>
  </eb-section-card>
</template>

<script setup>
import { computed, ref } from 'vue';
const props = defineProps({ run: { type: Object, required: true }, items: { type: Array, default: () => [] }, runs: { type: Array, default: () => [] }, progress: Number, active: Boolean, busy: { type: Object, default: () => ({}) }, itemFilter: String, liveConfirmed: Boolean, compareId: String, comparison: Object, compareError: String });
defineEmits(['action', 'export', 'compare', 'select-item', 'new-run', 'update:item-filter', 'update:live-confirmed', 'update:compare-id']);
const protocolOpen = ref(false);
const compareProtocolOpen = ref(false);
const retryable = computed(() => !props.active && (props.run.failed > 0 || ['failed', 'cancelled', 'interrupted'].includes(props.run.state)));
const json = value => JSON.stringify(value, null, 2);
const verdict = value => value == null ? 'N/A' : value ? '是' : '否';
const formatMetric = (value, ratio = false) => value == null ? 'N/A' : ratio ? `${(Number(value) * 100).toFixed(1)}%` : Number(value).toLocaleString();
const protocol = value => value ? ({ id: value.id, dataset_id: value.dataset_id, protocol: value.protocol, config: value.config, config_text: value.config_text, frozen: value.frozen }) : null;
const metrics = [
  { key: 'n', label: '已记录题数 n' }, { key: 'rule_match', label: '规则匹配率', ratio: true },
  { key: 'evidence_hit', label: '证据命中率', ratio: true }, { key: 'citation_resolved', label: '引用可解析率', ratio: true },
  { key: 'judge_correct', label: '判官正确率', ratio: true }, { key: 'judge_n', label: '成功判分题数 judge_n' },
  { key: 'closed_book_match', label: '闭卷规则匹配率', ratio: true }, { key: 'latency_ms', label: '累计延迟 (ms)' },
  { key: 'search_tokens', label: '检索 Tokens' }, { key: 'judge_tokens', label: '判官 Tokens' }, { key: 'closed_book_tokens', label: '闭卷 Tokens' },
];
const configDifferences = computed(() => {
  const left = props.comparison?.left?.config || {};
  const right = props.comparison?.right?.config || {};
  return [...new Set([...Object.keys(left), ...Object.keys(right)])].filter(key => json(left[key]) !== json(right[key])).map(key => ({ key, left: json(left[key]) ?? 'N/A', right: json(right[key]) ?? 'N/A' }));
});
const comparisonMetrics = computed(() => metrics.map(metric => {
  const delta = props.comparison?.comparable ? props.comparison?.deltas?.[metric.key] : null;
  return { ...metric, left: formatMetric(props.comparison?.left?.summary?.[metric.key], metric.ratio), right: formatMetric(props.comparison?.right?.summary?.[metric.key], metric.ratio), delta: delta == null ? 'N/A' : `${delta > 0 ? '+' : ''}${metric.ratio ? (delta * 100).toFixed(1) + ' 个百分点' : Number(delta).toLocaleString()}` };
}));
</script>

<style scoped>
.run-detail { display: grid; gap: var(--eb-space-4); min-width: 0; }
.run-detail p { margin: 0; }
.run-detail h4 { margin: 0; }
.metric-notes { border: none; color: var(--eb-text-color-placeholder); line-height: 1.8; }
.metric-notes summary { cursor: pointer; user-select: none; }
.compare-fold { border: 1px solid var(--eb-border-color-lighter); border-radius: 10px; padding: 8px 12px; }
.compare-fold summary { cursor: pointer; font-size: 13px; font-weight: 600; color: var(--eb-text-color-regular); user-select: none; }
.compare-body { padding-top: var(--eb-space-3); }
.metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--eb-space-5); margin: var(--eb-space-4) 0; }
.metrics dd { margin: var(--eb-space-1) 0 0; }
.filter-picker { width: min(100%, 220px); }
.compare-picker { width: min(100%, 480px); }
</style>

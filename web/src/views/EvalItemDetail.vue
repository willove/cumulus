<template>
  <eb-drawer :model-value="true" :title="`题目详情 · ${item.id}`" size="min(100%, 760px)" @update:model-value="value => { if (!value) $emit('close'); }">
    <article class="item-detail" aria-label="冻结逐题结果">
      <div class="form-actions"><eb-tag>{{ item.state }}</eb-tag><eb-tag>{{ item.mode }}</eb-tag><span class="tiny">第 {{ item.attempt ?? 'N/A' }} 次尝试</span></div>
      <section><h3>问题</h3><p class="answer-text">{{ item.query }}</p></section>
      <section><h3>参考答案</h3><p class="answer-text">{{ item.reference || 'N/A' }}</p><p class="tiny">标准证据键：{{ (item.gold_sources || []).join('、') || 'N/A' }}</p></section>
      <section><h3>系统答案</h3><p class="answer-text">{{ item.answer || '尚无答案' }}</p></section>
      <section><h3>闭卷基线答案</h3><p class="answer-text">{{ item.closed_book_answer || 'N/A（未运行或未产出）' }}</p></section>
      <dl class="item-metrics"><div v-for="metric in metrics" :key="metric.key"><dt class="tiny">{{ metric.label }}</dt><dd>{{ metric.boolean ? verdict(item[metric.key]) : item[metric.key] ?? 'N/A' }}</dd></div></dl>
      <eb-alert v-if="item.error" type="error" :title="`执行错误 · ${item.error_stage || '未知阶段'}`" :closable="false">{{ item.error }}</eb-alert>
      <section aria-label="判官详情"><h3>判官详情</h3><p>判官正确：{{ item.judge_error ? 'N/A（判分失败）' : verdict(item.judge_correct) }}</p><eb-alert v-if="item.judge_error" type="error" :title="item.judge_error" :closable="false" /><p class="answer-text">{{ item.judge_reason || '无判官理由（未判分时不视为错误答案）' }}</p></section>
      <section aria-labelledby="eval-frozen-citations-heading" class="item-detail">
        <h3 id="eval-frozen-citations-heading">冻结引用与原文摘录</h3>
        <p class="sub">以下内容来自本次运行保存的引用快照，不读取当前实时文档正文。引用可解析仅说明能定位来源，不等于证据蕴含答案。</p>
        <eb-section-card v-for="(citation, index) in item.citations || []" :key="index" :title="`${index + 1}. ${citation.title || citation.source_id}`">
          <p class="tiny">来源 ID：{{ citation.source_id }} · 可解析：{{ verdict(citation.resolved) }}</p>
          <blockquote class="answer-text">{{ citation.quote || '未保存摘录' }}</blockquote>
        </eb-section-card>
        <p v-if="!item.citations?.length" class="sub">本题没有冻结引用。</p>
      </section>
      <section aria-label="历史尝试" class="item-detail">
        <h3>历史尝试 · {{ item.attempts?.length || 0 }}</h3>
        <p class="sub">上方展示当前尝试。以下保留重试前的结果、错误与消耗；运行汇总包含各次尝试，不等同于仅当前答案的成本。</p>
        <div class="table-scroll"><eb-table :data="item.attempts || []" :scroll-x="740" aria-label="历史尝试成本"><eb-table-column prop="attempt" label="尝试" width="70" /><eb-table-column prop="state" label="状态" width="100" /><eb-table-column prop="error_stage" label="错误阶段" min-width="130" /><eb-table-column prop="latency_ms" label="延迟 ms" width="110" /><eb-table-column prop="search_tokens" label="检索 Tokens" width="110" /><eb-table-column prop="judge_tokens" label="判官 Tokens" width="110" /><eb-table-column prop="closed_book_tokens" label="闭卷 Tokens" width="110" /></eb-table></div>
        <eb-button link @click="rawOpen = !rawOpen">{{ rawOpen ? '收起' : '查看' }}完整结果与尝试记录</eb-button>
        <pre v-if="rawOpen" class="pre-block">{{ JSON.stringify(item, null, 2) }}</pre>
      </section>
    </article>
    <template #footer><eb-button @click="$emit('close')">关闭题目详情</eb-button></template>
  </eb-drawer>
</template>

<script setup>
import { ref } from 'vue';
defineProps({ item: { type: Object, required: true } });
defineEmits(['close']);
const rawOpen = ref(false);
const verdict = value => value == null ? 'N/A' : value ? '是' : '否';
const metrics = [
  { key: 'rule_match', label: '规则匹配（非 EM）', boolean: true }, { key: 'evidence_hit', label: '证据命中', boolean: true },
  { key: 'citation_resolved', label: '引用可解析', boolean: true }, { key: 'closed_book_match', label: '闭卷规则匹配', boolean: true },
  { key: 'latency_ms', label: '当前尝试延迟 (ms)' }, { key: 'search_tokens', label: '检索 Tokens' },
  { key: 'judge_tokens', label: '判官 Tokens' }, { key: 'closed_book_tokens', label: '闭卷 Tokens' },
];
</script>

<style scoped>
.item-detail { display: grid; gap: var(--eb-space-4); min-width: 0; }
.item-detail h3 { margin: 0; font-size: var(--eb-font-size-base); }
.item-detail p { margin: var(--eb-space-2) 0; }
.answer-text { white-space: pre-wrap; overflow-wrap: anywhere; margin: var(--eb-space-3) 0; }
.item-metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--eb-space-4); margin: 0; }
.item-metrics dd { margin: var(--eb-space-1) 0 0; }
</style>

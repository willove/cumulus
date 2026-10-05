<template>
  <section class="legacy-evals" aria-labelledby="legacy-evals-heading">
    <div class="page-heading"><h3 id="legacy-evals-heading">旧版历史报告</h3><eb-button :loading="evalLoading" @click="loadEvals">刷新旧版报告</eb-button></div>
    <eb-alert type="info" title="旧版评分协议，与评测工作台不可直接比较" :closable="false" />
    <eb-alert v-if="evalError" type="error" :title="evalError" :closable="false" />
    <div class="table-scroll"><eb-table :data="evalRuns" row-key="_id" :scroll-x="600" aria-label="旧版评测报告"><eb-table-column label="报告" min-width="240"><template #default="{ row }"><eb-button link type="primary" @click="openEval(row._id)">{{ row.tag || row._id }}</eb-button></template></eb-table-column><eb-table-column prop="at" label="时间" min-width="180" /><eb-table-column label="历史 EM" width="120" :formatter="row => percent(row.system?.em)" /></eb-table></div>
    <p v-if="!evalRuns.length && !evalLoading" class="sub">当前库没有旧版报告</p>
    <eb-section-card v-if="evalCur" :title="evalCur.tag || evalCur._id">
      <div class="legacy-evals">
        <eb-tag>{{ evalCur.judged ? '旧版已判官协议' : '未启用判官' }} · n={{ evalCur.n ?? 'N/A' }}</eb-tag>
        <dl class="legacy-metrics"><div v-for="metric in scoreItems" :key="metric.label"><dt class="tiny">{{ metric.label }}</dt><dd>{{ metric.value }}</dd></div></dl>
        <h4>失败分类（历史系统口径）</h4>
        <div v-for="category in taxonomy" :key="category.key" class="krow"><span>{{ category.label }}</span><b>{{ evalCur.system?.taxonomy?.[category.key] ?? 'N/A' }}</b></div>
        <h4>成本与旧版统计</h4>
        <div class="krow"><span>search tokens</span><b>{{ evalCur.search_tokens ?? 'N/A' }}</b></div>
        <div class="krow"><span>judge tokens</span><b>{{ evalCur.judge_tokens ?? 'N/A' }}</b></div>
        <div class="krow"><span>拒绝的复用提案</span><b>{{ evalCur.rejected_proposals ?? 'N/A' }}</b></div>
        <div class="krow"><span>McNemar（仅原报告统计，不外推新版显著性）</span><b>b={{ evalCur.mcnemar?.b_only ?? 'N/A' }} · c={{ evalCur.mcnemar?.c_only ?? 'N/A' }} · p={{ evalCur.mcnemar?.p == null ? 'N/A' : Number(evalCur.mcnemar.p).toFixed(3) }}</b></div>
        <div class="krow"><span>档位</span><b>{{ Object.entries(evalCur.modes || {}).map(([key, count]) => key + '×' + count).join(' ') || 'N/A' }}</b></div>
        <div class="krow"><span>题集指纹</span><b>{{ frozen?.items_sha || 'N/A' }}</b></div>
        <eb-button link @click="showRaw = !showRaw">{{ showRaw ? '收起' : '查看' }}旧版完整报告与冻结协议</eb-button>
        <pre v-if="showRaw" class="pre-block">{{ JSON.stringify(evalCur, null, 2) }}</pre>
      </div>
    </eb-section-card>
  </section>
</template>

<script setup>
import { computed, ref } from 'vue';
import { useEvalsPane } from '../panes/evals.js';
const { evalRuns, evalCur, evalLoading, evalError, loadEvals, openEval } = useEvalsPane();
const showRaw = ref(false);
const percent = value => value == null ? 'N/A' : (Number(value) * 100).toFixed(1) + '%';
const frozen = computed(() => evalCur.value?.frozen);
const scoreItems = computed(() => [
  { label: evalCur.value?.judged ? '历史 EM（已判官协议）' : '历史 EM（未判官）', value: percent(evalCur.value?.system?.em) },
  { label: '历史证据召回 Ev.Rec', value: percent(evalCur.value?.system?.ev_rec) },
  { label: '历史 Grounded', value: percent(evalCur.value?.system?.ground) },
  { label: '历史闭卷 EM', value: percent(evalCur.value?.closed_book?.em) },
]);
const taxonomy = [{ key: 'correct', label: '答对' }, { key: 'retrieved_but_unanswered', label: '找到未答好' }, { key: 'answered_but_wrong', label: '答错' }, { key: 'not_retrieved', label: '未定位' }];
</script>

<style scoped>
.legacy-evals { display: grid; gap: var(--eb-space-4); min-width: 0; }
.legacy-evals h4 { margin: 0; }
.legacy-metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: var(--eb-space-4); }
.legacy-metrics dd { margin: var(--eb-space-1) 0 0; }
</style>

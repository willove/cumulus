<template>
  <div class="pane usage-panel">
    <!-- 消费台账：按模型过滤的持久记录（重启不丢）。 -->
    <div class="usage-toolbar">
      <eb-select :model-value="modelFilter" class="usage-filter" aria-label="按模型筛选" clearable placeholder="全部模型" @update:model-value="pickModel">
        <eb-option v-for="m in modelOptions" :key="m" :value="m" :label="m" />
      </eb-select>
      <span class="tiny">共 {{ usageRecords.length }} 条<template v-if="usageScanned"> · 扫描 {{ usageScanned }} 条</template> · 合计 {{ totalTokens.toLocaleString() }} tokens</span>
      <eb-button size="small" :loading="usageBusy" @click="loadUsage(modelFilter)">刷新</eb-button>
    </div>
    <eb-alert v-if="usageError" type="error" :title="usageError" :closable="false" show-icon />
    <div v-if="usageBusy && !usageRecords.length" class="tiny" role="status">正在读取消费记录…</div>
    <div v-else-if="!usageRecords.length" class="empty-guide">
      <h3>还没有消费记录</h3>
      <p class="sub">每次检索都会在这里留一条：哪个库、哪个模型、花了多少 tokens。</p>
    </div>
    <div v-else class="table-scroll">
      <eb-table :data="usageRecords" row-key="at" aria-label="消费记录">
        <eb-table-column label="时间" width="170">
          <template #default="{ row }"><span class="num">{{ fmtWhen(row.at) }}</span></template>
        </eb-table-column>
        <eb-table-column prop="ns" label="知识库" width="130" />
        <eb-table-column prop="model" label="模型" min-width="150" />
        <eb-table-column label="输入 / 输出" width="150">
          <template #default="{ row }"><span class="num">{{ (row.prompt_tokens || 0).toLocaleString() }} / {{ (row.completion_tokens || 0).toLocaleString() }}</span></template>
        </eb-table-column>
        <eb-table-column label="合计 tokens" width="120">
          <template #default="{ row }"><b class="num">{{ (row.tokens || 0).toLocaleString() }}</b></template>
        </eb-table-column>
        <eb-table-column label="模式" width="100">
          <template #default="{ row }">{{ row.mode || '—' }}<template v-if="row.reused"> · 复用</template></template>
        </eb-table-column>
      </eb-table>
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from "vue";
import { useModelsPane } from "../panes/models.js";

const { usageRecords, usageScanned, usageBusy, usageError, loadUsage } = useModelsPane();
const modelFilter = ref("");

const modelOptions = computed(() => [...new Set(usageRecords.value.map(r => r.model).filter(Boolean))].sort());
const totalTokens = computed(() => usageRecords.value.reduce((sum, r) => sum + (r.tokens || 0), 0));

function pickModel(value) {
  modelFilter.value = value || "";
  loadUsage(modelFilter.value);
}
function fmtWhen(at) {
  if (!at) return "—";
  return new Date(at).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
</script>

<style src="./common.css"></style>
<style scoped>
.usage-panel { max-width: 1100px; }
.usage-toolbar { display: flex; align-items: center; gap: var(--eb-space-3); flex-wrap: wrap; margin-bottom: var(--eb-space-4); }
.usage-filter { width: 220px; }
</style>

<template>
  <section class="pane engine-page" aria-label="引擎">
    <!-- 页名顶栏已有；备注类说明句一律不留，配置热生效是行为不是标语。 -->
    <eb-segmented v-model="tab" :options="segments" />
    <div :key="tab" class="tab-swap engine-body">
      <MonitorView v-if="tab === 'status'" />
      <ModelsPanel v-else-if="tab === 'models'" />
      <UsagePanel v-else-if="tab === 'usage'" />
      <TestPanel v-else />
    </div>
  </section>
</template>

<script setup>
import { ref } from "vue";
import MonitorView from "./MonitorView.vue";
import ModelsPanel from "./ModelsPanel.vue";
import UsagePanel from "./UsagePanel.vue";
import TestPanel from "./TestPanel.vue";

const tab = ref("status");
// 「原始检索」是这一页唯一的检索面，也是它从「知识库」搬过来的原因：它打的是
// /v1/search（一次性 JSON），与问答页的 /v1/chat/completions（SSE）是两条不同接线，
// 语义不可互换——这个差异正是它的诊断价值，和「测试端点」是同一类东西。
const segments = [
  { label: "运行状态", value: "status" },
  { label: "模型与配置", value: "models" },
  { label: "消费记录", value: "usage" },
  { label: "原始检索", value: "search" },
];
</script>

<style src="./common.css"></style>
<style scoped>
.engine-page { display: flex; flex-direction: column; gap: var(--eb-space-4); }
.engine-body { min-width: 0; padding-top: var(--eb-space-2); }
</style>

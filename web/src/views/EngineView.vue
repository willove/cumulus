<template>
  <section class="pane engine-page" aria-labelledby="engine-heading">
    <header class="page-heading">
      <div>
        <h2 id="engine-heading">引擎</h2>
        <p class="sub">服务的运行读数、模型配置与消费记录。启用模型即热生效，无需重启。</p>
      </div>
    </header>
    <!-- 引擎三分段（segmented 胶囊滑块）：先看健康，再管配置，再对账。 -->
    <eb-segmented v-model="tab" :options="segments" />
    <div :key="tab" class="tab-swap engine-body">
      <MonitorView v-if="tab === 'status'" />
      <ModelsPanel v-else-if="tab === 'models'" />
      <UsagePanel v-else />
    </div>
  </section>
</template>

<script setup>
import { ref } from "vue";
import MonitorView from "./MonitorView.vue";
import ModelsPanel from "./ModelsPanel.vue";
import UsagePanel from "./UsagePanel.vue";

const tab = ref("status");
const segments = [
  { label: "运行状态", value: "status" },
  { label: "模型与配置", value: "models" },
  { label: "消费记录", value: "usage" },
];
</script>

<style src="./common.css"></style>
<style scoped>
.engine-page { display: flex; flex-direction: column; gap: var(--eb-space-4); }
.engine-body { min-width: 0; padding-top: var(--eb-space-4); }
</style>

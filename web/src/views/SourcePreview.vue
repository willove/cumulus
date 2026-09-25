<template>
  <eb-dialog :model-value="true" :title="source?.title || '文档原文'" :width="860" align-center @update:model-value="!$event && $emit('close')">
    <div v-if="busy" class="sub" role="status">正在加载原文…</div>
    <eb-alert v-else-if="error" type="error" :title="error" :closable="false" show-icon><eb-button type="primary" link @click="load">重试</eb-button></eb-alert>
    <template v-else-if="source">
      <p class="tiny source-location">{{ source.type }} · {{ source.uri || source.id }}</p>
      <pre class="source-body">{{ source.body || '文档没有可显示的正文。' }}</pre>
    </template>
    <template #footer><eb-button @click="$emit('close')">关闭</eb-button></template>
  </eb-dialog>
</template>

<script setup>
import { ref, onMounted, onUnmounted } from "vue";
import { withNS } from "../state.js";
import { requestJSON } from "../api.js";
const props = defineProps({ sourceId: { type: String, required: true } });
defineEmits(["close"]);
const source = ref(null);
const busy = ref(false);
const error = ref("");
const controller = new AbortController();
async function load() {
  busy.value = true;
  error.value = "";
  try { source.value = await requestJSON(withNS("/v1/sources/" + encodeURIComponent(props.sourceId)), { signal: controller.signal }); }
  catch (err) { if (err.name !== "AbortError") error.value = err.message; }
  finally { busy.value = false; }
}
onMounted(load);
onUnmounted(() => controller.abort());
</script>

<style scoped>
.source-location { overflow-wrap: anywhere; margin: 0 0 var(--eb-space-4); }
.source-body { margin: 0; padding: var(--eb-space-4); border: 1px solid var(--eb-border-color); border-radius: var(--eb-radius-md); background: var(--eb-fill-color-light); color: var(--eb-text-color-regular); white-space: pre-wrap; overflow-wrap: anywhere; font: var(--eb-font-size-sm)/1.9 var(--cul-sans); max-height: 60vh; overflow: auto; }
</style>

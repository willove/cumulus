<template>
  <!-- 右侧抽屉：引用卡的「查看」从这里打开原文，不打断会话流。 -->
  <eb-drawer :model-value="true" :title="source?.title || '文档原文'" direction="rtl"
             size="min(100%, 560px)" @update:model-value="value => { if (!value) $emit('close'); }">
    <div v-if="busy" class="sub" role="status">正在加载原文…</div>
    <eb-alert v-else-if="error" type="error" :title="error" :closable="false" show-icon><eb-button type="primary" link @click="load">重试</eb-button></eb-alert>
    <template v-else-if="source">
      <p class="tiny source-location">{{ source.type }} · {{ source.uri || displaySourceId(source.id) }}</p>
      <p v-if="highlight && bodyHasQuote" class="tiny highlight-note">已定位到引用窗口，引文以底色标注。</p>
      <p v-else-if="highlight" class="tiny dim">该引用窗口与当前原文已对不上（源可能已更新）。</p>
      <pre class="source-body" ref="bodyEl"><template v-for="(part, i) in bodyParts" :key="i"><mark v-if="part.hit" class="quote-hit">{{ part.text }}</mark><template v-else>{{ part.text }}</template></template></pre>
    </template>
    <template #footer><eb-button @click="$emit('close')">关闭</eb-button></template>
  </eb-drawer>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from "vue";
import { withNS } from "../state.js";
import { requestJSON } from "../api.js";
import { displaySourceId } from "../panes/chat.js";
const props = defineProps({
  sourceId: { type: String, required: true },
  highlight: { type: String, default: "" }, // 引用摘录：在原文中标注并滚动到位
});
defineEmits(["close"]);
const source = ref(null);
const busy = ref(false);
const error = ref("");
const bodyEl = ref(null);
const controller = new AbortController();
async function load() {
  busy.value = true;
  error.value = "";
  try { source.value = await requestJSON(withNS("/v1/sources/" + encodeURIComponent(props.sourceId)), { signal: controller.signal }); }
  catch (err) { if (err.name !== "AbortError") error.value = err.message; }
  finally { busy.value = false; }
}
// 正文按引文切段：命中段包 <mark>。引文常被引用管线截断（末字符切掉），
// 所以按前缀阶梯回退：全长 → 3/4 → 1/2 → 1/3 → 20 字，第一个能定位的
// 长度即为高亮跨度；全都定位不到（源已更新）才整段平原。
const bodyParts = computed(() => {
  const body = source.value?.body || "";
  const q = (props.highlight || "").trim();
  if (!body || !q) return [{ text: body }];
  const runes = [...q];
  const ladder = [...new Set([runes.length, Math.floor(runes.length * 3 / 4), Math.floor(runes.length / 2), Math.floor(runes.length / 3), 20].filter(n => n > 0 && n <= runes.length))];
  for (const n of ladder) {
    const probe = runes.slice(0, n).join("");
    const i = body.indexOf(probe);
    if (i >= 0) {
      return [
        { text: body.slice(0, i) },
        { hit: true, text: probe },
        { text: body.slice(i + probe.length) },
      ];
    }
  }
  return [{ text: body }];
});
const bodyHasQuote = computed(() => bodyParts.value.some((p) => p.hit));
// 打开后把引文滚到视口顶部（高亮在超长文档里否则找不到）。
watch(bodyHasQuote, async (hit) => {
  if (!hit) return;
  await nextTick();
  const el = bodyEl.value;
  if (el?.querySelector) el.querySelector(".quote-hit")?.scrollIntoView({ block: "center" });
});
onMounted(load);
onUnmounted(() => controller.abort());
</script>

<style scoped>
.source-location { overflow-wrap: anywhere; margin: 0 0 var(--eb-space-4); display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; line-clamp: 2; overflow: hidden; }
.highlight-note { margin: 0 0 var(--eb-space-2); color: var(--eb-color-primary); }
.source-body { margin: 0; padding: var(--eb-space-4); border: 1px solid var(--eb-border-color); border-radius: var(--eb-radius-md); background: var(--eb-fill-color-light); color: var(--eb-text-color-regular); white-space: pre-wrap; overflow-wrap: anywhere; font: var(--eb-font-size-sm)/1.9 var(--cul-sans); max-height: calc(100vh - 220px); overflow: auto; }
.dim { color: var(--eb-text-color-placeholder); }
.quote-hit { background: var(--eb-color-primary-light, rgba(64, 158, 255, .22)); color: inherit; border-radius: 2px; padding: 0 1px; }
</style>

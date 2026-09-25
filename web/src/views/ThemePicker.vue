<template>
  <eb-popover title="主题与外观" trigger="click" placement="bottom-end" width="min(360px, calc(100vw - 32px))">
    <eb-button size="small" icon="palette" aria-label="主题与外观">主题</eb-button>
    <template #content>
      <div class="theme-picker">
        <eb-segmented :model-value="isDark ? 'dark' : 'light'" :options="modes"
                      aria-label="明暗模式" @update:model-value="$emit('dark-change', $event === 'dark')" />
        <div class="theme-presets" aria-label="business-ui 内置主题">
          <eb-button v-for="preset in EB_THEME_PRESETS" :key="preset.value"
                     :type="primary === preset.value ? 'primary' : 'default'" plain
                     :aria-pressed="primary === preset.value" :aria-label="'应用' + preset.name + '主题'"
                     @mouseenter="preview = preset.value" @focus="preview = preset.value"
                     @click="apply(preset.value)">
            <span class="theme-swatch" :style="{ backgroundColor: preset.value }" aria-hidden="true"></span>{{ preset.name }}
          </eb-button>
        </div>
        <div class="theme-preview" :style="previewStyle" aria-label="主题组件预览">
          <div class="theme-preview-heading"><strong>{{ previewName }}</strong><span class="tiny">组件预览 · {{ isDark ? '深色' : '浅色' }}</span></div>
          <div class="form-actions"><eb-button type="primary" size="small">主要操作</eb-button><eb-button size="small">次要操作</eb-button><eb-tag type="primary" size="small">标签</eb-tag></div>
          <eb-input model-value="知识库内容" readonly aria-label="输入框预览" />
          <eb-progress :percentage="64" :show-text="false" />
        </div>
        <div class="theme-picker-footer"><span class="tiny" role="status">当前：{{ currentName }}</span><eb-button type="primary" link size="small" @click="apply(EB_THEME_PRESETS[0].value)">恢复默认主色</eb-button></div>
      </div>
    </template>
  </eb-popover>
</template>

<script setup>
import { ref, computed, watch } from "vue";
import { EB_THEME_PRESETS, generatePrimaryRamp } from "@wil-works/evoke-business-ui";
const props = defineProps({ primary: { type: String, required: true }, isDark: Boolean });
const emit = defineEmits(["change", "dark-change"]);
const preview = ref(props.primary);
const modes = [{ value: "light", label: "浅色" }, { value: "dark", label: "深色" }];
const currentName = computed(() => EB_THEME_PRESETS.find(preset => preset.value === props.primary)?.name || "自定义");
const previewName = computed(() => EB_THEME_PRESETS.find(preset => preset.value === preview.value)?.name || "自定义");
const previewStyle = computed(() => generatePrimaryRamp(preview.value, { dark: props.isDark }));
watch(() => props.primary, value => { preview.value = value; });
function apply(value) { preview.value = value; emit("change", value); }
</script>

<style scoped>
.theme-picker { display: grid; gap: var(--eb-space-4); }
.theme-presets { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--eb-space-2); }
.theme-presets .eb-button { margin: 0; justify-content: flex-start; }
.theme-swatch { width: 14px; height: 14px; flex: none; border-radius: var(--eb-radius-full); border: 1px solid var(--eb-border-color); margin-right: var(--eb-space-2); }
.theme-preview { display: grid; gap: var(--eb-space-3); padding: var(--eb-space-4); border: 1px solid var(--eb-border-color); border-radius: var(--eb-radius-md); background: var(--eb-bg-color); }
.theme-preview-heading, .theme-picker-footer { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: var(--eb-space-2); }
.theme-preview-heading strong { color: var(--eb-color-primary); }
</style>

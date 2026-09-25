import assert from "node:assert/strict";
import test from "node:test";
import vm from "node:vm";
import { readFile, readdir } from "node:fs/promises";
import { ref, watch, effectScope, nextTick } from "vue";

const source = (await readFile(new URL("./theme.js", import.meta.url), "utf8"))
  .replace(/^import .*;$/gm, "").replace(/^export /gm, "");
const presets = [{ name: "library default", value: "#175dff" }];
function appearance(t, { saved = null, mode = null, systemDark = false, blocked = false } = {}) {
  let stored = saved;
  const isDark = ref(false);
  const context = vm.createContext({
    ref, watch, EB_THEME_PRESETS: presets,
    normalizeHex: value => typeof value === "string" && /^#[0-9a-f]{6}$/i.test(value) ? value.toLowerCase() : null,
    loadThemeConfig: () => stored,
    saveThemeConfig: value => { if (blocked) return false; stored = value; return true; },
    useDarkMode: () => ({ isDark, setDark: value => { isDark.value = value; }, toggleDark: () => { isDark.value = !isDark.value; } }),
    localStorage: {
      getItem: () => { if (blocked) throw new Error("storage blocked"); return mode; },
      setItem: (_, value) => { if (blocked) throw new Error("storage blocked"); mode = value; },
    },
    window: { matchMedia: () => ({ matches: systemDark }) },
  });
  const scope = effectScope();
  scope.run(() => vm.runInContext(source + "\nglobalThis.theme = useAppearance();", context));
  t.after(() => scope.stop());
  return { theme: context.theme, stored: () => stored, mode: () => mode };
}

test("theme defaults to the library preset and follows the system on first use", t => {
  const { theme } = appearance(t, { systemDark: true });
  assert.equal(theme.primary.value, presets[0].value);
  assert.equal(theme.isDark.value, true);
});

test("persisted colors do not pin later selections to the old preset", t => {
  const saved = { primary: "#0fa968", semantic: { warning: "#ffaa00" } };
  const state = appearance(t, { saved, mode: "light", systemDark: true });
  assert.equal(state.theme.primary.value, saved.primary);
  assert.equal(state.theme.isDark.value, false);
  state.theme.selectPrimary("#7b2ff2");
  assert.equal(state.theme.primary.value, "#7b2ff2");
  assert.equal(state.stored().primary, "#7b2ff2");
  assert.deepEqual(state.stored().semantic, saved.semantic);
  state.theme.selectPrimary("#f5222d");
  assert.equal(state.theme.primary.value, "#f5222d");
  const reloaded = appearance(t, { saved: state.stored(), mode: state.mode() });
  assert.equal(reloaded.theme.primary.value, "#f5222d");
});

test("dark and light toggles are remembered", async t => {
  const state = appearance(t, { mode: "dark" });
  assert.equal(state.theme.isDark.value, true);
  state.theme.toggleDark();
  await nextTick();
  assert.equal(state.mode(), "light");
  state.theme.setDark(true);
  await nextTick();
  assert.equal(state.mode(), "dark");
});

test("invalid saved colors use the default and invalid selections are ignored", t => {
  const state = appearance(t, { saved: { primary: "not-a-color" } });
  assert.equal(state.theme.primary.value, presets[0].value);
  state.theme.selectPrimary("url(bad)");
  assert.equal(state.theme.primary.value, presets[0].value);
});

test("blocked storage still allows session-local appearance changes", async t => {
  const state = appearance(t, { blocked: true });
  state.theme.selectPrimary("#111827");
  state.theme.setDark(true);
  await nextTick();
  assert.equal(state.theme.primary.value, "#111827");
  assert.equal(state.theme.isDark.value, true);
});

test("app delegates tokens and controls to evoke-business-ui", async () => {
  const css = await readFile(new URL("./theme.css", import.meta.url), "utf8");
  const common = await readFile(new URL("./views/common.css", import.meta.url), "utf8");
  const app = await readFile(new URL("./App.vue", import.meta.url), "utf8");
  const picker = await readFile(new URL("./views/ThemePicker.vue", import.meta.url), "utf8");
  const documents = await readFile(new URL("./views/DocumentsPanel.vue", import.meta.url), "utf8");
  assert.match(documents, /<eb-upload[^>]+:auto-upload="false"/);
  assert.match(documents, /<eb-form[^>]+@finish="startUpload"/);
  assert.match(documents, /<eb-table\s/);
  assert.doesNotMatch(documents, /<table\s/);
  const evaluation = await readFile(new URL("./views/EvalsView.vue", import.meta.url), "utf8");
  assert.match(evaluation, /<eb-steps\s/);
  assert.match(evaluation, /<eb-upload[^>]+:auto-upload="false"/);
  assert.match(evaluation, /<eb-form\s/);
  assert.match(evaluation, /<eb-table\s/);
  assert.doesNotMatch(evaluation, /eval-run -file|:deep\(|#[0-9a-f]{3,8}\b/i);
  assert.doesNotMatch(css + common, /--eb-[\w-]+\s*:/);
  assert.match(app, /<eb-config-provider[^>]+:theme-color="primary"/);
  assert.doesNotMatch(app, /persist-theme|:is-dark="false"/);
  assert.match(picker, /v-for="preset in EB_THEME_PRESETS"/);
  assert.match(picker, /generatePrimaryRamp\(preview.value, \{ dark: props.isDark \}\)/);
  const views = await readdir(new URL("./views/", import.meta.url));
  for (const file of views.filter(name => name.endsWith(".vue"))) {
    const text = await readFile(new URL(`./views/${file}`, import.meta.url), "utf8");
    const nativeInputs = text.match(/<input\b[^>]*>/g) || [];
    for (const input of nativeInputs) {
      assert.equal(file, "DocumentsPanel.vue");
      assert.match(input, /type="file"/);
      assert.match(input, /\bwebkitdirectory\b/);
      assert.match(input, /\bhidden\b/);
    }
    assert.doesNotMatch(text.replace(/<input\b[^>]*>/g, ""), /<(?:button|select|textarea|dialog)(?:\s|>)/, file);
  }
});

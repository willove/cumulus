import { ref, watch } from "vue";
import { EB_THEME_PRESETS, loadThemeConfig, saveThemeConfig, normalizeHex, useDarkMode } from "@wil-works/evoke-business-ui";

const modeKey = "cumulus-color-mode";

export function useAppearance() {
  const saved = loadThemeConfig();
  const primary = ref(normalizeHex(saved?.primary) || EB_THEME_PRESETS[0].value);
  const { isDark, setDark, toggleDark } = useDarkMode();
  let mode;
  try { mode = localStorage.getItem(modeKey); } catch {}
  setDark(mode === "dark" || (mode !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches));
  watch(isDark, dark => {
    try { localStorage.setItem(modeKey, dark ? "dark" : "light"); } catch {}
  });

  function selectPrimary(value) {
    const color = normalizeHex(value);
    if (!color) return;
    primary.value = color;
    saveThemeConfig({ ...saved, primary: color });
  }

  return { primary, semantic: saved?.semantic, isDark, setDark, toggleDark, selectPrimary };
}

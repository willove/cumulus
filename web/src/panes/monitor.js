import { ref, onMounted, onUnmounted } from "vue";
import { withNS } from "../state.js";
import { requestJSON } from "../api.js";

export function useMonitorPane() {
  const mon = ref(null);
  const monBusy = ref(false);
  const monError = ref("");
  let timer = null;
  let disposed = false;

  async function loadMonitor() {
    if (monBusy.value) return;
    monBusy.value = true;
    try {
      const data = await requestJSON(withNS("/v1/monitor/overview"));
      if (!disposed) { mon.value = data; monError.value = ""; }
    } catch (error) { if (!disposed) monError.value = error.message; }
    finally { monBusy.value = false; }
  }

  onMounted(() => { loadMonitor(); timer = setInterval(loadMonitor, 5000); });
  onUnmounted(() => { disposed = true; clearInterval(timer); });
  return { mon, monBusy, monError, loadMonitor };
}

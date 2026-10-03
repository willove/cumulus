// 词档关联（affinity）面板逻辑：查一个词被引擎关联到哪些文档、权重多少。
//
// 这份账本不是展示品——检索在词面命中之外还用它把同族文档排到前面
// （cmd/cumulus-cluster/widen.go 的 affinityFirst），所以「为什么这篇排在前头」
// 的答案在这里。接口一直都有（GET /v1/affinity?token=&ns=，按权重取前 50），
// 但始终没有界面，属于「设计了却没接上」的那一类。
//
// token 是必填，后端缺了直接 400。所以空输入**不发请求**、就地给提示：
// 把一个必然失败的参数错误显示成「查无关联」，等于告诉用户这个词没有关联，
// 那是假信息而不是缺信息。
import { ref } from "vue";
import { nsSel, withNS } from "../state.js";
import { requestJSON } from "../api.js";

const token = ref("");
const docs = ref([]);
const busy = ref(false);
const error = ref("");
// searched 是「实际查成功的那个词」，与输入框里的 token 分开：
// 用户改输入框时结果区仍标着自己是哪次查询的产物。
const searched = ref("");
let request = 0;

export function useAffinityPane() {
  async function lookup(raw) {
    const term = String(raw == null ? token.value : raw).trim();
    error.value = "";
    if (!term) {
      docs.value = [];
      searched.value = "";
      error.value = "先输入一个词——这份账本是按词查的。";
      return;
    }
    if (!nsSel.value) {
      docs.value = [];
      searched.value = "";
      error.value = "先在顶部选择一个知识库。";
      return;
    }
    // 换词/换库的竞态：只有最后一次查询可以写结果。
    const epoch = ++request;
    busy.value = true;
    try {
      const data = await requestJSON(withNS("/v1/affinity?token=" + encodeURIComponent(term)));
      if (epoch !== request) return;
      docs.value = data.docs || [];
      searched.value = data.token || term;
    } catch (err) {
      if (epoch !== request) return;
      docs.value = [];
      searched.value = "";
      error.value = err.message;
    } finally {
      if (epoch === request) busy.value = false;
    }
  }

  function reset() {
    request++;
    token.value = "";
    docs.value = [];
    searched.value = "";
    error.value = "";
    busy.value = false;
  }

  return { token, docs, busy, error, searched, lookup, reset };
}

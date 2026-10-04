import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

// 与 App.test.mjs 同款 VM 壳：直引 panes/chat.js 会拉起 evoke-business-ui
// 的 dayjs 裸后缀解析（node 不认，vite 认），这里只测纯函数——剥掉 import
// 后在沙箱里执行，useChatPane 未被调用，其自由名不需要存在。
const strip = (src) => src.replace(/^import .*;$/gm, "").replace(/^export /gm, "");
const source = strip(await readFile(new URL("./panes/chat.js", import.meta.url), "utf8"));
const sandbox = {
  ref: (v) => ({ value: v }),
  onMounted: () => {}, onUnmounted: () => {}, nextTick: () => {}, watch: () => {},
};
vm.createContext(sandbox);
vm.runInContext(source, sandbox);
// vm 顶层 const/let 不挂到 globalThis（函数声明才挂），常量用求值表达式取。
const stageText = sandbox.stageText, stageDetailLines = sandbox.stageDetailLines, timelineFor = sandbox.timelineFor, displaySourceId = sandbox.displaySourceId;
const STAGE_ORDER = vm.runInContext("STAGE_ORDER", sandbox);

// 时间轴的「这步干了什么」行：detail 载荷必须变成可扫读的人话，且每个
// 阶段只挑它真正有的字段——没有 detail 的阶段（synth）不产出空行。

test("rewrite detail renders the from→to term change", () => {
  // VM 沙箱数组与 node realm 不同源，严格 deepEqual 比原型必炸：比拼接串。
  const join = (xs) => Array.from(xs).join(" | ");
  assert.equal(join(stageDetailLines("rewrite", { from: "闯红灯有什么处罚", to: "不按交通信号灯指示通行如何处罚" })),
    "「闯红灯有什么处罚」→「不按交通信号灯指示通行如何处罚」");
  assert.equal(stageText("rewrite"), "词汇鸿沟改写检索词");
  assert.equal(STAGE_ORDER.indexOf("rewrite"), 0, "rewrite sorts before analyze on the timeline");
});

test("analyze / cascade / sample details render their own fields only", () => {
  const join = (xs) => Array.from(xs).join(" | ");
  assert.equal(join(stageDetailLines("analyze", { intent: "search", primary: ["连接池", "超时"] })),
    "意图 search | 关键词 连接池、超时");
  assert.equal(join(stageDetailLines("cascade", { arm: "expanded", terms: ["信号灯", "通行", "处罚", "x", "y", "z"], top: ["道路交通安全法"] })),
    "扩展词命中 | 扩展词 信号灯、通行、处罚、x、y | 候选 道路交通安全法");
  assert.equal(join(stageDetailLines("sample", { source: "道路交通安全法", kept: 3, best: 8.5 })),
    "道路交通安全法 · 过线窗口 3 个");
  assert.equal(join(stageDetailLines("sample", { source: "会话文档", kept: 2, bridge: true })),
    "会话文档 · 过线窗口 2 个 · 会话桥接");
});

test("deep_sample truncates the uncovered list to four with an ellipsis", () => {
  const lines = stageDetailLines("deep_sample", { facts: 7, admitted: 5, missing: ["f1", "f2", "f3", "f4", "f5", "f6"] });
  assert.equal(lines.length, 1);
  assert.match(lines[0], /事实 7 项 · 收容窗口 5 个 · 未覆盖：f1、f2、f3、f4…$/);
});

test("stages without detail render nothing, unknown shapes survive", () => {
  assert.equal(stageDetailLines("synth", { anything: 1 }).length, 0);
  assert.equal(stageDetailLines("analyze", null).length, 0);
  assert.equal(stageDetailLines("analyze", "garbage").length, 0);
});

test("blk source ids decode for display, raw survives for lookup", () => {
  const raw = "src:blk/%E6%B3%95%E5%BE%8B%2F%E4%B8%AD%E5%8D%8E%E4%BA%BA%E6%B0%91%E5%85%B1%E5%92%8C%E5%9B%BD%E9%81%93%E8%B7%AF%E4%BA%A4%E9%80%9A%E5%AE%89%E5%85%A8%E6%B3%95.txt/000001#1";
  assert.equal(displaySourceId(raw), "法律/中华人民共和国道路交通安全法.txt/000001#1");
  // 路径形 id 不动；残缺编码序列不抛错、原样返回
  assert.equal(displaySourceId("src:行政法规/城市道路管理条例.txt#1"), "行政法规/城市道路管理条例.txt#1");
  assert.equal(displaySourceId("src:blk/%E6%3"), "src:blk/%E6%3");
  assert.equal(displaySourceId(undefined), "");
});

test("timelineFor keeps live details when the done payload rebuilds the times", () => {
  const message = {
    status: "done",
    // live 累积：detail 只在 live 行上有
    stages: [{ name: "rewrite", ms: 900, detail: { from: "a", to: "b" } }, { name: "analyze", ms: 800 }],
    // done 权威分段：微秒，没有 detail
    stats: { stages: { analyze: 1_800_000, rewrite: 900_000 } },
  };
  const rows = timelineFor(message);
  assert.equal(rows.map((r) => r.name).join(","), "rewrite,analyze");
  assert.equal(rows[0].ms, 900, "authoritative time wins");
  assert.equal(rows[0].detail.from + ">" + rows[0].detail.to, "a>b", "live detail survives the rebuild");
  assert.equal(rows[1].detail, undefined);
});

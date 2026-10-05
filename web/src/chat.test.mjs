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
  // top 支持 {t, id} 对象（思考框链接预览用）与旧版纯标题串两种形状
  assert.equal(join(stageDetailLines("cascade", { arm: "expanded", terms: ["信号灯", "通行", "处罚", "x", "y", "z"], top: [{ t: "道路交通安全法", id: "src:a#1" }, "备选串"] })),
    "扩展词命中 | 扩展词 信号灯、通行、处罚、x、y | 候选 道路交通安全法、备选串");
  assert.equal(join(stageDetailLines("sample", { source: "道路交通安全法", source_id: "src:a#1", kept: 3, best: 8.5 })),
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

// 会话恢复的重放：持久化的原始事件序列要重放出与实时折叠一致的思考框全文
// ——阶段/文件行按到达顺序，思考原文接在日志后，二次合成有分隔。
test("thinkContentFromSteps replays the persisted event stream into the box text", () => {
  const replay = sandbox.thinkContentFromSteps;
  assert.equal(replay(undefined), "", "absent think survives");
  assert.equal(replay([]), "", "empty steps survive");
  const out = replay([
    { kind: "stage", name: "analyze", ms: 120, detail: { intent: "search", primary: ["记分"] } },
    { kind: "file", file: "blk/%E6%B3%95%E5%BE%8B%2F%E8%AE%B0%E5%88%86.txt%2F000000", id: "src:blk/%E6%B3%95%E5%BE%8B%2F%E8%AE%B0%E5%88%86.txt%2F000000#1", score: 8.5, windows: 3 },
    { kind: "reasoning", text: "FAST 思考。" },
    { kind: "stage", name: "deep_sample", ms: 52000, detail: { facts: 7, admitted: 3 } },
    { kind: "stage", name: "deep_synth", ms: 9000 },
    { kind: "reasoning", text: "DEEP 思考。" },
  ]);
  const lines = out.split("\n");
  assert.equal(lines[0], "分析问题与检索意图：意图 search；关键词 记分 · 120ms");
  assert.match(lines[1], /^评分 \[法律\/记分\.txt\/000000\]\(doc:src%3Ablk%2F/, "file step replays as a doc link");
  assert.ok(lines[2] === "", "blank line separates the log from the reasoning");
  assert.ok(lines[3].startsWith("FAST 思考。"), "reasoning follows the log");
  assert.ok(out.includes("深度采样：逐篇收容评分：事实 7 项 · 收容窗口 3 个 · 52.0s"), "deep_sample line appends after the reasoning");
  assert.ok(out.includes("—— 重新合成 ——"), "resynth separator replays");
  assert.ok(out.endsWith("DEEP 思考。"), "second reasoning continues after the separator");
});

// 思考框的过程日志行：阶段行带 detail 人话与耗时，文档名走 doc: 协议的
// markdown 链接（视图层拦截点击开预览），无 id 退化纯文本；文件行是深度
// 循环逐篇评分的实时记录，畸形分数不抛错。
test("thinking-box log lines carry stage detail and per-file scores", () => {
  const stageThinkLine = sandbox.stageThinkLine, fileThinkLine = sandbox.fileThinkLine;
  assert.equal(stageThinkLine("cascade", { arm: "primary", top: [{ t: "道路交通安全法", id: "src:a#1" }, { t: "记分管理办法.txt", id: "src:b#2" }] }, 90),
    "关键词级联排序候选文档：主词命中；候选 [道路交通安全法](doc:src%3Aa%231)、[记分管理办法.txt](doc:src%3Ab%232) · 90ms");
  assert.equal(stageThinkLine("cascade", { arm: "primary", top: ["旧版纯标题"] }, 90),
    "关键词级联排序候选文档：主词命中；候选 旧版纯标题 · 90ms", "legacy string tops stay plain text");
  assert.equal(stageThinkLine("sample", { source: "记分管理办法.txt", source_id: "src:x#1", kept: 1 }, 3700),
    "采样并评分证据窗口：[记分管理办法.txt](doc:src%3Ax%231) · 过线窗口 1 个 · 3.7s");
  assert.equal(stageThinkLine("analyze", null, 120), "分析问题与检索意图 · 120ms", "no detail → no dangling colon");
  assert.equal(stageThinkLine("synth", { anything: 1 }, 0), "合成答案", "zero ms stays off the line");
  assert.equal(fileThinkLine("记分管理办法.txt", 8.5, 3, "src:a#1"), "评分 [记分管理办法.txt](doc:src%3Aa%231) 8.5 分 · 3 窗");
  assert.equal(fileThinkLine("记分管理办法.txt", 8.5, 3), "评分 记分管理办法.txt 8.5 分 · 3 窗", "no id degrades to plain text");
  assert.equal(fileThinkLine("麻醉药品目录.txt", 0, 2, "src:c#1"), "评分 [麻醉药品目录.txt](doc:src%3Ac%231) 0.0 分 · 2 窗");
  assert.equal(fileThinkLine("blk/%E6%B3%95%E5%BE%8B%2F%E5%AE%9E%E6%96%BD%E6%9D%A1%E4%BE%8B.txt%2F000000", 5, 1, "src:blk/%E6%B3%95%E5%BE%8B%2F%E5%AE%9E%E6%96%BD%E6%9D%A1%E4%BE%8B.txt%2F000000#1"),
    "评分 [法律/实施条例.txt/000000](doc:src%3Ablk%2F%25E6%25B3%2595%25E5%25BE%258B%252F%25E5%25AE%259E%25E6%2596%25BD%25E6%259D%25A1%25E4%25BE%258B.txt%252F000000%231) 5.0 分 · 1 窗", "URL-encoded keys decode for display, id rides encoded in the link");
  assert.equal(fileThinkLine("x.txt", undefined, undefined), "评分 x.txt ? 分", "missing score degrades, never throws");
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

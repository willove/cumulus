import assert from "node:assert/strict";
import test, { afterEach } from "node:test";
import vm from "node:vm";
import { readFile } from "node:fs/promises";
import { ref, computed, watch, effectScope } from "vue";
import { webcrypto } from "node:crypto";
import { evaluationAPI, evaluationURL } from "./api.js";

const code = (await readFile(new URL("./panes/evals.js", import.meta.url), "utf8"))
  .replace(/^import .*;$/gm, "").replace(/^export /gm, "");
const fixtures = [];
afterEach(() => fixtures.splice(0).forEach(f => f.dispose()));
const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
const tick = () => new Promise(resolve => setImmediate(resolve));
const plain = value => JSON.parse(JSON.stringify(value));
const item = { id: "q1", query: "连接池上限？", answer: "128", gold_sources: ["manual.txt"] };
const dataset = { id: "d1", name: "手册题集", count: 1, items: [item], sha: "items-sha" };
const result = { ...item, reference: item.answer, answer: "128", state: "completed", rule_match: true, evidence_hit: true };
const completed = { id: "r1", name: "运行", state: "completed", total: 1, done: 1, failed: 0, config: { mode: "offline" }, summary: { rule_match: 1 } };
function setup(override = () => undefined, namespace = "alpha") {
  const mounted = [], unmounted = [], calls = [], timers = new Map();
  let timer = 0;
  async function invoke(method, path, ns, body, signal) {
    calls.push({ method, path, ns, body: body ? plain(body) : body, signal });
    const custom = override(method, path, ns, body, signal);
    if (custom !== undefined) return plain(await custom);
    if (path === "capabilities") return { protocol: "eval-v2", live_available: false, max_items: 500, max_bytes: 2097152 };
    if (path === "datasets") return plain(method === "post" ? dataset : { datasets: [dataset] });
    if (path === "datasets/d1") return plain(dataset);
    if (path === "datasets/validate") return { valid: true, items: [item], errors: [], warnings: [], sha: "items-sha" };
    if (path === "runs") return plain(method === "post" ? completed : { runs: [completed] });
    if (path === "runs/r1") return plain(completed);
    if (path === "runs/r1/items") return plain({ items: [result] });
    return {};
  }
  const api = {
    get: (path, ns, signal) => invoke("get", path, ns, null, signal),
    post: (path, ns, body, signal) => invoke("post", path, ns, body, signal),
    download: (id, format, ns, signal) => invoke("download", id, ns, { format }, signal),
  };
  const nsSel = ref(namespace), pane = ref("evals");
  const scope = effectScope();
  const context = vm.createContext({ ref, computed, watch, nsSel, pane, evaluationAPI: api, crypto: webcrypto,
    onMounted: fn => mounted.push(fn), onUnmounted: fn => unmounted.push(fn), AbortController,
    setTimeout: fn => { const id = ++timer; timers.set(id, fn); return id; }, clearTimeout: id => timers.delete(id),
  });
  scope.run(() => vm.runInContext(code + "\nglobalThis.workbench = useEvaluationWorkbench();", context));
  const fixture = { ...context.workbench, nsSel, pane, calls, timers,
    mount: async () => { await Promise.all(mounted.map(fn => fn())); },
    timer: async () => { const entry = timers.entries().next().value; assert.ok(entry, "expected scheduled poll"); timers.delete(entry[0]); await entry[1](); await tick(); },
    dispose: () => { unmounted.forEach(fn => fn()); scope.stop(); },
  };
  fixtures.push(fixture);
  return fixture;
}
async function prepared(f) {
  // 线性流里「可以起跑」的全部前置就是：挂上 + 选了一个已保存的题集。
  await f.mount(); await f.chooseDataset("d1");
  assert.ok(f.datasetPreview.value);
}

test("evaluation API refuses implicit namespaces and encodes scoped requests", async t => {
  assert.throws(() => evaluationURL("runs", ""), /知识库/);
  assert.equal(evaluationURL("runs?limit=5", "a b"), "/v1/eval/runs?limit=5&ns=a%20b");
  let sent;
  t.mock.method(globalThis, "fetch", async (url, options) => { sent = { url, options }; return Response.json({ valid: false, errors: [{ line: 1, message: "missing answer" }] }); });
  const data = await evaluationAPI.post("datasets/validate", "alpha", { content: "bad" });
  assert.equal(sent.url, "/v1/eval/datasets/validate?ns=alpha");
  assert.equal(JSON.parse(sent.options.body).content, "bad");
  assert.equal(data.valid, false);
});

test("evaluation starts offline and no library makes no API calls", async () => {
  const f = setup(undefined, ""); await f.mount();
  assert.equal(f.calls.length, 0);
  assert.equal(f.config.value.mode, "offline");
  assert.equal(f.canStart.value, false);
  await f.startRun(); assert.match(f.formError.value, /知识库/);
});

test("dataset upload, validation, immutable save and confirmed start use one explicit namespace", async () => {
  const f = setup(); await f.mount();
  await f.readDataset(new File([JSON.stringify(item)], "handbook.jsonl"));
  assert.equal(f.datasetName.value, "handbook.jsonl");
  assert.equal(f.calls.filter(c => c.method === "post").length, 0);
  await f.validateDataset(); assert.equal(f.validation.value.valid, true);
  await f.saveDataset(); assert.equal(f.datasetID.value, "d1"); assert.ok(f.datasetPreview.value);
  await f.startRun();
  const start = f.calls.find(c => c.method === "post" && c.path === "runs");
  assert.ok(start.body.request_id);
  assert.equal(start.body.config.mode, "offline"); assert.equal(start.ns, "alpha");
  assert.equal(f.run.value.id, "r1"); assert.equal(f.items.value[0].answer, "128");
  assert.equal(f.progress.value, 100);
});

test("file size and blank input are rejected locally, invalid dataset cannot be saved", async () => {
  const f = setup(); await f.mount();
  await f.validateDataset(); assert.match(f.formError.value, /题集/);
  await f.readDataset({ name: "huge.jsonl", size: 2097153, text: () => { throw new Error("must not read"); } });
  assert.match(f.formError.value, /2 MiB/);
  await f.saveDataset(); assert.equal(f.calls.filter(c => c.method === "post").length, 0);
});

test("edits invalidate validation and ignore an older validation response", async () => {
  const gate = deferred();
  const f = setup((method, path) => path === "datasets/validate" ? gate.promise : undefined);
  await f.mount(); f.datasetContent.value = JSON.stringify(item);
  const pending = f.validateDataset();
  f.datasetContent.value = "changed";
  gate.resolve({ valid: true, items: [item] }); await pending;
  assert.equal(f.validation.value, null); assert.equal(f.busy.value.validate, false);
  assert.equal(f.calls.find(c => c.path === "datasets/validate").signal.aborted, true);
});

test("capability failure is not hidden by later successful list loads", async () => {
  const f = setup((method, path) => path === "capabilities" ? Promise.reject(new Error("cannot read capabilities")) : undefined);
  await f.mount(); assert.match(f.error.value, /cannot read capabilities/);
  assert.equal(f.capabilities.value, null); assert.equal(f.calls.length, 1);
});

test("live mode needs availability and explicit consent, switching offline clears judge", async () => {
  const f = setup(); await prepared(f);
  f.config.value.mode = "live"; await f.startRun();
  assert.equal(f.calls.some(c => c.method === "post" && c.path === "runs"), false);
  f.capabilities.value.live_available = true; await f.startRun();
  assert.equal(f.calls.some(c => c.method === "post" && c.path === "runs"), false);
  f.liveConfirmed.value = true; assert.equal(f.canStart.value, true);
  f.config.value.judge = true; f.config.value.closed_book = true;
  f.config.value.mode = "offline";
  assert.equal(f.config.value.judge, false); assert.equal(f.config.value.closed_book, false); assert.equal(f.liveConfirmed.value, false);
});

test("submit is single flight and ambiguous retry keeps idempotency key", async () => {
  let calls = 0; const gate = deferred();
  const f = setup((method, path) => method === "post" && path === "runs" ? (++calls === 1 ? gate.promise : completed) : undefined);
  await prepared(f); const pending = f.startRun(); await f.startRun();
  assert.equal(calls, 1); gate.reject(new Error("network lost")); await pending;
  assert.match(f.formError.value, /network lost/); await f.startRun();
  const submissions = f.calls.filter(c => c.method === "post" && c.path === "runs");
  assert.equal(submissions[0].body.request_id, submissions[1].body.request_id);
});

test("late selected-run responses cannot replace a different run", async () => {
  const gate = deferred();
  const f = setup((method, path) => path === "runs/slow" ? gate.promise : path === "runs/slow/items" ? { items: [] } : undefined);
  await f.mount(); const pending = f.openRun("slow"); await f.openRun("r1");
  gate.resolve({ id: "slow" }); await pending;
  assert.equal(f.run.value.id, "r1"); assert.equal(f.items.value.length, 1);
});

test("namespace switch invalidates pending dataset reads and run submissions", async () => {
  const gate = deferred();
  const f = setup((method, path) => method === "post" && path === "runs" ? gate.promise : undefined);
  await prepared(f); const pending = f.startRun(); f.nsSel.value = "beta";
  gate.resolve(completed); await pending; await tick();
  assert.equal(f.run.value, null); assert.equal(f.datasetID.value, "");
  assert.equal(f.calls.find(c => c.method === "post" && c.path === "runs").signal.aborted, true);
});

test("active tasks poll only on evaluation panel and stop after errors", async () => {
  let listReads = 0;
  const running = { ...completed, state: "running", done: 0 };
  const f = setup((method, path) => path === "runs" && method === "get" ? (++listReads > 1 ? Promise.reject(new Error("status unavailable")) : { runs: [running] }) : undefined);
  await f.mount(); assert.equal(f.timers.size, 1);
  f.pane.value = "chat"; assert.equal(f.timers.size, 0);
  f.pane.value = "evals"; assert.equal(f.timers.size, 1);
  await f.timer(); assert.match(f.pollError.value, /status unavailable/); assert.equal(f.timers.size, 0);
  assert.equal(f.runs.value[0].state, "running");
});

test("cancel and retry target the selected run and do not mutate another run", async () => {
  const gate = deferred();
  const f = setup((method, path) => path === "runs/r1/cancel" ? gate.promise : path === "runs/r2" ? { ...completed, id: "r2" } : path === "runs/r2/items" ? { items: [] } : undefined);
  await f.mount(); await f.openRun("r1"); const pending = f.runAction("cancel");
  await f.openRun("r2"); gate.resolve({ ...completed, state: "cancelled" }); await pending;
  assert.equal(f.run.value.id, "r2");
  f.run.value.config.mode = "live"; await f.runAction("retry");
  assert.match(f.error.value, /调用费用/);
  assert.equal(f.calls.some(c => c.path === "runs/r2/retry"), false);
});

test("comparison resets on selection changes and exposes incompatible reasons", async () => {
  const f = setup((method, path) => path.startsWith("compare?") ? { comparable: false, reasons: ["语料指纹不同"] } : undefined);
  await f.mount(); await f.openRun("r1"); f.compareID.value = "r2"; await f.compareRuns();
  assert.equal(f.comparison.value.comparable, false);
  assert.equal(f.comparison.value.reasons[0], "语料指纹不同");
  f.compareID.value = "r3"; assert.equal(f.comparison.value, null);
});

test("failed exports are surfaced instead of downloading an error document", async t => {
  t.mock.method(globalThis, "fetch", async () => Response.json({ error: "run not found" }, { status: 404 }));
  await assert.rejects(evaluationAPI.download("r1", "csv", "alpha"), /run not found/);
});

test("changed run configuration receives a new idempotency key", async () => {
  const f = setup((method, path) => method === "post" && path === "runs" ? Promise.reject(new Error("network lost")) : undefined);
  await prepared(f); await f.startRun();
  f.config.value.prior = false; await f.startRun();
  const starts = f.calls.filter(c => c.method === "post" && c.path === "runs");
  assert.notEqual(starts[0].body.request_id, starts[1].body.request_id);
});

test("failed item retry preserves the server's actionable interruption explanation", async () => {
  const f = setup((method, path) => path === "runs/r1/retry" ? Promise.reject(new Error("experiment interrupted; create a new run")) : undefined);
  await f.mount(); await f.openRun("r1");
  await f.runAction("retry");
  assert.match(f.error.value, /experiment interrupted/);
  assert.equal(f.run.value.state, "completed");
  assert.equal(f.busy.value.action, false);
});

test("a superseded comparison cannot appear beside another run", async () => {
  const gate = deferred();
  const f = setup((method, path) => path.startsWith("compare?") ? gate.promise : undefined);
  await f.mount(); await f.openRun("r1"); f.compareID.value = "r2";
  const pending = f.compareRuns(); f.compareID.value = "r3";
  gate.resolve({ comparable: true }); await pending;
  assert.equal(f.comparison.value, null);
});

test("late file reads cannot overwrite manually edited JSONL", async () => {
  const gate = deferred();
  const f = setup(); await f.mount();
  const pending = f.readDataset({ name: "slow.jsonl", size: 10, text: () => gate.promise });
  f.datasetContent.value = JSON.stringify({ ...item, query: "manual question" });
  gate.resolve(JSON.stringify(item)); await pending;
  assert.match(f.datasetContent.value, /manual question/);
  assert.equal(f.busy.value.file, false);
});

test("switching to an uploaded file invalidates a pending saved-dataset selection", async () => {
  const gate = deferred();
  const f = setup((method, path) => path === "datasets/d1" ? gate.promise : undefined); await f.mount();
  const pending = f.chooseDataset("d1");
  await f.readDataset(new File([JSON.stringify(item)], "fresh.jsonl"));
  gate.resolve(dataset); await pending;
  assert.equal(f.datasetID.value, ""); assert.equal(f.datasetPreview.value, null);
  assert.equal(f.datasetName.value, "fresh.jsonl");
});

test("unmount aborts readers and prevents polling resurrection", async () => {
  const gate = deferred();
  const f = setup((method, path) => path === "runs" ? gate.promise : undefined);
  const pending = f.mount(); await tick(); f.dispose();
  gate.resolve({ runs: [{ ...completed, state: "running" }] }); await pending;
  assert.equal(f.runs.value.length, 0); assert.equal(f.timers.size, 0);
});

test("editing a confirmed dataset invalidates its saved binding and confirmation", async () => {
  const f = setup(); await prepared(f);
  f.datasetContent.value = JSON.stringify({ ...item, query: "edited" });
  assert.equal(f.datasetID.value, ""); assert.equal(f.datasetPreview.value, null);
  await f.startRun(); assert.equal(f.calls.some(c => c.method === "post" && c.path === "runs"), false);
});

test("selecting a saved dataset cancels a prior upload read", async () => {
  const gate = deferred(); const f = setup(); await f.mount();
  const pending = f.readDataset({ name: "old.jsonl", size: 10, text: () => gate.promise });
  await f.chooseDataset("d1"); gate.resolve(JSON.stringify(item)); await pending;
  assert.equal(f.datasetID.value, "d1"); assert.equal(f.datasetPreview.value.id, "d1");
});

test("switching namespace during post-submit refresh cannot select the old run in the new library", async () => {
  const gate = deferred(); let reads = 0;
  const f = setup((method, path, ns) => method === "get" && path === "runs" && ns === "alpha" && ++reads > 1 ? gate.promise : undefined);
  await prepared(f); const pending = f.startRun(); await tick();
  f.nsSel.value = "beta"; gate.resolve({ runs: [completed] }); await pending; await tick();
  assert.equal(f.run.value, null);
  assert.equal(f.calls.some(c => c.ns === "beta" && c.path === "runs/r1"), false);
});

test("live creation consent never authorizes retry and retry consent is run-bound", async () => {
  const f = setup(); await f.mount(); await f.openRun("r1");
  f.run.value.config.mode = "live"; f.liveConfirmed.value = true;
  await f.runAction("retry"); assert.match(f.error.value, /调用费用/);
  f.retryConfirmed.value = true; await f.openRun("r1"); assert.equal(f.retryConfirmed.value, false);
});

test("post-submit list failures remain visible even when selected details succeed", async () => {
  let reads = 0;
  const f = setup((method, path) => method === "get" && path === "runs" && ++reads > 1 ? Promise.reject(new Error("cannot refresh list")) : undefined);
  await prepared(f); await f.startRun();
  assert.match(f.pollError.value, /cannot refresh list/); assert.equal(f.run.value.id, "r1");
});

test("item filters distinguish execution errors from scoring outcomes", async () => {
  const f = setup(); f.items.value = [result, { ...result, id: "bad", state: "failed", rule_match: false, evidence_hit: false }, { ...result, id: "wrong", rule_match: false }];
  f.itemFilter.value = "failed"; assert.deepEqual(plain(f.filteredItems.value.map(i => i.id)), ["bad"]);
  f.itemFilter.value = "wrong"; assert.equal(f.filteredItems.value.length, 2);
  f.itemFilter.value = "missed"; assert.equal(f.filteredItems.value.length, 1);
});

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test, { afterEach } from "node:test";
import vm from "node:vm";
import { ref, computed, watch, effectScope } from "vue";

// 在同一 VM 中执行真实 state/pane 代码：requestJSON 使用本用例的 fetch，
// Vue ref/computed/watch 也用真身。挂载显式进行，避免无关面板 GET 污染断言。
const strip = (src) => src.replace(/^import .*;$/gm, "").replace(/^export /gm, "");
const names = ["chat", "clusters", "ingest", "settings", "evals", "monitor"];
const source = [strip(await readFile(new URL("./api.js", import.meta.url), "utf8")),
  strip(await readFile(new URL("./state.js", import.meta.url), "utf8")),
  ...await Promise.all(names.map(async (p) => strip(await readFile(new URL(`./panes/${p}.js`, import.meta.url), "utf8")))),
].join("\n");
const fixtures = [];
afterEach(() => { for (const fixture of fixtures.splice(0)) fixture.dispose(); });
const json = (data, status = 200) => new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
const plain = (value) => JSON.parse(JSON.stringify(value));
const tick = () => new Promise((resolve) => setImmediate(resolve));
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const answer = (text = "answer") => new Response(
  `event: content\ndata: ${JSON.stringify({ text })}\n\n` +
  'event: citations\ndata: {"refs":[{"index":1,"source_id":"src:a","quote":"evidence"}]}\n\n' +
  'event: done\ndata: {"mode":"FAST","conf":0.8}\n\n',
);

function app(fetch, thinkError, namespace = "t1") {
  let hookOwner, sequence = 0, timerID = 0;
  const hooks = {}, scopes = {}, instances = {}, timers = new Map(), completed = [];
  const root = effectScope();
  const context = vm.createContext({
    ref, computed, watch,
    onMounted: (fn) => hooks[hookOwner].mounted.push(fn),
    onUnmounted: (fn) => hooks[hookOwner].unmounted.push(fn),
    nextTick: (fn) => Promise.resolve().then(fn),
    setTimeout: (fn) => { timers.set(++timerID, fn); return timerID; },
    clearTimeout: (id) => timers.delete(id),
    setInterval: () => ++timerID, clearInterval: () => {},
    TextDecoder, AbortController, FormData, File,
    fetch: (url, options = {}) => fetch(url, options),
    useChatEngine: () => {
      const messages = ref([]);
      const find = (id) => messages.value.find((m) => m.id === id);
      return {
        messages,
        addUserMessage(content) {
          const message = { id: `user-${++sequence}`, role: "user", content, status: "done" };
          messages.value.push(message);
          return message;
        },
        createAssistantMessage() {
          const message = { id: `assistant-${++sequence}`, role: "assistant", content: "", status: "pending" };
          messages.value.push(message);
          return message;
        },
        appendThinkContent(id, text) {
          if (thinkError) throw thinkError;
          Object.assign(find(id), { thinkContent: text, thinking: true });
        },
        stopThinking(id) { if (find(id)) find(id).thinking = false; },
        appendContent(id, text) { find(id).content += text; find(id).status = "streaming"; },
        updateMessage(id, updates) { Object.assign(find(id), updates); },
        completeMessage(id) { completed.push(id); Object.assign(find(id), { status: "done", thinking: false }); },
        setMessageError(id, error) { Object.assign(find(id), { status: "error", error, thinking: false }); },
        cancelMessage(id) { Object.assign(find(id), { status: "cancelled", thinking: false }); },
      };
    },
  });
  root.run(() => vm.runInContext(source + `
    globalThis.factories = { chat: useChatPane, clusters: useClustersPane, ingest: useIngestPane,
      settings: useSettingsPane, evals: useEvalsPane, monitor: useMonitorPane };
    globalThis.shared = { nsSel, pane, withNS, requestJSON };
  `, context));
  context.shared.nsSel.value = namespace;
  function make(name) {
    hookOwner = name;
    hooks[name] = { mounted: [], unmounted: [] };
    scopes[name] = effectScope();
    instances[name] = scopes[name].run(() => context.factories[name]());
    return instances[name];
  }
  names.forEach(make);
  function unmount(name) {
    if (!scopes[name]?.active) return;
    hooks[name].unmounted.forEach((fn) => fn());
    scopes[name].stop();
  }
  const result = Object.assign({}, ...Object.values(instances), context.shared, {
    chat: instances.chat, ingest: instances.ingest, timers, completed,
    mount: async (...panes) => { await Promise.all(panes.flatMap((name) => hooks[name].mounted.map((fn) => fn()))); await tick(); },
    unmount,
    remountIngest: async () => {
      unmount("ingest");
      const ingest = make("ingest");
      result.ingest = ingest;
      Object.assign(result, ingest);
      await result.mount("ingest");
      return ingest;
    },
    runTimer: async () => {
      const [id, fn] = timers.entries().next().value;
      timers.delete(id); fn(); await tick();
    },
    dispose: () => { names.forEach(unmount); root.stop(); },
  });
  fixtures.push(result);
  return result;
}

function chatBackend(override = () => {}) {
  const calls = [], sessions = new Map();
  let id = 0;
  const fetch = async (url, options) => {
    const body = options.body ? JSON.parse(options.body) : null;
    calls.push({ url, body, options });
    const custom = await override(url, options, body);
    if (custom !== undefined) return custom;
    if (url === "/v1/sessions" && options.method === "POST") {
      const session = { id: `s${++id}`, messages: [], ns: body.ns };
      sessions.set(session.id, session);
      return json(session, 201);
    }
    if (/^\/v1\/sessions(?:\?|$)/.test(url)) return json([...sessions.values()]);
    if (url.startsWith("/v1/sessions/")) return json(sessions.get(decodeURIComponent(url.split("/").pop().split("?")[0])));
    if (url === "/v1/search/stream") {
      // 后端在第一次成功落库时 ensure 建会话；前端不再预建，所以这里物化即可，
      // 且 id 由前端生成后应保持稳定（同一轮追问必须复用同一 id）。
      if (!sessions.has(body.session)) sessions.set(body.session, { id: body.session, messages: [], ns: body.ns });
      sessions.get(body.session).messages.push({ role: "user", content: body.query }, { role: "assistant", content: "answer" });
      return answer();
    }
    return json({});
  };
  return { fetch, calls, sessions };
}

// 原有 13 个用例的业务覆盖保留，并显式检查首问、namespace 及非成功状态。
test("send uses one lazily-materialized session, preserves two rounds and avoids chatbot user duplication", async () => {
  const server = chatBackend();
  const state = app(server.fetch);
  await state.onSend("question");
  // 会话 id 前端生成（12 hex），服务端在首轮落库时才建；预建会留下打不开的空壳
  assert.match(state.current.value, /^[0-9a-f]{12}$/);
  assert.equal(server.calls.filter((c) => c.url === "/v1/sessions" && c.options.method === "POST").length, 0, "must not pre-create a session");
  assert.deepEqual([...server.sessions.keys()], [state.current.value], "the turn materializes the session");
  assert.deepEqual(plain(state.messages.value.map((m) => [m.role, m.content])), [["user", "question"], ["assistant", "answer"]]);
  assert.equal(state.messages.value[1].status, "done");
  assert.ok(state.messages.value[1].thinkContent);
  assert.equal(state.sources.value[0].source, "src:a");
  // eb-chatbot inserts the second user message before emitting send.
  state.messages.value.push({ id: "from-chatbot", role: "user", content: "follow-up", status: "done" });
  await state.onSend("follow-up");
  assert.equal(state.messages.value.length, 4);
  assert.equal(state.messages.value[2].id, "from-chatbot");
  assert.deepEqual(server.calls.filter((c) => c.url === "/v1/search/stream").map((c) => [c.body.query, c.body.prior]),
    [["question", true], ["follow-up", true]]);
  const firstID = server.calls.find((c) => c.url === "/v1/search/stream").body.session;
  assert.equal(server.calls.filter((c) => c.url === "/v1/search/stream").every((c) => c.body.session === firstID), true, "both rounds reuse one session");
  await state.openSession({ id: firstID });
  assert.equal(state.messages.value.length, 4, "both rounds can be reopened from server history");
  assert.equal(state.error.value, "");
  assert.equal(state.loading.value, false);
});

test("HTTP error settles loading without marking the assistant done", async () => {
  const state = app(async () => new Response("unavailable", { status: 503 }));
  state.current.value = "existing";
  await state.onSend("question");
  assert.match(state.error.value, /HTTP 503/);
  assert.equal(state.meta.value, "");
  assert.equal(state.loading.value, false);
  assert.equal(state.messages.value[1].status, "error");
  assert.equal(state.completed.length, 0);
});

test("the namespace selector scopes every face and explicit withNS uses its argument", async () => {
  const server = chatBackend();
  const state = app(server.fetch);
  await state.onSend("question");
  assert.equal(server.calls.find((c) => c.url === "/v1/search/stream").body.ns, "t1");
  await state.loadClusters();
  assert.match(server.calls.at(-1).url, /\/v1\/clusters\?limit=200&ns=t1/);
  await state.loadSessions();
  assert.match(server.calls.at(-1).url, /\/v1\/sessions\?ns=t1/);
  assert.equal(state.withNS("/v1/jobs?x=1", "other /库"), "/v1/jobs?x=1&ns=other%20%2F%E5%BA%93");
  state.nsSel.value = "";
  await state.onSend("must select a library");
  assert.equal(server.calls.filter((c) => c.url === "/v1/search/stream").length, 1);
  assert.match(state.error.value, /知识库/);
});

test("settings form never holds the stored key and submits only changed fields", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: options?.body ? JSON.parse(options.body) : null, method: options?.method || "GET" });
    if (url === "/v1/model") return json({ installed: true, dir: "/d", dims: 384, files: [] });
    if (url === "/v1/config" && (!options || options.method !== "POST")) {
      return json({ base_url: "https://api.example.com/v1", chat_model: "m1", embed_model: "", api_key_set: true, api_key_len: 42, reasoning_split: false, offline: false });
    }
    if (url === "/v1/config") return json({ saved: ["LLM_MODEL_NAME", "AIGATE_CHAT_MODEL"], env_file: "/repo/.env", hot_applied: true });
    return json({});
  });
  await state.mount("settings");
  // 已设置的密钥绝不下发到前端状态里
  assert.equal(state.settingsForm.value.api_key, "");
  assert.equal(state.settingsForm.value.base_url, "https://api.example.com/v1");

  // 什么都没改 → 不发请求，只提示
  await state.saveConfig();
  assert.equal(calls.filter((c) => c.url === "/v1/config" && c.method === "POST").length, 0);
  assert.match(state.settingsMsg.value, /没有改动/);

  // 只改 chat 模型 + 填新密钥 → 只提交这两个字段
  state.settingsForm.value.chat_model = "m2";
  state.settingsForm.value.api_key = "sk-new";
  await state.saveConfig();
  const save = calls.find((c) => c.url === "/v1/config" && c.method === "POST");
  assert.deepEqual(plain(save.body), { chat_model: "m2", api_key: "sk-new" });
  // 保存成功后密钥不留在表单里
  assert.equal(state.settingsForm.value.api_key, "");
  assert.match(state.settingsMsg.value, /已保存 2 项/);
});

test("weight verification is reported as structure, and a failure keeps the reason", async () => {
  let fail = false;
  const state = app(async (url) => {
    if (url === "/v1/model") return json({ installed: true, dir: "/d", dims: 384, files: [] });
    if (url === "/v1/config") return json({ offline: false, api_key_set: false });
    if (url === "/v1/model/verify") {
      if (fail) return json({ error: "model: weights absent at /d — run `cumulus-cluster model install`" }, 502);
      return json({ ok: true, dims: 384, ms: 197, norm: 1.0000000998071195, probe: [0.100397445, -0.022973191, -0.00033277128, -0.090989165] });
    }
    return json({});
  });
  await state.mount("settings");
  await state.verifyWeights();
  assert.equal(state.settingsVerify.value.ok, true);
  assert.equal(state.settingsVerify.value.dims, 384);
  assert.equal(state.settingsVerify.value.norm > 0.99, true);
  assert.equal(state.settingsVerify.value.probe.length, 4);
  // 页顶那条一次性提示不再是唯一载体
  assert.equal(state.settingsMsg.value, "");

  fail = true;
  await state.verifyWeights();
  assert.equal(state.settingsVerify.value.ok, false);
  assert.match(state.settingsVerify.value.error, /weights absent|model install/);
});

test("connection test surfaces the provider answer and its own failure", async () => {
  let ok = true;
  const state = app(async (url) => {
    if (url === "/v1/model") return json({ installed: true, dir: "/d", dims: 384, files: [] });
    if (url === "/v1/config") return json({ api_key_set: true, api_key_len: 5, offline: false });
    if (url === "/v1/config/test") return ok ? json({ ok: true, model: "m", latency_ms: 812, tokens: 31, answer: "可用" }) : json({ ok: false, error: "llm: status 401" }, 502);
    return json({});
  });
  await state.mount("settings");
  await state.testConnection();
  assert.equal(state.settingsTest.value.ok, true);
  assert.equal(state.settingsTest.value.tokens, 31);
  ok = false;
  await state.testConnection();
  assert.equal(state.settingsTest.value.ok, false);
  assert.match(state.settingsTest.value.error, /401/);
});

test("the settings pane loads weight status and endpoint config", async () => {
  const urls = [];
  const state = app(async (url) => {
    urls.push(url);
    if (url === "/v1/model") return json({ installed: false, dir: "/h/.cumulus/models/x", dims: 384, files: [] });
    if (url === "/v1/config") return json({ base_url: "", chat_model: "", api_key_set: false });
    return json({});
  });
  await state.mount("settings");
  assert.deepEqual(urls, ["/v1/model", "/v1/config"]);
  assert.equal(state.settingsModel.value.installed, false);
  assert.equal(state.settingsConfig.value.api_key_set, false);
});

test("installWeights starts the download and verify runs the weights", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, method: options.method });
    if (url === "/v1/model" && options.method === "POST") return json({ started: true });
    if (url === "/v1/model/verify") return json({ ok: true, dims: 384, ms: 812 });
    return json({});
  });
  await state.installWeights();
  assert.ok(calls.some((c) => c.url === "/v1/model" && c.method === "POST"));
  assert.match(state.settingsMsg.value, /下载中/);
  await state.verifyWeights();
  // 验证结果不再只是一行提示：结构化报告 + 页顶提示清空
  assert.equal(state.settingsVerify.value.ok, true);
  assert.equal(state.settingsVerify.value.ms, 812);
  assert.equal(state.settingsMsg.value, "");
  assert.equal(state.fmtMB(470641600), "470.6 MB");
});

test("scan discovers candidates and the picked ones become a namespaced job", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: options.body && JSON.parse(options.body) });
    if (url === "/v1/scan") return json({ candidates: [{ path: "/d/a.md" }, { path: "/d/b.txt" }], skipped: { ext: 1, old: 1 } });
    if (url === "/v1/ingest/jobs") return json({ job: "j1", state: "queued", total: 1 });
    return json([]);
  });
  state.scanDir.value = "/d";
  await state.runScan();
  assert.equal(state.scanReport.value.candidates.length, 2);
  assert.deepEqual(plain(state.scanPicked.value), [true, true]);
  assert.match(state.scanMeta.value, /候选 2 个 · 跳过 ext×1 old×1/);
  state.scanPicked.value[1] = false;
  await state.ingestPicked();
  assert.deepEqual(calls.at(-1), { url: "/v1/ingest/jobs", body: { candidates: ["/d/a.md"], ns: "t1" } });
  assert.equal(state.jobs.value[0].id, "j1");
  assert.equal(state.jobs.value[0].total, 1);
  assert.equal(state.jobs.value[0].ns, "t1");
});

test("the scoreboard pane lists runs and opens one", async () => {
  const urls = [];
  const state = app(async (url) => {
    urls.push(url);
    if (url.startsWith("/v1/evals?")) return json({ runs: [{ _id: "run:1", tag: "chinalaw39" }, { _id: "run:2", tag: "baseline" }] });
    if (url.startsWith("/v1/evals/")) return json({ _id: "run:1", tag: "chinalaw39", n: 39, judged: true, system: { em: 0.667 } });
    return json({});
  });
  await state.mount("evals");
  state.pane.value = "evals";
  assert.equal(state.evalRuns.value.length, 2);
  assert.ok(urls.some((u) => /\/v1\/evals\?limit=50/.test(u)));
  await state.openEval("run:1");
  assert.equal(state.evalCur.value.tag, "chinalaw39");
  assert.equal(state.pct(0.667), "66.7%");
  assert.ok(Math.abs(state.txRatio({ correct: 20, retrieved_but_unanswered: 1, answered_but_wrong: 8, not_retrieved: 10 }, "correct") - 20 / 39) < 1e-9);
  await state.loadEvals();
  assert.match(urls.at(-1), /ns=t1/);
});

test("thinking initialization error settles loading and remains visible", async () => {
  const state = app(async () => { throw new Error("unexpected request"); }, new Error("thinking failed"));
  await state.onSend("question");
  assert.match(state.error.value, /thinking failed/);
  assert.equal(state.loading.value, false);
  assert.equal(state.messages.value[1].status, "error");
});

test("startIngest without a dir stays local", async () => {
  const calls = [];
  const state = app(async (url) => { calls.push(url); return json({}); });
  await state.startIngest();
  assert.equal(calls.length, 0);
  assert.match(state.ingMeta.value, /请先填写目录/);
  assert.equal(state.ingBusy.value, false);
});

test("startIngest posts the dir and queues the full job response", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body), method: options.method });
    return json({ job: "docs1", state: "queued", total: 3, skipped: 1, skip_reasons: { empty: 1 }, skip_errors: { "/d/empty": "empty" }, records: 8 }, 202);
  });
  state.ingDir.value = "/data/docs";
  state.ingRec.value = true;
  state.ingName.value = "docs1";
  await state.startIngest();
  assert.deepEqual(calls, [{ url: "/v1/ingest/jobs", method: "POST", body: { dir: "/data/docs", recursive: true, job: "docs1", ns: "t1" } }]);
  assert.equal(state.jobs.value.length, 1);
  assert.equal(state.curJob().id, "docs1");
  assert.equal(state.curJob().state, "queued");
  assert.equal(state.curJob().total, 3);
  assert.equal(state.curJob().skipped, 1);
  assert.equal(state.curJob().records, 8);
  assert.equal(state.curJob().skip_errors["/d/empty"], "empty");
  assert.match(state.ingMeta.value, /已提交任务 docs1/);
});

test("startIngest surfaces a rejected job including the backend hint", async () => {
  const state = app(async () => json({ error: "dir not found", hint: "check server path" }, 400));
  state.ingDir.value = "/nope";
  await state.startIngest();
  assert.equal(state.jobs.value.length, 0);
  assert.match(state.ingMeta.value, /dir not found.*check server path/);
  assert.equal(state.ingBusy.value, false);
});

test("pollJobs folds the job state machine and preserves skipped-file and record details", async () => {
  const seen = [];
  const state = app(async (url) => {
    seen.push(url);
    if (url === "/v1/ingest/jobs") return json({ job: "docs1", state: "queued", total: 3 }, 202);
    if (seen.filter((u) => u.startsWith("/v1/ingest/jobs/")).length === 1) return json({ state: "running", phase: "extracting", total: 3, done: 1 });
    return json({ state: "done", phase: "upserting", total: 3, done: 3, failed: 0, skipped: 1,
      skip_reasons: { extraction: 1 }, skip_errors: { "/d/a.md": "permission denied" }, records: 42, updated: "now" });
  });
  state.pane.value = "documents";
  await state.mount("ingest");
  state.ingDir.value = "/data/docs";
  await state.startIngest();
  await state.pollJobs();
  assert.equal(state.curJob().state, "running");
  assert.equal(state.curJob().done, 1);
  assert.equal(state.curJob().phase, "extracting");
  await state.pollJobs();
  assert.equal(state.curJob().state, "done");
  assert.equal(state.curJob().done, 3);
  assert.equal(state.curJob().skipped, 1);
  assert.equal(state.curJob().records, 42);
  assert.deepEqual(plain(state.curJob().skip_reasons), { extraction: 1 });
  assert.deepEqual(plain(state.curJob().skip_errors), { "/d/a.md": "permission denied" });
  assert.equal(state.timers.size, 0);
  const before = seen.length;
  await state.pollJobs();
  assert.equal(seen.length, before);
});

test("a failed job carries its backend error rather than a poll error", async () => {
  const state = app(async (url) => url === "/v1/ingest/jobs"
    ? json({ job: "docs1", state: "queued", total: 3 }, 202)
    : json({ state: "failed", phase: "extracting", total: 3, done: 1, failed: 1, error: "permission denied" }));
  state.ingDir.value = "/data/docs";
  await state.startIngest();
  await state.pollJobs();
  assert.equal(state.curJob().state, "failed");
  assert.match(state.curJob().error, /permission denied/);
  assert.equal(state.curJob().pollError, "");
});

test("blank sends are local and repeated text after an assistant is a new user turn", async () => {
  const server = chatBackend();
  const state = app(server.fetch);
  await state.onSend("  ");
  assert.equal(server.calls.length, 0);
  await state.onSend("question");
  await state.onSend("question");
  assert.deepEqual(plain(state.messages.value.map((m) => m.role)), ["user", "assistant", "user", "assistant"]);
});

for (const [label, response, expected] of [
  ["HTTP error", () => json({ error: "search rejected", hint: "select bucket" }, 400), /search rejected.*select bucket/],
  ["non-stream body", () => new Response("not JSON"), /截断|done/],
]) {
  test(`a failed first question leaves no empty session shell: ${label}`, async () => {
    const calls = [];
    const state = app(async (url) => { calls.push(url); return response(); });
    await state.onSend("first question");
    // 只打检索面：不再有预建会话这一步，所以失败后列表里不会多出空会话——
    // 这正是「历史会话点进去打不开」的成因。
    assert.deepEqual(calls, ["/v1/search/stream"]);
    assert.equal(state.messages.value[0].role, "user");
    assert.equal(state.messages.value[1].status, "error");
    assert.match(state.error.value, expected);
    assert.equal(state.loading.value, false);
    assert.equal(state.sessionsBusy.value, false);
    assert.equal(state.stats.value, null);
    assert.equal(state.completed.length, 0);
  });
}

test("an aborted first question keeps a reusable id and leaves nothing behind", async () => {
  const state = app(async (url, options) => {
    if (url === "/v1/search/stream") return new Promise((resolve, reject) => {
      options.signal.addEventListener("abort", () => reject(Object.assign(new Error("aborted"), { name: "AbortError" })));
    });
    return json([]);
  });
  const inflight = state.onSend("first question");
  state.stop();
  await inflight;
  assert.match(state.current.value, /^[0-9a-f]{12}$/, "the id stays usable for the retry");
  assert.match(state.error.value, /已取消/);
  assert.equal(state.loading.value, false);
});

test("newSession is lazy: it swaps in a fresh local id without creating an empty session", async () => {
  const calls = [];
  const state = app(async (url) => { calls.push(url); return json([]); });
  state.current.value = "old";
  state.messages.value = [{ role: "user", content: "old" }];
  await state.newSession();
  assert.deepEqual(calls.filter((u) => u.startsWith("/v1/sessions/")), [], "no session is created up front");
  assert.match(state.current.value, /^[0-9a-f]{12}$/);
  assert.notEqual(state.current.value, "old");
  assert.equal(state.messages.value.length, 0);
});

for (const method of ["loadSessions", "openSession", "delSession"]) {
  test(`${method} checks HTTP/JSON status and exposes session busy`, async () => {
    const gate = deferred();
    const state = app(async () => gate.promise);
    state.current.value = "old";
    const promise = method === "openSession" ? state[method]({ id: "other" }) : state[method]("old");
    assert.equal(state.sessionsBusy.value, true);
    gate.resolve(json({ error: "denied", hint: "retry later" }, 403));
    await promise;
    assert.equal(state.sessionsBusy.value, false);
    assert.match(state.error.value, /denied.*retry later/);
    assert.equal(state.current.value, method === "openSession" ? "" : "old");
  });
}

test("session delete scopes its body, clears current history only on success, and refreshes", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, options });
    return options.method === "DELETE" ? json({ deleted: true }) : json([]);
  });
  state.current.value = "id/with spaces";
  state.messages.value = [{ role: "user", content: "old" }];
  let stopped = false;
  await state.delSession(state.current.value, { stopPropagation() { stopped = true; } });
  assert.equal(stopped, true);
  assert.equal(calls[0].url, "/v1/sessions/id%2Fwith%20spaces");
  assert.deepEqual(JSON.parse(calls[0].options.body), { ns: "t1" });
  assert.equal(state.current.value, "");
  assert.equal(state.messages.value.length, 0);
  assert.equal(calls[1].url, "/v1/sessions?ns=t1");
});

test("late session open/list responses cannot replace the new namespace", async () => {
  const opened = deferred(), listed = deferred();
  const state = app(async (url) => {
    if (url === "/v1/sessions/old?ns=t1") return opened.promise;
    if (url === "/v1/sessions?ns=t1") return listed.promise;
    if (url === "/v1/sessions/new?ns=t2") return json({ messages: [{ role: "user", content: "new" }] });
    return json([{ id: "new" }]);
  });
  const oldOpen = state.openSession({ id: "old" });
  const oldList = state.loadSessions();
  state.nsSel.value = "t2";
  await state.openSession({ id: "new" });
  await state.loadSessions();
  opened.resolve(json({ messages: [{ role: "user", content: "stale" }] }));
  listed.resolve(json([{ id: "stale" }]));
  await Promise.all([oldOpen, oldList]);
  assert.equal(state.current.value, "new");
  assert.equal(state.messages.value[0].content, "new");
  assert.equal(state.sessions.value[0].id, "new");
  assert.equal(state.sessionsBusy.value, false);
  assert.equal(state.error.value, "");
});

test("SSE handles byte-split UTF-8, CRLF, multiline data and done without waiting for EOF", async () => {
  let cancelled = false;
  const text = ': keepalive\r\nevent:content\r\ndata: {\r\ndata: "text":"中文回答"}\r\n\r\n' +
    'event:done\r\ndata:{"mode":"FAST","tokens":9}\r\n\r\n';
  const state = app(async (url) => url === "/v1/search/stream" ? new Response(new ReadableStream({
    start(controller) { for (const byte of new TextEncoder().encode(text)) controller.enqueue(Uint8Array.of(byte)); },
    cancel() { cancelled = true; },
  })) : json([]));
  state.current.value = "existing";
  await state.onSend("question");
  assert.equal(state.messages.value[1].content, "中文回答");
  assert.equal(state.messages.value[1].status, "done");
  assert.equal(state.stats.value.tokens, 9);
  assert.equal(cancelled, true);
  assert.equal(state.completed.length, 1);
  assert.equal(state.error.value, "");
});

for (const [label, response, expected] of [
  ["event error", () => new Response('event: content\ndata: {"text":"partial"}\n\nevent:error\ndata:{"error":"search failed","hint":"try another bucket"}\n\nevent:done\ndata:{}\n\n'), /search failed.*try another bucket/],
  ["EOF without done", () => new Response('event: content\ndata: {"text":"partial"}\n\n'), /done/],
  ["truncated done frame", () => new Response('event: done\ndata: {"mode":"FAST"}'), /done/],
  ["malformed event JSON", () => new Response('event: content\ndata: {nope}\n\n'), /JSON|Unexpected|property/],
  ["JSON rejection", () => json({ error: "wrong bucket", hint: "select one" }, 400), /wrong bucket.*select one/],
  ["JSON instead of SSE", () => json({ error: "gateway error", hint: "retry" }), /gateway error.*retry/],
  ["unlabelled JSON error", () => new Response('{"error":"gateway error","hint":"retry"}'), /gateway error.*retry/],
]) {
  test(`stream failure is not completion: ${label}`, async () => {
    const state = app(async () => response());
    state.current.value = "existing";
    await state.onSend("question");
    assert.match(state.error.value, expected);
    assert.equal(state.meta.value, "");
    assert.equal(state.stats.value, null);
    assert.equal(state.loading.value, false);
    assert.equal(state.messages.value[1].status, "error");
    assert.equal(state.messages.value[1].thinking, false);
    assert.equal(state.completed.length, 0);
  });
}

test("a refused answer is surfaced as such, not as a normal answer", async () => {
  const state = app(async (url) => {
    if (url === "/v1/search/stream") {
      return new Response(
        'event: status\ndata: {"stage":"refused"}\n\n' +
        'event: content\ndata: {"text":"【DEEP 摘要】工伤是如何认定的\\n⚠ 证据不足：……"}\n\n' +
        'event: done\ndata: {"mode":"DEEP","conf":0.45,"refused":true}\n\n',
      );
    }
    return json([]);
  });
  await state.mount("chat");
  await state.onSend("工伤是如何认定的");
  assert.equal(state.stats.value.refused, true);
  assert.equal(state.stats.value.insufficient, false);
  // 阶段文案在 done 之后会被清掉（正常行为），所以断言落在状态与正文上
  assert.match(state.messages.value[1].content, /证据不足/);
  // 拒答仍是「完成」，不是错误：正文是服务端给的摘要，但状态标记必须为真
  assert.equal(state.messages.value[1].status, "done");
});

test("stop aborts a pending SSE read and retains partial text as cancelled", async () => {
  let signal, cancelled = false;
  const state = app(async (_url, options) => {
    signal = options.signal;
    return new Response(new ReadableStream({
      start(controller) { controller.enqueue(new TextEncoder().encode('event: content\ndata: {"text":"partial"}\n\n')); },
      cancel() { cancelled = true; },
    }));
  });
  state.current.value = "existing";
  const pending = state.onSend("question");
  await tick();
  assert.equal(state.messages.value[1].content, "partial");
  state.stop();
  assert.equal(signal.aborted, true);
  assert.equal(state.loading.value, false);
  await pending;
  assert.equal(cancelled, true);
  assert.equal(state.messages.value[1].content, "partial");
  assert.equal(state.messages.value[1].status, "cancelled");
  assert.match(state.error.value, /取消/);
  assert.equal(state.stats.value, null);
  assert.equal(state.completed.length, 0);
});

test("cancelled first send cannot overwrite a newer send", async () => {
  const gate = deferred();
  let firstSignal;
  const server = chatBackend((url, options, body) => {
    if (url === "/v1/search/stream" && body?.query === "old") { firstSignal = options.signal; return gate.promise; }
  });
  const state = app(server.fetch);
  const old = state.onSend("old");
  await tick();
  state.stop();
  assert.equal(firstSignal.aborted, true);
  assert.equal(state.sessionsBusy.value, false);
  await state.onSend("new");
  gate.resolve(answer("late"));
  await old;
  assert.match(state.messages.value[1].status, /cancelled/);
  assert.equal(state.messages.value[3].status, "done");
  assert.equal(server.calls.filter((c) => c.url === "/v1/search/stream").length, 2);
  assert.equal(state.error.value, "");
});

for (const action of ["open", "new", "namespace", "unmount"]) {
  test(`${action} cancels generation and ignores a late response`, async () => {
    const gate = deferred();
    let signal;
    const server = chatBackend((url, options) => {
      if (url === "/v1/search/stream") { signal = options.signal; return gate.promise; }
      if (url.startsWith("/v1/sessions/target")) return json({ messages: [{ role: "user", content: "target history" }] });
    });
    const state = app(server.fetch);
    const pending = state.onSend("old question");
    await tick();
    if (action === "open") await state.openSession({ id: "target" });
    if (action === "new") await state.newSession();
    if (action === "namespace") state.nsSel.value = "t2";
    if (action === "unmount") state.unmount("chat");
    const before = plain(state.messages.value);
    gate.resolve(answer("late answer"));
    await pending;
    assert.equal(signal.aborted, true);
    assert.deepEqual(plain(state.messages.value), before);
    assert.equal(state.loading.value, false);
    assert.equal(state.completed.length, 0);
    assert.equal(state.stats.value, null);
  });
}

test("all three ingestion entrances block a missing namespace before making requests", async () => {
  const calls = [];
  const state = app(async (url) => { calls.push(url); return json({}); }, undefined, "");
  state.ingDir.value = "/d";
  state.adPaths.value = "/d/a.json";
  await state.startIngest();
  await state.ingestPicked();
  await state.submitAdapt();
  assert.equal(calls.length, 0);
  for (const target of [state.ingMeta, state.scanMeta, state.adMeta]) assert.match(target.value, /选择知识库/);
  assert.equal(state.jobs.value.length, 0);
});

test("same job id in two namespaces stays isolated and polls each job's own namespace", async () => {
  const reads = [];
  const state = app(async (url, options) => {
    if (url === "/v1/ingest/jobs") return json({ job: "shared", state: "queued", total: JSON.parse(options.body).ns === "t1" ? 1 : 2 });
    if (url.startsWith("/v1/ingest/jobs/")) {
      reads.push(url);
      return json({ state: "done", records: url.endsWith("t1") ? 11 : 22, ns: "ignored-server-ns" });
    }
    return json([]);
  });
  state.ingDir.value = "/d";
  await state.startIngest();
  state.nsSel.value = "t2";
  assert.equal(state.jobs.value.length, 0);
  assert.equal(state.jobCur.value, "");
  await state.startIngest();
  assert.equal(state.jobs.value.length, 1);
  assert.equal(state.curJob().total, 2);
  await state.pollJobs();
  assert.deepEqual(reads.sort(), ["/v1/ingest/jobs/shared?ns=t1", "/v1/ingest/jobs/shared?ns=t2"]);
  assert.equal(state.curJob().records, 22);
  state.nsSel.value = "t1";
  assert.equal(state.jobs.value.length, 1);
  assert.equal(state.jobCur.value, "shared");
  assert.equal(state.curJob().records, 11);
  assert.equal(state.curJob().ns, "t1");
});

for (const route of ["directory", "scan", "adapt"]) {
  test(`late ${route} submit survives unmount without joining the newly selected namespace`, async () => {
    const gate = deferred();
    const posts = [];
    const state = app(async (url, options) => {
      if (url === "/v1/scan") return json({ candidates: [{ path: "/d/a.md" }] });
      if (url === "/v1/adapt/probe") return json({ probes: [{ path: "/d/a.json", bodyish: ["text"], idish: ["id"] }] });
      if (url === "/v1/ingest/jobs" || url === "/v1/adapt/ingest") { posts.push(JSON.parse(options.body)); return gate.promise; }
      return json([]);
    });
    state.pane.value = "documents";
    await state.mount("ingest");
    let pending;
    if (route === "directory") { state.ingDir.value = "/d"; pending = state.startIngest(); }
    if (route === "scan") { state.scanDir.value = "/d"; await state.runScan(); pending = state.ingestPicked(); }
    if (route === "adapt") { state.adPaths.value = "/d/a.json"; await state.runAdaptProbe(); pending = state.submitAdapt(); }
    assert.equal(posts[0].ns, "t1");
    if (route === "adapt") assert.equal(posts[0].body, "text");
    state.nsSel.value = "t2";
    const next = await state.remountIngest();
    gate.resolve(json({ job: "late", state: "queued", total: 1, skipped: 1, records: 3, skip_errors: { "/d/a": "empty" } }));
    await pending;
    assert.equal(next.jobs.value.length, 0);
    assert.equal(next.jobCur.value, "");
    assert.doesNotMatch(next.ingMeta.value + next.scanMeta.value + next.adMeta.value, /已提交/);
    // The new mounted instance observes the persisted queue and owns the timer.
    assert.equal(state.timers.size, 1);
    state.nsSel.value = "t1";
    assert.equal(next.curJob().id, "late");
    assert.equal(next.curJob().ns, "t1");
    assert.equal(next.curJob().skipped, 1);
    assert.equal(next.curJob().records, 3);
    assert.equal(next.curJob().skip_errors["/d/a"], "empty");
  });
}

test("polling errors preserve job state, expose pollError and allow manual retry", async () => {
  let attempt = 0;
  const state = app(async (url) => {
    if (url === "/v1/ingest/jobs") return json({ job: "j", state: "queued" });
    attempt++;
    if (attempt === 1) return json({ error: "temporarily missing", hint: "retry" }, 404);
    if (attempt === 2) return new Response("not JSON");
    if (attempt === 3) throw new Error("offline");
    return json({ state: "done", done: 1, records: 20 });
  });
  state.pane.value = "documents";
  await state.mount("ingest");
  state.ingDir.value = "/d";
  await state.startIngest();
  for (const expected of [/temporarily missing.*retry/, /JSON/, /offline/]) {
    await state.pollJobs();
    assert.equal(state.curJob().state, "queued");
    assert.equal(state.curJob().error, "");
    assert.match(state.curJob().pollError, expected);
    assert.match(state.pollError.value, expected);
    assert.equal(state.timers.size, 0);
  }
  await state.pollJobs();
  assert.equal(state.curJob().state, "done");
  assert.equal(state.curJob().pollError, "");
  assert.equal(state.pollError.value, "");
});

test("poll timers run on documents, not chat, and an unmounted in-flight read cannot revive them", async () => {
  const gate = deferred();
  let signal;
  const state = app(async (url, options) => {
    if (url === "/v1/ingest/jobs") return json({ job: "j", state: "queued" });
    signal = options.signal;
    return gate.promise;
  });
  await state.mount("ingest");
  state.ingDir.value = "/d";
  await state.startIngest();
  assert.equal(state.timers.size, 0, "chat no longer embeds ingestion");
  state.pane.value = "documents";
  assert.equal(state.timers.size, 1);
  state.pane.value = "settings";
  assert.equal(state.timers.size, 0);
  state.pane.value = "documents";
  const pending = state.pollJobs();
  const duplicate = state.pollJobs();
  assert.equal(pending, duplicate, "manual retry coalesces an active poll");
  state.unmount("ingest");
  assert.equal(signal.aborted, true);
  gate.resolve(json({ state: "running", done: 1 }));
  await pending;
  assert.equal(state.timers.size, 0);
  assert.equal(state.curJob().state, "queued");
  assert.equal(state.curJob().pollError, "");
});

test("scan results clear on rerun, failure and input changes; stale scan responses are ignored", async () => {
  const gate = deferred();
  let attempt = 0;
  const state = app(async (url) => {
    if (url !== "/v1/scan") return json([]);
    attempt++;
    if (attempt === 1) return json({ candidates: [{ path: "/d/a.md" }] });
    if (attempt === 2) return json({ error: "scan denied" }, 403);
    return gate.promise;
  });
  state.scanDir.value = "/d";
  await state.runScan();
  assert.equal(state.pickedCount(), 1);
  const failed = state.runScan();
  assert.equal(state.scanReport.value, null);
  assert.equal(state.pickedCount(), 0);
  await failed;
  assert.match(state.scanMeta.value, /scan denied/);
  const stale = state.runScan();
  state.scanDir.value = "/new";
  gate.resolve(json({ candidates: [{ path: "/d/stale.md" }] }));
  await stale;
  assert.equal(state.scanReport.value, null);
  assert.equal(state.scanBusy.value, false);
  await state.ingestPicked();
  assert.match(state.scanMeta.value, /重新扫描/);
});

test("probe mapping is invalidated by path changes, failed probes and late responses", async () => {
  const gate = deferred();
  let attempt = 0, submitted = 0;
  const state = app(async (url) => {
    if (url === "/v1/adapt/ingest") { submitted++; return json({ job: "unexpected" }); }
    if (url !== "/v1/adapt/probe") return json([]);
    attempt++;
    if (attempt === 1) return json({ probes: [{ path: "/a.json", bodyish: ["old_text"] }] });
    if (attempt === 2) return json({ error: "cannot probe" }, 400);
    return gate.promise;
  });
  state.adPaths.value = "/a.json";
  await state.runAdaptProbe();
  assert.equal(state.adMap.value.body, "old_text");
  state.adPaths.value = "/b.json";
  assert.equal(state.adProbes.value.length, 0);
  assert.equal(state.adMap.value.body, "");
  await state.submitAdapt();
  assert.match(state.adMeta.value, /重新探测/);
  await state.runAdaptProbe();
  assert.equal(state.adMap.value.body, "");
  assert.match(state.adMeta.value, /cannot probe/);
  const pending = state.runAdaptProbe();
  state.adPaths.value = "/c.json";
  gate.resolve(json({ probes: [{ path: "/b.json", bodyish: ["stale_text"] }] }));
  await pending;
  await state.submitAdapt();
  assert.equal(state.adProbes.value.length, 0);
  assert.equal(state.adMap.value.body, "");
  assert.equal(state.adBusy.value, false);
  assert.equal(submitted, 0);
});

test("a superseded list request cannot keep sessionsBusy stuck or replace the newest list", async () => {
  const gate = deferred();
  let calls = 0, oldSignal;
  const state = app(async (_url, options) => {
    if (++calls === 1) { oldSignal = options.signal; return gate.promise; }
    return json([{ id: "newest" }]);
  });
  const old = state.loadSessions();
  await state.loadSessions();
  assert.equal(oldSignal.aborted, true);
  assert.equal(state.sessionsBusy.value, false);
  assert.equal(state.sessions.value[0].id, "newest");
  gate.resolve(json([{ id: "stale" }]));
  await old;
  assert.equal(state.sessions.value[0].id, "newest");
});

test("late background list errors cannot overwrite the active search error", async () => {
  const gate = deferred();
  const state = app(async (url) => url.startsWith("/v1/sessions") ? gate.promise : json({ error: "search failed" }, 500));
  state.current.value = "existing";
  const listed = state.loadSessions();
  await state.onSend("question");
  gate.resolve(json({ error: "list failed" }, 500));
  await listed;
  assert.match(state.error.value, /search failed/);
});

test("deleting the active session blocks sends until its result is known", async () => {
  const gate = deferred();
  const calls = [];
  const state = app(async (url, options) => {
    calls.push(url);
    return options.method === "DELETE" ? gate.promise : json([]);
  });
  state.current.value = "existing";
  const deleting = state.delSession("existing");
  await state.onSend("must not race delete");
  assert.deepEqual(calls, ["/v1/sessions/existing"]);
  gate.resolve(json({ deleted: true }));
  await deleting;
  assert.equal(state.current.value, "");
  assert.equal(state.sessionsBusy.value, false);
});

test("late submit on an unmounted instance records the job without resurrecting a timer", async () => {
  const gate = deferred();
  const state = app(async () => gate.promise);
  state.pane.value = "documents";
  await state.mount("ingest");
  state.ingDir.value = "/d";
  const submitted = state.startIngest();
  state.unmount("ingest");
  gate.resolve(json({ job: "late", state: "queued" }));
  await submitted;
  assert.equal(state.jobs.value[0].ns, "t1");
  assert.equal(state.ingBusy.value, false);
  assert.equal(state.timers.size, 0);
  await state.remountIngest();
  assert.equal(state.timers.size, 1);
});

test("automatic polls skip jobs with read errors while healthy jobs continue", async () => {
  const reads = [];
  let creates = 0;
  const state = app(async (url) => {
    if (url === "/v1/ingest/jobs") return json({ job: ++creates === 1 ? "bad" : "good", state: "queued" });
    reads.push(url);
    return url.includes("/bad?") ? json({ error: "not found" }, 404) : json({ state: "running" });
  });
  state.pane.value = "documents";
  await state.mount("ingest");
  state.ingDir.value = "/d";
  await state.startIngest();
  await state.startIngest();
  await state.runTimer();
  assert.equal(reads.length, 2);
  await state.runTimer();
  assert.equal(reads.length, 3);
  assert.match(reads.at(-1), /\/good\?/);
  await state.pollJobs();
  assert.equal(reads.filter((url) => url.includes("/bad?")).length, 2);
});

test("remount after an off-screen namespace change clears old scan results and probe mappings", async () => {
  const state = app(async (url) => {
    if (url === "/v1/scan") return json({ candidates: [{ path: "/d/a.md" }] });
    if (url === "/v1/adapt/probe") return json({ probes: [{ path: "/d/a.json", bodyish: ["old_text"] }] });
    return json([]);
  });
  state.scanDir.value = "/d";
  state.adPaths.value = "/d/a.json";
  await state.runScan();
  await state.runAdaptProbe();
  state.unmount("ingest");
  state.nsSel.value = "t2";
  await state.remountIngest();
  assert.equal(state.scanReport.value, null);
  assert.equal(state.pickedCount(), 0);
  assert.equal(state.adProbes.value.length, 0);
  assert.equal(state.adMap.value.body, "");
});

test("namespace names matching Object prototype keys are valid job identities", async () => {
  const state = app(async () => json({ job: "j", state: "queued" }), undefined, "constructor");
  assert.equal(state.ingBusy.value, false);
  assert.equal(state.jobCur.value, "");
  state.ingDir.value = "/d";
  await state.startIngest();
  assert.equal(state.curJob().ns, "constructor");
  assert.equal(state.curJob().id, "j");
});

function uploadFile(path, content = "测试正文") {
  const file = new File([content], path.split("/").pop());
  Object.defineProperty(file, "webkitRelativePath", { value: path });
  return file;
}

test("directory selection previews supported files without uploading and keeps relative names", () => {
  const state = app(() => { throw new Error("selection must remain local"); });
  state.selectDirectory([uploadFile("docs/a.md"), uploadFile("docs/sub/a.md"), uploadFile("docs/.secret.txt"), uploadFile("docs/data.csv")]);
  assert.deepEqual(plain(state.uploadFiles.value.map(f => f.name)), ["docs/a.md", "docs/sub/a.md"]);
  assert.match(state.uploadMeta.value, /2.*跳过/);
  state.selectDirectory([]);
  assert.equal(state.uploadFiles.value.length, 2, "cancelled chooser preserves selection");
});

test("confirmed upload sends one namespaced multipart batch and enters the existing job queue", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, options });
    return json({ job: "uploaded", state: "queued", total: 2 }, 202);
  });
  state.selectDirectory([uploadFile("docs/a.md"), uploadFile("docs/sub/a.md")]);
  await state.startUpload();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/v1/ingest/upload?ns=t1");
  assert.equal(calls[0].options.headers, undefined, "browser must supply multipart boundary");
  assert.deepEqual(calls[0].options.body.getAll("files").map(f => f.name), ["docs/a.md", "docs/sub/a.md"]);
  assert.equal(state.curJob().ns, "t1");
  assert.equal(state.curJob().total, 2);
  assert.equal(state.uploadFiles.value.length, 0);
  assert.equal(state.uploadBusy.value, false);
});

test("upload validates empty selections, namespace and file limits before sending", async () => {
  let calls = 0;
  const state = app(async url => { if (url.startsWith("/v1/ingest")) calls++; return json([]); });
  await state.startUpload();
  assert.match(state.uploadMeta.value, /选择/);
  const huge = uploadFile("large.md");
  Object.defineProperty(huge, "size", { value: 16 * 1024 * 1024 + 1 });
  state.selectDirectory([huge]);
  await state.startUpload();
  assert.match(state.uploadMeta.value, /16/);
  state.selectDirectory(Array.from({ length: 501 }, (_, i) => uploadFile(i + ".md")));
  await state.startUpload();
  assert.match(state.uploadMeta.value, /500/);
  const batch = Array.from({ length: 5 }, (_, i) => {
    const file = uploadFile(i + ".md");
    Object.defineProperty(file, "size", { value: 16 * 1024 * 1024 });
    return file;
  });
  state.selectDirectory(batch);
  await state.startUpload();
  assert.match(state.uploadMeta.value, /64/);
  state.nsSel.value = "";
  await state.startUpload();
  assert.match(state.uploadMeta.value, /知识库/);
  assert.equal(calls, 0);
});

test("upload failures keep the selection and show backend hints", async () => {
  const state = app(async () => json({ error: "too large", hint: "split batch" }, 413));
  state.selectDirectory([uploadFile("docs/a.md")]);
  await state.startUpload();
  assert.equal(state.uploadFiles.value.length, 1);
  assert.match(state.uploadMeta.value, /too large.*split batch/);
  assert.equal(state.uploadBusy.value, false);
  assert.equal(state.jobs.value.length, 0);
});

test("late upload stays in its original library and cannot clear the next library's selection", async () => {
  const gate = deferred();
  const state = app(async url => url.startsWith("/v1/ingest/upload") ? gate.promise : json([]));
  state.selectDirectory([uploadFile("alpha/a.md")]);
  const pending = state.startUpload();
  state.nsSel.value = "t2";
  assert.equal(state.uploadFiles.value.length, 0);
  state.selectDirectory([uploadFile("beta/b.md")]);
  gate.resolve(json({ job: "alpha", state: "queued", total: 1 }));
  await pending;
  assert.equal(state.jobs.value.length, 0);
  assert.equal(state.uploadFiles.value[0].name, "beta/b.md");
  state.nsSel.value = "t1";
  assert.equal(state.curJob().id, "alpha");
});

test("search retains complete backend metrics and surfaces insufficient evidence", async () => {
  const server = chatBackend(url => url === "/v1/search/stream" ? new Response(
    'event: status\ndata: {"stage":"insufficient-evidence"}\n\n' +
    'event: content\ndata: {"text":"证据不足"}\n\n' +
    'event: done\ndata: {"mode":"DEEP","coverage":0.25,"reused":true,"cluster_id":"c1","stop_reason":"budget","conf":0.3}\n\n'
  ) : undefined);
  const state = app(server.fetch);
  await state.onSend("question");
  assert.equal(state.stats.value.coverage, 0.25);
  assert.equal(state.stats.value.reused, true);
  assert.equal(state.stats.value.cluster_id, "c1");
  assert.equal(state.stats.value.stop_reason, "budget");
  assert.equal(state.stats.value.insufficient, true);
});

test("upload batches cannot double-submit or mutate their selection while pending", async () => {
  const gate = deferred();
  let calls = 0;
  const state = app(async () => { calls++; return gate.promise; });
  state.selectDirectory([uploadFile("docs/a.md")]);
  const pending = state.startUpload();
  state.updateUploadFiles([]);
  state.selectDirectory([uploadFile("replacement.md")]);
  await state.startUpload();
  assert.equal(calls, 1);
  assert.equal(state.uploadFiles.value[0].name, "docs/a.md");
  state.unmount("ingest");
  gate.resolve(json({ job: "offscreen", state: "queued", total: 1 }));
  await pending;
  assert.equal(state.uploadFiles.value.length, 0);
  assert.match(state.uploadMeta.value, /已提交任务/);
  assert.equal(state.curJob().id, "offscreen");
  assert.equal(state.timers.size, 0);
});

test("file chooser additions accumulate and duplicate paths are excluded", () => {
  const state = app(() => { throw new Error("selection must remain local"); });
  for (const name of ["a.md", "b.md", "a.md"]) {
    const raw = uploadFile(name);
    state.updateUploadFiles([...state.uploadFiles.value, { raw, name, size: raw.size, uid: name }]);
  }
  assert.deepEqual(plain(state.uploadFiles.value.map(f => f.name)), ["a.md", "b.md"]);
  assert.match(state.uploadMeta.value, /1.*跳过/);
});

// 流式合成：content 是增量，replace 事件整段替换（流式失败回退时屏幕不叠加），
// stage 状态事件实时累积成时间轴，done 的权威分段落到消息上。
test("streaming synthesis, stage timeline and replace semantics", async () => {
  const server = chatBackend(url => url === "/v1/search/stream" ? new Response(
    'event: status\ndata: {"stage":"stage","name":"analyze","stage_ms":1151,"elapsed_ms":1152}\n\n' +
    'event: status\ndata: {"stage":"stage","name":"cascade","stage_ms":340,"elapsed_ms":1492}\n\n' +
    'event: content\ndata: {"text":"# 标题\\n"}\n\n' +
    'event: content\ndata: {"text":"第一段。"}\n\n' +
    'event: content\ndata: {"text":"# 完整答案","replace":true}\n\n' +
    'event: citations\ndata: {"refs":[{"index":1,"title":"法.txt","source_id":"src:法","quote":"条文","resolved":true}]}\n\n' +
    'event: done\ndata: {"mode":"DEEP","conf":0.7,"coverage":0.5,"loops":3,"tokens":1234,"latency_ms":5000,"reused":false,"cluster_id":"c9","stop_reason":"sufficient","stages":{"analyze":1151000,"cascade":340000,"deep_sample":900000,"deep_synth":600000}}\n\n'
  ) : undefined);
  const state = app(server.fetch);
  await state.onSend("问题");
  const msg = state.messages.value[1];
  // replace 语义：最终内容是权威全文，不是增量拼接。
  assert.equal(msg.content, "# 完整答案");
  // 实时时间轴：两个 stage 事件已累积。
  assert.deepEqual(plain(msg.stages.map(s => [s.name, s.ms])), [["analyze", 1151], ["cascade", 340]]);
  // done 的权威分段（微秒）与运行卡落到本条消息。
  assert.equal(msg.stats.mode, "DEEP");
  assert.equal(msg.stats.tokens, 1234);
  assert.equal(msg.stats.cluster_id, "c9");
  assert.deepEqual(Object.keys(msg.stats.stages).sort(), ["analyze", "cascade", "deep_sample", "deep_synth"]);
  // 引用挂到本条消息（历史恢复靠它）。
  assert.equal(msg.sources[0].source, "src:法");
});

// 刷新持久化：会话文档里带回 sources/stats 时，重新打开会话引用卡和运行卡都在。
test("history restore carries citations, stats and stage timeline", async () => {
  const server = chatBackend();
  const state = app(server.fetch);
  await state.onSend("第一问");
  await state.onSend("第二问");
  const firstID = server.calls.find(c => c.url === "/v1/search/stream").body.session;
  // 模拟服务端会话文档已随答案存下引用与运行卡（sessionMessage 的 sources/stats）。
  const doc = server.sessions.get(firstID);
  doc.messages[1].at = 1700000000000;
  doc.messages[1].sources = [{ index: 1, title: "源.txt", source_id: "src:a", quote: "条文一", resolved: true }];
  doc.messages[1].stats = { mode: "FAST", conf: 0.8, coverage: 0.6, loops: 0, tokens: 900, latency: 3200, reused: true, cluster_id: "cx", stages: { analyze: 1200000, cascade: 300000, sample: 800000, synth: 900000 } };
  await state.openSession({ id: firstID });
  const restored = state.messages.value[1];
  assert.equal(restored.sources.length, 1);
  assert.equal(restored.sources[0].source, "src:a");
  assert.equal(restored.stats.mode, "FAST");
  assert.equal(restored.stats.reused, true);
  // 时间轴从 stats.stages 还原（微秒 → 毫秒）。
  assert.deepEqual(plain(restored.stages.map(s => [s.name, s.ms])), [["analyze", 1200], ["cascade", 300], ["sample", 800], ["synth", 900]]);
  assert.equal(restored.createdAt, 1700000000000);
});

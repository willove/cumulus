import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./App.vue", import.meta.url), "utf8");
const script = source.match(/<script setup>([\s\S]*?)<\/script>/)[1].replace(/^import .*;$/gm, "");

function app(fetch, thinkError) {
  const messages = { value: [] };
  const context = vm.createContext({
    ref: (value) => ({ value }),
    onMounted: () => {},
    nextTick: (fn) => fn(),
    setTimeout: () => 0,
    setInterval: () => 1,
    clearInterval: () => {},
    clearTimeout: () => {},
    TextDecoder,
    fetch,
    useChatEngine: () => ({
      messages,
      createAssistantMessage() {
        const message = { id: "assistant", content: "", status: "loading" };
        messages.value.push(message);
        return message;
      },
      appendThinkContent(id, text) {
        if (thinkError) throw thinkError;
        messages.value.find((m) => m.id === id).thinkContent = text;
      },
      stopThinking: () => {},
      appendContent(id, text) { messages.value.find((m) => m.id === id).content += text; },
      completeMessage(id) { messages.value.find((m) => m.id === id).status = "done"; },
    }),
  });
  vm.runInContext(script + "\nglobalThis.app = { onSend, loading, meta, messages, sources, pane, ingDir, ingRec, ingName, ingBusy, ingMeta, jobs, jobCur, startIngest, pollJobs, openPane, selectJob, curJob, nsSel, onNsChange, loadSessions, loadClusters, settingsModel, settingsConfig, settingsBusy, settingsMsg, installWeights, verifyWeights, loadSettings, fmtMB, scanDir, scanLimit, scanNewer, scanBusy, scanReport, scanPicked, scanMeta, runScan, ingestPicked, pickedCount, toggleAllScan, evalRuns, evalCur, evalLoading, loadEvals, openEval, pct, txRatio };", context);
  return context.app;
}

test("send reaches search and completes streamed answer", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body) });
    return new Response([
      'event: content\ndata: {"text":"answer"}\n\n',
      'event: citations\ndata: {"refs":[{"index":1,"source_id":"src:a","quote":"evidence"}]}\n\n',
      'event: done\ndata: {"mode":"FAST","conf":0.8}\n\n',
    ].join(""));
  });
  await state.onSend("question");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/v1/search/stream");
  assert.equal(calls[0].body.query, "question");
  assert.equal(state.messages.value[0].content, "answer");
  assert.ok(state.messages.value[0].thinkContent);
  assert.equal(state.messages.value[0].status, "done");
  assert.equal(state.sources.value[0].source, "src:a");
  assert.equal(state.loading.value, false);
});

test("HTTP error settles loading and surfaces error", async () => {
  const state = app(async () => new Response("unavailable", { status: 503 }));
  await state.onSend("question");
  assert.match(state.meta.value, /HTTP 503/);
  assert.equal(state.loading.value, false);
  assert.equal(state.messages.value[0].status, "done");
});

test("the namespace selector scopes every face", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: options && options.body ? JSON.parse(options.body) : null });
    if (url.startsWith("/v1/sessions")) return new Response("[]");
    if (url.startsWith("/v1/clusters?")) return new Response(JSON.stringify({ clusters: [] }));
    if (url.startsWith("/v1/clusters/")) return new Response("{}");
    return new Response([
      'event: content\ndata: {"text":"answer"}\n\n',
      'event: done\ndata: {"mode":"FAST","conf":0.8}\n\n',
    ].join(""));
  });
  state.nsSel.value = "t1";
  // Search carries the ns in the body.
  await state.onSend("question");
  assert.equal(calls[0].body.ns, "t1");
  // Cluster list and detail carry it in the query.
  await state.loadClusters();
  assert.match(calls[calls.length - 1].url, /\/v1\/clusters\?limit=200&ns=t1/);
  await state.loadSessions();
  assert.match(calls[calls.length - 1].url, /\/v1\/sessions\?ns=t1/);
  // Empty selector sends nothing (serve-level -ns applies).
  state.nsSel.value = "";
  await state.onSend("question");
  assert.equal(calls[calls.length - 1].body.ns, undefined);
});

test("the settings pane loads weight status and endpoint config", async () => {
  const urls = [];
  const state = app(async (url) => {
    urls.push(url);
    if (url === "/v1/model") return new Response(JSON.stringify({ installed: false, dir: "/h/.cumulus/models/x", dims: 384, files: [] }));
    if (url === "/v1/config") return new Response(JSON.stringify({ base_url: "", chat_model: "", api_key_set: false }));
    return new Response("{}");
  });
  await state.openPane("settings");
  assert.equal(state.pane.value, "settings");
  assert.deepEqual(urls, ["/v1/model", "/v1/config"]);
  assert.equal(state.settingsModel.value.installed, false);
  assert.equal(state.settingsConfig.value.api_key_set, false);
});

test("installWeights starts the download and verify runs the weights", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, method: options && options.method });
    if (url === "/v1/model" && (!options || options.method === undefined || options.method === "GET")) {
      return new Response(JSON.stringify({ installed: true, dir: "/h/.cumulus/models/x", files: [{ name: "model.safetensors", size: 470641600 }] }));
    }
    if (url === "/v1/model" && options.method === "POST") {
      return new Response(JSON.stringify({ started: true }));
    }
    if (url === "/v1/model/verify") {
      return new Response(JSON.stringify({ ok: true, dims: 384, ms: 812 }));
    }
    return new Response("{}");
  });
  await state.installWeights();
  assert.equal(calls[calls.length - 1].method, "POST");
  assert.match(state.settingsMsg.value, /下载中/);
  await state.verifyWeights();
  assert.match(state.settingsMsg.value, /验证通过：384 维 · 812ms/);
  assert.equal(state.fmtMB(470641600), "470.6 MB");
});

test("scan discovers candidates and the picked ones become a job", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: options && options.body ? JSON.parse(options.body) : null });
    if (url === "/v1/scan") {
      return new Response(JSON.stringify({
        candidates: [
          { path: "/d/a.md", size: 2048, age_days: 1, headline: "连接池" },
          { path: "/d/b.txt", size: 99, age_days: 3, headline: "runbook" },
        ],
        skipped: { ext: 1, old: 1 },
      }));
    }
    if (url === "/v1/ingest/jobs") return new Response(JSON.stringify({ job: "j1", state: "queued", total: 1 }));
    return new Response("{}");
  });
  state.scanDir.value = "/d";
  await state.runScan();
  assert.equal(state.scanReport.value.candidates.length, 2);
  assert.deepEqual(state.scanPicked.value, [true, true]);
  assert.match(state.scanMeta.value, /候选 2 个 · 跳过 ext×1 old×1/);
  // Unpick the second; only the first goes to the job.
  state.scanPicked.value[1] = false;
  await state.ingestPicked();
  const post = calls[calls.length - 1];
  assert.equal(post.url, "/v1/ingest/jobs");
  assert.deepEqual(post.body.candidates, ["/d/a.md"]);
  assert.equal(state.jobs.value[0].id, "j1");
  assert.equal(state.jobs.value[0].total, 1);
});

test("the scoreboard pane lists runs and opens one", async () => {
  const urls = [];
  const state = app(async (url) => {
    urls.push(url);
    if (url.startsWith("/v1/evals?")) {
      return new Response(JSON.stringify({ runs: [
        { _id: "run:1", tag: "chinalaw39", n: 39, judged: true, system: { em: 0.667 } },
        { _id: "run:2", tag: "baseline", n: 39, judged: false, system: { em: 0.433 } },
      ] }));
    }
    if (url.startsWith("/v1/evals/")) {
      return new Response(JSON.stringify({
        _id: "run:1", tag: "chinalaw39", n: 39, judged: true, at: "2026-09-24T00:00:00Z",
        system: { em: 0.667, ev_rec: 0.433, ground: 0.967, taxonomy: { correct: 20, retrieved_but_unanswered: 1, answered_but_wrong: 8, not_retrieved: 10 } },
        closed_book: { em: 0.7 }, mcnemar: { b_only: 5, c_only: 12, p: 0.144 },
        modes: { DEEP: 30, FAST: 9 }, search_tokens: 164672, judge_tokens: 5349,
        extra: { frozen: { items_sha: "abcdef0123456789" } },
      }));
    }
    return new Response("{}");
  });
  await state.openPane("evals");
  assert.equal(state.pane.value, "evals");
  assert.equal(state.evalRuns.value.length, 2);
  assert.match(urls[0], /\/v1\/evals\?limit=50/);
  await state.openEval("run:1");
  assert.equal(state.evalCur.value.tag, "chinalaw39");
  assert.equal(state.pct(0.667), "66.7%");
  assert.ok(Math.abs(state.txRatio({ correct: 20, retrieved_but_unanswered: 1, answered_but_wrong: 8, not_retrieved: 10 }, "correct") - 20 / 39) < 1e-9);
  // Namespace selector scopes the scoreboard too.
  state.nsSel.value = "t1";
  await state.loadEvals();
  assert.match(urls[urls.length - 1], /ns=t1/);
});

test("thinking initialization error settles loading", async () => {
  const state = app(async () => { throw new Error("unexpected request"); }, new Error("thinking failed"));
  await state.onSend("question");
  assert.match(state.meta.value, /thinking failed/);
  assert.equal(state.loading.value, false);
});

test("startIngest without a dir stays local", async () => {
  const calls = [];
  const state = app(async (url) => { calls.push(url); return new Response("{}"); });
  await state.startIngest();
  assert.equal(calls.length, 0);
  assert.match(state.ingMeta.value, /请先填写目录/);
  assert.equal(state.ingBusy.value, false);
});

test("startIngest posts the dir and queues the job", async () => {
  const calls = [];
  const state = app(async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body), method: options.method });
    return new Response(JSON.stringify({ job: "docs1", state: "queued", total: 3 }), { status: 202 });
  });
  state.ingDir.value = "/data/docs";
  state.ingRec.value = true;
  state.ingName.value = "docs1";
  await state.startIngest();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/v1/ingest/jobs");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, { dir: "/data/docs", recursive: true, job: "docs1" });
  assert.equal(state.jobs.value.length, 1);
  assert.equal(state.jobs.value[0].id, "docs1");
  assert.equal(state.jobs.value[0].state, "queued");
  assert.equal(state.jobs.value[0].total, 3);
  assert.equal(state.jobCur.value, "docs1");
  assert.match(state.ingMeta.value, /已提交任务 docs1/);
});

test("startIngest surfaces a rejected job", async () => {
  const state = app(async () => new Response(JSON.stringify({ error: "dir not found" }), { status: 400 }));
  state.ingDir.value = "/nope";
  await state.startIngest();
  assert.equal(state.jobs.value.length, 0);
  assert.match(state.ingMeta.value, /dir not found/);
  assert.equal(state.ingBusy.value, false);
});

test("pollJobs folds the job state machine and stops when terminal", async () => {
  const seen = [];
  const state = app(async (url) => {
    seen.push(url);
    if (url === "/v1/ingest/jobs") {
      return new Response(JSON.stringify({ job: "docs1", state: "queued", total: 3 }), { status: 202 });
    }
    if (seen.filter((u) => u.startsWith("/v1/ingest/jobs/")).length === 1) {
      return new Response(JSON.stringify({ state: "running", phase: "extracting", total: 3, done: 1 }));
    }
    return new Response(JSON.stringify({ state: "done", phase: "upserting", total: 3, done: 3, failed: 0 }));
  });
  state.ingDir.value = "/data/docs";
  state.ingName.value = "docs1";
  await state.startIngest();
  await state.pollJobs();
  assert.equal(state.jobs.value[0].state, "running");
  assert.equal(state.jobs.value[0].done, 1);
  assert.equal(state.jobs.value[0].phase, "extracting");
  await state.pollJobs();
  assert.equal(state.jobs.value[0].state, "done");
  assert.equal(state.jobs.value[0].done, 3);
  const before = seen.length;
  await state.pollJobs(); // terminal: no further status GETs
  assert.equal(seen.length, before);
});

test("a failed job carries its error into the detail view", async () => {
  const state = app(async (url) => {
    if (url === "/v1/ingest/jobs") {
      return new Response(JSON.stringify({ job: "docs1", state: "queued", total: 3 }), { status: 202 });
    }
    return new Response(JSON.stringify({ state: "failed", phase: "extracting", total: 3, done: 1, failed: 1, error: "permission denied" }));
  });
  state.ingDir.value = "/data/docs";
  await state.startIngest();
  await state.pollJobs();
  assert.equal(state.jobs.value[0].state, "failed");
  assert.equal(state.jobs.value[0].error, "permission denied");
  assert.match(state.curJob().error, /permission denied/);
});

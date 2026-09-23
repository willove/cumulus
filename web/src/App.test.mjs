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
  vm.runInContext(script + "\nglobalThis.app = { onSend, loading, meta, messages, sources };", context);
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

test("thinking initialization error settles loading", async () => {
  const state = app(async () => { throw new Error("unexpected request"); }, new Error("thinking failed"));
  await state.onSend("question");
  assert.match(state.meta.value, /thinking failed/);
  assert.equal(state.loading.value, false);
});

import assert from "node:assert/strict";
import test from "node:test";
import { nsSel, documents, documentsBusy, documentsError, buckets, bucketsError, loadDocuments, loadBuckets, withNS } from "./state.js";
import { requestJSON } from "./api.js";

test("API errors preserve actionable backend hints", async t => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ error: "bucket not registered", hint: "create it first" }), { status: 400 }));
  await assert.rejects(requestJSON("/v1/search"), /bucket not registered.*create it first/);
});

test("non JSON failures are not mistaken for empty data", async t => {
  t.mock.method(globalThis, "fetch", async () => new Response("unavailable", { status: 503 }));
  await assert.rejects(requestJSON("/v1/sources"), /HTTP 503/);
});

test("late source responses cannot replace the selected library", async t => {
  const pending = new Map();
  t.mock.method(globalThis, "fetch", url => new Promise(resolve => pending.set(url, resolve)));
  nsSel.value = "alpha";
  const alpha = loadDocuments();
  nsSel.value = "beta";
  assert.deepEqual(documents.value, []);
  const beta = loadDocuments();
  pending.get("/v1/sources?ns=beta")(Response.json({ sources: [{ id: "beta-doc" }] }));
  await beta;
  pending.get("/v1/sources?ns=alpha")(Response.json({ sources: [{ id: "alpha-doc" }] }));
  await alpha;
  assert.equal(documents.value[0].id, "beta-doc");
  assert.equal(documentsBusy.value, false);
});

test("source load failures remain distinguishable from empty libraries", async t => {
  nsSel.value = "unavailable";
  t.mock.method(globalThis, "fetch", async () => Response.json({ error: "store unavailable" }, { status: 500 }));
  await loadDocuments();
  assert.equal(documentsError.value, "store unavailable");
  assert.equal(documentsBusy.value, false);
});

test("empty selection never silently reads the default corpus", async t => {
  nsSel.value = "";
  const mock = t.mock.method(globalThis, "fetch", async () => { throw new Error("unexpected request"); });
  await loadDocuments();
  assert.equal(mock.mock.callCount(), 0);
  assert.deepEqual(documents.value, []);
});

test("registered serve default is selected and explicit library survives refresh", async t => {
  nsSel.value = "";
  t.mock.method(globalThis, "fetch", async () => Response.json({ default_ns: "beta", buckets: [{ name: "alpha" }, { name: "beta", label: "Beta" }] }));
  await loadBuckets();
  assert.equal(nsSel.value, "beta");
  assert.equal(buckets.value[1].label, "Beta");
  nsSel.value = "alpha";
  await loadBuckets();
  assert.equal(nsSel.value, "alpha");
  assert.equal(bucketsError.value, "");
  assert.equal(withNS("/v1/ingest/jobs/j1", "beta"), "/v1/ingest/jobs/j1?ns=beta");
});

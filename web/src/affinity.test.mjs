import assert from "node:assert/strict";
import test from "node:test";
import { nsSel } from "./state.js";
import { useAffinityPane } from "./panes/affinity.js";

// /v1/affinity 一直有接口、一直没有界面。这组测试钉的是它上墙时最容易错的地方：
// 空词与「查无关联」必须可区分，换词/换库的竞态不能让旧结果覆盖新结果。

function fresh() {
  const pane = useAffinityPane();
  pane.reset();
  return pane;
}

test("an empty term never reaches the network and never reads as 查无关联", async t => {
  nsSel.value = "alpha";
  const pane = fresh();
  const mock = t.mock.method(globalThis, "fetch", async () => { throw new Error("unexpected request"); });

  await pane.lookup("   ");

  assert.equal(mock.mock.callCount(), 0, "a missing required parameter is not a query");
  assert.deepEqual(pane.docs.value, []);
  assert.equal(pane.searched.value, "", "searched must stay empty so the result area shows nothing, not a false 无关联");
  assert.match(pane.error.value, /先输入一个词/);
});

test("no selected library is reported instead of querying the default corpus", async t => {
  nsSel.value = "";
  const pane = fresh();
  const mock = t.mock.method(globalThis, "fetch", async () => { throw new Error("unexpected request"); });

  await pane.lookup("连接池");

  assert.equal(mock.mock.callCount(), 0);
  assert.match(pane.error.value, /选择一个知识库/);
});

test("a lookup encodes the term, scopes to the library, and reports what it searched", async t => {
  nsSel.value = "alpha";
  const pane = fresh();
  let url = "";
  t.mock.method(globalThis, "fetch", async (u) => {
    url = u;
    return Response.json({ namespace: "alpha", token: "连接 池", docs: [{ source_id: "src:a#1", weight: 3 }] });
  });

  await pane.lookup("连接 池");

  assert.match(url, /^\/v1\/affinity\?token=/);
  assert.ok(url.includes("token=" + encodeURIComponent("连接 池")), "term must be URL-encoded: " + url);
  assert.ok(url.includes("ns=alpha"), "must be scoped to the selected library: " + url);
  assert.equal(pane.docs.value.length, 1);
  assert.equal(pane.docs.value[0].weight, 3);
  assert.equal(pane.searched.value, "连接 池", "the backend's own token echoes back");
  assert.equal(pane.error.value, "");
  assert.equal(pane.busy.value, false);
});

test("a backend refusal stays an error and does not become an empty result", async t => {
  nsSel.value = "alpha";
  const pane = fresh();
  t.mock.method(globalThis, "fetch", async () => Response.json({ error: "token required", hint: "pass one" }, { status: 400 }));

  await pane.lookup("x");

  assert.deepEqual(pane.docs.value, []);
  assert.equal(pane.searched.value, "");
  assert.match(pane.error.value, /token required/);
  assert.equal(pane.busy.value, false);
});

test("a slow earlier lookup cannot overwrite the newer one", async t => {
  nsSel.value = "alpha";
  const pane = fresh();
  const pending = new Map();
  t.mock.method(globalThis, "fetch", (u) => new Promise(resolve => pending.set(u, resolve)));

  const first = pane.lookup("旧词");
  const second = pane.lookup("新词");
  const newer = [...pending.keys()].find(k => k.includes(encodeURIComponent("新词")));
  const older = [...pending.keys()].find(k => k.includes(encodeURIComponent("旧词")));
  pending.get(newer)(Response.json({ token: "新词", docs: [{ source_id: "src:new#1", weight: 1 }] }));
  await second;
  pending.get(older)(Response.json({ token: "旧词", docs: [{ source_id: "src:old#1", weight: 9 }] }));
  await first;

  assert.equal(pane.docs.value.length, 1);
  assert.equal(pane.docs.value[0].source_id, "src:new#1", "the stale response must be dropped");
  assert.equal(pane.searched.value, "新词");
  assert.equal(pane.busy.value, false);
});

test("an empty result is distinguishable from never having searched", async t => {
  nsSel.value = "alpha";
  const pane = fresh();
  t.mock.method(globalThis, "fetch", async () => Response.json({ token: "冷词", docs: [] }));

  await pane.lookup("冷词");

  assert.equal(pane.error.value, "", "zero rows is a real answer, not a failure");
  assert.equal(pane.searched.value, "冷词", "and the view keys its 无关联 empty state off this");
  assert.deepEqual(pane.docs.value, []);
});

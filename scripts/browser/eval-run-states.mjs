// Evaluation run-lifecycle UI contract (fast gate, no Go build, no store).
//
// Serves the built bundle from a throwaway static server and mocks /v1/**, so the
// states that are hard to produce on demand (cancelling, interrupted, a rejected
// retry, the live-cost consent gate) can be asserted deterministically. The real
// end-to-end path is covered by scripts/browser/eval-workbench.mjs; this file
// covers the button/alert contract those states drive:
//
//   * navigation          → the five v4 panes all mount
//   * completed/failed=0  → neither 重试失败题目 nor the cold-start block
//   * interrupted         → reason text + 重试失败题目 + 新建冷启动评测, no 取消运行
//   * live start          → 运行 opens a cost dialog and submits NOTHING until
//                           确认用真实模型跑; 回到离线 dismisses without submitting
//   * running             → 取消运行
//   * cancelling          → 正在取消, disabled
//   * cancelled           → 取消运行 gone; 重试失败题目 appears but stays disabled
//                           until the live-retry consent is ticked
//
// Rewritten against the UI as it stands after UI v4 (docs/ui-v4-design.md). The
// previous version asserted a generation that stopped existing on 2026-09-29
// (7181b66, the 4-navigation IA): the three-step wizard (下一步：配置 / 下一步：确认 /
// 保存不可变版本并配置) is gone, its heading 评测工作台 is now 评测, and the paid-start
// consent moved from "tick the box to enable the button" to a dialog.
//
// Runs used to be opened by a per-row button; they are now a table row click. That
// move is the reason this file is worth keeping: @row-click was written as
// `({ row }) => selectRun(row.id)` while eb-table emits `emit("row-click", row,
// index, e)` — positionally. The destructure produced undefined, selectRun(undefined)
// returned early, and the run detail became unreachable from the list without raising
// an error anywhere. The 116 composable tests pass and the page still looks fine;
// only a gate that drives rendered states sees this shape.
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { loadPlaywright, PLAYWRIGHT_HINT } from './playwright.mjs';

const playwright = loadPlaywright();
if (playwright.missing) {
  console.log('SKIP run-state contract: @playwright/test not found (' + playwright.missing.join(', ') + ')');
  console.log('     ' + PLAYWRIGHT_HINT);
  process.exit(2);
}
const { chromium, expect } = playwright;

const dist = path.resolve(import.meta.dirname, '../../cmd/cumulus-cluster/web/dist');
assert.ok(fs.existsSync(path.join(dist, 'index.html')), 'built bundle missing: run `make web` first (or use make browser-check)');
const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png' };
const serving = http.createServer((request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  let rel = url.pathname.startsWith('/ui/') ? url.pathname.slice('/ui'.length) : url.pathname;
  if (rel === '' || rel === '/') rel = '/index.html';
  const full = path.join(dist, rel);
  if (!full.startsWith(dist) || !fs.existsSync(full) || fs.statSync(full).isDirectory()) {
    response.writeHead(404); return response.end('not found');
  }
  response.writeHead(200, { 'Content-Type': TYPES[path.extname(full)] || 'application/octet-stream' });
  fs.createReadStream(full).pipe(response);
});
await new Promise((resolve) => serving.listen(0, '127.0.0.1', resolve));
const base = 'http://127.0.0.1:' + serving.address().port;

// ---- mock backend -----------------------------------------------------------
const LIVE_RUN = '模拟真实模型评测';
const item = { id: 'q1', query: '连接池多少？', answer: '128', gold_sources: ['manual'] };
const dataset = { id: 'd1', name: '模拟题集', count: 1, items: [item], sha: 'sha' };
const runs = new Map([
  ['r-done', { id: 'r-done', name: '已完成无失败', state: 'completed', config: { mode: 'offline' }, total: 1, done: 1, failed: 0, dataset_name: '模拟题集', summary: { n: 1, rule_match: 1 } }],
  ['r-int', { id: 'r-int', name: '被中断', state: 'interrupted', error: 'server restarted; isolated warm state was lost; start a new run', config: { mode: 'offline' }, total: 3, done: 2, failed: 1, dataset_name: '模拟题集', summary: { n: 2 } }],
]);
let live = null; // the run created by this check
let cancelPolls = 0; // the POST response shows 'cancelling'; a later poll settles it
const counters = { starts: 0, cancels: 0, retries: 0 };
const failures = [];
const errors = [];

const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1360, height: 1000 }, reducedMotion: 'reduce' });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
  await page.route('**/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    let data = {};
    if (url.pathname === '/v1/buckets') data = { buckets: [{ name: 'mock', label: '模拟评测（无模型调用）' }], default_ns: 'mock' };
    // 引擎面板也在挂载检查里。/v1/monitor/overview 的顶层 retrieval/system/llm 在
    // Go 侧是非指针、无 omitempty（internal/monitor/monitor.go:112-117），真实面永远会带；
    // 返回 {} 是这里编不出的一种形状，会让面板在 mon.retrieval.reuse_rate 上抛 TypeError。
    // knowledge 反过来确实可以缺席（omitempty），所以故意不给——把可缺席那条路径也走一遍。
    if (url.pathname === '/v1/monitor/overview') data = { queries: 0, uptime_sec: 0, retrieval: { reuse_rate: 0, reuse_hits: 0, queries: 0 }, system: { store_dir: '/tmp/mock' }, llm: {}, namespaces: [], recent: [] };
    if (url.pathname === '/v1/eval/capabilities') data = { protocol: 'eval-v2', live_available: true, model: 'mock-not-a-real-model', max_items: 500, max_bytes: 2097152, l1_available: true, concurrency: 1, queue_limit: 8 };
    if (url.pathname === '/v1/eval/datasets') data = { datasets: [dataset] };
    if (url.pathname === '/v1/eval/datasets/d1') data = dataset;
    if (url.pathname === '/v1/eval/runs') {
      if (request.method() === 'POST') {
        counters.starts++;
        const body = request.postDataJSON();
        live = { id: 'r1', name: LIVE_RUN, state: 'running', config: body.config, total: 1, done: 0, failed: 0, dataset_name: '模拟题集', summary: { n: 0 } };
        runs.set(live.id, live);
        data = live;
      } else {
        data = { runs: [...runs.values()] };
      }
    }
    const runRoute = url.pathname.match(/^\/v1\/eval\/runs\/([^/]+)(?:\/(items|cancel|retry))?$/);
    if (runRoute) {
      const id = runRoute[1];
      const action = runRoute[2];
      if (action === 'items') {
        data = { items: [] };
      } else if (action === 'cancel') {
        counters.cancels++;
        live = { ...live, state: 'cancelling' };
        runs.set(id, live);
        data = live;
      } else if (action === 'retry') {
        counters.retries++;
        live = { ...live, state: 'running', failed: 0 };
        runs.set(id, live);
        data = live;
      } else {
        // The first GET after the action still reports 'cancelling' (as the real
        // face does until the worker notices); a later poll settles it.
        if (id === 'r1' && live && live.state === 'cancelling') {
          cancelPolls++;
          if (cancelPolls >= 2) { live = { ...live, state: 'cancelled', failed: 1 }; runs.set(id, live); }
        }
        data = id === 'r1' ? (live || {}) : runs.get(id) || {};
      }
    }
    if (url.pathname.endsWith('/export')) {
      await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
      return;
    }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  });
  page.on('response', (response) => { if (!response.ok() && response.url().includes('/v1/')) failures.push(response.status() + ' ' + response.url()); });

  const detailHeading = (name) => page.getByRole('heading', { name: '运行详情 · ' + name, exact: true });
  // The detail card carries its name in an <h3>, not in an aria-label, so scope
  // through the heading instead of asserting a region role the component does not set.
  const detail = (name) => page.locator('section').filter({ has: detailHeading(name) }).last();
  async function openRun(name) {
    await page.locator('tr', { hasText: name }).first().click();
    await expect(detailHeading(name)).toBeVisible();
  }
  async function absent(scope, name, why) {
    assert.equal(await scope.getByRole('button', { name, exact: true }).count(), 0, why);
  }

  await page.goto(base + '/ui/#/chat');
  await page.reload();

  // 五个面板都得挂得上。重排动的正是这一层，而挂不上一般不报错——只是一片空白。
  for (const [hash, title] of [['#/chat', '检索问答'], ['#/corpus', '语料'], ['#/knowledge', '知识'], ['#/evals', '评测'], ['#/engine', '引擎']]) {
    await page.evaluate((h) => { location.hash = h; }, hash);
    await expect(page.getByRole('heading', { name: title, exact: true, level: 1 })).toBeVisible();
    assert.ok((await page.locator('main').innerText()).trim().length > 0, hash + ' mounted with an empty <main>');
  }

  await page.evaluate(() => { location.hash = '#/evals'; });
  await expect(page.getByRole('heading', { name: '评测', exact: true, level: 2 })).toBeVisible();
  // 表格本身带 aria-label，但它的 role 是 table 不是 region；断言行可见才是这里要的
  // 契约（列表渲染出来了，等下才能靠点行进详情）。
  await expect(page.getByRole('row', { name: /已完成无失败/ })).toBeVisible();
  await expect(page.getByRole('row', { name: /被中断/ })).toBeVisible();

  // 已完成且零失败：既不可重试，也不该出现冷启动提示
  await openRun('已完成无失败');
  const done = detail('已完成无失败');
  await expect(done.getByText('completed', { exact: true }).first()).toBeVisible();
  await absent(done, '重试失败题目', 'a clean completed run must not offer retry');
  await absent(done, '新建冷启动评测', 'a clean completed run must not offer cold start');

  // 被中断：原因、重试、冷启动三件套；终态不得给出取消入口
  await openRun('被中断');
  const interrupted = detail('被中断');
  await expect(interrupted.getByText('interrupted', { exact: true }).first()).toBeVisible();
  await expect(interrupted.getByText(/isolated warm state was lost/)).toBeVisible();
  await expect(interrupted.getByRole('button', { name: '重试失败题目', exact: true })).toBeEnabled();
  await expect(interrupted.getByRole('button', { name: '新建冷启动评测', exact: true })).toBeVisible();
  await absent(interrupted, '取消运行', 'a terminal run must not offer cancel');

  // 选了题集「运行」才可用：没有向导，这一行就是全部输入。
  // 命名只能靠 class 限定：aria-label="题集" 写在 eb-select 上，属性透传落在没有 role 的
  // 根 div 上，内部 role="combobox" 的元素的可及名其实来自内容（占位符「选择题集」），
  // 所以 getByRole('combobox', {name:'题集'}) 恒为 0。页面上另一个 combobox 是顶部库选择器。
  await expect(page.getByRole('button', { name: '运行', exact: true })).toBeDisabled();
  await page.locator('.dataset-pick').getByRole('combobox').click();
  // 下拉收起时选项 display:none，不进可及性树，getByRole('option') 此时为 0——必须先点开
  await expect(page.getByRole('option', { name: /模拟题集 · 1 题/ })).toBeVisible();
  await page.getByRole('option', { name: /模拟题集 · 1 题/ }).click();
  await expect(page.getByRole('button', { name: '运行', exact: true })).toBeEnabled();

  // 真实模式起跑：计费是事实不是吓唬，确认之前一次 POST 都不能发生
  await page.getByRole('tab', { name: '真实模型', exact: true }).click();
  await page.getByRole('button', { name: '运行', exact: true }).click();
  const consent = page.getByRole('dialog').filter({ hasText: '使用真实模型运行' });
  await expect(consent.getByText(/检索、判官、闭卷基线都按供应商价格计费/)).toBeVisible();
  assert.equal(counters.starts, 0, 'a paid run was submitted before the cost was acknowledged');
  await consent.getByRole('button', { name: '回到离线', exact: true }).click();
  await expect(page.getByRole('dialog').filter({ hasText: '使用真实模型运行' })).toBeHidden();
  assert.equal(counters.starts, 0, 'dismissing the cost dialog must not submit the run');

  await page.getByRole('button', { name: '运行', exact: true }).click();
  await consent.getByRole('button', { name: '确认用真实模型跑', exact: true }).click();
  assert.equal(counters.starts, 1, 'confirming the cost must submit exactly once');

  // 运行中 → 正在取消 → cancelled 后的重试契约
  const liveRegion = detail(LIVE_RUN);
  await expect(liveRegion.getByRole('button', { name: '取消运行', exact: true })).toBeVisible();
  await liveRegion.getByRole('button', { name: '取消运行', exact: true }).click();
  await expect(liveRegion.getByRole('button', { name: '正在取消', exact: true })).toBeDisabled();
  assert.equal(counters.cancels, 1);
  await expect(liveRegion.getByText('cancelled', { exact: true }).first()).toBeVisible({ timeout: 10000 });
  await absent(liveRegion, '取消运行', 'cancel must disappear once the run is terminal');
  const retryButton = liveRegion.getByRole('button', { name: '重试失败题目', exact: true });
  await expect(retryButton).toBeVisible();
  // live 运行的重试另需一次费用确认（这条仍是勾选框，与起跑的对话框不同）
  await expect(retryButton).toBeDisabled();
  assert.equal(counters.retries, 0, 'a paid retry was submitted without the extra-cost acknowledgement');
  await liveRegion.getByText('我确认重试真实模型调用会产生额外费用（检索、判官及闭卷基线）', { exact: true }).click();
  await expect(retryButton).toBeEnabled();
  await retryButton.click();
  await expect(liveRegion.getByRole('button', { name: '取消运行', exact: true })).toBeVisible();
  assert.equal(counters.retries, 1);

  assert.deepEqual(failures, [], 'the GUI saw failing /v1 responses: ' + failures.join('; '));
  assert.deepEqual(errors, [], 'the GUI raised page/console errors');
  console.log('PASS run-state contract: five panes mount; the cost dialog gates the paid start (dismissing submits nothing); cancel → cancelling → cancelled; a live retry needs its own acknowledgement; interrupted shows reason + cold start; a clean run offers neither; no real requests');
} finally {
  await browser.close();
  serving.close();
}

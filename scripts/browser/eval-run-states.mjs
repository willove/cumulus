// Evaluation run-lifecycle UI contract (fast gate, no Go build, no store).
//
// Serves the built bundle from a throwaway static server and mocks /v1/**, so the
// states that are hard to produce on demand (cancelling, interrupted, a rejected
// retry, the live-cost consent gate) can be asserted deterministically. The real
// end-to-end path is covered by scripts/browser/eval-workbench.mjs; this file
// covers the button/alert contract those states drive:
//
//   * live start        → 确认并开始评测 disabled until the cost consent is ticked
//   * running           → 取消运行
//   * cancelling        → 正在取消, disabled
//   * cancelled         → no 取消运行; 重试失败题目 appears, disabled while the
//                         live retry consent is unticked, enabled after ticking
//   * completed/failed=0→ neither 重试失败题目 nor the cold-start block
//   * interrupted       → reason alert + 重试失败题目 + 新建冷启动评测
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
assert.ok(fs.existsSync(path.join(dist, 'index.html')), 'built bundle missing: run `npm run build` in web/ first (or use make browser-check)');
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
const item = { id: 'q1', query: '连接池多少？', answer: '128', gold_sources: ['manual'] };
const dataset = { id: 'd1', name: '模拟题集', count: 1, items: [item], sha: 'sha' };
const runs = new Map([
  ['r-done', { id: 'r-done', name: '已完成无失败', state: 'completed', config: { mode: 'offline' }, total: 1, done: 1, failed: 0, dataset_name: '模拟题集', summary: { n: 1, rule_match: 1 } }],
  ['r-int', { id: 'r-int', name: '被中断', state: 'interrupted', error: 'server restarted; isolated warm state was lost; start a new run', config: { mode: 'offline' }, total: 3, done: 2, failed: 1, dataset_name: '模拟题集', summary: { n: 2 } }],
]);
let live = null; // the run created by this check
let cancelling = false;
let cancelPolls = 0; // the POST response shows 'cancelling'; the next poll settles it
const counters = { starts: 0, cancels: 0, retries: 0, exports: 0 };
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
    if (url.pathname === '/v1/buckets') data = { buckets: [{ name: 'mock', label: '模拟评测（无模型调用）' }] };
    if (url.pathname === '/v1/eval/capabilities') data = { protocol: 'eval-v2', live_available: true, model: 'mock-not-a-real-model', max_items: 500, max_bytes: 2097152, l1_available: true, concurrency: 1, queue_limit: 8 };
    if (url.pathname === '/v1/eval/datasets') data = { datasets: [dataset] };
    if (url.pathname === '/v1/eval/datasets/d1') data = dataset;
    if (url.pathname === '/v1/eval/runs') {
      if (request.method() === 'POST') {
        counters.starts++;
        const body = request.postDataJSON();
        live = { id: 'r1', name: '模拟真实模型评测', state: 'running', config: body.config, total: 1, done: 0, failed: 0, dataset_name: '模拟题集', summary: { n: 0 } };
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
      const current = id === 'r1' ? live : runs.get(id);
      if (action === 'items') {
        data = { items: [] };
      } else if (action === 'cancel') {
        counters.cancels++; cancelling = true;
        live = { ...live, state: 'cancelling' };
        data = live;
      } else if (action === 'retry') {
        counters.retries++;
        live = { ...live, state: 'running', failed: 0 };
        data = live;
      } else {
        // The first GET right after the action still reports 'cancelling' (as the
        // real face does until the worker notices); the scheduled poll settles it.
        if (id === 'r1' && cancelling && live.state === 'cancelling') {
          cancelPolls++;
          if (cancelPolls >= 2) { live = { ...live, state: 'cancelled', failed: 1 }; cancelling = false; }
        }
        data = id === 'r1' ? live : current || {};
      }
    }
    if (url.pathname.endsWith('/export')) {
      counters.exports++;
      await route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
      return;
    }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  });
  page.on('response', (response) => { if (!response.ok() && response.url().includes('/v1/')) failures.push(response.status() + ' ' + response.url()); });

  await page.goto(base + '/ui/#/evals');
  await page.locator('.library-selector').getByRole('combobox').click();
  await page.getByRole('option', { name: '模拟评测（无模型调用）', exact: true }).click();
  await expect(page.getByRole('heading', { name: '评测工作台', exact: true, level: 2 })).toBeVisible();

  // 已完成且零失败：既不可重试，也不该出现冷启动提示
  await page.getByRole('button', { name: '查看评测 已完成无失败', exact: true }).click();
  const done = page.getByRole('region', { name: '运行详情 · 已完成无失败', exact: true });
  await expect(done.getByText('completed', { exact: true }).first()).toBeVisible();
  assert.equal(await done.getByRole('button', { name: '重试失败题目', exact: true }).count(), 0, 'a clean completed run must not offer retry');
  assert.equal(await done.getByRole('button', { name: '新建冷启动评测', exact: true }).count(), 0, 'a clean completed run must not offer cold start');

  // 被中断：原因、重试、冷启动三件套
  await page.getByRole('button', { name: '查看评测 被中断', exact: true }).click();
  const interrupted = page.getByRole('region', { name: '运行详情 · 被中断', exact: true });
  await expect(interrupted.getByText('interrupted', { exact: true }).first()).toBeVisible();
  await expect(interrupted.getByText(/isolated warm state was lost/)).toBeVisible();
  await expect(interrupted.getByRole('button', { name: '重试失败题目', exact: true })).toBeEnabled();
  await expect(interrupted.getByRole('button', { name: '新建冷启动评测', exact: true })).toBeVisible();
  assert.equal(await interrupted.getByRole('button', { name: '取消运行', exact: true }).count(), 0, 'a terminal run must not offer cancel');

  // 真实模型起跑：费用确认前不得提交
  await page.getByRole('button', { name: '新建评测', exact: true }).click();
  const create = page.getByRole('region', { name: new RegExp('新建评测') });
  await create.getByRole('combobox').first().click();
  await page.getByRole('option', { name: /模拟题集/ }).click();
  await page.getByRole('button', { name: '下一步：配置', exact: true }).click();
  await create.getByRole('combobox').click();
  await page.getByRole('option', { name: '真实模型 · 产生调用费用', exact: true }).click();
  await page.getByRole('button', { name: '下一步：确认', exact: true }).click();
  await expect(page.getByRole('button', { name: '确认并开始评测', exact: true })).toBeDisabled();
  assert.equal(counters.starts, 0, 'the wizard submitted a paid run without consent');
  await page.getByText(/我确认对.*调用真实模型/).click();
  await page.getByRole('button', { name: '确认并开始评测', exact: true }).click();
  assert.equal(counters.starts, 1);

  // 运行中 → 正在取消 → cancelled 后的重试契约
  const liveRegion = page.getByRole('region', { name: '运行详情 · 模拟真实模型评测', exact: true });
  await expect(liveRegion.getByRole('button', { name: '取消运行', exact: true })).toBeVisible();
  await liveRegion.getByRole('button', { name: '取消运行', exact: true }).click();
  await expect(liveRegion.getByRole('button', { name: '正在取消', exact: true })).toBeDisabled();
  assert.equal(counters.cancels, 1);
  await expect(liveRegion.getByText('cancelled', { exact: true }).first()).toBeVisible({ timeout: 10000 });
  assert.equal(await liveRegion.getByRole('button', { name: '取消运行', exact: true }).count(), 0, 'cancel must disappear once terminal');
  const retryButton = liveRegion.getByRole('button', { name: '重试失败题目', exact: true });
  await expect(retryButton).toBeVisible();
  // live 运行的重试另需一次费用确认：未勾选不得点击
  await expect(retryButton).toBeDisabled();
  assert.equal(counters.retries, 0);
  await liveRegion.getByText('我确认重试真实模型调用会产生额外费用（检索、判官及闭卷基线）', { exact: true }).click();
  await expect(retryButton).toBeEnabled();
  await retryButton.click();
  await expect(liveRegion.getByRole('button', { name: '取消运行', exact: true })).toBeVisible();
  assert.equal(counters.retries, 1);

  assert.deepEqual(failures, [], 'the GUI saw failing /v1 responses: ' + failures.join('; '));
  assert.deepEqual(errors, []);
  console.log('PASS run-state contract: consent gates both paid start and paid retry; cancel → cancelling → cancelled; retry offered only when retryable; interrupted shows reason + cold start; no real requests');
} finally {
  await browser.close();
  serving.close();
}
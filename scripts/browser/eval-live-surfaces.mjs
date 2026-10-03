// Live-model workbench surfaces (optional gate, spends NO tokens).
//
// A completed live run is the only thing that can prove the paid surfaces reach
// the screen: judge verdict + reason, the closed-book baseline, real token
// counters. This check only READS such a run, then walks the linear flow far
// enough to see the cost-consent dialog — and dismisses it. It never submits, so
// re-running it is free.
//
// Rewritten for the linear flow on 2026-10-03 (7181b66 retired the wizard this
// file was written against). Two things changed beyond the labels:
//   * the paid-start consent is now a dialog, not a checkbox gating a button;
//   * the old N/A regexes were vacuous — the run-level label is 闭卷规则匹配**率**,
//     so /闭卷规则匹配\s*N\/A/ could never match what it claimed to guard.
//
//   EVAL_BASE      base URL of a `serve` with LLM_BASE_URL configured (required).
//                  live_available is pure config presence (evalexecutor.go:28-31),
//                  so a dummy http(s) base URL satisfies it — nothing is ever sent,
//                  which is what makes this gate free.
//   EVAL_NS        library (bucket) holding the live run (required)
//   EVAL_LIVE_RUN  run id; when unset, the newest completed live run is used
//   EVAL_SHOTS     screenshot directory (optional)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { loadPlaywright, PLAYWRIGHT_HINT } from './playwright.mjs';

const base = process.env.EVAL_BASE;
const ns = process.env.EVAL_NS;
if (!base || !ns) throw new Error('EVAL_BASE and EVAL_NS are required');

const playwright = loadPlaywright();
if (playwright.missing) {
  console.log('SKIP live surfaces: @playwright/test not found (' + playwright.missing.join(', ') + ')');
  console.log('     ' + PLAYWRIGHT_HINT);
  process.exit(2);
}
const { chromium, expect } = playwright;

const api = async (route) => {
  const response = await fetch(base + route);
  const text = await response.text();
  if (!response.ok) throw new Error(route + ' → ' + response.status + ' ' + text.slice(0, 200));
  return text ? JSON.parse(text) : null;
};

// serve 把包**编译期**嵌进二进制（go:embed all:web/dist），所以一个还在跑的 serve 完全可以
// 比 web/src 旧——届时下面每一条断言说的都是一个已经不存在的 UI，而且**会绿**（本轮实测踩过：
// 删掉一个付费指标行、重建了 dist 却没重建二进制，门照样 PASS）。
// 这道门自己不带 build 步骤，所以在这里把「服务中的包 == 磁盘上的构建产物」钉住。
const servedShell = await (await fetch(base + '/ui/')).text();
const diskShell = fs.readFileSync(new URL('../../cmd/cumulus-cluster/web/dist/index.html', import.meta.url), 'utf8');
const assetOf = (html) => (/assets\/index-[\w-]+\.js/.exec(html) || [])[0];
const [servedAsset, diskAsset] = [assetOf(servedShell), assetOf(diskShell)];
assert.ok(diskAsset, 'no built bundle on disk: run `make web` first');
assert.equal(servedAsset, diskAsset, 'serve is running ' + (servedAsset || 'a shell with no built bundle')
  + ' but cmd/cumulus-cluster/web/dist references ' + diskAsset
  + ' — rebuild the binary (`make build`) before trusting this gate');

const caps = await api('/v1/eval/capabilities?ns=' + encodeURIComponent(ns));if (!caps.live_available) {
  console.log('SKIP live surfaces: this serve reports live_available=false');
  console.log('     the consent half would then pass for the wrong reason (the face');
  console.log('     refuses live runs regardless of the dialog). Start serve with');
  console.log('     LLM_BASE_URL set — nothing is submitted, so it stays free.');
  process.exit(2);
}

let runId = process.env.EVAL_LIVE_RUN;
if (!runId) {
  const listed = await api('/v1/eval/runs?ns=' + encodeURIComponent(ns));
  const live = listed.runs.filter((run) => run.config && run.config.mode === 'live' && run.state === 'completed');
  if (!live.length) {
    console.log('SKIP live surfaces: no completed live run in library ' + ns);
    console.log('     run one first (评测 → 选题集 → 模式「真实模型」→ 运行 → 确认计费),');
    console.log('     then re-run this check. It costs real tokens; this gate does not.');
    process.exit(2);
  }
  runId = live[0].id;
}
const buckets = await api('/v1/buckets');
const bucket = (buckets.buckets || []).find((candidate) => candidate.name === ns);
assert.ok(bucket, 'library ' + ns + ' is not registered');
const run = await api('/v1/eval/runs/' + runId + '?ns=' + encodeURIComponent(ns));
assert.equal(run.config.mode, 'live', 'run ' + runId + ' is not a live run');
assert.equal(run.state, 'completed', 'run ' + runId + ' is ' + run.state);
const items = (await api('/v1/eval/runs/' + runId + '/items?ns=' + encodeURIComponent(ns))).items;
assert.ok(items.length > 0, 'live run has no items');
const paid = items.find((item) => item.judge_tokens > 0 || item.closed_book_tokens > 0) || items[0];
assert.ok(paid.search_tokens > 0, 'live run reported no retrieval tokens; nothing to render');
console.log('  .. verifying live run ' + runId + ' (' + items.length + ' item(s), model in config_text: ' + /model=([^;]+)/.exec(run.config_text)?.[1] + ')');

const errors = [];
const writes = [];
const shots = process.env.EVAL_SHOTS;
const shot = async (page, name) => {
  if (!shots) return;
  await page.screenshot({ path: path.join(shots, name), fullPage: true });
};

// The metric rows render as `label` then value, so the guard has to look at the
// value that FOLLOWS the named label — a bare /label\s*N\/A/ can never match when
// the label itself is a longer word (that is how the previous version went vacuous).
const metricNotNA = (text, label) => {
  const at = text.indexOf(label);
  assert.ok(at >= 0, label + ' is not on the screen at all');
  const window = text.slice(at + label.length, at + label.length + 24);
  assert.ok(!window.includes('N/A'), label + ' rendered as N/A on a live run that has the data');
};

const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, reducedMotion: 'reduce' });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
  page.on('request', (request) => { if (request.method() !== 'GET' && request.url().includes('/v1/eval/')) writes.push(request.method() + ' ' + new URL(request.url()).pathname); });

  await page.goto(base + '/ui/#/evals');
  await page.locator('.library-selector').getByRole('combobox').click();
  await page.getByRole('option', { name: bucket.label, exact: true }).click();
  await expect(page.getByRole('heading', { name: '评测', exact: true, level: 2 })).toBeVisible();
  // 线性流没有能力卡，「真实模型」这一档本身就是 live 可用的那面证据
  await expect(page.getByRole('tab', { name: '真实模型', exact: true })).toBeVisible();

  // 运行列表 → 打开那次真实运行：付费面必须上屏，且不能显示成 N/A
  await page.locator('tr', { hasText: run.name }).first().click();
  const detail = page.getByRole('region', { name: '运行详情 · ' + run.name, exact: true });
  await expect(detail).toBeVisible({ timeout: 20000 });
  const detailText = await detail.innerText();
  if (run.summary.judge_n > 0) metricNotNA(detailText, '判官正确率');
  if (run.summary.closed_book_match !== null && run.summary.closed_book_match !== undefined) {
    metricNotNA(detailText, '闭卷规则匹配率');
  }
  // 屏上数字带千分位（2,244），所以两种写法都算命中
  const renders = (text, value) => text.includes(String(value)) || text.includes(value.toLocaleString('en-US'));
  assert.ok(renders(detailText, paid.search_tokens), 'retrieval token counter (' + paid.search_tokens + ') did not render');
  if (paid.judge_tokens > 0) assert.ok(renders(detailText, paid.judge_tokens), 'judge token counter (' + paid.judge_tokens + ') did not render');
  if (paid.closed_book_tokens > 0) assert.ok(renders(detailText, paid.closed_book_tokens), 'closed-book token counter (' + paid.closed_book_tokens + ') did not render');

  await detail.getByRole('button', { name: '查看题目 ' + paid.id, exact: true }).click();
  const evidence = page.getByRole('article', { name: '冻结逐题结果' });
  const evidenceText = await evidence.innerText();
  assert.ok(evidenceText.includes('判官正确'), 'judge verdict block missing from the drawer');
  if (paid.judge_correct !== null) {
    assert.ok(paid.judge_reason && evidenceText.includes(paid.judge_reason), 'judge reason did not render');
  }
  if (paid.closed_book_answer) assert.ok(evidenceText.includes(paid.closed_book_answer.trim().slice(0, 8)), 'closed-book baseline answer did not render');
  assert.ok(evidenceText.includes(paid.answer.slice(0, 10)), 'frozen system answer did not render');
  await shot(page, 'live-evidence.png');
  await page.getByRole('button', { name: '关闭题目详情', exact: true }).click();

  // 计费确认闸门：走到对话框为止，点「回到离线」，全程不得有任何写请求
  await page.locator('.dataset-pick').getByRole('combobox').click();
  await page.getByRole('option', { name: new RegExp(run.dataset_name) }).click();
  await page.getByRole('tab', { name: '真实模型', exact: true }).click();
  await page.getByRole('button', { name: '运行', exact: true }).click();
  const consent = page.getByRole('dialog').filter({ hasText: '使用真实模型运行' });
  await expect(consent.getByText(/按供应商价格计费/)).toBeVisible();
  await shot(page, 'live-consent.png');
  assert.deepEqual(writes, [], 'the check submitted an evaluation before dismissing: ' + writes.join(', '));
  await consent.getByRole('button', { name: '回到离线', exact: true }).click();
  await expect(consent).toBeHidden();

  assert.deepEqual(writes, [], 'the check submitted an evaluation: ' + writes.join(', '));
  assert.deepEqual(errors, []);
  console.log('PASS live surfaces: judge/closed-book/token counters render from the frozen record; the cost dialog gates a paid start and dismissing it writes nothing; nothing submitted');
} finally {
  await browser.close();
}

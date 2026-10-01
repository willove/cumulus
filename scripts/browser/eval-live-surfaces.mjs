// Live-model workbench surfaces (optional gate, spends NO tokens).
//
// A completed live run is the only thing that can prove the paid surfaces reach
// the screen: judge verdict + reason, closed-book baseline, real token counters.
// This check only READS such a run and walks the wizard far enough to see the
// cost-consent gate (the submit button must stay disabled until it is ticked).
// It never submits, so re-running it is free.
//
//   EVAL_BASE      base URL of a `serve` with LLM_BASE_URL configured (required)
//   EVAL_NS        library (bucket) holding the live run (required)
//   EVAL_LIVE_RUN  run id; when unset, the newest completed live run is used
import assert from 'node:assert/strict';
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

let runId = process.env.EVAL_LIVE_RUN;
if (!runId) {
  const listed = await api('/v1/eval/runs?ns=' + encodeURIComponent(ns));
  const live = listed.runs.filter((run) => run.config && run.config.mode === 'live' && run.state === 'completed');
  if (!live.length) {
    console.log('SKIP live surfaces: no completed live run in library ' + ns);
    console.log('     run one first (工作台 → 新建评测 → 运行模式「真实模型」), then re-run this check');
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

const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, reducedMotion: 'reduce' });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
  page.on('request', (request) => { if (request.method() !== 'GET' && request.url().includes('/v1/eval/')) writes.push(request.method() + ' ' + new URL(request.url()).pathname); });

  await page.goto(base + '/ui/#/evals');
  await page.locator('.library-selector').getByRole('combobox').click();
  await page.getByRole('option', { name: bucket.label, exact: true }).click();
  await expect(page.getByText('真实模型：可用')).toBeVisible();

  // 运行列表 → 打开那次真实运行：付费面必须上屏，且不能显示成 N/A
  await page.getByRole('button', { name: '查看评测 ' + run.name, exact: true }).click();
  const detail = page.getByRole('region', { name: '运行详情 · ' + run.name, exact: true });
  await expect(detail).toBeVisible({ timeout: 20000 });
  const detailText = await detail.innerText();
  if (run.summary.judge_n > 0) {
    assert.ok(!/判官正确率\s*N\/A/.test(detailText), 'judge accuracy missing on a judged live run');
  }
  if (paid.closed_book_match !== null) {
    assert.ok(!/闭卷规则匹配\s*N\/A/.test(detailText), 'closed-book baseline rendered as N/A on a live run');
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

  // 费用确认闸门：未勾选时不得可提交，且本检查不许提交
  await page.getByRole('button', { name: '新建评测', exact: true }).click();
  const create = page.getByRole('region', { name: new RegExp('新建评测') });
  await create.getByRole('combobox').first().click();
  await page.getByRole('option', { name: run.dataset_name, exact: false }).first().click();
  await page.getByRole('button', { name: '下一步：配置', exact: true }).click();
  await create.getByRole('combobox').click();
  await page.getByRole('option', { name: '真实模型 · 产生调用费用', exact: true }).click();
  await page.getByRole('textbox', { name: '运行名称', exact: true }).fill('联调真实模型（未提交）');
  await page.getByRole('button', { name: '下一步：确认', exact: true }).click();
  const consent = page.getByText(/我确认对.*调用真实模型/);
  await expect(consent).toBeVisible();
  const confirm = page.getByRole('button', { name: '确认并开始评测', exact: true });
  await expect(confirm).toBeDisabled();
  await shot(page, 'live-consent.png');
  await consent.click();
  await expect(confirm).toBeEnabled();
  assert.deepEqual(writes, [], 'the check submitted an evaluation: ' + writes.join(', '));
  assert.deepEqual(errors, []);
  console.log('PASS live surfaces: judge/closed-book/token counters render from the frozen record; cost consent gates submission; nothing submitted');
} finally {
  await browser.close();
}
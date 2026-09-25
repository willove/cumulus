// Workbench browser integration check (optional gate).
//
// Runs against a PRODUCTION-served workbench: `serve` mounts the go:embed'd
// bundle at /ui/ and the evaluation face at /v1/eval/* on one origin — no vite
// dev server, no proxy. Drives the real wizard end to end and fails on any
// browser error, so the pure-JS composable tests in web/src are backed by one
// check that the rendered工作台 actually wires them up.
//
//   EVAL_BASE         base URL of a running `serve` (required)
//   EVAL_FIXTURE_DIR  directory holding invalid.jsonl + valid.jsonl (required)
//   EVAL_SHOTS        screenshot directory (optional; none written when unset)
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { loadPlaywright, PLAYWRIGHT_HINT } from './playwright.mjs';

const base = process.env.EVAL_BASE;
const fixtures = process.env.EVAL_FIXTURE_DIR;
const shots = process.env.EVAL_SHOTS;
if (!base) throw new Error('EVAL_BASE is required');
if (!fixtures) throw new Error('EVAL_FIXTURE_DIR is required');
const shot = async (page, name) => {
  if (!shots) return;
  await fs.mkdir(shots, { recursive: true });
  await page.screenshot({ path: path.join(shots, name), fullPage: true });
};

const playwright = loadPlaywright();
if (playwright.missing) {
  console.log('SKIP browser check: @playwright/test not found (' + playwright.missing.join(', ') + ')');
  console.log('     ' + PLAYWRIGHT_HINT);
  process.exit(2);
}
const { chromium, expect } = playwright;

const ns = 'eval-gui-' + Date.now();
const label = '联调库-' + Date.now();
const errors = [];
const evalCalls = [];
let browser;
try {
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
  page.on('request', (request) => { if (request.url().includes('/v1/eval/')) evalCalls.push(request.method() + ' ' + new URL(request.url()).pathname); });

  let response = await page.request.post(base + '/v1/buckets', { data: { name: ns, label } });
  assert.equal(response.status(), 201, await response.text());
  response = await page.request.post(base + '/v1/ingest/sources', { data: { ns, key: 'manual', type: 'txt', title: '连接池手册', body: '连接池最大连接数为 128。连接超时为 30 秒。' } });
  assert.equal(response.status(), 201, await response.text());

  // The served shell must be the built bundle, not a dev-server page: this check
  // exists precisely because the embedded dist once lagged web/src.
  const shell = await (await page.request.get(base + '/ui/')).text();
  assert.ok(shell.includes('/ui/assets/index-'), 'serve did not mount the built bundle');
  assert.ok(!shell.includes('/@vite/client'), 'served shell is a dev-server page');

  await page.goto(base + '/ui/#/evals');
  await page.locator('.library-selector').getByRole('combobox').click();
  await page.getByRole('option', { name: label, exact: true }).click();
  await expect(page.getByRole('heading', { name: '评测工作台', exact: true, level: 2 })).toBeVisible();
  await expect(page.getByText('真实模型：未配置 / 不可用')).toBeVisible();
  // 能力卡的排队上限来自 capabilities.queue_limit：契约字段必须在屏上可见。
  await expect(page.getByText(/排队上限：\s*8/)).toBeVisible();

  // 向导：坏题集必须挡住保存，好题集必须给出题数
  await page.getByRole('button', { name: '新建评测', exact: true }).click();
  await page.locator('input[type="file"]').setInputFiles(path.join(fixtures, 'invalid.jsonl'));
  await page.getByRole('button', { name: '校验题集', exact: true }).click();
  await expect(page.getByText('校验未通过，请修正以下问题')).toBeVisible();
  await expect(page.getByRole('button', { name: '保存不可变版本并配置' })).toBeDisabled();

  // 段落式参考答案：仍然可以保存，但向导必须把「规则臂会恒 0」这条警告显示出来
  await page.getByRole('button', { name: '移除文件', exact: true }).click();
  await page.locator('input[type="file"]').setInputFiles(path.join(fixtures, 'passage.jsonl'));
  await page.getByRole('button', { name: '校验题集', exact: true }).click();
  await expect(page.getByText('校验通过 · 1 题')).toBeVisible();
  await expect(page.getByText(/警告 · .*passage/)).toBeVisible();
  await shot(page, 'passage-warning.png');

  await page.getByRole('button', { name: '移除文件', exact: true }).click();
  await page.locator('input[type="file"]').setInputFiles(path.join(fixtures, 'valid.jsonl'));
  await page.getByRole('button', { name: '校验题集', exact: true }).click();
  await expect(page.getByText('校验通过 · 2 题')).toBeVisible();
  await expect(page.getByText(/警告 · /)).toHaveCount(0);
  await shot(page, 'dataset-validated.png');

  await page.getByRole('button', { name: '保存不可变版本并配置' }).click();
  await expect(page.getByRole('heading', { name: '配置运行', exact: true })).toBeVisible();
  await page.getByRole('textbox', { name: '运行名称', exact: true }).fill('联调离线评测 A');
  await page.getByRole('button', { name: '下一步：确认', exact: true }).click();
  await page.getByRole('button', { name: '确认并开始评测', exact: true }).click();

  // 运行：进度到达终态，且逐题结果可展开到冻结引用
  const detail = page.getByRole('region', { name: '运行详情 · 联调离线评测 A', exact: true });
  await expect(detail).toBeVisible({ timeout: 20000 });
  await expect(detail.getByRole('status')).toContainText('已处理 2', { timeout: 20000 });
  await expect(detail.getByText('completed', { exact: true }).first()).toBeVisible();
  await detail.getByRole('button', { name: '查看题目 q1', exact: true }).click();
  await expect(page.getByRole('heading', { name: '冻结引用与原文摘录' })).toBeVisible();
  const evidence = page.getByRole('article', { name: '冻结逐题结果' });
  const evidenceText = await evidence.innerText();
  assert.ok(evidenceText.includes('128'), 'frozen answer missing from the evidence drawer');
  assert.ok(evidenceText.includes('连接池最大连接数为 128'), 'frozen citation quote missing');
  await shot(page, 'evidence.png');
  await page.getByRole('button', { name: '关闭题目详情', exact: true }).click();

  // 导出：走真实下载事件，内容必须带逐题证据
  const pendingDownload = page.waitForEvent('download');
  await detail.getByRole('button', { name: '导出 JSON', exact: true }).click();
  const download = await pendingDownload;
  const exported = JSON.parse(await fs.readFile(await download.path(), 'utf8'));
  assert.ok(JSON.stringify(exported).includes('128'));
  assert.ok(JSON.stringify(exported).includes('citations'), 'export lost per-item evidence');

  // 刷新后仍在（持久化经 GUI 可见），再跑一次做对比
  await page.reload();
  await page.locator('.library-selector').getByRole('combobox').click();
  await page.getByRole('option', { name: label, exact: true }).click();
  await page.getByRole('button', { name: '查看评测 联调离线评测 A', exact: true }).click();
  await expect(page.getByRole('button', { name: '查看题目 q2', exact: true })).toBeVisible();

  await page.getByRole('button', { name: '新建评测', exact: true }).click();
  const create = page.getByRole('region', { name: new RegExp('新建评测') });
  await create.getByRole('combobox').first().click();
  await page.getByRole('option', { name: /valid\.jsonl/ }).click();
  await page.getByRole('button', { name: '下一步：配置', exact: true }).click();
  await page.getByRole('textbox', { name: '运行名称', exact: true }).fill('联调离线评测 B');
  await page.getByRole('button', { name: '下一步：确认', exact: true }).click();
  await page.getByRole('button', { name: '确认并开始评测', exact: true }).click();
  const second = page.getByRole('region', { name: '运行详情 · 联调离线评测 B', exact: true });
  await expect(second.getByRole('status')).toContainText('已处理 2', { timeout: 20000 });
  await second.locator('.compare-picker').getByRole('combobox').click();
  await page.getByRole('option', { name: /联调离线评测 A/ }).click();
  await second.getByRole('button', { name: '比较运行', exact: true }).click();
  await expect(page.getByText('符合当前协议的可比条件')).toBeVisible();
  await second.scrollIntoViewIfNeeded();
  await shot(page, 'comparison.png');

  // 深色 + 移动端：不得出现横向滚动
  await page.getByRole('button', { name: '切换为深色模式' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole('heading', { name: '评测工作台', exact: true, level: 2 }).scrollIntoViewIfNeeded();
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'mobile layout scrolls horizontally');
  await shot(page, 'mobile.png');

  // 走的是真接口而不是被前端短路的路由
  assert.ok(evalCalls.some((call) => call.startsWith('POST /v1/eval/datasets/validate')), 'GUI never validated a dataset');
  assert.ok(evalCalls.some((call) => call.startsWith('GET /v1/eval/runs')), 'GUI never polled run state');
  assert.ok(evalCalls.some((call) => call.endsWith('/compare')), 'GUI never compared two runs');
  assert.deepEqual(errors, []);
  console.log('PASS browser: wizard validation, immutable save, offline run, progress, frozen evidence, export, refresh persistence, comparison, dark mobile; no browser errors (' + evalCalls.length + ' eval requests)');
} finally {
  if (browser) await browser.close();
}
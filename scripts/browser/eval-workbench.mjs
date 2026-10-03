// Workbench browser integration check (optional gate).
//
// Runs against a PRODUCTION-served workbench: `serve` mounts the go:embed'd
// bundle at /ui/ and the evaluation face at /v1/eval/* on one origin — no vite
// dev server, no proxy. Drives the linear run flow end to end and fails on any
// browser error, so the pure-JS composable tests in web/src are backed by one
// check that the rendered 工作台 actually wires them up.
//
// The three-step wizard was retired on 2026-09-29 (7181b66): one line of input
// (题集 → 模式 → 运行) plus an upload fold. This file was rewritten for that shape
// on 2026-10-03 because it had been asserting the wizard's labels ever since and
// therefore could not run at all.
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
const runA = '联调离线评测 A';
const runB = '联调离线评测 B';
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

  async function pickLibrary() {
    await page.locator('.library-selector').getByRole('combobox').click();
    await page.getByRole('option', { name: label, exact: true }).click();
    await expect(page.getByRole('heading', { name: '评测', exact: true, level: 2 })).toBeVisible();
  }
  // 命名只能靠 class / placeholder：aria-label 写在 eb-select 与 eb-input 上，属性透传
  // 落在没有 role 的根节点，控件自己的可及名其实来自占位符内容。
  const datasetPick = page.locator('.dataset-pick').getByRole('combobox');
  const nameField = page.getByRole('textbox', { name: /回归题集/ });
  const contentField = page.locator('.upload-fold textarea');
  async function openFold() {
    if (await page.getByRole('button', { name: '上传题集', exact: true }).count()) {
      await page.getByRole('button', { name: '上传题集', exact: true }).click();
    }
  }
  async function pickFile(file) {
    // setInputFiles 之后 readDataset 还要 await raw.text()；面板刚由 v-if 挂上时，change
    // 事件有整个丢掉的一遭（实测第二次上传就复现），届时「校验」永远 disabled。
    // 所以把「校验 可点」当作文件已落地的信号，丢掉就重投一次，而不是设完就走。
    const input = page.locator('input[type="file"]');
    const validate = page.getByRole('button', { name: '校验', exact: true });
    await input.setInputFiles(path.join(fixtures, file));
    try {
      await expect(validate).toBeEnabled({ timeout: 5000 });
    } catch {
      await input.setInputFiles(path.join(fixtures, file));
      await expect(validate).toBeEnabled({ timeout: 5000 });
    }
  }
  async function saveDataset({ file, paste, name }) {
    if (file) await pickFile(file);
    if (paste) await contentField.fill(paste);
    await nameField.fill(name);
    await page.getByRole('button', { name: '校验', exact: true }).click();
    await page.getByRole('button', { name: '存为题集', exact: true }).click();
    await expect(datasetPick).toContainText(name);
    await expect(page.getByRole('button', { name: '运行', exact: true })).toBeEnabled();
  }

  await page.goto(base + '/ui/#/evals');
  await pickLibrary();
  await expect(page.getByText('用题集检验回答质量')).toBeVisible();

  // 上传折叠面板：坏题集挡住保存，段落式参考答案必须带出警告
  await openFold();
  await contentField.fill(await fs.readFile(path.join(fixtures, 'invalid.jsonl'), 'utf8'));
  await page.getByRole('button', { name: '校验', exact: true }).click();
  await expect(page.getByText('校验未通过，请修正以下问题')).toBeVisible();
  await expect(page.getByRole('button', { name: '存为题集', exact: true })).toBeDisabled();

  await contentField.fill(await fs.readFile(path.join(fixtures, 'passage.jsonl'), 'utf8'));
  await nameField.fill('段落参考答案题集');
  await page.getByRole('button', { name: '校验', exact: true }).click();
  await expect(page.getByText('校验通过 · 1 题')).toBeVisible();
  // 能存，但规则臂对整段参考答案恒 0——接口返回 warnings，脸上就得显示（eval/v2.go:141）
  await expect(page.getByText(/警告 · .*passage/)).toBeVisible();
  await shot(page, 'passage-warning.png');

  // 真实文件上传那一遍：读文件 → 校验 → 存为题集 → 运行一行了结
  await saveDataset({ file: 'valid.jsonl', name: runA });
  await expect(page.getByText(/警告 · /)).toHaveCount(0);
  // 题集已经在手上了，空态不能再同屏让你「上传第一个题集」
  assert.equal(await page.getByRole('button', { name: '上传第一个题集', exact: true }).count(), 0,
    'the empty guide kept offering a first dataset after one was already selected');
  await shot(page, 'dataset-validated.png');

  await page.getByRole('button', { name: '运行', exact: true }).click();
  const detail = page.getByRole('region', { name: '运行详情 · ' + runA, exact: true });
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

  // 刷新后仍在（持久化经 GUI 可见）。运行只能靠点表行打开——这正是 row-click
  // 与组件 emit 形状不一致时唯一会红的地方。
  await page.reload();
  await pickLibrary();
  await page.locator('tr', { hasText: runA }).first().click();
  await expect(page.getByRole('button', { name: '查看题目 q2', exact: true })).toBeVisible();

  // 第二次运行做对比：同一份 JSONL，改走「直接粘贴」那一栏，另存一个题集名
  await openFold();
  await saveDataset({ paste: await fs.readFile(path.join(fixtures, 'valid.jsonl'), 'utf8'), name: runB });
  await page.getByRole('button', { name: '运行', exact: true }).click();
  const second = page.getByRole('region', { name: '运行详情 · ' + runB, exact: true });
  await expect(second.getByRole('status')).toContainText('已处理 2', { timeout: 20000 });
  await second.getByText('对比另一运行（回归检验）').click();
  await second.locator('.compare-picker').getByRole('combobox').click();
  await page.getByRole('option', { name: new RegExp(runA) }).click();
  await second.getByRole('button', { name: '比较', exact: true }).click();
  await expect(page.getByText('符合当前协议的可比条件')).toBeVisible();
  await second.scrollIntoViewIfNeeded();
  await shot(page, 'comparison.png');

  // 深色 + 移动端：不得出现横向滚动
  await page.getByRole('button', { name: '切换为深色模式' }).click();
  await expect(page.locator('html')).toHaveClass(/dark/);
  await page.setViewportSize({ width: 390, height: 844 });
  // 「不得横向滚动」查不到被裁掉的文字——裁掉的部分根本不产生滚动。所以单独量每个
  // 表单标签：scrollWidth 超过 clientWidth 就是被裁了（组件库的 label 是 nowrap）。
  if (await page.getByRole('button', { name: '上传题集', exact: true }).count()) {
    await page.getByRole('button', { name: '上传题集', exact: true }).click();
  }
  // 量的是「label 的实宽 vs 它所在格子的宽」。两个坑都踩过：按 label 自己的
  // scrollWidth/clientWidth 断言是恒真的（nowrap 把它撑到 404×404，看不出裁切）；
  // 按格子的 scrollWidth 断言则会连带抓到控件的 4px 溢出（组件库的事，另记一条）。
  // 第一版恒真断言靠变异对照打回，第二版误抓控件溢出靠这次红发现。
  const clipped = await page.evaluate(() => [...document.querySelectorAll('.upload-fold .eb-form-item')]
    .map(item => ({ txt: item.textContent.trim().slice(0, 24), box: item.clientWidth,
      label: item.querySelector('.eb-form-item__label')?.getBoundingClientRect().width ?? 0 }))
    .filter(r => r.label > r.box + 1).map(r => r.txt + '(' + Math.round(r.label) + '>' + r.box + ')'));
  assert.deepEqual(clipped, [], 'form labels clipped at 390px: ' + clipped.join(' | '));
  await page.getByRole('heading', { name: '评测', exact: true, level: 2 }).scrollIntoViewIfNeeded();
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'mobile layout scrolls horizontally');
  await shot(page, 'mobile.png');

  // 走的是真接口而不是被前端短路的路由
  assert.ok(evalCalls.some((call) => call.startsWith('POST /v1/eval/datasets/validate')), 'GUI never validated a dataset');
  assert.ok(evalCalls.some((call) => call.startsWith('POST /v1/eval/datasets')), 'GUI never saved a dataset');
  assert.ok(evalCalls.some((call) => call.startsWith('POST /v1/eval/runs')), 'GUI never started a run');
  assert.ok(evalCalls.some((call) => call.startsWith('GET /v1/eval/runs')), 'GUI never polled run state');
  assert.ok(evalCalls.some((call) => call.endsWith('/compare')), 'GUI never compared two runs');
  assert.deepEqual(errors, []);
  console.log('PASS browser: upload fold validation (invalid blocked, passage warned), save, offline run, progress, frozen evidence, export, refresh + row-click persistence, comparison, dark mobile; no browser errors (' + evalCalls.length + ' eval requests)');
} finally {
  if (browser) await browser.close();
}

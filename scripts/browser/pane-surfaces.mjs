// Corpus / knowledge / engine rendering contract (fast gate, no Go build, no store).
//
// UI v4 rearranged these three panes, and until now the only thing any gate said
// about them was "the heading mounted and <main> was not empty". That is exactly
// the layer where a wiring break is invisible: 116 composable tests pass, `make
// check` passes, 187 e2e assertions pass, and a paid run still cannot start
// (3090d83). Same class, three more faces.
//
// Mocks are NOT invented: every payload shape below was dumped from a running
// `serve` against a real store first. That mattered immediately — a first draft
// returned `{}` for /v1/monitor/overview and `{ns:…}` for namespaces, and the two
// "defects" it appeared to prove (a missing 证据窗口 count, an empty 按知识库 row)
// were both the fixture's fault, not the UI's.
//
// Unknown endpoints are a hard failure: a pane that starts calling something new
// must not silently render `{}` the way the monitor pane did before this existed.
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { loadPlaywright, PLAYWRIGHT_HINT } from './playwright.mjs';

const playwright = loadPlaywright();
if (playwright.missing) {
  console.log('SKIP pane contract: @playwright/test not found (' + playwright.missing.join(', ') + ')');
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

// ---- fixtures, shaped from the live face ------------------------------------
const source = (id, title, bytes, cited) => ({ id, title, type: 'txt', uri: 'local://' + id, bytes, ingested_at: '2026-10-03T02:00:00Z', version: 1, cited });
const cluster = (id, name, lifecycle) => ({
  _id: id, topic_key: 'tk-' + id, level_keys: [{ level: 'L1', text: '连接池' }], name,
  content: name + '：参考答案正文', queries: [name === '连接池容量' ? '连接池多少？' : 'QPS 上限？'],
  confidence: 0.72, hotness: 0.4, lifecycle, version: 3, source_id: 'src:gate-pool#1',
  evidence: [{ start: 0, end: 12, content: '连接池最大连接数为 128', source: 'src:gate-pool#1', arm: 'lexical', covers: ['128'], score: 3 }],
});
const ROUTES = {
  // bucket.sources is deliberately 999: the corpus hero must show the LIVE list
  // count (2), not the ingest-time cached number (bucket.go:188).
  '/v1/buckets': { buckets: [{ name: 'mock', label: '门控库', sources: 999, clusters: 2, queries: 5 }], default_ns: 'mock' },
  '/v1/sources': { sources: [source('src:gate-qps#1', '限流手册', 48, 2), source('src:gate-pool#1', '连接池手册', 59, 7)] },
  '/v1/clusters': { clusters: [cluster('c-stable', '连接池容量', 'stable'), cluster('c-emerging', '限流阈值', 'emerging')], namespace: 'mock' },
  '/v1/affinity': { token: '连接池', namespace: 'mock', docs: [{ source_id: 'src:gate-pool#1', weight: 0.82 }, { source_id: 'src:gate-qps#1', weight: 0.31 }] },
  '/v1/learning': { namespace: 'mock', docs: { 'mock:clus_clusters': 2, 'mock:clus_evidence': 4, 'mock:clus_cites': 7 }, kv_keys: 3, total: 13, clean: false },
  '/v1/monitor/overview': {
    started_at: '2026-10-03T02:00:00Z', uptime_sec: 3600, queries: 5,
    system: { goroutines: 12, heap_mb: 24.5, rss_mb: 61.2, num_gc: 9, store_files_bytes: 2147483648, store_dir: '/tmp/mock/store' },
    llm: { calls: 0, tokens: 0, tokens_per_query: 0, calls_per_min: 0 },
    retrieval: { by_mode: {}, reuse_hits: 2, reuse_rate: 0.4, escalations: 1, self_corrected: 0, refused: 1, errors: 0, warm_count: 3, warm_p50_ms: 4, warm_p50_us: 4100, cold_count: 2, cold_p50_ms: 120, cold_p50_us: 120000, avg_confidence: 0.66, avg_coverage: 0.58, embedder: 'local-hash-64' },
    knowledge: { clusters: 2, by_lifecycle: { emerging: 1, stable: 1 }, avg_confidence: 0.71, avg_hotness: 0.4, evidence_windows: 4, contested: 0, needing_review: 1 },
    namespaces: [{ namespace: 'mock', queries: 5, reuse_hits: 2, avg_p50_us: 4100 }], recent: [],
  },
  '/v1/ingest/jobs': { jobs: [] },
  '/v1/sessions': { sessions: [] },
  '/v1/models': { models: [] },
  '/v1/model': { configured: false },
  '/v1/config': { source: '/dev/null', values: {}, secrets: [] },
  '/v1/usage': { rows: [], totals: {} },
  '/v1/eval/runs': { runs: [] },
};
const misses = [];
const calls = [];
const errors = [];
const failures = [];

const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1100 }, reducedMotion: 'reduce' });
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push(message.text()); });
  page.on('response', (response) => { if (!response.ok() && response.url().includes('/v1/')) failures.push(response.status() + ' ' + response.url()); });
  await page.route('**/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    calls.push(request.method() + ' ' + url.pathname);
    let data = ROUTES[url.pathname];
    if (url.pathname.startsWith('/v1/clusters/')) {
      data = url.pathname.endsWith('/review')
        ? { valid: true, checked: 2, windows: [{ ok: true, source: 'src:gate-pool#1' }], lifecycle: 'stable' }
        : { cluster: cluster('c-emerging', '限流阈值', 'emerging'), cites: [], namespace: 'mock' };
    }
    if (url.pathname.startsWith('/v1/sources/')) {
      data = { id: 'src:gate-pool#1', title: '连接池手册', type: 'txt', body: '连接池最大连接数为 128。连接超时为 30 秒。', version: 1, uri: 'local://gate-pool', bytes: 59 };
    }
    if (data === undefined) { misses.push(url.pathname + url.search); data = {}; }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  });
  const text = (loc) => loc.innerText().then(s => s.replace(/\s+/g, ' '));
  // 屏上数字带千分位（4,100），两种写法都算命中
  const renders = (hay, needle) => hay.includes(needle) || hay.includes(Number(needle).toLocaleString('en-US'));

  await page.goto(base + '/ui/#/corpus');
  await expect(page.getByRole('heading', { name: '语料', exact: true, level: 2 })).toBeVisible();

  // 语料：概览读数取的是「当前活着的列表」，不是摄取时缓存的桶计数
  await expect(page.locator('.ov-hero')).toContainText(/2\s*篇文档在库/);
  assert.ok(!(await text(page.locator('.ov-hero'))).includes('999'), 'the corpus hero reads the cached bucket count instead of the live list');
  const table = page.getByRole('table').filter({ hasText: '限流手册' });
  await expect(table.getByRole('cell', { name: '7', exact: true })).toBeVisible(); // 被引用列来自 sources[].cited
  await page.getByRole('textbox', { name: '搜索已导入文档', exact: true }).fill('限流');
  await expect(page.locator('.document-footer')).toContainText(/1\s*\/\s*2\s*篇/);
  assert.equal(await table.getByRole('row').count(), 1, 'the document filter did not narrow the rendered table');
  await page.getByRole('button', { name: '去提问', exact: true }).click();
  await expect(page.getByRole('heading', { name: '检索问答', exact: true, level: 1 })).toBeVisible();

  await page.evaluate(() => { location.hash = '#/knowledge'; });
  await expect(page.getByRole('heading', { name: '知识', exact: true, level: 2 })).toBeVisible();

  // 知识：三个读数分别来自三个不同的地方，缺一个都不会被别的面看见
  await expect(page.locator('.ov-hero')).toContainText(/2\s*个答案簇\s*·\s*1\s*待复核/);
  await expect(page.getByText(/^4\s*个原文窗口被簇引用/)).toBeVisible();
  await page.getByRole('tab', { name: '待复核', exact: true }).click();
  const rail = page.locator('.rail-item');
  assert.equal(await rail.count(), 1, 'the 待复核 tab did not filter the cluster rail');
  // 列表项必须键盘可达：role=button + tabindex + Enter 能打开详情。
  // 它曾经是一个无 role 的 `div @click`——鼠标能用、键盘到不了、读屏念不出这是可选项。
  const firstItem = page.getByRole('button', { name: /QPS 上限？/ });
  await expect(firstItem).toBeVisible();
  await firstItem.press('Enter');
  await expect(page.locator('.frow2')).toContainText('限流阈值');
  await expect(page.locator('.pre-block')).toContainText('限流阈值：参考答案正文');
  await page.getByRole('button', { name: '复核这个簇', exact: true }).click();
  await expect(page.locator('.review-row')).toContainText(/复核通过：2\s*个证据窗口与当前原文一致，已转为稳定。/);
  assert.ok(calls.includes('POST /v1/clusters/c-emerging/review'), '复核 never reached the wire');

  // 词档关联：/v1/affinity 是「接口一直都有、脸上没有」那一条，W4 才接上。
  // 断言的是标题而不是 id——source_id → title 的 join 就在这一步。
  const affinity = page.getByRole('region', { name: '词档关联', exact: true });
  await expect(affinity.getByRole('button', { name: '查关联', exact: true })).toBeDisabled();
  await page.getByRole('textbox', { name: '要查询的词', exact: true }).fill('连接池');
  await affinity.getByRole('button', { name: '查关联', exact: true }).click();
  // 用 expect 等而不是读完再判：查询是异步的，点完立刻读会读到还没回来的那一帧
  await expect(affinity.getByText('连接池手册')).toBeVisible();
  await expect(affinity.getByText('限流手册')).toBeVisible();
  assert.ok(calls.includes('GET /v1/affinity'), '查关联 never reached the wire');
  const affinityText = await text(affinity);
  assert.ok(affinityText.includes('连接池手册') && affinityText.includes('限流手册'), 'affinity rows lost the document titles: ' + affinityText);
  assert.ok(affinityText.includes('0.820') && affinityText.includes('0.310'), 'affinity weights did not render: ' + affinityText);

  await page.evaluate(() => { location.hash = '#/engine'; });
  await expect(page.getByRole('heading', { name: '引擎', exact: true, level: 2 })).toBeVisible();

  // 引擎：monitor 的读数必须真的落到格子上。这个面板是本轮才发现「响应形状不对就整片
  // 抛 TypeError」的那一个，所以这里既断言有值，也断言值来自哪个字段。
  const monitorText = await text(page.locator('main'));
  for (const needle of ['40%', '0.660', '/tmp/mock/store', 'local-hash-64', '12']) {
    assert.ok(monitorText.includes(needle), 'monitor surface did not render ' + needle);
  }
  const nsRow = page.getByRole('row', { name: /mock/ });
  assert.ok(await nsRow.count(), 'the 按知识库 table rendered no row for the namespace');
  const nsText = await text(nsRow);
  assert.ok(renders(nsText, '4100'), 'avg_p50_us did not reach the 按知识库 row: ' + nsText);
  for (const [tab, anchor] of [['模型与配置', '新建模型配置'], ['消费记录', '还没有消费记录'], ['原始检索', '测一问，看召回']]) {
    await page.getByRole('tab', { name: tab, exact: true }).click();
    await expect(page.getByText(anchor, { exact: false }).first()).toBeVisible();
  }

  assert.deepEqual(misses, [], 'the UI called endpoints this gate has no fixture for: ' + misses.join(', '));
  assert.deepEqual(failures, [], 'the GUI saw failing /v1 responses: ' + failures.join('; '));
  assert.deepEqual(errors, [], 'the GUI raised page/console errors');
  console.log('PASS pane contract: corpus reads the live list not the cached count, filter narrows rows, 去提问 switches pane; knowledge hero joins three separate sources, 待复核 filters, 复核 posts and reports, affinity joins id→title; engine renders monitor readings + all four tabs (' + calls.length + ' mocked requests)');
} finally {
  await browser.close();
  serving.close();
}

// Chat (检索问答) rendering contract (fast gate, no Go build, no store).
//
// The last pane without any rendering-level check. Its wire is the hardest to
// fake: OpenAI-style SSE chunks plus cumulus extension blocks
// ({"cumulus":{"kind":"stage|citations|done",…}}). The fixture below is NOT
// hand-written — it was captured from `serve -CLUS_OFFLINE=1` answering a real
// ingested document, so every field name and unit is the face's own.
//
// What this pins that 116 composable tests cannot: the answer text, the citation
// row (title + span + quote), the per-stage timing block (µs on the wire → ms on
// screen) and the run card (mode/confidence/coverage) all arrive through three
// different fold paths, and a break in any one of them renders as a plausible
// but wrong answer rather than an error.
//
// Session fixtures are equally measured: GET /v1/sessions returns a BARE ARRAY
// (chat.js:385 rejects anything else with 会话列表响应格式无效 — that strictness is
// what caught this gate's own first draft, which wrapped it in {sessions:…}).
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { loadPlaywright, PLAYWRIGHT_HINT } from './playwright.mjs';

const playwright = loadPlaywright();
if (playwright.missing) {
  console.log('SKIP chat contract: @playwright/test not found (' + playwright.missing.join(', ') + ')');
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

const ANSWER = '【摘要】连接池最大连接数是多少？\n【来源】连接池手册\n[1] (body full [0,25)) 连接池最大连接数为 128。连接超时为 30 秒。\n';
const QUOTE = '连接池最大连接数为 128。连接超时为 30 秒。';
const frame = (obj) => 'data: ' + JSON.stringify(obj);
const SSE = [
  frame({ choices: [{ delta: { role: 'assistant' }, index: 0 }] }),
  frame({ cumulus: { kind: 'stage', payload: { elapsed_ms: 0, name: 'analyze', stage: 'stage', stage_ms: 6 } } }),
  frame({ cumulus: { kind: 'stage', payload: { elapsed_ms: 6, name: 'cascade', stage: 'stage', stage_ms: 15 } } }),
  frame({ cumulus: { kind: 'stage', payload: { elapsed_ms: 21, name: 'sample', stage: 'stage', stage_ms: 3 } } }),
  frame({ cumulus: { kind: 'stage', payload: { elapsed_ms: 24, name: 'synth', stage: 'stage', stage_ms: 1 } } }),
  frame({ choices: [{ delta: { content: ANSWER }, index: 0 }] }),
  frame({ cumulus: { kind: 'citations', payload: { refs: [{ index: 1, source_id: 'src:pool#1', title: '连接池手册', start: 0, end: 25, quote: QUOTE, span: 'body', resolved: true }], legend: '引用编号 [n] 对应下方 refs；(span) 为原文定位标签。' } } }),
  frame({ choices: [{ delta: {}, finish_reason: 'stop', index: 0 }], usage: { total_tokens: 0 } }),
  frame({ cumulus: { kind: 'done', payload: { cluster_id: 'C15baec1313cfa14e', conf: 0.712, coverage: 0.7, latency_ms: 4, loops: 0, mode: 'FAST', refused: false, reused: false, skipped: false, stages: { analyze: 6000, cascade: 15000, sample: 3000, synth: 1000 }, stop_reason: '', tokens: 0, widened: 0 } } }),
  'data: [DONE]',
].join('\n\n') + '\n\n';

const STAMP = 1791036897098;
const ROUTES = {
  '/v1/buckets': { buckets: [{ name: 'mock', label: '问答门控库', sources: 1, clusters: 0, queries: 1 }], default_ns: 'mock' },
  '/v1/sources': { sources: [{ id: 'src:pool#1', title: '连接池手册', type: 'txt', uri: 'local://pool', bytes: 59, ingested_at: '2026-10-03T02:00:00Z', version: 1, cited: 1 }] },
  '/v1/sessions': [{ id: 's-persisted', title: '连接池会话', created_at: STAMP, updated_at: STAMP, messages: null }],
  '/v1/sessions/s-persisted': { id: 's-persisted', title: '连接池会话', created_at: STAMP, updated_at: STAMP, messages: [
    { role: 'user', content: '连接超时是多少秒？' },
    { role: 'assistant', content: '连接超时为 30 秒。', sources: [{ index: 1, source_id: 'src:pool#1', title: '连接池手册', quote: QUOTE, span: 'body', resolved: true }], stats: { mode: 'FAST', conf: 0.5, coverage: 0.4, stages: { analyze: 2000 } } },
  ] },
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
    if (url.pathname === '/v1/chat/completions') {
      await route.fulfill({ status: 200, headers: { 'content-type': 'text/event-stream' }, body: SSE });
      return;
    }
    let data = ROUTES[url.pathname];
    if (data === undefined) { misses.push(url.pathname + url.search); data = {}; }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
  });
  const text = (loc) => loc.innerText().then(s => s.replace(/\s+/g, ' '));

  await page.goto(base + '/ui/#/chat');
  await expect(page.getByRole('heading', { name: '你想从文档中了解什么？', exact: true })).toBeVisible();
  const composer = page.getByRole('textbox', { name: /向当前知识库提问/ });
  await expect(page.getByRole('button', { name: '发送', exact: true })).toBeDisabled();

  // 会话列表是真从服务端读的：裸数组里那一条必须出现在历史会话导航里。
  // 按钮的可及名带着 created_at 格式化出来的时间（1791036897098 是毫秒），所以这里
  // 只钉「日期出现了」这一形状——写死日期会让门跟着运行机器的时区跑。
  await expect(page.getByRole('button', { name: /连接池会话 \d{4}-\d{2}-\d{2}/ })).toBeVisible();

  await composer.fill('连接池最大连接数是多少？');
  await expect(page.getByRole('button', { name: '发送', exact: true })).toBeEnabled();
  await page.keyboard.press('Enter');

  // 答案正文（流式 delta 落地后的那一段）。取【摘要】这一段而不是引文：同一段引文
  // 同时出现在答案和引用行里，按引文断言会撞上 strict mode。
  const thread = page.getByRole('log', { name: '对话消息' });
  await expect(thread.getByText(/【摘要】/)).toBeVisible({ timeout: 10000 });
  assert.ok(calls.includes('POST /v1/chat/completions'), 'the question never reached the chat face');

  // 引用行：标题 + span + 引文，三样都得在同一行——这是「可核对」的全部内容
  const citation = thread.getByRole('button', { name: /连接池手册/ }).first();
  const citationText = await text(citation);
  assert.ok(citationText.includes('src:pool#1'), 'citation row lost the source id: ' + citationText);
  assert.ok(citationText.includes(QUOTE), 'citation row lost the quoted span: ' + citationText);
  assert.ok(citationText.includes('body'), 'citation row lost the span label: ' + citationText);

  // 分步耗时：wire 上是微秒，屏上得是毫秒（4 段都到齐才算折对）
  const stages = thread.getByText(/检索过程 · 4 段/);
  await expect(stages).toBeVisible();
  assert.match(await text(stages), /25\s*ms/, 'per-stage µs did not become ms: ' + await text(stages));

  // 运行卡：mode / 置信度 / 覆盖率 三个读数各来自 done payload 的一个字段
  const card = await text(thread);
  assert.ok(card.includes('快速回答'), 'mode FAST did not render as 快速回答');
  assert.ok(card.includes('71%'), 'confidence 0.712 did not render as 71%');
  assert.ok(card.includes('70%'), 'coverage 0.7 did not render as 70%');

  // 重新打开一个持久化会话：恢复路径与实时路径共用引用映射，断的是老消息也带引用。
  // 会话栏默认就是展开的（上面那条断言已经证明列表在屏上），不需要先点开它。
  await page.getByRole('button', { name: /连接池会话/ }).first().click();
  await expect(thread.getByText(/连接超时是多少秒？/)).toBeVisible();
  assert.ok(calls.includes('GET /v1/sessions/s-persisted'), 'opening a session never fetched it');

  assert.deepEqual(misses, [], 'the UI called endpoints this gate has no fixture for: ' + misses.join(', '));
  assert.deepEqual(failures, [], 'the GUI saw failing /v1 responses: ' + failures.join('; '));
  assert.deepEqual(errors, [], 'the GUI raised page/console errors: ' + errors.join(' | '));
  console.log('PASS chat contract: SSE answer + citation row (id/span/quote) + per-stage µs→ms + run card readings all fold onto the screen; session list is a bare array and reopening one refetches it (' + calls.length + ' mocked requests)');
} finally {
  await browser.close();
  serving.close();
}

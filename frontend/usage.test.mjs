import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./usage.js', import.meta.url), 'utf8');

function usagePanelEnv() {
  const nodes = new Map();
  const makeNode = (id = '') => {
    const node = {
      id, textContent: '', hidden: false, value: '', disabled: false, open: false, className: '',
      children: [], dataset: {}, style: {},
      replaceChildren(...children) { node.children = children; },
      append(...children) { node.children.push(...children); },
      addEventListener() {}, setAttribute() {}, getAttribute() { return ''; },
      querySelector() { return null; }, querySelectorAll() { return []; }, focus() {},
    };
    return node;
  };
  const document = {
    getElementById(id) {
      if (!nodes.has(id)) nodes.set(id, makeNode(id));
      return nodes.get(id);
    },
    createElement() { return makeNode(); },
    createElementNS() { return makeNode(); },
    querySelectorAll() { return []; },
  };
  const runtime = {};
  new Function('document', 'globalThis', source)(document, runtime);
  return { nodes, mount: runtime.KimiWorkUsage.mount };
}

const flush = () => new Promise(resolve => setImmediate(resolve));

/* 端点会话更换后，旧请求即使晚到也不能再写回面板；reset 还要清掉已渲染的
   旧账本和额度卡片，show 在已有请求在途时不能重复发起。 */
test('a reset invalidates in-flight usage and quota reads and clears rendered data', async () => {
  const env = usagePanelEnv();
  const calls = [];
  const request = (path, options) => new Promise((resolve, reject) => calls.push({ path, options, resolve, reject }));
  const panel = env.mount({ request, notify() {} });

  panel.show();
  assert.deepEqual(calls.map(call => call.path), ['/api/usage/query', '/api/quota']);
  panel.show();
  assert.equal(calls.length, 2, 'show must not duplicate requests while reads are pending');

  panel.reset();
  assert.equal(env.nodes.get('usage-stat-input').textContent, '—');
  assert.equal(env.nodes.get('quota-list').children.length, 0);
  assert.equal(env.nodes.get('usage-records-body').children.length, 0);

  calls[0].resolve({ summary: { inputTokens: 999 }, records: [{ at: '2026-11-19T00:00:00Z', inputTokens: 999 }], filters: {} });
  calls[1].resolve({ records: [{ sourceName: '旧来源' }] });
  await flush();
  assert.equal(env.nodes.get('usage-stat-input').textContent, '—', 'a stale usage reply must not overwrite the reset panel');
  assert.equal(env.nodes.get('quota-list').children.length, 0, 'a stale quota reply must not refill the old ledger');

  panel.show();
  panel.show();
  assert.equal(calls.length, 4, 'the new endpoint should be read once and not duplicated');
  calls[2].resolve({ summary: { inputTokens: 12 }, records: [], filters: {} });
  calls[3].resolve({ records: [] });
  await flush();
  assert.equal(env.nodes.get('usage-stat-input').textContent, '12');
});

test('quota source editor only exposes built-in official balance adapters without custom JSON path inputs', async () => {
  assert.equal(source.includes('quotaPathProblem'), false);
  assert.equal(source.includes('value="custom"'), false);
  assert.match(source, /<option value="deepseek">DeepSeek 官方余额<\/option>/);
  assert.match(source, /<option value="moonshot-cn">Moonshot 国内余额<\/option>/);
  assert.match(source, /<option value="moonshot-global">Moonshot 国际余额<\/option>/);

  const nodes = new Map();
  const makeNode = (id = '') => {
    const node = {
      id, textContent: '', hidden: false, value: '', disabled: false, open: false, className: '', innerHTML: '',
      children: [], dataset: {}, style: {}, listeners: {},
      replaceChildren(...children) { node.children = children; },
      append(...children) { node.children.push(...children); },
      addEventListener(type, fn) { (node.listeners[type] ||= []).push(fn); },
      setAttribute() {}, getAttribute() { return ''; },
      querySelector() { return makeNode(); }, querySelectorAll() { return []; }, focus() {},
    };
    if (id) nodes.set(id, node);
    return node;
  };
  const document = {
    getElementById(id) {
      if (id === 'quota-source-form' && !nodes.get('quota-source-editor')?.innerHTML.includes('id="quota-source-form"')) {
        return null;
      }
      if (!nodes.has(id)) nodes.set(id, makeNode(id));
      return nodes.get(id);
    },
    createElement() { return makeNode(); },
    createElementNS() { return makeNode(); },
    querySelectorAll() { return []; },
  };
  const runtime = {};
  new Function('document', 'globalThis', source)(document, runtime);
  const calls = [];
  const panel = runtime.KimiWorkUsage.mount({
    request: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/quota') return { records: [], sources: [], connections: [{ id: 'conn-ds', name: 'DeepSeek 主连接' }] };
      if (path === '/api/quota/sources') return { records: [], sources: [], connections: [{ id: 'conn-ds', name: 'DeepSeek 主连接' }] };
      return { summary: {}, records: [], filters: {} };
    },
    notify() {},
  });
  panel.show();
  await flush();

  document.getElementById('quota-source-adapter').value = 'deepseek';
  document.getElementById('quota-source-connection').value = 'conn-ds';
  nodes.get('quota-add').listeners.click[0]();

  const editorHtml = nodes.get('quota-source-editor').innerHTML;
  assert.doesNotMatch(editorHtml, /value="custom"/);
  assert.doesNotMatch(editorHtml, /quota-source-path|quota-source-reset|quota-source-used|quota-source-limit|quota-source-body/);
  assert.match(editorHtml, /deepseek/);
  assert.match(editorHtml, /moonshot-cn/);
  assert.match(editorHtml, /moonshot-global/);
  assert.match(nodes.get('quota-adapter-info').textContent, /https:\/\/api\.deepseek\.com\/user\/balance/);

  nodes.get('quota-source-adapter').value = 'moonshot-global';
  nodes.get('quota-source-adapter').listeners.change[0]();
  assert.match(nodes.get('quota-adapter-info').textContent, /https:\/\/api\.moonshot\.ai\/v1\/users\/me\/balance/);

  await nodes.get('quota-source-form').listeners.submit[0]({
    preventDefault() {},
    target: { querySelector() { return makeNode(); } },
  });
  const saveCall = calls.find(call => call.path === '/api/quota/sources');
  assert.ok(saveCall);
  assert.equal(saveCall.options.body.action, 'upsert');
  assert.equal(saveCall.options.body.source.adapter, 'moonshot-global');
  assert.equal(saveCall.options.body.source.endpoint, 'https://api.moonshot.ai/v1/users/me/balance');
  assert.equal(saveCall.options.body.source.auth.connectionId, 'conn-ds');
});

/* renderChart 需要真实 DOM 才能跑；这里只做一个够用的替身，用它验证
   图表在单点、多点与缺失值下的几何行为，以及 viewBox 是否随数据自适应。 */
function chartEnv() {
  const created = [];
  const makeElement = (ns, tag) => {
    const element = {
      tag, attrs: {}, children: [], textContent: '', className: '',
      setAttribute(name, value) { this.attrs[name] = String(value); },
      getAttribute(name) { return this.attrs[name]; },
      append(...nodes) { this.children.push(...nodes); },
      replaceChildren(...nodes) { this.children = nodes; },
      addEventListener() {},
    };
    created.push(element);
    return element;
  };
  const roots = { 'usage-chart-body': makeElement('html', 'div') };
  const document = {
    getElementById: id => roots[id] || null,
    createElement: tag => makeElement('html', tag),
    createElementNS: (_ns, tag) => makeElement('svg', tag),
  };
  const fnStart = source.indexOf('  function chartGeometry(buckets) {');
  const fnEnd = source.indexOf('\n  function selectBucket(bucket) {');
  assert.notEqual(fnStart, -1, 'missing chartGeometry in current source');
  assert.notEqual(fnEnd, -1, 'missing renderChart in current source');
  const code = source.slice(fnStart, fnEnd) + '\n  return { renderChart };';
  const chart = new Function('document', '$', 'number', 'token', 'fmt', 'bucketLabel', 'selectBucket', 'Intl', 'Number', 'Math', code)(
    document,
    id => roots[id] || null,
    value => Number.isSafeInteger(value) && value >= 0 ? value : null,
    (row, name) => {
      const value = row?.[name + 'Tokens'] ?? row?.[name];
      return Number.isSafeInteger(value) && value >= 0 ? value : null;
    },
    value => Number.isSafeInteger(value) ? new Intl.NumberFormat('zh-CN').format(value) : '未提供',
    bucket => bucket.label,
    () => {},
    Intl, Number, Math,
  );
  return { renderChart: chart.renderChart, roots, created };
}

const svgOf = (env) => env.roots['usage-chart-body'].children[0];

test('a single bucket still shows both token series', () => {
  const env = chartEnv();
  env.renderChart([{ label: '10:00', requests: 1, input: 120, output: 40 }]);
  const svg = svgOf(env);
  const points = svg.children.filter(node => node.tag === 'circle');
  const classOf = node => node.attrs.class || '';
  assert.equal(points.filter(node => classOf(node) === 'usage-chart__point').length, 1);
  assert.equal(points.filter(node => classOf(node).includes('usage-chart__point--output')).length, 1);
  // 单点没有相邻点可连，但两个系列各自都要有可见标记。
  assert.ok(svg.children.some(node => classOf(node).includes('usage-chart__tick') && !classOf(node).includes('--output')));
});

test('a missing value breaks the line instead of reading as zero', () => {
  const env = chartEnv();
  env.renderChart([
    { label: 'a', requests: 1, input: 100, output: 10 },
    { label: 'b', requests: 1, output: 20 },
    { label: 'c', requests: 1, input: 60, output: 30 },
  ]);
  const svg = svgOf(env);
  const classOf = node => node.attrs.class || '';
  /* 空缺两侧各剩一个孤立点，因此是两段单点标记而不是一条贯穿的线：
     把缺失值当 0 连起来会是两条 path。 */
  const inputMarks = svg.children.filter(node => node.tag === 'line'
    && classOf(node) === 'usage-chart__line usage-chart__tick');
  assert.equal(inputMarks.length, 2, 'the gap must split the input series in two');
  const inputPaths = svg.children.filter(node => node.tag === 'path' && classOf(node) === 'usage-chart__line');
  assert.equal(inputPaths.length, 0, 'a missing bucket must not be bridged by a line');
  for (const mark of inputMarks) {
    assert.ok(Number(mark.attrs.y1) <= 184, 'a missing bucket must not be plotted at zero height');
  }
});

test('the chart width follows the number of buckets instead of one fixed viewBox', () => {
  const narrow = chartEnv();
  narrow.renderChart([{ label: 'a', requests: 1, input: 10, output: 5 }]);
  const wide = chartEnv();
  wide.renderChart(Array.from({ length: 24 }, (_, index) => ({ label: String(index), requests: 1, input: 10, output: 5 })));
  const widthOf = (env) => Number(svgOf(env).attrs.viewBox.split(' ')[2]);
  assert.ok(widthOf(wide) > widthOf(narrow), 'more buckets must get more width');
  assert.equal(svgOf(narrow).attrs.preserveAspectRatio, 'xMidYMid meet');
});

test('every point of both series is keyboard reachable', () => {
  const env = chartEnv();
  env.renderChart([{ label: 'a', requests: 1, input: 10, output: 5 }, { label: 'b', requests: 1, input: 20, output: 8 }]);
  const points = svgOf(env).children.filter(node => node.tag === 'circle');
  assert.equal(points.length, 4);
  for (const point of points) {
    assert.equal(point.attrs.tabindex, '0');
    assert.equal(point.attrs.role, 'button');
    assert.match(point.attrs['aria-label'], /输入|输出/);
  }
});

test('usage export uses POST /api/usage/export, official quota links use POST /api/open-url, and quota boundary copy is present', async () => {
  assert.equal(source.includes('createObjectURL'), false);
  assert.equal(source.includes("target = '_blank'"), false);
  assert.equal(source.includes('target="_blank"'), false);
  assert.match(source, /联网搜索、调用 Kimi 资料库，以及 PPT、3D、图片生成、视频生成等能力仍走 Kimi Work 官方自身额度；KimiWork \+ 仅接管对话与子代理模型请求/);

  const nodes = new Map();
  const makeEl = (tag = 'div', id = '') => {
    const el = {
      tag, id, className: '', textContent: '', value: '', hidden: false, disabled: false, open: false,
      href: '', rel: '', dataset: {}, style: {}, children: [], listeners: {},
      setAttribute() {},
      append(...items) { this.children.push(...items); },
      replaceChildren(...items) { this.children = [...items]; },
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
      querySelector() { return makeEl('span'); },
      querySelectorAll() { return []; },
      focus() {},
    };
    if (id) nodes.set(id, el);
    return el;
  };
  for (const id of [
    'usage-root', 'usage-error', 'usage-custom-form', 'usage-from', 'usage-to',
    'usage-filter-model', 'usage-export', 'usage-clear', 'quota-refresh', 'quota-add',
    'quota-list', 'quota-empty', 'quota-sources', 'quota-source-editor',
  ]) makeEl('div', id);

  const prevDoc = globalThis.document;
  const calls = [];
  const toasts = [];
  globalThis.document = {
    getElementById: (id) => nodes.get(id) || null,
    createElement: (tag) => makeEl(tag),
    querySelectorAll: () => [],
  };
  try {
    const mod = {};
    new Function('globalThis', source)(mod);
    let exportResponse = { ok: true, path: '/tmp/usage/export-20261124.json' };
    mod.KimiWorkUsage.mount({
      request: async (path, opts) => {
        calls.push({ path, opts });
        if (path === '/api/usage/export') return exportResponse;
        if (path === '/api/quota/refresh') {
          return {
            records: [
              { sourceId: 'ds', sourceName: 'DeepSeek', kind: 'api_balance', status: 'ok', officialUrl: 'https://platform.deepseek.com/usage' },
              { sourceId: 'bad', sourceName: 'Bad', kind: 'api_balance', status: 'ok', officialUrl: 'http://insecure.example.com' },
            ],
            sources: [],
          };
        }
        return { ok: true };
      },
      notify: (msg, tone) => toasts.push({ msg, tone }),
    });

    await nodes.get('usage-export').listeners.click[0]();
    const expCall = calls.find(c => c.path === '/api/usage/export');
    assert.ok(expCall);
    assert.equal(expCall.opts.method, 'POST');
    assert.match(toasts.at(-1).msg, /\/tmp\/usage\/export-20261124\.json/);

    exportResponse = { ok: true };
    await nodes.get('usage-export').listeners.click[0]();
    assert.match(nodes.get('usage-error').textContent, /未返回导出文件路径/);

    await nodes.get('quota-refresh').listeners.click[0]();
    const links = [];
    (function collectLinks(n) {
      if (!n) return;
      if (n.tag === 'a') links.push(n);
      for (const c of n.children || []) collectLinks(c);
    })(nodes.get('quota-list'));
    assert.equal(links.length, 1, 'only https officialUrl links may render');
    let prevented = false;
    await links[0].listeners.click[0]({ preventDefault() { prevented = true; } });
    assert.equal(prevented, true);
    assert.deepEqual(calls.find(c => c.path === '/api/open-url'), {
      path: '/api/open-url',
      opts: { method: 'POST', body: { url: 'https://platform.deepseek.com/usage' } },
    });
  } finally {
    globalThis.document = prevDoc;
  }
});

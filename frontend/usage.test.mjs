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

test('usage panel renders 3 subtabs and folds external quota sources inside usage-pane-quotas', async () => {
  const nodes = new Map();
  const makeNode = (id = '') => {
    const node = {
      id, textContent: '', hidden: false, value: '', disabled: false, open: false, className: '', innerHTML: '',
      children: [], dataset: {}, style: {}, listeners: {}, attrs: {},
      replaceChildren(...children) { node.children = children; },
      append(...children) { node.children.push(...children); },
      addEventListener(type, fn) { (node.listeners[type] ||= []).push(fn); },
      setAttribute(name, value) { node.attrs[name] = String(value); },
      getAttribute(name) { return node.attrs[name] || ''; },
      querySelector() { return makeNode(); },
      querySelectorAll() { return []; },
      focus() {},
    };
    if (id) nodes.set(id, node);
    return node;
  };
  const tabs = ['requests', 'quotas', 'budget'].map(name => {
    const tab = makeNode('usage-tab-' + name);
    tab.dataset.usageTab = name;
    return tab;
  });
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
    querySelectorAll(selector) {
      if (selector === '[data-usage-tab]') return tabs;
      return [];
    },
  };
  const runtime = {};
  new Function('document', 'globalThis', source)(document, runtime);

  const calls = [];
  let sourcesState = [
    { sourceId: 'ds-1', sourceName: 'DeepSeek 余额', kind: 'api_balance', endpoint: 'https://api.deepseek.com/user/balance', removable: true },
  ];
  const panel = runtime.KimiWorkUsage.mount({
    request: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/quota') return { records: [], sources: sourcesState, connections: [{ id: 'conn-ds', name: 'DeepSeek' }] };
      if (path === '/api/quota/sources') {
        if (options.body.action === 'upsert') sourcesState = [...sourcesState, { ...options.body.source, removable: true }];
        if (options.body.action === 'remove') sourcesState = sourcesState.filter(s => s.sourceId !== options.body.sourceId);
        return { records: [], sources: sourcesState, connections: [{ id: 'conn-ds', name: 'DeepSeek' }] };
      }
      return { summary: {}, records: [], filters: {} };
    },
    notify() {},
  });

  const html = nodes.get('usage-root').innerHTML;
  assert.match(html, /class="subtabs"\s+role="tablist"/);
  assert.match(html, /id="usage-tab-requests"[^>]*data-usage-tab="requests"[^>]*>请求统计</);
  assert.match(html, /id="usage-tab-quotas"[^>]*data-usage-tab="quotas"[^>]*>额度总览</);
  assert.match(html, /id="usage-tab-budget"[^>]*data-usage-tab="budget"[^>]*>预算保护</);
  assert.equal(html.includes('usage-tab-sources'), false);
  assert.equal(html.includes('usage-pane-sources'), false);
  const quotasStart = html.indexOf('id="usage-pane-quotas"');
  const budgetStart = html.indexOf('id="usage-pane-budget"');
  const quotasSection = html.slice(quotasStart, budgetStart);
  assert.match(quotasSection, /<details class="growth-disclosure" id="quota-sources-disclosure">/);
  assert.match(quotasSection, /<summary>管理外部额度来源<\/summary>/);
  assert.match(quotasSection, /id="quota-add"/);
  assert.match(quotasSection, /id="quota-source-editor"/);
  assert.match(quotasSection, /id="quota-source-list"/);

  panel.show();
  await flush();

  tabs[1].listeners.click[0]();
  assert.equal(nodes.get('usage-pane-requests').hidden, true);
  assert.equal(nodes.get('usage-pane-quotas').hidden, false);
  assert.equal(nodes.get('usage-pane-budget').hidden, true);
  assert.equal(tabs[1].attrs['aria-selected'], 'true');

  const disclosure = document.getElementById('quota-sources-disclosure');
  disclosure.open = false;
  nodes.get('quota-add').listeners.click[0]();
  assert.equal(disclosure.open, true, 'clicking quota-add must open quota-sources-disclosure');

  const removeButtons = [];
  (function walk(node) {
    if (!node) return;
    if (node.textContent === '删除来源') removeButtons.push(node);
    for (const child of node.children || []) walk(child);
  })(nodes.get('quota-source-list'));
  assert.equal(removeButtons.length, 1);
  await removeButtons[0].listeners.click[0]();
  assert.deepEqual(calls.at(-1), {
    path: '/api/quota/sources',
    options: { method: 'POST', body: { action: 'remove', sourceId: 'ds-1' } },
  });
});

/* ---------- growth.js 扩展增强即点即存、收起与重启启用测试 ---------- */

const growthSource = readFileSync(new URL('./growth.js', import.meta.url), 'utf8');

function growthEnv({ includeRestartBtn = true, errorsInsideBody = false } = {}) {
  const nodes = new Map();
  const makeNode = (tag = 'div', id = '') => {
    const node = {
      tagName: tag.toUpperCase(), id, textContent: '', value: '', checked: false,
      hidden: false, disabled: false, open: false, readOnly: false, title: '', className: '',
      parentNode: null, children: [], dataset: {}, style: {}, listeners: {},
      get options() { return node.children; },
      replaceChildren(...items) {
        node.children = [];
        for (const item of items) { item.parentNode = node; node.children.push(item); }
      },
      append(...items) {
        for (const item of items) { item.parentNode = node; node.children.push(item); }
      },
      before(item) {
        if (!node.parentNode) return;
        const idx = node.parentNode.children.indexOf(node);
        item.parentNode = node.parentNode;
        if (idx >= 0) node.parentNode.children.splice(idx, 0, item);
        else node.parentNode.children.push(item);
      },
      after(item) {
        if (!node.parentNode) return;
        if (item.parentNode && item.parentNode !== node.parentNode) {
          const oldIdx = item.parentNode.children.indexOf(item);
          if (oldIdx >= 0) item.parentNode.children.splice(oldIdx, 1);
        }
        const idx = node.parentNode.children.indexOf(node);
        item.parentNode = node.parentNode;
        if (idx >= 0) node.parentNode.children.splice(idx + 1, 0, item);
        else node.parentNode.children.push(item);
        if (item.id) nodes.set(item.id, item);
      },
      addEventListener(type, fn) { (node.listeners[type] ||= []).push(fn); },
      dispatch(type) {
        const results = [];
        for (const fn of node.listeners[type] || []) results.push(fn({ target: node, preventDefault() {} }));
        return Promise.all(results);
      },
      closest() { return null; },
      focus() {},
    };
    if (id) nodes.set(id, node);
    return node;
  };

  const panel = makeNode('section', 'translation-panel');
  const trEnabled = makeNode('input', 'tr-enabled');
  const trBody = makeNode('div', 'tr-body');
  trBody.hidden = true;
  const trProvider = makeNode('select', 'tr-provider');
  const trLanguage = makeNode('input', 'tr-language');
  const trCopy = makeNode('button', 'tr-copy');
  const trCopyNote = makeNode('span', 'tr-copy-note');
  const trCustomDetails = makeNode('details', 'tr-custom-details');
  const trEndpoint = makeNode('input', 'tr-endpoint');
  const trModel = makeNode('input', 'tr-model');
  const trKey = makeNode('input', 'tr-key');
  const trKeyState = makeNode('span', 'tr-key-state');
  trCustomDetails.append(trEndpoint, trModel, trKey, trKeyState);
  const trViewTranslated = makeNode('input', 'tr-view-translated');
  const trViewOriginal = makeNode('input', 'tr-view-original');
  const trRuntime = makeNode('p', 'tr-runtime');
  const trErrors = makeNode('p', 'tr-errors');
  trErrors.hidden = true;
  const trSaveRow = makeNode('div', 'tr-save-row');
  const trDraft = makeNode('p', 'tr-draft');
  const trCheck = makeNode('button', 'tr-check');
  const trSave = makeNode('button', 'tr-save');
  trSaveRow.append(trDraft, trCheck, trSave);
  const trPreviewBox = makeNode('details', 'tr-preview-box');
  const trTest = makeNode('button', 'tr-test');
  const trPreviewOriginal = makeNode('p', 'tr-preview-original');
  const trPreviewResult = makeNode('p', 'tr-preview-result');
  trPreviewBox.append(trTest, trPreviewOriginal, trPreviewResult);
  trBody.append(trProvider, trLanguage, trCopy, trCopyNote, trCustomDetails, trViewTranslated, trViewOriginal, trRuntime);
  if (includeRestartBtn) {
    const restartBtn = makeNode('button', 'tr-restart-enable');
    restartBtn.hidden = true;
    restartBtn.textContent = '重启并启用';
    trBody.append(restartBtn);
  }
  if (errorsInsideBody) trBody.append(trErrors);
  trBody.append(trSaveRow, trPreviewBox);
  panel.append(trEnabled, trBody);
  if (!errorsInsideBody) panel.append(trErrors);

  const webPanel = makeNode('section', 'web-panel');
  const webProvider = makeNode('select', 'web-provider');
  const webKeyField = makeNode('label', 'web-key-field');
  const webKey = makeNode('input', 'web-key');
  const webKeyState = makeNode('span', 'web-key-state');
  const webState = makeNode('p', 'web-state');
  const webErrors = makeNode('p', 'web-errors');
  webErrors.hidden = true;
  const webDraft = makeNode('p', 'web-draft');
  const webTest = makeNode('button', 'web-test');
  const webSave = makeNode('button', 'web-save');
  webKeyField.append(webKey, webKeyState);
  webPanel.append(webProvider, webKeyField, webState, webErrors, webDraft, webTest, webSave);

  const document = {
    getElementById: id => nodes.get(id) || null,
    createElement: tag => makeNode(tag),
    createTextNode: text => ({ textContent: text }),
  };
  const runtime = {};
  new Function('document', 'globalThis', growthSource)(document, runtime);
  return { nodes, mountGrowth: runtime.KimiWorkGrowth.mountGrowth };
}

test('growth.js collapses translation when disabled, omits apiKeyEnv, auto-saves toggles, and guards incomplete custom config', async () => {
  const env = growthEnv({ errorsInsideBody: true });
  let state = {
    revision: 'rev-1',
    models: [],
    translation: {
      revision: 'tr-1',
      configured: { enabled: false, provider: 'bing', targetLanguage: 'zh', view: 'translated', customEndpoint: '', customModel: '', hasApiKey: false },
      environment: { keySet: false },
      runtime: { state: 'inactive', reason: 'translation_disabled' },
    },
    webTools: { provider: 'official', keySet: false },
  };
  const calls = [];
  const render = env.mountGrowth({
    request: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/translation') {
        const nextRev = 'rev-' + (calls.length + 1);
        const nextTrRev = 'tr-' + (calls.length + 1);
        state = {
          ...state,
          revision: nextRev,
          translation: {
            ...state.translation,
            revision: nextTrRev,
            configured: {
              enabled: options.body.enabled,
              provider: options.body.provider,
              targetLanguage: options.body.targetLanguage,
              view: options.body.view,
              customEndpoint: options.body.customEndpoint,
              customModel: options.body.customModel,
              hasApiKey: Boolean(options.body.apiKey || state.translation.configured.hasApiKey),
            },
          },
        };
        return { ok: true, state };
      }
      if (path === '/api/translation/preview') {
        return { preview: { original: 'Hello', translated: '你好' } };
      }
      return { ok: true };
    },
    getState: () => state,
    refresh: async () => {},
    notify() {},
    runAction() {},
  });

  render(state);
  assert.equal(env.nodes.get('tr-body').hidden, true, 'disabled translation must collapse #tr-body');
  assert.equal(env.nodes.get('tr-errors').parentNode, env.nodes.get('translation-panel'), '#tr-errors should stay visible outside hidden #tr-body');
  assert.equal(env.nodes.get('tr-key-state').textContent, '尚未填写 API Key');

  env.nodes.get('tr-enabled').checked = true;
  await env.nodes.get('tr-enabled').dispatch('change');
  await flush();
  assert.equal(env.nodes.get('tr-body').hidden, false);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, '/api/translation');
  assert.equal('apiKeyEnv' in calls[0].options.body, false, 'POST /api/translation must omit apiKeyEnv');
  assert.equal(calls[0].options.body.enabled, true);
  assert.equal(calls[0].options.body.revision, 'rev-1');
  assert.equal(calls[0].options.body.translationRevision, 'tr-1');
  assert.equal(env.nodes.get('tr-custom-details').hidden, true);
  assert.equal(env.nodes.get('tr-custom-details').open, false);
  assert.equal(env.nodes.get('tr-save').hidden, true);

  env.nodes.get('tr-provider').value = 'google';
  await env.nodes.get('tr-provider').dispatch('change');
  await flush();
  assert.equal(calls.length, 2);
  assert.equal(calls[1].options.body.provider, 'google');
  assert.equal(calls[1].options.body.revision, 'rev-2');
  assert.equal(calls[1].options.body.translationRevision, 'tr-2');

  env.nodes.get('tr-view-original').checked = true;
  env.nodes.get('tr-view-translated').checked = false;
  await env.nodes.get('tr-view-original').dispatch('change');
  await flush();
  assert.equal(calls.length, 3);
  assert.equal(calls[2].options.body.view, 'original');

  env.nodes.get('tr-provider').value = 'custom';
  await env.nodes.get('tr-provider').dispatch('change');
  await flush();
  assert.equal(calls.length, 3, 'incomplete custom provider must not send an empty POST');
  assert.equal(env.nodes.get('tr-custom-details').hidden, false);
  assert.equal(env.nodes.get('tr-custom-details').open, true);
  assert.equal(env.nodes.get('tr-save').hidden, false);

  env.nodes.get('tr-endpoint').value = 'https://api.example.com/v1/chat/completions';
  env.nodes.get('tr-model').value = 'gpt-4o-mini';
  env.nodes.get('tr-key').value = 'sk-secret-test';
  await env.nodes.get('tr-save').dispatch('click');
  await flush();
  assert.equal(calls.length, 4);
  assert.equal(calls[3].options.body.apiKey, 'sk-secret-test');
  assert.equal('apiKeyEnv' in calls[3].options.body, false);
  assert.equal(env.nodes.get('tr-key').value, '', 'apiKey input must clear after save');
  assert.equal(env.nodes.get('tr-key-state').textContent, '已保存 API Key，留空则沿用现有密钥');

  env.nodes.get('tr-preview-box').open = false;
  await env.nodes.get('tr-test').dispatch('click');
  await flush();
  assert.equal(env.nodes.get('tr-preview-box').open, true, 'clicking #tr-test must expand #tr-preview-box');
});

test('growth.js serializes rapid translation saves, rolls back on failure, and supports dynamic no_managed_child restart button', async () => {
  const env = growthEnv({ includeRestartBtn: false });
  let state = {
    revision: 'rev-1',
    models: [],
    translation: {
      revision: 'tr-1',
      configured: { enabled: false, provider: 'bing', targetLanguage: 'zh', view: 'translated', customEndpoint: '', customModel: '' },
      environment: { keySet: false },
      runtime: { state: 'inactive', reason: 'no_managed_child' },
    },
    webTools: { provider: 'official', keySet: false },
  };
  const pending = [];
  const actionCalls = [];
  let refreshCount = 0;

  const render = env.mountGrowth({
    request: (path, options) => {
      if (path === '/api/translation') {
        return new Promise((resolve, reject) => {
          pending.push({ body: options.body, resolve, reject });
        });
      }
      if (path === '/api/actions') {
        actionCalls.push(options.body);
        return new Promise(resolve => {
          pending.push({ path, body: options.body, resolve });
        });
      }
      return Promise.resolve({ ok: true });
    },
    getState: () => state,
    refresh: async () => { refreshCount++; },
    notify() {},
    runAction() {},
  });

  render(state);
  assert.match(env.nodes.get('tr-runtime').textContent, /重启一次 Kimi 即可启用页面翻译。/);
  assert.doesNotMatch(env.nodes.get('tr-runtime').textContent, /受管启动/);

  // 并发连续点击：先开开关，再在第一个请求返回前切到 google，必须串行单飞且第二个拿到更新后的 revision。
  env.nodes.get('tr-enabled').checked = true;
  void env.nodes.get('tr-enabled').dispatch('change');
  assert.equal(pending.length, 1);
  assert.equal(pending[0].body.revision, 'rev-1');

  env.nodes.get('tr-provider').value = 'google';
  void env.nodes.get('tr-provider').dispatch('change');
  assert.equal(pending.length, 1, 'second change must wait in single-flight queue');

  state = {
    ...state,
    revision: 'rev-2',
    translation: {
      ...state.translation,
      revision: 'tr-2',
      configured: { ...state.translation.configured, enabled: true },
    },
  };
  pending[0].resolve({ ok: true, state });
  await flush();
  await flush();
  assert.equal(pending.length, 2, 'queued save must fire after first completes');
  assert.equal(pending[1].body.provider, 'google');
  assert.equal(pending[1].body.revision, 'rev-2');
  assert.equal(pending[1].body.translationRevision, 'tr-2');

  state = {
    ...state,
    revision: 'rev-3',
    translation: {
      ...state.translation,
      revision: 'tr-3',
      configured: { ...state.translation.configured, enabled: true, provider: 'google' },
    },
  };
  pending[1].resolve({ ok: true, state });
  await flush();
  await flush();

  // 动态创建的 #tr-restart-enable 按钮应在开启且 no_managed_child 时显示
  const restartBtn = env.nodes.get('tr-restart-enable');
  assert.ok(restartBtn, '#tr-restart-enable must be dynamically created when absent');
  assert.equal(restartBtn.hidden, false);
  assert.equal(restartBtn.textContent, '重启并启用');

  const clickPromise = restartBtn.dispatch('click');
  await flush();
  assert.equal(restartBtn.disabled, true);
  assert.equal(restartBtn.textContent, '正在重启 Kimi…');
  assert.deepEqual(actionCalls, [{ action: 'restart-kimi' }]);
  const beforeRefresh = refreshCount;
  pending[2].resolve({ ok: true });
  await clickPromise;
  await flush();
  assert.equal(restartBtn.disabled, false);
  assert.equal(restartBtn.textContent, '重启并启用');
  assert.ok(refreshCount > beforeRefresh, 'restart must trigger host refresh callback');

  // 保存失败必须回滚控件与 #tr-body 到服务端快照，并展示 #tr-errors
  env.nodes.get('tr-enabled').checked = false;
  void env.nodes.get('tr-enabled').dispatch('change');
  pending[3].reject(new Error('save_boom'));
  await flush();
  await flush();
  assert.equal(env.nodes.get('tr-enabled').checked, true, 'failed toggle must roll back checkbox to server state');
  assert.equal(env.nodes.get('tr-body').hidden, false, 'failed toggle must restore #tr-body visibility');
  assert.equal(env.nodes.get('tr-errors').hidden, false);
  assert.match(env.nodes.get('tr-errors').textContent, /save_boom/);

  // 翻译尚未保存为 enabled: true 时点击 #tr-restart-enable，必须先保存再触发 restart-kimi
  state = {
    ...state,
    revision: 'rev-4',
    translation: {
      ...state.translation,
      revision: 'tr-4',
      configured: { ...state.translation.configured, enabled: false, provider: 'bing' },
    },
  };
  env.nodes.get('tr-enabled').checked = true;
  const restartWithSave = restartBtn.dispatch('click');
  await flush();
  assert.equal(pending.length, 5, 'must save enabled: true before restarting Kimi');
  assert.equal(pending[4].body.enabled, true);
  assert.equal(actionCalls.length, 1, 'must not call restart-kimi before save finishes');
  state = {
    ...state,
    revision: 'rev-5',
    translation: {
      ...state.translation,
      revision: 'tr-5',
      configured: { ...state.translation.configured, enabled: true, provider: 'bing' },
    },
  };
  pending[4].resolve({ ok: true, state });
  await flush();
  await flush();
  assert.equal(actionCalls.length, 2);
  assert.deepEqual(actionCalls[1], { action: 'restart-kimi' });
  pending[5].resolve({ ok: true });
  await restartWithSave;
});

test('growth.js web-tools auto-saves official and keyed tavily, skips keyless tavily, and rolls back on failure', async () => {
  const env = growthEnv();
  const state = {
    revision: 'rev-1',
    models: [],
    translation: {
      revision: 'tr-1',
      configured: { enabled: false, provider: 'bing', targetLanguage: 'zh', view: 'translated', customEndpoint: '', customModel: '' },
      environment: { keySet: false },
      runtime: { state: 'inactive', reason: 'translation_disabled' },
    },
    webTools: { provider: 'official', keySet: false },
  };
  const calls = [];
  let failNext = false;
  const render = env.mountGrowth({
    request: async (path, options) => {
      calls.push({ path, options });
      if (failNext) {
        failNext = false;
        throw new Error('web_save_failed');
      }
      if (path === '/api/web-tools' && options.body.action === 'save') {
        return {
          ok: true,
          state: {
            provider: options.body.provider,
            keySet: Boolean(options.body.apiKey || state.webTools.keySet),
          },
        };
      }
      return { ok: true };
    },
    getState: () => state,
    refresh: async () => {},
    notify() {},
    runAction() {},
  });

  render(state);
  assert.equal(env.nodes.get('web-key-field').hidden, true);
  assert.equal(env.nodes.get('web-save').hidden, true);
  assert.equal(env.nodes.get('web-draft').hidden, true);

  // 未保存 Tavily Key 时切到 tavily：展开输入框与保存按钮，但不发无 Key 请求
  env.nodes.get('web-provider').value = 'tavily';
  await env.nodes.get('web-provider').dispatch('change');
  await flush();
  assert.equal(calls.length, 0, 'switching to tavily without saved key must not send save');
  assert.equal(env.nodes.get('web-key-field').hidden, false);
  assert.equal(env.nodes.get('web-save').hidden, false);
  assert.match(env.nodes.get('web-draft').textContent, /保存服务方式/);

  // 填写 Key 并保存
  env.nodes.get('web-key').value = 'tvly-key';
  await env.nodes.get('web-save').dispatch('click');
  await flush();
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].options.body, { action: 'save', provider: 'tavily', apiKey: 'tvly-key' });
  assert.equal(state.webTools.keySet, true);

  // 切回 official：立即自动保存并隐藏 Key 与保存按钮
  env.nodes.get('web-provider').value = 'official';
  await env.nodes.get('web-provider').dispatch('change');
  await flush();
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[1].options.body, { action: 'save', provider: 'official' });
  assert.equal(env.nodes.get('web-key-field').hidden, true);
  assert.equal(env.nodes.get('web-save').hidden, true);

  // 已有 keySet 且 #web-key 为空时切到 tavily：立即自动保存
  env.nodes.get('web-provider').value = 'tavily';
  await env.nodes.get('web-provider').dispatch('change');
  await flush();
  assert.equal(calls.length, 3);
  assert.deepEqual(calls[2].options.body, { action: 'save', provider: 'tavily' });

  // 切回 official 失败：回退下拉选择到 tavily 并显示错误
  failNext = true;
  env.nodes.get('web-provider').value = 'official';
  await env.nodes.get('web-provider').dispatch('change');
  await flush();
  assert.equal(env.nodes.get('web-provider').value, 'tavily', 'failed save must roll back #web-provider');
  assert.equal(env.nodes.get('web-errors').hidden, false);
  assert.match(env.nodes.get('web-errors').textContent, /web_save_failed/);
});

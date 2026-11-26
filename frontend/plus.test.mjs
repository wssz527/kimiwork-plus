import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

/* plus.js 是一个 IIFE，挂载时读 DOM。这里的测试用一个极小的替身跑真实的源文件，
   只验证「怎么显示」这件事：哪些值必须原样保留、哪些必须显示为未提供、哪些
   按钮在什么状态下才出现。真实 DOM 与配色不在测试范围内。 */

const source = readFileSync(new URL('./plus.js', import.meta.url), 'utf8');

/* 最小 DOM：只实现 plus.js 用到的那几个成员，并记录渲染出的文本。 */
function makeDom(extraRoots = []) {
  class El {
    constructor(tag) {
      this.tagName = tag.toUpperCase();
      this.children = [];
      this.dataset = {};
      this.attributes = {};
      this.style = {};
      this.listeners = {};
      this.textContent = '';
      this.hidden = false;
      this.disabled = false;
      this.value = '';
      this.type = '';
      this.className = '';
      this.id = '';
    }
    append(...nodes) { for (const node of nodes) if (node) this.children.push(node); }
    replaceChildren(...nodes) { this.children = nodes.filter(Boolean); }
    addEventListener(name, handler) { (this.listeners[name] ||= []).push(handler); }
    setAttribute(name, value) { this.attributes[name] = String(value); }
    getAttribute(name) { return this.attributes[name]; }
    insertBefore(node, ref) {
      const index = ref ? this.children.indexOf(ref) : -1;
      if (index < 0) this.children.push(node);
      else this.children.splice(index, 0, node);
      return node;
    }
    querySelector() { return null; }
    querySelectorAll() { return []; }
    closest() { return null; }
    get firstChild() { return this.children[0] || null; }
    get parentNode() { return this.parent || null; }
    focus() {}
  }
  const registry = new Map();
  const mount = (id) => {
    if (!registry.has(id)) {
      const element = new El('div');
      element.id = id;
      registry.set(id, element);
    }
    return registry.get(id);
  };
  const roots = {
    'view-usage': mount('view-usage'),
    'view-overview': mount('view-overview'),
    'view-work': mount('view-work'),
    'view-maintenance': mount('view-maintenance'),
    'usage-pane-quotas': mount('usage-pane-quotas'),
    'usage-pane-budget': mount('usage-pane-budget'),
  };
  for (const id of extraRoots) roots[id] = mount(id);
  roots['view-usage'].append(roots['usage-pane-quotas'], roots['usage-pane-budget']);
  const tools = new El('div');
  tools.className = 'masthead__tools';

  /* 动态创建的节点（如 #plus-code-body）不在 registry 里：挂载后才存在于树上，
     所以查找必须走树，和真实 document.getElementById 的行为一致。 */
  function findByID(node, id) {
    if (!node) return null;
    if (node.id === id) return node;
    for (const child of node.children || []) {
      const found = findByID(child, id);
      if (found) return found;
    }
    return null;
  }
  function idOf(id) {
    if (registry.has(id)) return registry.get(id);
    for (const root of Object.values(roots)) {
      const found = findByID(root, id);
      if (found) return found;
    }
    return findByID(tools, id);
  }

  return {
    document: {
      createElement: (tag) => new El(tag),
      createElementNS: () => new El('svg'),
      getElementById: idOf,
      querySelector: (selector) => selector === '.masthead__tools' ? tools : null,
      body: new El('body'),
    },
    registry, roots, tools,
  };
}

function flatten(node, output = []) {
  if (!node) return output;
  if (node.textContent) output.push(node.textContent);
  if (Array.isArray(node.children)) for (const child of node.children) flatten(child, output);
  return output;
}

function textOf(node) { return flatten(node).join(' | '); }

function findButton(node, label) {
  if (!node) return null;
  if (node.tagName === 'BUTTON' && (node.textContent === label || textOf(node).includes(label))) return node;
  for (const child of node.children || []) {
    const found = findButton(child, label);
    if (found) return found;
  }
  return null;
}

function allButtons(node, output = []) {
  if (!node) return output;
  if (node.tagName === 'BUTTON') output.push(node);
  for (const child of node.children || []) allButtons(child, output);
  return output;
}

function makePlus({ data = {}, routes = {}, onAccounts, extraRoots = [] } = {}) {
  const dom = makeDom(extraRoots);
  const calls = [];
  const notices = [];
  const previous = {};
  for (const [name, value] of Object.entries({
    document: dom.document, Intl, Date, JSON, Math, Number, String, Array, Object, Set, Map,
    Promise,
  })) {
    previous[name] = globalThis[name];
    globalThis[name] = value;
  }
  const request = async (path, options = {}) => {
    calls.push({ path, method: options.method, body: options.body });
    const handler = routes[path];
    if (!handler) return {};
    return handler(options.body || null);
  };
  const notify = (message, tone) => notices.push({ message, tone });
  const source_text = source.replace('globalThis.KimiWorkPlus = Object.freeze(', 'globalThis.KimiWorkPlus = Object.freeze(');
  const load = () => new Function(source_text)();
  globalThis.KimiWorkPlus = undefined;
  load();
  const api = globalThis.KimiWorkPlus;
  let state = data;
  const panel = api.mount({ request, notify, getState: () => state, setState: (next) => { state = next; }, onAccounts });
  const restore = () => { for (const [name, value] of Object.entries(previous)) globalThis[name] = value; };
  /* 挂载后新建的节点不在 registry 里：按真实 getElementById 的方式在树上找。 */
  const byId = (id) => dom.document.getElementById(id);
  return { api, panel, dom, byId, calls, notices, restore, getState: () => state, setState: (next) => { state = next; } };
}

test('quota and budget sections mount inside their scrollable usage tabs', () => {
  const env = makePlus();
  try {
    assert.ok(env.dom.roots['usage-pane-quotas'].children.includes(env.byId('plus-usage-panel')));
    assert.ok(env.dom.roots['usage-pane-budget'].children.includes(env.byId('plus-budget-panel')));
    assert.equal(env.dom.roots['view-usage'].children.length, 2);
    assert.equal(env.dom.roots['view-maintenance'].firstChild.id, 'plus-accounts-panel');
  } finally { env.restore(); }
});

test('Magpie quota is opt in and is not refreshed by default', async () => {
  const env = makePlus({ routes: {
    '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], notes: [], sources: [] } }),
    '/api/quota/magpie': () => ({ state: { configured: false, status: 'not_read', notes: [], snapshot: null } }),
    '/api/budget': () => ({ budget: { configured: null, status: {} } }),
  } });
  try {
    await env.panel.refresh();
    const card = env.byId('plus-magpie-card');
    assert.ok(card);
    assert.equal(card.hidden, true);
    await env.panel.refresh({ live: true });
    assert.equal(env.calls.filter(call => call.path === '/api/quota/magpie' && call.body?.action === 'refresh').length, 0);
    const add = findButton(env.byId('plus-usage-panel'), '添加其他网关额度');
    assert.ok(add);
    await add.listeners.click[0]();
    assert.equal(card.hidden, false);
  } finally { env.restore(); }
});

test('an explicitly configured Magpie remains visible and refreshes', async () => {
  const env = makePlus({ routes: {
    '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], notes: [], sources: [] } }),
    '/api/quota/magpie': body => ({ state: { configured: true, status: body?.action === 'refresh' ? 'ok' : 'not_read', notes: [], snapshot: null } }),
    '/api/budget': () => ({ budget: { configured: null, status: {} } }),
  } });
  try {
    await env.panel.refresh();
    assert.equal(env.byId('plus-magpie-card').hidden, false);
    await env.panel.refresh({ live: true });
    assert.equal(env.calls.filter(call => call.path === '/api/quota/magpie' && call.body?.action === 'refresh').length, 1);
  } finally { env.restore(); }
});

test('a failed Kimi Code read shows no numbers at all', async () => {
  const env = makePlus({
    data: { workCapabilities: null },
    routes: {
      '/api/quota/code': () => ({ state: {
        status: 'error', errorReason: 'auth_rejected', windows: [], wallet: null,
        notes: [], sources: [{ id: 'manual', label: '手动填写的 Key', availability: 'available', selected: true }],
        selectedSource: 'manual',
      } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, effective: null, revision: 1, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-code-body'));
    assert.match(text, /读取失败/);
    assert.match(text, /Key 被拒绝或已过期/);
    // 失败不能变成 0：既没有百分比也没有金额。
    assert.doesNotMatch(text, /\d+%/);
    assert.doesNotMatch(text, /余额/);
  } finally { env.restore(); }
});

test('the Kimi Code wallet already carries cents from the backend', async () => {
  const env = makePlus({ routes: {
    '/api/quota/code': () => ({ state: { status: 'ok', windows: [], notes: [], sources: [], wallet: { balanceCents: 350, totalCents: 500, currency: 'USD' } } }),
    '/api/quota/magpie': () => ({ state: { status: 'not_read', snapshot: null } }),
    '/api/budget': () => ({ budget: { configured: null, status: {} } }),
  } });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-code-body'));
    assert.match(text, /3\.50/);
    assert.match(text, /5\.00/);
  } finally { env.restore(); }
});

test('the local Kimi Code sign-in is never adopted silently', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: {
        status: 'not_read', windows: [], wallet: null, notes: [],
        selectedSource: 'manual',
        sources: [
          { id: 'manual', label: '手动填写的 Key', availability: 'available' },
          { id: 'local', label: '本机 Kimi Code 登录', availability: 'unadopted', accountMasked: 'acct-1234' },
        ],
      } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const body = env.byId('plus-code-body');
    const adopt = findButton(body, '使用本机登录');
    assert.notEqual(adopt, null, 'the local source must offer an explicit adopt action');
    // 刷新期间没有任何 select 请求被发出。
    assert.equal(env.calls.some(call => call.body && call.body.action === 'select'), false);
    adopt.listeners.click[0]();
    await new Promise(resolve => setTimeout(resolve, 0));
  } finally { env.restore(); }
});

test('a window carrying only a display string keeps the text and says so', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [], selectedSource: null } }),
      '/api/quota/magpie': () => ({ state: {
        status: 'ok', notes: [], baseUrl: 'http://127.0.0.1:3425',
        snapshot: {
          fetchedAt: '2026-11-16T10:00:00Z',
          entries: [{
            provider: 'codex', name: 'Codex', kind: 'subscription',
            windows: [{ name: '每周', basis: 'text', display: '500 / 500' }],
          }],
        },
      } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-magpie-body'));
    assert.match(text, /500 \/ 500/);
    assert.match(text, /只给了文本/);
    assert.doesNotMatch(text, /\b0%\b/);
  } finally { env.restore(); }
});

test('a magnitude window renders amount and limit rather than a percentage', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [], selectedSource: null } }),
      '/api/quota/magpie': () => ({ state: {
        status: 'ok', notes: [], baseUrl: 'http://127.0.0.1:3425',
        snapshot: {
          fetchedAt: '2026-11-16T10:00:00Z',
          entries: [{
            provider: 'codearts', name: 'CodeArts', kind: 'subscription',
            windows: [{ name: '总积分', basis: 'amount', amount: 5500, limit: 5600, unit: 'credits' }],
          }],
        },
      } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-magpie-body'));
    assert.match(text, /5,500/);
    assert.match(text, /5,600/);
    assert.match(text, /credits/);
  } finally { env.restore(); }
});

test('the budget card never calls the estimate a provider hard limit', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [], selectedSource: null } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({
        budget: {
          configured: { enabled: true, currency: 'CNY', timeZone: 'Asia/Shanghai', daily: { limit: 20, action: 'block' } },
          revision: 3,
          status: {
            enforcement: 'estimate_from_ledger', currency: 'CNY', enabled: true,
            spend: {
              daily: { spent: 12.5, pricedRequests: 4, unpricedRequests: 2, inflightRequests: 1, partial: true,
                limit: 20, remaining: 7.5, reached: false, action: 'block', resetsAt: '2026-11-17T00:00:00Z' },
              monthly: null,
            },
            pricedModels: ['model-a'], unpricedModels: ['model-b'],
            warning: 'unpriced_models_not_limited',
          },
        },
      }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-budget-body'));
    assert.match(text, /不是供应商的硬上限/);
    assert.match(text, /未定价（未计入金额）/);
    assert.match(text, /在途（可能超出）/);
    assert.match(text, /model-b/);
    assert.match(text, /已记录费用仍参与预算判断/);
    assert.doesNotMatch(text, /不受预算限制/);
    assert.doesNotMatch(text, /硬上限[^。]*保证/);
  } finally { env.restore(); }
});

test('the budget form keeps one currency row and pairs each limit with its own action', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [], selectedSource: null } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({
        budget: {
          configured: { enabled: true, currency: 'CNY', timeZone: 'Asia/Shanghai', daily: { limit: 20, action: 'block' }, monthly: { limit: 200, action: 'remind' } },
          revision: 4,
          status: { enforcement: 'estimate_from_ledger', currency: 'CNY', enabled: true, spend: { daily: null, monthly: null } },
        },
      }),
    },
  });
  const find = (node, pred) => {
    if (!node) return null;
    if (pred(node)) return node;
    for (const child of node.children || []) {
      const hit = find(child, pred);
      if (hit) return hit;
    }
    return null;
  };
  try {
    await env.panel.refresh();
    const body = env.byId('plus-budget-body');
    const grid = find(body, node => node.className === 'field-row');
    assert.ok(grid, 'the budget form needs its two-column grid');
    assert.equal(grid.children.length, 5, 'currency · daily limit · daily action · monthly limit · monthly action');
    const labelOf = field => (field.children || []).find(child => child.className === 'field__label')?.textContent;
    assert.equal(grid.children[0].className, 'field field--wide', 'currency spans the first row alone');
    assert.equal(labelOf(grid.children[0]), '货币');
    assert.equal(labelOf(grid.children[1]), '每日上限（留空不限制）');
    assert.equal(labelOf(grid.children[2]), '每日达上限时');
    assert.equal(labelOf(grid.children[3]), '每月上限（留空不限制）');
    assert.equal(labelOf(grid.children[4]), '每月达上限时');
    assert.equal(new Set([labelOf(grid.children[2]), labelOf(grid.children[4])]).size, 2,
      'the two action fields must not share the same label');
  } finally { env.restore(); }
});

test('a needsLogin flag belongs to its own account row only', async () => {
  const env = makePlus({
    data: {
      accounts: {
        state: 'ready', currentId: 'acct-a', adding: false, restoreAccountId: '', activation: {},
        accounts: [
          { id: 'acct-a', name: '工作账号', capturedAt: 1, lastUsedAt: 2 },
          { id: 'acct-b', name: '', capturedAt: 3, needsLogin: true },
        ],
      },
      workCapabilities: null,
    },
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [] } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    env.panel.render(env.getState());
    const body = env.byId('plus-accounts-body');
    const rows = [];
    (function collect(node) {
      if (!node) return;
      if (node.className === 'plus-account') rows.push(textOf(node));
      for (const child of node.children || []) collect(child);
    }(body));
    assert.equal(rows.length, 2);
    assert.match(rows[0], /工作账号/);
    assert.doesNotMatch(rows[0], /需要重新登录/);
    assert.match(rows[1], /需要重新登录/);
    // 没有名字的账号用尾号默认名，并说明它是派生的。
    assert.match(rows[1], /默认名/);
  } finally { env.restore(); }
});

test('switching accounts explains the restart once and does not double-confirm', async () => {
  const env = makePlus({
    data: {
      accounts: {
        state: 'ready', currentId: 'acct-a', adding: false, accounts: [
          { id: 'acct-a', name: 'A' }, { id: 'acct-b', name: 'B' },
        ],
      },
      workCapabilities: null,
    },
    routes: {
      /* 立刻返回一个已结束的 operation：测试关心的是提示与提交内容，不是等待。 */
      '/api/actions': () => ({ operation: { id: 'op-1', state: 'done', action: 'account-switch' } }),
      '/api/state': () => ({
        operation: { id: 'op-1', state: 'done', action: 'account-switch' },
        accounts: { state: 'ready', currentId: 'acct-b', adding: false, accounts: [
          { id: 'acct-a', name: 'A' }, { id: 'acct-b', name: 'B' },
        ] },
        workCapabilities: null,
      }),
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [] } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    env.panel.render(env.getState());
    const bar = env.byId('plus-account-bar');
    const select = (function find(node) {
      if (!node) return null;
      if (node.tagName === 'SELECT') return node;
      for (const child of node.children || []) { const found = find(child); if (found) return found; }
      return null;
    }(bar));
    assert.notEqual(select, null);
    select.value = 'acct-b';
    select.listeners.change[0]();
    /* followAccountOperation 会在第一次 900ms 等待后读到终态。 */
    await new Promise(resolve => setTimeout(resolve, 1200));
    const switchCall = env.calls.find(call => call.body && call.body.action === 'account-switch');
    assert.notEqual(switchCall, undefined);
    assert.equal(switchCall.body.accountId, 'acct-b');
    // 兼容回退只提示一次，不额外确认。
    const restartNotes = env.notices.filter(notice => /重启/.test(notice.message));
    assert.equal(restartNotes.length, 1);
    assert.equal(env.notices.filter(notice => notice.tone === 'error').length, 0);
  } finally { env.restore(); }
});

function controlsOf(root, tag, output = []) {
  if (root.tagName === tag) output.push(root);
  for (const child of root.children || []) controlsOf(child, tag, output);
  return output;
}

test('activation statuses use the backend vocabulary without raw state codes', () => {
  const env = makePlus({ data: { accounts: {
    state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: 'A' }],
    activation: { state: 'restarted', restarted: true, verified: true },
  } } });
  try {
    env.panel.render(env.getState());
    const text = textOf(env.byId('plus-overview-account'));
    assert.match(text, /已应用/);
    assert.doesNotMatch(text, /restarted/);
  } finally { env.restore(); }
});

test('account activation distinguishes identity success from model readiness', () => {
  for (const modelsState of ['ready', 'pending', 'failed']) {
    const env = makePlus({ data: { accounts: {
      state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: 'A' }],
      activation: { state: 'hot', hot: true, verified: true, modelsState },
    } } });
    try {
      env.panel.render(env.getState());
      const text = textOf(env.byId('plus-overview-account'));
      assert.match(text, /未重启，热切换/);
      if (modelsState === 'ready') assert.match(text, /第三方模型已就绪/);
      else assert.match(text, /账号已切换，第三方模型尚未就绪，请在模型页同步/);
      assert.doesNotMatch(text, /应用失败/);
    } finally { env.restore(); }
  }
});

test('budget zero limits are refused locally instead of submitted to a positive-only API', async () => {
  const env = makePlus({ routes: {
    '/api/budget': () => ({ budget: { configured: null, revision: 'r1', status: {} } }),
  } });
  try {
    await env.panel.refresh();
    const input = controlsOf(env.byId('plus-budget-body'), 'INPUT').find(input => input.type === 'number');
    input.value = '0';
    input.listeners.input[0]();
    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    assert.equal(env.calls.some(call => call.path === '/api/budget/save'), false);
    assert.match(textOf(env.byId('plus-budget-body')), /大于 0/);
  } finally { env.restore(); }
});

test('budget saves reload the revisioned view and preserve the saved form', async () => {
  let revision = 3;
  let configured = { enabled: true, currency: 'CNY', timeZone: 'Asia/Shanghai', serialize: true, daily: { limit: 20, action: 'block' } };
  const savedPayloads = [];
  const env = makePlus({ routes: {
    '/api/budget': () => ({ budget: { configured, revision, status: { enabled: Boolean(configured), currency: 'CNY' } } }),
    '/api/budget/save': body => {
      assert.equal(body.revision, revision);
      savedPayloads.push(body.budget);
      configured = body.budget;
      revision++;
      return { ok: true, state: { budget: { enabled: Boolean(configured), currency: 'CNY' }, marker: revision } };
    },
  } });
  try {
    await env.panel.refresh();
    const formText = textOf(env.byId('plus-budget-body'));
    assert.doesNotMatch(formText, /时区/);
    assert.doesNotMatch(formText, /串行发送请求/);
    const initialInputs = controlsOf(env.byId('plus-budget-body'), 'INPUT');
    assert.equal(initialInputs.filter(input => input.type === 'checkbox').length, 1);
    assert.equal(initialInputs.filter(input => input.type === 'text').length, 0);

    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    assert.equal(env.getState().marker, 4);
    assert.equal(savedPayloads[0].timeZone, 'Asia/Shanghai');
    assert.equal(savedPayloads[0].serialize, true);
    const inputs = controlsOf(env.byId('plus-budget-body'), 'INPUT');
    assert.equal(inputs[0].checked, true);
    assert.equal(inputs.find(input => input.type === 'number').value, '20');
    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    await findButton(env.byId('plus-budget-body'), '清除预算设置').listeners.click[0]();
    assert.equal(revision, 6);
    assert.equal(controlsOf(env.byId('plus-budget-body'), 'INPUT')[0].checked, false);
    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    assert.equal(Object.hasOwn(savedPayloads.at(-1), 'timeZone'), false);
    assert.equal(savedPayloads.at(-1).serialize, false);
    assert.equal(env.notices.some(notice => notice.tone === 'error'), false);
  } finally { env.restore(); }
});

test('an interrupted account restore stays actionable and disables switching', () => {
  const env = makePlus({ data: { accounts: {
    state: 'ready', currentId: '', adding: false, restoreAccountId: 'acct-a',
    accounts: [{ id: 'acct-a', name: 'A' }, { id: 'acct-b', name: 'B' }],
  } } });
  try {
    env.panel.render(env.getState());
    const body = env.byId('plus-accounts-body');
    assert.match(textOf(body), /待恢复原账号/);
    assert.ok(findButton(body, '恢复原账号'));
    assert.equal(findButton(body, '添加账号'), null);
    assert.equal(env.byId('plus-account-select').disabled, true);
  } finally { env.restore(); }
});

test('refresh accounts really reads the latest account index', async () => {
  const env = makePlus({ data: { accounts: { state: 'ready', currentId: '', accounts: [] } }, routes: {
    '/api/accounts': () => ({ accounts: { state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: '刚保存的账号' }] } }),
  } });
  try {
    env.panel.render(env.getState());
    const body = env.byId('plus-accounts-body');
    assert.ok(findButton(body, '保存当前账号'));
    assert.doesNotMatch(textOf(body), /不会复制官方/);
    await findButton(body, '刷新账号列表').listeners.click[0]();
    assert.match(textOf(env.byId('plus-accounts-body')), /刚保存的账号/);
  } finally { env.restore(); }
});

test('an unsupported tower is reported as such, not as missing', async () => {
  const env = makePlus({
    data: {
      workCapabilities: {
        state: 'inspected', kernelVersion: '3.2.27', kernelCommit: 'ecb904a69',
        subagents: { forceKeySupported: false },
        swarm: { state: 'user-controlled', reason: 'work_swarm_requires_conversation', kernelAvailable: true },
        tower: { state: 'unsupported', reason: 'work_tower_desktop_unavailable', kernelAvailable: true },
      },
      accounts: { state: 'ready', currentId: '', accounts: [] },
    },
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [] } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    env.panel.render(env.getState());
    const text = textOf(env.byId('plus-capabilities-body'));
    assert.match(text, /当前版本不支持/);
    assert.match(text, /没有提供 Tower 入口/);
    assert.equal(findButton(env.byId('plus-capabilities-body'), '读取当前会话'), null);
    assert.equal(env.byId('plus-swarm-panel'), null);
    assert.equal(env.api.loadSwarm, undefined);
  } finally { env.restore(); }
});

/* 轮询每次 adoptState 后都会调用 render()。账号条、预算与能力面板都必须在这条
   路径上刷新，否则它们只会在挂载那一次显示，之后一直停在旧值。 */
test('render() refreshes the account bar, overview and capabilities', () => {
  const env = makePlus({
    data: {
      accounts: { state: 'ready', currentId: 'acct-a', adding: false, accounts: [
        { id: 'acct-a', name: '工作账号', lastUsedAt: 1700000000000 },
        { id: 'acct-b', name: '', capturedAt: 1700000000000 },
      ] },
      workCapabilities: {
        state: 'unverified', swarm: { state: 'unverified', reason: 'work_kernel_unverified' },
        tower: { state: 'unsupported', reason: 'work_tower_desktop_unavailable' }, subagents: {},
      },
    },
    routes: {
      '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], wallet: null, notes: [], sources: [] } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    // 挂载时不渲染（状态还没到）；第一次 render 后三处都要反映真实状态。
    env.panel.render(env.getState());
    assert.match(textOf(env.byId('plus-overview-account')), /工作账号/);
    assert.match(textOf(env.byId('plus-account-bar')), /账号/);
    assert.match(textOf(env.byId('plus-capabilities-body')), /未确认/);
    assert.match(textOf(env.byId('plus-capabilities-body')), /没有提供 Tower 入口/);

    // 当前账号缺失时不能空着，也不能编一个名称。
    env.setState({ ...env.getState(), accounts: { state: 'ready', currentId: '', accounts: [] }, workCapabilities: null });
    env.panel.render(env.getState());
    assert.match(textOf(env.byId('plus-overview-account')), /当前账号未记录/);
  } finally { env.restore(); }
});


test('the account bar opens account management even with no saved accounts', () => {
  let opened = 0;
  const env = makePlus({ data: { accounts: { state: 'ready', accounts: [] } }, onAccounts: () => { opened++; } });
  try {
    env.panel.render(env.getState());
    const manage = findButton(env.byId('plus-account-bar'), '账号管理');
    assert.ok(manage);
    manage.listeners.click[0]();
    assert.equal(opened, 1);
  } finally { env.restore(); }
});

test('adding an account does not require manually saving the first account', async () => {
  const env = makePlus({ data: { accounts: { state: 'ready', accounts: [] } }, routes: {
    '/api/actions': body => { assert.equal(body.action, 'account-begin-add'); return {}; },
  } });
  try {
    env.panel.render(env.getState());
    const add = findButton(env.byId('plus-accounts-body'), '添加账号');
    assert.ok(add);
    await add.listeners.click[0]();
    assert.equal(env.calls.filter(call => call.path === '/api/actions').length, 1);
  } finally { env.restore(); }
});

test('a lost Work session stays visible and can restart before continuing account add', async () => {
  const book = { state: 'ready', currentId: 'acct-a', adding: false, accounts: [{ id: 'acct-a', name: 'A' }] };
  let count = 0;
  let operation;
  const actions = [];
  const env = makePlus({ data: { accounts: book }, routes: {
    '/api/actions': body => {
      actions.push(body.action);
      operation = { id: 'op-' + (++count), action: body.action, state: count === 1 ? 'failed' : 'done' };
      if (count === 1) operation.reason = 'account_session_unavailable';
      return { operation };
    },
    '/api/state': () => ({ accounts: { ...book, adding: count === 3, restoreAccountId: count === 3 ? 'acct-a' : '' }, operation }),
  } });
  try {
    env.panel.render(env.getState());
    await findButton(env.byId('plus-accounts-body'), '添加账号').listeners.click[0]();
    const body = env.byId('plus-accounts-body');
    assert.match(textOf(body), /未连接到 Kimi 的登录窗口/);
    assert.doesNotMatch(textOf(body), /account_session_unavailable/);
    const recover = findButton(body, '正常重启 Kimi 并继续添加');
    assert.ok(recover);
    assert.match(textOf(body), /先结束正在运行的任务/);
    env.panel.render(env.getState());
    assert.match(textOf(env.byId('plus-accounts-body')), /未连接到 Kimi 的登录窗口/);
    await findButton(env.byId('plus-accounts-body'), '正常重启 Kimi 并继续添加').listeners.click[0]();
    assert.deepEqual(actions, ['account-begin-add', 'restart-kimi', 'account-begin-add']);
    assert.match(textOf(env.byId('plus-accounts-body')), /正在等你完成官方登录/);
    assert.doesNotMatch(textOf(env.byId('plus-accounts-body')), /未连接到 Kimi 的登录窗口/);
  } finally { env.restore(); }
});

test('a failed recovery restart never signs out or resubmits account add', async () => {
  const book = { state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: 'A' }] };
  let operation;
  const actions = [];
  const env = makePlus({ data: { accounts: book }, routes: {
    '/api/actions': body => {
      actions.push(body.action);
      operation = { id: String(actions.length), action: body.action, state: 'failed', reason: actions.length === 1 ? 'account_session_unavailable' : 'kimi_quit_timeout' };
      return { operation };
    },
    '/api/state': () => ({ accounts: book, operation }),
  } });
  try {
    env.panel.render(env.getState());
    await findButton(env.byId('plus-accounts-body'), '添加账号').listeners.click[0]();
    await findButton(env.byId('plus-accounts-body'), '正常重启 Kimi 并继续添加').listeners.click[0]();
    assert.deepEqual(actions, ['account-begin-add', 'restart-kimi']);
    assert.match(textOf(env.byId('plus-accounts-body')), /Kimi 尚未退出/);
    assert.equal(env.getState().accounts.currentId, 'acct-a');
  } finally { env.restore(); }
});

test('a real failure reason outranks the superseded note', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: {
        status: 'error', errorReason: 'auth_rejected', windows: [], wallet: null,
        notes: ['reading_superseded'], sources: [], selectedSource: 'manual',
      } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-code-body'));
    assert.match(text, /Key 被拒绝或已过期/);
    assert.doesNotMatch(text, /这份读数属于另一个来源或账号/);
  } finally { env.restore(); }
});

test('a missing panel key is reported in plain words, not as NO_KEY', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: { status: 'error', errorReason: 'NO_KEY', windows: [], wallet: null, notes: [], sources: [] } }),
      '/api/quota/magpie': () => ({ state: { status: 'error', errorReason: 'NO_KEY', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    for (const id of ['plus-code-body', 'plus-magpie-body']) {
      const text = textOf(env.byId(id));
      assert.doesNotMatch(text, /NO_KEY/);
      assert.match(text, /缺少面板会话密钥/);
    }
  } finally { env.restore(); }
});

test('an unadopted local source explains itself instead of printing its internal code', async () => {
  const env = makePlus({
    data: {},
    routes: {
      '/api/quota/code': () => ({ state: {
        status: 'not_read', windows: [], wallet: null, notes: [], selectedSource: 'manual',
        sources: [{ id: 'local', label: '本机 Kimi Code 登录', availability: 'unadopted',
          accountMasked: 'acct-c51b5062', detail: 'code_local_needs_adoption' }],
      } }),
      '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
      '/api/budget': () => ({ budget: { configured: null, status: {} } }),
    },
  });
  try {
    await env.panel.refresh();
    const text = textOf(env.byId('plus-code-body'));
    assert.match(text, /已检测到，尚未采用/);
    assert.match(text, /需要先采用本机 Kimi Code 登录/);
    assert.doesNotMatch(text, /code_local_needs_adoption/);
  } finally { env.restore(); }
});

test('the subagent force-key line never claims support the kernel lacks', () => {
  const cases = [
    [false, /当前内核不接受/],
    [true, /当前内核支持/],
  ];
  for (const [value, pattern] of cases) {
    const env = makePlus({
      data: { workCapabilities: { state: 'inspected', subagents: { forceKeySupported: value },
        swarm: { state: 'unverified' }, tower: { state: 'unverified' } } },
      routes: {
        '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], notes: [], sources: [] } }),
        '/api/quota/magpie': () => ({ state: { status: 'not_read', notes: [], snapshot: null } }),
        '/api/budget': () => ({ budget: { configured: null, status: {} } }),
      },
    });
    try {
      env.panel.render(env.getState());
      const text = textOf(env.byId('plus-capabilities-body'));
      assert.match(text, pattern);
      if (value === false) assert.doesNotMatch(text, /固定子代理模型：当前内核接受/);
    } finally { env.restore(); }
  }
});

test('a masthead refresh in the usage view re-reads the configured live quotas instead of the cache', async () => {
  const routes = {
    '/api/quota/code': () => ({ state: { status: 'not_read', windows: [], notes: [], sources: [] } }),
    '/api/quota/magpie': () => ({ state: { configured: true, status: 'not_read', notes: [], snapshot: null } }),
    '/api/budget': () => ({ budget: { configured: null, status: {} } }),
  };
  const env = makePlus({ routes });
  try {
    env.calls.length = 0;
    await env.panel.refresh();
    /* 默认刷新只读缓存：两个额度接口各取一次 GET，没有任何 POST。 */
    assert.equal(env.calls.filter(call => call.method === 'POST').length, 0);
    assert.equal(env.calls.filter(call => call.path === '/api/quota/code').length, 1);

    env.calls.length = 0;
    await env.panel.refresh({ live: true });
    const posts = env.calls.filter(call => call.method === 'POST');
    assert.deepEqual(posts.map(call => call.path).sort(), ['/api/quota/code', '/api/quota/magpie']);
    /* 顶栏刷新不得强制绕过节流窗口。 */
    const codeRefresh = posts.find(call => call.path === '/api/quota/code');
    assert.equal(codeRefresh.body.force, undefined);
    assert.equal(codeRefresh.body.action, 'refresh');
  } finally { env.restore(); }
});

test('account switching controls are disabled when compatibility guard blocks account writes', () => {
  const env = makePlus({
    data: {
      accounts: {
        state: 'ready',
        currentId: 'acc-1',
        accounts: [
          { id: 'acc-1', name: 'A' },
          { id: 'acc-2', name: 'B' },
        ],
      },
      compatibility: {
        status: 'degraded',
        summary: '账号切换不可用',
        guards: { modelSyncWriteBlocked: false, accountSwitchWriteBlocked: true },
      },
    },
  });
  try {
    env.panel.render(env.getState());
    const findNode = (root, pred) => {
      if (!root) return null;
      if (pred(root)) return root;
      for (const child of root.children || []) {
        const hit = findNode(child, pred);
        if (hit) return hit;
      }
      return null;
    };
    const select = findNode(env.byId('plus-account-bar'), node => node.tagName === 'SELECT');
    assert.ok(select && select.disabled, 'top account switcher should be disabled');
    const body = env.byId('plus-accounts-body');
    assert.match(textOf(body), /账号切换与写入已暂停/);
    const switchBtn = findNode(body, node => node.tagName === 'BUTTON' && textOf(node) === '切换');
    const addBtn = findNode(body, node => node.tagName === 'BUTTON' && textOf(node) === '添加账号');
    assert.ok(switchBtn && switchBtn.disabled, 'switch button should be disabled');
    assert.ok(addBtn && addBtn.disabled, 'add button should be disabled');
  } finally { env.restore(); }
});

test('interrupted restore remains recoverable after removing the failed target account', async () => {
  const locked = {
    state: 'ready',
    currentId: 'acct-a',
    adding: false,
    restoreAccountId: 'acct-a',
    accounts: [{ id: 'acct-a', name: '原账号' }],
    activation: {
      state: 'failed',
      reason: 'account_session_rollback_unverified',
      failureReason: 'account_session_restart_failed',
    },
  };
  const actions = [];
  let step = 0;
  let operation = null;
  const env = makePlus({
    data: { accounts: locked },
    routes: {
      '/api/actions': body => {
        actions.push(body.action);
        step++;
        if (step === 1) {
          operation = { id: 'op-1', action: body.action, state: 'failed', reason: 'account_session_rollback_unverified' };
        } else {
          operation = { id: 'op-' + step, action: body.action, state: 'done' };
        }
        return { operation };
      },
      '/api/state': () => ({
        accounts: step >= 2 ? { ...locked, restoreAccountId: '', activation: { state: 'restored', rolledBack: true } } : locked,
        operation,
      }),
    },
  });
  try {
    env.panel.render(env.getState());
    const bar = env.byId('plus-account-bar');
    const body = env.byId('plus-accounts-body');
    assert.ok(findButton(bar, '恢复原账号'), 'top bar must offer direct restore button');
    assert.ok(findButton(body, '恢复原账号'), 'pending card must offer restore button');
    assert.ok(findButton(body, '正常重启 Kimi 并恢复原账号'), 'pending card must offer restart-and-restore button');
    assert.ok(findButton(body, '保存当前登录并解除待恢复'), 'pending card must offer capture unlock button');

    await findButton(bar, '恢复原账号').listeners.click[0]();
    assert.match(textOf(env.byId('plus-accounts-body')), /暂未确认原账号登录状态/);
    assert.doesNotMatch(textOf(env.byId('plus-accounts-body')), /account_session_rollback_unverified/);

    await findButton(env.byId('plus-accounts-body'), '保存当前登录并解除待恢复').listeners.click[0]();
    assert.deepEqual(actions, ['account-cancel-add', 'account-capture']);
    assert.equal(env.getState().accounts.restoreAccountId, '');
    assert.ok(findButton(env.byId('plus-accounts-body'), '添加账号'));
  } finally { env.restore(); }
});

test('failed switch after verified rollback offers plain Kimi restart instead of begin-add', async () => {
  const book = {
    state: 'ready',
    currentId: 'acct-a',
    adding: false,
    restoreAccountId: '',
    accounts: [{ id: 'acct-a', name: 'A' }, { id: 'acct-b', name: 'B' }],
  };
  const actions = [];
  let operation = null;
  const env = makePlus({
    data: { accounts: book },
    routes: {
      '/api/actions': body => {
        actions.push(body.action);
        operation = body.action === 'account-switch'
          ? { id: 'op-1', action: 'account-switch', state: 'failed', reason: 'account_session_restart_failed' }
          : { id: 'op-2', action: body.action, state: 'done' };
        return { operation };
      },
      '/api/state': () => ({ accounts: book, operation }),
    },
  });
  try {
    env.panel.render(env.getState());
    findButton(env.byId('plus-accounts-body'), '切换').listeners.click[0]();
    await new Promise(resolve => setTimeout(resolve, 1100));
    const body = env.byId('plus-accounts-body');
    assert.equal(findButton(body, '正常重启 Kimi 并继续添加'), null);
    const restartBtn = findButton(body, '正常重启 Kimi');
    assert.ok(restartBtn);
    await restartBtn.listeners.click[0]();
    assert.deepEqual(actions, ['account-switch', 'restart-kimi']);
  } finally { env.restore(); }
});

/* 热回滚后凭据失效的是「刚才想切过去的那个账号」，原账号已经恢复。
   这里守住两个方向：不能说成当前窗口需要登录，也不能把正常的待登录状态吞掉。 */
test('a rolled-back switch blames the target account instead of the restored one', () => {
  const env = makePlus({ data: { accounts: {
    state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: '原账号' }],
    activation: { state: 'restored', rolledBack: true, requiresLogin: true },
  } } });
  try {
    env.panel.render(env.getState());
    const text = textOf(env.byId('plus-overview-account'));
    assert.match(text, /已恢复原账号/);
    assert.match(text, /所选账号需重新登录/);
    assert.doesNotMatch(text, /需要在该窗口登录/);
  } finally { env.restore(); }
});

test('a switch that still needs a login keeps the login prompt', () => {
  const env = makePlus({ data: { accounts: {
    state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: 'A' }],
    activation: { state: 'needs-login', requiresLogin: true },
  } } });
  try {
    env.panel.render(env.getState());
    const text = textOf(env.byId('plus-overview-account'));
    assert.match(text, /需要在该窗口登录/);
    assert.doesNotMatch(text, /所选账号需重新登录/);
  } finally { env.restore(); }
});

/* state=failed 但已明确 rolledBack 时，同样属于回到原账号回执。 */
test('a rolled-back failure keeps the target-account wording', () => {
  const env = makePlus({ data: { accounts: {
    state: 'ready', currentId: 'acct-a', accounts: [{ id: 'acct-a', name: '原账号' }],
    activation: { state: 'failed', rolledBack: true, requiresLogin: true },
  } } });
  try {
    env.panel.render(env.getState());
    const text = textOf(env.byId('plus-overview-account'));
    assert.match(text, /所选账号需重新登录/);
    assert.doesNotMatch(text, /需要在该窗口登录/);
  } finally { env.restore(); }
});

test('account cards show calm token health hints only for expired or expiring tokens', () => {
  const now = Date.now();
  const collectByClass = (root, cls, out = []) => {
    if (!root) return out;
    if ((root.className || '').split(/\s+/).includes(cls)) out.push(root);
    for (const child of root.children || []) collectByClass(child, cls, out);
    return out;
  };
  const env = makePlus({ data: { accounts: {
    state: 'ready',
    currentId: 'acc-healthy',
    accounts: [
      { id: 'acc-healthy', name: '健康账号', tokenHealth: 'healthy', refreshExpiresAt: now - 60000 },
      { id: 'acc-unknown', name: '未知账号', tokenHealth: 'unknown' },
      { id: 'acc-plain', name: '无字段账号' },
      { id: 'acc-expired', name: '过期账号', tokenHealth: 'expired' },
      { id: 'acc-soon', name: '临近过期账号', tokenHealth: 'expiring_soon' },
      { id: 'acc-refresh-past', name: '刷新已过期账号', refreshExpiresAt: new Date(now - 60000).toISOString() },
      { id: 'acc-refresh-soon', name: '刷新临近过期账号', refreshExpiresAt: now + 3600 * 1000 },
      { id: 'acc-refresh-ok-access-past', name: '刷新有效账号', refreshExpiresAt: now + 20 * 24 * 3600 * 1000, accessExpiresAt: now - 60000 },
      { id: 'acc-access-past', name: '访问已过期账号', accessExpiresAt: now - 60000 },
    ],
  } } });
  try {
    env.panel.render(env.getState());
    const body = env.byId('plus-accounts-body');
    const cards = collectByClass(body, 'plus-account');
    assert.equal(cards.length, 9);

    const hintOf = (name) => {
      const card = cards.find(item => textOf(item).includes(name));
      assert.ok(card, 'missing card: ' + name);
      return collectByClass(card, 'plus-account__health');
    };

    assert.equal(hintOf('健康账号').length, 0);
    assert.equal(hintOf('未知账号').length, 0);
    assert.equal(hintOf('无字段账号').length, 0);
    assert.equal(hintOf('刷新有效账号').length, 0);

    const expired = hintOf('过期账号');
    assert.equal(expired.length, 1);
    assert.equal(expired[0].dataset.health, 'expired');
    assert.match(expired[0].textContent, /登录令牌可能已过期，切换失败时请重新登录/);

    const soon = hintOf('临近过期账号');
    assert.equal(soon.length, 1);
    assert.equal(soon[0].dataset.health, 'expiring_soon');
    assert.match(soon[0].textContent, /登录令牌临近过期/);

    const refreshPast = hintOf('刷新已过期账号');
    assert.equal(refreshPast.length, 1);
    assert.equal(refreshPast[0].dataset.health, 'expired');
    assert.match(refreshPast[0].textContent, /登录令牌可能已过期/);

    const refreshSoon = hintOf('刷新临近过期账号');
    assert.equal(refreshSoon.length, 1);
    assert.equal(refreshSoon[0].dataset.health, 'expiring_soon');
    assert.match(refreshSoon[0].textContent, /登录令牌临近过期/);

    const accessPast = hintOf('访问已过期账号');
    assert.equal(accessPast.length, 1);
    assert.equal(accessPast[0].dataset.health, 'expired');

    const overviewHints = collectByClass(env.byId('plus-overview-account'), 'plus-account__health');
    assert.equal(overviewHints.length, 0);
  } finally { env.restore(); }
});

test('accounts bar and management short-circuit unchanged DOM and runAccount resets busy when operation id is absent', async () => {
  const env = makePlus({
    data: {
      accounts: {
        state: 'ready',
        currentId: 'acct-a',
        accounts: [
          { id: 'acct-a', name: '主账号' },
          { id: 'acct-b', name: '备账号' },
        ],
      },
    },
    routes: {
      '/api/actions': () => ({ operation: { state: 'done' } }),
      '/api/state': () => env.getState(),
    },
  });
  try {
    env.panel.render(env.getState());
    const selectFirst = env.byId('plus-account-select');
    const bodyFirst = env.byId('plus-accounts-body').children[0];
    assert.ok(selectFirst);
    assert.ok(bodyFirst);

    env.panel.render(env.getState());
    assert.strictEqual(env.byId('plus-account-select'), selectFirst, 'unchanged poll must not replace active select');
    assert.strictEqual(env.byId('plus-accounts-body').children[0], bodyFirst, 'unchanged poll must not rebuild accounts management DOM');

    // Triggering an account switch whose response omits operation.id must reset busy in finally.
    selectFirst.value = 'acct-b';
    await selectFirst.listeners.change[0]();
    assert.equal(env.byId('plus-account-select').disabled, false, 'busy must reset after response without operation.id');
  } finally { env.restore(); }
});

test('unavailable accounts book maps raw reason codes to Chinese copy instead of exposing English codes', () => {
  const env = makePlus({
    data: {
      accounts: {
        state: 'unavailable',
        reason: 'account_switch_unavailable',
        accounts: [],
      },
    },
  });
  try {
    env.panel.render(env.getState());
    const barText = textOf(env.byId('plus-account-slot'));
    const bodyText = textOf(env.byId('plus-accounts-body'));
    assert.doesNotMatch(barText, /account_switch_unavailable/);
    assert.doesNotMatch(bodyText, /account_switch_unavailable/);
    assert.match(bodyText, /这台机器上没有可用的账号切换接口/);
  } finally { env.restore(); }
});

test('offline account switching stays clickable when Kimi Work is not running and displays pending sync notice', async () => {
  const actions = [];
  let switched = false;
  const baseAccounts = {
    state: 'offline',
    currentId: 'acct-a',
    accounts: [
      { id: 'acct-a', name: '主账号' },
      { id: 'acct-b', name: '备账号' },
    ],
  };
  const env = makePlus({
    extraRoots: ['view-accounts'],
    data: { accounts: baseAccounts },
    routes: {
      '/api/actions': body => {
        actions.push(body);
        if (body.action === 'account-switch') {
          switched = true;
          return {
            operation: { id: 'op-offline', action: 'account-switch', state: 'done' },
            activation: { state: 'offline', accountId: body.accountId, verified: true },
          };
        }
        return { operation: { id: 'op-other', action: body.action, state: 'done' } };
      },
      '/api/state': () => ({
        operation: { id: 'op-offline', action: 'account-switch', state: 'done' },
        accounts: switched
          ? {
            ...baseAccounts,
            pendingAccountId: 'acct-b',
            activation: { state: 'offline', accountId: 'acct-b', verified: true },
          }
          : baseAccounts,
      }),
    },
  });
  try {
    assert.equal(env.dom.roots['view-accounts'].firstChild.id, 'plus-accounts-panel');
    env.panel.render(env.getState(), { view: 'accounts' });

    const body = env.byId('plus-accounts-body');
    const switchBtn = findButton(body, '切换');
    assert.ok(switchBtn, 'switch button must render even when accounts.state !== ready');
    assert.equal(switchBtn.disabled, false, 'switch button must remain clickable when Kimi Work is not running');

    const select = env.byId('plus-account-select');
    assert.ok(select && !select.disabled, 'top bar account switcher must remain enabled offline');

    switchBtn.listeners.click[0]();
    await new Promise(resolve => setTimeout(resolve, 1100));

    assert.deepEqual(actions[0], { action: 'account-switch', accountId: 'acct-b' });
    env.panel.render(env.getState(), { view: 'accounts' });
    const afterText = textOf(env.byId('plus-accounts-body'));
    assert.match(afterText, /已切换默认账号凭据，下次打开 Kimi Work 时将自动同步登录态/);

    env.panel.render(env.getState(), { view: 'overview' });
    const overviewText = textOf(env.byId('plus-overview-account'));
    assert.match(overviewText, /已切换默认账号凭据，下次打开 Kimi Work 时将自动同步登录态/);
    assert.doesNotMatch(overviewText, /\boffline\b/);

    const captureBtn = findButton(env.byId('plus-accounts-body'), '只保存当前登录');
    assert.ok(captureBtn);
    captureBtn.listeners.click[0]();
    assert.equal(actions.some(item => item.action === 'account-capture'), false);
    assert.match(textOf(env.byId('plus-accounts-body')), /请先打开并登录 Kimi Work，再保存当前账号/);
  } finally { env.restore(); }
});

test('the real kimiProcess shape drives offline switching even though accounts.state stays ready', async () => {
  const actions = [];
  const book = {
    state: 'ready',
    currentId: 'acct-a',
    accounts: [{ id: 'acct-a', name: '主账号' }, { id: 'acct-b', name: '备账号' }],
  };
  const stopped = { kimiProcess: { running: false, state: 'not_running' }, accounts: book };
  const env = makePlus({
    extraRoots: ['view-accounts'],
    data: stopped,
    routes: {
      '/api/actions': body => { actions.push(body); return { operation: { id: 'op-1', action: body.action, state: 'done' } }; },
      '/api/state': () => stopped,
    },
  });
  try {
    env.panel.render(env.getState(), { view: 'accounts' });
    const bar = env.byId('plus-account-bar');
    assert.match(textOf(bar), /可离线切换默认账号/);
    assert.doesNotMatch(textOf(bar), /必要时重启 Work/);
    assert.ok(findButton(env.byId('plus-accounts-body'), '只保存当前登录'));

    findButton(env.byId('plus-accounts-body'), '只保存当前登录').listeners.click[0]();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(actions.some(item => item.action === 'account-capture'), false);
    assert.match(textOf(env.byId('plus-accounts-body')), /请先打开并登录 Kimi Work，再保存当前账号/);
  } finally { env.restore(); }
});

test('the top bar re-renders from running to offline while accounts.state stays ready', () => {
  const book = {
    state: 'ready',
    currentId: 'acct-a',
    accounts: [{ id: 'acct-a', name: '主账号' }, { id: 'acct-b', name: '备账号' }],
  };
  const env = makePlus({ data: { kimiProcess: { running: true, state: 'running' }, accounts: book } });
  try {
    env.panel.render(env.getState());
    const bar = env.byId('plus-account-bar');
    assert.match(textOf(bar), /必要时重启 Work/);

    env.setState({ kimiProcess: { running: false, state: 'not_running' }, accounts: book });
    env.panel.render(env.getState());
    assert.match(textOf(env.byId('plus-account-bar')), /可离线切换默认账号/);

    env.setState({ kimiProcess: { running: null, state: 'unknown' }, accounts: book });
    env.panel.render(env.getState());
    assert.match(textOf(env.byId('plus-account-bar')), /必要时重启 Work/);
  } finally { env.restore(); }
});

test('an offline switch notice tells the user the sync happens next launch', async () => {
  const book = {
    state: 'ready',
    currentId: 'acct-a',
    accounts: [{ id: 'acct-a', name: '主账号' }, { id: 'acct-b', name: '备账号' }],
  };
  const env = makePlus({
    data: { kimiProcess: { running: false, state: 'not_running' }, accounts: book },
    routes: {
      '/api/actions': body => ({ operation: { id: 'op-1', action: body.action, state: 'done' } }),
      '/api/state': () => ({
        kimiProcess: { running: false, state: 'not_running' },
        accounts: book,
        operation: { id: 'op-1', action: 'account-switch', state: 'done' },
      }),
    },
  });
  try {
    env.panel.render(env.getState());
    const select = env.byId('plus-account-select');
    select.value = 'acct-b';
    select.listeners.change[0]();
    await new Promise(resolve => setTimeout(resolve, 1000));
    const notice = env.notices.find(item => /正在切换默认账号凭据/.test(item.message));
    assert.ok(notice, 'offline switch must announce the next-launch sync');
    assert.match(notice.message, /下次打开 Kimi Work 时将自动同步登录态/);
    assert.doesNotMatch(notice.message, /必要时正常重启/);
  } finally { env.restore(); }
});

test('account capture surfaces a plain login prompt when backend returns account_session_unavailable', async () => {
  const book = { state: 'ready', currentId: '', accounts: [] };
  const env = makePlus({
    extraRoots: ['plus-accounts-host'],
    data: { accounts: book },
    routes: {
      '/api/actions': body => ({ operation: { id: 'cap-1', action: body.action, state: 'failed', reason: 'account_session_unavailable' } }),
      '/api/state': () => ({ accounts: book, operation: { id: 'cap-1', action: 'account-capture', state: 'failed', reason: 'account_session_unavailable' } }),
    },
  });
  try {
    assert.equal(env.dom.roots['plus-accounts-host'].firstChild.id, 'plus-accounts-panel');
    env.panel.render(env.getState(), { view: 'accounts' });
    await findButton(env.byId('plus-accounts-body'), '保存当前账号').listeners.click[0]();
    const body = env.byId('plus-accounts-body');
    assert.match(textOf(body), /请先打开并登录 Kimi Work，再保存当前账号/);
    assert.equal(findButton(body, '正常重启 Kimi 并继续添加'), null);
  } finally { env.restore(); }
});

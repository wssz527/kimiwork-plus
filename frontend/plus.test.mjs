import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

/* plus.js 是一个 IIFE，挂载时读 DOM。这里的测试用一个极小的替身跑真实的源文件，
   只验证「怎么显示」这件事：哪些值必须原样保留、哪些必须显示为未提供、哪些
   按钮在什么状态下才出现。真实 DOM 与配色不在测试范围内。 */

const source = readFileSync(new URL('./plus.js', import.meta.url), 'utf8');

/* 最小 DOM：只实现 plus.js 用到的那几个成员，并记录渲染出的文本。 */
function makeDom() {
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

function makePlus({ data = {}, routes = {} } = {}) {
  const dom = makeDom();
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
    calls.push({ path, body: options.body });
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
  const panel = api.mount({ request, notify, getState: () => state, setState: (next) => { state = next; } });
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
    // 只有一条「会重启」的信息提示，没有额外的确认步骤。
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
  let configured = { enabled: true, currency: 'CNY', timeZone: 'Asia/Shanghai', daily: { limit: 20, action: 'block' } };
  const env = makePlus({ routes: {
    '/api/budget': () => ({ budget: { configured, revision, status: { enabled: Boolean(configured), currency: 'CNY' } } }),
    '/api/budget/save': body => {
      assert.equal(body.revision, revision);
      configured = body.budget;
      revision++;
      return { ok: true, state: { budget: { enabled: Boolean(configured), currency: 'CNY' }, marker: revision } };
    },
  } });
  try {
    await env.panel.refresh();
    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    assert.equal(env.getState().marker, 4);
    const inputs = controlsOf(env.byId('plus-budget-body'), 'INPUT');
    assert.equal(inputs[0].checked, true);
    assert.equal(inputs.find(input => input.type === 'number').value, '20');
    await findButton(env.byId('plus-budget-body'), '保存预算').listeners.click[0]();
    await findButton(env.byId('plus-budget-body'), '清除预算设置').listeners.click[0]();
    assert.equal(revision, 6);
    assert.equal(controlsOf(env.byId('plus-budget-body'), 'INPUT')[0].checked, false);
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

test('an unknown swarm snapshot cannot masquerade as an off switch', async () => {
  const env = makePlus({ data: { workCapabilities: { state: 'inspected', swarm: { state: 'user-controlled' } } }, routes: {
    '/api/work/swarm': () => ({ swarm: { state: 'unknown', conversationKey: 'conv-1', enabled: false, reason: 'request_failed' } }),
  } });
  try {
    env.panel.render(env.getState());
    await findButton(env.byId('plus-capabilities-body'), '读取当前会话').listeners.click[0]();
    const body = env.byId('plus-capabilities-body');
    assert.doesNotMatch(textOf(body), /当前已关闭/);
    assert.match(textOf(body), /读取失败/);
    assert.equal(findButton(body, '开启这个会话的 Swarm'), null);
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
    assert.match(text, /与普通子代理共用模型策略/);
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

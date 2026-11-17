import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const growthSource = readFileSync(new URL('./growth.js', import.meta.url), 'utf8');

function extractFunction(text, name, indent = '') {
  const declaration = [`\n${indent}function `, `\n${indent}async function `].map(prefix => prefix + name + '(').find(token => text.includes(token));
  assert.notEqual(declaration, undefined, 'missing source function: ' + name);
  const start = text.indexOf(declaration);
  const end = text.indexOf(`\n${indent}}`, start);
  assert.notEqual(end, -1, 'unterminated source function: ' + name);
  return text.slice(start, end + indent.length + 2);
}

function sourceFunction(name) {
  return extractFunction(source, name);
}

function growthFunction(name, indent = '') {
  return extractFunction(growthSource, name, indent);
}

test('unreadable connection credentials expose explicit system authorization even with no config changes', () => {
  const copy = source.slice(source.indexOf('const APPLY_COPY ='), source.indexOf('\nconst APPLY_REFUSAL ='));
  const state = { pendingAction: null, applyUnsupported: false };
  const dom = { applyButton: {}, restartHead: {}, applyVerdict: {}, applyVerdictDot: {}, applyVerdictWord: {}, applyVerdictNote: {} };
  const render = new Function('state', 'dom', `
    ${copy}
    const connectionsChanged = () => false;
    const isInFlight = op => op?.state === 'running';
    const setText = (element, text) => { element.textContent = text; };
    const setHidden = (element, hidden) => { element.hidden = hidden; };
    const setDot = () => {};
    ${sourceFunction('applyAvailability')}
    ${sourceFunction('applyState')}
    ${sourceFunction('renderApply')}
    return renderApply;
  `)(state, dom);
  const data = { connections: { restartRequired: false, apply: { state: 'failed', reason: 'credentials_unavailable' } } };
  render(data);
  assert.equal(dom.applyButton.hidden, false);
  assert.equal(dom.applyButton.textContent, '重新授权并应用连接');
  assert.equal(dom.applyVerdictWord.textContent, '连接需要系统授权');
  render({ ...data, operation: { action: 'apply-connections', state: 'running' } });
  assert.equal(dom.applyVerdictWord.textContent, '正在应用');
  assert.equal(dom.applyButton.hidden, true);
});

test('a verified account change requests model synchronization instead of another login', () => {
  const syncState = new Function(sourceFunction('syncState') + '\nreturn syncState;')();
  for (const reason of ['account_changed', 'native_import_account_changed']) {
    const data = { integration: { state: 'needs-native-import', reason, readback: { accountVerified: true } } };
    const verdict = syncState(data);
    assert.equal(verdict.code, 'pending');
    assert.equal(verdict.word, '当前账号待同步');
    assert.match(verdict.note, /当前账号已登录/);
    assert.doesNotMatch(verdict.note, /重启/);
    assert.equal(syncState({ integration: { ...data.integration, readback: { accountVerified: false } } }).code, 'login');
  }
  assert.equal(syncState({ integration: { reason: 'native_import_waiting_login', readback: { accountVerified: true } } }).code, 'login');
});

function makeProblems() {
  const code = [
    'const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;',
    "const CONNECTION_ID_HINT = '标识只能使用字母、数字、点、下划线与连字符，且以字母或数字开头。';",
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    'const onboardConnectionValues = () => ({ id: "myconn", name: "我的供应商", baseURL: "https://api.example.com/v1", apiKey: "sk-test", providerPresetId: "" });',
    'const savedOnboardRow = () => null;',
    ...['modelFamilyFor', 'routeIdFor', 'protocolList', 'connectionProtocolLimits', 'preferredProtocol', 'buildOnboardRow', 'normalizeBaseURL', 'protocolLabel', 'onboardProblems'].map(sourceFunction),
    'return onboardProblems;',
  ].join('\n');
  return new Function(code)();
}

function makeRow(id, extra = {}) {
  return {
    id,
    upstreamModelId: id,
    protocols: [],
    protocol: null,
    contextWindow: undefined,
    efforts: [],
    metadataKind: 'discovery',
    selected: true,
    ...extra,
  };
}

test('onboard validation skips empty route IDs without hiding real duplicates', () => {
  const onboardProblems = makeProblems();
  const missing = onboardProblems({ models: [makeRow('model-a'), makeRow('model-b'), makeRow('model-c')] });
  assert.equal(missing.problems.some((text) => text.includes('模型路由 ID 重复')), false);
  assert.deepEqual(missing.built.map((model) => model.id), ['', '', '']);
  assert.equal(missing.problems.some((text) => text.includes('接口协议')), true);

  const valid = onboardProblems({ models: [
    makeRow('model-a', { protocol: 'chat', contextWindow: 128000 }),
    makeRow('model-b', { protocol: 'chat', contextWindow: 128000 }),
    makeRow('model-c', { protocol: 'responses', contextWindow: 128000 }),
  ] });
  assert.deepEqual(valid.problems, []);

  const duplicate = onboardProblems({ models: [
    makeRow('model-a', { protocol: 'chat', contextWindow: 128000 }),
    makeRow('model-a', { protocol: 'chat', contextWindow: 128000 }),
  ] });
  assert.equal(duplicate.problems.filter((text) => text.includes('模型路由 ID 重复')).length, 1);
});

function makeOnboardEnv(savedConnections, { touched = true } = {}) {
  const dom = {
    onboardId: { value: '' },
    onboardName: { value: '我的供应商' },
    onboardBase: { value: 'https://api.example.com/v1' },
    onboardPreset: { value: '' },
    onboardKey: { value: 'sk-user-just-typed' },
    onboardKeyHint: null,
    onboardDiscoverState: null,
  };
  const state = {
    data: { connections: { saved: savedConnections } },
    onboard: { discovery: { models: [] }, touched },
  };
  const code = [
    ...['connectionRowsOf', 'savedOnboardTarget', 'onboardTouched', 'fillOnboardForm'].map(sourceFunction),
    'const fillOnboardPresets = () => {};',
    'const setText = () => {};',
    'const fmtCount = (value) => String(value);',
    'return { onboardTouched, fillOnboardForm };',
  ].join('\n');
  return { dom, state, ...new Function('dom', 'state', code)(dom, state) };
}

function pollOnboard(env) {
  if (!env.onboardTouched()) env.fillOnboardForm();
}

test('polling preserves a newly typed key even when the connection ID is empty', () => {
  const env = makeOnboardEnv([]);
  pollOnboard(env);
  assert.equal(env.dom.onboardKey.value, 'sk-user-just-typed');
  assert.equal(env.dom.onboardBase.value, 'https://api.example.com/v1');
});

test('polling preserves a key after the connection ID has been filled', () => {
  const env = makeOnboardEnv([]);
  env.dom.onboardId.value = 'myconn';
  pollOnboard(env);
  assert.equal(env.dom.onboardKey.value, 'sk-user-just-typed');
  assert.equal(env.dom.onboardId.value, 'myconn');
});

test('polling cannot overwrite a replacement endpoint and key with saved values', () => {
  const env = makeOnboardEnv([{ id: 'old', name: '旧连接', baseURL: 'https://old.example.com/v1', apiKeySet: true }]);
  pollOnboard(env);
  assert.equal(env.dom.onboardBase.value, 'https://api.example.com/v1');
  assert.equal(env.dom.onboardName.value, '我的供应商');
  assert.equal(env.dom.onboardKey.value, 'sk-user-just-typed');
});

test('saved form backfill no longer clears a key', () => {
  const env = makeOnboardEnv([{ id: 'old', name: '旧连接', baseURL: 'https://old.example.com/v1', apiKeySet: true }], { touched: false });
  pollOnboard(env);
  assert.equal(env.dom.onboardId.value, 'old');
  assert.equal(env.dom.onboardBase.value, 'https://old.example.com/v1');
  assert.equal(env.dom.onboardKey.value, 'sk-user-just-typed');
});

test('onboard save merges selected models and retains unselected saved models', () => {
  const mergeOnboardModels = new Function(sourceFunction('mergeOnboardModels') + '\nreturn mergeOnboardModels;')();
  const existing = [
    { id: 'myconn/kimi-old-a', connectionId: 'myconn', name: 'old-a' },
    { id: 'myconn/kimi-old-b', connectionId: 'myconn', name: 'old-b' },
    { id: 'other/kimi-other', connectionId: 'other', name: 'other' },
  ];
  const added = [{ id: 'myconn/kimi-new', connectionId: 'myconn', name: 'new' }];
  assert.deepEqual(mergeOnboardModels(existing, added, 'myconn'), [...existing, ...added]);

  const updated = [{ id: 'myconn/kimi-old-a', connectionId: 'myconn', name: 'updated' }];
  const merged = mergeOnboardModels(existing, updated, 'myconn');
  assert.deepEqual(merged.map((model) => model.id), ['myconn/kimi-old-b', 'other/kimi-other', 'myconn/kimi-old-a']);
  assert.equal(merged.at(-1).name, 'updated');
});

test('discovery retains custom names for saved models on the same connection', () => {
  const preserve = new Function(sourceFunction('preserveOnboardModelNames') + '\nreturn preserveOnboardModelNames;')();
  const discovered = [{ upstreamModelId: 'model-a', name: 'Upstream A' }, { upstreamModelId: 'model-b', name: 'Upstream B' }];
  const saved = [
    { connectionId: 'myconn', upstreamModelId: 'model-a', name: 'My custom A' },
    { connectionId: 'other', upstreamModelId: 'model-b', name: 'Other B' },
  ];
  assert.deepEqual(preserve(discovered, saved, 'myconn'), [
    { upstreamModelId: 'model-a', name: 'My custom A' },
    { upstreamModelId: 'model-b', name: 'Upstream B' },
  ]);
});

test('onboard search matches upstream ID, name and description', () => {
  const state = { onboard: { query: 'VISION', discovery: { models: [
    makeRow('alpha'), makeRow('beta', { name: 'Vision model' }), makeRow('gamma', { description: 'vision capable' }),
  ] } } };
  const code = [sourceFunction('onboardModelRows'), sourceFunction('filteredOnboardModels'), 'return filteredOnboardModels;'].join('\n');
  const filtered = new Function('state', code)(state);
  assert.deepEqual(filtered().map((row) => row.id), ['beta', 'gamma']);
  state.onboard.query = '';
  assert.equal(filtered().length, 3);
});

test('select all applies only to the current search results and toggles them off', () => {
  const state = { onboard: { query: 'match', touched: false, discovery: { models: [makeRow('match-a', { selected: false }), makeRow('match-b', { selected: false }), makeRow('other', { selected: false })] } } };
  const code = [
    ...['onboardModelRows', 'filteredOnboardModels', 'toggleFilteredOnboardModels'].map(sourceFunction),
    'const renderOnboard = () => {};',
    'return toggleFilteredOnboardModels;',
  ].join('\n');
  const toggle = new Function('state', code)(state);
  toggle();
  assert.deepEqual(state.onboard.discovery.models.map((row) => row.selected), [true, true, false]);
  toggle();
  assert.deepEqual(state.onboard.discovery.models.map((row) => row.selected), [false, false, false]);
});

test('batch protocol is explicit and respects declarations and unselected rows', () => {
  const state = { onboard: { touched: false, discovery: { models: [
    makeRow('allowed', { selected: true, protocolOptions: ['chat'] }),
    makeRow('responses-only', { selected: true, protocols: ['responses'], protocolOptions: ['responses'], protocol: 'responses' }),
    makeRow('unselected', { selected: false, protocolOptions: ['chat'] }),
  ] } } };
  const messages = [];
  const code = [
    'const PROTOCOL_CHOICES = ["chat", "responses"];',
    ...['onboardModelRows', 'selectedOnboardModels', 'assignOnboardProtocol', 'applyOnboardBatchProtocol', 'protocolLabel'].map(sourceFunction),
    'const fmtCount = (value) => String(value);',
    'const renderOnboard = () => {};',
    'const showToast = (message) => messages.push(message);',
    'return applyOnboardBatchProtocol;',
  ].join('\n');
  const apply = new Function('state', 'messages', code)(state, messages);
  apply('chat');
  assert.equal(state.onboard.discovery.models[0].protocol, 'chat');
  assert.equal(state.onboard.discovery.models[1].protocol, 'responses');
  assert.equal(state.onboard.discovery.models[2].protocol, null);
  assert.equal(state.onboard.touched, true);
  assert.match(messages.at(-1), /1 个因声明或连接限制未改动/);
});

test('batch context changes only selected rows and rejects unknown values', () => {
  const state = { onboard: { touched: false, discovery: { models: [makeRow('a', { selected: true }), makeRow('b', { selected: false })] } } };
  const dom = { onboardBatchContext: { value: '128000' } };
  const messages = [];
  const code = [
    ...['onboardModelRows', 'selectedOnboardModels', 'applyOnboardBatchContext'].map(sourceFunction),
    'const fmtCount = (value) => String(value);',
    'const fmtTokens = (value) => String(value);',
    'const renderOnboard = () => {};',
    'const showToast = (message) => messages.push(message);',
    'return applyOnboardBatchContext;',
  ].join('\n');
  const apply = new Function('dom', 'state', 'messages', code)(dom, state, messages);
  apply();
  assert.equal(state.onboard.discovery.models[0].contextWindow, 128000);
  assert.equal(state.onboard.discovery.models[0].metadataKind, 'manual');
  assert.equal(state.onboard.discovery.models[1].contextWindow, undefined);
  dom.onboardBatchContext.value = 'unknown';
  apply();
  assert.equal(state.onboard.discovery.models[0].contextWindow, 128000);
  assert.match(messages.at(-1), /正整数/);
});

test('late provider loading refreshes wizard options without changing typed fields', async () => {
  const state = { key: 'session-key', providers: null, onboard: { touched: true } };
  const dom = { onboardPreset: { value: '' }, onboardPresetHint: null, onboardKey: { value: 'sk-user-just-typed' } };
  const options = [];
  const renders = [];
  const code = [
    ...['providerPresets', 'providerPresetLabel', 'providerPresetOptions', 'fillOnboardPresets', 'loadProviders'].map(sourceFunction),
    'return loadProviders;',
  ].join('\n');
  const load = new Function('state', 'dom', 'API', 'request', 'setText', 'fmtCount', 'syncSelectOptions', 'renderConnectionEditor', 'refreshSavedProtocolDeclarations', 'renderOnboard', code)(
    state, dom, { providers: '/api/providers' }, async () => ({ presets: [{ id: 'deepseek', name: 'DeepSeek' }] }),
    () => {}, String, (select, rows, current) => { select.value = current; options.push(...rows); }, () => {}, () => {}, () => renders.push('onboard'),
  );
  await load();
  assert.deepEqual(options.map(option => option.value), ['', 'deepseek']);
  assert.equal(dom.onboardKey.value, 'sk-user-just-typed');
  assert.equal(state.onboard.touched, true);
  assert.deepEqual(renders, ['onboard']);
});

test('the shared wizard opens at provider choice without discarding a dirty editor', () => {
  const calls = [];
  const state = { editor: null };
  const dom = {
    onboardDisclosure: { open: false, dataset: {}, scrollIntoView: () => calls.push('scroll') },
    onboardPreset: { focus: () => calls.push('focus') },
  };
  const code = [
    sourceFunction('openOnboardWizard'),
    'const setView = (view) => calls.push(view);',
    'const editorDirty = () => state.editor?.dirty === true;',
    'const closeEditor = () => { state.editor = null; calls.push("close"); };',
    'const showToast = (message) => calls.push(message);',
    'return openOnboardWizard;',
  ].join('\n');
  const open = new Function('state', 'dom', 'calls', code)(state, dom, calls);
  open();
  assert.equal(dom.onboardDisclosure.open, true);
  assert.deepEqual(calls, ['models', 'close', 'scroll', 'focus']);
  calls.length = 0;
  state.editor = { dirty: true };
  open();
  assert.deepEqual(calls, ['models', '请先保存或取消正在编辑的模型，再打开添加向导。']);
});

test('catalogue reads require only address and key and never borrow a key for a changed endpoint', () => {
  const code = [
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    sourceFunction('normalizeBaseURL'), sourceFunction('discoveryBody'), 'return discoveryBody;'
  ].join('\n');
  const build = new Function(code)();
  const saved = { id: 'saved', baseURL: 'https://api.example.com/v1', apiKeySet: true };
  const anonymous = build({ id: '', baseURL: 'https://api.example.com/v1', apiKey: 'sk-new', providerPresetId: '' }, null, 'rev');
  assert.equal(anonymous.error, undefined);
  assert.deepEqual(anonymous.body, { revision: 'rev', baseURL: 'https://api.example.com/v1', apiKey: 'sk-new' });
  const reused = build({ id: 'saved', baseURL: 'https://api.example.com/v1', apiKey: '' }, saved, 'rev');
  assert.deepEqual(reused.body, { revision: 'rev', baseURL: 'https://api.example.com/v1', connectionId: 'saved' });
  assert.equal(reused.body.apiKey, undefined);
  const changed = build({ id: 'saved', baseURL: 'https://other.example.com/v1', apiKey: '' }, saved, 'rev');
  assert.match(changed.error, /请填写该上游的 API Key/);
  assert.equal(changed.body, undefined);
});

test('a new connection editor reads its current endpoint and key before an ID or saved row exists', async () => {
  const editing = { originalId: null, discovering: false, draft: { id: '', baseURL: 'http://127.0.0.1:39001/v1', apiKey: 'local-acceptance-key', providerPresetId: '' } };
  const state = { data: { revision: 'rev' }, conns: { editing } };
  const calls = [];
  const messages = [];
  const code = [
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    ...['normalizeBaseURL', 'discoveryBody', 'discoverConnectionEditorModels'].map(sourceFunction),
    'const readConnectionEditor = () => {};',
    'const connectionRowsOf = () => [];',
    'const renderConnectionEditor = () => {};',
    'const discoveryReasonText = code => code || "";',
    'const fmtCount = value => String(value);',
    'const showToast = message => messages.push(message);',
    'return discoverConnectionEditorModels;',
  ].join('\n');
  const discover = new Function('state', 'API', 'request', 'calls', 'messages', code)(
    state, { discovery: '/api/discovery' }, async (path, options) => {
      calls.push({ path, body: options.body });
      return { discovery: { state: 'passed', models: [{}], modelCount: 1 } };
    }, calls, messages,
  );
  const result = await discover();
  assert.equal(result.state, 'passed');
  assert.deepEqual(calls, [{ path: '/api/discovery', body: { revision: 'rev', baseURL: 'http://127.0.0.1:39001/v1', apiKey: 'local-acceptance-key' } }]);
  assert.equal(editing.discovering, false);
  assert.match(messages.at(-1), /读到 1 个模型/);
});

test('switching the private panel session clears the visible catalogue query and key', () => {
  const state = { key: 'old-session', onboard: { advanced: false, query: 'vision' } };
  const dom = { onboardKey: { value: 'old-key' }, onboardModelQuery: { value: 'vision' }, toast: { hidden: false }, shell: { hidden: false } };
  const code = [
    'let modelSignature = "", recentSignature = "", saModelSignature = "", saEffortSignature = "";',
    sourceFunction('adoptLocationKey'),
    'return adoptLocationKey;',
  ].join('\n');
  const rotate = new Function('state', 'dom', 'window', 'sessionStorage', 'history', 'KEY_STORE', 'stopPolling', 'setHidden', 'showGate', 'loadProviders', 'loadState', 'startPolling', code)(
    state, dom, { location: { hash: '#key=new-session', pathname: '/', search: '' } },
    { setItem: () => {} }, { replaceState: () => {} }, 'panel-key', () => {}, () => {}, () => {}, () => {}, async () => true, () => {},
  );
  assert.equal(rotate(), true);
  assert.equal(state.key, 'new-session');
  assert.equal(state.onboard.query, '');
  assert.equal(dom.onboardModelQuery.value, '');
  assert.equal(dom.onboardKey.value, '');
});

test('native search stays inside its single existing disclosure', () => {
  const nodes = {};
  const makeNode = (id = '') => ({
    id,
    children: [],
    parent: null,
    append(...children) {
      for (const child of children) {
        if (child.parent) child.parent.children.splice(child.parent.children.indexOf(child), 1);
        child.parent = this;
        this.children.push(child);
      }
    },
    before(child) {
      const index = this.parent.children.indexOf(this);
      this.parent.children.splice(index, 0, child);
      child.parent = this.parent;
    },
  });
  for (const id of ['view-tools', 'view-maintenance', 'native-search-details', 'native-search-settings', 'svc-panel', 'native-panel', 'update-panel', 'sa-diagnostics-body']) nodes[id] = makeNode(id);
  nodes['native-search-settings'].append(nodes['svc-panel']);
  nodes['native-search-details'].append(nodes['native-search-settings']);
  nodes['view-tools'].append(nodes['native-search-details'], nodes['native-panel'], nodes['update-panel']);
  const document = { getElementById: id => nodes[id] || null, createElement: () => makeNode() };
  const code = [
    'const $ = id => document.getElementById(id);',
    growthFunction('relocate'), growthFunction('mountLegacy'), 'return mountLegacy;',
  ].join('\n');
  new Function('document', code)(document)();
  assert.equal(nodes['svc-panel'].parent, nodes['native-search-settings']);
  assert.deepEqual(nodes['view-tools'].children.map(node => node.id), ['native-search-details']);
  assert.equal(nodes['sa-diagnostics-body'].children[0].id, 'role-contract');
});

test('web provider selection controls the real native search disclosure', () => {
  const elements = new Proxy({ 'web-provider': { value: '' } }, {
    get(target, id) { return target[id] ||= {}; },
  });
  const local = { webDraft: null, webResult: null };
  const code = growthFunction('renderWeb', '  ') + '\nreturn renderWeb;';
  const render = new Function('$', 'text', 'hidden', 'local', code)(
    id => elements[id],
    (id, value) => { elements[id].textContent = value; },
    (id, value) => { elements[id].hidden = value; },
    local,
  );
  const data = { webTools: { provider: 'official', keySet: false } };
  render(data);
  assert.equal(elements['native-search-details'].hidden, true);
  assert.notEqual(elements['svc-panel'].hidden, false);
  local.webDraft = 'native';
  render(data);
  assert.equal(elements['native-search-details'].hidden, false);
});

/* ---------- 阶段 2：视图合并与整组保存 ---------- */

function makeDomTree() {
  const byId = new Map();
  const makeNode = (id = '', tag = 'SECTION') => {
    const node = {
      id, tagName: tag, children: [], parent: null, open: false, hidden: false, focused: false,
      attributes: {}, scrolled: false,
      append(...nodes) {
        for (const child of nodes) {
          if (!child) continue;
          if (child.parent) child.parent.children.splice(child.parent.children.indexOf(child), 1);
          child.parent = this;
          this.children.push(child);
        }
      },
      remove() {
        if (this.parent) this.parent.children.splice(this.parent.children.indexOf(this), 1);
        this.parent = null;
        /* 真实 DOM 里被移除的节点不再能从 getElementById 取到。 */
        if (this.id && byId.get(this.id) === this) byId.delete(this.id);
      },
      querySelector(selector) {
        const tags = selector.split(',').map(part => part.trim().toUpperCase());
        const walk = node => {
          for (const child of node.children) {
            if (tags.includes(child.tagName)) return child;
            const found = walk(child);
            if (found) return found;
          }
          return null;
        };
        return walk(this);
      },
      closest(tag) {
        for (let node = this; node; node = node.parent) {
          if (node.tagName === tag.toUpperCase()) return node;
        }
        return null;
      },
      scrollIntoView() { this.scrolled = true; },
      focus() { this.focused = true; },
      hasAttribute(name) { return Object.hasOwn(this.attributes, name); },
      setAttribute(name, value) { this.attributes[name] = value; },
    };
    Object.defineProperty(node, 'parentElement', { get() { return this.parent; } });
    if (id) byId.set(id, node);
    return node;
  };
  const document = {
    getElementById: id => byId.get(id) || null,
    createElement: () => makeNode('', 'DIV'),
  };
  return { byId, makeNode, document };
}

test('mounting the merged views is idempotent and keeps one recent-requests table', () => {
  const { byId, makeNode, document } = makeDomTree();
  const root = (id, tag) => makeNode(id, tag);
  const modelsBody = root('models-services-body');
  const connectionsPanel = root('connections-panel');
  const maintenance = root('view-maintenance');
  const restartPanel = root('restart-panel');
  const work = root('view-work');
  const extensions = root('view-extensions');
  const subagentHeading = root('subagents-heading', 'H2');
  const subagentView = root('view-subagents');
  subagentView.append(subagentHeading, root('sa-target-panel'));
  const settingsView = root('view-settings');
  settingsView.append(root('settings-heading', 'H2'), root('context-panel'));
  const toolsView = root('view-tools');
  toolsView.append(root('tools-heading', 'H2'), root('web-panel'));
  const translationView = root('view-translation');
  translationView.append(root('translation-panel'));
  const usagePane = root('usage-pane-requests');
  const recentSection = root('recent-section');
  const recentTable = root('recent-table', 'TABLE');
  recentSection.append(recentTable);
  const records = root('overview-records');
  records.append(recentSection);

  const code = [
    'const $ = id => document.getElementById(id);',
    sourceFunction('mountUsageRequests'),
    sourceFunction('mountWorkspace'),
    'return mountWorkspace;',
  ].join('\n');
  const mount = new Function('document', code)(document);

  mount();
  assert.equal(connectionsPanel.parent, modelsBody);
  assert.equal(restartPanel.parent, maintenance);
  assert.equal(byId.get('view-subagents'), undefined);
  assert.equal(byId.get('sa-target-panel').parent, work);
  assert.equal(byId.get('context-panel').parent, work);
  assert.equal(byId.get('web-panel').parent, work);
  assert.equal(byId.get('translation-panel').parent, extensions);
  assert.equal(byId.get('subagents-heading'), undefined);

  mount();
  mount();
  assert.deepEqual(usagePane.children.sort((a, b) => a.id.localeCompare(b.id)).map(node => node.id).filter(id => id === 'recent-section'), ['recent-section']);
  assert.equal(work.children.filter(node => node.id === 'context-panel').length, 1);
});

test('section links open every collapsed ancestor before scrolling to the panel', () => {
  const { makeNode, document } = makeDomTree();
  const details = makeNode('mcp-details', 'DETAILS');
  const panel = makeNode('mcp-panel');
  const heading = makeNode('mcp-heading', 'H2');
  panel.append(heading);
  details.append(panel);
  const view = makeNode('view-work');
  view.append(details);
  const code = [
    'const $ = id => document.getElementById(id);',
    sourceFunction('revealWorkspaceSection'),
    'return revealWorkspaceSection;',
  ].join('\n');
  const reveal = new Function('document', code)(document);
  assert.equal(details.open, false);
  const target = reveal('mcp-panel');
  assert.equal(target, panel);
  assert.equal(details.open, true);
  assert.equal(panel.scrolled, true);
  assert.equal(heading.focused, true);
  assert.equal(heading.attributes.tabindex, '-1');
});

function makeSaveEnv({ applySucceeds = true } = {}) {
  const calls = [];
  const state = {
    key: 'session', stateReadEpoch: 0, modelsSaving: false, lastSync: 0,
    data: { revision: 'rev-1', configuration: { state: 'ready' }, models: [], connections: { saved: [], restartRequired: false } },
    editor: { baseRevision: 'rev-1' },
    conns: {
      editing: { draft: { id: 'draft-conn', name: '草稿连接', baseURL: 'https://api.example.com/v1' }, errors: [] },
      draft: [{ id: 'saved-conn', name: '已保存', baseURL: 'https://saved.example.com/v1' }],
    },
  };
  const dom = {};
  const code = [
    sourceFunction('saveCatalogue'),
    sourceFunction('connectionPayload'),
    'const configReady = data => data.configuration?.state === "ready";',
    'const applyConnectionEditor = () => {',
    '  if (!applySucceeds) { state.conns.editing.errors = ["草稿无效"]; return; }',
    '  state.conns.draft = [...state.conns.draft, {...state.conns.editing.draft}];',
    '  state.conns.editing = null;',
    '};',
    'const showToast = (message, tone) => calls.push({ message, tone });',
    'const renderEditor = () => {};',
    'const renderModels = () => {};',
    'const renderActions = () => {};',
    'const adoptState = next => { state.data = next; };',
    'const renderAll = () => {};',
    'const handleSaveFailure = async error => calls.push({ message: error.message, tone: "error" });',
    'return saveCatalogue;',
  ].join('\n');
  const save = new Function('state', 'dom', 'API', 'request', 'calls', 'applySucceeds', code)(
    state, dom, { workspace: '/api/workspace' },
    async (path, options) => { calls.push({ path, body: options.body }); return { state: { ...state.data, revision: 'rev-2' } }; },
    calls, applySucceeds,
  );
  return { save, state, calls };
}

test('saving a model aborts when the connection drawer still has an invalid draft', async () => {
  const env = makeSaveEnv({ applySucceeds: false });
  const ok = await env.save([{ id: 'model-a' }]);
  assert.equal(ok, false);
  assert.equal(env.calls.some(call => call.path), false);
  assert.equal(env.state.conns.editing.errors.length, 1);
  assert.match(env.calls.at(-1).message, /连接编辑器/);
});

test('saving a model folds a valid connection draft into the same transaction', async () => {
  const env = makeSaveEnv({ applySucceeds: true });
  const ok = await env.save([{ id: 'model-a' }]);
  assert.equal(ok, true);
  const request = env.calls.find(call => call.path === '/api/workspace');
  assert.equal(request.body.revision, 'rev-1');
  assert.deepEqual(request.body.models, [{ id: 'model-a' }]);
  assert.deepEqual(request.body.connections.map(row => row.id), ['saved-conn', 'draft-conn']);
});

/* --- pricing and failover survive every save path ------------------------- */

/* buildModel 是模型行唯一的构造函数。定价与备用连接必须能从草稿一路带到
   提交的目录，否则「改个名字」就会把用户的定价与故障转移配置清空。 */
function makeModelDraft() {
  const code = [
    'const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;',
    "const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];",
    'const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;',
    'const MAX_EFFORTS = 12;',
    ...['modelFamilyFor', 'hasVision', 'parseEfforts', 'metadataSourceOf',
      'priceDraftFrom', 'failoverDraftFrom', 'buildPrice', 'buildFailover',
      'buildModel', 'canonicalPrice', 'canonicalFailover', 'canonicalModel'].map(sourceFunction),
    'return { buildModel, canonicalModel, priceDraftFrom, failoverDraftFrom };',
  ].join('\n');
  return new Function(code)();
}

/* 定价与备用连接有两条保存路径，两条都必须保住它们：

   1. 编辑器保存已有模型：未改动的字段不进提交行，由 core.MergeModels 按
      originalId 合并保留；改动过的字段随行提交，清空则显式提交 null 删除。
   2. 向导重存同一条模型：行是重建的，mergeOnboardModels 负责带回。

   只要有一条路径漏掉，改个名字就会把用户的定价与故障转移配置清空。 */
test('an untouched price is left out so the server merge keeps it', () => {
  const { buildModel } = makeModelDraft();
  const base = {
    id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000,
    connectionId: 'conn', pricing: { inputPerMillion: 0.5, outputPerMillion: 1.5 },
    failover: [{ connectionId: 'backup' }],
  };
  // 只改名字：草稿只带改名，定价与备用连接保持原值。
  const draft = {
    id: base.id, name: '新名字', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '',
    connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '0.5', outputPerMillion: '1.5', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [{ connectionId: 'backup', upstreamModelId: '', upstreamProtocol: '' }],
    baseEntity: base, touched: new Set(['name']),
  };
  const { model, errors } = buildModel(draft);
  assert.deepEqual(errors, []);
  // 值没变，因此不进提交行：服务端合并时原值仍在。
  assert.equal('pricing' in model, false);
  assert.equal('failover' in model, false);
  assert.deepEqual(model.name, '新名字');
});

test('a changed price and standby list are submitted with the row', () => {
  const { buildModel } = makeModelDraft();
  const base = {
    id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000,
    connectionId: 'conn', pricing: { inputPerMillion: 0.5, outputPerMillion: 1.5 }, failover: [],
  };
  const draft = {
    id: base.id, name: '', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '',
    connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '2', outputPerMillion: '6', cachedInputPerMillion: '0.5', cacheWritePerMillion: '' },
    failover: [{ connectionId: 'backup', upstreamModelId: 'backup-model', upstreamProtocol: 'anthropic' }],
    baseEntity: base, touched: new Set(),
  };
  const { model, errors } = buildModel(draft);
  assert.deepEqual(errors, []);
  assert.deepEqual(model.pricing, { inputPerMillion: 2, outputPerMillion: 6, cachedInputPerMillion: 0.5 });
  assert.deepEqual(model.failover, [{ connectionId: 'backup', upstreamModelId: 'backup-model', upstreamProtocol: 'anthropic' }]);
});

test('clearing a price submits null so the stored price is removed', () => {
  const { buildModel } = makeModelDraft();
  const base = {
    id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000,
    connectionId: 'conn', pricing: { inputPerMillion: 0.5, outputPerMillion: 1.5 }, failover: [],
  };
  const draft = {
    id: base.id, name: '', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '',
    connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], baseEntity: base, touched: new Set(),
  };
  const { model, errors } = buildModel(draft);
  assert.deepEqual(errors, []);
  assert.equal(model.pricing, null);
  assert.equal(model.failover, null);
});

test('a price with only one side is refused rather than half-priced', () => {
  const { buildModel } = makeModelDraft();
  const draft = {
    id: 'conn/resp/model-a', name: '', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '', connectionId: 'conn',
    upstreamModelId: '', upstreamProtocol: '', contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '1', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], baseEntity: null, touched: new Set(),
  };
  const { errors } = buildModel(draft);
  assert.equal(errors.some(text => text.includes('同时填写输入与输出')), true);
});

test('a standby cannot duplicate the primary connection or exceed the limit', () => {
  const { buildModel } = makeModelDraft();
  const make = failover => ({
    id: 'conn/resp/model-a', name: '', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '', connectionId: 'conn',
    upstreamModelId: '', upstreamProtocol: '', contextLimit: '', metadataSource: null,
    pricing: null, failover, baseEntity: null, touched: new Set(),
  });
  const same = buildModel(make([{ connectionId: 'conn', upstreamModelId: '' }]));
  assert.equal(same.errors.some(text => text.includes('不能与主连接相同')), true);

  const many = buildModel(make(Array.from({ length: 9 }, (_, index) => ({ connectionId: 'backup' + index }))));
  assert.equal(many.errors.some(text => text.includes('最多 8 条')), true);

  const duplicates = buildModel(make([{ connectionId: 'backup' }, { connectionId: 'backup' }]));
  assert.deepEqual(duplicates.errors, []);
  assert.equal(duplicates.model.failover.length, 1);
});

/* 向导保存只重新声明连接与模型能力，不该顺手清掉用户自己的定价。 */
test('the add-a-provider wizard keeps existing pricing and failover', () => {
  const code = [sourceFunction('mergeOnboardModels'), 'return mergeOnboardModels;'].join('\n');
  const merge = new Function(code)();
  const existing = [{
    id: 'conn/resp/model-a', connectionId: 'conn', contextWindow: 200000,
    pricing: { inputPerMillion: 2, outputPerMillion: 4 },
    failover: [{ connectionId: 'backup' }],
  }];
  const built = [{ id: 'conn/resp/model-a', connectionId: 'conn', contextWindow: 200000, reasoning: true }];
  const merged = merge(existing, built, 'conn');
  assert.deepEqual(merged[0].pricing, { inputPerMillion: 2, outputPerMillion: 4 });
  assert.deepEqual(merged[0].failover, [{ connectionId: 'backup' }]);
  assert.equal(merged[0].reasoning, true);
});

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const appFiles = ['./app-core.js', './app-overview.js', './app-models.js', './app-connections.js', './app-work.js', './app.js'];
const source = '\n' + appFiles.map(file => readFileSync(new URL(file, import.meta.url), 'utf8')).join('\n');
const growthSource = readFileSync(new URL('./growth.js', import.meta.url), 'utf8');
const htmlSource = readFileSync(new URL('./index.html', import.meta.url), 'utf8');

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

/* syncState / nativeConnectionText 现在第一步就问接入模式。直连辅助函数用真实实现，
   这样「没有 integrationMode」的旧数据必须原样走网关分支，也一并被这些用例守住。
   失败码归一化也在这里用真实代码，避免测试自己复制一张会漂移的映射表。 */
const DIRECT_REASON_SOURCE = (() => {
  const start = source.indexOf('const DIRECT_REASON = {');
  assert.notEqual(start, -1, 'missing DIRECT_REASON table');
  const end = source.indexOf('\n};', start);
  assert.notEqual(end, -1, 'unterminated DIRECT_REASON table');
  return source.slice(start, end + 3);
})();
const INTEGRATION_HELPERS = [DIRECT_REASON_SOURCE, ...['savedIntegrationMode', 'effectiveIntegrationMode', 'directModeOf',
  'directImportComplete', 'directRouteCleanupPending', 'directStaticHostsReady', 'directModeState',
  'normalizeReasonCode', 'directReasonText'].map(sourceFunction)];

function makeSyncState() {
  return new Function('fmtCount', 'String',
    [...INTEGRATION_HELPERS, sourceFunction('syncState'), 'return syncState;'].join('\n'))(
    (value) => String(value), String);
}

function makeNativeConnectionText() {
  return new Function([...INTEGRATION_HELPERS, sourceFunction('nativeConnectionText'), 'return nativeConnectionText;'].join('\n'))();
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
  const syncState = makeSyncState();
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

test('missing official Kimi has an explicit installation next step', () => {
  const syncState = makeSyncState();
  const verdict = syncState({ integration: { state: 'stopped', reason: 'not_installed', desktop: { state: 'stopped', reason: 'not_installed' } }, models: [] });
  assert.equal(verdict.code, 'not-installed');
  assert.match(verdict.note, /安装官方 Kimi/);
  assert.match(verdict.note, /已有登录和 Work 数据会保留/);
});

test('overview only promotes restart when it is needed', () => {
  const launch = { dataset: {}, setAttribute() {} };
  const renderLaunch = new Function('dom', `
    const ACTION_LABEL = {'open-work': '打开 Kimi Work', 'launch-kimi': '启动 Kimi Work', 'restart-kimi': '重启 Kimi'};
    const state = {};
    const isInFlight = () => false;
    const syncState = data => ({ code: data.sync });
    const setText = (element, text) => { element.textContent = text; };
    ${sourceFunction('renderLaunch')}
    return renderLaunch;
  `)({ onboardLaunch: launch });
  renderLaunch({ kimiProcess: { running: true }, integration: { desktop: { managed: true } }, sync: 'synced' });
  assert.equal(launch.dataset.action, 'open-work');
  renderLaunch({ kimiProcess: { running: true }, integration: { desktop: { managed: false } } });
  assert.equal(launch.dataset.action, 'restart-kimi');
  renderLaunch({ kimiProcess: { running: true }, sync: 'restart' });
  assert.equal(launch.dataset.action, 'restart-kimi');
  renderLaunch({ kimiProcess: { running: false } });
  assert.equal(launch.dataset.action, 'launch-kimi');
});

test('budget notice distinguishes reminders, refusals and disabled protection', () => {
  const notice = new Function(sourceFunction('budgetNotice') + '\nreturn budgetNotice;')();
  const budget = { enabled: true, spend: { daily: { reached: true, action: 'remind' }, monthly: { reached: true, action: 'block' } } };
  assert.match(notice(budget), /今日.*此上限仅提醒.*本月.*新请求会被拒绝/);
  assert.equal(notice({ ...budget, enabled: false }), '');
  assert.equal(notice({ ...budget, spend: { daily: { reached: false } } }), '');
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
    'const resetModelSignature = () => {}, resetRecentSignature = () => {}, resetSubagentSignatures = () => {}, resetIntegrationModeSignature = () => {};',
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

function rotatedKeyHarness(loadState, calls) {
  const state = { key: 'old-session', view: 'usage', onboard: { advanced: false, query: 'vision' } };
  const dom = { conn: {}, onboardKey: { value: 'old-key' }, onboardModelQuery: { value: 'vision' }, toast: { hidden: false }, shell: { hidden: false } };
  const usagePanel = { reset() { calls.push('reset'); }, show() { calls.push('show'); }, reload: async () => {} };
  const code = [
    'const resetModelSignature = () => {}, resetRecentSignature = () => {}, resetSubagentSignatures = () => {}, resetIntegrationModeSignature = () => {};',
    sourceFunction('adoptLocationKey'),
    'return adoptLocationKey;',
  ].join('\n');
  return new Function('state', 'dom', 'window', 'sessionStorage', 'history', 'KEY_STORE', 'stopPolling', 'setHidden', 'showGate', 'loadProviders', 'loadState', 'startPolling', 'usagePanel', code)(
    state, dom, { location: { hash: '#key=new-session', pathname: '/', search: '' } },
    { setItem: () => {} }, { replaceState: () => {} }, 'panel-key',
    () => calls.push('stop'), () => {}, () => {}, () => {}, loadState, () => calls.push('start'), usagePanel,
  );
}

test('adopting a rotated key clears usage before reading the new endpoint', async () => {
  const calls = [];
  const rotate = rotatedKeyHarness(async () => { calls.push('load'); return true; }, calls);
  assert.equal(rotate(), true);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(calls, ['reset', 'stop', 'load', 'show', 'start']);
});

test('a failed new endpoint read leaves usage cleared instead of showing old data', async () => {
  const calls = [];
  const rotate = rotatedKeyHarness(async () => { calls.push('load'); return false; }, calls);
  assert.equal(rotate(), true);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(calls, ['reset', 'stop', 'load']);
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
      insertBefore(node, reference) {
        /* 与真实 DOM 一致：先把节点从原位置摘下来，再按参照物的新下标插入。 */
        if (node.parent) node.parent.children.splice(node.parent.children.indexOf(node), 1);
        node.parent = this;
        const index = reference ? this.children.indexOf(reference) : -1;
        if (index < 0) this.children.push(node);
        else this.children.splice(index, 0, node);
        return node;
      },
      scrollIntoView() { this.scrolled = true; },
      focus() { this.focused = true; },
      hasAttribute(name) { return Object.hasOwn(this.attributes, name); },
      setAttribute(name, value) { this.attributes[name] = value; },
    };
    Object.defineProperty(node, 'parentElement', { get() { return this.parent; } });
    Object.defineProperty(node, 'parentNode', { get() { return this.parent; } });
    if (id) byId.set(id, node);
    return node;
  };
  const document = {
    getElementById: id => byId.get(id) || null,
    createElement: () => makeNode('', 'DIV'),
  };
  return { byId, makeNode, document };
}

test('mountUsageRequests is idempotent and keeps one recent-requests table', () => {
  const { makeNode, document } = makeDomTree();
  const root = (id, tag) => makeNode(id, tag);
  const usagePane = root('usage-pane-requests');
  const recentSection = root('recent-section');
  const recentTable = root('recent-table', 'TABLE');
  recentSection.append(recentTable);
  const records = root('overview-records');
  records.append(recentSection);

  const code = [
    'const $ = id => document.getElementById(id);',
    sourceFunction('mountUsageRequests'),
    'return mountUsageRequests;',
  ].join('\n');
  const mount = new Function('document', code)(document);

  mount();
  mount();
  assert.deepEqual(usagePane.children.map(node => node.id), ['recent-section']);
  assert.equal(records.children.length, 0);
});

test('index.html lays out six flat tabs and static views without runtime mountWorkspace', () => {
  for (const id of ['nav-overview', 'nav-models', 'nav-usage', 'nav-work', 'nav-extensions', 'nav-maintenance']) {
    assert.ok(htmlSource.includes(`id="${id}"`), 'missing top tab ' + id);
  }
  assert.equal(htmlSource.includes('id="work-index"'), false);
  assert.equal(htmlSource.includes('id="quick-models"'), false);
  assert.equal(htmlSource.includes('id="models-services"'), false);
  assert.equal(source.includes('function mountWorkspace('), false);
  const modelsIdx = htmlSource.indexOf('id="view-models"');
  const usageIdx = htmlSource.indexOf('id="view-usage"');
  const modelsSlice = htmlSource.slice(modelsIdx, usageIdx);
  assert.ok(modelsSlice.includes('id="connections-panel"'));
  assert.ok(modelsSlice.includes('id="restart-panel"'));
  assert.ok(modelsSlice.indexOf('id="model-table"') < modelsSlice.indexOf('id="connections-panel"'));
});

test('computeOverviewIssue prioritizes gateway, connection apply, budget, integration and recent failures', () => {
  const compute = new Function(`
    const displayModelId = id => id;
    const applyState = data => data._apply || { tone: 'ok', word: '已应用', note: '' };
    const budgetNotice = b => b?.msg || '';
    const syncState = data => data._sync || { code: 'synced', tone: 'ok', word: '已同步', note: '' };
    const requestErrorText = row => row.msg || row.error || '';
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('computeOverviewIssue')}
    return computeOverviewIssue;
  `)();
  assert.equal(compute({ gateway: { state: 'stopped' } }).action.view, 'maintenance');
  assert.equal(compute({ gateway: { state: 'running' }, _apply: { tone: 'warn', word: '等待应用', note: '待应用' } }).action.section, 'restart-panel');
  assert.equal(compute({ gateway: { state: 'running' }, budget: { msg: '今日已达预算上限。' } }).action.type, 'budget');
  assert.equal(compute({ gateway: { state: 'running' }, _sync: { code: 'restart', tone: 'warn', word: '需要重启', note: '重启' } }).action.action, 'restart-kimi');
  const failed = compute({ gateway: { state: 'running' }, metrics: { recent: [{ model: 'deepseek/r1', status: 401, error: 'upstream_unauthorized', msg: '密钥无效' }] } });
  assert.equal(failed.tone, 'bad');
  assert.match(failed.note, /deepseek\/r1：密钥无效/);
  assert.equal(compute({ gateway: { state: 'running' }, metrics: { recent: [{ model: 'm', status: 200 }] } }).action, null);
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
    ...['connectionRowsOf', 'connectionDraftRow', 'connectionDraftFrom',
      'connectionsSignature', 'savedConnectionsSignature'].map(sourceFunction),
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

/* 模型保存会把连接草稿整组提交，返回状态里的连接就是已提交的基线。
   这里用真实的 adoptState 与连接辅助函数，只替掉渲染、请求等外围依赖：
   旧实现让 adoptState 走「脏草稿」分支，把 base/baseRevision 留在提交前，
   界面继续显示「草稿尚未保存」，下一次连接保存还带旧 revision。 */
function makeCatalogueSaveEnv(serverStates, { onRequest = null } = {}) {
  const calls = [];
  const state = {
    key: 'session', stateReadEpoch: 0, stateReadSeq: 0, modelsSaving: false, connsSaving: false, lastSync: 0,
    data: {
      revision: 'rev-1', configuration: { state: 'ready' },
      models: [{ id: 'glm/chat/glm-5.3', connectionId: 'glm', name: 'GLM-5.3 Flash' }],
      connections: {
        saved: [
          { id: 'glm', name: 'GLM 官方', baseURL: 'https://open.bigmodel.cn/api/paas/v4', protocols: ['chat'], apiKeySet: true },
          { id: 'ds', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', protocols: ['chat'], apiKeySet: true },
        ],
        restartRequired: false,
      },
    },
    editor: { baseRevision: 'rev-1' },
    draft: null, draftDirty: false, draftBaseEntity: null, draftBaseRevision: null,
    conns: { base: null, baseRevision: null, baseEntity: null, dirty: false, editing: null, draft: null, errors: [] },
    services: { base: null, dirty: false, saving: false, searchKey: '', fetchKey: '' },
  };
  const dom = {};
  const code = [
    ...['saveCatalogue', 'saveConnections', 'connectionPayload', 'connectionDraftFrom',
      'connectionDraftRow', 'connectionRowsOf', 'connectionsSignature',
      'savedConnectionsSignature', 'connectionsDirty', 'adoptState'].map(sourceFunction),
    'const configReady = data => data.configuration?.state === "ready";',
    "const draftFromSaved = () => ({ mode: 'inherit', model: null, effort: null, models: [] });",
    'const recomputeDraftDirty = () => {};',
    'const onboardTouched = () => true;',
    'const fillOnboardForm = () => {};',
    'const searchAvailable = () => false;',
    'const settlePending = () => {};',
    'const setConnErrors = errors => { state.conns.errors = errors; };',
    'const renderConnections = () => {};',
    'const renderConnectionsDiff = () => {};',
    'const renderAll = () => {};',
    'const renderEditor = () => {};',
    'const renderModels = () => {};',
    'const renderActions = () => {};',
    'const showToast = (message, tone) => calls.push({ message, tone });',
    'const handleSaveFailure = async error => calls.push({ message: error.message, tone: "error" });',
    'return { saveCatalogue, saveConnections, adoptState, connectionDraftFrom, savedConnectionsSignature };',
  ].join('\n');
  const api = new Function('state', 'dom', 'API', 'request', 'calls', code)(
    state, dom, { workspace: '/api/workspace' },
    async (path, options) => {
      calls.push({ path, body: options.body });
      if (onRequest) onRequest(state);
      return { state: serverStates[Math.min(calls.filter(call => call.path).length - 1, serverStates.length - 1)] };
    },
    calls,
  );
  return { ...api, state, calls };
}

test('a model save adopts the committed connection baseline instead of keeping the draft dirty', async () => {
  const committed = {
    revision: 'rev-2', configuration: { state: 'ready' },
    models: [{ id: 'glm/chat/glm-5.3', connectionId: 'glm', name: 'GLM 新名' }],
    connections: {
      saved: [
        { id: 'glm', name: 'GLM 新名', baseURL: 'https://open.bigmodel.cn/api/paas/v4', protocols: ['chat'], apiKeySet: true },
        { id: 'ds', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', protocols: ['chat'], apiKeySet: true },
      ],
      restartRequired: true,
    },
  };
  const later = { ...committed, revision: 'rev-3' };
  const env = makeCatalogueSaveEnv([committed, later]);
  env.adoptState(env.state.data, { initial: true });
  assert.equal(env.state.conns.baseRevision, 'rev-1');

  /* 连接编辑器改过 name 并已并入草稿：草稿与已保存值不一致。 */
  env.state.conns.draft = env.state.conns.draft.map(row => (row.id === 'glm' ? { ...row, name: 'GLM 新名' } : row));
  env.state.conns.dirty = true;

  const ok = await env.saveCatalogue([{ id: 'glm/chat/glm-5.3', connectionId: 'glm', name: 'GLM 新名' }]);
  assert.equal(ok, true);
  const sent = env.calls.find(call => call.path === '/api/workspace');
  assert.equal(sent.body.revision, 'rev-1');
  assert.equal(sent.body.connections.find(row => row.id === 'glm').name, 'GLM 新名');

  assert.equal(env.state.conns.dirty, false);
  assert.equal(env.state.conns.baseRevision, 'rev-2');
  assert.equal(env.state.conns.base, env.savedConnectionsSignature(committed));
  assert.deepEqual(env.state.conns.draft, env.connectionDraftFrom(committed));

  /* 紧接着再改一条连接：提交必须带服务端最新 revision，失败时不带旧值。 */
  env.state.conns.draft = env.state.conns.draft.map(row => (row.id === 'ds' ? { ...row, name: 'DeepSeek 新名' } : row));
  await env.saveConnections();
  const second = env.calls.filter(call => call.path === '/api/workspace').at(-1);
  assert.equal(second.body.revision, 'rev-2');
});

/* 会话在请求途中被替换时，返回的状态属于旧会话：既不采纳新基线，
   也不清掉用户正在编辑的草稿。 */
test('a stale model save reply leaves the connection draft untouched', async () => {
  const committed = {
    revision: 'rev-2', configuration: { state: 'ready' }, models: [],
    connections: { saved: [{ id: 'glm', name: 'GLM 新名', baseURL: 'https://open.bigmodel.cn/api/paas/v4' }] },
  };
  const env = makeCatalogueSaveEnv([committed], { onRequest: (state) => { state.key = 'other-session'; } });
  env.adoptState(env.state.data, { initial: true });
  const baseBefore = env.state.conns.base;
  env.state.conns.draft = env.state.conns.draft.map(row => (row.id === 'glm' ? { ...row, name: '本地未保存' } : row));

  const ok = await env.saveCatalogue([{ id: 'glm/chat/glm-5.3', connectionId: 'glm', name: 'GLM 新名' }]);
  assert.notEqual(ok, true);
  assert.equal(env.state.conns.baseRevision, 'rev-1');
  assert.equal(env.state.conns.base, baseBefore);
  assert.equal(env.state.conns.draft.find(row => row.id === 'glm').name, '本地未保存');
  assert.equal(env.state.data.revision, 'rev-1');
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
  assert.equal(Object.hasOwn(model, 'failover'), false);
  draft.baseEntity = { ...base, failover: [{ connectionId: 'backup' }] };
  assert.equal(buildModel(draft).model.failover, null);
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

test('renderFailoverRows keeps the active input node and focus across re-renders', () => {
  const replaceCalls = [];
  const doc = { activeElement: null };
  function makeNode(tag) {
    const node = {
      tagName: tag.toUpperCase(),
      className: '',
      dataset: {},
      attributes: {},
      children: [],
      parentNode: null,
      value: '',
      disabled: false,
      textContent: '',
      selectionStart: 0,
      selectionEnd: 0,
      setAttribute(name, value) { this.attributes[name] = String(value); },
      append(...items) {
        for (const item of items) {
          item.parentNode = this;
          this.children.push(item);
        }
      },
      replaceChildren(...items) {
        if (this === container) replaceCalls.push(items.length);
        for (const old of this.children) old.parentNode = null;
        this.children = [];
        this.append(...items);
      },
      contains(target) {
        for (let cur = target; cur; cur = cur.parentNode) if (cur === this) return true;
        return false;
      },
      closest(selector) {
        const match = /^\[data-failover-index\]$/.test(selector)
          ? (n) => n.dataset && n.dataset.failoverIndex !== undefined
          : () => false;
        for (let cur = this; cur; cur = cur.parentNode) if (match(cur)) return cur;
        return null;
      },
      querySelector(selector) {
        const field = selector.match(/^\[data-failover-field="([^"]+)"\]$/)?.[1];
        const remove = selector === '[data-failover-remove]';
        const indexMatch = selector.match(/^\[data-failover-index="([^"]+)"\]\s+\[data-failover-field="([^"]+)"\]$/);
        if (indexMatch) {
          const row = this.children.find((child) => child.dataset?.failoverIndex === indexMatch[1]);
          return row ? row.querySelector(`[data-failover-field="${indexMatch[2]}"]`) : null;
        }
        const walk = (root) => {
          for (const child of root.children) {
            if (field && child.dataset?.failoverField === field) return child;
            if (remove && child.dataset?.failoverRemove !== undefined) return child;
            const nested = walk(child);
            if (nested) return nested;
          }
          return null;
        };
        return walk(this);
      },
      focus() { doc.activeElement = this; },
      setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; },
    };
    return node;
  }
  doc.createElement = makeNode;
  doc.createElementNS = (_ns, tag) => makeNode(tag);

  const container = makeNode('div');
  const countNode = makeNode('span');
  const addButton = makeNode('button');
  const state = {
    modelsSaving: false,
    data: {
      connections: {
        saved: [
          { id: 'primary', name: '主连接' },
          { id: 'backup', name: '备用连接' },
          { id: 'third', name: '第三连接' },
        ],
      },
    },
  };
  const dom = { mFailoverRows: container, mFailoverCount: countNode, mFailoverAdd: addButton };
  const render = new Function('document', 'state', 'dom', `
    ${sourceFunction('connectionRowsOf')}
    ${sourceFunction('iconUse')}
    ${sourceFunction('renderFailoverRows')}
    return renderFailoverRows;
  `)(doc, state, dom);

  const editor = {
    draft: {
      connectionId: 'primary',
      failover: [{ connectionId: 'backup', upstreamModelId: '', upstreamProtocol: '' }],
    },
  };
  render(editor);
  assert.equal(replaceCalls.length, 1);

  const firstInput = container.querySelector('[data-failover-field="upstreamModelId"]');
  assert.notEqual(firstInput, null);
  firstInput.focus();
  firstInput.value = 'l';
  firstInput.setSelectionRange(1, 1);
  editor.draft.failover[0].upstreamModelId = 'l';

  render(editor);
  assert.equal(replaceCalls.length, 1);
  assert.equal(container.querySelector('[data-failover-field="upstreamModelId"]'), firstInput);
  assert.equal(doc.activeElement, firstInput);
  assert.equal(firstInput.value, 'l');

  /* 结构变化（增删行或主连接切换）必须重建行，并在同槽位存在时恢复焦点与光标。 */
  editor.draft.failover.push({ connectionId: '', upstreamModelId: '', upstreamProtocol: '' });
  render(editor);
  assert.equal(replaceCalls.length, 2);
  const rebuiltInput = container.querySelector('[data-failover-index="0"] [data-failover-field="upstreamModelId"]');
  assert.equal(doc.activeElement, rebuiltInput);
  assert.equal(rebuiltInput.selectionStart, 1);
  assert.equal(rebuiltInput.selectionEnd, 1);
  assert.equal(countNode.textContent, '2 / 8');
});

test('an untouched model stays clean with reordered metadata and an empty standby list', () => {
  const { buildModel, canonicalModel } = makeModelDraft();
  const base = {
    id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000,
    connectionId: 'conn', metadataSource: { checkedAt: '2026-11-16', kind: 'discovery' },
    failover: [],
  };
  const draft = {
    id: base.id, name: '', context: '200000', output: '', tools: undefined,
    reasoning: false, vision: false, efforts: [], defaultEffort: '',
    connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: base.metadataSource,
    pricing: {}, failover: [], baseEntity: base, touched: new Set(),
  };
  const built = buildModel(draft);
  assert.equal(canonicalModel(built.candidate), canonicalModel(base));
  assert.deepEqual(built.touched, []);
  assert.equal(Object.hasOwn(built.model, 'metadataSource'), false);
  assert.equal(Object.hasOwn(built.model, 'failover'), false);
  assert.equal(canonicalModel({ ...base, failover: undefined }), canonicalModel(base));

  draft.metadataSource = { kind: 'manual', checkedAt: '2026-11-17' };
  draft.failover = [{ connectionId: 'backup' }];
  const changed = buildModel(draft);
  assert.deepEqual(changed.touched, ['failover', 'metadataSource']);
  assert.notEqual(canonicalModel(changed.candidate), canonicalModel(base));
});

test('rebaseEditor preserves touched pricing and failover edits', () => {
  const state = {
    data: {
      revision: 'rev-2',
      models: [{
        id: 'conn/chat/model-a',
        connectionId: 'conn',
        contextWindow: 128000,
        name: '服务端新名',
        pricing: { inputPerMillion: 1, outputPerMillion: 2 },
        failover: [{ connectionId: 'old-backup' }],
      }],
    },
    editor: null,
  };
  const code = [
    'const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;',
    "const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];",
    'const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;',
    'const MAX_EFFORTS = 12;',
    'const dom = {};',
    'const findModel = id => state.data.models.find(row => row.id === id) || null;',
    'const upstreamProtocolSupported = () => false;',
    'const fillEditorFields = () => {};',
    'const renderEditor = () => {};',
    'const showToast = () => {};',
    ...['modelFamilyFor', 'hasVision', 'parseEfforts', 'metadataSourceOf',
      'priceDraftFrom', 'failoverDraftFrom', 'buildPrice', 'buildFailover',
      'canonicalFailover', 'buildModel', 'modelDraftFrom', 'rebaseEditor'].map(sourceFunction),
    'return rebaseEditor;',
  ].join('\n');
  const rebase = new Function('state', code)(state);
  const base = {
    id: 'conn/chat/model-a',
    connectionId: 'conn',
    contextWindow: 128000,
    name: '旧名',
    pricing: { inputPerMillion: 1, outputPerMillion: 2 },
    failover: [{ connectionId: 'old-backup' }],
  };
  state.editor = {
    originalId: base.id,
    baseRevision: 'rev-1',
    conflict: true,
    serverErrors: ['conflict'],
    draft: {
      id: base.id,
      name: '旧名',
      context: '128000',
      output: '',
      tools: undefined,
      reasoning: false,
      vision: false,
      efforts: [],
      defaultEffort: '',
      connectionId: 'conn',
      upstreamModelId: '',
      upstreamProtocol: '',
      upstreamProtocolChoices: null,
      contextLimit: '',
      metadataSource: null,
      pricing: { inputPerMillion: '3', outputPerMillion: '9', cachedInputPerMillion: '', cacheWritePerMillion: '' },
      failover: [{ connectionId: 'backup', upstreamModelId: 'model-b', upstreamProtocol: '' }],
      baseEntity: base,
      touched: new Set(['pricing', 'failover']),
    },
  };
  rebase();
  assert.equal(state.editor.draft.name, '服务端新名');
  assert.deepEqual(state.editor.draft.pricing, { inputPerMillion: '3', outputPerMillion: '9', cachedInputPerMillion: '', cacheWritePerMillion: '' });
  assert.deepEqual(state.editor.draft.failover, [{ connectionId: 'backup', upstreamModelId: 'model-b', upstreamProtocol: '' }]);
});

/* ---------- 设置页顺序、原始码展示与刷新联动 ---------- */

test('the daily connection panels stay above the recovery toggles and run records', () => {
  const { byId, makeNode, document } = makeDomTree();
  const maintenance = makeNode('view-maintenance');
  const nativePanel = makeNode('native-panel');
  const updatePanel = makeNode('update-panel');
  const roleContract = makeNode('role-contract');
  const diagnostics = makeNode('sa-diagnostics-body');
  diagnostics.append(roleContract);
  const advancedToggle = makeNode('ob-advanced-toggle', 'BUTTON');
  const advancedBody = makeNode('ob-advanced');
  const records = makeNode('overview-records', 'DETAILS');
  const restartPanel = makeNode('restart-panel');
  /* 真实页面里两个面板原本就在恢复项之前，是 relocate 把它们挪到末尾的；
     这里按「已被挪到末尾」的错位状态构造，验证修复会把它们放回去。 */
  maintenance.append(advancedToggle, advancedBody, records, restartPanel, nativePanel, updatePanel);

  const code = [
    'const $ = id => document.getElementById(id);',
    growingFunction('relocate'),
    growingFunction('mountLegacy'),
    'return mountLegacy;',
  ].join('\n');
  const mount = new Function('document', code)(document);
  mount();
  mount();

  const order = byId.get('view-maintenance').children.map(node => node.id);
  assert.ok(order.indexOf('native-panel') < order.indexOf('ob-advanced-toggle'));
  assert.ok(order.indexOf('update-panel') < order.indexOf('ob-advanced-toggle'));
  assert.ok(order.indexOf('native-panel') < order.indexOf('overview-records'));
  assert.ok(order.indexOf('update-panel') < order.indexOf('overview-records'));
  assert.equal(order.filter(id => id === 'native-panel').length, 1);
  assert.ok(diagnostics.children.some(node => node.id === 'role-contract'));
});

function growingFunction(name) {
  return growthFunction(name);
}

test('the runtime subagent mode is named in Chinese, not as a raw inherit', () => {
  const render = sourceFunction('renderSubagents');
  const line = render.split('\n').find(row => row.includes('dom.rtSub'));
  assert.notEqual(line, undefined, 'renderSubagents must publish the runtime summary');
  assert.match(line, /POLICY_MODE_WORD\[runtime\.mode\]/);
  assert.doesNotMatch(line, /\[runtime\.mode,/);
});

test('boot reads the session key before any view mounts', () => {
  const boot = sourceFunction('boot');
  const keyAt = boot.indexOf('state.key = readKey()');
  const mountAt = boot.indexOf('initView()');
  assert.notEqual(keyAt, -1);
  assert.notEqual(mountAt, -1);
  assert.ok(keyAt < mountAt, 'the session key must be ready before the view mounts');
});

/* 系统直接启动的 Kimi（managed=false）不能被说成受管进程已加载配置，
   也不能被当成首次使用让用户重复点启动。 */
test('an unmanaged Kimi started from the Dock is not reported as a managed load', () => {
  const syncState = makeSyncState();
  const nativeConnectionText = makeNativeConnectionText();
  const base = {
    kimiProcess: { running: true },
    integration: {
      applied: true,
      readback: { accountVerified: true },
      desktop: { state: 'stopped', managed: false, reason: 'managed_child_exited_application-exit' },
      imported: true,
      nativeImport: {},
    },
  };
  const verdict = syncState(base);
  assert.equal(verdict.code, 'synced');
  assert.doesNotMatch(verdict.note, /受管进程已加载/);
  assert.match(verdict.note, /未受本产品托管/);
  assert.match(verdict.note, /正常退出并重启 Kimi/);

  const text = nativeConnectionText(base);
  assert.match(text, /未受本产品托管/);
  assert.match(text, /正常退出并重启 Kimi/);
  assert.doesNotMatch(text, /首次使用/);

  /* 受管进程仍在时，原来说明必须保持不变。 */
  const managed = { ...base, integration: { ...base.integration, adopted: true, desktop: { state: 'ready', managed: true } } };
  assert.match(syncState(managed).note, /受管进程已加载对应配置代次/);
});

test('displayModelId decodes percent-encoded route IDs while falling back safely on malformed escapes', () => {
  const fn = new Function(sourceFunction('displayModelId') + '\nreturn displayModelId;')();
  assert.equal(fn('magpie/kimi-group%2Fglm-5-3-flash'), 'magpie/kimi-group/glm-5-3-flash');
  assert.equal(fn('magpie/kimi-group/glm-5-3-flash'), 'magpie/kimi-group/glm-5-3-flash');
  assert.equal(fn('bad%ZZescape'), 'bad%ZZescape');
  assert.equal(fn(''), '');
});

test('reportTheme emits Wails host theme events and masthead drag excludes interactive controls', () => {
  const sent = [];
  const doc = { title: '' };
  const webkit = { messageHandlers: { external: { postMessage: (msg) => sent.push(msg) } } };
  const runner = new Function(
    'document', 'globalThis',
    'const THEME_PREFIX = "kimi-work-plus:theme:";\n' +
    'const THEME_TOKENS = Object.freeze({ light: "light", dark: "dark", auto: "auto" });\n' +
    sourceFunction('postWailsMessage') + '\n' +
    sourceFunction('reportTheme') + '\n' +
    sourceFunction('isMastheadDragTarget') + '\n' +
    'return { reportTheme, isMastheadDragTarget };'
  )(doc, { webkit });
  runner.reportTheme('dark');
  runner.reportTheme('light');
  runner.reportTheme('auto');
  assert.deepEqual(sent, [
    'wails:event:emit:kwp:theme:dark',
    'wails:event:emit:kwp:theme:light',
    'wails:event:emit:kwp:theme:auto',
  ]);
  const blankMasthead = { closest: (sel) => sel === '.masthead' ? {} : null };
  const buttonInMasthead = { closest: (sel) => (sel === '.masthead' || sel.includes('button')) ? {} : null };
  assert.equal(runner.isMastheadDragTarget(blankMasthead), true);
  assert.equal(runner.isMastheadDragTarget(buttonInMasthead), false);
});

test('renderCompatibility distinguishes verified, new version pass, and degraded guards', () => {
  const nodes = {};
  for (const id of [
    'overview-compat-bar', 'compat-dot', 'compat-summary', 'compat-version', 'compat-note',
    'diag-compat-dot', 'diag-compat-version', 'diag-compat-kernel',
    'diag-compat-catalog', 'diag-compat-catalog-sub',
    'diag-compat-apis', 'diag-compat-apis-sub',
    'diag-compat-store', 'diag-compat-guards',
  ]) {
    nodes[id] = { id, textContent: '', hidden: false, dataset: {} };
  }
  const $ = (id) => nodes[id] || null;
  const setDot = (el, tone) => { if (el) el.dataset.tone = tone; };
  const setHidden = (el, flag) => { if (el) el.hidden = Boolean(flag); };
  const setText = (el, text) => { if (el) el.textContent = String(text ?? ''); };
  const fmtCount = (v) => String(v);
  const fmtTime = () => '12:00:00';
  const reasonText = (c) => c;
  const fn = new Function(
    '$', 'setDot', 'setHidden', 'setText', 'fmtCount', 'fmtTime', 'reasonText',
    sourceFunction('compatToneOf') + '\n' +
    sourceFunction('compatStoreLabel') + '\n' +
    sourceFunction('renderCompatibility') + '\n' +
    'return renderCompatibility;'
  )($, setDot, setHidden, setText, fmtCount, fmtTime, reasonText);

  fn({
    compatibility: {
      status: 'verified',
      summary: '兼容',
      officialVersion: '3.2.28',
      verifiedVersion: true,
      verifiedVersions: ['3.2.27', '3.2.28'],
      kernelVersion: '1.0.14',
      kernelCommit: '303f4196e',
      checks: {
        catalog: { status: 'pass', modelCount: 5, acceptedCount: 5 },
        accountApi: { status: 'pass', source: 'preload', getUserProfile: true, byokSourcesList: true },
        storeFormat: { status: 'pass', format: 'accounts' },
      },
      affectedFeatures: [],
      guards: { modelSyncWriteBlocked: false, accountSwitchWriteBlocked: false },
    },
  });
  assert.equal(nodes['overview-compat-bar'].dataset.tone, 'ok');
  assert.equal(nodes['compat-summary'].textContent, '兼容');
  assert.equal(nodes['compat-version'].textContent, 'Kimi 3.2.28');
  assert.match(nodes['diag-compat-store'].textContent, /accounts/);

  fn({
    compatibility: {
      status: 'new_version_ok',
      summary: '新版本，基础检查通过',
      officialVersion: '3.2.99',
      verifiedVersion: false,
      versionChanged: true,
      previousVersion: '3.2.28',
      checks: {
        catalog: { status: 'pass', modelCount: 5, acceptedCount: 5 },
        accountApi: { status: 'pass', source: 'preload', getUserProfile: true, byokSourcesList: true },
        storeFormat: { status: 'pass', format: 'accounts' },
      },
      affectedFeatures: [],
      guards: { modelSyncWriteBlocked: false, accountSwitchWriteBlocked: false },
    },
  });
  assert.equal(nodes['overview-compat-bar'].dataset.tone, 'warn');
  assert.equal(nodes['compat-summary'].textContent, '新版本，基础检查通过');
  assert.match(nodes['compat-note'].textContent, /3\.2\.28.*3\.2\.99/);

  fn({
    compatibility: {
      status: 'degraded',
      summary: '模型同步不可用',
      officialVersion: '3.3.0',
      verifiedVersion: false,
      checks: {
        catalog: { status: 'fail', reason: 'models_skipped' },
        accountApi: { status: 'pass', source: 'preload', getUserProfile: true, byokSourcesList: true },
        storeFormat: { status: 'fail', format: 'unknown' },
      },
      affectedFeatures: [{ feature: 'model_sync', label: '模型同步不可用', writeBlocked: true }],
      guards: { modelSyncWriteBlocked: true, accountSwitchWriteBlocked: false },
    },
  });
  assert.equal(nodes['overview-compat-bar'].dataset.tone, 'bad');
  assert.equal(nodes['compat-summary'].textContent, '模型同步不可用');
  assert.match(nodes['compat-note'].textContent, /已停止受影响功能的写入/);
  assert.match(nodes['diag-compat-guards'].textContent, /已停止模型同步写入/);
});

/* ---------- Wave 2B：零网关直连模式 ---------- */

test('direct mode reports the unsynced, ready, and zero-gateway states without a restart warning', () => {
  const syncState = makeSyncState();
  const models = [{ id: 'conn/resp/model-a' }, { id: 'conn/chat/model-b' }];
  const eligibleModelIds = ['conn/resp/model-a', 'conn/chat/model-b'];
  const imported = {
    mode: 'direct', state: 'ready', applied: true, adopted: true,
    aliases: { 'conn/resp/model-a': 'byok-uuid/model-a', 'conn/chat/model-b': 'byok-uuid/model-b' },
  };

  /* 已选直连但官方 Kimi 还没写入：无论 Kimi 是否运行，下一步都是同步。 */
  const pending = syncState({
    integrationMode: 'direct',
    models,
    integration: { mode: 'gateway', reason: 'direct_import_required' },
  });
  assert.equal(pending.code, 'pending');
  assert.equal(pending.tone, 'warn');
  assert.equal(pending.word, '待写入直连配置');
  assert.match(pending.note, /同步到 Work/);
  assert.doesNotMatch(pending.note, /重启/);

  /* 只要 integration.mode 还不是 direct，就必须说「待写入」。 */
  assert.equal(syncState({ integrationMode: 'direct', models, integration: {} }).word, '待写入直连配置');

  /* 已同步且静态 hosts 就绪：直连已就绪，不需要后台服务。 */
  const ready = syncState({
    integrationMode: 'direct',
    models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds, eligibleModels: 2, totalModels: 2, readyForZeroGateway: false },
    integration: imported,
  });
  assert.equal(ready.code, 'ready');
  assert.equal(ready.tone, 'ok');
  assert.equal(ready.word, '直连已就绪');
  assert.match(ready.note, /退出本软件及后台服务/);

  /* 用户直接双击官方 Kimi（managed=false）时，直连本来就不依赖受管进程。 */
  const unmanaged = syncState({
    integrationMode: 'direct',
    models,
    kimiProcess: { running: true },
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds, eligibleModels: 2, totalModels: 2, readyForZeroGateway: true },
    integration: { ...imported, desktop: { managed: false } },
  });
  assert.equal(unmanaged.code, 'ready');
  assert.equal(unmanaged.tone, 'ok');
  assert.equal(unmanaged.word, '直连运行中（免开本软件）');
  assert.match(unmanaged.note, /无需保持 KimiWork \+ 运行/);
  assert.doesNotMatch(unmanaged.note, /正常退出并重启/);
  assert.doesNotMatch(unmanaged.note, /需要重启/);
  assert.doesNotMatch(unmanaged.note, /未受本产品托管/);
});

test('skipped direct models still count as synced once the eligible set is imported', () => {
  const syncState = makeSyncState();
  const integration = {
    mode: 'direct', state: 'ready', applied: true, adopted: true,
    aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' },
  };
  const base = {
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/model-a' }, { id: 'conn/resp/no-tools' }],
    integration,
  };
  /* eligibleModelIds 少于模型总数：被跳过的模型不能让界面永远停在「待写入」。 */
  const verdict = syncState({
    ...base,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds: ['conn/resp/model-a'],
      eligibleModels: 1, totalModels: 2, skippedModels: [{ id: 'conn/resp/no-tools', reason: 'tools_required' }],
      readyForZeroGateway: true },
  });
  assert.equal(verdict.code, 'ready');
  assert.doesNotMatch(verdict.word, /待写入/);

  /* 同一个模型数但上游不同：别名数达标不代表要导入的 ID 都在。 */
  const swapped = syncState({
    ...base,
    integration: { ...integration, aliases: { 'conn/resp/other-model': 'byok-uuid/other-model' } },
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds: ['conn/resp/model-a'],
      eligibleModels: 1, totalModels: 2 },
  });
  assert.equal(swapped.code, 'pending');
  assert.equal(swapped.word, '待写入直连配置');
});

test('a ready direct alias set without applied evidence is still pending', () => {
  const syncState = makeSyncState();
  const models = [{ id: 'conn/resp/model-a' }];
  const directMode = { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds: ['conn/resp/model-a'],
    eligibleModels: 1, totalModels: 1 };
  /* applied 代表官方 store 与生成的 config 都读到了；只有 ready+adopted 不够。 */
  const adoptedOnly = syncState({
    integrationMode: 'direct', models, directMode,
    integration: { mode: 'direct', state: 'ready', adopted: true, applied: false,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  });
  assert.equal(adoptedOnly.code, 'pending');
  assert.equal(adoptedOnly.word, '待写入直连配置');

  /* 旧后端没有 eligibleModelIds：readback.modelAliases 覆盖了这次 spec 也算完成。 */
  const legacy = syncState({
    integrationMode: 'direct', models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' },
      readback: { modelAliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } } },
  });
  assert.equal(legacy.code, 'ready');

  /* 旧后端的 readback 指向别的模型：不能凭别名数量放行。 */
  const legacySwapped = syncState({
    integrationMode: 'direct', models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/other-model': 'byok-uuid/other-model' },
      readback: { modelAliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } } },
  });
  assert.equal(legacySwapped.code, 'pending');
});

test('a synced direct import without static hosts asks for one more sync', () => {
  const syncState = makeSyncState();
  const data = {
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/model-a' }],
    directMode: { mode: 'direct', supported: true, staticHostsReady: false, eligibleModelIds: ['conn/resp/model-a'],
      eligibleModels: 1, totalModels: 1, readyForZeroGateway: false },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  };
  const verdict = syncState(data);
  /* 模型写进去了但 hosts 还没补齐：不能报「已就绪」，否则用户会直接关掉后台。 */
  assert.equal(verdict.code, 'pending');
  assert.equal(verdict.tone, 'warn');
  assert.equal(verdict.word, '待补齐静态 hosts');
  assert.match(verdict.note, /再点一次「同步到 Work」/);
  assert.doesNotMatch(verdict.note, /退出本软件/);

  /* directMode 存在时 setup.dns.state === 'hosts' 不是证据：
     Windows 非 macOS 的 setup 默认就是 hosts，拿它回退等于跳过真实检查。 */
  const viaSetup = syncState({ ...data, setup: { dns: { state: 'hosts' } } });
  assert.equal(viaSetup.code, 'pending');
  assert.equal(viaSetup.word, '待补齐静态 hosts');
});

test('without a directMode row the legacy setup.dns answer still counts', () => {
  const syncState = makeSyncState();
  const verdict = syncState({
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/model-a' }],
    setup: { dns: { state: 'hosts' } },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  });
  assert.equal(verdict.code, 'ready');
  assert.equal(verdict.word, '直连已就绪');
});

test('direct mode with no eligible models names the real blocker', () => {
  const syncState = makeSyncState();
  const verdict = syncState({
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/no-tools' }],
    directMode: { mode: 'direct', supported: true, staticHostsReady: false, eligibleModels: 0, totalModels: 1,
      skippedModels: [{ id: 'conn/resp/no-tools', reason: 'tools_required' }] },
    integration: { mode: 'direct', state: 'ready', adopted: true },
  });
  assert.equal(verdict.code, 'pending');
  assert.equal(verdict.word, '没有可直连的模型');
  assert.match(verdict.note, /工具调用/);
});

test('gateway mode wording is untouched by the direct-mode switch', () => {
  const syncState = makeSyncState();
  /* 老数据没有 integrationMode / directMode：必须逐字保留原结论。 */
  assert.equal(syncState({ models: [] }).word, '等待模型');
  assert.equal(syncState({ models: [{ id: 'a' }], integration: { state: 'synchronized', applied: true, adopted: true } }).word, '已核对目录');
  assert.equal(syncState({ models: [{ id: 'a' }], integration: { applied: true, adopted: true, aliases: { a: 'a' } } }).word, '已保存，待接入');
  assert.equal(syncState({ integrationMode: 'gateway', models: [] }).code, 'empty');
});

test('a successful direct sync is a ready state, not an overview problem', () => {
  const compute = new Function(`
    const displayModelId = id => id;
    const applyState = data => data._apply || { tone: 'ok', word: '已应用', note: '' };
    const budgetNotice = b => b?.msg || '';
    const requestErrorText = row => row.msg || row.error || '';
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('syncState')}
    ${sourceFunction('computeOverviewIssue')}
    return computeOverviewIssue;
  `)();
  const models = [{ id: 'conn/resp/model-a' }];
  const readyData = {
    integrationMode: 'direct',
    models,
    gateway: { state: 'stopped' },
    directMode: { mode: 'direct', supported: true, staticHostsReady: true,
      eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  };
  const issue = compute(readyData);
  /* 直连正常时网关停着不是故障，也不能落进「接入与恢复」。 */
  assert.equal(issue.tone, 'ok');
  assert.equal(issue.title, '状态正常');
  assert.equal(issue.action, null);

  /* 网关模式原有判定不变：网关停了照旧报错。 */
  assert.equal(compute({ gateway: { state: 'stopped' } }).title, '本机网关未运行');
  /* 直连未写入时仍然要说出下一步，不能被当成正常。 */
  const pending = compute({ ...readyData, gateway: { state: 'running' }, integration: { mode: 'gateway' } });
  assert.equal(pending.title, '待写入直连配置');

  const cleanup = compute({ ...readyData, directMode: { ...readyData.directMode, routeCleanupPending: true } });
  assert.equal(cleanup.title, '切换收尾未完成');
  assert.match(cleanup.note, /不要退出后台服务/);
  assert.doesNotMatch(cleanup.note, /模型请求当前无法发出/);

  /* 版本不支持时，重起网关解决不了：概览要说版本前提而不是网关报错。 */
  const unsupported = compute({
    ...readyData,
    integration: { mode: 'gateway' },
    directMode: { ...readyData.directMode, supported: false },
  });
  assert.equal(unsupported.title, '当前官方 Kimi 不支持直连');
  assert.match(unsupported.note, /3\.2\.28\+/);
});

test('route cleanup pending never reads as a finished switch', () => {
  const syncState = makeSyncState();
  const models = [{ id: 'conn/resp/model-a' }];
  const directMode = { mode: 'direct', supported: true, staticHostsReady: true,
    eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1 };
  const integration = { mode: 'direct', state: 'ready', applied: true, adopted: true,
    aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } };

  /* 三条证据分别都能判定收尾未完成。 */
  const variants = [
    { ...directMode, routeCleanupPending: true },
    directMode,
    directMode,
  ];
  const integrations = [
    integration,
    { ...integration, nativeImport: { routeCleanupPending: true } },
    { ...integration, nativeImport: { reason: 'route_cleanup_pending' } },
  ];
  for (let i = 0; i < variants.length; i += 1) {
    const verdict = syncState({ integrationMode: 'direct', models, directMode: variants[i], integration: integrations[i] });
    assert.equal(verdict.code, 'pending', 'variant ' + i + ' must not report ready');
    assert.equal(verdict.word, '切换收尾未完成');
    assert.match(verdict.note, /旧的服务商来源仍保留/);
    assert.match(verdict.note, /再点一次「同步到 Work」/);
    assert.match(verdict.note, /不要退出后台服务/);
    assert.match(verdict.note, /旧路由已清理/);
    assert.doesNotMatch(verdict.note, /直连已就绪|切换全部完成/);
  }

  /* 收尾未完成不是整个导入失败：文案不能说成失败。 */
  const verdict = syncState({ integrationMode: 'direct', models, directMode: { ...directMode, routeCleanupPending: true }, integration });
  assert.doesNotMatch(verdict.note, /失败/);

  /* 回执还停在网关走法、但收尾标记已在：这时也不能退回「待写入」，
     因为配置确实已经写入，只差清理。 */
  const gatewayMode = syncState({
    integrationMode: 'direct',
    models,
    directMode: { ...directMode, routeCleanupPending: true },
    integration: { mode: 'gateway' },
  });
  assert.equal(gatewayMode.word, '切换收尾未完成');

  /* 没有收尾标记时仍是正常 ready，不能被这条判断顺手拖成 pending。 */
  assert.equal(syncState({ integrationMode: 'direct', models, directMode, integration }).code, 'ready');
});

test('an unsupported official version is named instead of asking for another sync', () => {
  const syncState = makeSyncState();
  const verdict = syncState({
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/model-a' }],
    directMode: { mode: 'direct', supported: false, staticHostsReady: false, eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'gateway' },
  });
  assert.equal(verdict.code, 'unsupported');
  assert.equal(verdict.tone, 'bad');
  assert.match(verdict.word, /不支持直连/);
  assert.match(verdict.note, /3\.2\.28\+/);
  assert.match(verdict.note, /本机网关/);
  assert.doesNotMatch(verdict.note, /同步到 Work/);
});

test('catalog-size and removed-model failures name the next step without claiming success', () => {
  const fn = new Function(
    [DIRECT_REASON_SOURCE, sourceFunction('normalizeReasonCode'), sourceFunction('directReasonText'),
      'return directReasonText;'].join('\n'),
  )();

  const tooLarge = fn('direct_catalog_too_large');
  assert.match(tooLarge, /减少单次导入的模型/);
  assert.match(tooLarge, /拆分连接/);
  assert.match(tooLarge, /本次未写入/);
  assert.doesNotMatch(tooLarge, /direct_catalog_too_large/);

  const removed = fn('models_removed');
  assert.match(removed, /重新同步到 Work/);
  assert.match(removed, /已经没有/);
  assert.doesNotMatch(removed, /models_removed/);

  const cleanup = fn('route_cleanup_pending');
  assert.match(cleanup, /已写入并读回/);
  assert.match(cleanup, /重试清理/);
  /* 收尾待清理不是导入失败。 */
  assert.doesNotMatch(cleanup, /导入失败|写入失败/);

  /* 旧前缀与连字符形式继续兼容。 */
  assert.equal(fn('native_import_route-cleanup-pending'), cleanup);
  assert.equal(fn('native_direct_mode_unsupported'), fn('direct_mode_unsupported'));
});

function makeWorkspaceSyncEnv() {
  const nodes = {};
  for (const id of ['workspace-sync-state', 'workspace-sync-note', 'workspace-restart']) {
    nodes[id] = { id, textContent: '', hidden: false };
  }
  const code = [
    ...INTEGRATION_HELPERS,
    'const $ = id => nodes[id] || null;',
    'const setText = (element, text) => { if (element) element.textContent = text; };',
    'const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };',
    sourceFunction('renderWorkspaceSync'),
    'return renderWorkspaceSync;',
  ].join('\n');
  const render = new Function('nodes', code)(nodes);
  return { render, nodes };
}

test('the connection line names the unfinished cleanup and the catalog failures', () => {
  const directText = makeNativeConnectionText();
  const directBase = {
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/model-a' }],
    directMode: { mode: 'direct', supported: true, staticHostsReady: true,
      eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  };
  const cleanupText = directText({
    ...directBase,
    integration: { ...directBase.integration, nativeImport: { reason: 'route_cleanup_pending', routeCleanupPending: true } },
  });
  assert.match(cleanupText, /切换收尾未完成/);
  assert.match(cleanupText, /旧的服务商来源还保留/);
  assert.match(cleanupText, /不要退出后台服务/);
  assert.doesNotMatch(cleanupText, /已写入官方 Kimi 且静态 hosts 就绪/);

  const readyText = directText(directBase);
  assert.match(readyText, /静态 hosts 就绪/);
  assert.doesNotMatch(readyText, /切换收尾未完成/);

  /* 目录过大与模型残留各自给出下一步，且不把码原样抛给用户。 */
  const tooLarge = directText({ ...directBase, integration: { ...directBase.integration, reason: 'direct_catalog_too_large' } });
  assert.match(tooLarge, /拆分连接/);
  assert.doesNotMatch(tooLarge, /direct_catalog_too_large/);
  const removed = directText({ ...directBase, integration: { ...directBase.integration, reason: 'models_removed' } });
  assert.match(removed, /重新同步/);
  assert.doesNotMatch(removed, /models_removed/);
});

test('the workspace bar and the gateway fact both show an unfinished route cleanup', () => {
  const { render, nodes } = makeWorkspaceSyncEnv();
  const models = [{ id: 'conn/resp/model-a' }];
  const base = {
    integrationMode: 'direct',
    models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true,
      eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  };
  for (const data of [
    { ...base, directMode: { ...base.directMode, routeCleanupPending: true } },
    { ...base, integration: { ...base.integration, nativeImport: { routeCleanupPending: true } } },
    { ...base, integration: { ...base.integration, nativeImport: { reason: 'route_cleanup_pending' } } },
  ]) {
    render(data);
    assert.equal(nodes['workspace-sync-state'].textContent, '切换收尾未完成');
    assert.match(nodes['workspace-sync-note'].textContent, /旧的服务商来源还保留/);
    assert.match(nodes['workspace-sync-note'].textContent, /不要退出后台服务/);
    assert.match(nodes['workspace-sync-note'].textContent, /旧路由已清理/);
    /* 收尾未完成时不能提示「需要重启 Kimi」把人带去别的动作。 */
    assert.equal(nodes['workspace-restart'].hidden, true);
  }

  /* 没有收尾标记时依旧是原来的已同步文案。 */
  render(base);
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Work');

  const fact = new Function(
    [...INTEGRATION_HELPERS,
      'const el = { textContent: "", hidden: true };',
      'const dom = { factGatewayMode: el };',
      'const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };',
      'const setText = (element, text) => { if (element) element.textContent = text; };',
      sourceFunction('renderGatewayModeFact'),
      'return { renderGatewayModeFact, el };'].join('\n'),
  )();
  fact.renderGatewayModeFact({ ...base, directMode: { ...base.directMode, routeCleanupPending: true } });
  assert.match(fact.el.textContent, /切换收尾未完成/);
  assert.match(fact.el.textContent, /暂不要退出后台/);
  fact.renderGatewayModeFact(base);
  assert.match(fact.el.textContent, /退出后台后第三方模型仍可用/);
});

test('the models sync bar names the pending direct write and the gateway fallback', () => {
  const { render, nodes } = makeWorkspaceSyncEnv();
  const models = [{ id: 'conn/resp/model-a' }, { id: 'conn/chat/model-b' }];

  /* 直连已选、官方 Kimi 还是网关：说明下一步是写入直连配置。 */
  render({ integrationMode: 'direct', models, integration: { mode: 'gateway' } });
  assert.equal(nodes['workspace-sync-state'].textContent, '待写入直连配置');
  assert.match(nodes['workspace-sync-note'].textContent, /服务商地址与静态 hosts 写入官方 Kimi/);
  assert.match(nodes['workspace-sync-note'].textContent, /之后可退出本软件/);

  /* 切回网关、官方 Kimi 仍是直连：说明下一步是恢复网关路由。 */
  render({ integrationMode: 'gateway', models, integration: { mode: 'direct' } });
  assert.equal(nodes['workspace-sync-state'].textContent, '待切回网关并同步');
  assert.match(nodes['workspace-sync-note'].textContent, /恢复为本机网关路由/);

  /* 直连已同步并跳过了一个模型：仍然显示已同步，并强调可以退出后台。 */
  render({
    integrationMode: 'direct',
    models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModelIds: ['conn/resp/model-a'],
      eligibleModels: 1, totalModels: 2,
      skippedModels: [{ id: 'conn/chat/model-b', reason: 'missing_api_key' }] },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  });
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Work');
  assert.match(nodes['workspace-sync-note'].textContent, /静态 hosts 就绪/);
  assert.match(nodes['workspace-sync-note'].textContent, /仍可直接使用这些模型/);

  /* 直连模型已写入但静态 hosts 还没就绪：明说还差一步。 */
  render({
    integrationMode: 'direct',
    models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: false,
      eligibleModelIds: ['conn/resp/model-a', 'conn/chat/model-b'], eligibleModels: 2, totalModels: 2 },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a', 'conn/chat/model-b': 'byok-uuid/model-b' } },
  });
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Work');
  assert.match(nodes['workspace-sync-note'].textContent, /补齐静态 hosts/);

  /* 网关模式原有四种文案不得变化。 */
  render({ models: [], integration: {} });
  assert.equal(nodes['workspace-sync-state'].textContent, '尚未添加模型');
  render({ models, integration: { adopted: true, aliases: { 'conn/resp/model-a': 'a', 'conn/chat/model-b': 'b' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '已同步到 Work');
  render({ models, integration: { applied: true, adopted: false, aliases: { 'conn/resp/model-a': 'a', 'conn/chat/model-b': 'b' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '需要重启 Kimi');
  assert.equal(nodes['workspace-restart'].hidden, false);
});

test('direct failure codes normalize across native_import_ prefixes and dashed page codes', () => {
  const fn = new Function(
    [DIRECT_REASON_SOURCE, sourceFunction('normalizeReasonCode'), sourceFunction('directReasonText'),
      'return { normalizeReasonCode, directReasonText };'].join('\n'),
  )();
  assert.equal(fn.normalizeReasonCode('native_import_direct-preview-refused'), 'direct_preview_refused');
  assert.equal(fn.normalizeReasonCode('native_direct_mode_unsupported'), 'direct_mode_unsupported');
  assert.equal(fn.normalizeReasonCode('direct-preview-refused'), 'direct_preview_refused');
  assert.equal(fn.normalizeReasonCode('direct_preview_refused'), 'direct_preview_refused');
  assert.equal(fn.normalizeReasonCode(''), '');
  assert.equal(fn.normalizeReasonCode(undefined), '');

  /* 三种写法指向同一句中文，且都不直接把诊断码摆给用户。 */
  const preview = fn.directReasonText('native_import_direct-preview-refused');
  assert.equal(preview, fn.directReasonText('direct-preview-refused'));
  assert.equal(preview, fn.directReasonText('direct_preview_refused'));
  assert.match(preview, /拒绝/);
  assert.match(preview, /重新同步/);
  assert.doesNotMatch(preview, /direct_preview_refused/);

  const unsupported = fn.directReasonText('native_direct_mode_unsupported');
  assert.match(unsupported, /不支持原生直连/);
  assert.match(unsupported, /本机网关/);

  /* 主会话要收敛的失败码都要有明确下一步。 */
  const cases = {
    direct_gateway_disconnect_failed: /仍指向本机网关/,
    direct_disconnect_failed: /原来的路由/,
    direct_import_failed: /重新同步/,
    direct_foreign_source: /核对现有来源/,
    direct_mode_unsupported: /本机网关/,
    direct_preview_refused: /重新同步/,
  };
  for (const [code, pattern] of Object.entries(cases)) {
    const text = fn.directReasonText(code);
    assert.match(text, pattern, code + ' must explain the next step');
    assert.doesNotMatch(text, new RegExp(code), code + ' must not leak the raw code');
    /* 旧后端可能再包一层前缀、把下划线换成连字符。 */
    assert.equal(fn.directReasonText('native_import_' + code.replace(/_/g, '-')), text);
  }

  /* 断开失败不能说成「旧路由已断开」。 */
  for (const code of ['direct_gateway_disconnect_failed', 'direct_disconnect_failed', 'gateway_disconnect_failed']) {
    const text = fn.directReasonText(code);
    assert.match(text, /可能仍/, code + ' must not claim the old route is gone');
    assert.doesNotMatch(text, /已经断开|已断开|不会再走/, code + ' must not claim success');
  }

  /* 真正未知的码不能被伪装成已知错误。 */
  assert.equal(fn.directReasonText('direct_some_new_failure'), '');
  assert.equal(fn.directReasonText('native_import_import-failed'), fn.directReasonText('import_failed'));
});

test('stripModelAliasPrefix removes gateway and UUID source prefixes but not custom names', () => {
  const fn = new Function(
    'MODEL_ALIAS_PREFIXES', 'MODEL_ALIAS_UUID_PREFIX',
    sourceFunction('stripModelAliasPrefix') + '\nreturn stripModelAliasPrefix;',
  )(['byok-kth-kimi/', 'byok-kth-resp/'], /^byok-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\//i);

  assert.equal(fn('byok-kth-kimi/conn/chat/model-a'), 'conn/chat/model-a');
  assert.equal(fn('byok-kth-resp/conn/resp/model-a'), 'conn/resp/model-a');
  assert.equal(fn('byok-3d8250d1-1234-5678-9abc-def012345678/conn/chat/model-a'), 'conn/chat/model-a');
  /* 用户自己的 byok- 命名不是别名，必须原样保留。 */
  assert.equal(fn('byok-custom/conn/chat/model-a'), 'byok-custom/conn/chat/model-a');
  assert.equal(fn('conn/chat/model-a'), 'conn/chat/model-a');
  assert.equal(fn(''), '');
  assert.equal(fn(undefined), undefined);
  /* 前缀后面为空时不能把整个名字吞掉。 */
  assert.equal(fn('byok-kth-kimi/'), 'byok-kth-kimi/');
});

test('resolveAlias unwraps prefixed aliases and falls back to the bare route id', () => {
  const state = { data: { models: [
    { id: 'conn/chat/model-a' },
    { id: 'conn/resp/model-b' },
    { id: 'byok-custom/conn/chat/model-c' },
  ] } };
  const code = [
    'const MODEL_ALIAS_PREFIXES = ["byok-kth-kimi/", "byok-kth-resp/"];',
    'const MODEL_ALIAS_UUID_PREFIX = /^byok-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\\//i;',
    'const displayModelId = value => value;',
    sourceFunction('stripModelAliasPrefix'),
    sourceFunction('modelIndex'),
    sourceFunction('modelIdFromAlias'),
    sourceFunction('resolveAlias'),
    'return { resolveAlias, modelIdFromAlias };',
  ].join('\n');
  const { resolveAlias, modelIdFromAlias } = new Function('state', code)(state);

  assert.equal(resolveAlias('byok-kth-kimi/conn/chat/model-a'), 'conn/chat/model-a');
  assert.equal(resolveAlias('byok-kth-resp/conn/resp/model-b'), 'conn/resp/model-b');
  assert.equal(resolveAlias('byok-3d8250d1-1234-5678-9abc-def012345678/conn/chat/model-a'), 'conn/chat/model-a');
  /* 目录里没有的别名也要去掉前缀再显示，而不是把 UUID 摆给用户。 */
  assert.equal(resolveAlias('byok-3d8250d1-1234-5678-9abc-def012345678/conn/chat/gone'), 'conn/chat/gone');
  /* byok-custom/ 是真实存在的模型 ID，不能被当成别名剥离。 */
  assert.equal(resolveAlias('byok-custom/conn/chat/model-c'), 'byok-custom/conn/chat/model-c');
  assert.equal(modelIdFromAlias('byok-3d8250d1-1234-5678-9abc-def012345678/conn/resp/model-b'), 'conn/resp/model-b');
  assert.equal(modelIdFromAlias('byok-custom/conn/chat/nope'), null);
  assert.equal(resolveAlias(''), '');
});

test('direct mode keeps budget reminders out of the overview issue card', () => {
  const compute = new Function(`
    const displayModelId = id => id;
    const applyState = data => data._apply || { tone: 'ok', word: '已应用', note: '' };
    const budgetNotice = b => b?.msg || '';
    const syncState = data => data._sync || { code: 'synced', tone: 'ok', word: '已同步', note: '' };
    const requestErrorText = row => row.msg || row.error || '';
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('computeOverviewIssue')}
    return computeOverviewIssue;
  `)();
  const budget = { enabled: true, spend: { daily: { reached: true, action: 'block' } }, msg: '今日已达预算上限。' };
  /* 直连下预算保护不生效：不能报「新请求会被拒绝」。 */
  const direct = compute({ integrationMode: 'direct', gateway: { state: 'running' }, budget });
  assert.doesNotMatch(direct.title, /预算/);
  assert.doesNotMatch(direct.note, /预算/);
  /* 网关模式下原行为不变。 */
  assert.equal(compute({ gateway: { state: 'running' }, budget }).title, '已达到预算上限');
});

test('the settings notice and sync button both name the pending cleanup', () => {
  const nodes = {};
  for (const id of ['direct-mode-notice', 'direct-mode-support', 'direct-mode-hosts', 'native-sync-label']) {
    nodes[id] = { id, textContent: '', hidden: false, dataset: {}, classList: { add() {}, toggle() {} } };
  }
  const dom = {
    modeGateway: { value: 'gateway', checked: false, disabled: false },
    modeDirect: { value: 'direct', checked: false, disabled: false },
    integrationModeGroup: null,
    integrationModeState: null,
    nativeSyncLabel: nodes['native-sync-label'],
    nativeSync: null,
    directModeNotice: nodes['direct-mode-notice'],
    directModeSupport: nodes['direct-mode-support'],
    directModeHosts: nodes['direct-mode-hosts'],
    directModeBypassed: null,
    directModeSkipped: null,
  };
  const code = [
    ...INTEGRATION_HELPERS,
    'let integrationModeSaving = false;',
    'const integrationModeBusy = data => integrationModeSaving || data?.operation?.state === "running";',
    'const setText = (element, text) => { if (element) element.textContent = text; };',
    'const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };',
    'const displayModelId = id => id;',
    'const DIRECT_BYPASS_WORD = {};',
    'const DIRECT_SKIP_REASON = {};',
    sourceFunction('renderIntegrationMode'),
    'return renderIntegrationMode;',
  ].join('\n');
  const render = new Function('dom', code)(dom);

  const models = [{ id: 'conn/resp/model-a' }];
  const base = {
    integrationMode: 'direct',
    models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true,
      eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1, routeCleanupPending: true },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } },
  };
  render(base);
  assert.equal(nodes['direct-mode-notice'].hidden, false);
  assert.equal(nodes['direct-mode-notice'].dataset.tone, 'warn');
  assert.match(nodes['direct-mode-support'].textContent, /切换收尾未完成/);
  assert.match(nodes['direct-mode-hosts'].textContent, /重试清理旧来源/);
  assert.match(nodes['direct-mode-hosts'].textContent, /不要退出后台服务/);
  /* 按钮说的也是重试清理，不是「写入直连配置」。 */
  assert.equal(nodes['native-sync-label'].textContent, '重试清理旧来源并同步');

  /* 收尾完成后回到原来的说明与按钮文案。 */
  render({ ...base, directMode: { ...base.directMode, routeCleanupPending: false } });
  assert.equal(nodes['direct-mode-notice'].dataset.tone, 'neutral');
  assert.match(nodes['direct-mode-support'].textContent, /直连模型已写入/);
  assert.equal(nodes['native-sync-label'].textContent, '写入直连配置并同步');

  /* 版本不支持时仍是原来的告警说明。 */
  render({ ...base, directMode: { ...base.directMode, supported: false, routeCleanupPending: false } });
  assert.equal(nodes['direct-mode-notice'].dataset.tone, 'warn');
  assert.match(nodes['direct-mode-support'].textContent, /不支持原生直连/);
});

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
const DIAG_REASON_SOURCE = (() => {
  const start = source.indexOf('const DIAG_REASON = {');
  assert.notEqual(start, -1, 'missing DIAG_REASON table');
  const end = source.indexOf('\n};', start);
  assert.notEqual(end, -1, 'unterminated DIAG_REASON table');
  return source.slice(start, end + 3);
})();
const DIRECT_REASON_SOURCE = (() => {
  const start = source.indexOf('const DIRECT_REASON = {');
  assert.notEqual(start, -1, 'missing DIRECT_REASON table');
  const end = source.indexOf('\n};', start);
  assert.notEqual(end, -1, 'unterminated DIRECT_REASON table');
  return source.slice(start, end + 3);
})();
const INTEGRATION_HELPERS = [DIAG_REASON_SOURCE, DIRECT_REASON_SOURCE, ...['modelTakeoverRemoved', 'savedIntegrationMode', 'effectiveIntegrationMode', 'directModeOf',
  'directImportComplete', 'directRouteCleanupPending', 'nativeRestartReceipt', 'directStaticHostsReady', 'directModeState',
  'directRestartRequired', 'normalizeReasonCode', 'directReasonText', 'reasonText'].map(sourceFunction)];

function makeSyncState() {
  return new Function('fmtCount', 'String',
    [...INTEGRATION_HELPERS, sourceFunction('syncState'), 'return syncState;'].join('\n'))(
    (value) => String(value), String);
}

function makeNativeConnectionText() {
  return new Function([...INTEGRATION_HELPERS, sourceFunction('nativeConnectionText'), 'return nativeConnectionText;'].join('\n'))();
}

function makeDirectRestartRequired() {
  return new Function([...INTEGRATION_HELPERS, 'return directRestartRequired;'].join('\n'))();
}

function growthFunction(name, indent = '') {
  return extractFunction(growthSource, name, indent);
}

test('a completed model-source disconnect offers a real re-takeover path', () => {
  const syncState = makeSyncState();
  const removed = {
    integrationMode: 'direct', models: [{ id: 'model-1' }],
    integration: { state: 'disabled', imported: false },
    operation: { action: 'disconnect-models', state: 'removed' },
  };
  const restored = syncState(removed);
  assert.equal(restored.code, 'restored');
  assert.match(restored.word, /可重新同步/);
  assert.match(restored.note, /本地服务商和模型仍保留/);
  const blockedRestored = syncState({
    ...removed,
    compatibility: { guards: { modelSyncWriteBlocked: true } },
  });
  assert.equal(blockedRestored.code, 'restored');
  assert.match(blockedRestored.note, /暂不可重新同步/);
  const connectionText = makeNativeConnectionText()(removed);
  assert.match(connectionText, /已移除本产品接管的模型来源/);
  assert.match(connectionText, /可重新接管/);
  const afterOtherOp = makeNativeConnectionText()({
    integrationMode: 'gateway',
    integration: { state: 'disabled', reason: 'disconnected', imported: false },
    operation: { action: 'check-compatibility', state: 'done' },
  });
  assert.match(afterOtherOp, /已移除本产品接管的模型来源/);
  for (const key of ['model_disconnect_deferred', 'model_disconnect_apply_failed', 'model_disconnect_byok_source_stale']) {
    assert.match(source, new RegExp(key + ':'));
  }
  const synced = syncState({
    integrationMode: 'direct', models: [{ id: 'model-1' }],
    integration: { state: 'ready', imported: true },
    operation: { action: 'disconnect-models', state: 'removed' },
  });
  assert.notEqual(synced.code, 'restored');
  assert.equal((htmlSource.match(/data-action="disconnect-models"/g) || []).length, 2);
  assert.equal(source.includes('window.confirm'), false, 'production JS must not call window.confirm');
  assert.match(htmlSource, /id="workspace-disconnect-confirm"/);
  assert.match(htmlSource, /id="native-disconnect-confirm"/);
});

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
    ${sourceFunction('credentialApplyReason')}
    ${sourceFunction('isManagedBackend')}
    ${sourceFunction('applyAvailability')}
    ${sourceFunction('applyState')}
    ${sourceFunction('renderApply')}
    return renderApply;
  `)(state, dom);
  const data = { connections: { restartRequired: false, apply: { state: 'failed', reason: 'credentials_unavailable' } } };
  render(data);
  assert.equal(dom.applyButton.hidden, false);
  assert.equal(dom.applyButton.textContent, '重新授权并应用连接');
  /* 待授权不是 Key 丢失：文案要说清条目还在、需要系统授权。 */
  assert.equal(dom.applyVerdictWord.textContent, '连接凭据待授权');
  assert.match(dom.applyVerdictNote.textContent, /仍在系统钥匙串里/);
  assert.match(dom.applyVerdictNote.textContent, /允许一次即可迁移/);
  assert.match(dom.applyVerdictNote.textContent, /后续升级不再需要钥匙串授权/);
  assert.doesNotMatch(dom.applyVerdictNote.textContent, /重新输入|密钥未保存/);
  assert.equal(dom.restartHead.textContent, '由当前窗口向系统申请读取已保存的连接凭据，后台不会自动弹出授权。');
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
    'const currentConnectionLimit = () => undefined;',
    'const existingOnboardRouteIds = () => new Set();',
    ...['modelFamilyFor', 'routeIdFor', 'protocolList', 'connectionProtocolLimits', 'preferredProtocol',
      'positiveInteger', 'buildOnboardRow', 'onboardConnectionProtocols', 'normalizeBaseURL',
      'protocolLabel', 'onboardProblems'].map(sourceFunction),
    'const providerPreset = () => null;',
    'return onboardProblems;',
  ].join('\n');
  return new Function('state', code)({ data: { models: [] } });
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
    ...['isInputActive', 'connectionRowsOf', 'savedOnboardTarget', 'onboardTouched', 'fillOnboardForm'].map(sourceFunction),
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

test('onboard discovery inherits missing metadata from the selected preset', async () => {
  const state = {
    data: { revision: 'rev', models: [], connections: { saved: [] } },
    onboard: { busy: false, seq: 0, discovery: null },
    providers: { presets: [{ id: 'preset', name: 'Preset', models: [{ id: 'model-a', name: 'Preset A', contextWindow: 262144, maxOutputTokens: 8192, reasoning: true, reasoningEfforts: ['low', 'high'], inputModalities: ['text', 'image'], source: { kind: 'preset', checkedAt: '2026-11-23' } }] }] },
  };
  const dom = {
    onboardId: { value: 'conn' }, onboardName: { value: 'Connection' },
    onboardBase: { value: 'https://api.example.com/v1' }, onboardKey: { value: 'synthetic' },
    onboardPreset: { value: 'preset' }, onboardErrors: {}, onboardDiscoverState: {},
  };
  const sourceHelpers = (() => {
    const start = source.indexOf('const SOURCE_STAMP_PATTERN');
    const end = source.indexOf('/* 预设目录在 providers.json', start);
    return source.slice(start, end);
  })();
  const code = [
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    'const PROTOCOL_CHOICES = ["chat", "responses"];',
    sourceHelpers,
    ...['normalizeBaseURL', 'onboardConnectionValues', 'connectionRowsOf', 'savedOnboardRow', 'discoveryBody',
      'onboardEndpointKey', 'syncOnboardEndpoint', 'protocolList', 'preferredProtocol', 'connectionProtocolLimits',
      'protocolChoicesFor', 'providerPresets', 'providerPreset', 'presetOnboardModels', 'mergeOnboardPresetMetadata',
      'onboardRowSource', 'onboardRowFrom', 'preserveOnboardModelNames', 'discoverOnboardModels'].map(sourceFunction),
    'return discoverOnboardModels;',
  ].join('\n');
  const discover = new Function('state', 'dom', 'API', 'request', 'setHidden', 'setText', 'showToast', 'fmtCount', 'discoveryReasonText', 'renderOnboard', 'clearOnboardSaveErrors', code)(
    state, dom, { discovery: '/api/discovery' }, async () => ({ discovery: {
      state: 'passed', modelCount: 1, checkedAt: '2026-11-23T01:00:00Z',
      models: [{ id: 'model-a', protocols: ['chat'], source: { checkedAt: '2026-11-23' } }],
    } }), () => {}, () => {}, () => {}, String, () => '', () => {}, () => {},
  );
  await discover();
  const row = state.onboard.discovery.models[0];
  assert.equal(row.contextWindow, 262144);
  assert.equal(row.maxOutputTokens, 8192);
  assert.equal(row.reasoning, true);
  assert.deepEqual(row.efforts, ['low', 'high']);
  assert.equal(row.vision, true);
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
  const dual = [
    { connectionId: 'myconn', upstreamModelId: 'model-a', protocol: 'chat', name: 'Chat name', defaultEffort: 'low' },
    { connectionId: 'myconn', upstreamModelId: 'model-a', protocol: 'responses', name: 'Responses name', defaultEffort: 'high' },
  ];
  const separated = preserve([
    { upstreamModelId: 'model-a', protocol: 'responses', name: 'Upstream', efforts: ['low', 'high'] },
    { upstreamModelId: 'model-a', protocol: 'chat', name: 'Upstream', efforts: ['low', 'high'] },
  ], dual, 'myconn');
  assert.equal(separated[0].name, 'Responses name');
  assert.equal(separated[0].defaultEffort, 'high');
  assert.equal(separated[1].name, 'Chat name');
  assert.equal(separated[1].defaultEffort, 'low');
});

test('saved onboarding rows keep the default effort for edit and resave', () => {
  const code = [
    'const PROTOCOL_CHOICES = ["chat", "responses"];',
    ...['positiveInteger', 'protocolList', 'preferredProtocol', 'protocolChoicesFor', 'providerPresets',
      'presetProtocolsFor', 'declaredProtocolsFor', 'savedOnboardModelRow'].map(sourceFunction),
    'return savedOnboardModelRow;',
  ].join('\n');
  const build = new Function('state', code)({ providers: { presets: [] } });
  const row = build({
    id: 'conn/resp/model-a', upstreamModelId: 'model-a', name: 'Model A', protocol: 'responses',
    contextWindow: 100000, contextLimit: 200000, reasoning: true, reasoningEfforts: ['low', 'high'],
    defaultEffort: 'high', inputModalities: ['text'], metadataSource: { kind: 'manual' },
  }, null, null);
  assert.equal(row.efforts.includes('high'), true);
  assert.equal(row.defaultEffort, 'high');
  assert.equal(row.name, 'Model A');
  /* 已保存的上下文上限必须回到草稿，重新保存时不会被当成「清空」。 */
  assert.equal(row.contextLimit, 200000);
});

test('rediscovery keeps a saved default effort only while the catalogue declares it', () => {
  const preserve = new Function(sourceFunction('preserveOnboardModelNames') + '\nreturn preserveOnboardModelNames;')();
  const saved = [{ connectionId: 'myconn', upstreamModelId: 'model-a', defaultEffort: 'high' }];
  const rows = preserve([
    { upstreamModelId: 'model-a', efforts: ['low', 'high'] },
    { upstreamModelId: 'model-b', efforts: ['low'] },
  ], saved, 'myconn');
  assert.equal(rows[0].defaultEffort, 'high');
  assert.equal(rows[1].defaultEffort, undefined);
  const dropped = preserve([{ upstreamModelId: 'model-a', efforts: ['low'] }], saved, 'myconn');
  assert.equal(dropped[0].defaultEffort, undefined);
});

test('onboard model drafts preserve selected thinking efforts and the default effort', () => {
  const code = [
    'const currentConnectionLimit = () => undefined;',
    ...['routeIdFor', 'positiveInteger', 'buildOnboardRow'].map(sourceFunction),
    'return buildOnboardRow;',
  ].join('\n');
  const build = new Function(code)();
  const model = build({
    connectionId: 'conn', upstreamModelId: 'model-a', protocol: 'responses', contextWindow: 262144,
    reasoning: true, efforts: ['low', 'high'], defaultEffort: 'high', vision: false,
  });
  assert.deepEqual(model.reasoningEfforts, ['low', 'high']);
  assert.equal(model.defaultEffort, 'high');
});

test('provider preset models normalize both preset and detail shapes', () => {
  const code = [
    sourceFunction('providerPresets'),
    sourceFunction('providerPreset'),
    sourceFunction('presetOnboardModels'),
    'return presetOnboardModels;',
  ].join('\n');
  const state = { providers: { presets: [], details: { p: { source: { url: 'https://example.invalid/models', checkedAt: '2026-11-23' }, models: [
    { upstreamModelId: 'model-a', label: 'Model A', contextWindow: 1000 },
  ] } } } };
  const models = new Function('state', code)(state)('p');
  assert.equal(models[0].id, 'model-a');
  assert.equal(models[0].upstreamModelId, 'model-a');
  assert.equal(models[0].name, 'Model A');
  assert.deepEqual(models[0].source, state.providers.details.p.source);
});

test('recommended preset models keep names, context, output, capabilities and efforts', () => {
  const state = {
    data: { models: [], connections: { saved: [] } },
    onboard: {},
    providers: { details: { p: { models: [{
      upstreamModelId: 'model-a', label: 'Model A', contextWindow: 1000, maxOutputTokens: 500,
      tools: true, reasoning: true, inputModalities: ['text', 'image'],
      reasoningEfforts: ['low', 'high'], source: { url: 'https://example.invalid/docs', checkedAt: '2026-11-23' },
    }] } } },
  };
  const dom = {
    onboardId: { value: '' }, onboardName: { value: '' }, onboardBase: { value: '' },
    onboardKey: { value: '' }, onboardPreset: { value: 'p' },
  };
  const sourceHelpers = (() => {
    const start = source.indexOf('const SOURCE_STAMP_PATTERN');
    const end = source.indexOf('/* 预设目录在 providers.json', start);
    return source.slice(start, end);
  })();
  const code = [
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    'const PROTOCOL_CHOICES = ["chat", "responses"];',
    sourceHelpers,
    ...['normalizeBaseURL', 'onboardConnectionValues', 'connectionRowsOf', 'savedOnboardRow',
      'onboardEndpointKey', 'protocolList', 'preferredProtocol', 'connectionProtocolLimits',
      'protocolChoicesFor', 'providerPresets', 'providerPreset', 'presetOnboardModels', 'presetMetadataSource',
      'onboardRowSource', 'onboardRowFrom', 'useRecommendedOnboardModels'].map(sourceFunction),
    'const renderOnboard = () => {};',
    'const clearOnboardSaveErrors = () => {};',
    'return useRecommendedOnboardModels;',
  ].join('\n');
  const use = new Function('state', 'dom', 'showToast', code)(state, dom, () => {});
  const models = use();
  assert.equal(models.length, 1);
  const row = state.onboard.discovery.models[0];
  assert.equal(row.id, 'model-a');
  assert.equal(row.name, 'Model A');
  assert.equal(row.contextWindow, 1000);
  assert.equal(row.maxOutputTokens, 500);
  assert.equal(row.tools, true);
  assert.equal(row.reasoning, true);
  assert.equal(row.vision, true);
  assert.deepEqual(row.efforts, ['low', 'high']);
  assert.equal(row.metadataKind, 'preset');
  assert.equal(row.sourceURL, 'https://example.invalid/docs');
  assert.equal(row.checkedAt, '2026-11-23');
});

test('upstream discovery values win over preset metadata and unknown efforts are not invented', () => {
  const SOURCE_SOURCE = (() => {
    const start = source.indexOf('const SOURCE_STAMP_PATTERN');
    assert.notEqual(start, -1, 'missing SOURCE_STAMP_PATTERN');
    const end = source.indexOf('/* 预设目录在 providers.json', start);
    assert.notEqual(end, -1, 'unterminated source helpers');
    return source.slice(start, end);
  })();
  const merge = new Function([SOURCE_SOURCE, sourceFunction('mergeOnboardPresetMetadata'), 'return mergeOnboardPresetMetadata;'].join('\n'))();
  const preset = {
    name: 'Preset A', contextWindow: 262144, maxOutputTokens: 512, tools: true, reasoning: true,
    inputModalities: ['text', 'image'], reasoningEfforts: ['low', 'high'], description: 'preset note',
    /* 真实 providers.json 的 preset.source 没有 kind。 */
    source: { url: 'https://api-docs.example.invalid/list-models', checkedAt: '2026-11-23' },
  };
  const live = merge({
    id: 'model-a', name: 'Live A', contextWindow: 1000, tools: false, reasoningEfforts: [],
    inputModalities: ['text'], source: { url: 'https://upstream.example.invalid/v1/models', checkedAt: '2026-11-24' },
  }, preset);
  assert.equal(live.name, 'Live A');
  assert.equal(live.contextWindow, 1000);
  assert.equal(live.maxOutputTokens, 512);
  assert.equal(live.tools, false);
  assert.equal(live.reasoning, true);
  assert.deepEqual(live.inputModalities, ['text']);
  assert.deepEqual(live.reasoningEfforts, []);
  /* 被补全字段的来源只能是预设文档：kind=preset，URL 不能是上游 /models。 */
  assert.equal(live.metadataSource.kind, 'preset');
  assert.equal(live.metadataSource.url, 'https://api-docs.example.invalid/list-models');
  assert.equal(live.metadataSource.checkedAt, '2026-11-23');
  const unknown = merge({ id: 'model-b' }, { ...preset, reasoningEfforts: 'unknown' });
  assert.equal(unknown.reasoningEfforts, undefined);
  const oversized = merge({ id: 'model-c', contextWindow: 1000 }, { ...preset, maxOutputTokens: 8192 });
  assert.equal(oversized.maxOutputTokens, undefined);
});

test('onboard draft names and effort choices are editable without leaving a stale default', () => {
  const helpers = new Function([
    sourceFunction('setOnboardModelName'),
    sourceFunction('toggleOnboardModelEffort'),
    sourceFunction('setOnboardModelDefaultEffort'),
    'return { setOnboardModelName, toggleOnboardModelEffort, setOnboardModelDefaultEffort };',
  ].join('\n'))();
  const row = { reasoning: true, efforts: ['low', 'high'], defaultEffort: 'high', name: '' };
  /* 输入过程中保留原样空白，提交构建时才 trim，避免打断 IME 组合。 */
  assert.equal(helpers.setOnboardModelName(row, '  My A  '), true);
  assert.equal(row.name, '  My A  ');
  helpers.toggleOnboardModelEffort(row, 'high');
  assert.deepEqual(row.efforts, ['low']);
  assert.equal(row.defaultEffort, undefined);
  helpers.toggleOnboardModelEffort(row, 'xhigh');
  assert.deepEqual(row.efforts, ['low', 'xhigh']);
  helpers.setOnboardModelDefaultEffort(row, 'xhigh');
  assert.equal(row.defaultEffort, 'xhigh');
  const build = new Function([
    'const currentConnectionLimit = () => undefined;',
    ...['routeIdFor', 'positiveInteger', 'buildOnboardRow'].map(sourceFunction), 'return buildOnboardRow;',
  ].join('\n'))();
  const saved = build({
    connectionId: 'conn', upstreamModelId: 'model-a', name: row.name, protocol: 'chat',
    contextWindow: 1000, reasoning: true, efforts: row.efforts, defaultEffort: row.defaultEffort,
  });
  assert.equal(saved.name, 'My A');
  assert.deepEqual(saved.reasoningEfforts, ['low', 'xhigh']);
  assert.equal(saved.defaultEffort, 'xhigh');
  helpers.setOnboardModelDefaultEffort(row, 'max');
  assert.equal(row.defaultEffort, undefined);
  row.reasoning = false;
  assert.equal(helpers.toggleOnboardModelEffort(row, 'low'), false);
});

test('diagnostics export note allows a missing operation log and reports what is known', () => {
  const note = new Function('fmtCount', 'fmtTime', 'reasonText',
    sourceFunction('diagnosticsOperationLogNote') + '\nreturn diagnosticsOperationLogNote;')(
    (value) => String(value), () => '12:00:00', (code) => code);
  assert.match(note({}), /导出仍可用/);
  assert.match(note({ diagnostics: { operationLog: { available: true, retained: 12, limit: 500 } } }), /12 条/);
  assert.match(note({ diagnostics: { operationLog: { available: false, reason: 'not_retained' } } }), /不可用/);
});

test('resaving an existing route sends explicit nulls for cleared optional fields', () => {
  const code = [
    'const currentConnectionLimit = () => undefined;',
    ...['routeIdFor', 'positiveInteger', 'buildOnboardRow'].map(sourceFunction),
    'return buildOnboardRow;',
  ].join('\n');
  const build = new Function(code)();
  const base = {
    connectionId: 'conn', upstreamModelId: 'model-a', protocol: 'responses', contextWindow: 262144,
    reasoning: true, efforts: [], vision: false,
  };
  /* 已有路由：取消推理、清空名称与输出必须显式 null，否则 MergeModels 保留旧值。 */
  const cleared = build({ ...base, name: '', maxOutputTokens: undefined }, { existing: true });
  assert.equal(cleared.name, null);
  assert.equal(cleared.maxOutputTokens, null);
  assert.equal(cleared.reasoningEfforts, null);
  assert.equal(cleared.defaultEffort, null);
  /* 原模型没有上限时不应凭空写 null；只有确实存在旧上限才清。 */
  assert.equal('contextLimit' in cleared, false);
  const limited = build({ ...base, contextLimit: 262144, contextWindow: 300000 }, { existing: true });
  assert.equal(limited.contextLimit, null);
  const rediscovered = build({ ...base, contextWindow: 300000 }, { existing: true, previousLimit: 262144 });
  assert.equal(rediscovered.contextLimit, null);
  const reasoningUnknown = build({ ...base, reasoning: undefined }, { existing: true });
  assert.equal(reasoningUnknown.reasoning, null);
  /* 新路由仍按契约省略未知字段。 */
  const fresh = build({ ...base, name: '', maxOutputTokens: undefined });
  assert.equal('name' in fresh, false);
  assert.equal('maxOutputTokens' in fresh, false);
  assert.equal('reasoningEfforts' in fresh, false);
  assert.equal('defaultEffort' in fresh, false);
  assert.equal('contextLimit' in fresh, false);
});

test('non-integer context or output blocks saving instead of reaching the gateway', () => {
  const onboardProblems = makeProblems();
  const bad = onboardProblems({ models: [makeRow('model-a', {
    protocol: 'chat', contextWindow: 128000.5, maxOutputTokens: -1,
  })] });
  assert.equal(bad.problems.some((text) => text.includes('上下文窗口必须是正整数')), true);
  assert.equal(bad.problems.some((text) => text.includes('最大输出必须是正整数')), true);
  const zero = onboardProblems({ models: [makeRow('model-a', { protocol: 'chat', contextWindow: 0 })] });
  assert.equal(zero.problems.some((text) => text.includes('请为这些模型填写上下文窗口')), true);
  const good = onboardProblems({ models: [makeRow('model-a', { protocol: 'chat', contextWindow: 128000 })] });
  assert.deepEqual(good.problems, []);
});

test('a known context limit is saved with the window and an unknown one is not invented', () => {
  const code = [
    ...['routeIdFor', 'positiveInteger'].map(sourceFunction),
    'let limit = null;',
    'const currentConnectionLimit = () => limit;',
    sourceFunction('buildOnboardRow'),
    'return { build: buildOnboardRow, setLimit: (value) => { limit = value; } };',
  ].join('\n');
  const helpers = new Function(code)();
  const row = { connectionId: 'conn', upstreamModelId: 'model-a', protocol: 'chat', contextWindow: 100000, reasoning: false };
  const unknown = helpers.build({ ...row });
  assert.equal('contextLimit' in unknown, false);
  helpers.setLimit(200000);
  const known = helpers.build({ ...row });
  assert.equal(known.contextLimit, 200000);
  const tighter = helpers.build({ ...row, contextWindow: 300000 });
  assert.equal('contextLimit' in tighter, false);
});

test('switching presets replaces stale suggested ids but keeps a hand-edited one', () => {
  const code = [
    sourceFunction('presetsSuggestedId'),
    sourceFunction('providerPresets'),
    sourceFunction('providerPreset'),
    sourceFunction('connectionRowsOf'),
    sourceFunction('savedOnboardTarget'),
    sourceFunction('onboardSuggestedId'),
    sourceFunction('onboardSuggestedName'),
    sourceFunction('staleSuggested'),
    sourceFunction('applyOnboardPreset'),
    'const setText = () => {};',
    'const renderOnboard = () => {};',
    'const clearOnboardSaveErrors = () => {};',
    'return applyOnboardPreset;',
  ].join('\n');
  const makeEnv = ({ id, name, saved = null }) => {
    const state = {
      providers: { presets: [
        { id: 'a', name: 'Alpha', baseURL: 'https://a.example.com/v1' },
        { id: 'b', name: 'Beta', baseURL: 'https://b.example.com/v1' },
      ] },
      data: { connections: { saved: saved ? [saved] : [] } },
      onboard: { touched: false },
    };
    const dom = {
      onboardBase: { value: '' }, onboardId: { value: id }, onboardName: { value: name },
      onboardKey: { value: 'sk' }, onboardPreset: { value: 'a', dataset: {} },
      onboardPresetHint: { textContent: '' },
    };
    dom.onboardPreset.dataset.presetId = 'a';
    return { state, dom, apply: new Function('state', 'dom', code)(state, dom) };
  };
  /* 仍等于上一预设的建议值：换成新预设的建议值。 */
  const stale = makeEnv({ id: 'a', name: 'Alpha' });
  stale.apply('b');
  assert.equal(stale.dom.onboardId.value, 'b');
  assert.equal(stale.dom.onboardName.value, 'Beta');
  /* 仍等于已保存连接的自动回填值：同样更新。 */
  const backfilled = makeEnv({ id: 'alpha', name: 'Alpha saved', saved: { id: 'alpha', name: 'Alpha saved' } });
  backfilled.apply('b');
  assert.equal(backfilled.dom.onboardId.value, 'b');
  assert.equal(backfilled.dom.onboardName.value, 'Beta');
  /* 首次空白或用户清空后，预设建议值仍然正常填入。 */
  const empty = makeEnv({ id: '', name: '' });
  empty.apply('b');
  assert.equal(empty.dom.onboardId.value, 'b');
  assert.equal(empty.dom.onboardName.value, 'Beta');
  /* 用户手改过：保留原值，避免覆盖另一条连接。 */
  const custom = makeEnv({ id: 'my-second', name: 'My Second' });
  custom.apply('b');
  assert.equal(custom.dom.onboardId.value, 'my-second');
  assert.equal(custom.dom.onboardName.value, 'My Second');
});

test('onboarding keeps the saved apiKeyEnv and brings the preset protocols', () => {
  const state = { providers: { presets: [{ id: 'p', protocols: ['chat', 'responses'] }] } };
  const code = [
    sourceFunction('providerPresets'),
    sourceFunction('providerPreset'),
    sourceFunction('onboardConnectionProtocols'),
    sourceFunction('onboardConnectionPayload'),
    'return onboardConnectionPayload;',
  ].join('\n');
  const build = new Function('state', code)(state);
  const values = { id: 'conn', name: 'Conn', providerPresetId: 'p', baseURL: 'https://api.example.com/v1', apiKey: 'sk-new' };
  const withSaved = build(values, { id: 'conn', apiKeyEnv: 'CONN_API_KEY', protocols: ['chat'] });
  assert.equal(withSaved.apiKeyEnv, 'CONN_API_KEY');
  assert.deepEqual(withSaved.protocols, ['chat', 'responses']);
  const narrowed = build(values, { id: 'conn', providerPresetId: 'p', protocols: ['chat'] });
  assert.deepEqual(narrowed.protocols, ['chat']);
  const withoutSaved = build({ ...values, providerPresetId: '' }, null);
  assert.equal('apiKeyEnv' in withoutSaved, false);
  assert.equal('protocols' in withoutSaved, false);
  assert.equal(withoutSaved.apiKey, 'sk-new');
  const savedOnly = build({ ...values, providerPresetId: '' }, { id: 'conn', apiKeyEnv: 'CONN_API_KEY', protocols: ['responses'] });
  assert.deepEqual(savedOnly.protocols, ['responses']);
});

test('a successful onboarding save resets the connection draft baseline and the old catalogue', async () => {
  const savedState = {
    revision: 'rev-2',
    models: [{ id: 'conn/resp/model-a', connectionId: 'conn' }],
    connections: { saved: [{ id: 'conn', name: 'Connection' }] },
  };
  const state = {
    data: { revision: 'rev-1', models: [], connections: { saved: [] } },
    onboard: { saving: false, touched: true, discovery: { models: [] }, discoveryFor: 'old' },
    conns: { editing: null, draft: [], dirty: true, base: 'old' },
    stateReadEpoch: 0, key: 'session',
  };
  const dom = { onboardKey: { value: 'sk' }, onboardDisclosure: { open: true } };
  const code = [sourceFunction('saveOnboard'), 'const clearOnboardSaveErrors = () => {};', 'return saveOnboard;'].join('\n');
  const save = new Function(
    'state', 'dom', 'API', 'selectedOnboardModels', 'onboardProblems', 'connectionPayload', 'connectionRowsOf',
    'mergeOnboardModels', 'request', 'adoptState', 'renderAll', 'renderOnboard', 'renderActions', 'showToast',
    'handleSaveFailure', 'setSubtab', 'ApiError', 'fmtCount',
    'savedOnboardRow', 'onboardConnectionPayload', 'savedConnectionsSignature', 'connectionDraftFrom',
    'fillOnboardForm', 'onboardEndpointKey', code,
  )(
    state, dom, { workspace: '/api/workspace' }, () => [{}], () => ({
      problems: [], built: [{ id: 'conn/resp/model-a' }],
      values: { id: 'conn', name: 'Connection', providerPresetId: '', baseURL: 'https://api.example.com/v1', apiKey: 'sk' },
    }), () => [], () => [], (_existing, built) => built,
    async () => ({ state: savedState }), next => { state.data = next; }, () => {}, () => {}, () => {}, () => {}, async () => {},
    () => {}, class ApiError extends Error {}, String,
    () => null, () => ({ id: 'conn' }), () => 'fresh', () => [], () => {}, () => 'fresh-endpoint',
  );
  assert.equal(await save(), true);
  assert.equal(state.conns.base, 'fresh');
  assert.equal(state.conns.dirty, false);
  assert.equal(state.conns.baseRevision, 'rev-2');
  assert.equal(state.onboard.discovery, null);
  assert.equal(state.onboard.discoveryFor, null);
});

test('preset provenance keeps only its own https url and a legal checked timestamp', () => {
  const sourceHelpers = (() => {
    const start = source.indexOf('const SOURCE_STAMP_PATTERN');
    const end = source.indexOf('/* 预设目录在 providers.json', start);
    return source.slice(start, end);
  })();
  const helpers = new Function([sourceHelpers, sourceFunction('presetMetadataSource'), 'return presetMetadataSource;'].join('\n'))();
  const clean = helpers({ source: { url: 'https://docs.example.invalid/models', checkedAt: '2026-11-02' } });
  assert.deepEqual(clean, { kind: 'preset', url: 'https://docs.example.invalid/models', checkedAt: '2026-11-02' });
  /* 上游 /models 是 http、带查询参数或根本不是预设地址时都不能被写成预设来源。 */
  const unsafe = helpers({ source: { url: 'http://upstream.example.invalid/v1/models?key=1', checkedAt: 'yesterday' } });
  assert.deepEqual(unsafe, { kind: 'preset' });
  const missing = helpers({});
  assert.deepEqual(missing, { kind: 'preset' });
});

test('a row without its own metadata source is not attributed to the upstream catalogue', () => {
  const sourceHelpers = (() => {
    const start = source.indexOf('const SOURCE_STAMP_PATTERN');
    const end = source.indexOf('/* 预设目录在 providers.json', start);
    return source.slice(start, end);
  })();
  const rowSource = new Function([sourceHelpers, sourceFunction('onboardRowSource'), 'return onboardRowSource;'].join('\n'))();
  /* 预设补全过：只有 metadataSource 决定来源，不能回落到上游 URL。 */
  const preset = rowSource({
    metadataSource: { kind: 'preset', url: 'https://docs.example.invalid/models', checkedAt: '2026-11-02' },
    source: { url: 'https://api.example.invalid/v1/models', checkedAt: '2026-11-03' },
  });
  assert.deepEqual(preset, {
    checkedAt: '2026-11-02', sourceURL: 'https://docs.example.invalid/models', metadataKind: 'preset',
  });
  /* 没有 metadataSource 的正常目录行才用上游 source，并标成 discovery。 */
  const discovery = rowSource({ source: { url: 'https://api.example.invalid/v1/models', checkedAt: '2026-11-03' } });
  assert.equal(discovery.metadataKind, 'discovery');
  assert.equal(discovery.sourceURL, 'https://api.example.invalid/v1/models');
});

test('the default protocol comes from the options the connection actually allows', () => {
  const code = [
    'const PROTOCOL_CHOICES = ["chat", "responses"];',
    ...['protocolList', 'preferredProtocol', 'connectionProtocolLimits', 'protocolChoicesFor', 'onboardRowSource'].map(sourceFunction),
    sourceFunction('onboardRowFrom'),
    'return onboardRowFrom;',
  ].join('\n');
  const build = new Function('document', code)(undefined);
  /* 上游声明两个协议、连接只允许 chat：默认值必须是可选的 chat。 */
  const limited = build({ id: 'model-a', protocols: ['chat', 'responses'] }, { connection: { protocols: ['chat'] } });
  assert.deepEqual(limited.protocolOptions, ['chat']);
  assert.equal(limited.protocol, 'chat');
  /* 没有连接限制时仍按既有偏好选 responses。 */
  const open = build({ id: 'model-b', protocols: ['chat', 'responses'] }, { connection: null });
  assert.equal(open.protocol, 'responses');
});

test('a hand-typed name keeps its raw value until save trims it', () => {
  const build = new Function([
    'const currentConnectionLimit = () => undefined;',
    ...['routeIdFor', 'positiveInteger', 'buildOnboardRow'].map(sourceFunction),
    'return buildOnboardRow;',
  ].join('\n'))();
  const row = { connectionId: 'conn', upstreamModelId: 'model-a', protocol: 'chat', contextWindow: 1000, name: '  My A  ' };
  assert.equal(build(row).name, 'My A');
  /* 名称与上游 ID 相同时仍不写 name，避免无意义字段。 */
  assert.equal('name' in build({ ...row, name: 'model-a' }), false);
});

test('same upstream id with two protocols keeps independent editable rows', () => {
  const code = [
    sourceFunction('onboardRowKey'),
    sourceFunction('onboardRowByKey'),
    'const onboardModelRows = () => state.onboard.discovery.models;',
    'return { onboardRowKey, onboardRowByKey };',
  ].join('\n');
  const state = { onboard: { discovery: { models: [
    { key: 'conn/chat/model-a', id: 'model-a', upstreamModelId: 'model-a', protocol: 'chat', name: 'A chat' },
    { key: 'conn/resp/model-a', id: 'model-a', upstreamModelId: 'model-a', protocol: 'responses', name: 'A resp' },
  ] } } };
  const helpers = new Function('state', code)(state);
  assert.equal(helpers.onboardRowKey(state.onboard.discovery.models[0]), 'conn/chat/model-a');
  assert.equal(helpers.onboardRowKey(state.onboard.discovery.models[1]), 'conn/resp/model-a');
  /* 按 key 查到的是各自那一行，不会落到同 ID 的兄弟行。 */
  assert.equal(helpers.onboardRowByKey('conn/resp/model-a').name, 'A resp');
  assert.equal(helpers.onboardRowByKey('conn/chat/model-a').name, 'A chat');
});

test('resaving an unchanged saved model keeps its real context limit', () => {
  const code = [
    'const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;',
    "const CONNECTION_ID_HINT = 'hint';",
    'const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);',
    'const state = { data: { models: [{ id: "conn/resp-model-a", connectionId: "conn", contextLimit: 200000 }] } };',
    "const dom = {};",
    "const onboardConnectionValues = () => ({ id: 'conn', name: 'C', baseURL: 'https://api.example.com/v1', apiKey: 'sk' });",
    "const savedOnboardRow = () => ({ id: 'conn', apiKeySet: true, baseURL: 'https://api.example.com/v1' });",
    ...['positiveInteger', 'currentConnectionLimit', 'existingOnboardRouteIds', 'modelFamilyFor', 'routeIdFor',
      'protocolList', 'connectionProtocolLimits', 'protocolChoicesFor', 'preferredProtocol', 'buildOnboardRow',
      'onboardConnectionProtocols', 'normalizeBaseURL', 'protocolLabel', 'onboardProblems'].map(sourceFunction),
    'const providerPreset = () => null;',
    'return onboardProblems;',
  ].join('\n');
  const onboardProblems = new Function(code)();
  const built = onboardProblems({ models: [makeRow('model-a', {
    protocol: 'responses', contextWindow: 100000, contextLimit: 200000, efforts: ['low'], selected: true,
  })] }).built[0];
  assert.equal(built.contextLimit, 200000);
  const rediscovered = onboardProblems({ models: [makeRow('model-a', {
    protocol: 'responses', contextWindow: 300000, selected: true,
  })] }).built[0];
  assert.equal(rediscovered.contextLimit, null);
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
    'const markOnboardEdited = () => { state.onboard.touched = true; };',
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
    'const markOnboardEdited = () => { state.onboard.touched = true; };',
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
    ...['positiveInteger', 'onboardModelRows', 'selectedOnboardModels', 'applyOnboardBatchContext'].map(sourceFunction),
    'const markOnboardEdited = () => { state.onboard.touched = true; };',
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

test('new connection flows default to custom and require an explicit preset selection', () => {
  const select = { value: '', dataset: {}, disabled: false, replaceChildren() { this.children = []; }, append(option) { this.children.push(option); } };
  const events = [];
  globalThis.document = {
    createElement: tag => ({ tagName: tag, append(...children) { this.children = children; } }),
  };
  try {
    const render = new Function('select', 'events', [
      'const providers = { menu: { families: [{ family: "kimi-code", regions: [{ region: "cn", plans: [{ plan: "coding", variants: [{ presetId: "kimi-code-cn", name: "Kimi Code（国内）" }] }] }] }] }, presets: [{ id: "kimi-code-cn", family: "kimi-code", region: "cn", kind: "coding" }] };',
      'const state = { family: "kimi-code", region: "cn", plan: "coding" };',
      'const family = { value: state.family, addEventListener() {}, dispatchEvent() {} };',
      'const region = { value: state.region, addEventListener() {}, dispatchEvent() {} };',
      'const plan = { value: state.plan, addEventListener() {}, dispatchEvent() {} };',
      growthFunction('fillProviderOptions'),
      'const previous = select.value;',
      'const families = providers.menu.families;',
      'const selectedFamily = families.find(row => row.family === state.family);',
      'const regions = selectedFamily?.regions || [];',
      'const selectedRegion = regions.find(row => String(row.region) === state.region);',
      'const plans = selectedRegion?.plans || [];',
      'const variants = plans.find(row => row.plan === state.plan)?.variants || [];',
      'fillProviderOptions(select, [{ value: "", label: "选择接口与端点" }, ...variants.map(row => ({ value: row.presetId, label: row.name }))], select.value);',
      'select.disabled = false;',
      'events.push({ value: select.value, options: select.children.map(option => option.value) });',
    ].join('\n'))(select, events);
    assert.equal(select.value, '');
    assert.deepEqual(events, [{ value: '', options: ['', 'kimi-code-cn'] }]);
    assert.match(htmlSource, /id="ob-name"[^>]*placeholder="自定义名称（选填）"/);
    assert.match(htmlSource, /id="ob-id"[^>]*placeholder="自定义连接标识（选填）"/);
  } finally {
    delete globalThis.document;
  }
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
    onboardDisclosure: { open: false, dataset: {} },
    onboardPreset: { focus: () => calls.push('focus') },
  };
  const code = [
    sourceFunction('openOnboardWizard'),
    'const setView = (view) => calls.push(view);',
    'const setSubtab = (group, tab) => calls.push(group + ":" + tab);',
    'const scrollMainToElement = () => calls.push("scroll-main");',
    'const editorDirty = () => state.editor?.dirty === true;',
    'const closeEditor = () => { state.editor = null; calls.push("close"); };',
    'const showToast = (message) => calls.push(message);',
    'return openOnboardWizard;',
  ].join('\n');
  const open = new Function('state', 'dom', 'calls', code)(state, dom, calls);
  open();
  assert.equal(dom.onboardDisclosure.open, true);
  assert.deepEqual(calls, ['models', 'models:connections', 'close', 'scroll-main', 'focus']);
  calls.length = 0;
  state.editor = { dirty: true };
  open();
  assert.deepEqual(calls, ['请先保存或取消正在编辑的模型，再打开添加向导。']);
});

test('a successful onboarding save closes the form and shows the saved model catalogue', async () => {
  const savedState = { revision: 'rev-2', models: [{ id: 'conn/resp/model-a' }], connections: { saved: [{ id: 'conn' }] } };
  const state = {
    data: { revision: 'rev-1', models: [], connections: { saved: [] } },
    onboard: { saving: false, touched: true },
    conns: { editing: null, draft: [] },
    stateReadEpoch: 0, key: 'session',
  };
  const dom = { onboardKey: { value: 'synthetic' }, onboardDisclosure: { open: true } };
  const tabs = [];
  const code = [
    sourceFunction('saveOnboard'),
    'const clearOnboardSaveErrors = () => {};',
    'return saveOnboard;',
  ].join('\n');
  const save = new Function(
    'state', 'dom', 'API', 'selectedOnboardModels', 'onboardProblems', 'connectionPayload', 'connectionRowsOf',
    'mergeOnboardModels', 'request', 'adoptState', 'renderAll', 'renderOnboard', 'renderActions', 'showToast',
    'handleSaveFailure', 'setSubtab', 'ApiError', 'fmtCount',
    'savedOnboardRow', 'onboardConnectionPayload', 'savedConnectionsSignature', 'connectionDraftFrom',
    'fillOnboardForm', 'onboardEndpointKey', code,
  )(
    state, dom, { workspace: '/api/workspace' }, () => [{}], () => ({
      problems: [], built: [{ id: 'conn/resp/model-a' }],
      values: { id: 'conn', name: 'Connection', providerPresetId: '', baseURL: 'https://api.example.com/v1', apiKey: 'synthetic' },
    }), () => [], () => [], (_existing, built) => built,
    async () => ({ state: savedState }), next => { state.data = next; }, () => {}, () => {}, () => {}, () => {}, async () => {},
    (group, tab) => tabs.push([group, tab]), class ApiError extends Error {}, String,
    () => null, () => ({ id: 'conn' }), () => 'sig', () => [], () => {}, () => 'endpoint',
  );
  assert.equal(await save(), true);
  assert.equal(dom.onboardDisclosure.open, false);
  assert.deepEqual(tabs, [['models', 'catalog']]);
  assert.equal(dom.onboardKey.value, '');
});

test('a failed onboarding save keeps the form, the key and the current subtab', async () => {
  const state = {
    data: { revision: 'rev-1', models: [], connections: { saved: [] } },
    onboard: { saving: false, touched: true },
    conns: { editing: null, draft: [] },
    stateReadEpoch: 0, key: 'session',
  };
  const dom = { onboardKey: { value: 'sk-typed' }, onboardDisclosure: { open: true } };
  const tabs = [];
  const code = [sourceFunction('saveOnboard'), 'const clearOnboardSaveErrors = () => {};', 'return saveOnboard;'].join('\n');
  const save = new Function(
    'state', 'dom', 'API', 'selectedOnboardModels', 'onboardProblems', 'connectionPayload', 'connectionRowsOf',
    'mergeOnboardModels', 'request', 'adoptState', 'renderAll', 'renderOnboard', 'renderActions', 'showToast',
    'handleSaveFailure', 'setSubtab', 'ApiError', 'fmtCount',
    'savedOnboardRow', 'onboardConnectionPayload', 'savedConnectionsSignature', 'connectionDraftFrom',
    'fillOnboardForm', 'onboardEndpointKey', code,
  )(
    state, dom, { workspace: '/api/workspace' }, () => [{}], () => ({
      problems: [], built: [{ id: 'conn/resp/model-a' }],
      values: { id: 'conn', name: 'Connection', providerPresetId: '', baseURL: 'https://api.example.com/v1', apiKey: 'sk-typed' },
    }), () => [], () => [], (_existing, built) => built,
    async () => { throw new Error('offline'); }, next => { state.data = next; }, () => {}, () => {}, () => {}, () => {},
    async () => {}, (group, tab) => tabs.push([group, tab]), class ApiError extends Error {}, String,
    () => null, () => ({ id: 'conn' }), () => 'sig', () => [], () => {}, () => 'endpoint',
  );
  assert.equal(await save(), false);
  assert.equal(dom.onboardKey.value, 'sk-typed');
  assert.equal(dom.onboardDisclosure.open, true);
  assert.deepEqual(tabs, []);
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
    'const resetModelSignature = () => {}, resetRecentSignature = () => {}, resetSubagentSignatures = () => {}, resetIntegrationModeSignature = () => {}, clearDisconnectConfirm = () => {}, refreshServiceStatus = () => {};',
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
    'const resetModelSignature = () => {}, resetRecentSignature = () => {}, resetSubagentSignatures = () => {}, resetIntegrationModeSignature = () => {}, clearDisconnectConfirm = () => {}, refreshServiceStatus = () => {};',
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

test('diagnostics export uses POST, reports the saved file path, and surfaces failures', async () => {
  const calls = [];
  const toasts = [];
  const state = { diagnosticsExporting: false, diagnosticsExportError: '', diagnosticsExportPath: '', data: {} };
  const code = [sourceFunction('exportDiagnostics'), 'return exportDiagnostics;'].join('\n');
  const make = (requestImpl) => new Function(
    'state', 'API', 'request', 'showToast', 'renderDiagnosticsExport', code,
  )(
    state, { diagnosticsExport: '/api/diagnostics/export' }, requestImpl,
    (message, tone) => toasts.push({ message, tone }), () => {},
  );
  const ok = make(async (path, options) => {
    calls.push([path, options]);
    return { ok: true, path: '/tmp/diagnostics/report-20261124.json', report: { schemaVersion: 1 } };
  });
  assert.equal(await ok(), true);
  assert.deepEqual(calls, [['/api/diagnostics/export', { method: 'POST', body: {} }]]);
  assert.equal(state.diagnosticsExportPath, '/tmp/diagnostics/report-20261124.json');
  assert.match(toasts.at(-1).message, /\/tmp\/diagnostics\/report-20261124\.json/);
  const missingPath = make(async () => ({ ok: true }));
  assert.equal(await missingPath(), false);
  assert.match(state.diagnosticsExportError, /未返回导出文件路径/);
  const failing = make(async () => { throw new Error('boom'); });
  assert.equal(await failing(), false);
  assert.match(state.diagnosticsExportError, /^导出失败：boom/);
  assert.equal(state.diagnosticsExporting, false);
});

test('custom model context shortcuts and thinking efforts are visible outside advanced settings', () => {
  const form = htmlSource.slice(htmlSource.indexOf('id="model-form"'), htmlSource.indexOf('id="model-errors"'));
  const advanced = form.indexOf('id="m-advanced"');
  for (const id of ['m-budget-group', 'm-context', 'm-output', 'm-effort-field', 'm-default']) {
    const position = form.indexOf(`id="${id}"`);
    assert.ok(position >= 0, 'missing ' + id);
    assert.ok(position < advanced, id + ' must be in the main form, not inside advanced settings');
  }
});

test('index.html lays out five flat tabs, subtabs, and static views without scrollIntoView or overflow:clip', () => {
  for (const id of ['nav-overview', 'nav-models', 'nav-subagents', 'nav-usage', 'nav-maintenance']) {
    assert.ok(htmlSource.includes(`id="${id}"`), 'missing top tab ' + id);
  }
  assert.equal(htmlSource.includes('id="nav-work"'), false);
  assert.equal(htmlSource.includes('id="nav-extensions"'), false);
  assert.equal(htmlSource.includes('id="work-index"'), false);
  assert.equal(htmlSource.includes('id="quick-models"'), false);
  assert.equal(htmlSource.includes('id="models-services"'), false);
  assert.equal(source.includes('function mountWorkspace('), false);
  assert.equal(source.includes('.scrollIntoView('), false, 'production JS must scroll .main, never call scrollIntoView');
  const cssSource = readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
  assert.equal(cssSource.includes('overflow: clip'), false, 'styles.css must avoid overflow: clip on root');
  assert.match(cssSource, /\.shell\s*\{[^}]*position:\s*fixed;[^}]*inset:\s*0;/);
  for (const id of ['models-tab-catalog', 'models-tab-connections', 'subagents-tab-models', 'subagents-tab-swarm',
    'settings-tab-accounts', 'settings-tab-connection', 'settings-tab-extensions', 'settings-tab-advanced']) {
    assert.ok(htmlSource.includes(`id="${id}"`), 'missing subtab ' + id);
  }
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

test('section links activate parent subtab and open collapsed ancestors while scrolling only .main', () => {
  const { makeNode, document } = makeDomTree();
  const main = makeNode('main', 'MAIN');
  main.scrollTop = 10;
  main.getBoundingClientRect = () => ({ top: 60 });
  const pane = makeNode('settings-pane-extensions', 'DIV');
  pane.dataset = { subtabGroup: 'maintenance', subtabPane: 'extensions' };
  const details = makeNode('mcp-details', 'DETAILS');
  const panel = makeNode('mcp-panel');
  panel.getBoundingClientRect = () => ({ top: 260 });
  const heading = makeNode('mcp-heading', 'H2');
  panel.append(heading);
  details.append(panel);
  pane.append(details);
  const view = makeNode('view-maintenance');
  view.append(pane);
  const subtabsActivated = [];
  const code = [
    'const $ = id => document.getElementById(id);',
    'const setSubtab = (group, tab) => subtabsActivated.push([group, tab]);',
    sourceFunction('scrollMainToElement'),
    sourceFunction('revealWorkspaceSection'),
    'return revealWorkspaceSection;',
  ].join('\n');
  const reveal = new Function('document', 'subtabsActivated', code)(document, subtabsActivated);
  assert.equal(details.open, false);
  const target = reveal('mcp-panel');
  assert.equal(target, panel);
  assert.deepEqual(subtabsActivated, [['maintenance', 'extensions']]);
  assert.equal(details.open, true);
  assert.equal(panel.scrolled, false, 'must never call scrollIntoView');
  assert.equal(main.scrollTop, 194);
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
      adopted: false,
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

test('a complete direct import only requires a restart when a managed process has not adopted it', () => {
  const directRestartRequired = makeDirectRestartRequired();
  const syncState = makeSyncState();
  const models = [{ id: 'conn/resp/model-a' }];
  const directMode = { mode: 'direct', supported: true, staticHostsReady: true,
    eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1 };
  const integration = { mode: 'direct', state: 'ready', applied: true, adopted: false,
    aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } };

  /* Kimi 未运行：adopted=false 是常态，下次启动自然会采用，不是重启。 */
  const stopped = {
    integrationMode: 'direct', models, kimiProcess: { running: false }, directMode,
    integration: { ...integration, desktop: { state: 'stopped', managed: false } },
  };
  assert.equal(directRestartRequired(stopped), false);
  const stoppedVerdict = syncState(stopped);
  assert.equal(stoppedVerdict.code, 'ready');
  assert.equal(stoppedVerdict.word, '直连已就绪');
  assert.doesNotMatch(stoppedVerdict.note, /重启/);

  /* 非托管运行：系统直接启动的 Kimi 没有可重启的受管子进程。 */
  const unmanaged = {
    ...stopped, kimiProcess: { running: true },
    integration: { ...integration, desktop: { state: 'ready', managed: false } },
  };
  assert.equal(directRestartRequired(unmanaged), false);
  const unmanagedVerdict = syncState(unmanaged);
  assert.equal(unmanagedVerdict.code, 'ready');
  assert.equal(unmanagedVerdict.word, '直连运行中（免开本软件）');
  assert.doesNotMatch(unmanagedVerdict.note, /需要重启|正常退出并重启/);

  /* 受管进程在跑却还没采用配置：这时才要求重启。 */
  const managed = {
    ...unmanaged,
    integration: { ...integration, desktop: { state: 'ready', managed: true } },
  };
  assert.equal(directRestartRequired(managed), true);
  const managedVerdict = syncState(managed);
  assert.equal(managedVerdict.code, 'restart');
  assert.match(managedVerdict.word, /需要重启 Kimi/);
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
    integration: { ...imported, adopted: false, desktop: { managed: false } },
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
  fact.renderGatewayModeFact({
    ...base,
    integration: { mode: 'direct', state: 'pending',
      nativeImport: { state: 'requires-restart', requiresRestart: true, restartDeferred: true, routeCleanupPending: true } },
  });
  assert.equal(fact.el.textContent, '直连模式 · 需要重启 Kimi');
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

test('a direct write that needs restart exposes the restart action instead of another write', () => {
  const { render, nodes } = makeWorkspaceSyncEnv();
  const models = [{ id: 'conn/resp/model-a' }, { id: 'conn/chat/model-b' }];
  const directMode = { mode: 'direct', supported: true, staticHostsReady: true, routeCleanupPending: true,
    eligibleModelIds: ['conn/resp/model-a', 'conn/chat/model-b'], eligibleModels: 2, totalModels: 2 };
  const pending = {
    integrationMode: 'direct', models, directMode,
    integration: { mode: 'direct', state: 'pending', reason: 'native_profile_pending',
      nativeImport: { state: 'requires-restart', reason: 'native_import_requires_restart', requiresRestart: true, restartDeferred: true, routeCleanupPending: true } },
  };
  render(pending);
  assert.equal(nodes['workspace-sync-state'].textContent, '需要重启 Kimi');
  assert.match(nodes['workspace-sync-note'].textContent, /直连配置已写入/);
  assert.match(nodes['workspace-sync-note'].textContent, /保留后台服务/);
  assert.doesNotMatch(nodes['workspace-sync-note'].textContent, /重试清理|切换收尾未完成/);
  assert.equal(nodes['workspace-restart'].hidden, false);
  const syncState = makeSyncState();
  assert.equal(syncState(pending).code, 'restart');
  assert.equal(syncState(pending).word, '需要重启 Kimi');
  assert.doesNotMatch(syncState(pending).word, /待写入/);
  assert.doesNotMatch(syncState(pending).note, /重试清理/);
  const connectionText = makeNativeConnectionText()(pending);
  assert.match(connectionText, /正常重启 Kimi 后完成应用/);
  assert.doesNotMatch(connectionText, /重试清理/);

  const ready = {
    ...pending,
    directMode: { ...pending.directMode, routeCleanupPending: false },
    integration: { ...pending.integration, state: 'ready', applied: true, adopted: true,
      nativeImport: { ...pending.integration.nativeImport, routeCleanupPending: false },
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a', 'conn/chat/model-b': 'byok-uuid/model-b' } },
  };
  render(ready);
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Work');
  assert.equal(nodes['workspace-restart'].hidden, true);
  assert.equal(syncState(ready).word, '直连已就绪');

  const awaitingAdoption = {
    ...ready,
    /* 受管进程已启动但还没采用配置：这才是「需要重启」的真实证据。 */
    integration: { ...ready.integration, adopted: false, desktop: { managed: true } },
  };
  render(awaitingAdoption);
  assert.equal(nodes['workspace-sync-state'].textContent, '需要重启 Kimi');
  assert.equal(nodes['workspace-restart'].hidden, false);
  assert.equal(syncState(awaitingAdoption).code, 'restart');
  assert.match(makeNativeConnectionText()(awaitingAdoption), /正常重启 Kimi 后完成应用/);

  /* 手动重启完成后 adopted=true：重启提示让位给切换收尾清理，同步入口恢复。 */
  const cleanupAfterRestart = {
    ...ready,
    directMode: { ...ready.directMode, routeCleanupPending: true },
    integration: { ...ready.integration, adopted: true, desktop: { managed: true },
      nativeImport: { ...ready.integration.nativeImport, routeCleanupPending: true } },
  };
  render(cleanupAfterRestart);
  assert.equal(nodes['workspace-sync-state'].textContent, '切换收尾未完成');
  assert.match(nodes['workspace-sync-note'].textContent, /重试清理/);
  assert.equal(nodes['workspace-restart'].hidden, true);
  assert.equal(syncState(cleanupAfterRestart).code, 'pending');
  assert.match(makeNativeConnectionText()(cleanupAfterRestart), /重试清理/);
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

function makeIntegrationModeEnv() {
  const nodes = {};
  for (const id of ['integration-mode-state', 'direct-mode-notice', 'direct-mode-support', 'direct-mode-hosts',
    'native-sync-label', 'native-sync', 'native-recover']) {
    nodes[id] = { id, textContent: '', hidden: false, disabled: false, dataset: {}, setAttribute() {},
      classList: { add() {}, toggle() {} } };
  }
  nodes['native-sync'].dataset.action = 'connect-kimi';
  nodes['native-recover'].dataset.action = 'prepare-native';
  const dom = {
    modeGateway: { value: 'gateway', checked: false, disabled: false },
    modeDirect: { value: 'direct', checked: false, disabled: false },
    integrationModeGroup: null,
    integrationModeState: nodes['integration-mode-state'],
    nativeSyncLabel: nodes['native-sync-label'],
    nativeSync: nodes['native-sync'],
    nativeRecover: nodes['native-recover'],
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
    'const state = { actionButtons: [dom.nativeSync, dom.nativeRecover], pendingAction: null, modelsSaving: false, connsSaving: false };',
    'const onboardBusy = () => false;',
    'const renderOnboard = () => {};',
    'const renderOnboardOverview = () => {};',
    'const clearDisconnectConfirm = () => {};',
    'const configReady = () => true;',
    'const ACTION_LABEL = {};',
    'const setText = (element, text) => { if (element) element.textContent = text; };',
    'const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };',
    'const displayModelId = id => id;',
    'const DIRECT_BYPASS_WORD = {};',
    'const DIRECT_SKIP_REASON = {};',
    sourceFunction('isInFlight'),
    sourceFunction('renderIntegrationMode'),
    sourceFunction('renderActions'),
    'return { renderIntegrationMode, renderActions };',
  ].join('\n');
  const renderers = new Function('dom', code)(dom);
  const render = data => {
    renderers.renderIntegrationMode(data);
    renderers.renderActions(data);
  };
  return { nodes, dom, renderers, render };
}

test('the settings notice and sync button both name the pending cleanup', () => {
  const { nodes, render } = makeIntegrationModeEnv();

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

  const deferredRestart = {
    ...base,
    integration: { mode: 'direct', state: 'pending',
      nativeImport: { state: 'requires-restart', requiresRestart: true, restartDeferred: true, routeCleanupPending: true } },
  };
  const awaitingAdoption = {
    ...base,
    /* 只有受管进程尚未采用配置才应进入重启态；未托管/未运行不在此列。 */
    integration: { ...base.integration, adopted: false, desktop: { managed: true } },
  };
  for (const restarting of [deferredRestart, awaitingAdoption]) {
    render(restarting);
    assert.equal(nodes['direct-mode-notice'].dataset.tone, 'warn');
    assert.match(nodes['direct-mode-support'].textContent, /正常重启 Kimi/);
    assert.match(nodes['direct-mode-hosts'].textContent, /无需重复同步/);
    assert.doesNotMatch(nodes['direct-mode-hosts'].textContent, /重试清理/);
    assert.equal(nodes['native-sync-label'].textContent, '等待重启 Kimi');
    assert.equal(nodes['native-sync'].disabled, true, 'renderActions must not re-enable sync');
    assert.equal(nodes['native-recover'].disabled, true, 'prepare-native must not trigger another import either');
  }

  render(base);
  assert.equal(nodes['native-sync-label'].textContent, '重试清理旧来源并同步');
  assert.equal(nodes['native-sync'].disabled, false);
  assert.equal(nodes['native-recover'].disabled, false, 'cleanup after a manual restart must keep the recovery entry');
  assert.match(nodes['direct-mode-support'].textContent, /切换收尾未完成/);
  assert.match(nodes['direct-mode-hosts'].textContent, /重试清理旧来源/);
});

test('a saved gateway mode reports the unfinished direct cleanup and obeys a deferred restart receipt', () => {
  const syncState = makeSyncState();
  const connectionText = makeNativeConnectionText();
  const { render: renderWorkspace, nodes: workspace } = makeWorkspaceSyncEnv();
  const { nodes, render } = makeIntegrationModeEnv();

  const models = [{ id: 'conn/resp/model-a' }];
  const directMode = { mode: 'gateway', supported: true, staticHostsReady: false,
    eligibleModelIds: ['conn/resp/model-a'], eligibleModels: 1, totalModels: 1, routeCleanupPending: true };
  const integration = {
    mode: 'gateway', state: 'ready', applied: true, adopted: true,
    aliases: { 'conn/resp/model-a': 'byok-gateway/model-a' },
    readback: { modelAliases: { 'conn/resp/model-a': 'byok-gateway/model-a' } },
    nativeImport: { routeCleanupPending: true },
  };
  const cleanup = { integrationMode: 'gateway', models, directMode, integration };

  /* Direct → Gateway 切换收尾：不能因为 readback/applied 就绿色显示已同步。 */
  const verdict = syncState(cleanup);
  assert.equal(verdict.code, 'pending');
  assert.equal(verdict.word, '切换收尾未完成');
  assert.match(verdict.note, /旧的服务商来源/);
  assert.match(verdict.note, /重试清理旧来源并同步/);
  assert.match(verdict.note, /不要退出后台服务/);
  assert.doesNotMatch(verdict.word, /已同步|已核对目录/);

  /* directMode 行与回执里的任一条证据都必须生效。 */
  for (const variant of [
    { ...cleanup, directMode: { ...directMode, routeCleanupPending: true }, integration: { ...integration, nativeImport: {} } },
    { ...cleanup, directMode: { ...directMode, routeCleanupPending: false }, integration: { ...integration, nativeImport: { routeCleanupPending: true } } },
    { ...cleanup, directMode: { ...directMode, routeCleanupPending: false }, integration: { ...integration, nativeImport: { reason: 'route_cleanup_pending' } } },
  ]) {
    assert.equal(syncState(variant).word, '切换收尾未完成');
  }

  /* 读回成功或 synchronized 都不能盖过收尾标记：绿色结论必须让位。 */
  assert.equal(syncState(cleanup).code, 'pending');
  const synchronized = { ...cleanup, integration: { ...integration, state: 'synchronized' } };
  assert.equal(syncState(synchronized).code, 'pending');
  assert.equal(syncState(synchronized).word, '切换收尾未完成');

  renderWorkspace(cleanup);
  assert.equal(workspace['workspace-sync-state'].textContent, '切换收尾未完成');
  assert.match(workspace['workspace-sync-note'].textContent, /重试清理旧来源并同步/);
  assert.equal(workspace['workspace-restart'].hidden, true);

  assert.match(connectionText(cleanup), /切换收尾未完成/);
  assert.match(connectionText(cleanup), /重试清理旧来源并同步/);

  /* 设置页：状态行与按钮都提示清理，且同步与恢复入口保持可用。 */
  render(cleanup);
  assert.equal(nodes['direct-mode-notice'].hidden, true);
  assert.equal(nodes['integration-mode-state'].hidden, false);
  assert.match(nodes['integration-mode-state'].textContent, /切换收尾未完成/);
  assert.match(nodes['integration-mode-state'].textContent, /重试清理旧来源并同步/);
  assert.equal(nodes['native-sync-label'].textContent, '重试清理旧来源并同步');
  assert.equal(nodes['native-sync'].disabled, false);
  assert.equal(nodes['native-recover'].disabled, false);

  /* 清理完成：网关原文案恢复为绿色已同步。 */
  const clean = {
    ...cleanup,
    directMode: { ...directMode, routeCleanupPending: false },
    integration: { ...integration, nativeImport: {} },
  };
  assert.equal(syncState(clean).code, 'synced');
  renderWorkspace(clean);
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Work');
  render(clean);
  assert.equal(nodes['integration-mode-state'].hidden, true);
  assert.equal(nodes['native-sync-label'].textContent, '重新准备接入');
  assert.equal(nodes['native-sync'].disabled, false);

  const unmanagedRestart = {
    ...clean,
    kimiProcess: { running: true },
    integration: { ...clean.integration, adopted: false, desktop: { managed: false },
      nativeImport: { state: 'requires-restart', requiresRestart: true, restartDeferred: true } },
  };
  assert.equal(syncState(unmanagedRestart).code, 'synced');
  assert.doesNotMatch(syncState(unmanagedRestart).word, /重启/);
  renderWorkspace(unmanagedRestart);
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Work');
  assert.equal(workspace['workspace-restart'].hidden, true);
  assert.match(workspace['workspace-sync-note'].textContent, /非托管运行/);
  renderWorkspace({ ...unmanagedRestart, kimiProcess: { running: false } });
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Work');
  assert.match(workspace['workspace-sync-note'].textContent, /打开 Kimi 后/);
  assert.equal(workspace['workspace-restart'].hidden, true);
  assert.doesNotMatch(connectionText(unmanagedRestart), /需要重启|等待.*重启/);
  assert.match(connectionText(unmanagedRestart), /未受本产品托管/);
  render(unmanagedRestart);
  assert.equal(nodes['integration-mode-state'].hidden, true);
  assert.notEqual(nodes['native-sync-label'].textContent, '等待重启 Kimi');
  const unmanagedCleanup = {
    ...cleanup,
    kimiProcess: { running: true },
    integration: { ...cleanup.integration, adopted: false, desktop: { managed: false },
      nativeImport: { state: 'requires-restart', requiresRestart: true, restartDeferred: true, routeCleanupPending: true } },
  };
  assert.equal(syncState(unmanagedCleanup).code, 'pending');
  assert.equal(syncState(unmanagedCleanup).word, '切换收尾未完成');

  /* deferred 回执优先于清理提示；state=requires-restart 单独也成立。
     回执要求重启时 adopted 必为 false（重启尚未发生）。 */
  for (const receipt of [
    { state: 'requires-restart', requiresRestart: true, restartDeferred: true, routeCleanupPending: true },
    { state: 'requires-restart', routeCleanupPending: true },
  ]) {
    const deferred = { ...cleanup, integration: { ...integration, adopted: false, nativeImport: receipt } };
    const deferredVerdict = syncState(deferred);
    assert.equal(deferredVerdict.code, 'restart');
    assert.equal(deferredVerdict.word, '等待 Kimi 重启');
    assert.doesNotMatch(deferredVerdict.note, /重试清理|切换收尾/);

    renderWorkspace(deferred);
    assert.equal(workspace['workspace-sync-state'].textContent, '需要重启 Kimi');
    assert.match(workspace['workspace-sync-note'].textContent, /正常重启 Kimi/);
    assert.doesNotMatch(workspace['workspace-sync-note'].textContent, /重试清理|切换收尾/);
    assert.equal(workspace['workspace-restart'].hidden, false);

    assert.match(connectionText(deferred), /重启 Kimi Work/);
    assert.doesNotMatch(connectionText(deferred), /重试清理/);

    render(deferred);
    assert.match(nodes['integration-mode-state'].textContent, /正常重启 Kimi/);
    assert.equal(nodes['native-sync-label'].textContent, '等待重启 Kimi');
    /* 网关模式没有可锁定的直连导入：按钮保持可用，避免旧回执永久挡住重试。 */
    assert.equal(nodes['native-sync'].disabled, false);
    assert.equal(nodes['native-recover'].disabled, false);
  }

  /* 重启回执优先于「已读回」这条绿色结论。 */
  const restartOverReadback = {
    ...cleanup,
    directMode: { ...directMode, routeCleanupPending: false },
    integration: { ...integration, state: 'ready', adopted: false, nativeImport: { requiresRestart: true } },
  };
  assert.equal(syncState(restartOverReadback).code, 'restart');
  assert.equal(syncState(restartOverReadback).word, '等待 Kimi 重启');
});

/* ---------- 升级后的连接凭据待授权 ---------- */

function makeCredentialEnv() {
  const copy = source.slice(source.indexOf('const APPLY_COPY ='), source.indexOf('\nconst APPLY_REFUSAL ='));
  const blockedCodes = source.slice(source.indexOf('const CREDENTIAL_VAULT_BLOCKED ='), source.indexOf('\nfunction credentialAuthPending'));
  const state = { pendingAction: null, applyUnsupported: false };
  const dom = { restartHead: {}, applyVerdict: {}, applyVerdictDot: {}, applyVerdictWord: {}, applyVerdictNote: {}, applyButton: {} };
  const code = `
    ${copy}
    ${blockedCodes}
    const connectionsChanged = () => false;
    const isInFlight = op => op?.state === 'running';
    const setText = (element, text) => { element.textContent = text; };
    const setHidden = (element, hidden) => { element.hidden = hidden; };
    const setDot = () => {};
    ${sourceFunction('credentialApplyReason')}
    ${sourceFunction('credentialAuthPending')}
    ${sourceFunction('connectionRowsOf')}
    ${sourceFunction('connectionKeyText')}
    ${sourceFunction('isManagedBackend')}
    ${sourceFunction('applyAvailability')}
    ${sourceFunction('applyState')}
    ${sourceFunction('renderApply')}
    return { applyAvailability, applyState, credentialAuthPending, connectionKeyText, isManagedBackend, renderApply };
  `;
  const fn = new Function('state', 'dom', code)(state, dom);
  return { ...fn, dom };
}

test('a cancelled or denied system authorization is reported as what actually happened', () => {
  const { applyState, renderApply, dom } = makeCredentialEnv();
  const failed = (reason) => ({
    connections: { restartRequired: false, apply: { state: 'failed', reason: 'credentials_unavailable' } },
    operation: { action: 'apply-connections', state: 'failed', reason },
  });

  /* 取消：不是失败，也不是 Key 丢失；保留重试入口。 */
  const cancelled = applyState(failed('credential_authorization_cancelled'));
  assert.equal(cancelled.word, '已取消系统授权');
  assert.equal(cancelled.credential, true);
  assert.match(cancelled.note, /没有改动/);
  assert.match(cancelled.note, /允许一次即可完成迁移/);
  assert.doesNotMatch(cancelled.note, /密钥未保存/);
  renderApply(failed('credential_authorization_cancelled'));
  assert.equal(dom.applyButton.hidden, false);
  assert.equal(dom.applyButton.textContent, '重新授权并应用连接');

  /* 拒绝：说明未获授权，仍可重来。 */
  assert.equal(applyState(failed('credential_authorization_failed')).word, '系统未允许读取凭据');

  /* 钥匙串锁定：给出解锁后再重试的下一步。 */
  const locked = applyState(failed('credential_keychain_locked'));
  assert.equal(locked.word, '登录钥匙串已锁定');
  assert.match(locked.note, /解锁/);

  /* 无 GUI 会话：指向应用窗口，而不是让人重输 Key。 */
  const session = applyState(failed('credential_session_unavailable'));
  assert.equal(session.word, '当前会话无法授权');
  assert.match(session.note, /应用窗口/);
  assert.equal(applyState(failed('credential_keychain_unavailable')).word, '登录钥匙串不可用');
  assert.equal(applyState(failed('credential_authorization_required')).word, '连接凭据待授权');

  /* 轮询里只有全局 reason 时，仍回落到待授权。 */
  assert.equal(applyState({
    connections: { restartRequired: false, apply: { state: 'failed', reason: 'credentials_unavailable' } },
  }).word, '连接凭据待授权');

  /* 没失败时不受影响。 */
  assert.equal(applyState({ connections: { restartRequired: false, apply: { state: 'applied' } } }).word, '已应用');
});

test('a connection row says the saved key needs authorization instead of asking for a new key', () => {
  const { connectionKeyText } = makeCredentialEnv();
  for (const code of ['credential_authorization_required', 'credential_keychain_locked',
    'credential_keychain_unavailable', 'credential_session_unavailable']) {
    const text = connectionKeyText({ credentialError: code });
    assert.match(text, /重新授权并应用连接/, code + ' must point at the authorization entry');
    assert.doesNotMatch(text, /重新输入/, code + ' must not ask for a new key');
  }
  assert.match(connectionKeyText({ credentialError: 'credential_store_corrupt' }), /重新输入 Key/);
  assert.equal(connectionKeyText({ credentialError: 'credential_missing' }), '凭据不可读，请编辑并重新输入 Key 后保存');
  assert.equal(connectionKeyText({ apiKeySet: true, credentialError: 'credential_authorization_required' }), 'Key 已在运行环境中');
});

test('an update that leaves credentials unreadable is not reported as fully ready', () => {
  const blockedCodes = source.slice(source.indexOf('const CREDENTIAL_VAULT_BLOCKED ='), source.indexOf('\nfunction credentialAuthPending'));
  const verdict = new Function(
    `${blockedCodes}\n${sourceFunction('connectionRowsOf')}\n${sourceFunction('credentialAuthPending')}\n${sourceFunction('updateCompletionVerdict')}\nreturn updateCompletionVerdict;`,
  )();
  const blocked = { connections: { saved: [{ id: 'c1', credentialError: 'credential_authorization_required' }] } };
  const ready = { connections: { saved: [{ id: 'c1', apiKeySet: true }] } };

  const pending = verdict({ ...blocked, updates: { currentVersion: '1.1.0' } }, '版本替换');
  assert.equal(pending.complete, false);
  assert.equal(pending.tone, 'warn');
  assert.match(pending.text, /1\.1\.0/);
  assert.match(pending.text, /重新授权并应用连接/);
  assert.match(pending.text, /始终允许/);
  assert.doesNotMatch(pending.text, /^版本替换已完成，当前版本 1\.1\.0。$/);

  /* 凭据可读时保留原来的正常完成提示，绿色不降级。 */
  const done = verdict({ ...ready, updates: { currentVersion: '1.1.0' } }, '版本替换');
  assert.equal(done.complete, true);
  assert.equal(done.tone, 'ok');
  assert.equal(done.text, '版本替换已完成，当前版本 1.1.0。');
});

test('the shell banner keeps the credential prompt alive across pages', () => {
  const blockedCodes = source.slice(source.indexOf('const CREDENTIAL_VAULT_BLOCKED ='), source.indexOf('\nfunction credentialAuthPending'));
  const nodes = {
    'restart-banner': { textContent: '', hidden: true },
    'restart-banner-text': { textContent: '', hidden: true },
  };
  const dom = {
    railGatewayDot: {}, railGatewayText: {}, railGatewayUrl: {}, sync: {},
    restartBanner: nodes['restart-banner'], restartBannerText: nodes['restart-banner-text'],
    navModelCount: {}, budgetBanner: {}, budgetBannerText: {},
  };
  const renderShell = new Function('dom', `${blockedCodes}
    const state = { lastSync: 0 };
    const setText = (element, text) => { if (element) element.textContent = text; };
    const setHidden = (element, hidden) => { if (element) element.hidden = hidden; };
    const setDot = () => {};
    const fmtTime = () => '12:00:00';
    const fmtCount = v => String(v);
    const savedIntegrationMode = data => data?.integrationMode === 'direct' ? 'direct' : 'gateway';
    const budgetNotice = () => '';
    ${sourceFunction('credentialAuthPending')}
    ${sourceFunction('connectionRowsOf')}
    ${sourceFunction('renderShell')}
    return renderShell;`)(dom);

  /* 配置未变更、但有连接待授权：横幅仍然出现，并指向重授权。 */
  renderShell({ gateway: { state: 'running' }, configuration: { state: 'ready' },
    connections: { saved: [{ id: 'c1', credentialError: 'credential_authorization_required' }] } });
  assert.equal(nodes['restart-banner'].hidden, false);
  assert.match(nodes['restart-banner-text'].textContent, /部分已保存的连接凭据暂时无法读取/);
  assert.doesNotMatch(nodes['restart-banner-text'].textContent, /钥匙串/);
  assert.match(nodes['restart-banner-text'].textContent, /「模型」页查看原因并重新授权/);

  /* 配置变更提示优先，保持原措辞不被覆盖。 */
  renderShell({ gateway: { state: 'running' }, configuration: { state: 'changed' },
    connections: { saved: [{ id: 'c1', credentialError: 'credential_authorization_required' }] } });
  assert.match(nodes['restart-banner-text'].textContent, /连接配置已修改/);

  /* 都正常时横幅隐藏，原行为不变。 */
  renderShell({ gateway: { state: 'running' }, configuration: { state: 'ready' },
    connections: { saved: [{ id: 'c1', apiKeySet: true }] } });
  assert.equal(nodes['restart-banner'].hidden, true);
});

test('local credential failures explain recovery instead of requesting keychain authorization', () => {
  const start = source.indexOf('const ERROR_HINT = {');
  assert.notEqual(start, -1);
  const end = source.indexOf('\n};', start);
  assert.notEqual(end, -1);
  const hints = new Function(`${source.slice(start, end + 3)}; return ERROR_HINT;`)();
  assert.match(hints.credential_master_key_invalid, /保留应用数据并从备份恢复/);
  assert.match(hints.credential_readback_mismatch, /未验证的值已清理/);
  const keyText = new Function('CREDENTIAL_VAULT_BLOCKED',
    `${sourceFunction('connectionKeyText')}; return connectionKeyText;`)([]);
  const message = keyText({ credentialError: 'credential_master_key_invalid' });
  assert.match(message, /保留应用数据/);
  assert.doesNotMatch(message, /重新授权/);
});

test('renderUpdates displays dual-source mirror status and safe release page links', () => {
  assert.match(htmlSource, /id="upd-release-link"[^>]*target="_blank"[^>]*rel="noopener noreferrer"[^>]*>查看发布页<\/a>/);
  assert.doesNotMatch(htmlSource, /前往 GitHub/);

  const makeEl = () => ({
    textContent: '',
    hidden: false,
    disabled: false,
    checked: false,
    value: '',
    style: {},
    dataset: {},
    attrs: {},
    classes: new Set(),
    classList: { toggle(name, on) { if (on) this.classes.add(name); else this.classes.delete(name); } },
    setAttribute(name, value) { this.attrs[name] = String(value); },
    removeAttribute(name) { delete this.attrs[name]; },
  });
  const ids = [
    'upd-current', 'upd-current-sub', 'upd-state-dot', 'upd-state-word', 'upd-last-check', 'upd-source',
    'upd-channel', 'upd-auto-check', 'upd-release-box', 'upd-release-title', 'upd-release-notes',
    'update-top-banner', 'update-top-banner-text', 'overview-update-card', 'overview-update-title', 'overview-update-notes',
    'upd-progress-wrap', 'upd-progress-bar', 'upd-progress-label',
    'upd-error-box', 'upd-error-msg', 'upd-error-next', 'upd-error-detail',
    'upd-history-note', 'upd-check', 'upd-download', 'upd-cancel', 'upd-rollback', 'upd-release-link',
  ];
  const nodes = Object.fromEntries(ids.map(id => [id, makeEl()]));
  for (const el of Object.values(nodes)) {
    el.classList = { toggle(name, on) { if (on) el.classes.add(name); else el.classes.delete(name); } };
  }
  const $ = id => nodes[id] || null;
  const document = { activeElement: null };
  const state = { view: 'maintenance' };
  const renderUpdates = new Function('$', 'document', 'state', 'setDot', 'setText', 'fmtTime', `
    const credentialAuthPending = () => false;
    const scheduleUpdateFastPoll = () => {};
    const waitForUpdateReconnect = () => {};
    ${sourceFunction('fmtBytes')}
    ${sourceFunction('normalizeUpdateSourceKind')}
    ${sourceFunction('updateSourceLabel')}
    ${sourceFunction('safeHttpUrl')}
    ${sourceFunction('updateStateSummary')}
    ${sourceFunction('renderUpdates')}
    return renderUpdates;
  `)($, document, state, (el, tone) => { if (el) el.dataset.tone = tone; }, (el, text) => { if (el) el.textContent = text; }, () => '12:00:00');

  renderUpdates({
    updates: {
      currentVersion: '1.1.3',
      state: 'idle',
      preferredSource: 'cnb',
      activeSource: '',
      source: 'cnb:wssz527/kimiwork-plus',
      releaseUrl: 'https://cnb.cool/wssz527/kimiwork-plus/-/releases',
    },
  });
  assert.equal(nodes['upd-source'].hidden, false);
  assert.equal(nodes['upd-source'].textContent, '更新源 · 优先国内镜像（CNB），失败自动回退 GitHub');
  assert.equal(nodes['upd-release-link'].hidden, false);
  assert.equal(nodes['upd-release-link'].attrs.href, 'https://cnb.cool/wssz527/kimiwork-plus/-/releases');

  renderUpdates({
    updates: {
      currentVersion: '1.1.3',
      state: 'available',
      preferredSource: 'cnb',
      activeSource: 'cnb',
      releaseUrl: 'https://cnb.cool/wssz527/kimiwork-plus/-/releases',
      available: {
        version: '1.1.4',
        channel: 'stable',
        source: 'cnb',
        releaseUrl: 'https://cnb.cool/wssz527/kimiwork-plus/-/releases/tag/v1.1.4',
      },
    },
  });
  assert.equal(nodes['upd-source'].textContent, '更新源 · 国内镜像（CNB）');
  assert.equal(nodes['upd-release-link'].attrs.href, 'https://cnb.cool/wssz527/kimiwork-plus/-/releases/tag/v1.1.4');

  renderUpdates({
    updates: {
      currentVersion: '1.1.3',
      state: 'up_to_date',
      preferredSource: 'cnb',
      activeSource: 'github',
      releaseUrl: 'javascript:alert(1)',
    },
  });
  assert.equal(nodes['upd-source'].textContent, '更新源 · GitHub（备用源）');
  assert.equal(nodes['upd-release-link'].hidden, true);
  assert.equal(nodes['upd-release-link'].attrs.href, undefined);

  renderUpdates({
    updates: {
      currentVersion: '1.1.3',
      state: 'up_to_date',
      preferredSource: 'github',
      activeSource: 'github',
    },
  });
  assert.equal(nodes['upd-source'].textContent, '更新源 · GitHub');

  renderUpdates({ updates: { currentVersion: '1.1.3', state: 'idle' } });
  assert.equal(nodes['upd-source'].hidden, true);
  assert.equal(nodes['upd-release-link'].hidden, true);
});

test('direct mode stays neutral while official compatibility check is still running', () => {
  const syncState = makeSyncState();
  const nativeText = makeNativeConnectionText();
  const compute = new Function(`
    const displayModelId = id => id;
    const applyState = () => ({ tone: 'ok', word: '已应用', note: '' });
    const budgetNotice = () => '';
    const requestErrorText = () => '';
    const fmtCount = v => String(v);
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('syncState')}
    ${sourceFunction('computeOverviewIssue')}
    return computeOverviewIssue;
  `)();

  const checkingData = {
    gateway: { state: 'running' },
    integrationMode: 'direct',
    compatibility: { status: 'checking' },
    directMode: { mode: 'direct', supported: false, staticHostsReady: false, eligibleModelIds: ['c1/m1'], eligibleModels: 1 },
    models: [{ id: 'c1/m1' }],
    integration: {},
  };

  const verdict = syncState(checkingData);
  assert.equal(verdict.code, 'checking');
  assert.equal(verdict.tone, 'neutral');
  assert.match(verdict.word, /正在检查官方 Kimi 兼容性/);
  assert.match(verdict.note, /稍候自动刷新/);
  assert.match(nativeText(checkingData), /正在检查官方 Kimi 兼容性/);

  const issue = compute(checkingData);
  assert.equal(issue.tone, 'neutral');
  assert.match(issue.title, /正在检查官方 Kimi 兼容性/);
  assert.equal(issue.action, null);

  const { nodes, render } = makeIntegrationModeEnv();
  render(checkingData);
  assert.equal(nodes['direct-mode-notice'].dataset.tone, 'neutral');
  assert.match(nodes['direct-mode-support'].textContent, /正在检查官方 Kimi 兼容性/);
  assert.doesNotMatch(nodes['direct-mode-support'].textContent, /不支持原生直连/);

  const verifiedUnsupported = { ...checkingData, compatibility: { status: 'verified' } };
  assert.equal(syncState(verifiedUnsupported).code, 'unsupported');
  assert.equal(syncState(verifiedUnsupported).tone, 'bad');
  assert.equal(syncState(verifiedUnsupported).word, '当前官方 Kimi 不支持直连');

  const alreadySupported = {
    ...checkingData,
    directMode: { ...checkingData.directMode, supported: true },
  };
  assert.notEqual(syncState(alreadySupported).code, 'checking');
});

test('settings and preview migration copy avoid misleading keychain wording', () => {
  const hintMatch = htmlSource.match(/<p[^>]*id="preview-service-hint"[^>]*>([\s\S]*?)<\/p>/);
  assert.ok(hintMatch, 'missing preview-service-hint');
  assert.match(hintMatch[1], /本机加密凭据保持不变/);
  assert.doesNotMatch(hintMatch[1], /钥匙串/);

  const makeEl = () => ({ textContent: '', hidden: true, disabled: false });
  const nodes = {
    'preview-service-state': makeEl(),
    'preview-service-sub': makeEl(),
    'service-register': makeEl(),
    'service-enable': makeEl(),
    'service-disable': makeEl(),
    'migration-banner': makeEl(),
    'migration-note': makeEl(),
    'migration-actions': makeEl(),
    'migration-top-banner': makeEl(),
    'migration-top-banner-text': makeEl(),
  };
  const renderPreviewLifecycle = new Function('$', 'setText', `
    let trackedServiceOperation = '';
    const renderUpdates = () => {};
    const trackServiceOperation = () => {};
    ${sourceFunction('renderPreviewLifecycle')}
    return renderPreviewLifecycle;
  `)(id => nodes[id] || null, (el, text) => { if (el) el.textContent = text; });

  renderPreviewLifecycle({
    service: { state: 'running', registered: true, loginStartup: 'manual' },
    migration: { status: 'needs_authorization', pendingCount: 2 },
  });
  assert.equal(nodes['migration-banner'].hidden, false);
  assert.match(nodes['migration-note'].textContent, /旧版本凭据条目/);
  assert.doesNotMatch(nodes['migration-note'].textContent, /钥匙串/);

  renderPreviewLifecycle({
    service: { state: 'running', registered: true, loginStartup: 'manual' },
    migration: { status: 'completed', legacyDirPreserved: true },
  });
  assert.match(nodes['migration-note'].textContent, /旧版本凭据条目/);
  assert.doesNotMatch(nodes['migration-note'].textContent, /钥匙串/);
});

test('release running service is recognized as managed backend for connection apply', () => {
  const { isManagedBackend, applyAvailability, applyState, renderApply, dom } = makeCredentialEnv();

  assert.equal(isManagedBackend({ mode: 'release', state: 'running' }), true);
  assert.equal(isManagedBackend({ mode: 'service' }), true);
  assert.equal(isManagedBackend({ mode: 'release', state: 'stopped' }), false);
  assert.equal(isManagedBackend({ mode: 'foreground', state: 'running' }), false);
  assert.equal(isManagedBackend(null), false);

  const releaseRunning = {
    service: { mode: 'release', state: 'running' },
    connections: { restartRequired: true },
  };
  assert.equal(applyAvailability(releaseRunning), 'pending');
  assert.equal(applyState(releaseRunning).word, '等待应用');
  renderApply(releaseRunning);
  assert.equal(dom.applyButton.hidden, false);
  assert.equal(dom.applyButton.disabled, false);
  assert.equal(dom.applyButton.textContent, '应用已保存的连接');
  assert.match(dom.restartHead.textContent, /本面板已连接本机后台服务/);
  assert.doesNotMatch(dom.restartHead.textContent, /不是由受管后台服务运行/);

  const releaseStopped = {
    service: { mode: 'release', state: 'stopped' },
    connections: { restartRequired: true },
  };
  assert.equal(applyAvailability(releaseStopped), 'unsupported');
  assert.equal(applyState(releaseStopped).word, '自动应用不可用');
  renderApply(releaseStopped);
  assert.equal(dom.applyButton.hidden, true);
  assert.match(dom.restartHead.textContent, /未连接运行中的后台服务/);
});

test('subagent role assignment syncs deduplicated pool rows, preserves drafts across mode switches, and strips autoRole on save', async () => {
  const models = [
    { id: 'ds/chat', name: 'DeepSeek', tools: true, reasoning: true, reasoningEfforts: ['low', 'high'] },
    { id: 'glm/flash', name: 'GLM Flash', tools: true, reasoning: false },
    { id: 'kimi/k2', name: 'Kimi K2', tools: true, reasoning: false },
  ];
  const state = {
    key: 'sess',
    stateReadEpoch: 1,
    subagentSaving: false,
    subagentErrors: [],
    subagentModeCache: { force: null, pool: null },
    data: {
      revision: 'rev-1',
      configuration: { state: 'ready' },
      models,
      subagents: { configured: { mode: 'inherit' } },
    },
    draft: null,
    draftDirty: false,
    draftBaseRevision: 'rev-1',
    draftBaseEntity: null,
    lastSync: 0,
  };
  const posted = [];
  const toasts = [];
  const roleConst = source.slice(source.indexOf('const SUBAGENT_ROLES = ['), source.indexOf('\nfunction emptyRoles('));
  const code = `
    ${roleConst}
    const API = { subagents: '/api/subagents' };
    const SUBAGENT_CAPABILITY_MISSING = 'missing';
    const configReady = d => d?.configuration?.state === 'ready';
    const isCapable = m => Boolean(m && m.tools === true);
    const findModel = id => (state.data.models || []).find(m => m.id === id) || null;
    const capableModels = () => (state.data.models || []).filter(isCapable);
    const modelIdFromAlias = () => null;
    const resolveAlias = x => x;
    const declaredEfforts = m => m?.reasoningEfforts || [];
    const renderSubagents = () => {};
    const renderAll = () => {};
    const adoptState = (next) => { state.data = next; };
    const loadState = async () => true;
    const handleSaveFailure = async () => {};
    const showToast = (msg, tone) => toasts.push([msg, tone]);
    const request = async (path, opts) => {
      posted.push({ path, body: opts.body });
      return {
        state: {
          ...state.data,
          revision: 'rev-2',
          subagents: {
            configured: { mode: opts.body.mode, model: opts.body.model, models: opts.body.models, roles: opts.body.roles },
            policy: { mode: opts.body.mode, transport: 'native-config' },
            runtime: { state: 'file-applied', mode: opts.body.mode },
          },
        },
      };
    };
    ${sourceFunction('emptyRoles')}
    ${sourceFunction('normalizeRoles')}
    ${sourceFunction('roleDescriptionFor')}
    ${sourceFunction('isGeneratedRoleDescription')}
    ${sourceFunction('syncRoleAssignmentsToPool')}
    ${sourceFunction('cacheDraftMode')}
    ${sourceFunction('savedTarget')}
    ${sourceFunction('draftFromSaved')}
    ${sourceFunction('sameTarget')}
    ${sourceFunction('recomputeDraftDirty')}
    ${sourceFunction('evaluateSubagentDraft')}
    ${sourceFunction('poolRow')}
    ${sourceFunction('poolDraftIds')}
    ${sourceFunction('setMode')}
    ${sourceFunction('pickModel')}
    ${sourceFunction('pickEffort')}
    ${sourceFunction('pickRoleDefault')}
    ${sourceFunction('pickRoleModel')}
    ${sourceFunction('appendPoolModel')}
    ${sourceFunction('removePoolModel')}
    ${sourceFunction('discardDraft')}
    ${sourceFunction('saveSubagentTarget')}
    return { draftFromSaved, evaluateSubagentDraft, setMode, pickModel, pickEffort, pickRoleDefault, pickRoleModel, appendPoolModel, removePoolModel, discardDraft, saveSubagentTarget };
  `;
  const api = new Function('state', 'posted', 'toasts', code)(state, posted, toasts);
  state.draft = api.draftFromSaved(state.data);
  state.draftBaseEntity = api.draftFromSaved(state.data);

  api.setMode('pool');
  const emptyEval = api.evaluateSubagentDraft(state.draft, state.data);
  assert.equal(emptyEval.problems.some(p => p.includes('未声明工具调用')), false);

  api.pickRoleDefault('ds/chat');
  api.pickRoleModel('plan', 'ds/chat');
  api.pickRoleModel('coder', 'glm/flash');
  api.pickRoleModel('explore', 'kimi/k2');
  assert.equal(state.draft.model, 'ds/chat');
  assert.deepEqual(state.draft.roles, { plan: 'ds/chat', coder: 'glm/flash', explore: 'kimi/k2' });
  assert.deepEqual(state.draft.models.map(r => r.id), ['ds/chat', 'glm/flash', 'kimi/k2']);
  assert.match(state.draft.models[0].description, /默认子代理；用于 plan/);

  // Switching pool -> force -> inherit -> pool must preserve all unsaved pool roles and models.
  api.setMode('force');
  api.pickModel('ds/chat');
  api.pickEffort('high');
  api.setMode('inherit');
  api.setMode('pool');
  assert.equal(state.draft.model, 'ds/chat');
  assert.deepEqual(state.draft.roles, { plan: 'ds/chat', coder: 'glm/flash', explore: 'kimi/k2' });
  assert.deepEqual(state.draft.models.map(r => r.id), ['ds/chat', 'glm/flash', 'kimi/k2']);

  // Switching back to force preserves force model and effort too.
  api.setMode('force');
  assert.equal(state.draft.model, 'ds/chat');
  assert.equal(state.draft.effort, 'high');

  // Switch back to pool, remove explore model, verify role reference is cleared cleanly.
  api.setMode('pool');
  api.removePoolModel('kimi/k2');
  assert.equal(state.draft.roles.explore, '');
  assert.deepEqual(state.draft.models.map(r => r.id), ['ds/chat', 'glm/flash']);

  await api.saveSubagentTarget();
  assert.equal(posted.length, 1);
  assert.equal(posted[0].path, '/api/subagents');
  assert.equal(posted[0].body.mode, 'pool');
  assert.equal(posted[0].body.model, 'ds/chat');
  assert.deepEqual(posted[0].body.roles, { plan: 'ds/chat', coder: 'glm/flash' });
  assert.equal(posted[0].body.models.every(row => !Object.hasOwn(row, 'autoRole')), true);
  assert.match(toasts[0][0], /config\.toml/);

  // Discard resets cached drafts.
  api.setMode('inherit');
  api.discardDraft();
  assert.equal(state.draft.mode, 'pool');
  assert.equal(state.subagentModeCache.pool, null);
});

test('4-step quickstart guide tracks real state, auto-collapses once ready, and supports toggle/dismiss/reopen', () => {
  const storage = new Map();
  const localStorage = {
    getItem: k => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => storage.set(k, String(v)),
    removeItem: k => storage.delete(k),
  };
  const makeEl = () => ({ hidden: false, textContent: '', dataset: {}, attrs: {}, setAttribute(k, v) { this.attrs[k] = String(v); } });
  const nodes = {};
  for (const id of [
    'quickstart-guide', 'quickstart-progress', 'quickstart-toggle',
    'quickstart-step-1', 'quickstart-step-1-dot', 'quickstart-step-1-tag',
    'quickstart-step-2', 'quickstart-step-2-dot', 'quickstart-step-2-tag',
    'quickstart-step-3', 'quickstart-step-3-dot', 'quickstart-step-3-tag',
    'quickstart-step-4', 'quickstart-step-4-dot', 'quickstart-step-4-tag',
  ]) nodes[id] = makeEl();
  const dom = {
    quickstartGuide: nodes['quickstart-guide'],
    quickstartProgress: nodes['quickstart-progress'],
    quickstartToggle: nodes['quickstart-toggle'],
  };
  const state = { guideCollapsed: null, overviewToday: { requests: 0 }, data: null };
  const fn = new Function('$', 'dom', 'state', 'localStorage', `
    const GUIDE_DISMISSED_STORE = 'kwg.panel.guide.dismissed';
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    const setText = (el, t) => { if (el) el.textContent = t; };
    const setDot = (el, tone) => { if (el) el.dataset.tone = tone; };
    const syncState = (d) => d._sync || { code: 'empty' };
    ${sourceFunction('isQuickstartDismissed')}
    ${sourceFunction('dismissQuickstartGuide')}
    ${sourceFunction('reopenQuickstartGuide')}
    ${sourceFunction('toggleQuickstartGuide')}
    ${sourceFunction('evaluateQuickstartSteps')}
    ${sourceFunction('renderQuickstartGuide')}
    return { dismissQuickstartGuide, reopenQuickstartGuide, toggleQuickstartGuide, renderQuickstartGuide };
  `)(id => nodes[id] || null, dom, state, localStorage);

  // Fresh install: 0/4 and expanded.
  state.data = { models: [], _sync: { code: 'empty' }, subagents: { configured: { mode: 'inherit' } }, kimiProcess: { running: false } };
  fn.renderQuickstartGuide(state.data);
  assert.equal(nodes['quickstart-guide'].hidden, false);
  assert.equal(nodes['quickstart-guide'].dataset.collapsed, 'false');
  assert.equal(nodes['quickstart-progress'].textContent, '0 / 4');
  assert.equal(nodes['quickstart-step-3-tag'].textContent, '可选');

  // Models + synced + Kimi running: 3/4 and auto-collapsed.
  state.data = { models: [{ id: 'm1' }], _sync: { code: 'ready' }, subagents: { configured: { mode: 'inherit' } }, kimiProcess: { running: true } };
  fn.renderQuickstartGuide(state.data);
  assert.equal(nodes['quickstart-progress'].textContent, '3 / 4');
  assert.equal(nodes['quickstart-guide'].dataset.collapsed, 'true');
  assert.equal(nodes['quickstart-toggle'].textContent, '展开引导');

  // Toggle expands explicitly.
  fn.toggleQuickstartGuide();
  assert.equal(nodes['quickstart-guide'].dataset.collapsed, 'false');
  assert.equal(nodes['quickstart-toggle'].textContent, '收起');

  // Dismiss hides, reopen restores expanded.
  fn.dismissQuickstartGuide();
  assert.equal(nodes['quickstart-guide'].hidden, true);
  fn.reopenQuickstartGuide();
  assert.equal(nodes['quickstart-guide'].hidden, false);
  assert.equal(nodes['quickstart-guide'].dataset.collapsed, 'false');
});

test('app.js wires subagents view rendering and release link external browser endpoint', () => {
  assert.match(source, /if \(view === 'subagents'\) renderSubagents\(\);/);
  assert.match(source, /request\('\/api\/updates\/open-release',\s*\{\s*method:\s*'POST',\s*body:\s*\{\s*\}\s*\}\)/);
  assert.match(source, /const signature = JSON\.stringify\(\[pool, draft\.model \|\| null, state\.subagentSaving\]\);/);
  assert.match(source, /已保存为继承主模型，但配置文件存在冲突未被覆盖/);
});

test('auto-start, unmanaged Kimi and waiting-work states render clear Chinese guidance without raw codes', async () => {
  const actionStateStart = source.indexOf('const ACTION_STATE = {');
  const actionStateEnd = source.indexOf('\n};', actionStateStart);
  assert.notEqual(actionStateStart, -1);
  assert.notEqual(actionStateEnd, -1);
  const actionStateSource = source.slice(actionStateStart, actionStateEnd + 3);
  const actionLabelStart = source.indexOf('const ACTION_LABEL = {');
  const actionLabelEnd = source.indexOf('\n};', actionLabelStart);
  assert.notEqual(actionLabelStart, -1);
  assert.notEqual(actionLabelEnd, -1);
  const actionLabelSource = source.slice(actionLabelStart, actionLabelEnd + 3);

  const helpers = new Function(`
    ${actionLabelSource}
    ${actionStateSource}
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('operationNote')}
    return { ACTION_STATE, reasonText, operationNote };
  `)();

  for (const stateKey of ['waiting-kimi-exit', 'waiting-login', 'waiting-work', 'waiting-settings', 'requires-restart']) {
    assert.ok(helpers.ACTION_STATE[stateKey], 'missing ACTION_STATE label for ' + stateKey);
  }
  for (const code of [
    'unmanaged_kimi_running',
    'official_kimi_already_running',
    'native_bridge_unavailable',
    'native_bridge_waiting_work',
    'native_bridge_waiting_settings',
    'native_bridge_settings_open_refused',
    'native_bridge_activation_refused',
    'native_bridge_timeout',
    'native_bridge_busy',
    'native_bridge_cancelled',
    'native_bridge_contract_changed',
    'native_bridge_failed',
    'native_bridge_invalid_request',
    'native_bridge_unauthorized',
    'native_import_waiting_login',
    'native_import_account_changed',
    'native_import_account_unverified',
    'native_import_page_reloaded',
    'native_import_busy',
    'native_import_not_verified',
    'native_import_requires_restart',
    'kimi_process_state_unknown',
    'kimi_start_failed',
    'kimi_start_cancelled',
    'kimi_stop_failed',
    'kimi_user_data_dir_invalid',
    'kimi_user_data_dir_unavailable',
    'platform_cannot_manage_child',
    'translation_attach_failed',
    'injection_payload_unavailable',
    'managed_child_unattached',
    'operation_failed',
  ]) {
    const text = helpers.reasonText(code);
    assert.notEqual(text, code, 'reasonText returned raw code: ' + code);
    assert.match(text, /[\u4e00-\u9fff]/, 'reasonText is not Chinese for ' + code);
  }

  assert.match(helpers.operationNote({ action: 'connect-kimi', state: 'waiting-kimi-exit', reason: 'unmanaged_kimi_running' }), /正常退出并重启 Kimi/);
  assert.match(helpers.operationNote({ action: 'connect-kimi', state: 'waiting-login' }), /完成登录/);
  assert.match(helpers.operationNote({ action: 'disconnect-models', state: 'failed', reason: 'native_bridge_waiting_work' }), /Work 页面尚未就绪/);

  const connectionText = makeNativeConnectionText();
  assert.match(connectionText({ integration: { reason: 'native_bridge_waiting_work' } }), /Work 页面尚未就绪/);
  assert.match(connectionText({ integration: { reason: 'native_bridge_unavailable' } }), /同步到 Work/);
  assert.match(connectionText({ integration: { desktop: { reason: 'official_kimi_already_running' } } }), /正常退出并重启 Kimi/);

  /* 直连模式下未同步状态遇到非托管 Kimi、等待 Work 或启动失败，不能退回泛化文案或矛盾提示。 */
  const syncState = makeSyncState();
  const computeOverviewIssue = new Function(`
    const fmtCount = (v) => String(v);
    const budgetNotice = () => '';
    const applyState = () => ({ word: '已应用' });
    const savedGatewayConnections = () => [];
    const savedModelsOf = (d) => (Array.isArray(d?.models) ? d.models : []);
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('syncState')}
    ${sourceFunction('computeOverviewIssue')}
    return computeOverviewIssue;
  `)();
  const directUnmanaged = {
    integrationMode: 'direct',
    gateway: { state: 'running' },
    models: [{ id: 'ds/chat/deepseek-chat' }],
    kimiProcess: { running: true },
    integration: {
      reason: 'direct_import_required',
      desktop: { state: 'needs_user_close', managed: false, reason: 'official_kimi_already_running' },
      directMode: { supported: true, eligibleModels: 1 },
    },
  };
  const directVerdict = syncState(directUnmanaged);
  assert.equal(directVerdict.code, 'manual-exit');
  assert.match(directVerdict.note, /正常退出并重启 Kimi/);
  const directIssue = computeOverviewIssue(directUnmanaged);
  assert.equal(directIssue.action?.action, 'restart-kimi');
  assert.match(connectionText(directUnmanaged), /正常退出并重启 Kimi/);

  const directWaitingWork = {
    integrationMode: 'direct',
    models: [{ id: 'ds/chat/deepseek-chat' }],
    integration: {
      reason: 'direct_import_required',
      nativeImport: { state: 'waiting-work', reason: 'native_bridge_waiting_work' },
      directMode: { supported: true, eligibleModels: 1 },
    },
  };
  assert.match(connectionText(directWaitingWork), /Work 页面尚未就绪/);

  const directStartFailed = {
    integrationMode: 'direct',
    models: [{ id: 'ds/chat/deepseek-chat' }],
    integration: {
      reason: 'direct_import_required',
      desktop: { state: 'failed', reason: 'kimi_start_failed' },
      directMode: { supported: true, eligibleModels: 1 },
    },
  };
  assert.match(connectionText(directStartFailed), /未能自动启动官方 Kimi/);

  const dom = {
    opbar: { hidden: true, dataset: {} },
    opbarDot: { dataset: {} },
    opbarName: { textContent: '' },
    opbarNote: { textContent: '' },
  };
  const renderOperation = new Function('dom', `
    ${actionLabelSource}
    ${actionStateSource}
    ${INTEGRATION_HELPERS.join('\n')}
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    const setDot = (el, t) => { if (el) el.dataset.tone = t; };
    const setText = (el, t) => { if (el) el.textContent = t; };
    ${sourceFunction('isInFlight')}
    ${sourceFunction('operationNote')}
    ${sourceFunction('renderOperation')}
    return renderOperation;
  `)(dom);

  renderOperation({ operation: { action: 'connect-kimi', state: 'waiting-kimi-exit', reason: 'unmanaged_kimi_running' } });
  assert.equal(dom.opbarDot.dataset.tone, 'warn');
  assert.equal(dom.opbarName.textContent, '重新准备接入 · 等待 Kimi 退出');
  assert.match(dom.opbarNote.textContent, /正常退出并重启 Kimi/);

  renderOperation({ operation: { action: 'connect-kimi', state: 'waiting-login' } });
  assert.equal(dom.opbarDot.dataset.tone, 'warn');
  assert.equal(dom.opbarName.textContent, '重新准备接入 · 等待登录 Kimi');

  /* confirmActionResult 与 runAction 的 Toast 文案覆盖。 */
  const toasts = [];
  const actionState = {
    pendingAction: null,
    pendingOpId: null,
    data: null,
  };
  const actionEnv = new Function('state', 'toasts', 'API', 'request', `
    ${actionLabelSource}
    ${actionStateSource}
    ${INTEGRATION_HELPERS.join('\n')}
    const dom = { conn: {}, connText: {} };
    const showToast = (message, tone) => toasts.push({ message, tone });
    const renderActions = () => {};
    const renderOperation = () => {};
    const adoptState = (next) => { state.data = next; };
    const loadState = async () => {};
    const actionUUID = () => 'uuid-1';
    const clearKey = () => {};
    const clearDisconnectConfirm = () => {};
    const stopPolling = () => {};
    const showGate = () => {};
    const setHidden = () => {};
    const setText = () => {};
    ${sourceFunction('isInFlight')}
    ${sourceFunction('runAction')}
    ${sourceFunction('confirmActionResult')}
    return { runAction, confirmActionResult };
  `)(actionState, toasts, { actions: '/api/actions' }, async (_url, opts) => ({
    operation: { id: 'op-' + opts.body.action, action: opts.body.action, state: 'running' },
  }));

  actionState.data = { integration: { desktop: { managed: false } }, kimiProcess: { running: false } };
  await actionEnv.runAction('connect-kimi');
  assert.match(toasts.at(-1).message, /自动启动 Kimi Work 并同步模型配置/);
  actionState.data = { integration: { desktop: { managed: false } }, kimiProcess: { running: false } };
  await actionEnv.runAction('disconnect-models');
  assert.match(toasts.at(-1).message, /自动启动 Kimi Work 并移除本产品接管的模型来源/);
  actionState.data = { integration: { desktop: { managed: true } }, kimiProcess: { running: true } };
  await actionEnv.runAction('connect-kimi');
  assert.match(toasts.at(-1).message, /正在同步模型配置到 Kimi Work/);

  for (const [op, expectedPattern, expectedTone] of [
    [{ id: 'op-1', action: 'connect-kimi', state: 'waiting-kimi-exit', reason: 'unmanaged_kimi_running' }, /正常退出并重启 Kimi/, 'error'],
    [{ id: 'op-2', action: 'connect-kimi', state: 'waiting-login' }, /完成登录/, 'warn'],
    [{ id: 'op-3', action: 'connect-kimi', state: 'requires-restart' }, /正常退出并重启 Kimi/, 'warn'],
    [{ id: 'op-4', action: 'connect-kimi', state: 'failed', reason: 'kimi_start_failed' }, /未能自动启动官方 Kimi/, 'error'],
    [{ id: 'op-5', action: 'disconnect-models', state: 'failed', reason: 'operation_failed' }, /操作未能完成/, 'error'],
  ]) {
    toasts.length = 0;
    actionState.data = { operation: op };
    await actionEnv.confirmActionResult(op.action, op.id);
    assert.equal(toasts.length, 1, 'expected one toast for ' + op.state);
    assert.equal(toasts[0].tone, expectedTone);
    assert.match(toasts[0].message, expectedPattern);
    assert.doesNotMatch(toasts[0].message, /kimi_start_failed|operation_failed|unmanaged_kimi_running/);
  }
});

test('disconnect-models uses inline confirmation without window.confirm', () => {
  const actions = [];
  const timers = [];
  const makeEl = (tag = 'div') => ({
    tag, hidden: true, className: '', textContent: '', dataset: {}, attrs: {}, children: [], listeners: {},
    setAttribute(k, v) { this.attrs[k] = String(v); },
    append(...nodes) { this.children.push(...nodes); },
    replaceChildren(...nodes) { this.children = nodes; },
    addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    focus() {},
  });
  const boxes = {
    'workspace-disconnect-confirm': makeEl('div'),
    'native-disconnect-confirm': makeEl('div'),
  };
  const trigger = makeEl('button');
  trigger.dataset = { action: 'disconnect-models', confirmTarget: 'workspace-disconnect-confirm' };
  const documentStub = {
    createElement: (tag) => makeEl(tag),
    querySelectorAll: () => [trigger],
  };
  const windowStub = {
    setTimeout: (fn, ms) => { timers.push({ fn, ms, cleared: false }); return timers.length; },
    clearTimeout: (id) => { if (timers[id - 1]) timers[id - 1].cleared = true; },
  };
  const state = { data: { integrationMode: 'direct' }, disconnectConfirm: null };
  const api = new Function('state', '$', 'document', 'window', 'setHidden', 'runAction', `
    ${sourceFunction('clearDisconnectConfirm')}
    ${sourceFunction('requestDisconnectConfirm')}
    return { clearDisconnectConfirm, requestDisconnectConfirm };
  `)(state, id => boxes[id] || null, documentStub, windowStub, (el, h) => { if (el) el.hidden = Boolean(h); },
    (action, mode) => actions.push({ action, mode }));

  assert.equal(api.requestDisconnectConfirm(trigger), true);
  const box = boxes['workspace-disconnect-confirm'];
  assert.equal(box.hidden, false);
  assert.equal(trigger.attrs['aria-expanded'], 'true');
  assert.match(box.children[0].textContent, /确定移除 KimiWork \+ 接管的模型来源吗/);
  assert.match(box.children[0].textContent, /静态 hosts 也会保留/);
  const [confirmBtn, cancelBtn] = box.children[1].children;
  cancelBtn.listeners.click[0]();
  assert.equal(box.hidden, true);
  assert.equal(actions.length, 0);

  api.requestDisconnectConfirm(trigger);
  const [confirmBtn2] = box.children[1].children;
  confirmBtn2.listeners.click[0]();
  assert.equal(box.hidden, true);
  assert.deepEqual(actions, [{ action: 'disconnect-models', mode: undefined }]);

  api.requestDisconnectConfirm(trigger);
  assert.equal(box.hidden, false);
  timers.at(-1).fn();
  assert.equal(box.hidden, true);
});

test('connectionDraftFrom preserves credentialError through connection row rendering while connectionPayload omits it', () => {
  const makeEl = (tag = 'div') => ({
    tag, className: '', textContent: '', dataset: {}, attrs: {}, children: [], disabled: false, type: '',
    setAttribute(k, v) { this.attrs[k] = String(v); },
    append(...nodes) { this.children.push(...nodes); },
  });
  const textOf = (node) => (node.textContent || '') + (node.children || []).map(textOf).join(' ');
  const api = new Function('document', 'state', `
    const CREDENTIAL_VAULT_BLOCKED = ['credential_keychain_locked', 'credential_keychain_unavailable',
      'credential_session_unavailable', 'credential_authorization_required', 'credential_authorization_failed'];
    const DIAG_REASON = {};
    const UPSTREAM_REASON = {};
    const fmtCount = n => String(n);
    const fmtMs = n => n + 'ms';
    const providerPreset = () => null;
    ${sourceFunction('connectionRowsOf')}
    ${sourceFunction('connectionDraftRow')}
    ${sourceFunction('connectionDraftFrom')}
    ${sourceFunction('connectionsSignature')}
    ${sourceFunction('connectionPayload')}
    ${sourceFunction('protocolLabel')}
    ${sourceFunction('upstreamReasonText')}
    ${sourceFunction('connectionKeyText')}
    ${sourceFunction('buildConnectionRow')}
    return { connectionDraftFrom, connectionsSignature, connectionPayload, buildConnectionRow };
  `)({ createElement: (tag) => makeEl(tag) }, { connsSaving: false, connChecks: {}, data: { configuration: { state: 'ready' } } });

  const blockedData = {
    connections: {
      saved: [{ id: 'ds', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', apiKeySet: false, credentialError: 'credential_authorization_required' }],
    },
  };
  const clearedData = {
    connections: {
      saved: [{ id: 'ds', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', apiKeySet: false, credentialError: '' }],
    },
  };
  const draftBlocked = api.connectionDraftFrom(blockedData);
  const draftCleared = api.connectionDraftFrom(clearedData);
  assert.equal(draftBlocked[0].credentialError, 'credential_authorization_required');
  assert.notEqual(api.connectionsSignature(draftBlocked), api.connectionsSignature(draftCleared));
  const payload = api.connectionPayload(draftBlocked);
  assert.equal('credentialError' in payload[0], false);
  const rowEl = api.buildConnectionRow(draftBlocked[0], 0);
  assert.match(textOf(rowEl), /已保存 Key 待系统授权/);
});

test('cold-start /api/state failure hides fullscreen gate so connection banner and retry stay accessible', () => {
  const dom = {
    conn: { hidden: true },
    connText: { textContent: '' },
    sync: { textContent: '' },
    shell: { hidden: true },
    gate: { hidden: false },
  };
  const state = { data: null, started: false, lastSync: null };
  const calls = [];
  const handleApiFailure = new Function('state', 'dom', 'calls', `
    const clearKey = () => calls.push('clearKey');
    const stopPolling = () => calls.push('stopPolling');
    const showGate = (kind) => { dom.gate.hidden = false; calls.push('gate:' + kind); };
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    const setText = (el, v) => { if (el) el.textContent = v; };
    const fmtTime = () => '12:00';
    const renderLoading = () => calls.push('loading');
    ${sourceFunction('handleApiFailure')}
    return handleApiFailure;
  `)(state, dom, calls);

  handleApiFailure({ code: 'NETWORK', message: 'offline' });
  assert.equal(dom.conn.hidden, false);
  assert.equal(dom.shell.hidden, false);
  assert.equal(dom.gate.hidden, true);
  assert.match(dom.connText.textContent, /无法连接本机面板服务/);

  dom.gate.hidden = false;
  handleApiFailure({ code: 'SERVER_ERROR', status: 502, message: '上游重启中' });
  assert.equal(dom.gate.hidden, true);
  assert.equal(dom.shell.hidden, false);

  handleApiFailure({ code: 'unauthorized' });
  assert.equal(dom.gate.hidden, false);
  assert.ok(calls.includes('gate:expired'));
});

test('direct mode renderLaunch avoids unnecessary restart and renderOnboardSteps accepts ready', () => {
  const launch = { dataset: {}, setAttribute() {} };
  const steps = ['connection', 'catalogue', 'apply', 'sync'].map((step) => ({ dataset: { step, done: 'false' } }));
  const dom = {
    onboardLaunch: launch,
    onboardLaunchHint: { hidden: true, textContent: '' },
    onboardSteps: { querySelectorAll: () => steps },
  };
  const api = new Function('dom', `
    const ACTION_LABEL = { 'open-work': '打开 Kimi Work', 'launch-kimi': '启动 Kimi Work', 'restart-kimi': '重启 Kimi' };
    const state = { data: { connections: { saved: [{ id: 'ds', baseURL: 'https://api.deepseek.com/v1' }] } } };
    const isInFlight = () => false;
    const syncState = data => ({ code: data.sync });
    const connectionsChanged = () => false;
    const connectionRowsOf = (data) => data?.connections?.saved || [];
    const setText = (el, t) => { if (el) el.textContent = t; };
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    ${sourceFunction('savedOnboardTarget')}
    ${sourceFunction('mountedSavedModels')}
    ${sourceFunction('recoveryCopy')}
    ${sourceFunction('renderLaunch')}
    ${sourceFunction('renderOnboardSteps')}
    return { renderLaunch, renderOnboardSteps };
  `)(dom);

  api.renderLaunch({ integrationMode: 'direct', kimiProcess: { running: true }, integration: { desktop: { managed: false } }, sync: 'ready' });
  assert.equal(launch.dataset.action, 'open-work');
  api.renderLaunch({ integrationMode: 'direct', kimiProcess: { running: true }, integration: { desktop: { managed: false } }, sync: 'restart' });
  assert.equal(launch.dataset.action, 'restart-kimi');

  const done = api.renderOnboardSteps(
    { integrationMode: 'direct', sync: 'ready', models: [{ id: 'ds/chat/r1', connectionId: 'ds' }], connections: { saved: [{ id: 'ds' }] } },
    { selected: 1, rows: 1 },
  );
  assert.equal(done.sync, true);
  assert.equal(steps.find((s) => s.dataset.step === 'sync').dataset.done, 'true');
});

test('renderConnectionEditor protects active or composing inputs, respects managed providerFlow, and applyProviderPreset clears ceKey', () => {
  const dispatched = [];
  const syncCalls = [];
  const dom = {
    connEditor: { hidden: true },
    connEditorTitle: { textContent: '' },
    ceName: { value: '正在输入中' },
    ceId: { value: 'ds', disabled: false },
    ceBase: { value: 'https://api.deepseek.com/v1' },
    ceKeyEnv: { value: '' },
    ceKey: { value: 'sk-old-secret', placeholder: '' },
    ceChat: { checked: false },
    ceResponses: { checked: false },
    cePreset: { value: '', dataset: { providerFlow: 'managed' }, dispatchEvent(ev) { dispatched.push(ev.type); } },
    cePresetHint: { textContent: '' },
    ceKeyHint: { textContent: '' },
    connEditorTest: { disabled: false, hidden: false, textContent: '' },
  };
  const documentStub = { activeElement: dom.ceName };
  const state = {
    composing: false,
    providers: { presets: [{ id: 'deepseek', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', protocols: ['chat'] }] },
    conns: {
      editing: {
        originalId: null,
        keyEdited: true,
        discovering: false,
        draft: { id: 'ds', name: 'DeepSeek 官方', providerPresetId: 'deepseek', baseURL: 'https://api.deepseek.com/v1', apiKeyEnv: '', protocols: ['chat'], apiKey: 'sk-old-secret' },
      },
    },
  };
  const api = new Function('state', 'dom', 'document', 'syncCalls', `
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    const setText = (el, t) => { if (el) el.textContent = t; };
    const syncSelectOptions = (...args) => syncCalls.push(args);
    const renderConnections = () => {};
    ${sourceFunction('isInputActive')}
    ${sourceFunction('providerPresets')}
    ${sourceFunction('providerPreset')}
    ${sourceFunction('providerPresetLabel')}
    ${sourceFunction('providerPresetOptions')}
    ${sourceFunction('presetsSuggestedId')}
    ${sourceFunction('renderConnectionEditor')}
    ${sourceFunction('applyProviderPreset')}
    return { renderConnectionEditor, applyProviderPreset };
  `)(state, dom, documentStub, syncCalls);

  api.renderConnectionEditor();
  assert.equal(dom.ceName.value, '正在输入中', 'focused input must not be overwritten');
  assert.equal(syncCalls.length, 0, 'managed providerFlow select options must not be overwritten');
  assert.equal(dom.cePreset.dataset.desiredPreset, 'deepseek');
  assert.deepEqual(dispatched, ['kwp:sync-provider-flow']);

  documentStub.activeElement = null;
  state.composing = true;
  api.renderConnectionEditor();
  assert.equal(dom.ceName.value, '正在输入中', 'IME composing must protect inputs');

  state.composing = false;
  api.applyProviderPreset('deepseek');
  assert.equal(state.conns.editing.keyEdited, false);
  assert.equal(state.conns.editing.draft.apiKey, '');
  assert.equal(dom.ceKey.value, '');
});

test('search service 400 error persists across renderServices until input changes, and adoptState syncs services & pendingService', async () => {
  const renderedErrors = [];
  const dom = {
    svcPanel: {}, svcUnavailable: {}, svcSave: {}, svcReset: {}, svcDraft: {},
    svcSearchOn: { checked: true }, svcFetchOn: { checked: false },
    svcSearchUrl: { value: 'https://search.example.com' }, svcFetchUrl: { value: '' },
    svcSearchKey: { value: 'secret-token' }, svcFetchKey: { value: '' },
    svcSearchSaved: {}, svcSearchSavedSub: {}, svcFetchSaved: {}, svcFetchSavedSub: {},
    svcSearchKeyHint: {}, svcVerdictDot: {}, svcVerdictWord: {}, svcVerdictNote: {}, svcVerdict: { dataset: {} },
    svcErrors: {},
  };
  const state = {
    composing: false,
    data: {
      configuration: { state: 'ready' },
      search: { services: { search: { enabled: false, endpoint: '', hasKey: false }, fetch: { enabled: false, endpoint: '', hasKey: false } } },
    },
    services: {
      base: '',
      baseRow: { search: { enabled: false, endpoint: '', hasKey: false }, fetch: { enabled: false, endpoint: '', hasKey: false } },
      draft: { searchOn: true, searchUrl: 'https://search.example.com', fetchOn: false, fetchUrl: '' },
      dirty: true, saving: false, searchKey: 'secret-token', fetchKey: '', serverErrors: [],
    },
    draft: null, draftDirty: false, conns: { dirty: false, editing: null, draft: null },
    pendingService: { enabled: true, state: 'running', registered: true },
  };
  const svcFns = new Function('state', 'dom', 'renderedErrors', 'request', `
    const API = { context: '/api/context' };
    const LOOPBACK = new Set(['127.0.0.1', 'localhost', '::1', '[::1]']);
    const SVC_STATUS = { inherit: { tone: 'neutral', word: '官方', note: '' } };
    const SVC_REASON = {};
    const setHidden = () => {};
    const setText = () => {};
    const setDot = () => {};
    const showToast = () => {};
    const loadState = async () => {};
    const handleSaveFailure = async () => {};
    const renderErrorList = (_el, list) => renderedErrors.push(list.slice());
    const draftFromSaved = () => ({ mode: 'inherit' });
    const recomputeDraftDirty = () => {};
    const savedConnectionsSignature = () => '';
    const connectionDraftFrom = () => [];
    const connectionsDirty = () => false;
    const onboardTouched = () => true;
    const fillOnboardForm = () => {};
    const settlePending = () => {};
    ${sourceFunction('isInputActive')}
    ${sourceFunction('normalizeBaseURL')}
    ${sourceFunction('searchStateOf')}
    ${sourceFunction('searchAvailable')}
    ${sourceFunction('serviceRowOf')}
    ${sourceFunction('serviceDraftFrom')}
    ${sourceFunction('serviceSignature')}
    ${sourceFunction('servicesDirty')}
    ${sourceFunction('validateServiceDraft')}
    ${sourceFunction('servicePayload')}
    ${sourceFunction('evaluateServices')}
    ${sourceFunction('serviceRowText')}
    ${sourceFunction('servicesObservedText')}
    ${sourceFunction('renderServices')}
    ${sourceFunction('readServiceForm')}
    ${sourceFunction('fillServiceForm')}
    ${sourceFunction('saveSearchServices')}
    ${sourceFunction('adoptState')}
    return { saveSearchServices, renderServices, readServiceForm, adoptState };
  `)(state, dom, renderedErrors, async () => {
    const err = new Error('后端拒绝：端点不可达');
    err.status = 400;
    throw err;
  });

  await svcFns.saveSearchServices();
  assert.deepEqual(state.services.serverErrors, ['后端拒绝：端点不可达']);
  assert.deepEqual(renderedErrors.at(-1), ['后端拒绝：端点不可达']);
  svcFns.renderServices(state.data);
  assert.deepEqual(renderedErrors.at(-1), ['后端拒绝：端点不可达'], '400 error must survive subsequent renders');
  svcFns.readServiceForm();
  assert.deepEqual(state.services.serverErrors, [], 'editing input clears server error');

  state.services.dirty = false;
  const nextState = {
    revision: 'rev-2',
    search: { services: { search: { enabled: true, endpoint: 'https://new.example.com', hasKey: true }, fetch: { enabled: false, endpoint: '', hasKey: false } } },
  };
  svcFns.adoptState(nextState);
  assert.equal(state.services.draft.searchUrl, 'https://new.example.com');
  assert.equal(state.services.baseRow.search.hasKey, true);
  assert.equal(state.data.service.loginStartup, 'enabled');
  assert.equal(state.pendingService, null);
});

test('setText preserves button SVG icons via .btn__label, fmtTokens(0) returns 0, and applyConnections limits Keychain copy to legacy migration', async () => {
  const label = { textContent: '旧文案' };
  const btn = { textContent: '不应改整个按钮', querySelector: (sel) => sel === '.btn__label' ? label : null };
  const { setText, fmtTokens } = new Function(`
    ${sourceFunction('setText')}
    ${sourceFunction('fmtTokens')}
    return { setText, fmtTokens };
  `)();
  setText(btn, '新文案');
  assert.equal(label.textContent, '新文案');
  assert.equal(btn.textContent, '不应改整个按钮');
  assert.equal(fmtTokens(0), '0');
  assert.equal(fmtTokens(-1), '未知');
  assert.match(fmtTokens(128000), /128,000 tokens/);

  const toasts = [];
  const runApply = (platform, pending) => {
    const state = { data: { connections: { saved: [] } }, pendingAction: null, pendingOpId: null };
    const fn = new Function('state', 'document', 'toasts', 'pending', `
      const API = { actions: '/api/actions' };
      const dom = { applyButton: {} };
      const APPLY_COPY = { unsupported: { tone: 'bad', word: '', note: '' } };
      const APPLY_REFUSAL = {};
      const isInFlight = () => false;
      const actionUUID = () => 'u1';
      const renderApply = () => {};
      const adoptState = () => {};
      const request = async () => ({ operation: { id: 'op1', state: 'requested' } });
      const loadState = async () => {};
      const showToast = (message, tone) => toasts.push({ message, tone });
      const credentialAuthPending = () => pending;
      ${sourceFunction('applyConnections')}
      return applyConnections;
    `)(state, { documentElement: { dataset: { platform } } }, toasts, pending);
    return fn();
  };
  await runApply('mac', false);
  assert.doesNotMatch(toasts.at(-1).message, /始终允许|钥匙串/);
  await runApply('windows', true);
  assert.doesNotMatch(toasts.at(-1).message, /macOS|钥匙串|始终允许/);
  await runApply('mac', true);
  assert.match(toasts.at(-1).message, /待迁移.*登录钥匙串.*始终允许/);
});

test('app.js enforces single visibilitychange listener, external adoptState on migration-resume, updated copy, and safe finally blocks', () => {
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const workText = readFileSync(new URL('./app-work.js', import.meta.url), 'utf8');
  assert.equal((appText.match(/addEventListener\('visibilitychange'/g) || []).length, 1);
  assert.match(appText, /adoptState\(result\.state,\s*\{\s*external:\s*true\s*\}\)/);
  assert.equal(appText.includes('finally { button.disabled = false; }'), false);
  assert.equal(appText.includes('finally { updDownload.disabled = false; }'), false);
  assert.match(htmlSource, /联网搜索、调用 Kimi 资料库，以及 PPT、3D、图片生成、视频生成等能力仍走 Kimi Work 官方自身额度；KimiWork \+ 仅接管对话与子代理模型请求/);
  assert.match(htmlSource, /角色分配写入 <code>config\.toml<\/code> 模型池用途说明，供主模型派发子代理时参考选用/);
  assert.match(workText, /角色分配已写入 config\.toml 模型池用途说明，供主模型派发 plan \/ coder \/ explore 子代理时参考选用，新建会话后生效/);
  assert.equal(htmlSource.includes('连接与工具'), false);
  assert.equal(source.includes('MCP_NAME_PATTERN'), false);
  assert.equal(source.includes('dom.legacyPanel'), false);
});

test('growth.js replaces MCP window.confirm with inline confirm, opens https links via POST /api/open-url, and preserves ctxDraft on save error', async () => {
  const growthSource = readFileSync(new URL('./growth.js', import.meta.url), 'utf8');
  assert.equal(growthSource.includes('window.confirm'), false);
  assert.equal(growthSource.includes("target = '_blank'"), false);
  assert.equal(growthSource.includes('target="_blank"'), false);

  const elements = new Map();
  const makeNode = (tag = 'div', id = '') => {
    const el = {
      tag, id, className: '', textContent: '', value: '', checked: false, hidden: false, disabled: false,
      href: '', rel: '', title: '', dataset: {}, children: [], listeners: {}, parentNode: null,
      append(...nodes) { for (const n of nodes) { if (n && typeof n === 'object') { n.parentNode = this; if (n.id) elements.set(n.id, n); } this.children.push(n); } },
      replaceChildren(...nodes) { this.children = []; this.append(...nodes); },
      before(...nodes) { for (const n of nodes) { if (n && typeof n === 'object' && n.id) elements.set(n.id, n); } },
      after(...nodes) { for (const n of nodes) { if (n && typeof n === 'object' && n.id) elements.set(n.id, n); } },
      insertBefore(n) { if (n && typeof n === 'object' && n.id) elements.set(n.id, n); },
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
      setAttribute() {},
      closest() { return makeNode('label'); },
      querySelector() { return null; },
      focus() {},
    };
    if (id) elements.set(id, el);
    return el;
  };
  const getEl = (id) => {
    if (elements.has(id)) return elements.get(id);
    if (String(id).startsWith('provider-document-')) return null;
    return makeNode('div', id);
  };
  getEl('ob-preset').value = 'deepseek';
  getEl('context-inherit').checked = true;

  const prevDoc = globalThis.document;
  const prevWin = globalThis.window;
  const calls = [];
  const toasts = [];
  let failContext = true;
  globalThis.document = {
    getElementById: getEl,
    createElement: (tag) => makeNode(tag),
    createTextNode: (text) => ({ textContent: text }),
  };
  globalThis.window = { setTimeout: () => 1, clearTimeout: () => {} };
  try {
    const mod = {};
    new Function('globalThis', growthSource)(mod);
    const data = {
      models: [],
      translation: { enabled: false },
      context: { status: 'inherit', policy: { mode: 'inherit' } },
      tools: { available: true, servers: [{ name: 'fs', transport: 'stdio', enabled: true, command: 'npx' }] },
      webTools: { provider: 'official' },
    };
    const providers = {
      presets: [{ id: 'deepseek', name: 'DeepSeek', models: [], keyUrl: 'https://platform.deepseek.com/api_keys' }],
    };
    const render = mod.KimiWorkGrowth.mountGrowth({
      request: async (path, opts) => {
        calls.push({ path, opts });
        if (path === '/api/context' && failContext) throw { message: '写入失败' };
        if (path === '/api/tools') return { tools: { available: true, servers: [] }, result: { state: 'removed' } };
        return { ok: true };
      },
      getState: () => data,
      getProviders: () => providers,
      refresh: async () => {},
      notify: (msg, tone) => toasts.push({ msg, tone }),
      runAction: async () => {},
    });

    render(data);
    // MCP inline remove flow: first click opens confirm bar without calling /api/tools.
    const findBy = (root, pred) => {
      if (!root) return null;
      if (pred(root)) return root;
      for (const c of root.children || []) { const hit = findBy(c, pred); if (hit) return hit; }
      return null;
    };
    const removeBtn = findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove');
    assert.ok(removeBtn);
    await elements.get('mcp-list').listeners.click[0]({ target: { closest: () => removeBtn } });
    assert.equal(calls.some(c => c.path === '/api/tools'), false);
    const confirmBtn = findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove-confirm');
    assert.ok(confirmBtn);
    assert.match(confirmBtn.className, /btn--danger/);
    await elements.get('mcp-list').listeners.click[0]({ target: { closest: () => confirmBtn } });
    assert.equal(calls.some(c => c.path === '/api/tools' && c.opts?.body?.action === 'remove'), true);

    // Re-entrant clicks while MCP request is in flight must be ignored and confirm bar must collapse immediately.
    data.tools = { available: true, servers: [{ name: 'git', transport: 'stdio', enabled: true, command: 'npx' }] };
    let resolveInFlight;
    const prevToolsCount = calls.filter(c => c.path === '/api/tools').length;
    let blockTools = true;
    const guardedRender = mod.KimiWorkGrowth.mountGrowth({
      request: (path, opts) => {
        calls.push({ path, opts });
        if (path === '/api/tools' && blockTools) {
          return new Promise((resolve) => {
            resolveInFlight = () => resolve({ tools: { available: true, servers: [] }, result: { state: 'removed' } });
          });
        }
        return Promise.resolve({ ok: true });
      },
      getState: () => data,
      getProviders: () => providers,
      refresh: async () => {},
      notify: (msg, tone) => toasts.push({ msg, tone }),
      runAction: async () => {},
    });
    guardedRender(data);
    const rmBtn2 = findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove' && n.dataset?.mcpName === 'git');
    assert.ok(rmBtn2);
    await elements.get('mcp-list').listeners.click.at(-1)({ target: { closest: () => rmBtn2 } });
    const cfBtn2 = findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove-confirm' && n.dataset?.mcpName === 'git');
    assert.ok(cfBtn2);
    const pendingPromise = elements.get('mcp-list').listeners.click.at(-1)({ target: { closest: () => cfBtn2 } });
    // Before request resolves, confirm bar must already be collapsed and duplicate clicks ignored.
    assert.equal(findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove-confirm'), null);
    assert.equal(findBy(elements.get('mcp-list'), n => n.dataset?.mcpAction === 'remove').disabled, true);
    await elements.get('mcp-list').listeners.click.at(-1)({ target: { closest: () => cfBtn2 } });
    assert.equal(calls.filter(c => c.path === '/api/tools').length, prevToolsCount + 1);
    blockTools = false;
    resolveInFlight();
    await pendingPromise;

    // External API key link calls POST /api/open-url instead of target="_blank".
    const docNote = elements.get('provider-document-ob-preset');
    const link = findBy(docNote, n => n.tag === 'a');
    assert.ok(link);
    assert.equal(link.href, 'https://platform.deepseek.com/api_keys');
    let prevented = false;
    await link.listeners.click[0]({ preventDefault() { prevented = true; } });
    assert.equal(prevented, true);
    assert.deepEqual(calls.find(c => c.path === '/api/open-url'), {
      path: '/api/open-url',
      opts: { method: 'POST', body: { url: 'https://platform.deepseek.com/api_keys' } },
    });

    // Context save failure must keep draft dirty (not overwrite local.ctxDraft).
    elements.get('context-inherit').checked = false;
    elements.get('context-custom').checked = true;
    elements.get('context-ratio').value = '0.85';
    elements.get('context-reserved').value = '16000';
    await elements.get('context-save').listeners.click[0]();
    render(data);
    assert.equal(elements.get('context-draft').textContent, '有未保存的修改');
  } finally {
    globalThis.document = prevDoc;
    globalThis.window = prevWin;
  }
});

test('renderAll and setView render services on maintenance, onboard overview on overview, and pass view to plusPanel', () => {
  const calls = [];
  const state = { data: { revision: 'r1' }, view: 'overview', key: 'k', composing: true, onboard: {} };
  const dom = { viewTitle: {}, viewLede: {} };
  const VIEW_ALIASES = {};
  const VIEW_COPY = {
    overview: { title: '总览', lede: '' },
    models: { title: '模型', lede: '' },
    subagents: { title: '子代理', lede: '' },
    usage: { title: '用量', lede: '' },
    maintenance: { title: '设置', lede: '' },
  };
  const api = new Function('state', 'dom', 'calls', 'VIEW_ALIASES', 'VIEW_COPY', `
    const VIEW_STORE = 'v';
    const document = { body: { dataset: {} } };
    const sessionStorage = { setItem() {} };
    const $ = () => null;
    const clearDisconnectConfirm = () => calls.push('clearDisconnect');
    const renderShell = () => calls.push('shell');
    const renderOperation = () => calls.push('op');
    const renderActions = () => calls.push('actions');
    const renderPreviewLifecycle = () => calls.push('lifecycle');
    const renderOverview = (_d, opts) => calls.push('overview:' + opts?.view);
    const renderOnboardOverview = () => calls.push('onboardOverview');
    const renderWorkspaceSync = () => calls.push('workspaceSync');
    const renderOnboard = () => calls.push('onboard');
    const renderModels = () => calls.push('models');
    const renderConnection = () => calls.push('connection');
    const renderPicker = () => calls.push('picker');
    const renderSubagents = () => calls.push('subagents');
    const renderIntegrationMode = () => calls.push('integrationMode');
    const renderServices = () => calls.push('services');
    const renderGrowth = (_d, opts) => calls.push('growth:' + opts?.view);
    const renderUpdates = () => calls.push('updates');
    const usagePanel = { show() { calls.push('usageShow'); } };
    const plusPanel = {
      render(_d, opts) { calls.push('plus:' + opts?.view); },
      refresh() { calls.push('plusRefresh'); },
    };
    ${sourceFunction('setView')}
    ${sourceFunction('renderAll')}
    return { setView, renderAll };
  `)(state, dom, calls, VIEW_ALIASES, VIEW_COPY);

  api.renderAll();
  assert.ok(calls.includes('overview:overview'));
  assert.ok(calls.includes('onboardOverview'));
  assert.ok(calls.includes('plus:overview'));
  assert.equal(calls.includes('services'), false);

  calls.length = 0;
  state.composing = true;
  api.setView('maintenance', { persist: false });
  assert.equal(state.composing, false, 'setView must reset state.composing');
  assert.ok(calls.includes('services'), 'setView(maintenance) must render search services');
  assert.ok(calls.includes('plus:maintenance'));

  calls.length = 0;
  api.renderAll();
  assert.ok(calls.includes('services'), 'renderAll on maintenance must refresh search services');
  assert.ok(calls.includes('plus:maintenance'));
});

test('openConnectionEditor forces field populate even when an input is focused or composing, and applyProviderPreset resets secretSet', () => {
  const dom = {
    connEditor: { hidden: true },
    connEditorTitle: { textContent: '' },
    ceName: { value: '残留旧名', focus() {} },
    ceId: { value: 'old-id', disabled: false },
    ceBase: { value: 'https://old.example.com/v1' },
    ceKeyEnv: { value: 'OLD_ENV' },
    ceKey: { value: 'sk-stale-secret', placeholder: '' },
    ceChat: { checked: false },
    ceResponses: { checked: false },
    cePreset: { value: 'old', dataset: {} },
    cePresetHint: { textContent: '' },
    ceKeyHint: { textContent: '' },
    connEditorTest: { disabled: false, hidden: false, textContent: '' },
  };
  const documentStub = { activeElement: dom.ceName };
  const state = {
    composing: true,
    data: { revision: 'rev-1' },
    providers: { presets: [{ id: 'deepseek', name: 'DeepSeek', baseURL: 'https://api.deepseek.com/v1', protocols: ['chat'] }] },
    conns: {
      baseRevision: 'rev-1',
      editing: null,
      draft: [
        { id: 'conn-a', name: '连接 A', baseURL: 'https://a.example.com/v1', apiKeySet: true, protocols: ['chat'] },
        { id: 'conn-b', name: '连接 B', baseURL: 'https://b.example.com/v1', apiKeySet: false, protocols: ['responses'] },
      ],
    },
  };
  const api = new Function('state', 'dom', 'document', `
    const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
    const CONNECTION_ID_HINT = 'bad id';
    const LOOPBACK = new Set(['127.0.0.1', 'localhost']);
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    const setText = (el, t) => { if (el) el.textContent = t; };
    const syncSelectOptions = (sel, _opts, val) => { sel.value = val; };
    const renderConnections = () => renderConnectionEditor();
    ${sourceFunction('isInputActive')}
    ${sourceFunction('normalizeBaseURL')}
    ${sourceFunction('connectionDraftRow')}
    ${sourceFunction('providerPresets')}
    ${sourceFunction('providerPreset')}
    ${sourceFunction('providerPresetLabel')}
    ${sourceFunction('providerPresetOptions')}
    ${sourceFunction('presetsSuggestedId')}
    ${sourceFunction('validateConnectionDraft')}
    ${sourceFunction('openConnectionEditor')}
    ${sourceFunction('closeConnectionEditor')}
    ${sourceFunction('readConnectionEditor')}
    ${sourceFunction('applyProviderPreset')}
    ${sourceFunction('renderConnectionEditor')}
    return { openConnectionEditor, closeConnectionEditor, readConnectionEditor, applyProviderPreset, validateConnectionDraft };
  `)(state, dom, documentStub);

  // Switching to conn-b while ceName is focused and composing=true must overwrite stale values & clear ceKey.
  api.openConnectionEditor(state.conns.draft[1]);
  assert.equal(state.composing, false);
  assert.equal(dom.ceName.value, '连接 B');
  assert.equal(dom.ceId.value, 'conn-b');
  assert.equal(dom.ceBase.value, 'https://b.example.com/v1');
  assert.equal(dom.ceKey.value, '');

  // New connection: typing a key sets secretSet=true; switching preset must reset secretSet=false so empty key fails validation.
  api.openConnectionEditor(null);
  dom.ceName.value = '新建连接';
  dom.ceId.value = 'new-conn';
  dom.ceBase.value = 'https://api.example.com/v1';
  dom.ceKey.value = 'sk-typed-before-switch';
  api.readConnectionEditor();
  assert.equal(state.conns.editing.draft.secretSet, true);
  api.applyProviderPreset('deepseek');
  assert.equal(state.conns.editing.draft.apiKey, '');
  assert.equal(state.conns.editing.draft.secretSet, false);
  assert.equal(dom.ceKey.value, '');
  const checkNew = api.validateConnectionDraft(state.conns.editing.draft, state.conns.draft, { originalId: null });
  assert.ok(checkNew.errors.some(msg => msg.includes('API Key')), 'new connection must require API Key after preset switch');

  // Existing connection with saved key keeps secretSet=true after switching preset.
  api.openConnectionEditor(state.conns.draft[0]);
  api.applyProviderPreset('deepseek');
  assert.equal(state.conns.editing.draft.secretSet, true);

  state.composing = true;
  api.closeConnectionEditor();
  assert.equal(state.composing, false);

  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  assert.match(appText, /addEventListener\('focusout',\s*\(\)\s*=>\s*\{\s*state\.composing\s*=\s*false;\s*\}\)/);
  assert.match(appText, /addEventListener\('blur',\s*\(\)\s*=>\s*\{\s*state\.composing\s*=\s*false;\s*\}\)/);
});

test('inline disconnect confirm is cleared by runAction, chooseIntegrationMode, and in-flight renderActions', async () => {
  const cleared = [];
  const state = {
    data: { revision: 'rev-1', integrationMode: 'gateway', operation: null, configuration: { state: 'ready' } },
    pendingAction: null, pendingOpId: null, modelsSaving: false, connsSaving: false, actionButtons: [], view: 'maintenance',
  };
  const api = new Function('state', 'cleared', `
    let integrationModeSaving = false;
    const INTEGRATION_MODES = ['gateway', 'direct'];
    const API = { actions: '/api/actions', integrationMode: '/api/integration-mode' };
    const ACTION_LABEL = {};
    const dom = { conn: {}, connText: {} };
    const clearDisconnectConfirm = () => cleared.push('cleared');
    const onboardBusy = () => true;
    const renderOnboard = () => {};
    const renderOnboardOverview = () => {};
    const configReady = () => true;
    const savedIntegrationMode = (d) => d?.integrationMode || 'gateway';
    const directRestartRequired = () => false;
    const integrationModeBusy = () => integrationModeSaving;
    const renderIntegrationMode = () => {};
    const renderOperation = () => {};
    const adoptState = (next) => { state.data = next; };
    const loadState = async () => {};
    const handleSaveFailure = async () => {};
    const showToast = () => {};
    const actionUUID = () => 'u1';
    const request = async (_path, opts) => opts.body.mode ? { state: { ...state.data, integrationMode: opts.body.mode } } : { operation: { id: 'op1', action: opts.body.action, state: 'running' } };
    ${sourceFunction('isInFlight')}
    ${sourceFunction('renderActions')}
    ${sourceFunction('chooseIntegrationMode')}
    ${sourceFunction('runAction')}
    return { renderActions, chooseIntegrationMode, runAction };
  `)(state, cleared);

  api.renderActions({ ...state.data, operation: { state: 'running' } });
  assert.equal(cleared.length, 1, 'renderActions must clear confirm when operation is in flight');

  await api.chooseIntegrationMode('direct');
  assert.equal(cleared.length, 2, 'chooseIntegrationMode must clear confirm when mode changes');

  await api.runAction('connect-kimi');
  assert.ok(cleared.length >= 3, 'runAction must clear confirm at entry');
});

function conversationPanelTest(initial, rebindReply) {
  class Element {
    constructor(tag) {
      this.tag = tag;
      this.children = [];
      this.listeners = new Map();
      this.classList = { toggle() {} };
      this.value = '';
    }
    addEventListener(name, fn) { this.listeners.set(name, fn); }
    async fire(name, target = this) { await this.listeners.get(name)?.({ target }); }
    append(...children) { this.children.push(...children); }
    replaceChildren(...children) {
      this.children = children;
      if (children[0]?.tag === 'option') this.value = children[0].value;
    }
    querySelectorAll() {
      return this.children.flatMap(child => child.tag === 'input' && child.type === 'checkbox' ? [child] : child.querySelectorAll());
    }
    focus() { this.focused = true; }
  }
  const ids = new Map();
  const element = id => {
    if (!ids.has(id)) ids.set(id, new Element(id));
    return ids.get(id);
  };
  const api = { workConversations: '/api/work/conversations', rebindConversations: '/api/work/conversations/rebind' };
  const calls = [];
  let next = initial;
  const request = async (path, options) => {
    calls.push([path, options]);
    const reply = path === api.workConversations ? next : rebindReply;
    if (reply instanceof Error) throw reply;
    return reply;
  };
  const code = ['conversationListStatus', 'conversationModelLabel', 'conversationRow', 'conversationRebindPayload',
    'conversationPanelSignature', 'mountConversationRebind'].map(sourceFunction).join('\n');
  const updatePanel = new Function('$', 'API', 'document', 'state', 'request', 'setHidden', 'setText', code + '\nreturn mountConversationRebind();')(
    element, api, { createElement: tag => new Element(tag) }, { key: 'local-test' }, request,
    (el, value) => { el.hidden = value; }, (el, value) => { el.textContent = value; });
  return { calls, panel: element('conversation-rebind'), get: id => element('rebind-' + id), setNext: value => { next = value; }, updatePanel };
}

const oldConversationRows = [
  { conversationKey: 'official-1', title: 'Existing conversation', modelAlias: 'kimi-k3', updatedAtMs: 10, isByok: false },
  { conversationKey: 'third-party-2', title: 'Other conversation', modelAlias: 'byok-kth/example', updatedAtMs: 8, isByok: true },
];
const rebindTarget = { alias: 'byok-custom/claude', displayName: 'Claude', protocol: 'anthropic', sourceKind: 'custom',
  supportEfforts: ['low', 'high'], defaultEffort: 'low', directAnthropicHistoryWarning: true };

async function openConversationTestPanel(env) {
  env.panel.open = true;
  await env.panel.fire('toggle');
  await Promise.resolve();
}

test('old conversations render in Kimi access settings with model, date and distinct empty states', async () => {
  assert.match(htmlSource, /id="native-panel"[\s\S]+id="conversation-rebind"[\s\S]+id="update-panel"/);
  const env = conversationPanelTest({ conversations: oldConversationRows, models: [rebindTarget], application: 'not_running', kimiRunning: false });
  env.panel.open = true;
  const loading = env.panel.fire('toggle');
  assert.match(env.get('status').textContent, /正在读取/);
  await loading;
  assert.equal(env.get('form').hidden, false);
  const label = env.get('list').children[0].children[0];
  assert.deepEqual(label.children[1].children.map(el => el.textContent), ['Existing conversation', 'kimi-k3']);
  assert.equal(label.children[2].dateTime, new Date(10).toISOString());
  assert.match(label.children[2].textContent, /1970|1969/);
  assert.equal(env.get('model').children[1].textContent, 'Claude · Anthropic Messages · 直连');

  for (const [data, message] of [
    [{ reason: 'conversation_database_missing', conversations: [], models: [] }, /还没有 Kimi Work 会话库/],
    [{ reason: 'conversation_rollback_failed', conversations: [], models: [], application: 'running' }, /恢复尚未完成.*退出 Kimi/],
    [{ reason: 'conversation_rollback_failed', conversations: [], models: [], application: 'not_running' }, /恢复尚未完成.*写入权限/],
    [{ conversations: [], models: [rebindTarget], application: 'not_running' }, /没有可改绑/],
    [{ conversations: oldConversationRows, models: [], application: 'not_running' }, /没有可用的第三方模型/],
    [{ conversations: oldConversationRows, models: [rebindTarget], application: 'unknown' }, /无法确认 Kimi 的运行状态/],
    [new Error('读取失败'), /读取失败/],
  ]) {
    env.setNext(data);
    await env.get('refresh').fire('click');
    assert.match(env.get('status').textContent, message);
    assert.equal(env.get('review').hidden, true);
  }
});

test('a single old conversation shows the direct Anthropic warning and submits only after confirmation', async () => {
  const env = conversationPanelTest({ conversations: oldConversationRows, models: [rebindTarget], application: 'not_running', kimiRunning: false },
    { conversationKeys: ['official-1'], kimiRestarted: false, directAnthropicHistoryWarning: true });
  await openConversationTestPanel(env);
  const [first, second] = env.get('list').querySelectorAll();
  env.get('model').value = rebindTarget.alias;
  await env.get('model').fire('change');
  assert.equal(env.get('warning').hidden, true);
  first.checked = true;
  await env.get('list').fire('change', first);
  assert.equal(env.get('warning').hidden, false);
  assert.equal(second.checked, false);
  assert.equal(env.get('all').indeterminate, true);
  assert.equal(env.get('review').textContent, '改绑所选会话');
  assert.deepEqual(env.get('effort').children.map(option => option.textContent), ['模型默认（low）', 'low', 'high']);
  env.get('effort').value = 'high';
  await env.get('review').fire('click');
  assert.equal(env.calls.length, 1);
  assert.match(env.get('confirm-text').textContent, /将 1 个会话改绑/);
  await env.get('submit').fire('click');
  assert.deepEqual(env.calls[1], ['/api/work/conversations/rebind', { method: 'POST', body: {
    conversationKeys: ['official-1'], targetModelAlias: rebindTarget.alias, thinkingLevel: 'high', restartKimi: false,
  } }]);
  assert.match(env.get('result').textContent, /已改绑 1 个会话.*直连 Anthropic/);
});

test('batch selection confirms Kimi restart and displays a restart failure after committed changes', async () => {
  const env = conversationPanelTest({ conversations: oldConversationRows, models: [rebindTarget], application: 'running', kimiRunning: true },
    { conversationKeys: ['official-1', 'third-party-2'], kimiRestarted: false, restartReason: 'restart failed' });
  await openConversationTestPanel(env);
  env.get('model').value = rebindTarget.alias;
  await env.get('model').fire('change');
  env.get('all').checked = true;
  await env.get('all').fire('change');
  assert.deepEqual(env.get('list').querySelectorAll().map(input => input.checked), [true, true]);
  assert.equal(env.get('review').textContent, '改绑并重启 Kimi');
  await env.get('review').fire('click');
  assert.match(env.get('confirm-text').textContent, /退出后重新打开 Kimi/);
  await env.get('cancel').fire('click');
  await env.get('submit').fire('click');
  assert.equal(env.calls.length, 1);
  await env.get('review').fire('click');
  await env.get('submit').fire('click');
  assert.deepEqual(env.calls[1][1].body, {
    conversationKeys: ['official-1', 'third-party-2'], targetModelAlias: rebindTarget.alias, thinkingLevel: '', restartKimi: true,
  });
  assert.match(env.get('result').textContent, /已改绑 2 个会话。Kimi 未能重启，请手动打开 Kimi/);
});

test('conversation rebind prefers the first non-off supported effort when the model has no default', async () => {
  const target = { ...rebindTarget, protocol: 'google-genai', defaultEffort: '',
    supportEfforts: ['zeta', 'max', 'high', 'low', 'off', 'alpha', 'medium', 'xhigh'] };
  const offOnly = { ...target, alias: 'byok-custom/off-only', displayName: 'OffOnly', supportEfforts: ['off'] };
  const chat = { ...target, alias: 'byok-custom/chat', protocol: 'openai', supportEfforts: [] };
  const env = conversationPanelTest({ conversations: oldConversationRows, models: [target, offOnly, chat], application: 'not_running', kimiRunning: false },
    { conversationKeys: ['official-1'], kimiRestarted: false });
  await openConversationTestPanel(env);
  assert.deepEqual(env.get('model').children.slice(1).map(option => option.textContent),
    ['Claude · Google GenAI · 直连', 'OffOnly · Google GenAI · 直连', 'Claude · OpenAI Chat · 直连']);
  env.get('model').value = target.alias;
  await env.get('model').fire('change');
  assert.deepEqual(env.get('effort').children.map(option => option.value), ['off', 'low', 'medium', 'high', 'xhigh', 'max', 'alpha', 'zeta']);
  assert.equal(env.get('effort').value, 'low');
  const [first] = env.get('list').querySelectorAll();
  first.checked = true;
  await env.get('list').fire('change', first);
  await env.get('review').fire('click');
  assert.match(env.get('confirm-text').textContent, /思考档位：low/);
  await env.get('submit').fire('click');
  assert.equal(env.calls[1][1].body.thinkingLevel, 'low');
  env.get('model').value = offOnly.alias;
  await env.get('model').fire('change');
  assert.equal(env.get('effort').value, 'off');
  env.get('model').value = chat.alias;
  await env.get('model').fire('change');
  assert.equal(env.get('effort').disabled, true);
  assert.equal(env.get('effort').value, '');
});

test('failed rebind with restart consent refreshes process state and keeps the original error', async () => {
  const list = { conversations: oldConversationRows, models: [rebindTarget], application: 'running', kimiRunning: true };
  const error = Object.assign(new Error('会话库写入失败'), { code: 'conversation_database_unavailable' });
  const env = conversationPanelTest(list, error);
  await openConversationTestPanel(env);
  env.get('model').value = rebindTarget.alias;
  await env.get('model').fire('change');
  env.get('all').checked = true;
  await env.get('all').fire('change');
  await env.get('review').fire('click');
  env.setNext({ ...list, application: 'not_running', kimiRunning: false });
  await env.get('submit').fire('click');
  assert.deepEqual(env.calls.map(call => call[0]), ['/api/work/conversations', '/api/work/conversations/rebind', '/api/work/conversations']);
  assert.equal(env.calls[1][1].body.restartKimi, true);
  assert.match(env.get('result').textContent, /会话库写入失败/);
  assert.match(env.get('status').textContent, /Kimi 已退出/);
  assert.equal(env.get('review').textContent, '改绑所选会话');
});

test('an open rebind panel invalidates its confirmation when account, config or process changes', async () => {
  const list = { conversations: oldConversationRows, models: [rebindTarget], application: 'running', kimiRunning: true };
  const env = conversationPanelTest(list);
  assert.match(sourceFunction('adoptState'), /state\.conversationRebindStateChanged\?\.\(next\)/);
  const data = { accounts: { currentId: 'a', state: 'ready' }, revision: 'r1', kimiProcess: { running: true, state: 'running' } };
  env.updatePanel(data);
  await openConversationTestPanel(env);
  const updates = [
    { ...data, accounts: { currentId: 'b', state: 'ready' } },
    { ...data, revision: 'r2' },
    { ...data, revision: 'r2', kimiProcess: { running: false, state: 'not_running' } },
  ];
  for (const update of updates) {
    env.get('model').value = rebindTarget.alias;
    await env.get('model').fire('change');
    env.get('all').checked = true;
    await env.get('all').fire('change');
    await env.get('review').fire('click');
    assert.equal(env.get('confirm').hidden, false);
    env.updatePanel(update);
    assert.match(env.get('status').textContent, /已变化，请刷新会话/);
    assert.equal(env.get('review').disabled, true);
    assert.equal(env.get('confirm').hidden, true);
    await env.get('submit').fire('click');
    assert.equal(env.calls.some(call => call[1]?.method === 'POST'), false);
    await env.get('refresh').fire('click');
    assert.equal(env.get('form').disabled, false);
  }
  env.updatePanel({ ...updates.at(-1), updates: { state: 'checking' } });
  assert.doesNotMatch(env.get('status').textContent, /已变化/);
});

test('a pending conversation query is marked stale even if the account changes back', async () => {
  let finish;
  const pending = new Promise(resolve => { finish = resolve; });
  const env = conversationPanelTest(pending);
  const original = { accounts: { currentId: 'a' } };
  env.updatePanel(original);
  env.panel.open = true;
  const loading = env.panel.fire('toggle');
  env.updatePanel({ accounts: { currentId: 'b' } });
  env.updatePanel(original);
  finish({ conversations: oldConversationRows, models: [rebindTarget], application: 'not_running', kimiRunning: false });
  await loading;
  await Promise.resolve();
  assert.match(env.get('status').textContent, /已变化，请刷新会话/);
  assert.equal(env.get('form').disabled, true);
});

test('K3 alias migration asks for confirmation and sends the model instead of recent conversation IDs', async () => {
  const model = { ...rebindTarget, alias: 'byok-kth-kimi/kimi-k3-kwp-example',
    previousAlias: 'byok-kth-kimi/group/kimi-example', protocol: 'kimi', sourceKind: 'kth' };
  const env = conversationPanelTest({ conversations: oldConversationRows, models: [model], application: 'running', kimiRunning: true },
    { conversationKeys: ['older-than-recent-list'], kimiRestarted: true, hasMore: true });
  await openConversationTestPanel(env);
  env.get('model').value = model.alias;
  await env.get('model').fire('change');
  assert.equal(env.get('migrate').hidden, false);
  assert.equal(env.get('review').disabled, true);
  await env.get('migrate').fire('click');
  assert.match(env.get('confirm-text').textContent, /旧别名.*退出后重新打开 Kimi/);
  assert.equal(env.calls.length, 1);
  await env.get('submit').fire('click');
  assert.deepEqual(env.calls[1][1].body, { targetModelAlias: model.alias, thinkingLevel: '', restartKimi: true, migratePreviousAlias: true });
  assert.match(env.get('result').textContent, /已迁移 1 个会话.*还有会话待迁移/);
});

test('K3 compatibility setting renders gateway-only state copy and saves or restores on failure', async () => {
  const makeNode = id => ({ id, textContent: '', hidden: false, disabled: false, checked: false, dataset: {}, setAttribute() {}, classList: { add() {}, toggle() {} } });
  const dom = {
    modeGateway: { value: 'gateway', checked: false, disabled: false },
    modeDirect: { value: 'direct', checked: false, disabled: false },
    integrationModeGroup: null,
    integrationModeState: makeNode('integration-mode-state'),
    kimiK3Setting: makeNode('kimi-k3-setting'),
    kimiK3Enabled: makeNode('kimi-k3-enabled'),
    kimiK3Status: makeNode('kimi-k3-status'),
    nativeSyncLabel: makeNode('native-sync-label'),
    nativeSync: makeNode('native-sync'),
    nativeRecover: makeNode('native-recover'),
    directModeNotice: makeNode('direct-mode-notice'),
    directModeSupport: makeNode('direct-mode-support'),
    directModeHosts: makeNode('direct-mode-hosts'),
    directModeBypassed: null,
    directModeSkipped: null,
  };
  const calls = [];
  const toasts = [];
  const errors = [];
  let nextReply = { ok: true, state: { revision: 'r2', integrationMode: 'gateway', kimiK3Compatibility: true, kimiK3State: 'needs_sync', models: [] } };
  const state = {
    data: { revision: 'r1', integrationMode: 'gateway', kimiK3Compatibility: false, kimiK3State: '', models: [] },
    actionButtons: [dom.nativeSync, dom.nativeRecover],
    pendingAction: null,
    modelsSaving: false,
    connsSaving: false,
  };
  const api = new Function('dom', 'state', 'request', 'showToast', 'handleSaveFailure', `
    ${INTEGRATION_HELPERS.join('\n')}
    const API = { kimiK3Compatibility: '/api/kimi-k3-compatibility' };
    let integrationModeSaving = false;
    const integrationModeBusy = data => integrationModeSaving || data?.operation?.state === 'running';
    const onboardBusy = () => false;
    const renderOnboard = () => {};
    const renderOnboardOverview = () => {};
    const clearDisconnectConfirm = () => {};
    const configReady = () => true;
    const ACTION_LABEL = {};
    const setText = (el, text) => { if (el) el.textContent = text; };
    const setHidden = (el, hidden) => { if (el) el.hidden = Boolean(hidden); };
    const displayModelId = id => id;
    const DIRECT_BYPASS_WORD = {};
    const DIRECT_SKIP_REASON = {};
    ${sourceFunction('isInFlight')}
    ${sourceFunction('renderIntegrationMode')}
    ${sourceFunction('renderActions')}
    ${sourceFunction('chooseKimiK3Compatibility')}
    return { renderIntegrationMode, chooseKimiK3Compatibility };
  `)(dom, state, async (path, options) => {
    calls.push([path, options]);
    if (nextReply instanceof Error) throw nextReply;
    return nextReply;
  }, (msg, tone) => toasts.push([msg, tone]), async (err, scope) => errors.push([err.message, scope]));

  api.renderIntegrationMode(state.data);
  assert.equal(dom.kimiK3Setting.hidden, false);
  assert.equal(dom.kimiK3Enabled.checked, false);
  assert.equal(dom.kimiK3Status.hidden, true);

  dom.kimiK3Enabled.checked = true;
  await api.chooseKimiK3Compatibility(true, { onSaved: next => { state.data = next; } });
  assert.deepEqual(calls[0], ['/api/kimi-k3-compatibility', { method: 'POST', body: { revision: 'r1', enabled: true } }]);
  assert.equal(dom.kimiK3Enabled.checked, true);
  assert.equal(dom.kimiK3Status.hidden, false);
  assert.match(dom.kimiK3Status.textContent, /同步到 Work/);
  assert.equal(toasts.length, 1);

  api.renderIntegrationMode({ ...state.data, kimiK3State: 'ready' });
  assert.match(dom.kimiK3Status.textContent, /切到官方 K3/);

  api.renderIntegrationMode({ ...state.data, integrationMode: 'direct', kimiK3Compatibility: false });
  assert.equal(dom.kimiK3Setting.hidden, true);

  nextReply = new Error('save_failed');
  dom.kimiK3Enabled.checked = false;
  await api.chooseKimiK3Compatibility(false);
  assert.deepEqual(errors, [['save_failed', 'integration-mode']]);
  assert.equal(dom.kimiK3Enabled.checked, true);
});

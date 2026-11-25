import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const appFiles = ['./app-core.js', './app-overview.js', './app-models.js', './app-connections.js', './app-work.js', './app-conversations.js', './app.js'];
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
  'directHasNoEligibleModels', 'directImportComplete', 'directRouteCleanupPending', 'nativeRestartReceipt', 'directStaticHostsReady', 'directModeState',
  'directRestartRequired', 'normalizeReasonCode', 'directReasonText', 'reasonText'].map(sourceFunction)];

/* 真实原因码翻译：测试直接执行 app-core.js 的三张表与 reasonText，
   不再手写一张小映射，避免生产表缺项时被假 mock 掩盖。 */
const REASON_TEXT_SOURCES = [
  DIAG_REASON_SOURCE,
  DIRECT_REASON_SOURCE,
  ...['normalizeReasonCode', 'directReasonText', 'reasonText'].map(sourceFunction),
];

function makeReasonText() {
  return new Function([...REASON_TEXT_SOURCES, 'return reasonText;'].join('\n'))();
}

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
  const launch = { dataset: {}, setAttribute() {}, hidden: false, disabled: false, textContent: '' };
  const handoff = { dataset: {} };
  const retryApply = { hidden: false, disabled: false };
  const api = new Function('dom', `
    const ACTION_LABEL = {'open-work': '打开 Kimi Work', 'launch-kimi': '启动 Kimi Work', 'restart-kimi': '重启 Kimi'};
    const state = { pendingAction: null };
    const isInFlight = op => op?.state === 'running';
    const setDot = () => {};
    const setText = (element, text) => { if (element) element.textContent = text; };
    const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };
    const overviewState = data => data._info;
    const OVERVIEW_NAV_ACTIONS = new Set(['restart-kimi', 'open-models', 'open-connection']);
    const OVERVIEW_LAUNCH_LABELS = { 'launch-kimi': '启动 Kimi Work', 'open-work': '打开 Kimi Work' };
    ${sourceFunction('overviewLaunchAction')}
    ${sourceFunction('overviewLaunchLabel')}
    ${sourceFunction('renderOverviewStatus')}
    return { overviewLaunchAction, renderOverviewStatus };
  `)({ onboardLaunch: launch, onboardLaunchHint: null, onboardVerdictDot: null, onboardVerdictWord: null,
    onboardVerdictNote: null, handoff, onboardRetryApply: retryApply });

  api.renderOverviewStatus({ kimiProcess: { running: true }, _info: { code: 'ready', tone: 'ok', word: '已就绪', note: '' } });
  assert.equal(launch.dataset.action, 'open-work');
  assert.equal(retryApply.hidden, true, 'the legacy retry-apply button never comes back on the new overview');
  assert.equal(handoff.dataset.state, 'idle');
  api.renderOverviewStatus({ kimiProcess: { running: true }, _info: { code: 'restart', tone: 'warn', word: '', note: '', launch: 'restart-kimi' } });
  assert.equal(launch.dataset.action, 'restart-kimi');
  assert.equal(handoff.dataset.tone, 'warn');
  api.renderOverviewStatus({ kimiProcess: { running: false }, _info: { code: 'pending', tone: 'warn', word: '', note: '' } });
  assert.equal(launch.dataset.action, 'launch-kimi');
  api.renderOverviewStatus({ kimiProcess: { running: false }, _info: { code: 'empty', tone: 'neutral', word: '', note: '', launch: 'open-models' } });
  assert.equal(launch.dataset.action, 'open-models');
  api.renderOverviewStatus({ kimiProcess: { running: false }, _info: { code: 'no-eligible', tone: 'warn', word: '', note: '', launch: 'open-models' } });
  assert.equal(launch.dataset.action, 'open-models');
  /* 未知的 launch 值不直接写进 data-action：兜底回打开/启动。 */
  assert.equal(api.overviewLaunchAction({ kimiProcess: { running: true } }, { launch: 'takeover' }), 'open-work');
  assert.equal(api.overviewLaunchAction({ kimiProcess: { running: false } }, { launch: 'takeover' }), 'launch-kimi');
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
    'const scrollMainToElement = () => calls.push("scroll-main");',
    'const editorDirty = () => state.editor?.dirty === true;',
    'const closeEditor = () => { state.editor = null; calls.push("close"); };',
    'const showToast = (message) => calls.push(message);',
    'return openOnboardWizard;',
  ].join('\n');
  const open = new Function('state', 'dom', 'calls', code)(state, dom, calls);
  open();
  assert.equal(dom.onboardDisclosure.open, true);
  assert.deepEqual(calls, ['models', 'close', 'scroll-main', 'focus']);
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
  const code = [
    sourceFunction('saveOnboard'),
    'const clearOnboardSaveErrors = () => {};',
    'return saveOnboard;',
  ].join('\n');
  const save = new Function(
    'state', 'dom', 'API', 'selectedOnboardModels', 'onboardProblems', 'connectionPayload', 'connectionRowsOf',
    'mergeOnboardModels', 'request', 'adoptState', 'renderAll', 'renderOnboard', 'renderActions', 'showToast',
    'handleSaveFailure', 'ApiError', 'fmtCount',
    'savedOnboardRow', 'onboardConnectionPayload', 'savedConnectionsSignature', 'connectionDraftFrom',
    'fillOnboardForm', 'onboardEndpointKey', code,
  )(
    state, dom, { workspace: '/api/workspace' }, () => [{}], () => ({
      problems: [], built: [{ id: 'conn/resp/model-a' }],
      values: { id: 'conn', name: 'Connection', providerPresetId: '', baseURL: 'https://api.example.com/v1', apiKey: 'synthetic' },
    }), () => [], () => [], (_existing, built) => built,
    async () => ({ state: savedState }), next => { state.data = next; }, () => {}, () => {}, () => {}, () => {}, async () => {},
    class ApiError extends Error {}, String,
    () => null, () => ({ id: 'conn' }), () => 'sig', () => [], () => {}, () => 'endpoint',
  );
  assert.equal(await save(), true);
  assert.equal(dom.onboardDisclosure.open, false);
  assert.equal(dom.onboardKey.value, '');
});

test('a failed onboarding save keeps the form and the key', async () => {
  const state = {
    data: { revision: 'rev-1', models: [], connections: { saved: [] } },
    onboard: { saving: false, touched: true },
    conns: { editing: null, draft: [] },
    stateReadEpoch: 0, key: 'session',
  };
  const dom = { onboardKey: { value: 'sk-typed' }, onboardDisclosure: { open: true } };
  const code = [sourceFunction('saveOnboard'), 'const clearOnboardSaveErrors = () => {};', 'return saveOnboard;'].join('\n');
  const save = new Function(
    'state', 'dom', 'API', 'selectedOnboardModels', 'onboardProblems', 'connectionPayload', 'connectionRowsOf',
    'mergeOnboardModels', 'request', 'adoptState', 'renderAll', 'renderOnboard', 'renderActions', 'showToast',
    'handleSaveFailure', 'ApiError', 'fmtCount',
    'savedOnboardRow', 'onboardConnectionPayload', 'savedConnectionsSignature', 'connectionDraftFrom',
    'fillOnboardForm', 'onboardEndpointKey', code,
  )(
    state, dom, { workspace: '/api/workspace' }, () => [{}], () => ({
      problems: [], built: [{ id: 'conn/resp/model-a' }],
      values: { id: 'conn', name: 'Connection', providerPresetId: '', baseURL: 'https://api.example.com/v1', apiKey: 'sk-typed' },
    }), () => [], () => [], (_existing, built) => built,
    async () => { throw new Error('offline'); }, next => { state.data = next; }, () => {}, () => {}, () => {}, () => {},
    async () => {}, class ApiError extends Error {}, String,
    () => null, () => ({ id: 'conn' }), () => 'sig', () => [], () => {}, () => 'endpoint',
  );
  assert.equal(await save(), false);
  assert.equal(dom.onboardKey.value, 'sk-typed');
  assert.equal(dom.onboardDisclosure.open, true);
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
  const panelCalls = [];
  const code = [
    'const resetModelSignature = () => {}, resetRecentSignature = () => {}, resetSubagentSignatures = () => {}, resetIntegrationModeSignature = () => {}, clearDisconnectConfirm = () => {}, refreshServiceStatus = () => {};',
    sourceFunction('adoptLocationKey'),
    'return adoptLocationKey;',
  ].join('\n');
  const rotate = new Function('state', 'dom', 'window', 'sessionStorage', 'history', 'KEY_STORE', 'stopPolling', 'setHidden', 'showGate', 'loadProviders', 'loadState', 'startPolling', 'conversationsPanel', code)(
    state, dom, { location: { hash: '#key=new-session', pathname: '/', search: '' } },
    { setItem: () => {} }, { replaceState: () => {} }, 'panel-key', () => {}, () => {}, () => {}, () => {}, async () => true, () => {},
    { reset: () => panelCalls.push('reset') },
  );
  assert.equal(rotate(), true);
  assert.equal(state.key, 'new-session');
  assert.equal(state.onboard.query, '');
  assert.equal(dom.onboardModelQuery.value, '');
  assert.equal(dom.onboardKey.value, '');
  assert.deepEqual(panelCalls, ['reset'], 'the conversations page must drop its cached signature on a key switch');
  assert.equal(state.conversations.rowConfirm, null);
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
  return new Function('state', 'dom', 'window', 'sessionStorage', 'history', 'KEY_STORE', 'stopPolling', 'setHidden', 'showGate', 'loadProviders', 'loadState', 'startPolling', 'usagePanel', 'conversationsPanel', code)(
    state, dom, { location: { hash: '#key=new-session', pathname: '/', search: '' } },
    { setItem: () => {} }, { replaceState: () => {} }, 'panel-key',
    () => calls.push('stop'), () => {}, () => {}, () => {}, loadState, () => calls.push('start'), usagePanel,
    { reset() {} },
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

test('index.html lays out six top-level tabs, the merged model page, the conversations page and the update banner', () => {
  for (const id of ['nav-overview', 'nav-models', 'nav-conversations', 'nav-subagents', 'nav-usage', 'nav-maintenance']) {
    assert.ok(htmlSource.includes(`id="${id}"`), 'missing top tab ' + id);
  }
  assert.match(htmlSource, /id="nav-models"[^>]*>模型与服务商 <span class="nav__count mono" id="nav-model-count"><\/span>/);
  assert.match(htmlSource, /id="nav-maintenance"[^>]*>[\s\S]{0,160}id="nav-update-dot"/);
  assert.equal(htmlSource.includes('id="nav-work"'), false);
  assert.equal(htmlSource.includes('id="nav-extensions"'), false);
  assert.equal(htmlSource.includes('models-tab-catalog'), false, 'the model page must not keep a catalog subtab');
  assert.equal(htmlSource.includes('models-tab-connections'), false, 'the model page must not keep a connections subtab');
  assert.equal(htmlSource.includes('data-subtab-group="models"'), false);
  assert.equal(htmlSource.includes('id="kimi-k3-setting"'), false, 'the manual K3 switch is gone');
  assert.ok(htmlSource.includes('id="native-troubleshoot"'));
  assert.ok(htmlSource.indexOf('id="native-troubleshoot"') < htmlSource.indexOf('id="native-sync"'));
  assert.ok(htmlSource.includes('id="view-conversations"'));
  assert.ok(htmlSource.includes('id="conv-list"'));
  assert.ok(htmlSource.includes('id="conv-migration-tip"'));
  assert.ok(htmlSource.includes('id="update-banner"'));
  assert.ok(htmlSource.includes('id="update-banner-primary"'));
  assert.equal(source.includes('function mountWorkspace('), false);
  assert.equal(source.includes('.scrollIntoView('), false, 'production JS must scroll .main, never call scrollIntoView');
  const cssSource = readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
  assert.equal(cssSource.includes('overflow: clip'), false, 'styles.css must avoid overflow: clip on root');
  assert.match(cssSource, /\.shell\s*\{[^}]*position:\s*fixed;[^}]*inset:\s*0;/);
  for (const id of ['subagents-tab-models', 'subagents-tab-swarm',
    'settings-tab-accounts', 'settings-tab-connection', 'settings-tab-extensions', 'settings-tab-advanced']) {
    assert.ok(htmlSource.includes(`id="${id}"`), 'missing subtab ' + id);
  }
  const modelsIdx = htmlSource.indexOf('id="view-models"');
  const conversationsIdx = htmlSource.indexOf('id="view-conversations"');
  const usageIdx = htmlSource.indexOf('id="view-usage"');
  const modelsSlice = htmlSource.slice(modelsIdx, conversationsIdx);
  assert.ok(modelsSlice.includes('id="connections-panel"'));
  assert.ok(modelsSlice.includes('id="catalog-panel"'));
  assert.ok(modelsSlice.includes('id="restart-panel"'));
  assert.ok(modelsSlice.indexOf('id="connections-panel"') < modelsSlice.indexOf('id="catalog-panel"'),
    'service connections come first, the enabled catalog follows');
  assert.ok(modelsSlice.indexOf('id="catalog-panel"') < modelsSlice.indexOf('id="model-table"'));
  assert.ok(htmlSource.slice(conversationsIdx, usageIdx).includes('id="conv-list"'));

  const form = htmlSource.slice(htmlSource.indexOf('id="model-form"'), htmlSource.indexOf('id="model-errors"'));
  const advanced = form.indexOf('id="m-advanced"');
  assert.match(form.slice(advanced, form.indexOf('</summary>', advanced)), /高级：上游接口协议、定价与故障转移备用连接/);
  for (const id of ['m-name', 'm-connection', 'm-candidates', 'm-upstream', 'm-tools', 'm-reasoning', 'm-vision',
    'm-context', 'm-output', 'm-effort-field', 'm-default']) {
    const position = form.indexOf(`id="${id}"`);
    assert.ok(position >= 0, 'missing ' + id);
    assert.ok(position < advanced, id + ' must stay in the default editor, not inside advanced settings');
  }
  for (const id of ['m-id', 'm-protocol-group', 'm-price-input', 'm-failover-rows']) {
    const position = form.indexOf(`id="${id}"`);
    assert.ok(position > advanced, id + ' belongs to the advanced disclosure');
  }
  assert.match(htmlSource, /所有协议模型都能在 Kimi 下拉框原会话切换/);
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
  assert.equal(compute({ gateway: { state: 'running' }, _apply: { tone: 'warn', word: '等待应用', note: '待应用' } }).action.section, 'native-troubleshoot');
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
  assert.match(pending.note, /重新同步模型目录/);
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
  assert.match(verdict.note, /点「重新同步模型目录」/);
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
    assert.match(verdict.note, /点「重新同步模型目录」/);
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
  assert.doesNotMatch(verdict.note, /重新同步模型目录/);
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
  assert.match(removed, /重新同步模型目录/);
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
  for (const id of ['workspace-sync-state', 'workspace-sync-note', 'workspace-restart', 'workspace-resync']) {
    nodes[id] = { id, textContent: '', hidden: false, disabled: false };
  }
  nodes['workspace-resync'].hidden = true;
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
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');

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
  assert.equal(nodes['workspace-sync-state'].textContent, '等待自动写入直连配置');
  assert.match(nodes['workspace-sync-note'].textContent, /自动写入服务商地址与静态 hosts/);
  assert.match(nodes['workspace-sync-note'].textContent, /之后可退出本软件/);
  assert.match(nodes['workspace-sync-note'].textContent, /重新同步模型目录/);

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
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');
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
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');
  assert.match(nodes['workspace-sync-note'].textContent, /补齐静态 hosts/);

  /* 网关模式原有四种文案不得变化。 */
  render({ models: [], integration: {} });
  assert.equal(nodes['workspace-sync-state'].textContent, '尚未添加模型');
  render({ models, integration: { adopted: true, aliases: { 'conn/resp/model-a': 'a', 'conn/chat/model-b': 'b' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '已同步到 Kimi Work');
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
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');
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
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Kimi Work');
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
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Kimi Work');
  assert.equal(workspace['workspace-restart'].hidden, true);
  assert.match(workspace['workspace-sync-note'].textContent, /非托管运行/);
  renderWorkspace({ ...unmanagedRestart, kimiProcess: { running: false } });
  assert.equal(workspace['workspace-sync-state'].textContent, '已同步到 Kimi Work');
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

test('the shell banner promotes one direct restart action and keeps the credential prompt', () => {
  const blockedCodes = source.slice(source.indexOf('const CREDENTIAL_VAULT_BLOCKED ='), source.indexOf('\nfunction credentialAuthPending'));
  const nodes = {
    'restart-banner': { textContent: '', hidden: true },
    'restart-banner-text': { textContent: '', hidden: true },
    'restart-banner-link': { textContent: '', hidden: false, dataset: {}, setAttribute() {} },
  };
  const dom = {
    railGatewayDot: {}, railGatewayText: {}, railGatewayUrl: {}, sync: {},
    restartBanner: nodes['restart-banner'], restartBannerText: nodes['restart-banner-text'],
    restartBannerLink: nodes['restart-banner-link'],
    navModelCount: {}, budgetBanner: {}, budgetBannerText: {},
  };
  const renderShell = new Function('dom', `${blockedCodes}
    const state = { lastSync: 0, pendingAction: null };
    const setText = (element, text) => { if (element) element.textContent = text; };
    const setHidden = (element, hidden) => { if (element) element.hidden = hidden; };
    const setDot = () => {};
    const fmtTime = () => '12:00:00';
    const fmtCount = v => String(v);
    const savedIntegrationMode = data => data?.integrationMode === 'direct' ? 'direct' : 'gateway';
    const budgetNotice = () => '';
    ${sourceFunction('isInFlight')}
    ${sourceFunction('modelTakeoverRemoved')}
    ${sourceFunction('directModeOf')}
    ${sourceFunction('directHasNoEligibleModels')}
    ${sourceFunction('nativeRestartReceipt')}
    ${sourceFunction('credentialAuthPending')}
    ${sourceFunction('connectionRowsOf')}
    ${sourceFunction('autoSyncOf')}
    ${sourceFunction('syncNeedsImport')}
    ${sourceFunction('restartBannerState')}
    ${sourceFunction('renderShell')}
    return renderShell;`)(dom);

  /* 配置未变更、旧凭据待授权：横幅保留，并指向模型与服务商页重授权。 */
  renderShell({ gateway: { state: 'running' }, configuration: { state: 'ready' },
    connections: { saved: [{ id: 'c1', credentialError: 'credential_authorization_required' }] } });
  assert.equal(nodes['restart-banner'].hidden, false);
  assert.match(nodes['restart-banner-text'].textContent, /部分已保存的连接凭据暂时无法读取/);
  assert.match(nodes['restart-banner-text'].textContent, /模型与服务商/);
  assert.equal(nodes['restart-banner-link'].textContent, '前往模型与服务商处理');
  assert.equal(nodes['restart-banner-link'].dataset.bannerAction, 'models');

  /* 仅配置文件状态变化不再要求去模型页点「应用已保存的连接」。 */
  renderShell({ gateway: { state: 'running' }, configuration: { state: 'changed' },
    connections: { saved: [{ id: 'c1', apiKeySet: true }] } });
  assert.equal(nodes['restart-banner'].hidden, true);

  /* 自动同步要求重启刷新菜单：按钮直接是「立即重启 Kimi 生效」。 */
  renderShell({ gateway: { state: 'running' }, models: [{ id: 'a' }],
    kimiProcess: { running: true },
    integration: { autoSync: { state: 'requires_restart' }, desktop: { managed: true } },
    connections: { saved: [] } });
  assert.equal(nodes['restart-banner'].hidden, false);
  assert.equal(nodes['restart-banner-text'].textContent, '模型菜单已更新，重启 Kimi Work 后即可在下拉框看到最新变化。');
  assert.equal(nodes['restart-banner-link'].textContent, '立即重启 Kimi 生效');
  assert.equal(nodes['restart-banner-link'].dataset.bannerAction, 'restart-kimi');

  /* 外部启动的 Kimi 正在运行：同一入口改为「重启并接管 Kimi 生效」。 */
  renderShell({ gateway: { state: 'running' }, models: [{ id: 'a' }],
    kimiProcess: { running: true, state: 'running' },
    integration: { autoSync: { state: 'waiting_unmanaged' }, desktop: { managed: false } },
    connections: { saved: [] } });
  assert.equal(nodes['restart-banner'].hidden, false);
  assert.match(nodes['restart-banner-text'].textContent, /外部启动的官方 Kimi Work 正在运行/);
  assert.equal(nodes['restart-banner-link'].textContent, '重启并接管 Kimi 生效');
  assert.equal(nodes['restart-banner-link'].dataset.bannerAction, 'restart-kimi');

  renderShell({ gateway: { state: 'running' }, models: [{ id: 'a' }],
    kimiProcess: { running: true },
    integration: { autoSync: { state: 'synced' }, desktop: { managed: true }, adopted: true },
    connections: { saved: [] } });
  assert.equal(nodes['restart-banner'].hidden, true);

  /* 点击横幅不再先跳页面：restart-kimi 两种文案都直接走 runAction。 */
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const start = appText.indexOf("if (dom.restartBannerLink) dom.restartBannerLink.addEventListener('click'");
  assert.notEqual(start, -1);
  const handler = appText.slice(start, appText.indexOf('});', start) + 3);
  assert.match(handler, /runAction\('restart-kimi'\)/);
  assert.doesNotMatch(handler, /setSubtab\('models'/);
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
  assert.equal(dom.applyButton.hidden, true, 'saving applies connections; no daily apply button remains');
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
  assert.match(connectionText({ integration: { reason: 'native_bridge_unavailable' } }), /重新同步模型目录/);
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

test('the onboarding steps accept a ready direct sync and the old launch override is gone', () => {
  const steps = ['connection', 'catalogue', 'apply', 'sync'].map((step) => ({ dataset: { step, done: 'false' } }));
  const dom = { onboardSteps: { querySelectorAll: () => steps } };
  const api = new Function('dom', `
    const state = { data: { connections: { saved: [{ id: 'ds', baseURL: 'https://api.deepseek.com/v1' }] } } };
    const connectionsChanged = () => false;
    const connectionRowsOf = (data) => data?.connections?.saved || [];
    const syncState = data => ({ code: data.sync });
    const setHidden = (el, h) => { if (el) el.hidden = Boolean(h); };
    ${sourceFunction('savedOnboardTarget')}
    ${sourceFunction('mountedSavedModels')}
    ${sourceFunction('renderOnboardSteps')}
    return { renderOnboardSteps };
  `)(dom);

  const done = api.renderOnboardSteps(
    { integrationMode: 'direct', sync: 'ready', models: [{ id: 'ds/chat/r1', connectionId: 'ds' }], connections: { saved: [{ id: 'ds' }] } },
    { selected: 1, rows: 1 },
  );
  assert.equal(done.sync, true);
  assert.equal(steps.find((s) => s.dataset.step === 'sync').dataset.done, 'true');
  /* 旧 renderLaunch 会覆盖总览的主动作；现在只剩 renderOverviewStatus 一个入口。 */
  assert.equal(source.includes('function renderLaunch('), false);
  assert.equal(source.includes('function recoveryCopy('), false);
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
  /* 旧 renderOnboardOverview 会在这里用 syncState 覆盖总览主卡；现在只剩一个入口。 */
  assert.equal(calls.includes('overview:overview'), true);
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


/* ---------- 独立「会话」页 ---------- */

function makeConversationModel(extra = {}) {
  return {
    alias: 'byok-kth-kimi/kimi-k3-kwp-' + 'a'.repeat(64),
    displayName: 'DeepSeek V4.1 Flash',
    protocol: 'kimi',
    sourceKind: 'kth',
    supportEfforts: ['low', 'high'],
    defaultEffort: 'low',
    ...extra,
  };
}

const CONVERSATION_CONSTANTS = [
  source.slice(source.indexOf('const OFFICIAL_MODEL_NAMES = {'), source.indexOf('\n};', source.indexOf('const OFFICIAL_MODEL_NAMES = {')) + 3),
  'const KIMI_COMPATIBLE_ID = /^kimi-k3-kwp-[0-9a-f]{64}$/i;',
  'const EFFORT_ORDER = ["off", "low", "medium", "high", "xhigh", "max"];',
  'const CONVERSATION_REASONS = {};',
  'const MODEL_ALIAS_PREFIXES = ["byok-kth-kimi/", "byok-kth-resp/"];',
  'const MODEL_ALIAS_UUID_PREFIX = /^byok-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\\//i;',
].join('\n');

class ConvElement {
  constructor(tag) {
    this.tagName = String(tag || 'div').toUpperCase();
    this.children = [];
    this.listeners = new Map();
    this.dataset = {};
    this.value = '';
    this.textContent = '';
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.classList = { add() {}, remove() {}, toggle() {} };
  }
  append(...nodes) {
    for (const node of nodes) {
      if (node && typeof node === 'object') node.parent = this;
      this.children.push(node);
    }
  }
  replaceChildren(...nodes) {
    this.children = [];
    this.append(...nodes);
  }
  addEventListener(name, fn) {
    const list = this.listeners.get(name) || [];
    list.push(fn);
    this.listeners.set(name, list);
  }
  async fire(name, target = this) {
    for (const fn of this.listeners.get(name) || []) await fn({ target, key: name });
  }
  setAttribute() {}
  focus() { this.focused = true; }
  querySelector(selector) {
    const match = /^\[data-([a-z-]+)\]$/.exec(selector || '');
    const key = match ? match[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()) : '';
    const walk = (node) => {
      if (!node || typeof node !== 'object') return null;
      if (key && node.dataset && Object.prototype.hasOwnProperty.call(node.dataset, key)) return node;
      for (const child of node.children || []) {
        const found = walk(child);
        if (found) return found;
      }
      return null;
    };
    return walk(this);
  }
  querySelectorAll(selector) {
    const match = /^\[data-([a-z-]+)\]$/.exec(selector || '');
    const key = match ? match[1].replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()) : '';
    const out = [];
    const walk = (node) => {
      if (!node || typeof node !== 'object') return;
      if (key && node.dataset && Object.prototype.hasOwnProperty.call(node.dataset, key)) out.push(node);
      for (const child of node.children || []) walk(child);
    };
    walk(this);
    return out;
  }
}

function makeConversationPageEnv({ kimiRunning = false, models = null, conversations = null, reasonText = makeReasonText() } = {}) {
  const ui = {};
  for (const name of ['guide', 'guideText', 'refresh', 'status', 'result', 'query', 'count', 'list', 'empty',
    'emptyNote', 'migration', 'migrationText', 'migrateAll', 'batch', 'batchModel', 'batchEffort', 'batchGo',
    'batchMigrate', 'batchNote', 'confirm', 'confirmText', 'confirmSubmit', 'confirmCancel']) {
    ui[name] = new ConvElement('div');
  }
  ui.queryClear = new ConvElement('button');
  ui.result.hidden = true;
  ui.migration.hidden = true;
  ui.confirm.hidden = true;
  const rows = conversations || [
    { conversationKey: 'recent', title: '重构网关路由', workspacePath: '/Users/dev/projects/kimiwork-plus', updatedAtMs: Date.now() - 120000, modelAlias: 'k2d8-preview', isByok: false },
    { conversationKey: 'older', title: '写一个排序算法', workspacePath: 'C:\\work\\algo', updatedAtMs: Date.now() - 7200000, modelAlias: 'byok-kth-kimi/kimi-k3-kwp-' + 'a'.repeat(64), isByok: true },
  ];
  const listModels = models || [makeConversationModel()];
  const calls = [];
  const toasts = [];
  const state = {
    key: 'session-key',
    view: 'conversations',
    data: { integrationMode: 'gateway', models: [{ id: 'deepseek-v4-1-flash', name: 'DeepSeek V4.1 Flash' }] },
    conversations: { list: null, loading: false, error: '', query: '', stale: false, pending: false,
      targets: {}, efforts: {}, selected: [], noConfirm: false, result: '', confirm: null },
  };
  let nextList = { conversations: rows, models: listModels, application: kimiRunning ? 'running' : 'not_running', kimiRunning };
  let reply = { conversationKeys: ['recent'], targetModelAlias: listModels[0]?.alias, kimiRestarted: kimiRunning };
  const request = async (path, options) => {
    calls.push([path, options]);
    if (path === '/api/work/conversations') return nextList;
    if (path === '/api/work/conversations/rebind') {
      if (reply instanceof Error) throw reply;
      return reply;
    }
    return {};
  };
  const dom = {
    convGuide: ui.guide, convGuideText: ui.guideText, convRefresh: ui.refresh, convStatus: ui.status,
    convResult: ui.result, convQ: ui.query, convCount: ui.count, convList: ui.list, convEmpty: ui.empty,
    convEmptyNote: ui.emptyNote, convMigrationTip: ui.migration, convMigrationText: ui.migrationText,
    convMigrateAll: ui.migrateAll, convBatch: ui.batch, convBatchModel: ui.batchModel, convBatchEffort: ui.batchEffort,
    convBatchGo: ui.batchGo, convBatchMigrate: ui.batchMigrate, convBatchNote: ui.batchNote,
    convConfirm: ui.confirm, convConfirmText: ui.confirmText, convConfirmSubmit: ui.confirmSubmit,
    convConfirmCancel: ui.confirmCancel,
  };
  const code = [
    CONVERSATION_CONSTANTS,
    'const $ = id => id === "conv-q-clear" ? ui.queryClear : null;',
    'const savedIntegrationMode = data => data?.integrationMode === "direct" ? "direct" : "gateway";',
    sourceFunction('sourceWord'),
    sourceFunction('displayNameOf'),
    sourceFunction('withSource'),
    sourceFunction('directoryNameOf'),
    sourceFunction('stripModelAliasPrefix'),
    sourceFunction('aliasSourceKind'),
    sourceFunction('humanizeConversationModel'),
    sourceFunction('canSwitchConversations'),
    sourceFunction('migrationMatchesOf'),
    sourceFunction('relativeTime'),
    sourceFunction('workspaceTail'),
    sourceFunction('conversationListStatus'),
    sourceFunction('conversationReasonText'),
    sourceFunction('effortChoices'),
    sourceFunction('defaultEffortFor'),
    'const dom = __dom;',
    sourceFunction('mountConversationsPage'),
    'return mountConversationsPage;',
  ].join('\n');
  const factory = new Function('__dom', 'ui', 'state', 'request', 'document', 'API', 'setHidden', 'setText', 'showToast', 'loadState', 'reasonText', code)(
    dom, ui, state, request, { createElement: tag => new ConvElement(tag) },
    { workConversations: '/api/work/conversations', rebindConversations: '/api/work/conversations/rebind' },
    (el, hidden) => { if (el) el.hidden = Boolean(hidden); },
    (el, text) => { if (el) el.textContent = text; },
    (message, tone) => toasts.push([message, tone]), async () => true, reasonText,
  );
  return { ui, state, calls, toasts, page: factory(), setReply: (value) => { reply = value; }, setList: (value) => { nextList = value; } };
}

async function flushConversation() {
  for (let i = 0; i < 8; i += 1) await new Promise(resolve => setImmediate(resolve));
}

function findNode(root, predicate) {
  if (!root || typeof root !== 'object') return null;
  if (predicate(root)) return root;
  for (const child of root.children || []) {
    const found = findNode(child, predicate);
    if (found) return found;
  }
  return null;
}

test('conversation model aliases become readable names and never leak raw UUID or hash prefixes', () => {
  const fn = new Function([
    CONVERSATION_CONSTANTS,
    'const state = { data: { integrationMode: "gateway" } };',
    'const savedIntegrationMode = data => data?.integrationMode === "direct" ? "direct" : "gateway";',
    sourceFunction('stripModelAliasPrefix'),
    sourceFunction('sourceWord'),
    sourceFunction('displayNameOf'),
    sourceFunction('withSource'),
    sourceFunction('directoryNameOf'),
    sourceFunction('aliasSourceKind'),
    sourceFunction('humanizeConversationModel'),
    'return humanizeConversationModel;',
  ].join('\n'))();
  assert.equal(fn('k3-agent'), '官方 · K3');
  assert.equal(fn('k3d1-agent'), '官方 · K3.1 Pro');
  assert.equal(fn('k2d8-preview'), '官方 · K2.8 Preview');
  const gatewayAlias = 'byok-kth-kimi/kimi-k3-kwp-' + 'a'.repeat(64);
  const known = [{ alias: gatewayAlias, displayName: 'DeepSeek V4.1 Flash', sourceKind: 'kth' }];
  assert.equal(fn(gatewayAlias, known, []), 'DeepSeek V4.1 Flash · 网关');
  const uuidAlias = 'byok-12345678-1234-1234-1234-123456789abc/glm-5';
  /* `byok-<uuid>/` 是官方直连写入的别名：来源标签只看别名前缀，不跟当前模式。 */
  assert.equal(fn(uuidAlias, [], [{ id: 'glm-5', name: 'GLM-5.3 Flash' }]), 'GLM-5.3 Flash · 直连');
  assert.equal(fn('byok-kth-kimi/glm-5', [], [{ id: 'glm-5', name: 'GLM-5.3 Flash' }]), 'GLM-5.3 Flash · 网关');
  const unknown = fn('byok-kth-kimi/kimi-k3-kwp-' + 'c'.repeat(64), [], []);
  assert.doesNotMatch(unknown, /byok|kimi-k3-kwp/);
  assert.match(unknown, /目录未列出/);
  const previous = fn('byok-kth-kimi/group/deepseek', [{ alias: 'byok-kth-kimi/kimi-new', previousAlias: 'byok-kth-kimi/group/deepseek', displayName: 'DeepSeek V4.1 Flash', sourceKind: 'kth' }], []);
  assert.match(previous, /DeepSeek V4\.1 Flash/);
  assert.match(previous, /旧别名/);
});

test('the conversations page pins the most recent conversation and confirms a running Kimi only once', async () => {
  const env = makeConversationPageEnv({ kimiRunning: true });
  await env.page.refresh();
  assert.match(env.ui.guideText.textContent, /本机网关模式/);
  assert.match(env.ui.guideText.textContent, /K3\.1 Pro/);
  assert.match(env.ui.guideText.textContent, /无需在此操作/);
  assert.equal(env.ui.list.children.length, 2);
  const recent = env.ui.list.children[0];
  assert.match(recent.className, /conv-row--recent/);
  const tag = findNode(recent, node => node.textContent === '最近活跃会话');
  assert.ok(tag);
  const badge = findNode(recent, node => node.dataset?.convCurrent === 'recent');
  assert.equal(badge.textContent, '官方 · K2.8 Preview');
  assert.equal(findNode(env.ui.list.children[1], node => node.dataset?.convCurrent === 'older').textContent, 'DeepSeek V4.1 Flash · 网关');

  const switchButton = findNode(recent, node => node.dataset?.convSwitch === 'recent');
  await switchButton.fire('click');
  const confirmHost = findNode(recent, node => node.dataset?.convConfirm === 'recent');
  assert.equal(confirmHost.hidden, false);
  assert.equal(env.calls.filter(call => call[1]?.method === 'POST').length, 0, 'running Kimi waits for confirmation');

  const remember = findNode(confirmHost, node => node.dataset?.convNoconfirm === 'true');
  remember.checked = true;
  const submit = findNode(confirmHost, node => node.dataset?.convConfirmSubmit === 'recent');
  await submit.fire('click');
  await flushConversation();
  const post = env.calls.find(call => call[1]?.method === 'POST');
  assert.deepEqual(post[1].body, {
    conversationKeys: ['recent'], targetModelAlias: makeConversationModel().alias, thinkingLevel: '', restartKimi: true,
  });
  assert.match(env.ui.result.textContent, /已将 1 个会话切换/);
  assert.match(env.ui.result.textContent, /Kimi 已自动重启/);
  assert.equal(env.state.conversations.noConfirm, true);
});

test('a stopped Kimi switches a single row immediately without a restart dialog', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  const row = env.ui.list.children[1];
  const switchButton = findNode(row, node => node.dataset?.convSwitch === 'older');
  await switchButton.fire('click');
  await flushConversation();
  const post = env.calls.find(call => call[1]?.method === 'POST');
  assert.ok(post);
  assert.equal(post[1].body.restartKimi, false);
  assert.deepEqual(post[1].body.conversationKeys, ['older']);
  assert.equal(findNode(row, node => node.dataset?.convConfirm === 'older').hidden, true);
});

test('the conversations guide follows the saved integration mode and search filters by title or workspace', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  env.state.data.integrationMode = 'direct';
  env.page.show();
  assert.match(env.ui.guideText.textContent, /零网关直连模式/);
  assert.match(env.ui.guideText.textContent, /新开对话/);
  env.ui.query.value = '排序';
  await env.ui.query.fire('input');
  assert.equal(env.ui.list.children.length, 1);
  assert.match(env.ui.count.textContent, /显示 1 \/ 共 2/);
  env.ui.query.value = 'kimiwork-plus';
  await env.ui.query.fire('input');
  assert.equal(env.ui.list.children.length, 1);
  env.ui.query.value = '不存在';
  await env.ui.query.fire('input');
  assert.equal(env.ui.empty.hidden, false);
});

test('the alias migration entry only appears for real matches and migrates every matched model', async () => {
  const first = makeConversationModel({ alias: 'byok-kth-kimi/kimi-a', displayName: '模型 A', previousAlias: 'byok-kth-kimi/group/a' });
  const second = makeConversationModel({ alias: 'byok-kth-kimi/kimi-b', displayName: '模型 B', previousAlias: 'byok-kth-kimi/group/b' });
  const rows = [
    { conversationKey: 'recent', title: '新会话', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'k2d8-preview', isByok: false },
    { conversationKey: 'older', title: '旧会话', workspacePath: '/tmp/b', updatedAtMs: Date.now(), modelAlias: 'byok-kth-kimi/group/b', isByok: true },
  ];
  const env = makeConversationPageEnv({ kimiRunning: false, models: [first, second], conversations: rows });
  env.setReply({ conversationKeys: ['older'], kimiRestarted: false });
  await env.page.refresh();
  /* 只有 previousAlias 字段、没有匹配会话的模型不显示迁移提示。 */
  assert.equal(env.ui.migration.hidden, false);
  assert.match(env.ui.migrationText.textContent, /发现 1 条旧版模型别名会话/);
  assert.equal(env.ui.migrateAll.disabled, false);
  await env.ui.migrateAll.fire('click');
  await flushConversation();
  const posts = env.calls.filter(call => call[1]?.method === 'POST');
  assert.equal(posts.length, 1, 'only the model with a real match is migrated, and only once');
  assert.equal(posts[0][1].body.targetModelAlias, second.alias);
  assert.equal(posts[0][1].body.migratePreviousAlias, true);
  assert.equal(posts[0][1].body.restartKimi, false);
  assert.match(env.ui.result.textContent, /已迁移 1 个旧别名会话/);

  const quiet = makeConversationPageEnv({ kimiRunning: false, models: [first], conversations: rows });
  await quiet.page.refresh();
  assert.equal(quiet.ui.migration.hidden, true, 'a previousAlias without matching conversations must stay quiet');
  assert.equal(quiet.ui.migrateAll.disabled, true);

  /* 多个模型各自有命中时逐个迁移，只有最后一笔请求带重启标记。 */
  const multiRows = [
    { conversationKey: 'k1', title: 'A', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'byok-kth-kimi/group/a', isByok: true },
    { conversationKey: 'k2', title: 'B', workspacePath: '/tmp/b', updatedAtMs: Date.now(), modelAlias: 'byok-kth-kimi/group/b', isByok: true },
  ];
  const multi = makeConversationPageEnv({ kimiRunning: true, models: [first, second], conversations: multiRows });
  multi.setReply({ conversationKeys: ['k2'], kimiRestarted: true });
  await multi.page.refresh();
  assert.match(multi.ui.migrationText.textContent, /发现 2 条旧版模型别名会话/);
  await multi.ui.migrateAll.fire('click');
  /* Kimi 运行中先确认一次，不直接发请求。 */
  assert.equal(multi.calls.filter(call => call[1]?.method === 'POST').length, 0);
  assert.equal(multi.ui.confirm.hidden, false);
  assert.match(multi.ui.confirmText.textContent, /2 条旧别名会话/);
  await multi.ui.confirmSubmit.fire('click');
  await flushConversation();
  const migrationPosts = multi.calls.filter(call => call[1]?.method === 'POST');
  assert.equal(migrationPosts.length, 2);
  assert.equal(migrationPosts[0][1].body.targetModelAlias, first.alias);
  assert.equal(migrationPosts[0][1].body.restartKimi, false, 'earlier models must not restart Kimi');
  assert.equal(migrationPosts[1][1].body.targetModelAlias, second.alias);
  assert.equal(migrationPosts[1][1].body.restartKimi, true, 'only the last request restarts Kimi once');
  assert.match(multi.ui.result.textContent, /已迁移 2 个旧别名会话/);
});

test('the restart banner clicks restart-kimi directly and only credential cases navigate', async () => {
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const start = appText.indexOf("if (dom.restartBannerLink) dom.restartBannerLink.addEventListener('click'");
  assert.notEqual(start, -1);
  const block = appText.slice(start, appText.indexOf('});', start) + 3);
  const link = { dataset: {}, addEventListener(type, fn) { this.click = fn; } };
  const calls = [];
  const handler = new Function('dom', 'runAction', 'setView', 'revealWorkspaceSection', block);
  handler({ restartBannerLink: link }, action => calls.push(['action', action]),
    view => calls.push(['view', view]), section => calls.push(['section', section]));

  link.dataset.bannerAction = 'restart-kimi';
  await link.click();
  assert.deepEqual(calls, [['action', 'restart-kimi']]);

  calls.length = 0;
  link.dataset.bannerAction = 'models';
  await link.click();
  assert.deepEqual(calls, [['view', 'models'], ['section', 'restart-panel']]);
});

test('the overview update banner primary action follows the backend state', async () => {
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const start = appText.indexOf("if (dom.updateBannerPrimary) dom.updateBannerPrimary.addEventListener('click'");
  assert.notEqual(start, -1);
  const end = appText.indexOf('if (dom.updateBannerNotes)', start);
  assert.ok(end > start);
  const block = appText.slice(start, end);
  const primary = { dataset: { updateAction: 'download' }, disabled: false, addEventListener(type, fn) { this.click = fn; } };
  const calls = [];
  const handler = new Function('dom', 'state', 'showToast', 'renderUpdates', 'updateAction', block);
  handler({ updateBannerPrimary: primary, updateBannerNotes: null }, { data: {} }, () => {}, () => {},
    async action => { calls.push(action); });

  await primary.click();
  assert.deepEqual(calls, ['download']);
  primary.dataset.updateAction = 'apply';
  await primary.click();
  assert.deepEqual(calls, ['download', 'apply']);
});

test('app.js exposes the tray navigation hooks and only scrolls .main', () => {
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  assert.match(appText, /window\.__kwpOpenAccounts\s*=/);
  assert.match(appText, /window\.__kwpOpenConversations\s*=/);
  assert.match(appText, /window\.__kwpOpenUpdates\s*=/);
  const block = appText.slice(appText.indexOf('function openUpdatesPanel()'), appText.indexOf('function bindEvents()'));
  assert.match(block, /setView\('maintenance'\)/);
  assert.match(block, /setSubtab\('maintenance', 'connection'\)/);
  assert.match(block, /scrollMainToElement\(panel\)/);
  assert.doesNotMatch(block, /scrollIntoView/);
  assert.match(block, /focus\(\{ preventScroll: true \}\)/);
  assert.match(htmlSource, /id="settings-tab-connection"/);
  assert.match(htmlSource, /id="update-panel"/);
  assert.match(htmlSource, /id="upd-check"/);
});

test('focus update checks skip fresh results, throttle repeats and stay silent', async () => {
  const calls = [];
  const toasts = [];
  const now = Date.parse('2026-11-25T12:00:00+08:00');
  const state = {
    key: 'session', lastFocusUpdateCheckAt: 0,
    data: { updates: { autoCheck: true, state: 'idle', lastCheckedAt: new Date(now - 10 * 60 * 1000).toISOString() } },
  };
  const code = [
    'const FOCUS_UPDATE_MIN_INTERVAL_MS = 60_000;',
    'const FOCUS_UPDATE_STALE_MS = 30 * 60 * 1000;',
    'const FOCUS_UPDATE_BUSY_STATES = ["checking", "downloading", "verifying", "downloaded", "applying", "rolling_back", "waiting_idle"];',
    sourceFunction('focusUpdateDue'),
    sourceFunction('runFocusUpdateCheck'),
    sourceFunction('maybeCheckUpdatesOnFocus'),
    'return { focusUpdateDue, maybeCheckUpdatesOnFocus };',
  ].join('\n');
  const api = new Function('state', 'request', 'API', 'loadState', 'renderUpdates', code)(
    state,
    async (path, options) => { calls.push([path, options]); return { updates: { state: 'up_to_date' } }; },
    { updatesCheck: '/api/updates/check' },
    async () => true,
    () => {},
  );
  assert.equal(api.maybeCheckUpdatesOnFocus({ now }), false, 'a 10 minute old check is fresh enough');
  assert.equal(calls.length, 0);
  state.data.updates.lastCheckedAt = new Date(now - 31 * 60 * 1000).toISOString();
  assert.equal(api.maybeCheckUpdatesOnFocus({ now }), true);
  await flushConversation();
  assert.deepEqual(calls[0], ['/api/updates/check', { method: 'POST', body: {} }]);
  assert.equal(api.maybeCheckUpdatesOnFocus({ now: now + 5000 }), false, '60 second throttle');
  state.data.updates.autoCheck = false;
  state.data.updates.lastCheckedAt = '';
  assert.equal(api.maybeCheckUpdatesOnFocus({ now: now + 120000 }), false);
  state.data.updates.autoCheck = true;
  state.data.updates.state = 'downloading';
  assert.equal(api.maybeCheckUpdatesOnFocus({ now: now + 240000 }), false);
  state.data.updates.state = 'idle';
  state.data.updates.lastCheckedAt = '';
  assert.equal(api.maybeCheckUpdatesOnFocus({ now: now + 360000 }), true);
  await flushConversation();
  assert.equal(calls.length, 2);
  assert.deepEqual(toasts, [], 'silent checks never surface error toasts');
});

test('an available update lights the settings dot and the overview banner with direct actions', () => {
  const makeEl = () => ({ textContent: '', hidden: true, disabled: false, checked: false, value: '', style: {},
    dataset: {}, attrs: {}, classList: { toggle() {} }, setAttribute(name, value) { this.attrs[name] = String(value); },
    removeAttribute(name) { delete this.attrs[name]; } });
  const ids = ['upd-current', 'upd-current-sub', 'upd-state-dot', 'upd-state-word', 'upd-last-check', 'upd-source',
    'upd-channel', 'upd-auto-check', 'upd-release-box', 'upd-release-title', 'upd-release-notes',
    'update-top-banner', 'update-top-banner-text', 'update-banner', 'update-banner-text', 'update-banner-primary',
    'nav-update-dot', 'overview-update-card', 'overview-update-title', 'overview-update-notes',
    'upd-progress-wrap', 'upd-progress-bar', 'upd-progress-label', 'upd-error-box', 'upd-error-msg',
    'upd-error-next', 'upd-error-detail', 'upd-history-note', 'upd-check', 'upd-download', 'upd-cancel',
    'upd-rollback', 'upd-release-link'];
  const nodes = Object.fromEntries(ids.map(id => [id, makeEl()]));
  const $ = id => nodes[id] || null;
  const state = { view: 'overview' };
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
  `)($, { activeElement: null }, state, () => {}, (el, text) => { if (el) el.textContent = text; }, () => '12:00');

  renderUpdates({ updates: { state: 'available', available: { version: '1.1.11', notes: '修复若干问题' } } });
  assert.equal(nodes['nav-update-dot'].hidden, false);
  assert.equal(nodes['update-banner'].hidden, false);
  assert.match(nodes['update-banner-text'].textContent, /发现新版本 v1\.1\.11/);
  assert.equal(nodes['update-banner-primary'].dataset.updateAction, 'download');
  assert.equal(nodes['update-banner-primary'].textContent, '立即下载并更新');
  /* 旧的重复卡片已从页面移除：总览只有顶部一条 #update-banner。 */
  assert.equal(htmlSource.includes('overview-update-card'), false);

  /* 下载/校验/等待期间只说明进度，不提供后端不存在的安装动作。 */
  renderUpdates({ updates: { state: 'downloading', available: { version: '1.1.11' } } });
  assert.equal(nodes['update-banner'].hidden, false);
  assert.equal(nodes['update-banner-primary'].hidden, true);
  assert.match(nodes['update-banner-text'].textContent, /正在下载更新/);
  assert.equal(nodes['nav-update-dot'].hidden, true, 'only a plain available state lights the settings dot');

  renderUpdates({ updates: { state: 'downloaded', available: { version: '1.1.11' } } });
  assert.equal(nodes['update-banner-primary'].dataset.updateAction, 'download');
  assert.notEqual(nodes['update-banner-primary'].textContent, '立即安装并重启');

  renderUpdates({ updates: { state: 'up_to_date' } });
  assert.equal(nodes['nav-update-dot'].hidden, true);
  assert.equal(nodes['update-banner'].hidden, true);

  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  assert.match(appText, /dom\.updateBannerPrimary\.dataset\.updateAction/);
  assert.match(appText, /await updateAction\(action, \{\}\)/);
  assert.match(appText, /window\.__kwpOpenUpdates\(\)|openUpdatesPanel\(\)/);
});

test('the front-end update endpoints match the bridge routes and never call a missing apply route', () => {
  const coreText = readFileSync(new URL('./app-core.js', import.meta.url), 'utf8');
  const overviewText = readFileSync(new URL('./app-overview.js', import.meta.url), 'utf8');
  const serverText = readFileSync(new URL('../internal/bridge/server.go', import.meta.url), 'utf8');
  assert.doesNotMatch(coreText, /updatesApply/);
  assert.doesNotMatch(overviewText, /updates\/apply/);
  for (const route of ['check', 'download', 'cancel', 'rollback', 'settings']) {
    assert.match(serverText, new RegExp('"/api/updates/' + route + '"'));
    assert.match(overviewText, new RegExp("'/api/updates/" + route + "'"));
  }
  assert.doesNotMatch(serverText, /\/api\/updates\/apply"/);
  /* 更新横幅的主按钮只下载；安装由后台在空闲时自动完成。 */
  const endpoints = overviewText.slice(overviewText.indexOf('const UPDATE_ENDPOINTS = {'), overviewText.indexOf('};', overviewText.indexOf('const UPDATE_ENDPOINTS = {')));
  assert.doesNotMatch(endpoints, /apply/);
});

/* ---------- 总览状态机与后台自动同步的回归 ---------- */

function makeOverviewState() {
  return new Function(`
    const fmtCount = value => String(value);
    const isInFlight = op => op?.state === 'running';
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('autoSyncOf')}
    ${sourceFunction('syncNeedsImport')}
    ${sourceFunction('overviewState')}
    return overviewState;
  `)();
}

function makeRestartBannerState() {
  return new Function(`
    const credentialAuthPending = () => false;
    ${INTEGRATION_HELPERS.join('\n')}
    ${sourceFunction('autoSyncOf')}
    ${sourceFunction('syncNeedsImport')}
    ${sourceFunction('restartBannerState')}
    return restartBannerState;
  `)();
}

function makeOverviewStatusEnv() {
  const nodes = {};
  for (const id of ['ob-verdict-dot', 'ob-verdict-word', 'ob-verdict-note', 'ob-launch', 'ob-launch-hint', 'ob-retry-apply', 'handoff']) {
    nodes[id] = { id, textContent: '', hidden: false, disabled: false, dataset: {}, setAttribute() {} };
  }
  nodes['ob-launch-hint'].hidden = true;
  const dom = {
    onboardVerdictDot: nodes['ob-verdict-dot'],
    onboardVerdictWord: nodes['ob-verdict-word'],
    onboardVerdictNote: nodes['ob-verdict-note'],
    onboardLaunch: nodes['ob-launch'],
    onboardLaunchHint: nodes['ob-launch-hint'],
    onboardRetryApply: nodes['ob-retry-apply'],
    handoff: nodes.handoff,
  };
  const code = [
    'const ACTION_LABEL = { "launch-kimi": "启动 Kimi Work", "open-work": "打开 Kimi Work", "restart-kimi": "重启 Kimi" };',
    'const setDot = (element, tone) => { if (element) element.dataset.tone = tone; };',
    'const setText = (element, text) => { if (element) element.textContent = text; };',
    'const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };',
    'const isInFlight = op => op?.state === "running";',
    'const OVERVIEW_NAV_ACTIONS = new Set(["restart-kimi", "open-models", "open-connection", "connect-kimi"]);',
    'const OVERVIEW_LAUNCH_LABELS = { "launch-kimi": "启动 Kimi Work", "open-work": "打开 Kimi Work" };',
    'const renderShell = () => {};',
    'const renderOperation = () => {};',
    'const renderActions = () => {};',
    'const plusPanel = { render() {} };',
    'const renderPreviewLifecycle = () => {};',
    ...INTEGRATION_HELPERS,
    sourceFunction('autoSyncOf'),
    sourceFunction('syncNeedsImport'),
    sourceFunction('overviewState'),
    sourceFunction('overviewLaunchAction'),
    sourceFunction('overviewLaunchLabel'),
    sourceFunction('renderOverviewStatus'),
    /* 与真实的 renderOverview 一致：总览主卡只有这一个渲染入口。 */
    'const renderOverview = data => renderOverviewStatus(data);',
    sourceFunction('renderAll'),
    'return { renderAll, overviewState };',
  ].join('\n');
  const state = { pendingAction: null, view: 'overview', actionButtons: [], data: null };
  const api = new Function('dom', 'state', code)(dom, state);
  return { nodes, state, api };
}

test('renderAll keeps the overview verdict and action for every auto-sync state', () => {
  const { nodes, state, api } = makeOverviewStatusEnv();
  const integration = { mode: 'gateway', imported: true, applied: true, adopted: true, desktop: { managed: true } };
  const cases = [
    ['requires_restart', 'restart', 'restart-kimi'],
    ['syncing', 'syncing', 'open-work'],
    ['deferred_offline', 'deferred', 'open-work'],
    ['waiting_unmanaged', 'takeover', 'restart-kimi'],
    ['synced', 'ready', 'open-work'],
  ];
  for (const [syncState, code, action] of cases) {
    const data = {
      integrationMode: 'gateway',
      models: [{ id: 'conn/resp/a' }],
      kimiProcess: { running: true, state: 'running' },
      integration: { ...integration, autoSync: { state: syncState } },
      operation: { action: 'auto-sync', state: syncState },
    };
    const expected = api.overviewState(data);
    assert.equal(expected.code, code, syncState + ' must map to ' + code);
    state.data = data;
    api.renderAll();
    assert.equal(nodes['ob-verdict-word'].textContent, expected.word,
      syncState + ': the old onboard overview must not overwrite the verdict');
    assert.equal(nodes['ob-launch'].dataset.action, action, syncState + ': the launch action must follow the verdict');
    assert.equal(nodes['ob-retry-apply'].hidden, true, syncState + ': the legacy retry-apply button stays hidden');
    assert.equal(nodes['handoff'].dataset.tone, expected.tone);
  }
  /* 旧实现里的第二个入口与它的私有 launch 推导都已删除。 */
  assert.doesNotMatch(source, /renderOnboardOverview\(state\.data\)/);
  assert.doesNotMatch(source, /function renderLaunch\(/);
  const start = source.indexOf('function renderOnboardOverview(');
  const delegate = source.slice(start, source.indexOf('\n}', start));
  assert.match(delegate, /renderOverviewStatus\(data\)/);
  assert.doesNotMatch(delegate, /syncState|renderLaunch|onboardRetryApply/);
});

test('a finished auto-sync no longer sticks on “正在自动同步…” or leaks the raw state code', () => {
  const table = source.slice(source.indexOf('const ACTION_STATE = {'), source.indexOf('\n};', source.indexOf('const ACTION_STATE = {')) + 3);
  for (const key of ['synced', 'deferred_offline', 'waiting_unmanaged', 'requires_restart', 'waiting_login', 'idle', 'scheduled', 'syncing']) {
    assert.match(table, new RegExp(key + ':'), 'ACTION_STATE must translate ' + key);
  }
  const nodes = {
    opbar: { dataset: {}, hidden: false },
    opbarDot: { dataset: {} },
    opbarName: { textContent: '' },
    opbarNote: { textContent: '' },
  };
  const renderOperation = new Function('dom', `
    const ACTION_LABEL = { 'auto-sync': '自动同步模型目录' };
    ${table}
    const setDot = (element, tone) => { if (element) element.dataset.tone = tone; };
    const setText = (element, text) => { if (element) element.textContent = text; };
    const setHidden = (element, hidden) => { if (element) element.hidden = Boolean(hidden); };
    ${REASON_TEXT_SOURCES.join('\n')}
    const AUTO_SYNC_QUIET_STATES = new Set(['synced', 'idle', 'deferred_offline']);
    ${sourceFunction('isInFlight')}
    ${sourceFunction('operationNote')}
    ${sourceFunction('renderOperation')}
    return renderOperation;
  `)(nodes);

  for (const state of ['synced', 'idle', 'deferred_offline']) {
    nodes.opbar.hidden = false;
    renderOperation({ operation: { action: 'auto-sync', state } });
    assert.equal(nodes.opbar.hidden, true, state + ' is a quiet finished state');
  }
  /* 读回确认目录生效后，残留的 requires_restart 操作记录必须让位给
     integration.autoSync；仍在同步的状态则继续显示这条提示。 */
  nodes.opbar.hidden = false;
  renderOperation({ operation: { action: 'auto-sync', state: 'requires_restart' },
    integration: { autoSync: { state: 'synced' } } });
  assert.equal(nodes.opbar.hidden, true, 'a settled readback retires the stale restart strip');
  renderOperation({ operation: { action: 'auto-sync', state: 'requires_restart' },
    integration: { autoSync: { state: 'syncing' } } });
  assert.equal(nodes.opbar.hidden, false, 'a live sync keeps the strip visible');
  renderOperation({ operation: { action: 'auto-sync', state: 'running' } });
  assert.equal(nodes.opbar.hidden, false);
  assert.match(nodes.opbarName.textContent, /正在自动同步模型目录/);
  assert.equal(nodes.opbarDot.dataset.tone, 'warn');
  renderOperation({ operation: { action: 'auto-sync', state: 'syncing' } });
  assert.match(nodes.opbarName.textContent, /正在自动同步模型目录/);
  renderOperation({ operation: { action: 'auto-sync', state: 'scheduled' } });
  assert.match(nodes.opbarName.textContent, /等待自动同步模型目录/);
  renderOperation({ operation: { action: 'auto-sync', state: 'requires_restart' } });
  assert.match(nodes.opbarName.textContent, /待重启 Kimi 生效/);
  assert.doesNotMatch(nodes.opbarName.textContent, /requires_restart/);
  assert.equal(nodes.opbarDot.dataset.tone, 'warn');
  renderOperation({ operation: { action: 'auto-sync', state: 'failed', reason: 'direct_import_failed' } });
  assert.equal(nodes.opbarDot.dataset.tone, 'bad');
  assert.match(nodes.opbarNote.textContent, /官方 Kimi 没有接受这次直连写入/);
  assert.doesNotMatch(nodes.opbarNote.textContent, /direct_import_failed/);
  assert.doesNotMatch(nodes.opbarName.textContent, /failed|synced|deferred_offline/);
});

test('a removed takeover survives later read-only operations and direct limits never ask for a restart', () => {
  const overviewState = makeOverviewState();
  const restartBannerState = makeRestartBannerState();
  const removed = {
    integrationMode: 'gateway',
    models: [{ id: 'conn/resp/a' }],
    integration: { imported: false, autoSync: { reason: 'disconnected' } },
    operation: { action: 'check-compatibility', state: 'done' },
  };
  assert.equal(overviewState(removed).code, 'restored');
  assert.equal(restartBannerState(removed), null, 'a removed takeover must not promote a restart');

  const directLimited = {
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/a' }],
    kimiProcess: { running: true, state: 'running' },
    integration: { desktop: { managed: false }, autoSync: { state: 'waiting_unmanaged' } },
    directMode: { supported: false },
  };
  const unsupported = overviewState(directLimited);
  assert.equal(unsupported.code, 'unsupported');
  assert.equal(unsupported.launch, 'open-connection');
  assert.equal(restartBannerState(directLimited), null);

  const noEligible = { ...directLimited, directMode: { supported: true, eligibleModels: 0 } };
  const noEligibleVerdict = overviewState(noEligible);
  assert.equal(noEligibleVerdict.code, 'no-eligible');
  assert.equal(noEligibleVerdict.launch, 'open-models');
  assert.match(noEligibleVerdict.note, /工具调用/);
  assert.equal(restartBannerState(noEligible), null);

  /* 网关模式下旧直连来源欠清理时不能说「已就绪」。 */
  const cleanup = overviewState({
    integrationMode: 'gateway',
    models: [{ id: 'conn/resp/a' }],
    kimiProcess: { running: true },
    integration: { imported: true, applied: true, adopted: true, desktop: { managed: true },
      autoSync: { state: 'synced' },
      nativeImport: { reason: 'route_cleanup_pending', routeCleanupPending: true } },
  });
  assert.equal(cleanup.code, 'pending');
  assert.equal(cleanup.word, '切换收尾未完成');

  /* 同步失败与等待登录都不属于「已写入、只差重启」。 */
  const failedSync = { integrationMode: 'gateway', models: [{ id: 'a' }],
    kimiProcess: { running: true },
    integration: { imported: false, applied: true, adopted: false, desktop: { managed: true }, autoSync: { state: 'failed' } } };
  assert.equal(restartBannerState(failedSync), null);
  const waitingLogin = { ...failedSync, integration: { ...failedSync.integration, autoSync: { state: 'waiting_login' } } };
  assert.equal(restartBannerState(waitingLogin), null);
});

test('a provider mismatch asks for one repair sync instead of reporting ready', () => {
  const overviewState = makeOverviewState();
  const data = {
    integrationMode: 'gateway',
    models: [{ id: 'conn/resp/a' }],
    kimiProcess: { running: true, state: 'running' },
    integration: { mode: 'gateway', imported: true, applied: true, adopted: true,
      desktop: { managed: true }, autoSync: { state: 'synced' } },
    kimiK3State: 'provider_mismatch',
  };
  /* 官方用的是临时来源标识：模型确实导入了，但直切会新建会话，不能说已就绪。 */
  const verdict = overviewState(data);
  assert.equal(verdict.code, 'repair');
  assert.equal(verdict.tone, 'warn');
  assert.equal(verdict.word, '需修复原位切换');
  assert.equal(verdict.launch, 'connect-kimi');
  assert.equal(verdict.launchLabel, '重新同步修复');
  assert.match(verdict.note, /临时来源标识/);
  assert.match(verdict.note, /重新同步修复/);

  /* 正常就绪保持原样。 */
  for (const state of ['ready', '']) {
    const normal = overviewState({ ...data, kimiK3State: state });
    assert.equal(normal.code, 'ready', state + ' must keep the ready verdict');
    assert.equal(normal.word, '已就绪');
    assert.equal(normal.launch, 'launch-kimi');
  }

  /* 更紧要的状态排在修复之前：还没同步完、要重启时先说那件事。 */
  assert.equal(overviewState({ ...data, integration: { ...data.integration, autoSync: { state: 'requires_restart' } } }).code, 'restart');
  assert.equal(overviewState({ ...data, integration: { ...data.integration, autoSync: { state: 'syncing' } } }).code, 'syncing');

  /* 直连模式不消费这个字段。 */
  const direct = overviewState({
    integrationMode: 'direct',
    models: [{ id: 'conn/resp/a' }],
    kimiProcess: { running: true },
    kimiK3State: 'provider_mismatch',
    integration: { mode: 'direct', imported: true, applied: true, adopted: true,
      state: 'ready', aliases: { 'conn/resp/a': 'byok-src-1/model-a' },
      desktop: { managed: true }, autoSync: { state: 'synced' } },
    directMode: { supported: true, staticHostsReady: true, eligibleModelIds: ['conn/resp/a'] },
  });
  assert.equal(direct.code, 'ready');
});

test('the overview repair action runs the manual sync through the launch button', () => {
  const { nodes, state, api } = makeOverviewStatusEnv();
  state.data = {
    integrationMode: 'gateway',
    models: [{ id: 'conn/resp/a' }],
    kimiProcess: { running: true, state: 'running' },
    integration: { mode: 'gateway', imported: true, applied: true, adopted: true,
      desktop: { managed: true }, autoSync: { state: 'synced' } },
    kimiK3State: 'provider_mismatch',
  };
  api.renderAll();
  assert.equal(nodes['ob-verdict-word'].textContent, '需修复原位切换');
  assert.equal(nodes['ob-launch'].dataset.action, 'connect-kimi');
  assert.match(nodes['ob-launch'].textContent, /重新同步修复/);
  assert.equal(nodes['ob-launch-hint'].hidden, false);
  assert.match(nodes['ob-launch-hint'].textContent, /无需重启本软件/);
  assert.equal(nodes['handoff'].dataset.tone, 'warn');

  /* 正常就绪时按钮回到启动/打开，提示收起。 */
  state.data = { ...state.data, kimiK3State: 'ready' };
  api.renderAll();
  assert.equal(nodes['ob-launch'].dataset.action, 'open-work');
  assert.equal(nodes['ob-launch-hint'].hidden, true);
});

test('an empty model list sends the overview action to the models page instead of a launch', () => {
  const { nodes, state, api } = makeOverviewStatusEnv();
  state.data = { integrationMode: 'gateway', models: [], kimiProcess: { running: false } };
  api.renderAll();
  assert.equal(nodes['ob-launch'].dataset.action, 'open-models');
  assert.equal(nodes['ob-launch'].textContent, '前往添加模型');
  assert.equal(nodes['ob-launch-hint'].hidden, false);
});

test('canSwitchConversations refuses unreadable lists, unknown Kimi state and busy requests', () => {
  const state = { conversations: { loading: false, pending: false } };
  const fn = new Function('state', `
    ${sourceFunction('canSwitchConversations')}
    return canSwitchConversations;
  `)(state);
  assert.equal(fn(null), false);
  assert.equal(fn({ reason: 'conversation_database_missing' }), false);
  assert.equal(fn({ application: 'unknown' }), false);
  assert.equal(fn({ application: 'not_running' }), true);
  state.conversations.loading = true;
  assert.equal(fn({ application: 'not_running' }), false);
  state.conversations.loading = false;
  state.conversations.pending = true;
  assert.equal(fn({ application: 'not_running' }), false);
});

test('an unknown Kimi state disables every conversation switch entry', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  env.setList({
    conversations: [
      { conversationKey: 'recent', title: '新会话', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'k2d8-preview', isByok: false },
    ],
    models: [makeConversationModel()],
    application: 'unknown',
    kimiRunning: false,
  });
  await env.page.refresh();
  const row = env.ui.list.children[0];
  const switchButton = findNode(row, node => node.dataset?.convSwitch === 'recent');
  assert.equal(switchButton.disabled, true);
  assert.equal(findNode(row, node => node.dataset?.convTarget === 'recent').disabled, true);
  assert.equal(env.ui.batchGo.disabled, true);
  assert.equal(env.ui.batchMigrate.disabled, true);
  await switchButton.fire('click');
  await flushConversation();
  assert.equal(env.calls.filter(call => call[1]?.method === 'POST').length, 0, 'unknown state must never write a binding');

  /* 运行状态未知时 onState 必须把列表标成 unknown，而不是当成「未运行」。 */
  env.page.onState({ revision: 'rev-unknown', kimiProcess: { running: false, state: 'unknown' } });
  assert.equal(env.state.conversations.list.application, 'unknown');
  assert.equal(env.state.conversations.list.kimiRunning, false);
  await flushConversation();
});

test('the conversations page refreshes on return and keeps the user selection when polling', async () => {
  const rows = [
    { conversationKey: 'recent', title: '新会话', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'k2d8-preview', isByok: false },
    { conversationKey: 'older', title: '旧会话', workspacePath: '/tmp/b', updatedAtMs: Date.now(), modelAlias: 'k2d8-preview', isByok: false },
  ];
  const env = makeConversationPageEnv({ kimiRunning: false, conversations: rows });
  await env.page.refresh();
  env.state.conversations.selected = ['older'];
  env.state.conversations.targets.recent = env.state.conversations.list.models[0].alias;
  env.state.conversations.efforts.recent = 'high';

  /* 离页期间状态变化：只记 stale，等回到会话页再读。 */
  env.state.view = 'overview';
  env.page.onState({ revision: 'rev-1', kimiProcess: { running: false, state: 'not_running' } });
  env.page.onState({ revision: 'rev-2', kimiProcess: { running: false, state: 'not_running' } });
  assert.equal(env.state.conversations.stale, true);
  assert.equal(env.calls.filter(call => call[0] === '/api/work/conversations').length, 1, 'no background fetch while away');
  env.state.view = 'conversations';
  env.page.show();
  await flushConversation();
  assert.equal(env.calls.filter(call => call[0] === '/api/work/conversations').length, 2, 'returning to the page refreshes');
  assert.deepEqual(env.state.conversations.selected, ['older'], 'selection survives the refresh');
  assert.equal(env.state.conversations.efforts.recent, 'high', 'effort choice survives the refresh');
});

test('a save blocked by an advanced field opens the advanced drawer', () => {
  const patterns = source.slice(source.indexOf('const ADVANCED_ERROR_PATTERNS = ['),
    source.indexOf('\n];', source.indexOf('const ADVANCED_ERROR_PATTERNS = [')) + 3);
  const dom = { mAdvanced: { open: false } };
  const validateEditor = new Function('state', 'dom', `
    ${patterns}
    const buildModel = draft => ({ model: { id: draft.id, connectionId: '', protocol: 'responses' }, errors: draft.errors.slice() });
    const findConnection = () => null;
    const connectionLabel = () => '';
    const protocolLabel = value => value;
    ${sourceFunction('errorNeedsAdvanced')}
    ${sourceFunction('validateEditor')}
    return validateEditor;
  `)({ data: { models: [] } }, dom);

  validateEditor({ originalId: null, draft: { id: 'a', errors: [] } });
  assert.equal(dom.mAdvanced.open, false);
  validateEditor({ originalId: null, draft: { id: 'a', errors: ['备用连接最多 8 条。'] } });
  assert.equal(dom.mAdvanced.open, true, 'failover errors live inside the advanced drawer');
  dom.mAdvanced.open = false;
  validateEditor({ originalId: null, draft: { id: 'a', errors: ['定价必须同时填写输入与输出价格；只有一半无法估算花费。'] } });
  assert.equal(dom.mAdvanced.open, true, 'pricing errors live inside the advanced drawer');
  dom.mAdvanced.open = false;
  validateEditor({ originalId: null, draft: { id: 'a', errors: ['上下文窗口必须是正整数。'] } });
  assert.equal(dom.mAdvanced.open, false, 'main-field errors leave the drawer as it was');
});

test('a local build without an updater never triggers the focus check', () => {
  const focusUpdateDue = new Function('state', `
    const FOCUS_UPDATE_MIN_INTERVAL_MS = 60_000;
    const FOCUS_UPDATE_STALE_MS = 30 * 60 * 1000;
    const FOCUS_UPDATE_BUSY_STATES = ['checking', 'downloading', 'verifying', 'downloaded', 'applying', 'rolling_back', 'waiting_idle'];
    ${sourceFunction('focusUpdateDue')}
    return focusUpdateDue;
  `)({ lastFocusUpdateCheckAt: 0 });
  const now = Date.parse('2026-11-25T12:00:00+08:00');
  /* 'local' 是没有更新器的构建：后端每次都回 503，不能每 60 秒发一次。 */
  assert.equal(focusUpdateDue({ autoCheck: true, state: 'local', lastCheckedAt: '' }, now), false);
  assert.equal(focusUpdateDue({ autoCheck: true, state: 'idle', lastCheckedAt: '' }, now), true);
  assert.equal(focusUpdateDue({ autoCheck: false, state: 'idle', lastCheckedAt: '' }, now), false);
});

test('a tray update request before the shell is revealed is replayed once', () => {
  const calls = [];
  const dom = { shell: { hidden: true } };
  const panel = { tagName: 'SECTION' };
  const check = { focus(options) { calls.push(['focus', options]); } };
  const api = new Function('dom', '$', 'setView', 'setSubtab', 'scrollMainToElement', `
    let pendingUpdatesPanelReveal = false;
    ${sourceFunction('openUpdatesPanel')}
    ${sourceFunction('flushPendingUpdatesPanelReveal')}
    return { openUpdatesPanel, flushPendingUpdatesPanelReveal, pending: () => pendingUpdatesPanelReveal };
  `)(dom, id => (id === 'update-panel' ? panel : id === 'upd-check' ? check : null),
    view => calls.push(['view', view]), (group, tab) => calls.push(['subtab', group, tab]),
    element => calls.push(['scroll', element]));

  api.openUpdatesPanel();
  assert.deepEqual(calls, [['view', 'maintenance'], ['subtab', 'maintenance', 'connection']]);
  assert.equal(api.pending(), true, 'the reveal must wait for #shell');
  dom.shell.hidden = false;
  api.flushPendingUpdatesPanelReveal();
  assert.equal(calls.filter(call => call[0] === 'scroll' && call[1] === panel).length, 1);
  assert.equal(calls.filter(call => call[0] === 'focus').length, 1);
  assert.deepEqual(calls.find(call => call[0] === 'focus')[1], { preventScroll: true });
  assert.equal(api.pending(), false);
  api.flushPendingUpdatesPanelReveal();
  assert.equal(calls.filter(call => call[0] === 'scroll').length, 1, 'only replayed once');
});

test('the top-bar refresh also reloads the conversations page', () => {
  const appText = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const start = appText.indexOf('if (dom.refresh) dom.refresh.addEventListener');
  assert.notEqual(start, -1);
  const block = appText.slice(start, appText.indexOf('if (dom.connRetry)', start));
  assert.match(block, /state\.view === 'conversations'/);
  assert.match(block, /conversationsPanel\.refresh\(\)/);
});

test('the workspace bar only offers a manual resync where auto-sync cannot finish alone', () => {
  assert.match(htmlSource, /id="workspace-resync"/);
  assert.match(htmlSource, /id="workspace-resync"[^>]*data-action="connect-kimi"|data-action="connect-kimi"[^>]*id="workspace-resync"/);
  const { render, nodes } = makeWorkspaceSyncEnv();
  const models = [{ id: 'conn/resp/model-a' }];

  render({ integrationMode: 'gateway', models, integration: { imported: false },
    operation: { action: 'disconnect-models', state: 'removed' } });
  assert.equal(nodes['workspace-resync'].hidden, false);
  assert.match(nodes['workspace-sync-note'].textContent, /重新同步模型目录/);

  render({ integrationMode: 'direct', models, integration: { mode: 'gateway' } });
  assert.equal(nodes['workspace-resync'].hidden, false);

  render({ integrationMode: 'direct', models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: true, eligibleModels: 1, eligibleModelIds: ['conn/resp/model-a'] },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');
  assert.equal(nodes['workspace-resync'].hidden, true, 'a settled direct import needs no manual resync');

  render({ integrationMode: 'gateway', models,
    directMode: { mode: 'gateway', supported: true, routeCleanupPending: true },
    integration: { mode: 'gateway', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '切换收尾未完成');
  assert.equal(nodes['workspace-resync'].hidden, false);

  render({ integrationMode: 'gateway', models,
    compatibility: { guards: { modelSyncWriteBlocked: true } },
    integration: { mode: 'gateway', imported: true } });
  assert.equal(nodes['workspace-resync'].hidden, true, 'a compatibility gate cannot be bypassed by resync');

  render({ integrationMode: 'direct', models, directMode: { mode: 'direct', supported: false },
    integration: { mode: 'gateway' } });
  assert.equal(nodes['workspace-sync-state'].textContent, '当前官方 Kimi 不支持直连');
  assert.equal(nodes['workspace-resync'].hidden, true);

  render({ integrationMode: 'direct', models, directMode: { mode: 'direct', supported: true, eligibleModels: 0 },
    integration: { mode: 'gateway' } });
  assert.equal(nodes['workspace-sync-state'].textContent, '没有可直连的模型');
  assert.equal(nodes['workspace-resync'].hidden, true);
});

test('the batch migrate action follows the model selected in the toolbar, not the first one', async () => {
  const first = makeConversationModel({ alias: 'byok-kth-kimi/kimi-a', displayName: '模型 A', previousAlias: 'byok-kth-kimi/group/a' });
  const second = makeConversationModel({ alias: 'byok-kth-kimi/kimi-b', displayName: '模型 B', previousAlias: 'byok-kth-kimi/group/b' });
  const rows = [
    { conversationKey: 'k1', title: 'A', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'byok-kth-kimi/group/a', isByok: true },
    { conversationKey: 'k2', title: 'B', workspacePath: '/tmp/b', updatedAtMs: Date.now(), modelAlias: 'byok-kth-kimi/group/b', isByok: true },
  ];
  const env = makeConversationPageEnv({ kimiRunning: false, models: [first, second], conversations: rows });
  env.setReply({ conversationKeys: ['k2'], kimiRestarted: false });
  await env.page.refresh();
  assert.equal(env.ui.batchMigrate.disabled, false, 'the default first model also has a match');

  /* 选中第二个模型后，批量迁移必须迁移第二个模型的旧别名会话。 */
  env.ui.batchModel.value = second.alias;
  await env.ui.batchModel.fire('change');
  env.ui.batchEffort.value = 'high';
  await env.ui.batchEffort.fire('change');
  await env.ui.batchMigrate.fire('click');
  await flushConversation();
  const post = env.calls.find(call => call[1]?.method === 'POST');
  assert.equal(post[1].body.targetModelAlias, second.alias);
  assert.equal(post[1].body.thinkingLevel, 'high');
  assert.equal(post[1].body.migratePreviousAlias, true);

  /* 选中没有 previousAlias 的模型时迁移按钮必须禁用。 */
  env.state.conversations.batchModel = '';
  env.setList({ conversations: rows, models: [makeConversationModel({ alias: 'byok-kth-kimi/kimi-c' })], application: 'not_running', kimiRunning: false });
  await env.page.refresh();
  assert.equal(env.ui.batchMigrate.disabled, true);
});

test('a restart failure is translated instead of exposing the raw reason code', async () => {
  /* 直接使用 app-core.js 的真实 reasonText：生产表缺项时这条用例必须失败。 */
  const reasonText = makeReasonText();
  assert.match(reasonText('conversation_kimi_restart_failed'), /Kimi 未能自动重新打开/);
  const env = makeConversationPageEnv({ kimiRunning: true, reasonText });
  env.setReply({ conversationKeys: ['recent'], kimiRestarted: false, restartReason: 'conversation_kimi_restart_failed' });
  await env.page.refresh();
  const row = env.ui.list.children[0];
  const switchButton = findNode(row, node => node.dataset?.convSwitch === 'recent');
  await switchButton.fire('click');
  const submit = findNode(row, node => node.dataset?.convConfirmSubmit === 'recent');
  await submit.fire('click');
  await flushConversation();
  assert.match(env.ui.result.textContent, /Kimi 未能自动重新打开，请手动打开 Kimi Work 后继续/);
  assert.doesNotMatch(env.ui.result.textContent, /conversation_kimi_restart_failed/);
});

test('polling never rebuilds the conversations page while a confirmation is open', async () => {
  const env = makeConversationPageEnv({ kimiRunning: true });
  await env.page.refresh();
  const row = env.ui.list.children[0];
  const switchButton = findNode(row, node => node.dataset?.convSwitch === 'recent');
  await switchButton.fire('click');
  assert.equal(findNode(row, node => node.dataset?.convConfirm === 'recent').hidden, false);

  /* 轮询带来新 revision：展开中的确认条必须留着，只标记 stale。 */
  env.page.onState({ revision: 'rev-1', kimiProcess: { running: true, state: 'checked' } });
  env.page.onState({ revision: 'rev-2', kimiProcess: { running: true, state: 'checked' } });
  assert.equal(env.state.conversations.stale, true);
  assert.equal(env.calls.filter(call => call[0] === '/api/work/conversations').length, 1, 'no reload while confirming');
  assert.equal(findNode(env.ui.list.children[0], node => node.dataset?.convConfirm === 'recent').hidden, false);

  /* 轮询还会调用 show()：只要确认条还开着，就必须继续等。 */
  env.page.show();
  await flushConversation();
  assert.equal(env.calls.filter(call => call[0] === '/api/work/conversations').length, 1, 'show() must not rebuild the open confirm either');
  assert.equal(findNode(env.ui.list.children[0], node => node.dataset?.convConfirm === 'recent').hidden, false);
});

test('a known Kimi state releases the switch guard that an unknown state set', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  env.page.onState({ revision: 'rev-1', kimiProcess: { running: false, state: 'checked' } });
  env.page.onState({ revision: 'rev-2', kimiProcess: { running: false, state: 'unknown' } });
  assert.equal(env.state.conversations.list.application, 'unknown');
  env.page.onState({ revision: 'rev-3', kimiProcess: { running: false, state: 'checked' } });
  assert.equal(env.state.conversations.list.application, 'not_running');
  env.page.onState({ revision: 'rev-4', kimiProcess: { running: true, state: 'checked' } });
  assert.equal(env.state.conversations.list.kimiRunning, true);
  await flushConversation();
});

/* ---------- 原因码翻译与会话页边界（独立审查跟进） ---------- */

test('reasonText translates every reason code the UI can show', () => {
  const reasonText = makeReasonText();
  const codes = [
    'conversation_kimi_restart_failed',
    'native_import_failed',
    'native_import_store_changed',
    'native_import_foreign_source',
    'native_import_provider_reset_failed',
    'native_import_non_canonical_provider',
    'native_import_apply_failed',
    'model_provider_reset_pending',
    'direct_prepare_required',
    'model_sync_write_blocked',
    'configuration_unavailable',
    'kimi_not_running',
    'unmanaged_kimi_running',
    'disconnected',
    'waiting_login',
    'account_changed',
  ];
  for (const code of codes) {
    const text = reasonText(code);
    assert.ok(text && text !== code, code + ' must be translated');
    assert.match(text, /[\u4e00-\u9fff]/, code + ' must resolve to Chinese');
    assert.doesNotMatch(text, new RegExp(code), code + ' must not leak the raw code');
  }
  assert.match(reasonText('configuration_unavailable'), /尚未读取到本机配置/);
  assert.match(reasonText('direct_prepare_required'), /静态 hosts 系统授权/);
  assert.match(reasonText('conversation_kimi_restart_failed'), /Kimi 未能自动重新打开/);
  const nonCanonical = reasonText('native_import_non_canonical_provider');
  assert.match(nonCanonical, /原位切换/);
  assert.match(nonCanonical, /重新同步/);
  assert.doesNotMatch(nonCanonical, /native_import/);
  const applyFailed = reasonText('native_import_apply_failed');
  assert.match(applyFailed, /生效/);
  assert.doesNotMatch(applyFailed, /native_import/);
});

test('the workspace bar offers a manual resync when static hosts are still missing', () => {
  const { render, nodes } = makeWorkspaceSyncEnv();
  const models = [{ id: 'conn/resp/model-a' }];
  render({ integrationMode: 'direct', models,
    directMode: { mode: 'direct', supported: true, staticHostsReady: false,
      eligibleModels: 1, eligibleModelIds: ['conn/resp/model-a'] },
    integration: { mode: 'direct', state: 'ready', applied: true, adopted: true,
      aliases: { 'conn/resp/model-a': 'byok-uuid/model-a' } } });
  assert.equal(nodes['workspace-sync-state'].textContent, '已直连同步到 Kimi');
  assert.match(nodes['workspace-sync-note'].textContent, /补齐静态 hosts/);
  assert.equal(nodes['workspace-resync'].hidden, false, 'missing static hosts needs one manual resync');
});

test('a deleted model drops its stale effort and batch selection instead of submitting an invalid level', async () => {
  const modelA = makeConversationModel({ alias: 'byok-kth-kimi/kimi-a', displayName: '模型 A', supportEfforts: ['low', 'high'], defaultEffort: '' });
  const modelB = makeConversationModel({ alias: 'byok-kth-kimi/kimi-b', displayName: '模型 B', supportEfforts: ['medium'], defaultEffort: '' });
  const rows = [
    { conversationKey: 'recent', title: '新会话', workspacePath: '/tmp/a', updatedAtMs: Date.now(), modelAlias: 'k2d8-preview', isByok: false },
  ];
  const env = makeConversationPageEnv({ kimiRunning: false, models: [modelA], conversations: rows });
  await env.page.refresh();
  env.state.conversations.targets.recent = modelA.alias;
  env.state.conversations.efforts.recent = 'high';
  env.state.conversations.batchModel = modelA.alias;
  env.state.conversations.batchEffort = 'high';

  /* 模型 A 被删掉，只剩不支持 high 的模型 B。 */
  env.setList({ conversations: rows, models: [modelB], application: 'not_running', kimiRunning: false });
  await env.page.refresh();
  assert.equal(env.state.conversations.targets.recent, modelB.alias);
  assert.equal(env.state.conversations.efforts.recent, undefined, 'high is not a valid level for model B');
  /* 失效的模型 A 别名被丢弃；渲染后回落到当前唯一的模型 B 与它的默认档位。 */
  assert.notEqual(env.state.conversations.batchModel, modelA.alias);
  assert.equal(env.state.conversations.batchModel, modelB.alias);
  assert.equal(env.state.conversations.batchEffort, 'medium');
  assert.equal(env.ui.batchModel.value, modelB.alias);
  const row = env.ui.list.children[0];
  assert.equal(findNode(row, node => node.dataset?.convEffort === 'true').value, 'medium',
    'the row effort falls back to the new model default');

  /* 仍有效的批量选择保留。 */
  env.state.conversations.batchModel = modelB.alias;
  env.state.conversations.batchEffort = 'medium';
  env.setList({ conversations: rows, models: [modelB], application: 'not_running', kimiRunning: false });
  await env.page.refresh();
  assert.equal(env.state.conversations.batchModel, modelB.alias);
  assert.equal(env.state.conversations.batchEffort, 'medium');
});

test('an invalid batch effort falls back to the selected model default', async () => {
  const model = makeConversationModel({ supportEfforts: ['medium'], defaultEffort: '' });
  const env = makeConversationPageEnv({ kimiRunning: false, models: [model] });
  await env.page.refresh();
  env.state.conversations.batchModel = model.alias;
  env.state.conversations.batchEffort = 'high';
  await env.page.refresh();
  assert.equal(env.ui.batchEffort.value, 'medium');
  assert.equal(env.state.conversations.batchEffort, 'medium');
});

test('the batch switch explains a missing target model instead of doing nothing', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false, models: [] });
  env.setList({ conversations: [], models: [], application: 'not_running', kimiRunning: false });
  await env.page.refresh();
  await env.ui.batchGo.fire('click');
  assert.ok(env.toasts.some(([message]) => /请先选择目标模型/.test(message)),
    'a click without a target model must explain itself');
});

test('the row effort select follows the same switch guard as the row button', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  const row = env.ui.list.children[0];
  assert.equal(findNode(row, node => node.dataset?.convEffort === 'true').disabled, false);
  env.setList({ conversations: env.state.conversations.list.conversations, models: [makeConversationModel()], application: 'unknown', kimiRunning: false });
  await env.page.refresh();
  const guarded = env.ui.list.children[0];
  assert.equal(findNode(guarded, node => node.dataset?.convEffort === 'true').disabled, true);
  assert.equal(findNode(guarded, node => node.dataset?.convTarget === 'recent').disabled, true);
  assert.equal(findNode(guarded, node => node.dataset?.convSwitch === 'recent').disabled, true);
});

test('the first state adopts an already-loaded conversation list', async () => {
  const env = makeConversationPageEnv({ kimiRunning: false });
  await env.page.refresh();
  assert.equal(env.ui.status.textContent.includes('无法确认'), false);
  /* 冷启动：会话先读到，首个 /api/state 才到，且运行状态未知。 */
  env.page.onState({ revision: 'rev-1', kimiProcess: { running: false, state: 'unknown' } });
  assert.match(env.ui.status.textContent, /无法确认 Kimi 的运行状态/);
  const row = env.ui.list.children[0];
  if (row) assert.equal(findNode(row, node => node.dataset?.convSwitch === 'recent').disabled, true);
});

test('an unknown transition closes a stale confirm while the user is busy', async () => {
  const env = makeConversationPageEnv({ kimiRunning: true });
  await env.page.refresh();
  const row = env.ui.list.children[0];
  await findNode(row, node => node.dataset?.convSwitch === 'recent').fire('click');
  const confirmHost = findNode(row, node => node.dataset?.convConfirm === 'recent');
  assert.equal(confirmHost.hidden, false);
  env.page.onState({ revision: 'rev-1', kimiProcess: { running: true, state: 'checked' } });
  env.page.onState({ revision: 'rev-2', kimiProcess: { running: true, state: 'unknown' } });
  assert.match(env.ui.status.textContent, /无法确认 Kimi 的运行状态/);
  assert.equal(confirmHost.hidden, true, 'a confirm that can no longer succeed must close');
  assert.equal(env.state.conversations.rowConfirm, null);
  assert.equal(env.calls.filter(call => call[0] === '/api/work/conversations').length, 1, 'no reload while the user is busy');
});

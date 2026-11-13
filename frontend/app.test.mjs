import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

function sourceFunction(name) {
  const start = source.indexOf('\nfunction ' + name + '(');
  assert.notEqual(start, -1, 'missing source function: ' + name);
  const end = source.indexOf('\n}', start);
  assert.notEqual(end, -1, 'unterminated source function: ' + name);
  return source.slice(start, end + 2);
}

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

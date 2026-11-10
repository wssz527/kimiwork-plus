// Runs the panel-action fixtures through the LEGACY panel controller so the Go
// `PanelAction` can be compared with the exact behaviour the existing UI got.
//
//   bun run internal/quota/testdata/panel-action-legacy-run.mjs <cases.json> <out.json>
//
// Each case is one call to the controller method the route maps to, with the
// same body the UI sends. Only the outcome is recorded: the HTTP status the
// panel would answer with, the error code, and — on success — the four fields
// the UI reads plus the source list.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const LEGACY = path.resolve(import.meta.dir, '../../../../../work/kimi-work-gateway');

const { QuotaAccounts } = await import(path.join(LEGACY, 'src/quota-accounts.mjs'));
const { PanelController } = await import(path.join(LEGACY, 'src/panel-controller.mjs'));

const KEYS = {
  'deepseek:default': ['sk', 'deepseek', 'test', 'a1b2c3d4'].join('-'),
  'moonshot:default': ['sk', 'moonshot', 'test', '5e6f7a8b'].join('-'),
  custom: ['gw', 'relay', 'test', '9c0d1e2f'].join('-'),
};

const document = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-panel-action-'));
const observations = [];

for (const testCase of document.cases) {
  let clock = 1794196800000;
  const secrets = Object.values(KEYS);
  const connections = testCase.connections ?? [];

  // A source borrowing a connection resolves that connection's credential.
  const resolve = ref => {
    if (typeof ref === 'string' && ref.startsWith('connection:')) {
      const id = ref.slice('connection:'.length);
      const connection = connections.find(row => row.id === id);
      return connection?.hasCredential ? KEYS.custom : null;
    }
    return KEYS[ref] ?? null;
  };
  let current = testCase.reply ?? { status: 200, body: {} };
  const fetchImpl = async () => {
    const bytes = new TextEncoder().encode(JSON.stringify(current.body ?? {}));
    const status = current.status ?? 200;
    return {
      ok: status >= 200 && status < 300, status,
      headers: { get: name => (name.toLowerCase() === 'content-length' ? String(bytes.length) : null) },
      async text() { return new TextDecoder().decode(bytes); },
      body: { getReader() { let sent = false; return {
        async read() { if (sent) return { done: true }; sent = true; return { done: false, value: bytes }; },
        async cancel() {} }; } },
    };
  };
  const accounts = new QuotaAccounts({ stateDir: null, credentials: { resolve }, fetchImpl,
    now: () => clock, freshnessMs: 15 * 60 * 1000, throttleMs: 0 });

  const config = { connections: connections.map(connection => ({
    id: connection.id, name: connection.name,
    ...(connection.providerPresetId ? { providerPresetId: connection.providerPresetId } : {}),
    apiKeyEnv: 'KIMI_WORK_' + connection.id.toUpperCase() + '_KEY',
  })), models: [] };
  const env = {};
  for (const connection of connections) {
    if (connection.hasCredential) env['KIMI_WORK_' + connection.id.toUpperCase() + '_KEY'] = KEYS.custom;
  }
  const controller = new PanelController({ configFile: path.join(scratch, 'gateway.json'), config,
    gateway: { metrics: { recent: [] } }, backend: { models: [] },
    stateDir: path.join(scratch, 'state'), kimiDir: path.join(scratch, 'kimi'),
    quotaAccounts: accounts, env,
    policy: { mode: 'inherit' }, runtime: { inspect: () => ({}) },
    translationRuntime: { configure() {}, inspect: () => ({ state: 'disabled' }) },
    updater: { inspect: () => ({}) }, webTools: null, contextPolicy: null,
    mcpManager: null, servicesPolicy: null });

  const observation = { label: testCase.label, path: testCase.path };
  try {
    for (const source of testCase.setup?.sources ?? []) accounts.addSource(source);
    let result;
    if (testCase.path === '/api/quota/refresh') result = await controller.refreshQuotas(testCase.input);
    else if (testCase.path === '/api/quota/sources') result = controller.configureQuotaSource(testCase.input);
    else result = controller.quotaState();
    let text = JSON.stringify({
      sources: result.sources.map(source => ({
        sourceId: source.sourceId, adapter: source.adapter, kind: source.kind,
        unit: source.unit, credentialRef: source.credentialRef,
        status: source.status, credentialSet: source.credentialSet,
        removable: source.removable, officialUrl: source.officialUrl,
        endpoint: source.endpoint ?? null, method: source.method ?? null,
        windowLabel: source.windowLabel ?? null,
        fields: source.fields ?? null, auth: source.auth ?? null,
      })),
      records: result.records.map(record => ({
        sourceId: record.sourceId, remaining: record.remaining, limit: record.limit,
        used: record.used, kind: record.kind, unit: record.unit,
      })),
      combining: result.combining,
      lastRefresh: result.lastRefresh ? {
        results: result.lastRefresh.results.map(entry => ({
          sourceId: entry.sourceId, status: entry.status,
          errorReason: entry.errorReason ?? null,
        })),
      } : null,
    });
    for (const secret of secrets) text = text.split(secret).join('<credential>');
    observation.ok = JSON.parse(text);
  } catch (error) {
    observation.status = error?.status ?? null;
    observation.code = error?.message ?? String(error);
  }
  observations.push(observation);
}

fs.rmSync(scratch, { recursive: true, force: true });
fs.writeFileSync(process.argv[3], JSON.stringify({ schemaVersion: 1,
  implementation: 'legacy-0.12.1-panel-action', cases: observations }, null, 2) + '\n');
process.stdout.write('legacy panel-action observations: ' + observations.length + '\n');

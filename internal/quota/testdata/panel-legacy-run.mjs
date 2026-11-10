// Runs the shared quota scenarios through the LEGACY panel decoration
// (`PanelController.quotaState`) so the Go `PanelState` can be compared with it.
// Only the decoration is exercised: the controller is built with the smallest
// possible config and a real QuotaAccounts over a scripted transport.
//
//   bun run internal/quota/testdata/panel-legacy-run.mjs <cases.json> <out.json>

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
// The controller resolves a few paths at construction time; a scratch directory
// keeps this run away from any real Kimi or Work state.
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-panel-dual-'));
const observations = [];

for (const scenario of document.scenarios) {
  let clock = 1794196800000;
  const secrets = Object.values(KEYS);
  // A source borrowing a connection resolves that connection's credential,
  // which is how the real caller wires it.
  const resolve = ref => {
    if (typeof ref === 'string' && ref.startsWith('connection:')) {
      const id = ref.slice('connection:'.length);
      const connection = (scenario.connections ?? []).find(row => row.id === id);
      return connection?.hasCredential ? KEYS.custom : null;
    }
    return KEYS[ref] ?? null;
  };
  let current = { status: 200, body: {} };
  const fetchImpl = async (url, init = {}) => {
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
    now: () => clock, freshnessMs: scenario.freshnessMs ?? 15 * 60 * 1000, throttleMs: 0 });

  // The panel reads its connections from the config; the fixture declares them
  // so `credentialSet` for a borrowed connection is exercised for real.
  const config = { connections: (scenario.connections ?? []).map(connection => ({
    id: connection.id, name: connection.name,
    ...(connection.providerPresetId ? { providerPresetId: connection.providerPresetId } : {}),
    apiKeyEnv: connection.apiKeyEnv ?? ('KIMI_WORK_' + connection.id.toUpperCase() + '_KEY'),
  })), models: [] };
  const env = {};
  for (const connection of scenario.connections ?? []) {
    if (connection.hasCredential) env[config.connections.find(row => row.id === connection.id).apiKeyEnv] = KEYS.custom;
  }

  const controller = new PanelController({ configFile: path.join(scratch, 'gateway.json'), config,
    gateway: { metrics: { recent: [] } }, backend: { models: [] }, stateDir: path.join(scratch, 'state'),
    kimiDir: path.join(scratch, 'kimi'), quotaAccounts: accounts, env,
    policy: { mode: 'inherit' }, runtime: { inspect: () => ({}) },
    translationRuntime: { configure() {}, inspect: () => ({ state: 'disabled' }) },
    updater: { inspect: () => ({}) }, webTools: null, contextPolicy: null,
    mcpManager: null, servicesPolicy: null });

  const steps = [];
  try {
    accounts.addSource(scenario.source);
    for (const extra of scenario.extraSources ?? []) accounts.addSource(extra);
    for (const step of scenario.steps) {
      clock += step.advanceMs ?? 0;
      if (step.kind === 'refresh') {
        current = step.reply;
        await accounts.refresh();
      }
      const panel = controller.quotaState();
      let text = JSON.stringify({ records: panel.records, sources: panel.sources,
        connections: panel.connections, combining: panel.combining });
      for (const secret of secrets) text = text.split(secret).join('<credential>');
      steps.push(JSON.parse(text));
    }
  } catch (error) {
    steps.push({ error: String(error?.message ?? error) });
  }
  observations.push({ name: scenario.name, steps });
}

fs.rmSync(scratch, { recursive: true, force: true });
fs.writeFileSync(process.argv[3], JSON.stringify({ schemaVersion: 1,
  implementation: 'legacy-0.12.1-panel', scenarios: observations }, null, 2) + '\n');
process.stdout.write('legacy panel observations: ' + observations.length + '\n');

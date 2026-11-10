// Runs the shared quota fixtures through the LEGACY 0.12.1 quota manager and
// writes the observations the Go implementation must reproduce.
//
//   bun run internal/quota/testdata/legacy-run.mjs <cases.json> <out.json>
//
// Every transport is a recorder: no request leaves this process, and the only
// credentials are fakes that must never reach the output file.

import fs from 'node:fs';
import path from 'node:path';

const LEGACY = path.resolve(import.meta.dir, '../../../../../work/kimi-work-gateway');

const {
  QuotaAccounts, accountTag, checkEndpoint, toAmount, toTimestamp,
} = await import(path.join(LEGACY, 'src/quota-accounts.mjs'));

const KEYS = {
  'deepseek:default': ['sk', 'deepseek', 'test', 'a1b2c3d4'].join('-'),
  'moonshot:default': ['sk', 'moonshot', 'test', '5e6f7a8b'].join('-'),
  custom: ['gw', 'relay', 'test', '9c0d1e2f'].join('-'),
};

// The observation never carries a credential, so the fixture's fake keys are
// replaced by a marker before anything is written.
function scrub(value, secrets) {
  let text = JSON.stringify(value);
  for (const secret of secrets) text = text.split(secret).join('<credential>');
  return JSON.parse(text);
}

function recorder(reply) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method, redirect: init.redirect,
      headers: Object.keys(init.headers ?? {}), body: init.body ?? null });
    const bytes = new TextEncoder().encode(JSON.stringify(reply.body ?? {}));
    return {
      ok: (reply.status ?? 200) >= 200 && (reply.status ?? 200) < 300,
      status: reply.status ?? 200,
      headers: { get: name => (name.toLowerCase() === 'content-length' ? String(bytes.length) : null) },
      async text() { return new TextDecoder().decode(bytes); },
      body: { getReader() {
        let sent = false;
        return { async read() { if (sent) return { done: true }; sent = true; return { done: false, value: bytes }; },
          async cancel() {} };
      } },
    };
  };
  return { calls, fetchImpl };
}

let clock = 1794196800000;
const now = () => clock;

const scenarios = [];
const document = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));

for (const scenario of document.scenarios) {
  clock = 1794196800000;
  const observations = { name: scenario.name, steps: [] };
  const secrets = [];
  const resolve = ref => {
    const value = KEYS[ref] ?? null;
    if (value) secrets.push(value);
    return value;
  };
  // A per-step transport: the reply is chosen by the step, so one scenario can
  // succeed and then fail without a second manager.
  let current = { status: 200, body: {} };
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method, redirect: init.redirect });
    return recorder(current).fetchImpl(url, init);
  };
  const accounts = new QuotaAccounts({
    stateDir: null, credentials: { resolve }, fetchImpl, now,
    freshnessMs: scenario.freshnessMs ?? 15 * 60 * 1000,
    throttleMs: 0,
  });
  try {
    accounts.addSource(scenario.source);
    for (const extra of scenario.extraSources ?? []) accounts.addSource(extra);
    for (const step of scenario.steps) {
      clock += step.advanceMs ?? 0;
      const stepObservation = { kind: step.kind };
      if (step.kind === 'refresh') {
        current = step.reply;
        stepObservation.report = await accounts.refresh();
        stepObservation.calls = calls.splice(0, calls.length);
      }
      const state = accounts.state();
      stepObservation.state = scrub({
        combining: state.combining,
        refreshedAt: state.refreshedAt,
        freshnessMs: state.freshnessMs,
        cards: state.cards.map(card => ({
          sourceId: card.sourceId, providerId: card.providerId, sourceName: card.sourceName,
          kind: card.kind, supported: card.supported, status: card.status, stale: card.stale,
          staleReason: card.staleReason, accountRefMasked: card.accountRefMasked,
          billingIdentity: card.billingIdentity, observedAt: card.observedAt,
          lastSuccessfulAt: card.lastSuccessfulAt, lastAttemptAt: card.lastAttemptAt,
          errorReason: card.errorReason, available: card.available, parts: card.parts,
          credentialRef: card.credentialRef, officialPage: card.officialPage,
          consoleUrl: card.consoleUrl,
          records: card.records.map(record => ({
            sourceId: record.sourceId, kind: record.kind, windowLabel: record.windowLabel,
            unit: record.unit, limit: record.limit, used: record.used, remaining: record.remaining,
            periodStart: record.periodStart, resetAt: record.resetAt, observedAt: record.observedAt,
            lastSuccessfulAt: record.lastSuccessfulAt, stale: record.stale, status: record.status,
            errorReason: record.errorReason,
          })),
        })),
        records: state.records.map(record => ({
          sourceId: record.sourceId, kind: record.kind, windowLabel: record.windowLabel,
          unit: record.unit, limit: record.limit, used: record.used, remaining: record.remaining,
          periodStart: record.periodStart, resetAt: record.resetAt, observedAt: record.observedAt,
          lastSuccessfulAt: record.lastSuccessfulAt, stale: record.stale, status: record.status,
          errorReason: record.errorReason,
        })),
        sources: state.sources.map(source => ({
          sourceId: source.sourceId, adapter: source.adapter, providerId: source.providerId,
          sourceName: source.sourceName, kind: source.kind, unit: source.unit,
          supported: source.supported, credentialRef: source.credentialRef,
          accountLabel: source.accountLabel,
          ...(source.adapter === 'custom' ? {
            method: source.method, endpoint: source.endpoint, host: source.host,
            fields: source.fields, auth: source.auth, windowLabel: source.windowLabel,
          } : {}),
          officialPage: source.officialPage, consoleUrl: source.consoleUrl,
        })),
      }, secrets);
      observations.steps.push(stepObservation);
    }
  } catch (error) {
    observations.error = String(error?.message ?? error);
  }
  scenarios.push(observations);
}

const checks = {};
for (const check of document.checks) {
  if (check.name === 'amount-parsing') {
    checks[check.name] = check.inputs.map(value => toAmount(value));
  } else if (check.name === 'timestamp-parsing') {
    checks[check.name] = check.inputs.map(value => toTimestamp(value));
  } else if (check.name === 'endpoint-gate') {
    checks[check.name] = check.inputs.map(value => {
      const result = checkEndpoint(value);
      return { ok: result.ok === true, code: result.ok ? null : result.code };
    });
  }
}
checks.accountTag = accountTag(KEYS['deepseek:default']);

fs.writeFileSync(process.argv[3], JSON.stringify({ schemaVersion: 1,
  implementation: 'legacy-0.12.1', scenarios, checks }, null, 2) + '\n');
process.stdout.write('legacy quota observations: ' + scenarios.length + '\n');

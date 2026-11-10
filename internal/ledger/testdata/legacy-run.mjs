// Runs the shared usage-ledger fixtures through the LEGACY 0.12.1 ledger and
// writes the observations the Go implementation must reproduce.
//
//   bun run internal/ledger/testdata/legacy-run.mjs <cases.json> <out.json>
//
// The observation is the published query payload with the account digest
// resolved, so the Go side is compared on what a UI actually reads rather than
// on internal field names.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const LEGACY = path.resolve(import.meta.dir, '../../../../../work/kimi-work-gateway');

const { UsageLedger, redactIdentifier } = await import(path.join(LEGACY, 'src/usage-ledger.mjs'));

const [, , casesFile, outFile] = process.argv;
const document = JSON.parse(fs.readFileSync(casesFile, 'utf8'));
const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'legacy-ledger-dual-'));
const file = path.join(directory, 'usage', 'ledger.jsonl');

const ledger = new UsageLedger({ file });
for (const entry of document.entries) {
  const result = ledger.record(entry);
  if (!result.ok) throw new Error('legacy refused ' + entry.operationId + ': ' + result.reason);
}

// The fixture names the digest so a query can filter on the redacted value
// without this script ever holding the raw account string.
const digest = redactIdentifier('synthetic-account-label');

function resolve(value) {
  if (typeof value === 'string') return value.replaceAll('__REDACTED_ACCOUNT__', digest);
  if (Array.isArray(value)) return value.map(resolve);
  if (value && typeof value === 'object') {
    const out = {};
    for (const [key, entry] of Object.entries(value)) out[key] = resolve(entry);
    return out;
  }
  return value;
}

const observations = [];
for (const testCase of document.queries) {
  const observation = { label: testCase.label };
  try {
    observation.result = ledger.query(resolve(testCase.query));
  } catch (error) {
    observation.error = String(error?.message ?? error);
  }
  observations.push(observation);
}
const errorObservations = [];
for (const testCase of document.errors) {
  const observation = { label: testCase.label };
  try {
    observation.result = ledger.query(resolve(testCase.query));
  } catch (error) {
    observation.error = String(error?.message ?? error);
  }
  errorObservations.push(observation);
}

const status = ledger.status();
const exported = JSON.parse(ledger.exportJSON({ preset: '90d', timeZone: 'Asia/Shanghai',
  now: 1794196800000 }));

fs.writeFileSync(outFile, JSON.stringify({ schemaVersion: 1, implementation: 'legacy-0.12.1',
  digest, status: { state: status.state, records: status.records, skippedLines: status.skippedLines,
    schemaVersion: status.schemaVersion },
  queries: observations, errors: errorObservations,
  export: { range: exported.range, summary: exported.summary, coverage: exported.coverage,
    recordCount: exported.records.length } }, null, 2) + '\n');

fs.rmSync(directory, { recursive: true, force: true });
process.stdout.write('legacy ledger observations: ' + observations.length + '\n');

// Runs the shared web-adapter fixtures through the LEGACY 0.12.1 adapter so the
// Go implementation can be compared with it. Every vendor answer is a local
// mock; nothing here reaches Tavily.
//
//   bun run internal/webtools/testdata/legacy-run.mjs <cases.json> <out.json>

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

const LEGACY = path.resolve(import.meta.dir, '../../../../../work/kimi-work-gateway');
const { adaptWebSearch, adaptWebFetch } = await import(path.join(LEGACY, 'src/tavily-adapter.mjs'));
const { normalizeServiceURL } = await import(path.join(LEGACY, 'src/native-services-config.mjs'));

const KEY = ['tvly', 'test', 'key', '0001'].join('-');

// One loopback vendor whose answer is chosen per case.
function vendor(status, body) {
  const server = http.createServer((req, res) => {
    res.writeHead(status ?? 200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body ?? {}));
  });
  return new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve({
    url: 'http://127.0.0.1:' + server.address().port,
    close: () => new Promise(done => server.close(done)),
  })));
}

const document = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const out = { schemaVersion: 1, implementation: 'legacy-0.12.1', search: [], searchErrors: [],
  fetch: [], urlGate: [], requests: [] };

for (const testCase of document.search) {
  const server = await vendor(200, testCase.body);
  const entry = { label: testCase.label };
  try { entry.answer = await adaptWebSearch({ text_query: 'how does it work' }, { apiKey: KEY, endpoint: server.url, fetchImpl: fetch }); }
  catch (error) { entry.code = error.code; entry.status = error.status; }
  await server.close();
  out.search.push(entry);
}

for (const testCase of document.searchErrors) {
  const server = await vendor(testCase.status, { detail: 'vendor said no' });
  const entry = { label: testCase.label };
  try { await adaptWebSearch({ text_query: 'x' }, { apiKey: KEY, endpoint: server.url, fetchImpl: fetch }); }
  catch (error) { entry.code = error.code; entry.status = error.status; entry.vendorStatus = error.vendorStatus; }
  await server.close();
  out.searchErrors.push(entry);
}

for (const testCase of document.fetch) {
  const server = await vendor(200, testCase.body);
  const entry = { label: testCase.label };
  try { entry.page = await adaptWebFetch({ url: testCase.url }, { apiKey: KEY, endpoint: server.url, fetchImpl: fetch }); }
  catch (error) { entry.code = error.code; entry.status = error.status; }
  await server.close();
  out.fetch.push(entry);
}

for (const raw of document.urlGate) {
  out.urlGate.push({ input: raw, accepted: normalizeServiceURL(raw) !== null });
}

for (const testCase of document.requests) {
  const entry = { label: testCase.label };
  const options = { apiKey: KEY, endpoint: 'http://127.0.0.1:1',
    ...(testCase.searchDepth ? { searchDepth: testCase.searchDepth } : {}),
    fetchImpl: async () => { throw new Error('should not be called'); } };
  try {
    if (testCase.kind === 'search') await adaptWebSearch(testCase.request, options);
    else await adaptWebFetch(testCase.request, options);
    entry.accepted = true;
  } catch (error) { entry.code = error.code; entry.status = error.status; }
  out.requests.push(entry);
}

fs.writeFileSync(process.argv[3], JSON.stringify(out, null, 2) + '\n');
process.stdout.write('legacy webtools observations written\n');

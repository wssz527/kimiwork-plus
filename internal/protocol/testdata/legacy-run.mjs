// Runs the shared gateway fixtures through the LEGACY 0.12.1 implementation and
// writes one observation per case. Nothing here reaches a real vendor: every
// upstream is a local loopback server built from the fixture.
//
//   bun run internal/protocol/testdata/legacy-run.mjs <cases.json> <out.json>
//
// The observation is deliberately the smallest set of facts that the Go
// implementation must reproduce: the client-visible status, the body the client
// received, and what the upstream was actually asked. A difference in any of
// them is a real behavioural difference, not a formatting one.

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

// testdata -> protocol -> internal -> go-wails -> migration -> worktree, then the legacy tree.
const LEGACY = path.resolve(import.meta.dir, '../../../../../work/kimi-work-gateway');

const { KimiWorkGateway, validateModels } = await import(path.join(LEGACY, 'src/gateway.mjs'));
const { OpenAIBackend } = await import(path.join(LEGACY, 'src/openai-backend.mjs'));

const GATEWAY_KEY = 'synthetic-dualrun-key';

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject);
      resolve('http://127.0.0.1:' + server.address().port);
    });
  });
}

function close(server) {
  return new Promise(resolve => {
    server.closeAllConnections?.();
    server.close(resolve);
  });
}

// One upstream that answers exactly the fixture's reply and records the request.
async function upstream(reply) {
  const requests = [];
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => chunks.push(chunk));
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8');
      requests.push({ url: req.url, method: req.method, headers: { ...req.headers }, body });
      res.writeHead(reply.status ?? 200, { 'Content-Type': reply.contentType ?? 'application/json' });
      res.end(reply.body ?? '');
    });
  });
  const baseURL = await listen(server);
  return { baseURL, requests, close: () => close(server) };
}

function normalizeBody(text) {
  if (!text) return '';
  // An SSE body is compared as an ordered list of `data:` payloads so a
  // difference in frame boundaries is not mistaken for a difference in events.
  if (text.includes('data:')) {
    // Each frame is compared as the JSON value it carries, so a difference in
    // key order is not mistaken for a difference in the event. `[DONE]` is the
    // one payload that is not JSON and stays the literal string.
    return text.split(/\r\n|\r|\n/)
      .filter(line => line.startsWith('data:'))
      .map(line => line.slice(5).trim())
      .filter(Boolean)
      .map(payload => {
        if (payload === '[DONE]') return payload;
        try { return JSON.parse(payload); } catch { return payload; }
      });
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

async function runCase(testCase) {
  const server = await upstream(testCase.upstream);
  const backend = new OpenAIBackend({
    baseURL: server.baseURL,
    apiKey: 'synthetic-vendor-key',
    models: validateModels(testCase.models),
  });
  const gateway = new KimiWorkGateway({ backend, recordRequests: true });
  const observation = { name: testCase.name };
  try {
    const base = await gateway.configure({ key: GATEWAY_KEY, port: 0 });
    const response = await fetch(base + testCase.route.replace('/v1', ''), {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + GATEWAY_KEY, 'Content-Type': 'application/json' },
      body: JSON.stringify(testCase.request),
    });
    const text = await response.text();
    observation.status = response.status;
    observation.body = normalizeBody(text);
    observation.contentType = response.headers.get('content-type');
    observation.upstream = server.requests.map(request => ({
      url: request.url,
      method: request.method,
      // The credential itself is never written to the evidence file.
      authorization: request.headers.authorization ? 'present' : 'absent',
      apiKey: request.headers['x-api-key'] ? 'present' : 'absent',
      anthropicVersion: request.headers['anthropic-version'] ?? null,
      body: normalizeBody(request.body),
    }));
    const record = gateway.metrics.recent[0] ?? null;
    observation.record = record === null ? null : {
      status: record.status,
      outcome: record.outcome ?? null,
      error: record.error ?? null,
      side: record.side ?? null,
      upstreamProtocol: record.upstreamProtocol ?? null,
      model: record.model ?? null,
      usage: record.usage ?? null,
      failureReason: record.failure?.reason ?? null,
      failureSide: record.failure?.side ?? null,
      upstreamType: record.failure?.upstreamType ?? null,
    };
  } catch (error) {
    observation.error = String(error?.message ?? error);
  } finally {
    await gateway.stop();
    await server.close();
  }
  return observation;
}

const [, , casesFile, outFile] = process.argv;
const document = JSON.parse(fs.readFileSync(casesFile, 'utf8'));
const observations = [];
for (const testCase of document.cases) observations.push(await runCase(testCase));
fs.writeFileSync(outFile, JSON.stringify({ schemaVersion: 1, implementation: 'legacy-0.12.1',
  cases: observations }, null, 2) + '\n');
process.stdout.write('legacy observations: ' + observations.length + '\n');

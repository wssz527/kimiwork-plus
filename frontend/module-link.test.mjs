import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import test from 'node:test';
import { SourceTextModule } from 'node:vm';

const root = new URL('./', import.meta.url);
const modules = new Map();

function load(path) {
  const url = new URL(path, root).href;
  if (!modules.has(url)) {
    modules.set(url, new SourceTextModule(readFileSync(new URL(url), 'utf8'), { identifier: url }));
  }
  return modules.get(url);
}

test('the production frontend links as real ES modules without a DOM', async () => {
  const files = readdirSync(root).filter(file => file.endsWith('.js'));
  assert.ok(files.includes('app.js') && files.includes('app-connections.js'));
  for (const file of files) {
    modules.clear();
    const entry = load(file);
    await entry.link((specifier, referencing) => load(new URL(specifier, referencing.identifier).href));
    assert.equal(entry.status, 'linked', entry.identifier);
  }
});

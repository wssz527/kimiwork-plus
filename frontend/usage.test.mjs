import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./usage.js', import.meta.url), 'utf8');
const start = source.indexOf('  function quotaPathProblem(value, required = false) {');
const end = source.indexOf('\n  }', start);
assert.notEqual(start, -1, 'missing quotaPathProblem in current source');
assert.notEqual(end, -1, 'unterminated quotaPathProblem in current source');
const quotaPathProblem = new Function(source.slice(start, end + 4) + '\nreturn quotaPathProblem;')();

test('quota paths accept documented balance fields and array indexes', () => {
  assert.equal(quotaPathProblem('data.balance', true), '');
  assert.equal(quotaPathProblem('balance_infos.0.total_balance', true), '');
  assert.equal(quotaPathProblem('a.b.c.d.e.f.g.h', true), '');
});

test('optional quota paths may be empty while the value path is required', () => {
  assert.equal(quotaPathProblem(''), '');
  assert.equal(quotaPathProblem('   '), '');
  assert.match(quotaPathProblem('', true), /请填写/);
  assert.match(quotaPathProblem('   ', true), /请填写/);
});

test('quota paths reject empty segments and invalid field syntax', () => {
  assert.notEqual(quotaPathProblem('data..balance'), '');
  assert.notEqual(quotaPathProblem('.data.balance'), '');
  assert.notEqual(quotaPathProblem('data.balance.'), '');
  assert.notEqual(quotaPathProblem('balance_infos.0000.total_balance'), '');
});

test('quota paths reject prototype properties', () => {
  for (const field of ['__proto__', 'prototype', 'constructor']) {
    assert.match(quotaPathProblem(`data.${field}.balance`), /保留字段/);
  }
});

test('quota paths enforce the backend depth and length limits', () => {
  assert.match(quotaPathProblem('a.b.c.d.e.f.g.h.i'), /8 层/);
  assert.match(quotaPathProblem('a'.repeat(201)), /200 个字符/);
  const maximum = ['a'.repeat(64), 'b'.repeat(64), 'c'.repeat(64), 'd'.repeat(5)].join('.');
  assert.equal(maximum.length, 200);
  assert.equal(quotaPathProblem(maximum), '');
});

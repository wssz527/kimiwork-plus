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

/* renderChart 需要真实 DOM 才能跑；这里只做一个够用的替身，用它验证
   图表在单点、多点与缺失值下的几何行为，以及 viewBox 是否随数据自适应。 */
function chartEnv() {
  const created = [];
  const makeElement = (ns, tag) => {
    const element = {
      tag, attrs: {}, children: [], textContent: '', className: '',
      setAttribute(name, value) { this.attrs[name] = String(value); },
      getAttribute(name) { return this.attrs[name]; },
      append(...nodes) { this.children.push(...nodes); },
      replaceChildren(...nodes) { this.children = nodes; },
      addEventListener() {},
    };
    created.push(element);
    return element;
  };
  const roots = { 'usage-chart-body': makeElement('html', 'div') };
  const document = {
    getElementById: id => roots[id] || null,
    createElement: tag => makeElement('html', tag),
    createElementNS: (_ns, tag) => makeElement('svg', tag),
  };
  const fnStart = source.indexOf('  function chartGeometry(buckets) {');
  const fnEnd = source.indexOf('\n  function selectBucket(bucket) {');
  assert.notEqual(fnStart, -1, 'missing chartGeometry in current source');
  assert.notEqual(fnEnd, -1, 'missing renderChart in current source');
  const code = source.slice(fnStart, fnEnd) + '\n  return { renderChart };';
  const chart = new Function('document', '$', 'number', 'token', 'fmt', 'bucketLabel', 'selectBucket', 'Intl', 'Number', 'Math', code)(
    document,
    id => roots[id] || null,
    value => Number.isSafeInteger(value) && value >= 0 ? value : null,
    (row, name) => {
      const value = row?.[name + 'Tokens'] ?? row?.[name];
      return Number.isSafeInteger(value) && value >= 0 ? value : null;
    },
    value => Number.isSafeInteger(value) ? new Intl.NumberFormat('zh-CN').format(value) : '未提供',
    bucket => bucket.label,
    () => {},
    Intl, Number, Math,
  );
  return { renderChart: chart.renderChart, roots, created };
}

const svgOf = (env) => env.roots['usage-chart-body'].children[0];

test('a single bucket still shows both token series', () => {
  const env = chartEnv();
  env.renderChart([{ label: '10:00', requests: 1, input: 120, output: 40 }]);
  const svg = svgOf(env);
  const points = svg.children.filter(node => node.tag === 'circle');
  const classOf = node => node.attrs.class || '';
  assert.equal(points.filter(node => classOf(node) === 'usage-chart__point').length, 1);
  assert.equal(points.filter(node => classOf(node).includes('usage-chart__point--output')).length, 1);
  // 单点没有相邻点可连，但两个系列各自都要有可见标记。
  assert.ok(svg.children.some(node => classOf(node).includes('usage-chart__tick') && !classOf(node).includes('--output')));
});

test('a missing value breaks the line instead of reading as zero', () => {
  const env = chartEnv();
  env.renderChart([
    { label: 'a', requests: 1, input: 100, output: 10 },
    { label: 'b', requests: 1, output: 20 },
    { label: 'c', requests: 1, input: 60, output: 30 },
  ]);
  const svg = svgOf(env);
  const classOf = node => node.attrs.class || '';
  /* 空缺两侧各剩一个孤立点，因此是两段单点标记而不是一条贯穿的线：
     把缺失值当 0 连起来会是两条 path。 */
  const inputMarks = svg.children.filter(node => node.tag === 'line'
    && classOf(node) === 'usage-chart__line usage-chart__tick');
  assert.equal(inputMarks.length, 2, 'the gap must split the input series in two');
  const inputPaths = svg.children.filter(node => node.tag === 'path' && classOf(node) === 'usage-chart__line');
  assert.equal(inputPaths.length, 0, 'a missing bucket must not be bridged by a line');
  for (const mark of inputMarks) {
    assert.ok(Number(mark.attrs.y1) <= 184, 'a missing bucket must not be plotted at zero height');
  }
});

test('the chart width follows the number of buckets instead of one fixed viewBox', () => {
  const narrow = chartEnv();
  narrow.renderChart([{ label: 'a', requests: 1, input: 10, output: 5 }]);
  const wide = chartEnv();
  wide.renderChart(Array.from({ length: 24 }, (_, index) => ({ label: String(index), requests: 1, input: 10, output: 5 })));
  const widthOf = (env) => Number(svgOf(env).attrs.viewBox.split(' ')[2]);
  assert.ok(widthOf(wide) > widthOf(narrow), 'more buckets must get more width');
  assert.equal(svgOf(narrow).attrs.preserveAspectRatio, 'xMidYMid meet');
});

test('every point of both series is keyboard reachable', () => {
  const env = chartEnv();
  env.renderChart([{ label: 'a', requests: 1, input: 10, output: 5 }, { label: 'b', requests: 1, input: 20, output: 8 }]);
  const points = svgOf(env).children.filter(node => node.tag === 'circle');
  assert.equal(points.length, 4);
  for (const point of points) {
    assert.equal(point.attrs.tabindex, '0');
    assert.equal(point.attrs.role, 'button');
    assert.match(point.attrs['aria-label'], /输入|输出/);
  }
});

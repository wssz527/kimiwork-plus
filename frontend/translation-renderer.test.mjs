import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const rendererSource = readFileSync(new URL('../assets/injection/translation-renderer.v1.js', import.meta.url), 'utf8');

function matchesCompoundSelector(el, selector) {
  if (!el || el.nodeType !== 1) return false;
  const trimmed = selector.trim();
  if (!trimmed) return false;
  if (trimmed.startsWith('[')) {
    const attrMatch = /^\[([A-Za-z0-9_-]+)(?:="([^"]*)")?\]$/.exec(trimmed);
    if (!attrMatch) return false;
    const val = el.getAttribute(attrMatch[1]);
    return attrMatch[2] === undefined ? val !== null : val === attrMatch[2];
  }
  const tokens = trimmed.match(/(\.[A-Za-z0-9_-]+|#[A-Za-z0-9_-]+|\[[^\]]+\]|[a-zA-Z][a-zA-Z0-9-]*)/g);
  if (!tokens) return false;
  for (const token of tokens) {
    if (token.startsWith('.')) {
      if (!el.classList.contains(token.slice(1))) return false;
    } else if (token.startsWith('#')) {
      if (el.id !== token.slice(1)) return false;
    } else if (token.startsWith('[')) {
      if (!matchesCompoundSelector(el, token)) return false;
    } else if (el.tagName.toLowerCase() !== token.toLowerCase()) {
      return false;
    }
  }
  return true;
}

function matchesSimpleSelector(el, selector) {
  const parts = selector.trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return false;
  if (!matchesCompoundSelector(el, parts[parts.length - 1])) return false;
  let cur = el.parentElement;
  for (let i = parts.length - 2; i >= 0; i -= 1) {
    while (cur && !matchesCompoundSelector(cur, parts[i])) {
      cur = cur.parentElement;
    }
    if (!cur) return false;
    cur = cur.parentElement;
  }
  return true;
}

function createFakeDom() {
  const observers = [];

  class FakeClassList {
    constructor(owner) {
      this._owner = owner;
      this._set = new Set();
    }
    _syncFromString(str) {
      this._set = new Set(String(str || '').split(/\s+/).filter(Boolean));
    }
    _toString() {
      return [...this._set].join(' ');
    }
    add(...names) {
      for (const n of names) if (n) this._set.add(n);
    }
    remove(...names) {
      for (const n of names) this._set.delete(n);
    }
    contains(name) {
      return this._set.has(name);
    }
    toggle(name, force) {
      const next = force === undefined ? !this._set.has(name) : Boolean(force);
      if (next) this._set.add(name);
      else this._set.delete(name);
      return next;
    }
  }

  class FakeElement {
    constructor(tagName) {
      this.nodeType = 1;
      this.tagName = String(tagName || 'div').toUpperCase();
      this.id = '';
      this.parentNode = null;
      this.childNodes = [];
      this.attributes = new Map();
      this.dataset = {};
      this.listeners = new Map();
      this.classList = new FakeClassList(this);
      this.hidden = false;
      this.type = '';
      this.title = '';
      this._textContent = '';
    }
    get className() {
      return this.classList._toString();
    }
    set className(value) {
      this.classList._syncFromString(value);
    }
    get parentElement() {
      return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null;
    }
    get isConnected() {
      let cur = this;
      while (cur) {
        if (cur === doc.body || cur === doc.head || cur === doc.documentElement) return true;
        cur = cur.parentNode;
      }
      return false;
    }
    get textContent() {
      if (this.childNodes.length === 0) return this._textContent;
      return this.childNodes.map((child) => child.textContent).join('');
    }
    set textContent(value) {
      this.childNodes = [];
      this._textContent = String(value ?? '');
    }
    get innerText() {
      return this.textContent;
    }
    set innerText(value) {
      this.textContent = value;
    }
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    }
    getAttribute(name) {
      return this.attributes.has(name) ? this.attributes.get(name) : null;
    }
    removeAttribute(name) {
      this.attributes.delete(name);
    }
    appendChild(child) {
      if (!child) return child;
      if (child.parentNode) child.parentNode.removeChild(child);
      child.parentNode = this;
      this.childNodes.push(child);
      return child;
    }
    append(...nodes) {
      for (const node of nodes) if (node) this.appendChild(node);
    }
    insertBefore(node, ref) {
      if (!node) return node;
      if (node.parentNode) node.parentNode.removeChild(node);
      const index = ref ? this.childNodes.indexOf(ref) : -1;
      node.parentNode = this;
      if (index < 0) this.childNodes.push(node);
      else this.childNodes.splice(index, 0, node);
      return node;
    }
    removeChild(child) {
      const index = this.childNodes.indexOf(child);
      if (index >= 0) {
        this.childNodes.splice(index, 1);
        child.parentNode = null;
      }
      return child;
    }
    remove() {
      if (this.parentNode) this.parentNode.removeChild(this);
    }
    matches(selector) {
      return selector.split(',').some((part) => matchesSimpleSelector(this, part));
    }
    closest(selector) {
      let cur = this;
      while (cur) {
        if (cur.matches(selector)) return cur;
        cur = cur.parentNode;
      }
      return null;
    }
    querySelectorAll(selector) {
      const out = [];
      const visit = (node) => {
        for (const child of node.childNodes) {
          if (child.matches(selector)) out.push(child);
          visit(child);
        }
      };
      visit(this);
      return out;
    }
    querySelector(selector) {
      return this.querySelectorAll(selector)[0] || null;
    }
    addEventListener(event, fn) {
      const list = this.listeners.get(event) || [];
      list.push(fn);
      this.listeners.set(event, list);
    }
    removeEventListener(event, fn) {
      const list = this.listeners.get(event) || [];
      this.listeners.set(event, list.filter((item) => item !== fn));
    }
    click() {
      const list = [...(this.listeners.get('click') || [])];
      const evt = { preventDefault() {}, stopPropagation() {}, target: this };
      for (const fn of list) fn(evt);
    }
  }

  class FakeMutationObserver {
    constructor(callback) {
      this.callback = callback;
      this.active = false;
      observers.push(this);
    }
    observe() {
      this.active = true;
    }
    disconnect() {
      this.active = false;
    }
  }

  const doc = {
    documentElement: new FakeElement('html'),
    head: new FakeElement('head'),
    body: new FakeElement('body'),
    createElement: (tag) => new FakeElement(tag),
    getElementById(id) {
      const visit = (node) => {
        if (node.id === id) return node;
        for (const child of node.childNodes) {
          const found = visit(child);
          if (found) return found;
        }
        return null;
      };
      return visit(doc.documentElement);
    },
    querySelectorAll: (sel) => doc.body.querySelectorAll(sel),
    querySelector: (sel) => doc.body.querySelector(sel),
    addEventListener() {},
    removeEventListener() {},
  };
  doc.documentElement.append(doc.head, doc.body);

  function notifyMutations(records) {
    for (const obs of observers) {
      if (obs.active) obs.callback(records);
    }
  }

  return { doc, FakeElement, FakeMutationObserver, notifyMutations };
}

function createClock() {
  let now = 100000;
  let nextId = 1;
  const timers = new Map();

  const FakeDate = class extends Date {
    constructor(...args) {
      super(...(args.length ? args : [now]));
    }
    static now() {
      return now;
    }
  };

  function setTimeout(fn, delay = 0) {
    const id = nextId++;
    timers.set(id, { id, fn, at: now + Math.max(0, Number(delay) || 0) });
    return id;
  }

  function clearTimeout(id) {
    timers.delete(id);
  }

  async function advance(ms) {
    const target = now + ms;
    while (true) {
      let earliest = null;
      for (const entry of timers.values()) {
        if (entry.at <= target && (!earliest || entry.at < earliest.at || (entry.at === earliest.at && entry.id < earliest.id))) {
          earliest = entry;
        }
      }
      if (!earliest) break;
      timers.delete(earliest.id);
      now = earliest.at;
      earliest.fn();
      await Promise.resolve();
      await Promise.resolve();
    }
    now = target;
    await Promise.resolve();
  }

  return { FakeDate, setTimeout, clearTimeout, advance, now: () => now };
}

function createHarness({ href = 'file:///Applications/Kimi.app/Contents/Resources/app/kimi-agent.html?session=1', surface = 'work', config = {} } = {}) {
  const { doc, FakeMutationObserver, notifyMutations } = createFakeDom();
  const clock = createClock();
  const requests = [];
  const contextObj = {
    document: doc,
    location: { href },
    URL,
    Date: clock.FakeDate,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    MutationObserver: FakeMutationObserver,
    __kwgTranslationGeneration: 7,
    __kwgTranslationConfig: {
      enabled: true,
      provider: 'bing',
      targetLanguage: 'zh',
      view: 'translated',
      surface,
      ...config,
    },
    __kwgTranslate(raw) {
      requests.push(JSON.parse(raw));
    },
  };
  contextObj.globalThis = contextObj;
  contextObj.window = contextObj;
  contextObj.top = contextObj;
  const ctx = vm.createContext(contextObj);

  function boot() {
    return vm.runInContext(rendererSource, ctx);
  }

  function makeWorkBlock({
    expanded = false,
    labelText = '思考已完成',
    shimmer = false,
    bodyText = '',
    bodyClass = 'markdown',
    expandable = true,
    onExpandText = 'Reasoning steps in English.',
  } = {}) {
    const block = doc.createElement('div');
    block.className = 'think-block';

    const summary = doc.createElement('button');
    summary.className = 'think-summary';
    summary.setAttribute('aria-expanded', expanded ? 'true' : 'false');

    const label = doc.createElement('span');
    label.className = shimmer ? 'think-label shimmer-text' : 'think-label';
    label.textContent = labelText;
    summary.appendChild(label);

    const wrap = doc.createElement('div');
    wrap.className = 'think-body-wrap';

    const mountBody = (text) => {
      wrap.childNodes.slice().forEach((c) => c.remove());
      const body = doc.createElement('div');
      body.className = 'think-body';
      const md = doc.createElement('div');
      md.className = bodyClass;
      md.textContent = text;
      body.appendChild(md);
      wrap.appendChild(body);
      return { body, md };
    };

    if (expanded && bodyText) mountBody(bodyText);

    let summaryClicks = 0;
    if (expandable) {
      summary.addEventListener('click', () => {
        summaryClicks += 1;
        const currentlyExpanded = summary.getAttribute('aria-expanded') !== 'false';
        if (currentlyExpanded) {
          summary.setAttribute('aria-expanded', 'false');
          wrap.childNodes.slice().forEach((c) => c.remove());
          notifyMutations([{ type: 'attributes', target: summary }]);
        } else {
          summary.setAttribute('aria-expanded', 'true');
          mountBody(onExpandText);
          notifyMutations([
            { type: 'attributes', target: summary },
            { type: 'childList', target: wrap, addedNodes: [...wrap.childNodes], removedNodes: [] },
          ]);
        }
      });
    } else {
      summary.click = undefined;
    }

    block.append(summary, wrap);
    doc.body.appendChild(block);

    return {
      block,
      summary,
      label,
      wrap,
      mountBody,
      summaryClicks: () => summaryClicks,
    };
  }

  return { doc, clock, requests, contextObj, boot, makeWorkBlock, notifyMutations };
}

test('collapsed Work block displays hint and clicking translated or retry auto-expands and dispatches translation', async () => {
  const h = createHarness();
  const item = h.makeWorkBlock({
    expanded: false,
    labelText: '思考已完成',
    onExpandText: 'First analyze the issue, then verify the fix.',
    bodyClass: 'markdown',
  });

  const status = h.boot();
  assert.equal(status.state, 'active');
  assert.equal(typeof h.contextObj.__kwgTranslation?.status, 'function');
  assert.equal(typeof h.contextObj.__kwgTranslationReceive, 'function');

  const bar = item.block.querySelector('.kwg-bar');
  assert.ok(bar, 'toolbar is mounted on collapsed thinking block');
  const statusEl = bar.querySelector('.kwg-status');
  const buttons = bar.querySelectorAll('button');
  const translatedBtn = buttons[0];
  const retryBtn = buttons[2];

  assert.equal(statusEl.textContent, '展开后翻译');
  assert.equal(h.requests.length, 0);

  translatedBtn.click();
  assert.equal(item.summaryClicks(), 1, 'clicking 译文 expands collapsed block via .think-summary.click()');
  assert.equal(item.summary.getAttribute('aria-expanded'), 'true');
  assert.ok(item.block.querySelector('.think-body .markdown'), 'expanded .think-body .markdown is mounted');
  assert.equal(statusEl.textContent, '正在展开…');
  assert.equal(statusEl.dataset.state, 'busy');

  await h.clock.advance(1200);
  assert.equal(h.requests.length, 1, 'dispatches __kwgTranslate after expand and quiet window');
  assert.equal(h.requests[0].text, 'First analyze the issue, then verify the fix.');
  assert.equal(h.requests[0].generation, 7);

  h.contextObj.__kwgTranslationReceive(h.requests[0].id, { id: h.requests[0].id, text: '先分析问题，再验证修复。' });
  await h.clock.advance(10);

  const mirror = item.block.querySelector('.kwg-translated');
  assert.ok(mirror);
  assert.equal(mirror.hidden, false);
  assert.equal(mirror.textContent, '先分析问题，再验证修复。');
  assert.equal(item.block.classList.contains('kwg-hiding-source'), true);
  assert.equal(statusEl.textContent, '');

  // Collapse the block again, then click 重译 to verify retry also auto-expands and re-requests
  item.summary.click();
  await h.clock.advance(500);
  assert.equal(item.summary.getAttribute('aria-expanded'), 'false');
  assert.equal(mirror.hidden, true);

  retryBtn.click();
  assert.equal(item.summaryClicks(), 3, 'clicking 重译 re-expands collapsed block');
  assert.equal(item.summary.getAttribute('aria-expanded'), 'true');
  assert.equal(statusEl.textContent, '正在展开…');

  await h.clock.advance(1200);
  assert.equal(h.requests.length, 2, 'dispatches fresh translation after retry expands block');
  assert.equal(h.requests[1].text, 'First analyze the issue, then verify the fix.');
});

test('collapsed Work block without expandable summary shows explicit prompt instead of silent no-op', async () => {
  const h = createHarness();
  const unclickable = h.makeWorkBlock({ expanded: false, expandable: false });
  const stubborn = h.makeWorkBlock({ expanded: false, expandable: true });
  stubborn.summary.listeners.set('click', []);

  h.boot();

  const bar1 = unclickable.block.querySelector('.kwg-bar');
  const status1 = bar1.querySelector('.kwg-status');
  const translated1 = bar1.querySelectorAll('button')[0];
  assert.equal(status1.textContent, '展开后翻译');

  translated1.click();
  assert.equal(status1.textContent, '请先展开思考内容');
  await h.clock.advance(1500);
  assert.equal(status1.textContent, '请先展开思考内容', 'background scan does not overwrite manual expand prompt');
  assert.equal(h.requests.length, 0);

  const bar2 = stubborn.block.querySelector('.kwg-bar');
  const status2 = bar2.querySelector('.kwg-status');
  const retry2 = bar2.querySelectorAll('button')[2];
  retry2.click();
  assert.equal(status2.textContent, '正在展开…');
  await h.clock.advance(120);
  assert.equal(status2.textContent, '请先展开思考内容', 'failed expand transitions out of busy state');
  assert.equal(h.requests.length, 0);
});

test('streaming Chinese label and shimmer-text hold translation until block settles', async () => {
  const h = createHarness();
  const item = h.makeWorkBlock({
    expanded: true,
    labelText: '正在思考',
    shimmer: true,
    bodyText: 'Draft reasoning that is still streaming.',
    bodyClass: 'markdown',
  });

  h.boot();
  await h.clock.advance(2500);
  assert.equal(h.requests.length, 0, 'does not dispatch while shimmer-text and 正在思考 are present');

  item.label.classList.remove('shimmer-text');
  h.notifyMutations([{ type: 'attributes', target: item.label }]);
  await h.clock.advance(2500);
  assert.equal(h.requests.length, 0, 'still holds translation while Chinese 正在思考 label remains');

  item.label.classList.add('shimmer-text');
  item.label.textContent = '思考已完成';
  h.notifyMutations([{ type: 'attributes', target: item.label }]);
  await h.clock.advance(2500);
  assert.equal(h.requests.length, 0, 'still holds translation while shimmer-text class remains');

  item.label.classList.remove('shimmer-text');
  h.notifyMutations([{ type: 'attributes', target: item.label }]);
  await h.clock.advance(1500);
  assert.equal(h.requests.length, 1, 'dispatches once shimmer-text is removed and label says 思考已完成');
  assert.equal(h.requests[0].text, 'Draft reasoning that is still streaming.');
});

test('localized Chinese labels render for translation_busy, translation_disabled and translation_input_invalid', async () => {
  const h = createHarness();
  const item = h.makeWorkBlock({
    expanded: true,
    labelText: '思考已完成',
    shimmer: false,
    bodyText: 'Check error label mapping.',
    bodyClass: 'markdown',
  });

  h.boot();
  await h.clock.advance(100);
  assert.equal(h.requests.length, 1);

  const bar = item.block.querySelector('.kwg-bar');
  const statusEl = bar.querySelector('.kwg-status');
  const retryBtn = bar.querySelectorAll('button')[2];

  const expected = [
    ['translation_busy', '翻译队列繁忙'],
    ['translation_disabled', '翻译已关闭'],
    ['translation_input_invalid', '思考内容格式无效'],
  ];

  for (let i = 0; i < expected.length; i += 1) {
    const [code, label] = expected[i];
    if (i > 0) {
      retryBtn.click();
      await h.clock.advance(100);
    }
    const req = h.requests[i];
    assert.ok(req, `request ${i} dispatched for ${code}`);
    h.contextObj.__kwgTranslationReceive(req.id, { id: req.id, error: code });
    await h.clock.advance(10);
    assert.equal(statusEl.textContent, label);
    assert.equal(statusEl.dataset.state, 'error');
  }
});

test('collapsed Chat thinking block auto-expands via .toolcall-title-container and dispatches translation', async () => {
  const h = createHarness({ href: 'https://www.kimi.com/chat/abc123', surface: 'chat' });
  const block = h.doc.createElement('div');
  block.className = 'toolcall-container thinking-container';
  const toggle = h.doc.createElement('div');
  toggle.className = 'toolcall-title-container';
  let toggleClicks = 0;
  toggle.addEventListener('click', () => {
    toggleClicks += 1;
    block.classList.add('is-expanded');
    h.notifyMutations([{ type: 'attributes', target: block }]);
  });
  const resize = h.doc.createElement('div');
  resize.className = 'resize-container';
  const content = h.doc.createElement('div');
  content.className = 'toolcall-content';
  const md = h.doc.createElement('div');
  md.className = 'markdown';
  md.textContent = 'Chat thinking text kept in collapsed DOM.';
  content.appendChild(md);
  resize.appendChild(content);
  block.append(toggle, resize);
  h.doc.body.appendChild(block);

  h.boot();
  const bar = block.querySelector('.kwg-bar');
  const statusEl = bar.querySelector('.kwg-status');
  const translatedBtn = bar.querySelectorAll('button')[0];

  assert.equal(statusEl.textContent, '展开后翻译');
  assert.equal(h.requests.length, 0);

  translatedBtn.click();
  assert.equal(toggleClicks, 1);
  assert.equal(block.classList.contains('is-expanded'), true);
  assert.equal(statusEl.textContent, '正在展开…');

  await h.clock.advance(200);
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].text, 'Chat thinking text kept in collapsed DOM.');
});

test('async think-body mount after expand keeps busy status without flashing empty-content notice', async () => {
  const h = createHarness();
  const asyncItem = h.makeWorkBlock({ expanded: false, labelText: '思考已完成', expandable: true });
  asyncItem.summary.listeners.set('click', [() => {
    asyncItem.summary.setAttribute('aria-expanded', 'true');
    h.notifyMutations([{ type: 'attributes', target: asyncItem.summary }]);
    h.clock.setTimeout(() => {
      asyncItem.mountBody('Asynchronously mounted thinking body.');
      h.notifyMutations([
        { type: 'childList', target: asyncItem.wrap, addedNodes: [...asyncItem.wrap.childNodes], removedNodes: [] },
      ]);
    }, 140);
  }]);

  const emptyItem = h.makeWorkBlock({ expanded: false, labelText: '思考已完成', expandable: true });
  emptyItem.summary.listeners.set('click', [() => {
    emptyItem.summary.setAttribute('aria-expanded', 'true');
    h.notifyMutations([{ type: 'attributes', target: emptyItem.summary }]);
  }]);

  h.boot();

  const bar = asyncItem.block.querySelector('.kwg-bar');
  const statusEl = bar.querySelector('.kwg-status');
  const translatedBtn = bar.querySelectorAll('button')[0];
  const seenStatuses = [];
  const origDesc = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(statusEl), 'textContent');
  Object.defineProperty(statusEl, 'textContent', {
    get() { return origDesc.get.call(this); },
    set(value) {
      seenStatuses.push(String(value ?? ''));
      origDesc.set.call(this, value);
    },
    configurable: true,
  });

  translatedBtn.click();
  assert.equal(asyncItem.summary.getAttribute('aria-expanded'), 'true');
  assert.equal(statusEl.textContent, '正在展开…');
  assert.equal(statusEl.dataset.state, 'busy');

  // At 80ms and 120ms, .think-body has not mounted yet: grace window must keep 正在展开…
  await h.clock.advance(80);
  assert.equal(statusEl.textContent, '正在展开…');
  assert.equal(statusEl.dataset.state, 'busy');
  await h.clock.advance(40);
  assert.equal(statusEl.textContent, '正在展开…');

  // At 140ms .think-body mounts asynchronously, then after quiet window translation dispatches cleanly
  await h.clock.advance(1200);
  assert.equal(seenStatuses.includes('暂无可翻译内容'), false, 'must never flash 暂无可翻译内容 while async think-body mounts');
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].text, 'Asynchronously mounted thinking body.');

  h.contextObj.__kwgTranslationReceive(h.requests[0].id, { id: h.requests[0].id, text: '异步挂载的思考译文。' });
  await h.clock.advance(10);
  assert.equal(asyncItem.block.querySelector('.kwg-translated')?.textContent, '异步挂载的思考译文。');
  assert.equal(statusEl.textContent, '');

  // A genuinely empty expanded block transitions to 暂无可翻译内容 once the 450ms grace expires
  const emptyBar = emptyItem.block.querySelector('.kwg-bar');
  const emptyStatus = emptyBar.querySelector('.kwg-status');
  emptyBar.querySelectorAll('button')[0].click();
  await h.clock.advance(200);
  assert.equal(emptyStatus.textContent, '正在展开…');
  await h.clock.advance(400);
  assert.equal(emptyStatus.textContent, '暂无可翻译内容');
});

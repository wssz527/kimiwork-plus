(() => {
  const $ = id => document.getElementById(id);
  const doc = () => document;
  let request = async () => null;
  let notify = () => {};

  const number = value => typeof value === 'number' && Number.isFinite(value) ? value : null;
  const integer = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const textOf = value => typeof value === 'string' ? value : '';

  function node(tag, className, text) {
    const element = doc().createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = text;
    return element;
  }

  function button(label, { className = 'btn btn--outline btn--sm', onClick, disabled = false } = {}) {
    const element = node('button', className, label);
    element.type = 'button';
    element.disabled = disabled;
    if (onClick) element.addEventListener('click', onClick);
    return element;
  }

  const time = value => {
    if (typeof value !== 'string' || !value) return '';
    const date = new Date(value);
    return Number.isFinite(date.getTime())
      ? new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(date)
      : '';
  };

  const stamp = value => {
    const at = integer(value);
    if (at === null || at <= 0) return '';
    return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(new Date(at));
  };

  function money(amount, currency) {
    const value = number(amount);
    if (value === null) return null;
    const code = textOf(currency).toUpperCase();
    if (/^[A-Z]{3}$/.test(code)) {
      try {
        return new Intl.NumberFormat('zh-CN', { style: 'currency', currency: code,
          maximumFractionDigits: Math.abs(value) < 10 ? 4 : 2 }).format(value);
      } catch { /* 未知货币码退回纯数字 */ }
    }
    return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(value) + (code ? ' ' + code : '');
  }

  const percent = value => {
    const parsed = number(value);
    return parsed === null ? null : new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 }).format(parsed) + '%';
  };

  /* ---------- 额度：Kimi Code 三来源 ---------- */

  const CODE_REASON = {
    credential_missing: '没有可用的凭据，未发出请求',
    auth_rejected: 'Key 被拒绝或已过期',
    usage_endpoint_unavailable: '这个 Key 不是 Kimi Code Key',
    rate_limited: '官方限流，稍后再读',
    upstream_error: '官方接口返回错误',
    timeout: '读取超时',
    network_error: '网络不可达',
    response_too_large: '回复超过允许大小',
    invalid_response: '回复格式与已确认接口不符',
    age: '读数已超过新鲜期',
    throttled: '刚读过，等一会儿再读',
    selection_changed: '读取期间换了来源，这次结果已作废',
  };

  function codeReasonText(code, notes = []) {
    if (notes.includes('reading_superseded')) return '这份读数属于另一个来源或账号，不作数';
    return CODE_REASON[code] || code || '';
  }

  const code = {
    state: null,
    loading: false,
    form: { open: false, key: '', busy: false },
  };

  async function loadCode() {
    if (code.loading) return;
    code.loading = true;
    try {
      const result = await request('/api/quota/code');
      if (result && result.state) code.state = result.state;
    } catch (error) {
      code.state = { status: 'error', errorReason: error.code || 'request_failed', sources: [], windows: [], notes: [] };
    } finally {
      code.loading = false;
    }
    renderCode();
  }

  async function codeAction(action, input = {}) {
    try {
      const result = await request('/api/quota/code', { method: 'POST', body: { action, ...input } });
      if (result && result.state) code.state = result.state;
      renderCode();
      return result;
    } catch (error) {
      notify(error.message || '额度操作失败', 'error');
      return null;
    }
  }

  function renderCode() {
    const body = $('plus-code-body');
    if (!body) return;
    const state = code.state;
    if (!state) {
      body.replaceChildren(node('p', 'empty', '正在读取 Kimi Code 额度。'));
      return;
    }
    const parts = [renderCodeVerdict(state), renderCodeWindows(state), renderCodeWallet(state), renderCodeSources(state)];
    body.replaceChildren(...parts.filter(Boolean));
  }

  function renderCodeVerdict(state) {
    const wrap = node('div', 'plus-verdict');
    const dot = node('span', 'dot');
    const status = textOf(state.status) || 'not_read';
    const tone = status === 'ok' ? 'ok' : status === 'stale' ? 'warn' : status === 'error' ? 'bad' : 'neutral';
    dot.dataset.tone = tone;
    const words = {
      ok: '已读到本机额度',
      stale: '上次成功读数 · 已过期',
      error: '读取失败',
      not_read: '尚未读取',
      not_configured: '没有可用凭据',
      throttled: '刚读过，稍后再读',
      superseded: '读取期间换了来源',
    };
    const word = node('strong', null, words[status] || status);
    wrap.append(dot, word);

    const reason = state.errorReason ? codeReasonText(state.errorReason, state.notes || []) : '';
    if (reason) wrap.append(node('span', 'plus-verdict__note', reason));
    const observed = time(state.observedAt);
    if (observed) wrap.append(node('span', 'plus-verdict__note', '读数时间 ' + observed));
    return wrap;
  }

  function renderCodeWindows(state) {
    const windows = Array.isArray(state.windows) ? state.windows : [];
    if (!windows.length) return null;
    const list = node('div', 'plus-windows');
    for (const window of windows) {
      const row = node('div', 'plus-window');
      row.append(node('p', 'plus-window__label', textOf(window.label) || textOf(window.key)));
      const used = percent(window.usedPercent);
      const right = node('div', 'plus-window__right');
      right.append(node('strong', 'plus-window__value', used === null ? '未提供' : used));
      const remaining = percent(window.remainingPercent);
      if (remaining !== null) right.append(node('small', null, '剩余 ' + remaining));
      row.append(right);
      if (used !== null) {
        const bar = node('div', 'plus-progress');
        const fill = node('span');
        fill.style.width = Math.max(0, Math.min(100, number(window.usedPercent) || 0)) + '%';
        bar.append(fill);
        row.append(bar);
      }
      const reset = time(window.resetAt);
      if (reset) row.append(node('small', 'plus-window__reset', '重置于 ' + reset));
      list.append(row);
    }
    return list;
  }

  function renderCodeWallet(state) {
    const wallet = state.wallet;
    if (!wallet || typeof wallet !== 'object') return null;
    const centsToMoney = value => number(value) === null ? null : money(number(value) / 100, wallet.currency || 'USD');
    const box = node('div', 'plus-wallet');
    box.append(node('p', 'plus-window__label', '加速包余额'));
    const rows = [
      ['可用余额', centsToMoney(wallet.balanceCents)],
      ['总额', centsToMoney(wallet.totalCents)],
    ];
    if (wallet.monthlyChargeLimitEnabled === true) {
      rows.push(['本月已用', centsToMoney(wallet.monthlyUsedCents)]);
      rows.push(['本月上限', centsToMoney(wallet.monthlyChargeLimitCents)]);
    }
    for (const [label, value] of rows) {
      const line = node('div', 'plus-wallet__row');
      line.append(node('span', null, label), node('strong', null, value === null ? '未提供' : value));
      box.append(line);
    }
    return box;
  }

  function renderCodeSources(state) {
    const box = node('div', 'plus-sources');
    box.append(node('p', 'plus-window__label', '额度来源'));
    const sources = Array.isArray(state.sources) ? state.sources : [];
    const selected = textOf(state.selectedSource);
    for (const entry of sources) {
      const id = textOf(entry.id);
      const row = node('div', 'plus-source');
      row.dataset.selected = id === selected ? 'true' : 'false';
      const main = node('div', 'plus-source__main');
      main.append(node('strong', null, textOf(entry.label) || id));
      const meta = [];
      if (entry.accountMasked) meta.push('账号 ' + entry.accountMasked);
      meta.push(availabilityText(entry.availability));
      if (entry.detail) meta.push(CODE_REASON[entry.detail] || textOf(entry.detail));
      main.append(node('small', null, meta.join(' · ')));
      row.append(main);

      const actions = node('div', 'plus-source__actions');
      if (id === selected) {
        actions.append(node('span', 'plus-tag', '正在使用'));
      } else if (entry.availability === 'unadopted') {
        /* 采用本机登录是显式动作：勾选就等于同意这个产品使用那份登录。 */
        actions.append(button('使用本机登录', { onClick: () => adopt('local') }));
      } else if (entry.availability === 'available') {
        actions.append(button('用这个来源', { onClick: () => adopt(id) }));
      }
      row.append(actions);
      box.append(row);
    }

    const form = node('div', 'plus-code-form');
    if (code.form.open) {
      const label = node('label', 'field');
      label.append(node('span', 'field__label', 'Kimi Code Key'));
      const input = doc().createElement('input');
      input.type = 'password';
      input.className = 'mono';
      input.autocomplete = 'off';
      input.spellcheck = false;
      input.placeholder = '粘贴官方 Kimi Code Key';
      input.value = code.form.key;
      input.addEventListener('input', () => { code.form.key = input.value; });
      label.append(input, node('span', 'field__hint', '只写入本机系统凭据存储；保存后会立即回读校验。'));
      const line = node('div', 'action-line action-line--start');
      line.append(
        button('保存并改用这个 Key', { className: 'btn btn--primary btn--sm', onClick: () => saveManual(input) }),
        button('取消', { className: 'btn btn--quiet btn--sm', onClick: () => { code.form = { open: false, key: '', busy: false }; renderCode(); } }),
      );
      form.append(label, line);
    } else {
      const line = node('div', 'action-line action-line--start');
      line.append(button('填写 Kimi Code Key', { onClick: () => { code.form.open = true; renderCode(); } }));
      const manualSelected = selected === 'manual';
      if (manualSelected) {
        line.append(button('清除手填 Key', { className: 'btn btn--quiet btn--sm', onClick: clearManual }));
      }
      form.append(line);
    }
    box.append(form);
    return box;
  }

  function availabilityText(value) {
    return ({ available: '凭据就绪', missing: '未配置', unadopted: '已检测到，尚未采用', unavailable: '不可用' })[textOf(value)] || textOf(value) || '未知';
  }

  async function adopt(source) {
    const result = await codeAction('select', { source });
    if (result) notify(source === 'local' ? '已采用本机 Kimi Code 登录；现在读一次额度。' : '已改用该来源。', 'ok');
    if (result) void refreshCode(true);
  }

  async function saveManual(input) {
    const value = textOf(input && input.value).trim();
    if (!value) { notify('请先填写 Key。', 'error'); return; }
    const result = await codeAction('saveManual', { key: value });
    if (!result) return;
    code.form = { open: false, key: '', busy: false };
    notify('Key 已保存并回读校验。', 'ok');
    void refreshCode(true);
  }

  async function clearManual() {
    const result = await codeAction('clearManual');
    if (result) notify('手填 Key 已清除，选择已回退。', 'ok');
  }

  async function refreshCode(force = false) {
    try {
      await request('/api/quota/code', { method: 'POST', body: { action: 'refresh', ...(force ? { force: true } : {}) } });
      await loadCode();
    } catch (error) {
      notify(error.message || '额度刷新失败', 'error');
    }
  }

  /* ---------- 额度：Magpie ---------- */

  const MAGPIE_REASON = {
    magpie_unreachable: '连不上 Magpie；确认它在运行并监听这个地址',
    magpie_key_rejected: 'Magpie 拒绝了这个密钥',
    magpie_endpoint_unavailable: '这个地址上没有 Magpie 额度接口',
    rate_limited: '请求过于频繁',
    magpie_error: 'Magpie 返回错误',
    timeout: '读取超时',
    network_error: '网络不可达',
    invalid_response: '回复格式不被接受',
    response_too_large: '回复超过允许大小',
    invalid_magpie_address: '地址不符合规则',
    connection_unavailable: '这条连接已不在配置中',
    invalid_magpie_connection: '这条连接的地址不是 Magpie 网关',
  };

  const magpie = { state: null, loading: false, form: { open: false, connectionId: '', baseUrl: '', busy: false } };

  async function loadMagpie() {
    if (magpie.loading) return;
    magpie.loading = true;
    try {
      const result = await request('/api/quota/magpie');
      if (result && result.state) magpie.state = result.state;
    } catch (error) {
      magpie.state = { status: 'error', errorReason: error.code || 'request_failed', notes: [], snapshot: null };
    } finally {
      magpie.loading = false;
    }
    renderMagpie();
  }

  async function magpieAction(body) {
    try {
      const result = await request('/api/quota/magpie', { method: 'POST', body });
      if (result && result.state) magpie.state = result.state;
      renderMagpie();
      return result;
    } catch (error) {
      notify(error.message || 'Magpie 操作失败', 'error');
      return null;
    }
  }

  function renderMagpie() {
    const body = $('plus-magpie-body');
    if (!body) return;
    const state = magpie.state;
    if (!state) { body.replaceChildren(node('p', 'empty', '正在读取 Magpie 额度。')); return; }
    const wrap = node('div', 'plus-verdict');
    const dot = node('span', 'dot');
    const status = textOf(state.status) || 'not_read';
    dot.dataset.tone = status === 'ok' ? 'ok' : status === 'stale' ? 'warn' : status === 'error' ? 'bad' : 'neutral';
    const words = { ok: '已读到余额', stale: '上次成功读数 · 已过期', error: '读取失败', not_read: '尚未读取' };
    wrap.append(dot, node('strong', null, words[status] || status));
    if (state.errorReason) wrap.append(node('span', 'plus-verdict__note', MAGPIE_REASON[state.errorReason] || state.errorReason));
    if ((state.notes || []).includes('reading_from_another_address')) {
      wrap.append(node('span', 'plus-verdict__note', '上次读数来自另一个地址，换址后已作废'));
    }
    if (state.baseUrl) wrap.append(node('span', 'plus-verdict__note mono', state.baseUrl));
    const fetched = state.snapshot && time(state.snapshot.fetchedAt);
    if (fetched) wrap.append(node('span', 'plus-verdict__note', '读数时间 ' + fetched));

    const children = [wrap, renderMagpieEntries(state), renderMagpieForm()];
    body.replaceChildren(...children.filter(Boolean));
  }

  function renderMagpieEntries(state) {
    const snapshot = state.snapshot;
    if (!snapshot || !Array.isArray(snapshot.entries) || !snapshot.entries.length) return null;
    const list = node('div', 'plus-magpie');
    for (const entry of snapshot.entries) {
      const card = node('article', 'plus-magpie__entry');
      const head = node('div', 'plus-magpie__head');
      head.append(node('strong', null, textOf(entry.name) || textOf(entry.provider)));
      const bits = [textOf(entry.kind) === 'balance' ? '余额' : textOf(entry.kind) === 'subscription' ? '订阅' : ''];
      if (entry.plan) bits.push(textOf(entry.plan));
      if (entry.user) bits.push(textOf(entry.user));
      head.append(node('small', null, bits.filter(Boolean).join(' · ')));
      card.append(head);

      /* 余额条目只有格式化字符串时照原样显示，绝不把无法解析的值写成 0。 */
      if (entry.balance) {
        const line = node('div', 'plus-wallet__row');
        line.append(node('span', null, '余额'), node('strong', null, textOf(entry.balance)));
        card.append(line);
      }
      for (const window of Array.isArray(entry.windows) ? entry.windows : []) {
        card.append(renderMagpieWindow(window));
      }
      list.append(card);
    }
    return list;
  }

  function renderMagpieWindow(window) {
    const row = node('div', 'plus-window');
    row.append(node('p', 'plus-window__label', textOf(window.name) || '额度'));
    const right = node('div', 'plus-window__right');
    if (textOf(window.basis) === 'amount') {
      const amount = number(window.amount);
      const limit = number(window.limit);
      const unit = textOf(window.unit);
      const figure = amount === null ? '未提供' : new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(amount)
        + (limit === null ? '' : ' / ' + new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(limit))
        + (unit ? ' ' + unit : '');
      right.append(node('strong', 'plus-window__value', figure));
      if (amount !== null && limit !== null && limit > 0) {
        const bar = node('div', 'plus-progress');
        const fill = node('span');
        fill.style.width = Math.max(0, Math.min(100, (amount / limit) * 100)) + '%';
        bar.append(fill);
        row.append(bar);
      }
    } else if (textOf(window.basis) === 'percent') {
      const used = number(window.used);
      right.append(node('strong', 'plus-window__value', used === null ? '未提供' : percent(used)));
      if (used !== null) {
        const bar = node('div', 'plus-progress');
        const fill = node('span');
        fill.style.width = Math.max(0, Math.min(100, used)) + '%';
        bar.append(fill);
        row.append(bar);
      }
    } else {
      /* 只有预渲染字符串：数字未知，照原样显示并明确说明。 */
      right.append(node('strong', 'plus-window__value', textOf(window.display) || '未提供'));
      if (textOf(window.display)) right.append(node('small', null, 'Magpie 只给了文本，未提供数值'));
    }
    row.append(right);
    if (window.note) row.append(node('small', 'plus-window__reset', textOf(window.note)));
    const reset = time(window.resetsAt);
    if (reset) row.append(node('small', 'plus-window__reset', '重置于 ' + reset));
    return row;
  }

  function renderMagpieForm() {
    const form = node('div', 'plus-code-form');
    if (!magpie.form.open) {
      const line = node('div', 'action-line action-line--start');
      line.append(button('更改 Magpie 地址', { onClick: () => { magpie.form.open = true; renderMagpie(); } }));
      form.append(line);
      return form;
    }
    const connections = connectionRows();
    const grid = node('div', 'field-row');
    const useConnection = node('label', 'field');
    useConnection.append(node('span', 'field__label', '复用已保存的连接'));
    const select = doc().createElement('select');
    const blank = doc().createElement('option');
    blank.value = '';
    blank.textContent = '不复用，直接填地址';
    select.append(blank);
    for (const row of connections) {
      const option = doc().createElement('option');
      option.value = row.id;
      option.textContent = (row.name || row.id) + ' · ' + (row.baseURL || row.id);
      select.append(option);
    }
    select.value = magpie.form.connectionId;
    select.addEventListener('change', () => { magpie.form.connectionId = select.value; });
    useConnection.append(select, node('span', 'field__hint', '复用这条连接的地址与密钥；密钥只在本机读取。'));
    grid.append(useConnection);

    const direct = node('label', 'field');
    direct.append(node('span', 'field__label', '或直接填地址'));
    const input = doc().createElement('input');
    input.type = 'url';
    input.className = 'mono';
    input.placeholder = 'http://127.0.0.1:3425';
    input.value = magpie.form.baseUrl;
    input.addEventListener('input', () => { magpie.form.baseUrl = input.value; });
    direct.append(input, node('span', 'field__hint', '只接受 HTTPS，或本机回环上的 HTTP。直接填地址时不带密钥。'));
    grid.append(direct);
    form.append(grid);

    const line = node('div', 'action-line action-line--start');
    line.append(
      button('保存地址', { className: 'btn btn--primary btn--sm', onClick: saveMagpieAddress }),
      button('取消', { className: 'btn btn--quiet btn--sm', onClick: () => { magpie.form = { open: false, connectionId: '', baseUrl: '', busy: false }; renderMagpie(); } }),
    );
    form.append(line);
    return form;
  }

  async function saveMagpieAddress() {
    const body = { action: 'address' };
    if (magpie.form.connectionId) body.connectionId = magpie.form.connectionId;
    else body.baseUrl = textOf(magpie.form.baseUrl).trim();
    if (!body.connectionId && !body.baseUrl) { notify('请选择连接或填写 Magpie 地址。', 'error'); return; }
    const result = await magpieAction(body);
    if (!result) return;
    magpie.form = { open: false, connectionId: '', baseUrl: '', busy: false };
    notify('地址已保存；旧地址的读数已作废，正在重读。', 'ok');
    void refreshMagpie();
  }

  async function refreshMagpie() {
    const result = await magpieAction({ action: 'refresh' });
    if (result) renderMagpie();
  }

  function connectionRows() {
    const rows = stateData()?.connections;
    if (!rows) return [];
    const list = Array.isArray(rows.saved) ? rows.saved : Array.isArray(rows.active) ? rows.active : [];
    return list.filter(row => row && typeof row === 'object' && row.id && !row.legacy);
  }

  /* ---------- 账号 ---------- */

  const ACCOUNT_STATE = {
    ready: '可用', unavailable: '账号切换不可用',
  };

  const account = { busy: null, confirm: null, error: null };
  let openAccounts = () => {};

  function accountsOf(data) {
    const raw = data && data.accounts;
    if (!raw || typeof raw !== 'object') return null;
    return {
      state: textOf(raw.state) || 'unknown',
      reason: textOf(raw.reason),
      accounts: Array.isArray(raw.accounts) ? raw.accounts.filter(row => row && typeof row === 'object') : [],
      currentId: textOf(raw.currentId),
      adding: raw.adding === true,
      restoreAccountId: textOf(raw.restoreAccountId),
      activation: raw.activation && typeof raw.activation === 'object' ? raw.activation : {},
    };
  }

  /* 官方账号在没登录名称时只给一个 ID；显示尾号比显示一串 ID 更容易分辨，
     并明确它是从 ID 派生的默认名。 */
  function accountName(row) {
    const name = textOf(row.name).trim();
    if (name) return name;
    const id = textOf(row.id);
    const tail = id.replace(/[^A-Za-z0-9]/g, '').slice(-6);
    return tail ? '账号 ' + tail + '（默认名）' : '未命名账号';
  }

  function renderAccountsBar(data) {
    const slot = $('plus-account-bar');
    if (!slot) return;
    const book = accountsOf(data);
    if (!book || book.state !== 'ready') {
      slot.replaceChildren(node('span', 'plus-account-bar__label', '账号'));
      slot.append(node('span', 'plus-account-bar__hint', book && book.reason ? '账号切换不可用（' + book.reason + '）' : '账号切换不可用'));
      slot.append(button('账号管理', { className: 'btn btn--quiet btn--sm', onClick: () => openAccounts() }));
      return;
    }
    const current = book.accounts.find(row => textOf(row.id) === book.currentId);
    const parts = [node('span', 'plus-account-bar__label', '账号')];
    if (book.accounts.length) {
      const select = doc().createElement('select');
      select.id = 'plus-account-select';
      select.setAttribute('aria-label', '切换当前账号');
      if (!current) {
        const option = doc().createElement('option');
        option.value = '';
        option.textContent = '当前账号未记录';
        select.append(option);
      }
      for (const row of book.accounts) {
        const option = doc().createElement('option');
        option.value = textOf(row.id);
        option.textContent = accountName(row);
        select.append(option);
      }
      select.value = current ? textOf(current.id) : '';
      select.disabled = Boolean(account.busy || book.adding || book.restoreAccountId);
      select.addEventListener('change', () => requestSwitch(select.value));
      parts.push(select);
    } else {
      parts.push(node('span', 'plus-account-bar__hint', '尚未保存任何账号'));
    }
    if (book.accounts.length > 1 || account.busy) {
      parts.push(node('span', 'plus-account-bar__hint', account.busy ? '正在切换…' : '必要时重启 Work'));
    }
    parts.push(button('账号管理', { className: 'btn btn--quiet btn--sm', onClick: () => openAccounts() }));
    slot.replaceChildren(...parts);
  }

  function requestSwitch(id) {
    if (!id || id === (accountsOf(stateData())?.currentId || '')) return;
    /* 切换前的提示是信息而不是第二次危险确认：Work 里的任务会被检查，必要时
       正常重启。用户已经在下拉里选择了，就不再弹一个确认框拦一道。 */
    notify('正在切换到所选账号。Work 里的任务会先被检查，必要时正常重启 Kimi 以应用登录。', 'ok');
    void runAccount('account-switch', { accountId: id });
  }

  async function runAccount(action, extra = {}) {
    if (account.busy) return null;
    account.busy = action;
    account.error = null;
    renderAccountsAll();
    try {
      const result = await request('/api/actions', { method: 'POST', body: { action, ...extra } });
      const operation = result && result.operation ? result.operation : null;
      if (operation && operation.id) return await followAccountOperation(operation.id, action);
    } catch (error) {
      account.error = { action, reason: textOf(error.code), message: error.message || '账号操作失败，请刷新状态后重试。' };
      notify(account.error.message, 'error');
      account.busy = null;
      renderAccountsAll();
    }
    return null;
  }

  async function restartAndContinueAccount() {
    const restoring = Boolean(accountsOf(stateData())?.restoreAccountId);
    const operation = await runAccount('restart-kimi');
    if (operation && !['failed', 'unavailable', 'interrupted'].includes(textOf(operation.state))) {
      await runAccount(restoring ? 'account-cancel-add' : 'account-begin-add');
    }
  }

  /* 账号动作是异步 operation：面板已有轮询机制，这里用同一份状态等它结束，
     而不是自己起一个独立的轮询循环。 */
  async function followAccountOperation(id, action) {
    const deadline = Date.now() + 100000;
    while (Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, 900));
      let data = null;
      try {
        data = await request('/api/state');
      } catch { /* 一次读取失败不终止等待 */ }
      if (data) stateSet(data);
      const op = data && data.operation;
      if (!op || op.id !== id) continue;
      if (op.state === 'running' || op.state === 'pending') continue;
      account.busy = null;
      reportAccountResult(action, op);
      renderAccountsAll();
      return op;
    }
    account.busy = null;
    account.error = { action, reason: 'account_operation_timeout', message: '账号操作仍未返回结果；请刷新状态确认，不要重复提交。' };
    renderAccountsAll();
    notify(account.error.message, 'error');
    return null;
  }

  function reportAccountResult(action, op) {
    const failed = ['failed', 'unavailable', 'interrupted'].includes(textOf(op.state));
    const reason = textOf(op.reason);
    if (failed) {
      account.error = { action, reason, message: ACCOUNT_REASON[reason] || '账号操作未完成，请刷新状态或重新准备 Kimi 接入。' };
      notify(account.error.message, 'error');
      return;
    }
    account.error = null;
    notify(ACTION_WORD[action] || '账号操作', 'ok');
  }

  const ACTION_WORD = {
    'account-capture': '已保存当前账号',
    'account-begin-add': '已开始添加账号',
    'account-complete-add': '新账号已保存',
    'account-cancel-add': '已取消添加并恢复原账号',
    'account-switch': '账号已切换',
    'account-remove': '账号已移除',
    'restart-kimi': 'Kimi 已正常重启',
  };

  const ACCOUNT_REASON = {
    account_session_unavailable: '未连接到 Kimi 的登录窗口，请正常重启 Kimi 后继续添加。',
    account_session_page_timeout: 'Kimi 登录窗口没有及时响应，请正常重启 Kimi 后重试。',
    account_identity_unverified: '还没有确认当前登录，请在 Kimi 窗口完成登录后重试。',
    account_session_work_busy: 'Work 有任务正在运行，请先结束任务再添加或切换账号。',
    account_work_busy: 'Work 有任务正在运行，请先结束任务再添加或切换账号。',
    kimi_quit_timeout: 'Kimi 尚未退出，当前登录未被清除。请检查 Kimi 窗口后重试。',
    unmanaged_kimi_running: '当前 Kimi 尚未接入，请正常重启 Kimi 后继续。',
    account_login_pending: '有一个添加流程还没结束，请先完成或取消它',
    account_login_not_pending: '当前没有进行中的添加流程',
    account_login_unchanged: '登录的还是同一个账号，没有新账号可保存',
    account_switch_unavailable: '这台机器上没有可用的账号切换接口',
    account_switch_restore_failed: '切换失败且恢复原账号也失败了，请按提示手动恢复',
    account_identity_mismatch: '切换后读到的账号与所选不一致，已恢复原账号',
    operation_busy: '已有操作在进行，等它结束再试',
    credentials_unavailable: '系统凭据存储不可用',
  };

  function renderAccountsAll() {
    const data = stateData();
    renderAccountsBar(data);
    renderAccountsOverview(data);
    renderAccountsManagement(data);
  }

  function renderAccountsOverview(data) {
    const slot = $('plus-overview-account');
    if (!slot) return;
    const book = accountsOf(data);
    if (!book) { slot.replaceChildren(node('p', 'hint', '账号状态尚未读取。')); return; }
    if (book.state !== 'ready') {
      slot.replaceChildren(node('p', 'hint', '账号切换不可用' + (book.reason ? '（' + (ACCOUNT_REASON[book.reason] || book.reason) + '）' : '') + '。'));
      return;
    }
    const current = book.accounts.find(row => textOf(row.id) === book.currentId);
    const wrap = node('div', 'plus-overview');
    if (current) {
      wrap.append(node('strong', 'plus-overview__value', accountName(current)));
      const meta = [];
      if (current.lastUsedAt) meta.push('上次使用 ' + stamp(current.lastUsedAt));
      if (book.accounts.length > 1) meta.push('共 ' + book.accounts.length + ' 个已保存账号');
      if (meta.length) wrap.append(node('small', null, meta.join(' · ')));
    } else {
      wrap.append(node('strong', 'plus-overview__value', '当前账号未记录'));
      wrap.append(node('small', null, '打开 Work 并登录后，可在设置里保存当前账号。'));
    }
    const activation = book.activation || {};
    if (textOf(activation.state)) wrap.append(activationLine(activation));
    slot.replaceChildren(wrap);
  }

  function activationLine(activation) {
    const state = textOf(activation.state);
    const words = { hot: '已应用', restarted: '已应用', 'needs-login': '需要重新登录', restored: '已恢复原账号', failed: '应用失败' };
    const bits = [words[state] || state];
    if (activation.restarted === true) bits.push('已重启 Work');
    if (activation.verified === true) bits.push('读回一致');
    if (activation.hot === true) bits.push('未重启，热切换');
    if (activation.requiresLogin === true) bits.push('需要在该窗口登录');
    if (activation.modelsState === 'ready') bits.push('第三方模型已就绪');
    if (activation.modelsState === 'pending' || activation.modelsState === 'failed') {
      bits.push('账号已切换，第三方模型尚未就绪，请在模型页同步');
    }
    return node('small', 'plus-overview__meta', bits.join(' · '));
  }

  function appendAccountError(wrap) {
    if (!account.error) return;
    const { action, reason, message } = account.error;
    const box = node('div', 'plus-pending plus-pending--warn');
    box.setAttribute('role', 'alert');
    box.append(node('p', null, ACCOUNT_REASON[reason] || message));
    if (['account-begin-add', 'account-capture', 'account-cancel-add'].includes(action)
      && ['account_session_unavailable', 'account_session_page_timeout', 'unmanaged_kimi_running'].includes(reason)) {
      const restoring = Boolean(accountsOf(stateData())?.restoreAccountId);
      box.append(node('p', 'hint', restoring
        ? '请先结束正在运行的任务。正常重启后会核对并恢复已保存的原账号。'
        : '请先结束正在运行的任务。正常重启后会先保存当前登录，再打开另一个账号的登录页。'));
      box.append(button(restoring ? '正常重启 Kimi 并恢复原账号' : '正常重启 Kimi 并继续添加', {
        className: 'btn btn--primary btn--sm', disabled: Boolean(account.busy), onClick: restartAndContinueAccount,
      }));
    }
    wrap.append(box);
  }

  function renderAccountsManagement(data) {
    const slot = $('plus-accounts-body');
    if (!slot) return;
    const book = accountsOf(data);
    if (!book) { slot.replaceChildren(node('p', 'hint', '账号状态尚未读取。')); return; }
    if (book.state !== 'ready') {
      const wrap = node('div');
      wrap.append(node('p', 'note note--warn', '账号切换不可用' + (book.reason ? '：' + (ACCOUNT_REASON[book.reason] || book.reason) : '') + '。'));
      appendAccountError(wrap);
      wrap.append(accountActionLine(book));
      slot.replaceChildren(wrap);
      return;
    }
    const wrap = node('div');
    appendAccountError(wrap);

    if (book.adding || book.restoreAccountId) {
      const pending = node('div', 'plus-pending');
      pending.append(node('strong', null, book.adding ? '正在等你完成官方登录' : '待恢复原账号'));
      pending.append(node('p', 'hint', book.adding
        ? '请在刚打开的 Kimi Work 窗口完成登录。登录完成后这边会自动识别；也可以手动确认。'
        : '上次切换未能恢复原账号。请先重试恢复，再开始其他账号操作。'));
      const line = node('div', 'action-line action-line--start');
      if (book.adding) line.append(button('已完成登录，保存这个账号', {
        className: 'btn btn--primary btn--sm', disabled: Boolean(account.busy), onClick: () => runAccount('account-complete-add'),
      }));
      line.append(button(book.adding ? '取消并恢复原账号' : '恢复原账号', {
        className: 'btn btn--quiet btn--sm', disabled: Boolean(account.busy), onClick: () => runAccount('account-cancel-add'),
      }));
      pending.append(line);
      wrap.append(pending);
    }

    const list = node('div', 'plus-accounts');
    if (!book.accounts.length) {
      list.append(node('p', 'empty', '还没有保存的账号。添加账号会先保存当前登录，再打开官方登录窗口。'));
    }
    for (const row of book.accounts) {
      const id = textOf(row.id);
      const item = node('div', 'plus-account');
      item.dataset.current = id === book.currentId ? 'true' : 'false';
      const main = node('div', 'plus-account__main');
      main.append(node('strong', null, accountName(row)));
      const meta = [];
      if (id === book.currentId) meta.push('当前');
      if (row.capturedAt) meta.push('保存于 ' + stamp(row.capturedAt));
      if (row.lastUsedAt) meta.push('上次使用 ' + stamp(row.lastUsedAt));
      main.append(node('small', null, meta.join(' · ')));
      /* needsLogin 只描述这一行：别的账号要登录，不能变成整页的警告。 */
      if (row.needsLogin === true) main.append(node('span', 'plus-tag plus-tag--warn', '需要重新登录'));
      item.append(main);

      const actions = node('div', 'plus-account__actions');
      if (id !== book.currentId) {
        actions.append(button('切换', { disabled: Boolean(account.busy || book.adding || book.restoreAccountId), onClick: () => requestSwitch(id) }));
      }
      actions.append(button('移除', {
        className: 'btn btn--quiet btn--sm', disabled: Boolean(account.busy || id === book.currentId || id === book.restoreAccountId),
        onClick: () => confirmRemove(row),
      }));
      item.append(actions);
      list.append(item);
    }
    wrap.append(list);

    if (account.confirm && account.confirm.kind === 'remove') {
      const targetId = account.confirm.id;
      const row = book.accounts.find(entry => textOf(entry.id) === targetId);
      const box = node('div', 'plus-pending plus-pending--warn');
      box.append(node('strong', null, '移除「' + (row ? accountName(row) : '这个账号') + '」？'));
      box.append(node('p', 'hint', '本机保存的这份登录会被删除；官方账号本身不受影响，之后仍可重新添加。'));
      const line = node('div', 'action-line action-line--start');
      line.append(
        button('确认移除', {
          className: 'btn btn--primary btn--sm',
          onClick: () => {
            account.confirm = null;
            void runAccount('account-remove', { accountId: targetId });
          },
        }),
        button('保留', { className: 'btn btn--quiet btn--sm', onClick: () => { account.confirm = null; renderAccountsAll(); } }),
      );
      box.append(line);
      wrap.append(box);
    }

    wrap.append(accountActionLine(book));
    slot.replaceChildren(wrap);
  }

  function accountActionLine(book) {
    const line = node('div', 'action-line action-line--start');
    if (!book.adding && !book.restoreAccountId) {
      line.append(button('添加账号', {
        className: 'btn btn--primary btn--sm', disabled: Boolean(account.busy),
        onClick: () => runAccount('account-begin-add'),
      }));
      line.append(button(book.accounts.length ? '只保存当前登录' : '保存当前账号', {
        disabled: Boolean(account.busy), onClick: () => runAccount('account-capture'),
      }));
    }
    line.append(button('刷新账号列表', { className: 'btn btn--quiet btn--sm', onClick: refreshAccounts }));
    line.append(node('span', 'hint', '切换时优先保持 Work 打开，必要时正常重启。添加账号会打开官方登录页。凭据仅保存在本机系统钥匙串，不上传或写入日志。'));
    return line;
  }

  async function refreshAccounts() {
    try {
      const result = await request('/api/accounts');
      stateSet({ ...stateData(), accounts: result.accounts });
      renderAccountsAll();
    } catch (error) {
      notify(error.message || '刷新账号列表失败', 'error');
    }
  }

  function confirmRemove(row) {
    account.confirm = { kind: 'remove', id: textOf(row.id) };
    renderAccountsAll();
  }

  /* ---------- 预算保护 ---------- */

  const budget = { view: null, saving: false, form: null, errors: [] };
  const CURRENCIES = ['CNY', 'USD', 'EUR', 'JPY', 'HKD', 'GBP'];

  async function loadBudget() {
    try {
      const result = await request('/api/budget');
      if (result && result.budget) budget.view = result.budget;
    } catch (error) {
      budget.view = { status: { state: 'error', reason: error.code || 'request_failed' }, configured: null };
    }
    if (!budget.form) budget.form = budgetFormFrom(budget.view);
    renderBudget();
  }

  function budgetFormFrom(view) {
    const configured = view && view.configured && typeof view.configured === 'object' ? view.configured : null;
    const limitOf = key => {
      const row = configured && configured[key];
      return row && typeof row === 'object' ? { limit: row.limit === undefined || row.limit === null ? '' : String(row.limit), action: row.action === 'block' ? 'block' : 'remind' } : { limit: '', action: 'remind' };
    };
    return {
      enabled: Boolean(configured && configured.enabled),
      currency: textOf(configured && configured.currency) || 'CNY',
      timeZone: textOf(configured && configured.timeZone) || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
      serialize: Boolean(configured && configured.serialize),
      daily: limitOf('daily'),
      monthly: limitOf('monthly'),
      dirty: false,
    };
  }

  function renderBudget() {
    const slot = $('plus-budget-body');
    if (!slot) return;
    const view = budget.view;
    if (!view) { slot.replaceChildren(node('p', 'empty', '正在读取预算设置。')); return; }
    const status = view.status && typeof view.status === 'object' ? view.status : {};
    const wrap = node('div');
    wrap.append(renderBudgetStatus(status));
    wrap.append(renderBudgetForm());
    slot.replaceChildren(wrap);
  }

  function renderBudgetStatus(status) {
    const box = node('div', 'plus-budget-status');
    const enforcement = textOf(status.enforcement);
    if (!textOf(status.currency)) {
      box.append(node('p', 'hint', '还没有启用预算保护。这里的花费是按本机账本和自填定价估算的，不是供应商账单。'));
      return box;
    }
    if (enforcement === 'estimate_from_ledger') {
      box.append(node('p', 'plus-verdict__note', '保护依据是本机请求账本与模型定价的估算，不是供应商的硬上限；供应商侧仍可能按它自己的计费继续放行。'));
    }
    for (const name of ['daily', 'monthly']) {
      const row = status.spend && status.spend[name];
      if (!row || typeof row !== 'object') continue;
      const card = node('div', 'plus-spend');
      card.append(node('p', 'plus-window__label', name === 'daily' ? '今日' : '本月'));
      const line = node('div', 'plus-wallet__row');
      const spent = money(row.spent, status.currency);
      const limit = money(row.limit, status.currency);
      line.append(node('span', null, '已用'), node('strong', null, spent === null ? '未提供' : spent + (limit === null ? '' : ' / ' + limit)));
      card.append(line);

      const bits = [];
      const priced = integer(row.pricedRequests);
      const unpriced = integer(row.unpricedRequests);
      const inflight = integer(row.inflightRequests);
      if (priced !== null) bits.push(priced + ' 次已定价');
      if (unpriced) bits.push(unpriced + ' 次未定价（未计入金额）');
      if (inflight) bits.push(inflight + ' 次在途（可能超出）');
      if (row.partial === true) bits.push('金额不完整');
      if (row.reached === true) bits.push(textOf(row.action) === 'block' ? '已达上限，新请求会被拒绝' : '已达上限，仅提醒');
      const reset = time(row.resetsAt);
      if (reset) bits.push('重置于 ' + reset);
      if (bits.length) card.append(node('small', null, bits.join(' · ')));
      box.append(card);
    }
    if (textOf(status.warning) === 'unpriced_models_not_limited') {
      const unpricedModels = Array.isArray(status.unpricedModels) ? status.unpricedModels : [];
      box.append(node('p', 'note note--warn', '这些模型没有定价，新费用不会计入金额：' + (unpricedModels.join('、') || '未列出') + '。在模型编辑器里补充定价后才会计入。已记录费用仍参与预算判断。'));
    }
    if (Array.isArray(status.pricedModels) && status.pricedModels.length) {
      box.append(node('p', 'hint', '已定价、会被计入的模型：' + status.pricedModels.join('、')));
    }
    return box;
  }

  function renderBudgetForm() {
    const form = budget.form;
    const box = node('div', 'plus-budget-form');
    if (!form) return box;

    const enabled = node('label', 'check');
    const box1 = doc().createElement('input');
    box1.type = 'checkbox';
    box1.checked = form.enabled;
    box1.addEventListener('change', () => { form.enabled = box1.checked; form.dirty = true; renderBudget(); });
    enabled.append(box1, node('span', null, '启用预算保护'));

    const grid = node('div', 'field-row');
    grid.append(
      field('货币', selectField(CURRENCIES.map(code => [code, code]), form.currency, value => { form.currency = value; form.dirty = true; })),
      field('时区', textField(form.timeZone, value => { form.timeZone = value; form.dirty = true; }, 'Asia/Shanghai')),
    );
    for (const name of ['daily', 'monthly']) {
      const row = form[name];
      grid.append(
        field(name === 'daily' ? '每日上限（留空不限制）' : '每月上限（留空不限制）', textField(row.limit, value => { row.limit = value; form.dirty = true; }, '例如 20', 'number')),
        field('达到上限时', selectField([['remind', '只提醒'], ['block', '拒绝新请求']], row.action, value => { row.action = value; form.dirty = true; })),
      );
    }
    const serialize = node('label', 'check');
    const box2 = doc().createElement('input');
    box2.type = 'checkbox';
    box2.checked = form.serialize;
    box2.addEventListener('change', () => { form.serialize = box2.checked; form.dirty = true; renderBudget(); });
    serialize.append(box2, node('span', null, '串行发送请求'));
    const hint = node('p', 'field__hint', '开启后同一时间只发一个请求。它能把「在途请求让花费超过上限」的窗口关掉，代价是吞吐下降、响应变慢。');

    const line = node('div', 'action-line action-line--start');
    line.append(
      button('保存预算', { className: 'btn btn--primary btn--sm', onClick: saveBudget }),
      button('还原为已保存值', { className: 'btn btn--quiet btn--sm', onClick: () => { budget.form = budgetFormFrom(budget.view); budget.errors = []; renderBudget(); } }),
      button('清除预算设置', { className: 'btn btn--quiet btn--sm', onClick: clearBudget }),
    );
    line.append(node('span', 'draft', form.dirty ? '有未保存的修改' : '与已保存值一致'));

    const errors = node('p', 'errors');
    errors.hidden = !budget.errors.length;
    errors.replaceChildren(...budget.errors.map(message => node('p', null, message)));

    box.append(enabled, grid, serialize, hint, errors, line);
    return box;
  }

  function textField(value, onChange, placeholder, type = 'text') {
    const input = doc().createElement('input');
    input.type = type;
    if (type === 'number') { input.min = '0'; input.step = 'any'; input.inputMode = 'decimal'; }
    input.autocomplete = 'off';
    input.spellcheck = false;
    if (placeholder) input.placeholder = placeholder;
    input.value = value === undefined || value === null ? '' : String(value);
    input.addEventListener('input', () => onChange(input.value));
    return input;
  }

  function selectField(options, value, onChange) {
    const select = doc().createElement('select');
    for (const [key, label] of options) {
      const option = doc().createElement('option');
      option.value = key;
      option.textContent = label;
      select.append(option);
    }
    select.value = value;
    select.addEventListener('change', () => onChange(select.value));
    return select;
  }

  function field(label, control) {
    const wrap = node('label', 'field');
    wrap.append(node('span', 'field__label', label), control);
    return wrap;
  }

  async function saveBudget() {
    const form = budget.form;
    const view = budget.view;
    if (!form || budget.saving) return;
    const errors = [];
    const policy = { enabled: form.enabled, currency: form.currency, timeZone: form.timeZone.trim(), serialize: form.serialize };
    for (const name of ['daily', 'monthly']) {
      const row = form[name];
      const text = String(row.limit === '' ? '' : row.limit).trim();
      if (!text) continue;
      const value = Number(text);
      if (!Number.isFinite(value) || value <= 0 || value > 1e9 || Math.round(value * 1e6) === 0) {
        errors.push((name === 'daily' ? '每日' : '每月') + '上限必须大于 0，最多 10 亿且精确到小数点后 6 位，或留空。');
        continue;
      }
      policy[name] = { limit: value, action: row.action === 'block' ? 'block' : 'remind' };
    }
    if (!policy.timeZone) errors.push('请填写时区。');
    if (errors.length) { budget.errors = errors; renderBudget(); return; }

    budget.saving = true;
    budget.errors = [];
    try {
      const result = await request('/api/budget/save', { method: 'POST', body: {
        revision: view && view.revision !== undefined ? view.revision : null, budget: policy,
      } });
      await readSavedBudget(result);
      notify('预算设置已保存。', 'ok');
    } catch (error) {
      budget.errors = [error.message || '保存失败'];
      notify(error.message || '预算保存失败', 'error');
    } finally {
      budget.saving = false;
      renderBudget();
    }
  }

  async function readSavedBudget(result) {
    if (result.state) stateSet(result.state);
    const saved = await request('/api/budget');
    budget.view = saved.budget;
    budget.form = budgetFormFrom(budget.view);
  }

  async function clearBudget() {
    const view = budget.view;
    budget.saving = true;
    try {
      const result = await request('/api/budget/save', { method: 'POST', body: {
        revision: view && view.revision !== undefined ? view.revision : null, budget: null,
      } });
      await readSavedBudget(result);
      notify('预算设置已清除；不再限制新请求。', 'ok');
    } catch (error) {
      budget.errors = [error.message || '清除失败'];
      notify(error.message || '清除预算失败', 'error');
    } finally {
      budget.saving = false;
      renderBudget();
    }
  }

  /* ---------- Work 能力 ---------- */

  const capabilityWords = {
    inspected: '已检查内核', unverified: '未确认', unavailable: '不可用', unsupported: '当前版本不支持',
    'user-controlled': '由你在会话里开启',
  };

  const capabilityReason = {
    work_kernel_unverified: '没有读到 Work 内核文件，无法判断这项能力',
    work_tower_desktop_unavailable: '当前 Work 桌面版没有提供 Tower 入口',
    work_swarm_requires_conversation: 'Swarm 只在具体会话里开启',
    work_swarm_page_unavailable: '读不到当前 Work 页面，无法开关 Swarm',
    work_swarm_conversation_unavailable: '当前没有打开的会话',
    work_swarm_unsupported: '这个 Work 页面不支持 Swarm 接口',
    work_swarm_unavailable: '这台机器上没有可用的 Swarm 接口',
    work_swarm_not_confirmed: '写入后读回不一致，未确认生效',
    native_preview_unavailable: '这个面板版本还没有提供 Swarm 接口，需要更新的后端',
    request_failed: '读取失败，稍后重试',
  };

  const swarm = { snapshot: null, loading: false, reason: '', writing: false };

  function renderCapabilities(data) {
    const slot = $('plus-capabilities-body');
    if (!slot) return;
    const caps = data && data.workCapabilities;
    if (!caps || typeof caps !== 'object') {
      slot.replaceChildren(node('p', 'hint', '还没有读到 Work 能力信息。'));
      return;
    }
    const wrap = node('div');
    const head = node('div', 'plus-cap-head');
    head.append(node('strong', null, capabilityWords[textOf(caps.state)] || textOf(caps.state) || '未确认'));
    const bits = [];
    if (caps.kernelVersion) bits.push('内核 ' + caps.kernelVersion);
    if (caps.kernelCommit) bits.push('提交 ' + String(caps.kernelCommit).slice(0, 7));
    const force = caps.subagents && caps.subagents.forceKeySupported;
    if (force === false) bits.push('固定子代理模型：当前内核接受');

    if (bits.length) head.append(node('small', null, bits.join(' · ')));
    wrap.append(head);

    for (const [key, label] of [['swarm', 'Swarm 并行'], ['tower', 'Tower']]) {
      const entry = caps[key] && typeof caps[key] === 'object' ? caps[key] : {};
      const row = node('div', 'plus-cap-row');
      row.append(node('strong', null, label));
      row.append(node('small', null, (capabilityWords[textOf(entry.state)] || textOf(entry.state) || '未知')
        + (entry.reason ? ' · ' + (capabilityReason[entry.reason] || entry.reason) : '')));
      wrap.append(row);
    }
    wrap.append(renderSwarmControls(caps));
    slot.replaceChildren(wrap);
  }

  function renderSwarmControls(caps) {
    const box = node('div', 'plus-swarm');
    const entry = caps && caps.swarm && typeof caps.swarm === 'object' ? caps.swarm : {};
    box.append(node('p', 'plus-window__label', '当前会话的 Swarm'));
    const snapshot = swarm.snapshot;
    const known = snapshot && textOf(snapshot.conversationKey) && ['on', 'off'].includes(snapshot.state) && !snapshot.reason;
    if (known) {
      box.append(node('p', 'hint', '会话 ' + String(snapshot.conversationKey).slice(0, 12)
        + '：当前' + (snapshot.enabled ? '已开启' : '已关闭')
        + (snapshot.confirmed ? '，写入已确认' : '') + '。'));
    } else if (swarm.reason) {
      box.append(node('p', 'hint', capabilityReason[swarm.reason] || swarm.reason));
    } else if (entry.state === 'user-controlled') {
      box.append(node('p', 'hint', '读取当前会话后可以在这里显式开启或关闭。'));
    } else {
      box.append(node('p', 'hint', '无法判断当前会话的 Swarm 状态' + (entry.reason ? '：' + (capabilityReason[entry.reason] || entry.reason) : '') + '。'));
    }
    /* Swarm 只在用户显式点击时设置：不用页面默认值、不自动开启。 */
    const line = node('div', 'action-line action-line--start');
    line.append(button('读取当前会话', { onClick: () => loadSwarm() }));
    if (known) {
      line.append(button(snapshot.enabled ? '关闭这个会话的 Swarm' : '开启这个会话的 Swarm', {
        className: 'btn btn--primary btn--sm',
        onClick: () => setSwarm(!snapshot.enabled),
      }));
    }
    box.append(line);
    box.append(node('p', 'field__hint', 'Swarm 与普通子代理共用模型策略（[secondary_model]），不会单独保存另一套模型。'));
    box.append(node('p', 'field__hint', '开启需要你的账号有对应权限；没有权限时官方页面会拒绝，这里照实显示原因。'));
    return box;
  }

  async function loadSwarm() {
    if (swarm.loading) return;
    swarm.loading = true;
    swarm.reason = '';
    try {
      const result = await request('/api/work/swarm');
      const snapshot = result && (result.swarm || result);
      if (snapshot && typeof snapshot === 'object') {
        swarm.snapshot = snapshot;
        if (snapshot.reason) swarm.reason = snapshot.reason;
      }
    } catch (error) {
      swarm.snapshot = null;
      /* 运算符优先级要写清楚：先取到错误码，再按「这个端点还没有」降级。
         `error.code || error.status === 404 ? … : …` 会把错误码当成条件，
         任何有错误码的情况都被说成「接口不存在」。 */
      const code = textOf(error.code);
      swarm.reason = code ? code : error.status === 404 ? 'work_swarm_unavailable' : 'request_failed';
    } finally {
      swarm.loading = false;
    }
    renderCapabilities(stateData());
  }

  async function setSwarm(enabled) {
    if (swarm.writing) return;
    swarm.writing = true;
    try {
      const result = await request('/api/actions', { method: 'POST', body: { action: 'set-swarm', enabled } });
      const operation = result && result.operation ? result.operation : null;
      notify(enabled ? '已请求开启当前会话的 Swarm。' : '已请求关闭当前会话的 Swarm。', 'ok');
      if (operation && operation.id) {
        const deadline = Date.now() + 40000;
        while (Date.now() < deadline && ['running', 'pending'].includes(textOf(operation.state))) {
          await new Promise(resolve => setTimeout(resolve, 900));
          const id = operation.id;
          let data = null;
          try { data = await request('/api/state'); } catch { /* 继续等 */ }
          if (data) stateSet(data);
          const op = data && data.operation;
          if (!op || op.id !== id) break;
          if (!['running', 'pending'].includes(textOf(op.state))) {
            if (['failed', 'unavailable', 'interrupted'].includes(textOf(op.state))) {
              notify('Swarm 未生效：' + (capabilityReason[textOf(op.reason)] || textOf(op.reason) || textOf(op.state)), 'error');
            }
            break;
          }
        }
      }
      await loadSwarm();
    } catch (error) {
      notify(error.message || 'Swarm 操作失败', 'error');
    } finally {
      swarm.writing = false;
      renderCapabilities(stateData());
    }
  }

  /* ---------- 状态与挂载 ---------- */

  let stateGet = () => null;
  let stateSet = () => {};
  const stateData = () => stateGet();

  function mount(options) {
    request = options.request;
    notify = options.notify || notify;
    stateGet = options.getState || stateGet;
    stateSet = options.setState || stateSet;
    openAccounts = options.onAccounts || openAccounts;

    mountUsage();
    mountAccountBar();
    mountOverview();
    mountAccountsManagement();
    mountCapabilities();

    if (!$('plus-usage-panel') && !$('plus-account-bar')) return { render() {}, refresh() {} };
    return {
      render(data) {
        renderAccountsAll();
        if (data) renderCapabilities(data);
      },
      async refresh() {
        await Promise.all([loadCode(), loadMagpie(), loadBudget()]);
        renderCapabilities(stateData());
      },
    };
  }

  function mountUsage() {
    const host = $('usage-pane-quotas');
    const budgetHost = $('usage-pane-budget');
    if (!host || !budgetHost) return;
    const section = node('section', 'band plus-usage');
    section.id = 'plus-usage-panel';
    const head = node('div', 'band__head');
    head.append(node('div'));
    head.firstChild.append(node('h2', null, 'Kimi Code 与 Magpie 额度'));
    head.firstChild.append(node('p', null, '这两份读数来自不同账目：Kimi Code 是官方订阅窗口与加速包，Magpie 是本机网关自己记的余额。不跨来源求和，也不会把失败显示成 0。'));
    const refresh = button('刷新额度', { onClick: () => { void Promise.all([refreshCode(true), refreshMagpie()]); } });
    head.append(refresh);
    section.append(head);

    const codeBox = node('div', 'plus-block');
    codeBox.append(node('h3', null, 'Kimi Code'));
    const codeBody = node('div');
    codeBody.id = 'plus-code-body';
    codeBox.append(codeBody);
    section.append(codeBox);

    const magpieBox = node('div', 'plus-block');
    magpieBox.append(node('h3', null, 'Magpie'));
    const magpieBody = node('div');
    magpieBody.id = 'plus-magpie-body';
    magpieBox.append(magpieBody);
    section.append(magpieBox);

    host.insertBefore(section, host.firstChild);

    const budgetSection = node('section', 'band plus-budget');
    budgetSection.id = 'plus-budget-panel';
    const budgetHead = node('div', 'band__head');
    budgetHead.append(node('div'));
    budgetHead.firstChild.append(node('h2', null, '预算保护'));
    budgetHead.firstChild.append(node('p', null, '按本机请求账本与你在模型上填的定价估算花费，达到上限时提醒或拒绝新请求。这是本工具的估算，不是供应商的硬上限：供应商仍会按它自己的计费继续放行。'));
    budgetSection.append(budgetHead);
    const budgetBody = node('div');
    budgetBody.id = 'plus-budget-body';
    budgetSection.append(budgetBody);
    budgetHost.append(budgetSection);
  }

  function mountAccountBar() {
    const tools = doc().querySelector('.masthead__tools');
    if (!tools || $('plus-account-bar')) return;
    const bar = node('div', 'plus-account-bar');
    bar.id = 'plus-account-bar';
    tools.insertBefore(bar, tools.firstChild);
  }

  function mountOverview() {
    const host = $('view-overview');
    if (!host || $('plus-overview-account')) return;
    const card = node('section', 'plus-overview-card');
    card.append(node('p', 'plus-window__label', '当前账号'));
    const body = node('div');
    body.id = 'plus-overview-account';
    card.append(body);
    const anchor = $('onboard-panel');
    if (anchor && anchor.parentNode === host) host.insertBefore(card, anchor.nextSibling);
    else host.append(card);
  }

  function mountAccountsManagement() {
    const host = $('view-maintenance');
    if (!host || $('plus-accounts-panel')) return;
    const section = node('section', 'band plus-accounts-panel');
    section.id = 'plus-accounts-panel';
    const head = node('div', 'band__head');
    head.append(node('div'));
    head.firstChild.append(node('h2', null, '账号'));
    head.firstChild.append(node('p', null, '保存多个 Work 账号并按需切换。登录凭据只存在系统凭据存储；切换时优先保持 Work 打开，必要时正常重启。运行中的任务会阻止切换。'));
    section.append(head);
    const body = node('div');
    body.id = 'plus-accounts-body';
    section.append(body);
    host.insertBefore(section, host.firstChild);
  }

  function mountCapabilities() {
    const host = $('view-work');
    if (!host || $('plus-capabilities-panel')) return;
    const section = node('section', 'band plus-cap-panel');
    section.id = 'plus-capabilities-panel';
    const head = node('div', 'band__head');
    head.append(node('div'));
    head.firstChild.append(node('h2', null, 'Work 能力检测'));
    head.firstChild.append(node('p', null, '这里只报告从 Work 内核文件里读到的能力，以及当前会话能被读到的状态。读不到就说读不到，不猜。'));
    section.append(head);
    const body = node('div');
    body.id = 'plus-capabilities-body';
    section.append(body);
    host.insertBefore(section, host.firstChild ? host.firstChild.nextSibling : null);
  }

  globalThis.KimiWorkPlus = Object.freeze({ mount, loadBudget, loadCode, loadMagpie, loadSwarm });
})();

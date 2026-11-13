(() => {
  const $ = id => document.getElementById(id);
  const number = value => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const amount = value => typeof value === 'number' && Number.isFinite(value) ? value : null;
  const fmtAmount = value => amount(value) === null ? '未提供' : new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(value);
  const token = (row, name) => number(row?.[name + 'Tokens']) ?? number(row?.[name]);
  const text = (id, value) => { const node = $(id); if (node) node.textContent = value; };
  const hide = (id, value) => { const node = $(id); if (node) node.hidden = value; };
  const fmt = value => number(value) === null ? '未提供' : new Intl.NumberFormat('zh-CN').format(value);
  const time = value => {
    const date = new Date(value);
    return Number.isFinite(date.getTime()) ? new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(date) : '未知';
  };
  const kindNames = { subscription_window: '订阅配额', coding_plan: 'Coding Plan', api_balance: 'API 余额', gateway_limit: '外部网关额度' };
  const quotaReason = code => ({
    credential_missing: '凭据未配置', auth_rejected: '认证失败或已过期', rate_limited: '服务方限制了查询频率',
    upstream_error: '服务方查询失败', network_error: '网络连接失败', timeout: '查询超时',
    invalid_response: '返回格式与已确认接口不符', value_missing: '声明的数值不存在或不是数值',
    response_too_large: '返回内容超过允许大小', blocked_target: '目标地址已被安全规则拒绝',
    insecure_endpoint: '远程地址必须使用 HTTPS', duplicate_source: '这个来源已存在，请先删除旧来源或使用另一个标识',
    invalid_path: '请填写有效的 JSON 数值路径', invalid_window: '额度周期说明不能超过 40 个字符',
    inference_endpoint: '这是模型推理地址，请填写非推理的额度接口', unsupported_source: '当前来源尚未接入只读查询',
  })[code] || code;
  function quotaPathProblem(value, required = false) {
    const text = String(value || '').trim();
    if (!text) return required ? '请填写 JSON 数值路径，例如 data.balance。' : '';
    if (text.length > 200) return 'JSON 路径最长 200 个字符。';
    const segments = text.split('.');
    if (segments.length > 8) return 'JSON 路径最多包含 8 层。';
    if (segments.some(segment => !/^(?:[A-Za-z_][A-Za-z0-9_-]{0,63}|\d{1,3})$/.test(segment))) {
      return '路径使用点号分隔字段；字段可含字母、数字、下划线与连字符，数组下标使用数字。';
    }
    if (segments.some(segment => ['__proto__', 'prototype', 'constructor'].includes(segment))) {
      return 'JSON 路径不能包含保留字段名。';
    }
    return '';
  }

  const quotaStatus = record => record.stale ? '上次成功值 · 已过期'
    : record.status === 'unsupported' ? '暂不支持自动查询'
      : record.status === 'error' ? '查询失败'
        : record.status === 'not_configured' ? '凭据未配置'
          : record.status === 'not_read' ? '尚未查询' : '最近一次查询成功';
  const state = { tab: 'requests', range: 'today', filters: {}, result: null, quota: null, loading: false, bucket: null, clearArmed: false };
  let request;
  let notify;
  let clearTimer;

  function template() {
    $('usage-root').innerHTML = `
      <div class="usage-tabs" role="tablist" aria-label="用量中心">
        <button id="usage-tab-requests" type="button" role="tab" data-usage-tab="requests" aria-selected="true">请求消耗</button>
        <button id="usage-tab-quotas" type="button" role="tab" data-usage-tab="quotas" aria-selected="false">额度总览</button>
        <button id="usage-tab-sources" type="button" role="tab" data-usage-tab="sources" aria-selected="false">账户与数据来源</button>
      </div>
      <p class="errors usage-error" id="usage-error" role="alert" hidden></p>
      <section class="usage-pane" id="usage-pane-requests" role="tabpanel" aria-labelledby="usage-tab-requests">
        <div class="usage-ranges" role="group" aria-label="统计时间">
          <button class="btn btn--outline btn--sm" type="button" data-usage-range="today" aria-pressed="true">今天 · 逐小时</button>
          <button class="btn btn--outline btn--sm" type="button" data-usage-range="7d" aria-pressed="false">最近 7 天 · 逐日</button>
          <button class="btn btn--outline btn--sm" type="button" data-usage-range="30d" aria-pressed="false">最近 30 天 · 逐日</button>
          <button class="btn btn--outline btn--sm" type="button" data-usage-range="custom" aria-pressed="false">自定义时间</button>
        </div>
        <form class="usage-custom" id="usage-custom-form" hidden>
          <label class="field" for="usage-from"><span class="field__label">开始日期</span><input id="usage-from" type="date" required></label>
          <label class="field" for="usage-to"><span class="field__label">结束日期（含当天）</span><input id="usage-to" type="date" required></label>
          <button class="btn btn--outline" type="submit">应用时间范围</button>
        </form>
        <div class="usage-toolbar">
          <label class="field" for="usage-filter-provider"><span class="field__label">服务商</span><select id="usage-filter-provider" data-usage-filter="providerId"><option value="">全部服务商</option></select></label>
          <label class="field" for="usage-filter-connection"><span class="field__label">连接</span><select id="usage-filter-connection" data-usage-filter="connectionId"><option value="">全部连接</option></select></label>
          <div class="field"><span class="field__label" id="usage-model-label">模型</span><details class="usage-picker" id="usage-filter-model"><summary aria-labelledby="usage-model-label usage-model-current"><span id="usage-model-current">全部</span></summary><div class="usage-picker__options" id="usage-model-options" role="group" aria-label="可用模型"></div></details></div>
          <label class="field" for="usage-filter-account"><span class="field__label">账户标识</span><select id="usage-filter-account" data-usage-filter="account"><option value="">全部已识别账户</option></select><span class="field__hint">仅显示不可逆标识，不显示 API Key。</span></label>
          <label class="field" for="usage-filter-conversation"><span class="field__label">会话</span><select id="usage-filter-conversation" data-usage-filter="conversationId"><option value="">全部会话</option></select><span class="field__hint" id="usage-conversation-hint">只使用有可靠来源的会话 ID。</span></label>
        </div>
        <p class="note usage-coverage" id="usage-coverage">仅统计经过本机网关的可观察请求；官方直连和接入前历史不会补算。</p>
        <dl class="usage-summary" id="usage-summary">
          <div class="usage-stat"><dt>输入 Token</dt><dd id="usage-stat-input">—</dd></div>
          <div class="usage-stat"><dt>输出 Token</dt><dd id="usage-stat-output">—</dd></div>
          <div class="usage-stat"><dt>总 Token</dt><dd id="usage-stat-total">—</dd><small>仅汇总上游明确报告的数值</small></div>
          <div class="usage-stat"><dt>缓存读取</dt><dd id="usage-stat-cache-read">—</dd></div>
          <div class="usage-stat"><dt>缓存写入</dt><dd id="usage-stat-cache-write">—</dd></div>
          <div class="usage-stat"><dt>请求次数</dt><dd id="usage-stat-requests">—</dd><small id="usage-stat-observed">含未返回 usage 的请求</small></div>
          <div class="usage-stat"><dt>错误次数</dt><dd id="usage-stat-errors">—</dd></div>
          <div class="usage-stat"><dt>估算费用</dt><dd id="usage-stat-cost">未提供</dd><small>不是官方实际账单</small></div>
        </dl>
        <figure class="usage-chart" id="usage-chart">
          <div class="usage-chart__legend"><span>输入 Token</span><span>输出 Token</span></div>
          <div id="usage-chart-body"><p class="usage-chart__empty">正在读取所选时间段。</p></div>
          <figcaption class="hint">选择数据点可查看该时间段明细；空缺表示没有可确认的 Token 数值。</figcaption>
        </figure>
        <section class="usage-detail" id="usage-bucket-detail" hidden><h3 class="usage-detail__title" id="usage-bucket-title"></h3><div id="usage-bucket-records"></div></section>
        <section class="usage-detail" aria-labelledby="usage-records-title">
          <div class="band__head"><div><h3 class="usage-detail__title" id="usage-records-title">请求明细</h3><p>按时间倒序，不保存或展示 prompt、响应正文和 API Key。</p></div><div><button class="btn btn--outline btn--sm" id="usage-export" type="button">导出脱敏 JSON</button><button class="btn btn--quiet btn--sm" id="usage-clear" type="button">清除历史</button></div></div>
          <p class="empty" id="usage-records-empty">该时间段尚无可观察请求。</p>
          <p class="hint" id="usage-records-limit" role="status"></p>
          <div class="table-wrap" id="usage-records-body"></div>
        </section>
      </section>
      <section class="usage-pane" id="usage-pane-quotas" role="tabpanel" aria-labelledby="usage-tab-quotas" hidden>
        <div class="band__head"><div><h2>额度总览</h2><p>订阅窗口、Coding Plan、API 余额和外部网关自设额度是不同账目，不跨来源求和。</p></div><button class="btn btn--outline" id="quota-refresh" type="button">刷新已接入来源</button></div>
        <p class="note">只有点击刷新才会查询已授权的非推理接口。失败时保留最后成功值并标记过期，不把失败显示为 0 或无限额。</p>
        <div class="usage-quota-list" id="quota-list"></div><p class="empty" id="quota-empty">尚未接入可查询的额度来源。这不表示余额为 0。</p>
      </section>
      <section class="usage-pane" id="usage-pane-sources" role="tabpanel" aria-labelledby="usage-tab-sources" hidden>
        <div class="band__head"><div><h2>账户与数据来源</h2><p>每个来源单独标明数据类型、凭据是否就绪、最后成功时间和失败原因。不会读取浏览器 Cookie。</p></div><button class="btn btn--outline" id="quota-add" type="button">添加余额来源</button></div>
        <div id="quota-source-editor"></div>
        <div class="usage-source-list" id="quota-source-list"></div>
      </section>`;
  }

  function showError(error) {
    const node = $('usage-error');
    node.hidden = !error;
    node.textContent = error ? (error.message || String(error)) : '';
  }

  function queryPayload() {
    const payload = { preset: state.range, filters: { ...state.filters },
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone };
    if (state.range === 'custom') {
      const from = $('usage-from').value;
      const to = $('usage-to').value;
      if (!from || !to || from > to) throw Error('请选择有效的开始和结束日期。');
      payload.from = from;
      payload.to = to;
    }
    return payload;
  }

  async function loadUsage() {
    const sequence = ++loadUsage.sequence;
    state.loading = true;
    showError(null);
    try {
      const result = await request('/api/usage/query', { method: 'POST', body: queryPayload() });
      if (sequence !== loadUsage.sequence) return;
      state.result = result;
      state.bucket = null;
      renderUsage();
    } catch (error) { if (sequence === loadUsage.sequence) showError(error); }
    finally { if (sequence === loadUsage.sequence) state.loading = false; }
  }
  loadUsage.sequence = 0;

  async function loadQuota() {
    try {
      state.quota = await request('/api/quota');
      renderQuota();
    } catch (error) { showError(error); }
  }

  async function reload() {
    await Promise.all([loadUsage(), loadQuota()]);
  }

  function selectTab(tab) {
    state.tab = tab;
    document.querySelectorAll('[data-usage-tab]').forEach(button => button.setAttribute('aria-selected', String(button.dataset.usageTab === tab)));
    for (const name of ['requests', 'quotas', 'sources']) hide('usage-pane-' + name, name !== tab);
    if (tab !== 'requests' && !state.quota) void loadQuota();
  }

  const filterViews = {
    providerId: { key: 'providers', id: 'provider' }, connectionId: { key: 'connections', id: 'connection' },
    modelId: { key: 'models', id: 'model' }, conversationId: { key: 'conversations', id: 'conversation' },
    account: { key: 'accounts', id: 'account' },
  };

  function optionsFor(name) {
    const values = state.result?.filters?.[filterViews[name].key] ?? [];
    return Array.isArray(values) ? values : [];
  }

  function renderFilters() {
    for (const name of Object.keys(filterViews)) {
      const select = $('usage-filter-' + filterViews[name].id);
      const current = state.filters[name] || '';
      const rows = optionsFor(name);
      const values = rows.map(row => typeof row === 'string' ? row : row.value ?? row.id);
      if (name === 'modelId') {
        const root = $('usage-model-options'); root.replaceChildren();
        const options = [{ value: '', label: '全部' }, ...rows.map(row => typeof row === 'string' ? { value: row, label: row } : { value: row.value ?? row.id, label: row.label ?? row.name ?? row.value ?? row.id })];
        if (current && !values.includes(current)) options.push({ value: current, label: current + '（当前范围无记录）' });
        for (const option of options) {
          const button = document.createElement('button'); button.type = 'button'; button.textContent = option.label;
          button.dataset.modelValue = option.value; button.setAttribute('aria-pressed', String(option.value === current));
          button.addEventListener('click', () => { applyFilter(name, option.value); select.open = false; select.querySelector('summary').focus(); });
          root.append(button);
        }
        text('usage-model-current', options.find(option => option.value === current)?.label || current);
        continue;
      }
      select.replaceChildren();
      const all = document.createElement('option');
      all.value = '';
      all.textContent = name === 'conversationId' && !rows.length ? '无法关联会话' : '全部';
      select.append(all);
      for (const row of rows) {
        const option = document.createElement('option');
        option.value = typeof row === 'string' ? row : row.value ?? row.id;
        option.textContent = typeof row === 'string' ? row : row.label ?? row.name ?? option.value;
        select.append(option);
      }
      if (current && !values.includes(current)) {
        const kept = document.createElement('option'); kept.value = current; kept.textContent = current + '（当前范围无记录）'; select.append(kept);
      }
      select.value = current;
      select.disabled = name === 'conversationId' && !rows.length;
    }
    text('usage-conversation-hint', optionsFor('conversationId').length ? '仅列出账本中具有可信会话 ID 的请求。' : '当前记录没有可靠会话 ID，不按模型或时间猜测归属。');
  }

  function renderUsage() {
    const result = state.result;
    if (!result) return;
    renderFilters();
    const summary = result.summary || {};
    for (const [name, id] of [['input', 'input'], ['output', 'output'], ['total', 'total'], ['cacheRead', 'cache-read'], ['cacheWrite', 'cache-write']]) {
      text('usage-stat-' + id, fmt(token(summary, name)));
    }
    const requests = summary.requests ?? summary.requestCount;
    const observed = summary.usageRequests ?? summary.requestsWithUsage ?? summary.observedRequests;
    text('usage-stat-requests', fmt(number(requests) ?? 0));
    text('usage-stat-observed', number(observed) === null ? '有 usage 的请求数未知' : fmt(observed) + ' 笔有实际上游 usage');
    text('usage-stat-errors', fmt(number(summary.errors ?? summary.errorCount) ?? 0));
    text('usage-stat-cost', costText(summary.estimatedCost, summary.currency));
    const coverage = result.coverage || {};
    text('usage-coverage', '仅统计经过本机网关的可观察请求；官方直连和接入前历史不会补算。'
      + (coverage.recordingSince ? ' 账本最早记录：' + time(coverage.recordingSince) + '。' : '')
      + (['unavailable', 'write_failed', 'unsafe_file'].includes(coverage.state) ? ' 持久化当前不可用，重启后可能无法保留。'
        : coverage.state === 'corrupt' ? ' 账本中有无法读取的行，其余记录仍保留。' : ''));
    renderChart(result.buckets || []);
    renderRecords(result.records || [], 'usage-records-body');
    const rows = result.records || [];
    hide('usage-records-empty', rows.length > 0);
    text('usage-records-limit', result.recordsTruncated ? '明细仅显示最近 ' + fmt(rows.length) + ' 笔，共 ' + fmt(result.recordsTotal) + ' 笔；汇总和趋势包含该范围的全部记录。' : '');
  }

  function costText(value, currency) {
    if (value && typeof value === 'object') return Number.isFinite(value.amount) ? new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(value.amount) + ' ' + (value.currency || currency || '') : '未提供';
    return Number.isFinite(value) ? new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(value) + ' ' + (currency || '') : '未提供';
  }

  function bucketLabel(bucket) {
    return bucket.label || time(bucket.start || bucket.startAt || bucket.at);
  }

  function renderChart(buckets) {
    const root = $('usage-chart-body');
    root.replaceChildren();
    if (!buckets.some(row => row.requests > 0)) { const p = document.createElement('p'); p.className = 'usage-chart__empty'; p.textContent = '该时间段尚无可观察请求。'; root.append(p); return; }
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('viewBox', '0 0 800 220');
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', '输入与输出 Token 趋势图');
    const values = buckets.flatMap(row => [token(row, 'input'), token(row, 'output')]).filter(value => value !== null);
    const max = Math.max(1, ...values);
    const x = index => buckets.length === 1 ? 400 : 52 + index * (720 / (buckets.length - 1));
    const y = value => 184 - value / max * 150;
    for (const [label, value] of [[fmt(max), 34], [fmt(Math.round(max / 2)), 109], ['0', 184]]) {
      const line = document.createElementNS(ns, 'line'); line.setAttribute('x1', '52'); line.setAttribute('x2', '780'); line.setAttribute('y1', String(value)); line.setAttribute('y2', String(value)); line.setAttribute('class', 'usage-chart__grid'); svg.append(line);
      const node = document.createElementNS(ns, 'text'); node.setAttribute('x', '46'); node.setAttribute('y', String(value + 3)); node.setAttribute('text-anchor', 'end'); node.setAttribute('class', 'usage-chart__label'); node.textContent = label; svg.append(node);
    }
    for (const [name, className] of [['input', 'usage-chart__line'], ['output', 'usage-chart__line usage-chart__line--output']]) {
      let segment = [];
      const flush = () => { if (segment.length > 1) { const path = document.createElementNS(ns, 'path'); path.setAttribute('d', segment.map((point, index) => (index ? 'L' : 'M') + point.join(',')).join(' ')); path.setAttribute('class', className); svg.append(path); } segment = []; };
      buckets.forEach((row, index) => { const value = token(row, name); if (value === null) flush(); else segment.push([x(index).toFixed(1), y(value).toFixed(1)]); });
      flush();
    }
    const labelEvery = Math.max(1, Math.ceil(buckets.length / 6));
    buckets.forEach((row, index) => {
      if (index % labelEvery === 0 || index === buckets.length - 1) { const label = document.createElementNS(ns, 'text'); label.setAttribute('x', String(x(index))); label.setAttribute('y', '207'); label.setAttribute('text-anchor', 'middle'); label.setAttribute('class', 'usage-chart__label'); label.textContent = bucketLabel(row); svg.append(label); }
      const pointValue = token(row, 'input') ?? token(row, 'output');
      if (pointValue === null) return;
      const point = document.createElementNS(ns, 'circle');
      point.setAttribute('cx', String(x(index))); point.setAttribute('cy', String(y(pointValue))); point.setAttribute('r', '4'); point.setAttribute('class', 'usage-chart__point'); point.setAttribute('tabindex', '0'); point.setAttribute('role', 'button'); point.setAttribute('aria-label', bucketLabel(row) + '，输入 ' + fmt(token(row, 'input')) + '，输出 ' + fmt(token(row, 'output')));
      const title = document.createElementNS(ns, 'title'); title.textContent = bucketLabel(row) + ' · 输入 ' + fmt(token(row, 'input')) + ' · 输出 ' + fmt(token(row, 'output')); point.append(title);
      const choose = () => selectBucket(row);
      point.addEventListener('click', choose); point.addEventListener('keydown', event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); choose(); } });
      svg.append(point);
    });
    root.append(svg);
  }

  function selectBucket(bucket) {
    state.bucket = bucket;
    text('usage-bucket-title', bucketLabel(bucket) + ' 的请求');
    const start = new Date(bucket.start || bucket.startAt || bucket.at).getTime();
    const end = new Date(bucket.end || bucket.endAt || 0).getTime();
    const rows = (state.result?.records || []).filter(row => {
      const at = new Date(row.at || row.timestamp || row.completedAt).getTime();
      return !Number.isFinite(start) || !Number.isFinite(end) || at >= start && at < end;
    });
    renderRecords(rows, 'usage-bucket-records', true);
    hide('usage-bucket-detail', false);
  }

  const statusText = record => ({ started: '进行中', completed: '已完成', incomplete: '未完成', failed: '失败', cancelled: '已取消' })[record.status] || record.status || '未知';
  function cell(row, value, className = '') { const node = document.createElement('td'); if (className) node.className = className; node.textContent = value; row.append(node); }
  function renderRecords(records, target, compact = false) {
    const body = $(target); body.replaceChildren();
    if (!records.length) { const p = document.createElement('p'); p.className = 'empty'; p.textContent = '该时间段尚无可观察请求。'; body.append(p); return; }
    const table = document.createElement('table'); table.className = 'table usage-table';
    const head = document.createElement('thead');
    const tr = document.createElement('tr');
    const labels = compact ? ['时间', '模型', '输入', '输出', '状态']
      : ['时间', '来源 / 连接', '模型', '输入', '输出', '缓存读取', '缓存写入', '耗时', '状态', '估算费用'];
    for (const label of labels) { const th = document.createElement('th'); th.textContent = label; tr.append(th); }
    head.append(tr); table.append(head);
    const tbody = document.createElement('tbody');
    for (const record of records) {
      const row = document.createElement('tr');
      if (compact) {
        cell(row, time(record.at || record.timestamp || record.completedAt)); cell(row, record.modelId || record.model || '未知'); cell(row, fmt(token(record, 'input')), 'num'); cell(row, fmt(token(record, 'output')), 'num'); cell(row, statusText(record));
      } else {
        cell(row, time(record.at || record.timestamp || record.completedAt));
        cell(row, [record.providerId, record.connectionId].filter(Boolean).join(' / ') || record.source || '本机网关');
        cell(row, record.modelId || record.model || '未知');
        cell(row, fmt(token(record, 'input')), 'num'); cell(row, fmt(token(record, 'output')), 'num');
        cell(row, fmt(token(record, 'cacheRead') ?? number(record.cachedInputTokens)), 'num'); cell(row, fmt(token(record, 'cacheWrite')), 'num');
        cell(row, Number.isFinite(record.latencyMs ?? record.ms) ? fmt(record.latencyMs ?? record.ms) + ' ms' : '未提供', 'num');
        cell(row, record.error || record.failure ? '失败' : statusText(record));
        cell(row, costText(record.estimatedCost, record.currency));
      }
      tbody.append(row);
    }
    table.append(tbody); body.append(table);
  }

  function quotaRows() {
    if (Array.isArray(state.quota?.records)) return state.quota.records;
    if (Array.isArray(state.quota?.quotas)) return state.quota.quotas;
    return [];
  }

  function renderQuota() {
    const records = quotaRows();
    const list = $('quota-list'); list.replaceChildren();
    for (const record of records) {
      const row = document.createElement('article'); row.className = 'quota-row';
      const main = document.createElement('div'); const title = document.createElement('h3'); title.textContent = record.sourceName || record.sourceId || '未命名来源'; main.append(title);
      const kind = document.createElement('p'); kind.textContent = (kindNames[record.kind] || '额度来源') + (record.windowLabel ? ' · ' + record.windowLabel : ''); main.append(kind);
      const status = document.createElement('small'); status.textContent = quotaStatus(record); main.append(status); row.append(main);
      const identity = document.createElement('p'); identity.textContent = '账户：' + (record.accountRefMasked || '未确认'); main.append(identity);
      const value = document.createElement('div'); value.className = 'quota-row__value';
      value.textContent = amount(record.remaining) !== null ? fmtAmount(record.remaining) + ' ' + (record.unit || '') : amount(record.used) !== null ? '已用 ' + fmtAmount(record.used) + ' ' + (record.unit || '') : amount(record.limit) !== null ? '限额 ' + fmtAmount(record.limit) + ' ' + (record.unit || '') : record.status === 'unsupported' ? '不可查询' : '未提供';
      row.append(value);
      const meta = document.createElement('div'); meta.className = 'quota-row__meta';
      const updated = document.createElement('p'); updated.textContent = '最后成功：' + (record.lastSuccessfulAt ? time(record.lastSuccessfulAt) : '尚无'); meta.append(updated);
      const reset = document.createElement('p'); reset.textContent = record.resetAt ? '重置时间：' + time(record.resetAt) : '重置时间：未提供'; meta.append(reset);
      for (const part of record.parts || []) { const detail = document.createElement('p'); detail.textContent = part.label + '：' + fmtAmount(part.amount) + ' ' + (part.unit || record.unit || ''); meta.append(detail); }
      if (record.stale) { const age = document.createElement('p'); age.textContent = '当前读数已过期，请手动刷新；上次值仍保留。'; meta.append(age); }
      if (record.errorReason) { const reason = document.createElement('p'); reason.textContent = '原因：' + quotaReason(record.errorReason); meta.append(reason); }
      if (record.officialUrl) { const link = document.createElement('a'); link.href = record.officialUrl; link.target = '_blank'; link.rel = 'noreferrer'; link.textContent = '打开官方额度页'; meta.append(link); }
      row.append(meta); list.append(row);
      if (amount(record.limit) > 0 && amount(record.remaining) !== null) { const bar = document.createElement('div'); bar.className = 'quota-progress'; const fill = document.createElement('span'); fill.style.width = Math.max(0, Math.min(100, record.remaining / record.limit * 100)) + '%'; bar.append(fill); main.append(bar); }
    }
    hide('quota-empty', records.length > 0);
    renderSources();
  }

  function renderSourceEditor() {
    const root = $('quota-source-editor');
    root.innerHTML = `<form class="usage-source-form" id="quota-source-form">
      <div class="band__head"><div><h3>添加余额或外部网关额度来源</h3><p>这里只保存非敏感配置。认证复用连接中已有的 Key，页面不会回显或另存 Key；保存后不会自动查询。</p></div></div>
      <label class="field" for="quota-source-adapter"><span class="field__label">厂商适配器</span><select id="quota-source-adapter"><option value="deepseek">DeepSeek 官方余额</option><option value="moonshot-cn">Moonshot 国内余额</option><option value="moonshot-global">Moonshot 国际余额</option><option value="custom">高级：自定义非推理接口</option></select><span class="field__hint">官方适配器使用固定文档端点；自定义接口只读取声明的数值路径。</span></label>
      <div class="field-row"><label class="field" for="quota-source-name"><span class="field__label">显示名称</span><input id="quota-source-name" maxlength="80" required placeholder="例如：DeepSeek 余额"></label><label class="field" for="quota-source-id" id="quota-source-id-field"><span class="field__label">来源标识</span><input id="quota-source-id" class="mono" required pattern="[A-Za-z0-9][A-Za-z0-9._-]{0,63}" placeholder="my-balance"><span class="field__hint">自定义来源使用；官方来源按连接自动生成。</span></label></div>
      <label class="field" for="quota-source-connection"><span class="field__label">复用连接凭据</span><select id="quota-source-connection"></select><span class="field__hint">没有可用连接时，先到「连接」保存并应用。</span></label>
      <details class="growth-disclosure" id="quota-adapter-details"><summary id="quota-adapter-summary">查看官方接口与适配规则</summary><div class="growth-disclosure__body">
      <p class="note" id="quota-custom-note">请使用服务方文档提供的余额或额度接口；此处只查询数据，不调用模型。</p>
      <div class="field-row"><label class="field" for="quota-source-kind"><span class="field__label">账目类型</span><select id="quota-source-kind"><option value="api_balance">API 余额</option><option value="gateway_limit">外部网关自设额度</option><option value="coding_plan">外部网关 Coding Plan 窗口</option></select></label><label class="field" for="quota-source-unit"><span class="field__label">数值单位</span><select id="quota-source-unit"><option>CNY</option><option>USD</option><option>EUR</option><option>JPY</option><option>HKD</option><option>GBP</option><option>credits</option><option>requests</option><option>tokens</option></select><span class="field__hint">必须与服务方返回单位一致。</span></label></div>
      <label class="field" for="quota-source-url"><span class="field__label">非推理余额查询地址</span><input id="quota-source-url" type="url" required placeholder="https://gateway.example/api/user/balance"><span class="field__hint">只接受明确的额度接口，不能填写模型、工具、图片或长上下文地址。</span></label>
      <div class="field-row"><label class="field" for="quota-source-method"><span class="field__label">查询方法</span><select id="quota-source-method"><option value="GET">GET</option><option value="POST">POST（静态非推理请求）</option></select></label><label class="field" for="quota-source-path"><span class="field__label">JSON 数值路径</span><input id="quota-source-path" class="mono" required placeholder="data.balance"><span class="field__hint" id="quota-source-path-hint">例如 data.balance 或 balance_infos.0.total_balance。</span></label></div>
      <label class="field" for="quota-source-auth"><span class="field__label">认证方式</span><select id="quota-source-auth"><option value="bearer">复用连接 Key · Bearer</option><option value="x-api-key">复用连接 Key · X-API-Key</option></select></label>
      <div id="quota-custom-window" class="field-row"><label class="field" for="quota-source-window"><span class="field__label">额度周期（可选）</span><input id="quota-source-window" maxlength="40" placeholder="例如：5 小时 / 每月"></label><label class="field" for="quota-source-reset"><span class="field__label">重置时间 JSON 路径（可选）</span><input id="quota-source-reset" class="mono" placeholder="data.reset_at"><span class="field__hint" id="quota-source-reset-hint">服务方提供的 ISO 时间或 Unix 时间戳。</span></label></div>
      <div id="quota-custom-fields" class="field-row"><label class="field" for="quota-source-used"><span class="field__label">已用数值 JSON 路径（可选）</span><input id="quota-source-used" class="mono" placeholder="data.used"><span class="field__hint" id="quota-source-used-hint">留空表示不读取这个数值。</span></label><label class="field" for="quota-source-limit"><span class="field__label">总额度 JSON 路径（可选）</span><input id="quota-source-limit" class="mono" placeholder="data.limit"><span class="field__hint" id="quota-source-limit-hint">留空表示不读取这个数值。</span></label></div>
      <label class="field" for="quota-source-body" id="quota-source-body-field" hidden><span class="field__label">POST 静态 JSON</span><textarea id="quota-source-body" class="mono" rows="3">{}</textarea><span class="field__hint">不能包含 prompt、model、messages、工具或图片等推理字段。</span></label>
      </div></details>
      <p class="errors" id="quota-source-error" hidden></p><div class="action-line"><button class="btn btn--quiet" id="quota-source-cancel" type="button">取消</button><button class="btn btn--primary" type="submit">保存数据来源</button></div>
    </form>`;
    const connections = Array.isArray(state.quota?.connections) ? state.quota.connections : [];
    const select = $('quota-source-connection');
    for (const connection of connections) {
      const option = document.createElement('option'); option.value = connection.id; option.textContent = connection.name || connection.id;
      select.append(option);
    }
    if (!connections.length) { const option = document.createElement('option'); option.value = ''; option.textContent = '暂无已生效连接'; select.append(option); }
    const applyAdapter = () => {
      const adapter = $('quota-source-adapter').value;
      const preset = {
        deepseek: { name: 'DeepSeek 余额', url: 'https://api.deepseek.com/user/balance', path: 'balance_infos.0.total_balance', unit: 'CNY' },
        'moonshot-cn': { name: 'Moonshot 国内余额', url: 'https://api.moonshot.cn/v1/users/me/balance', path: 'data.available_balance', unit: 'CNY' },
        'moonshot-global': { name: 'Moonshot 国际余额', url: 'https://api.moonshot.ai/v1/users/me/balance', path: 'data.available_balance', unit: 'USD' },
      }[adapter];
      hide('quota-custom-window', Boolean(preset)); hide('quota-custom-fields', Boolean(preset)); hide('quota-custom-note', Boolean(preset));
      hide('quota-source-id-field', Boolean(preset));
      $('quota-adapter-details').open = !preset;
      text('quota-adapter-summary', preset ? '查看官方接口与适配规则' : '自定义接口与 JSON 路径');
      for (const id of ['quota-source-kind', 'quota-source-unit', 'quota-source-method', 'quota-source-auth']) $(id).disabled = Boolean(preset);
      for (const id of ['quota-source-url', 'quota-source-path']) $(id).readOnly = Boolean(preset);
      if (!preset) return;
      $('quota-source-id').value = adapter;
      $('quota-source-name').value = preset.name;
      $('quota-source-kind').value = 'api_balance';
      $('quota-source-unit').value = preset.unit;
      $('quota-source-url').value = preset.url;
      $('quota-source-method').value = 'GET';
      $('quota-source-path').value = preset.path;
      $('quota-source-auth').value = 'bearer';
      hide('quota-source-body-field', true);
    };
    $('quota-source-adapter').addEventListener('change', applyAdapter);
    applyAdapter();
    for (const [id, required] of [['quota-source-path', true], ['quota-source-reset', false], ['quota-source-used', false], ['quota-source-limit', false]]) {
      const input = $(id);
      const hint = $(id + '-hint');
      const original = hint.textContent;
      input.addEventListener('input', () => {
        const problem = quotaPathProblem(input.value, required);
        input.setAttribute('aria-invalid', problem ? 'true' : 'false');
        input.setCustomValidity(problem);
        hint.textContent = problem || original;
      });
    }
    $('quota-source-method').addEventListener('change', () => hide('quota-source-body-field', $('quota-source-method').value !== 'POST'));
    $('quota-source-cancel').addEventListener('click', () => root.replaceChildren());
    $('quota-source-form').addEventListener('submit', async event => {
      event.preventDefault();
      const error = $('quota-source-error'); error.hidden = true;
      let body;
      try { body = $('quota-source-method').value === 'POST' ? JSON.parse($('quota-source-body').value || '{}') : undefined; }
      catch { error.textContent = 'POST 内容必须是有效 JSON。'; error.hidden = false; return; }
      const source = {
        adapter: $('quota-source-adapter').value,
        sourceId: $('quota-source-id').value.trim(), sourceName: $('quota-source-name').value.trim(), kind: $('quota-source-kind').value,
        windowLabel: $('quota-source-window').value.trim(), resetAtPath: $('quota-source-reset').value.trim(),
        usedPath: $('quota-source-used').value.trim(), limitPath: $('quota-source-limit').value.trim(),
        endpoint: $('quota-source-url').value.trim(), method: $('quota-source-method').value, valuePath: $('quota-source-path').value.trim(), unit: $('quota-source-unit').value.trim(),
        auth: { type: $('quota-source-auth').value, ...(select.disabled ? {} : { connectionId: select.value }) },
        ...(body === undefined ? {} : { body }),
      };
      if (source.adapter !== 'custom') {
        const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(source.auth.connectionId));
        source.sourceId = source.adapter + '-' + Array.from(new Uint8Array(bytes), value => value.toString(16).padStart(2, '0')).join('').slice(0, 12);
      }
      const submit = event.target.querySelector('[type="submit"]'); submit.disabled = true;
      try {
        state.quota = await request('/api/quota/sources', { method: 'POST', body: { action: 'upsert', source } });
        root.replaceChildren(); renderQuota(); notify('数据来源已保存，尚未发起额度查询。', 'ok');
      } catch (failure) { error.textContent = quotaReason(failure.code) || failure.message || '来源未保存。'; error.hidden = false; }
      finally { submit.disabled = false; }
    });
  }

  function renderSources() {
    const root = $('quota-source-list'); root.replaceChildren();
    const sources = Array.isArray(state.quota?.sources) ? state.quota.sources : [];
    if (!sources.length) { const p = document.createElement('p'); p.className = 'empty'; p.textContent = '没有已配置的数据来源。Kimi Code 订阅和第三方 Coding Plan 只有在存在受支持的只读接口时才会显示实值。'; root.append(p); return; }
    for (const source of sources) {
      const row = document.createElement('article'); row.className = 'usage-source';
      const title = document.createElement('h3'); title.textContent = source.sourceName || source.name || source.sourceId || '数据来源'; row.append(title);
      const desc = document.createElement('p'); desc.textContent = (kindNames[source.kind] || '数据来源') + ' · ' + (source.accountRefMasked || source.providerId || '账户未识别'); row.append(desc);
      const stateText = document.createElement('p'); stateText.textContent = source.status === 'unsupported' || source.supported === false ? '暂不支持自动查询，不会通过模型推理估算。' : source.credentialSet === false || source.credentials === 'missing' ? '凭据未就绪；请在连接中准备后重试。' : '可手动刷新；切换页面不会自动查询。'; row.append(stateText);
      const details = document.createElement('p'); details.textContent = '来源：' + (source.endpoint || source.officialUrl || source.adapter) + (source.windowLabel ? ' · 周期：' + source.windowLabel : ''); row.append(details);
      for (const note of source.notes || []) { const detail = document.createElement('p'); detail.textContent = note; row.append(detail); }
      if (source.errorReason) { const reason = document.createElement('p'); reason.textContent = '最近失败：' + quotaReason(source.errorReason); row.append(reason); }
      const actions = document.createElement('div'); actions.className = 'action-line';
      if (source.status !== 'unsupported' && source.supported !== false) {
        const refresh = document.createElement('button'); refresh.type = 'button'; refresh.className = 'btn btn--outline btn--sm'; refresh.textContent = '查询此来源';
        refresh.addEventListener('click', async () => {
          refresh.disabled = true;
          try { state.quota = await request('/api/quota/refresh', { method: 'POST', body: { sourceIds: [source.sourceId] } }); renderQuota(); notify('已完成此来源查询，请按状态和更新时间查看结果。', 'ok'); }
          catch (error) { showError(error); refresh.disabled = false; }
        });
        actions.append(refresh);
      }
      if (source.removable === true) {
        const remove = document.createElement('button'); remove.className = 'btn btn--quiet btn--sm'; remove.type = 'button'; remove.textContent = '删除来源';
        remove.addEventListener('click', async () => {
          remove.disabled = true;
          try { state.quota = await request('/api/quota/sources', { method: 'POST', body: { action: 'remove', sourceId: source.sourceId } }); renderQuota(); notify('来源配置已删除；连接和已有 Key 未改变。', 'ok'); }
          catch (error) { showError(error); remove.disabled = false; }
        });
        actions.append(remove);
      }
      if (actions.children.length) row.append(actions);
      root.append(row);
    }
  }

  function applyFilter(name, value) {
    if ((state.filters[name] || '') === value) return;
    if (value) state.filters[name] = value; else delete state.filters[name];
    void loadUsage();
  }

  function bind() {
    document.querySelectorAll('[data-usage-tab]').forEach(button => button.addEventListener('click', () => selectTab(button.dataset.usageTab)));
    document.querySelectorAll('[data-usage-range]').forEach(button => button.addEventListener('click', () => {
      state.range = button.dataset.usageRange;
      document.querySelectorAll('[data-usage-range]').forEach(item => item.setAttribute('aria-pressed', String(item === button)));
      hide('usage-custom-form', state.range !== 'custom');
      if (state.range !== 'custom') void loadUsage();
    }));
    $('usage-custom-form').addEventListener('submit', event => { event.preventDefault(); void loadUsage(); });
    document.querySelectorAll('[data-usage-filter]').forEach(select => {
      const apply = () => applyFilter(select.dataset.usageFilter, select.value);
      select.addEventListener('change', apply); select.addEventListener('input', apply);
    });
    $('usage-filter-model').addEventListener('keydown', event => {
      if (event.key === 'Escape') { event.preventDefault(); $('usage-filter-model').open = false; $('usage-filter-model').querySelector('summary').focus(); }
    });
    $('usage-export').addEventListener('click', async () => {
      try {
        const data = await request('/api/usage/export');
        const blob = new Blob([JSON.stringify(data, null, 2) + '\n'], { type: 'application/json' });
        const url = URL.createObjectURL(blob); const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'kimi-work-usage.json'; anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
        notify('已生成脱敏用量 JSON；不包含 prompt、正文或 API Key。', 'ok');
      } catch (error) { showError(error); }
    });
    $('usage-clear').addEventListener('click', async () => {
      if (!state.clearArmed) { state.clearArmed = true; text('usage-clear', '再次点击确认清除'); clearTimeout(clearTimer); clearTimer = setTimeout(() => { state.clearArmed = false; text('usage-clear', '清除历史'); }, 8000); return; }
      clearTimeout(clearTimer); state.clearArmed = false; text('usage-clear', '清除历史');
      try { await request('/api/usage/clear', { method: 'POST', body: { confirm: true } }); state.result = null; await loadUsage(); notify('本机用量账本已清除；模型与连接配置未改变。', 'ok'); }
      catch (error) { showError(error); }
    });
    $('quota-refresh').addEventListener('click', async () => {
      const button = $('quota-refresh'); button.disabled = true; showError(null);
      try { state.quota = await request('/api/quota/refresh', { method: 'POST', body: {} }); renderQuota(); notify('额度刷新已结束，请按各来源时间查看结果。', 'ok'); }
      catch (error) { showError(error); }
      finally { button.disabled = false; }
    });
    $('quota-add').addEventListener('click', () => {
      if ($('quota-source-form')) $('quota-source-editor').replaceChildren();
      else renderSourceEditor();
    });
  }

  function mount(options) {
    request = options.request; notify = options.notify || (() => {});
    if (!$('usage-root')) return { show() {}, reload: async () => {} };
    template(); bind();
    return {
      show(filter = null) {
        if (filter?.connectionId && state.filters.connectionId !== filter.connectionId) {
          state.filters.connectionId = filter.connectionId;
          state.result = null;
        }
        if (!state.result) void loadUsage();
        if (!state.quota) void loadQuota();
      },
      reload,
    };
  }

  globalThis.KimiWorkUsage = { mount };
})();

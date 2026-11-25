import { $, API, ApiError, CONFIG_STATE, CONNECTION_ID_PATTERN, DIAG_REASON, DISCOVERY_REASON, LOOPBACK, UPSTREAM_REASON, actionUUID, clearKey, dom, fmtCount, fmtK, fmtMs, fmtTokens, isInputActive, renderErrorList, request, scrollMainToElement, setConnErrors, setDot, setHidden, setText, shortRev, showGate, showToast, state, syncSelectOptions } from './app-core.js';
import { closeEditor, connectionRowsOf, connectionsAvailable, connectionsChanged, editorDirty, routeIdFor } from './app-models.js';
import { isInFlight, renderActions, renderOverviewStatus, syncState } from './app-overview.js';
import { adoptState, clearOnboardSaveErrors, handleSaveFailure, loadState, renderAll, setView, stopPolling } from './app.js';

/* ---------- connection helpers ---------- */

function normalizeBaseURL(value) {
  const text = String(value || '').trim();
  let url;
  try {
    url = new URL(text);
  } catch {
    return { error: '上游地址不是合法的 URL。' };
  }
  if (!['http:', 'https:'].includes(url.protocol)) return { error: '上游地址必须使用 HTTPS（本机回环可用 HTTP）。' };
  if (/^[a-z][a-z0-9+.-]*:\/\/[^/]*@/i.test(url.href)) return { error: '上游地址不能包含账号或密码。' };
  if (url.search || url.hash) return { error: '上游地址不能包含查询参数或片段。' };
  if (url.protocol === 'http:' && !LOOPBACK.has(url.hostname)) return { error: '非本机地址必须使用 HTTPS。' };
  return { value: url.href.replace(/\/$/, '') };
}

function connectionSource(data) {
  const connection = (data && data.connection) || {};
  return connection.saved || connection.active || null;
}

const SVC_STATUS = {
  inherit: { tone: 'neutral', word: '使用 Kimi 官方服务', note: '没有写入任何 services 表，搜索与抓取保持官方默认。' },
  applied: { tone: 'ok', word: '已写入原生配置', note: '配置已写入；Kimi 内核监听该文件并热重载，新会话按新配置调用。' },
  unchanged: { tone: 'ok', word: '与已保存一致', note: '磁盘上的值已等于保存的配置，本次没有写入文件。' },
  repaired: { tone: 'warn', word: '已修复一次', note: '磁盘上的值被改动过，本次按保存的配置重写了一次。' },
  restored: { tone: 'ok', word: '已还原', note: '该服务已还原为接管前的原始配置。' },
  retained: { tone: 'warn', word: '保留他人改动', note: '磁盘上的值已被其他来源改写，未覆盖；所有权记录已清理。' },
  overwritten: { tone: 'bad', word: '值已被改写', note: '磁盘上的值不再是保存的配置，等待下一次自动核对。' },
  'reverted-by-daimon': { tone: 'warn', word: '被 Kimi 重新生成', note: 'Kimi 已把配置整体重写回官方值；本工具默认不再抢写，需要时重新保存即可。' },
  'repair-suppressed': { tone: 'bad', word: '暂停修复', note: '短时间内反复被改写，已停止抢写以免形成写循环。' },
  'pending-disable': { tone: 'warn', word: '待还原', note: '已关闭但仍检测到本工具写入的表，下一次核对会还原。' },
  'waiting-config': { tone: 'warn', word: '等待原生配置', note: 'Kimi 的 config.toml 尚不存在；配置已保存，等它生成后自动写入。' },
  'env-override': { tone: 'warn', word: '环境变量优先', note: '内核读取到 KIMI_WEB_* 环境变量，它优先于文件；文件写入不会生效。' },
  conflict: { tone: 'bad', word: '冲突', note: '配置结构不受支持或所有权记录不一致，未写入任何内容。' },
};

const SVC_REASON = {
  invalid_services: '提交的服务设置不被接受。',
  invalid_service_endpoint: '端点必须是完整的 http(s) 地址，本机可用 http；不能带账号、查询参数或片段。',
  invalid_service_key: '启用该服务时必须填写密钥；密钥不能有首尾空格或控制字符。',
  receipt_unverified: '本工具的所有权记录已损坏或属于另一个配置，未写入。',
  intent_unverified: '保存的搜索服务意图已损坏，未写入。',
  concurrent_modification: '配置文件在你保存的同时被其他来源改写，本次未写入。',
  write_raced: '写入后文件又被改写，本次结果未确认。',
  repair_budget_exhausted: '短时间内多次被改写，已停止自动修复。',
  foreign_value: '磁盘上的值来自其他来源，未覆盖。',
  service_value_drifted: '磁盘上的值已不再是保存的配置。',
  desktop_regenerated_services: 'Kimi 重新生成了原生配置。',
  service_environment_override: '内核读到了 KIMI_WEB_* 环境变量，它优先于文件。',
  config_missing: 'Kimi 的原生 config.toml 尚不存在。',
  disable_not_applied: '关闭尚未写入文件。',
  search_services_unavailable: '面板服务没有提供搜索服务接口。',
  separator_unverified: '配置文件在表前的换行已不是本工具写入的那个，未改动任何一行以免粘连相邻内容。',
  mixed_line_endings: '配置文件混用了不同换行风格，无法保证逐字节还原，未写入。',
  unsupported_layout: '该 services 表的写法不受支持（内联表、点号键或数组表），未写入。',
  unsupported_value: '该 services 表的值不是可识别的字符串，未写入。',
  duplicate_table: '文件中已有重复的 services 表，未写入。',
  duplicate_key: '该 services 表里有重复的键，未写入。',
  unsupported_toml: '配置文件含无法安全解析的写法，未写入。',
  config_symlink: '配置文件是符号链接，拒绝改写。',
  config_not_file: '配置路径不是普通文件，拒绝改写。',
  config_too_large: '配置文件超出允许大小，拒绝改写。',
  path_symlink: 'Kimi 目录路径包含符号链接，拒绝改写。',
  path_unavailable: '无法读取 Kimi 目录，未写入。',
  path_not_directory: 'Kimi 路径不是目录，未写入。',
  config_unavailable: '无法读取配置文件，未写入。',
};

function searchStateOf(data) {
  const value = data && data.search;
  return value && typeof value === 'object' && value.services && typeof value.services === 'object' ? value : null;
}

function searchAvailable(data) {
  return searchStateOf(data) !== null;
}

function serviceRowOf(data, service) {
  const search = searchStateOf(data);
  const row = search && search.services ? search.services[service] : null;
  return row && typeof row === 'object' ? row : null;
}

function serviceDraftFrom(data) {
  if (!searchAvailable(data)) return null;
  return {
    searchOn: serviceRowOf(data, 'search')?.enabled === true,
    searchUrl: serviceRowOf(data, 'search')?.endpoint || '',
    fetchOn: serviceRowOf(data, 'fetch')?.enabled === true,
    fetchUrl: serviceRowOf(data, 'fetch')?.endpoint || '',
  };
}

/* 密钥是只写的：草稿里保留用户本次输入的明文，但绝不从状态回填。 */
function serviceSignature(draft) {
  return draft ? JSON.stringify([draft.searchOn, draft.searchUrl, draft.fetchOn, draft.fetchUrl,
    state.services.searchKey, state.services.fetchKey]) : '';
}

function servicesDirty() {
  return serviceSignature(state.services.draft) !== state.services.base;
}

function validateServiceDraft(draft) {
  const errors = [];
  for (const [onKey, urlKey, label] of [['searchOn', 'searchUrl', '搜索'], ['fetchOn', 'fetchUrl', '抓取']]) {
    if (!draft || !draft[onKey]) continue;
    const raw = String(draft[urlKey] || '').trim();
    if (!raw) { errors.push(label + '服务需要填写完整端点地址。'); continue; }
    let parsed;
    try { parsed = new URL(raw); } catch { errors.push(label + '服务的地址不是合法的 URL。'); continue; }
    if (!['http:', 'https:'].includes(parsed.protocol)) errors.push(label + '服务只接受 http 或 https 地址。');
    else if (parsed.username || parsed.password) errors.push(label + '服务的地址不能包含账号或密码。');
    else if (parsed.search || parsed.hash) errors.push(label + '服务的地址不能带查询参数或片段。');
    else if (parsed.protocol === 'http:' && !LOOPBACK.has(parsed.hostname)) {
      errors.push(label + '服务用 http 时只能是本机地址；其他主机请用 https。');
    }
  }
  return { errors };
}

/* 只有用户改过的服务才进载荷：省略即保持已保存意图，关闭时发 enabled:false。
   apiKey 只在用户本次输入过时发送，避免空值把已存密钥清掉。 */
function servicePayload(draft) {
  const payload = {};
  const row = (on, url, key) => {
    if (!on) return { enabled: false };
    const value = { enabled: true, baseURL: String(url || '').trim() };
    if (typeof key === 'string' && key) value.apiKey = key;
    return value;
  };
  const savedSearch = state.services.baseRow?.search;
  const savedFetch = state.services.baseRow?.fetch;
  const searchChanged = !savedSearch || savedSearch.enabled !== draft.searchOn
    || (savedSearch.endpoint || '') !== String(draft.searchUrl || '').trim() || Boolean(state.services.searchKey);
  const fetchChanged = !savedFetch || savedFetch.enabled !== draft.fetchOn
    || (savedFetch.endpoint || '') !== String(draft.fetchUrl || '').trim() || Boolean(state.services.fetchKey);
  if (searchChanged) payload.search = row(draft.searchOn, draft.searchUrl, state.services.searchKey);
  if (fetchChanged) payload.fetch = row(draft.fetchOn, draft.fetchUrl, state.services.fetchKey);
  return payload;
}

function evaluateServices(data) {
  const search = searchStateOf(data);
  if (!search) {
    return { tone: 'neutral', word: '不可用', note: '面板服务没有提供搜索服务状态；搜索与抓取不受影响，仍走 Kimi 官方。', status: null };
  }
  const status = typeof search.status === 'string' ? search.status : 'inherit';
  const info = SVC_STATUS[status] || { tone: 'neutral', word: status, note: '' };
  const reason = typeof search.reason === 'string' && search.reason ? (SVC_REASON[search.reason] || search.reason) : '';
  return { ...info, note: reason || info.note, status };
}

function serviceRowText(row) {
  if (!row) return { word: '不可用', sub: '—' };
  const state = SVC_STATUS[row.status] || { word: row.status || '未知' };
  if (row.mode !== 'custom') return { word: state.word, sub: '未写入该服务表。' };
  return { word: state.word, sub: (row.endpoint || '未提供端点') + (row.hasKey ? ' · 已保存密钥' : ' · 未保存密钥') };
}

function servicesObservedText(data, service) {
  const row = serviceRowOf(data, service);
  const observed = row && row.observed ? row.observed : null;
  if (!observed || observed.present !== true) return '未观测到该 services 表。';
  const bits = [observed.endpoint || '无 base_url'];
  bits.push(observed.hasKey ? '有密钥' : '无密钥');
  if (observed.oauth) bits.push('含 oauth 引用');
  if (Number(observed.extraKeys) > 0) bits.push('另有 ' + observed.extraKeys + ' 个字段');
  return bits.join(' · ');
}

function renderServices(data) {
  if (!dom.svcPanel) return;
  const available = searchAvailable(data);
  setHidden(dom.svcUnavailable, available);
  const savedDraft = serviceDraftFrom(data);
  /* 轮询不得覆盖正在输入的内容：只有非脏状态才回填，密钥草稿永不回填。 */
  if (available && (!state.services.dirty || !state.services.draft)) {
    state.services.draft = savedDraft;
    state.services.baseRow = {
      search: serviceRowOf(data, 'search'),
      fetch: serviceRowOf(data, 'fetch'),
    };
    state.services.base = serviceSignature(savedDraft);
    state.services.dirty = false;
  }
  const draft = state.services.draft;
  if (!available || !draft) {
    setText(dom.svcSave ? dom.svcSave : null, '保存搜索服务');
    if (dom.svcSave) dom.svcSave.disabled = true;
    if (dom.svcReset) dom.svcReset.disabled = true;
    setDot(dom.svcVerdictDot, 'neutral');
    setText(dom.svcVerdictWord, '不可用');
    setText(dom.svcVerdictNote, '面板服务没有提供搜索服务接口。');
    return;
  }
  const dirty = servicesDirty();
  state.services.dirty = dirty;
  if (dom.svcSearchOn) dom.svcSearchOn.checked = draft.searchOn === true;
  if (dom.svcFetchOn) dom.svcFetchOn.checked = draft.fetchOn === true;
  if (dom.svcSearchUrl && !state.services.urlEditing?.search && !isInputActive(dom.svcSearchUrl) && dom.svcSearchUrl.value !== draft.searchUrl) {
    dom.svcSearchUrl.value = draft.searchUrl;
  }
  if (dom.svcFetchUrl && !state.services.urlEditing?.fetch && !isInputActive(dom.svcFetchUrl) && dom.svcFetchUrl.value !== draft.fetchUrl) {
    dom.svcFetchUrl.value = draft.fetchUrl;
  }
  const config = data.configuration || {};
  const blocked = config.state === 'unavailable';
  if (dom.svcSearchUrl) dom.svcSearchUrl.disabled = draft.searchOn !== true || state.services.saving || blocked;
  if (dom.svcSearchKey) dom.svcSearchKey.disabled = draft.searchOn !== true || state.services.saving || blocked;
  if (dom.svcFetchUrl) dom.svcFetchUrl.disabled = draft.fetchOn !== true || state.services.saving || blocked;
  if (dom.svcFetchKey) dom.svcFetchKey.disabled = draft.fetchOn !== true || state.services.saving || blocked;

  const searchRow = serviceRowOf(data, 'search');
  const fetchRow = serviceRowOf(data, 'fetch');
  const searchText = serviceRowText(searchRow);
  const fetchText = serviceRowText(fetchRow);
  setText(dom.svcSearchSaved, searchText.word);
  setText(dom.svcSearchSavedSub, searchText.sub + ' · 观测：' + servicesObservedText(data, 'search'));
  setText(dom.svcFetchSaved, fetchText.word);
  setText(dom.svcFetchSavedSub, fetchText.sub + ' · 观测：' + servicesObservedText(data, 'fetch'));
  /* 已有密钥不回显：只提示「留空即沿用」，避免把密钥写进页面。 */
  setText(dom.svcSearchKeyHint, searchRow?.hasKey
    ? '已保存密钥；留空表示沿用，填写则替换。' : '该服务需要密钥；内核用它发送 Authorization: Bearer。');
  if (dom.svcSearchKey && !state.services.searchKey && !isInputActive(dom.svcSearchKey)) dom.svcSearchKey.value = '';
  if (dom.svcFetchKey && !state.services.fetchKey && !isInputActive(dom.svcFetchKey)) dom.svcFetchKey.value = '';

  const verdict = evaluateServices(data);
  setDot(dom.svcVerdictDot, verdict.tone);
  setText(dom.svcVerdictWord, verdict.word);
  setText(dom.svcVerdictNote, verdict.note);
  if (dom.svcVerdict) dom.svcVerdict.dataset.status = verdict.status || '';

  const { errors } = validateServiceDraft(draft);
  const serverErrors = Array.isArray(state.services.serverErrors) ? state.services.serverErrors : [];
  renderErrorList(dom.svcErrors, (errors.length ? errors : serverErrors).slice(0, 2));
  if (dom.svcSave) {
    dom.svcSave.disabled = !dirty || errors.length > 0 || state.services.saving || blocked;
    setText(dom.svcSave, state.services.saving ? '正在保存…' : '保存搜索与抓取');
  }
  if (dom.svcReset) dom.svcReset.disabled = !dirty || state.services.saving;
  if (dom.svcDraft) setText(dom.svcDraft, blocked ? '配置文件不可读，无法保存搜索服务'
    : dirty ? '草稿尚未保存' : '草稿与已保存配置一致');
}

function readServiceForm() {
  const draft = state.services.draft;
  if (!draft) return;
  if (dom.svcSearchOn) draft.searchOn = dom.svcSearchOn.checked;
  if (dom.svcFetchOn) draft.fetchOn = dom.svcFetchOn.checked;
  if (dom.svcSearchUrl) draft.searchUrl = dom.svcSearchUrl.value;
  if (dom.svcFetchUrl) draft.fetchUrl = dom.svcFetchUrl.value;
  if (dom.svcSearchKey) state.services.searchKey = dom.svcSearchKey.value;
  if (dom.svcFetchKey) state.services.fetchKey = dom.svcFetchKey.value;
  state.services.serverErrors = [];
  state.services.dirty = servicesDirty();
}

function fillServiceForm() {
  const draft = state.services.draft;
  if (!draft) return;
  if (dom.svcSearchOn) dom.svcSearchOn.checked = draft.searchOn === true;
  if (dom.svcFetchOn) dom.svcFetchOn.checked = draft.fetchOn === true;
  if (dom.svcSearchUrl) dom.svcSearchUrl.value = draft.searchUrl;
  if (dom.svcFetchUrl) dom.svcFetchUrl.value = draft.fetchUrl;
  if (dom.svcSearchKey) dom.svcSearchKey.value = state.services.searchKey;
  if (dom.svcFetchKey) dom.svcFetchKey.value = state.services.fetchKey;
}

async function saveSearchServices() {
  const data = state.data;
  if (!data || state.services.saving || !searchAvailable(data)) return;
  const draft = state.services.draft;
  const { errors } = validateServiceDraft(draft);
  renderErrorList(dom.svcErrors, errors);
  if (errors.length) return;
  const payload = servicePayload(draft);
  if (!Object.keys(payload).length) {
    showToast('搜索与抓取配置没有变化。', 'ok');
    return;
  }
  state.services.saving = true;
  state.services.serverErrors = [];
  renderServices(data);
  try {
    const res = await request(API.context, { method: 'POST', body: { services: payload } });
    const search = res && res.search;
    if (search && typeof search === 'object' && search.services) {
      state.data = { ...state.data, search };
      state.services.saving = false;
      state.services.searchKey = '';
      state.services.fetchKey = '';
      state.services.serverErrors = [];
      state.services.baseRow = {
        search: serviceRowOf(state.data, 'search'),
        fetch: serviceRowOf(state.data, 'fetch'),
      };
      const next = serviceDraftFrom(state.data);
      state.services.draft = next;
      state.services.base = serviceSignature(next);
      state.services.dirty = false;
      fillServiceForm();
      renderServices(state.data);
      const verdict = evaluateServices(state.data);
      const good = ['applied', 'unchanged', 'repaired', 'restored', 'inherit'].includes(verdict.status);
      showToast(good ? '搜索服务：' + verdict.word + '。' : '搜索服务未完成：' + verdict.word + ' — ' + verdict.note,
        good ? 'ok' : 'error');
      return;
    }
    state.services.saving = false;
    await loadState({ silent: true });
    showToast('搜索服务已提交，状态已刷新。', 'ok');
  } catch (error) {
    state.services.saving = false;
    if (error.status === 400) {
      state.services.serverErrors = [error.message];
      return;
    }
    await handleSaveFailure(error, 'services');
  } finally {
    state.services.saving = false;
    if (state.data) renderServices(state.data);
  }
}

/* ---------- named connections ---------- */

/* 草稿只保存可回传的字段与只读诊断状态 credentialError；connectionPayload 会过滤掉诊断字段。
   apiKey 只在用户本次输入时存在，且不会回填到任何输入框；签名比较同样排除它，
   避免把「刚输入但未保存的 key」当成未变更。 */
function connectionDraftRow(row) {
  return {
    id: typeof row.id === 'string' ? row.id : '',
    name: typeof row.name === 'string' ? row.name : '',
    providerPresetId: typeof row.providerPresetId === 'string' ? row.providerPresetId : '',
    baseURL: typeof row.baseURL === 'string' ? row.baseURL : '',
    apiKeyEnv: typeof row.apiKeyEnv === 'string' ? row.apiKeyEnv : '',
    protocols: Array.isArray(row.protocols) ? row.protocols.slice() : null,
    legacy: row.legacy === true,
    modelCount: Number.isInteger(row.modelCount) ? row.modelCount : null,
    apiKeySet: row.apiKeySet === true,
    credentialError: typeof row.credentialError === 'string' ? row.credentialError : '',
    apiKey: '',
    secretSet: false,
  };
}

function connectionDraftFrom(data) {
  const rows = connectionRowsOf(data);
  if (!rows) return null;
  return rows.map(connectionDraftRow);
}

/* 一条连接只服务一种接口协议。旧配置可能没有协议或同时保留两种，这里只
   如实读出唯一值；0 个、多个或含未知值都返回 null，由表单要求用户明确选择。 */
function connectionProtocol(row) {
  const list = Array.isArray(row && row.protocols) ? row.protocols : [];
  if (list.length !== 1) return null;
  return list[0] === 'chat' || list[0] === 'responses' ? list[0] : null;
}

function connectionProtocolText(row) {
  const list = Array.isArray(row && row.protocols) ? row.protocols : [];
  if (list.length === 1 && (list[0] === 'chat' || list[0] === 'responses')) return protocolLabel(list[0]);
  if (list.length > 1) return list.map(protocolLabel).join('/') + '（待选择一种）';
  if (list.length === 1) return '接口协议不受支持';
  return '未选择';
}

function connectionProtocolError(row) {
  const list = Array.isArray(row && row.protocols) ? row.protocols : [];
  if (list.length === 1 && (list[0] === 'chat' || list[0] === 'responses')) return '';
  const name = row && (row.name || row.id) ? row.name || row.id : '未命名连接';
  if (list.length > 1) {
    return '连接「' + name + '」同时允许 ' + list.map(protocolLabel).join('/')
      + '；一条连接只能使用一种接口协议，请只保留一种。';
  }
  if (list.length === 1) {
    return '连接「' + name + '」的接口协议不受支持；请选择 Chat Completions 或 Responses。';
  }
  return '连接「' + name + '」还没有选择接口协议，请选择 Chat Completions 或 Responses。';
}

function connectionProtocolErrors(rows) {
  return (rows || []).filter((row) => row && !row.legacy).map(connectionProtocolError).filter(Boolean);
}

function connectionModelLabel(model) {
  if (!model) return '未命名模型';
  return model.name || model.upstreamModelId || '未命名模型';
}

/* 连接改协议时，已有模型不能悄悄跟着翻协议：旧路由 ID 与旧会话绑定必须
   保持。返回具体模型名，供保存前的错误提示逐条列出。 */
function connectionProtocolConflicts(connectionId, protocol) {
  if (!connectionId || (protocol !== 'chat' && protocol !== 'responses')) return [];
  const models = Array.isArray(state.data?.models) ? state.data.models : [];
  return models.filter((model) => model && model.connectionId === connectionId
    && model.protocol !== protocol);
}

function connectionProtocolConflictText(row, conflicts) {
  const target = protocolLabel(connectionProtocol(row));
  return '连接「' + (row.name || row.id) + '」现在只使用 ' + target + '，但这些已有模型仍按另一协议调用上游：'
    + conflicts.map((model) => connectionModelLabel(model) + '（'
      + (model.protocol === 'chat' || model.protocol === 'responses' ? protocolLabel(model.protocol) : '协议未声明')
      + '）').join('、')
    + '。为保持它们的模型 ID 和旧会话绑定，本次未保存；请先移除这些模型或把它们改到协议相同的连接。';
}

function connectionsSignature(rows) {
  if (!Array.isArray(rows)) return '';
  return JSON.stringify(rows.map((row) => [row.legacy ? 'legacy' : 'named', row.id, row.name,
    row.providerPresetId || '', row.baseURL, row.apiKeyEnv || '',
    (row.protocols || []).slice().sort().join(','), row.credentialError || '', row.apiKey ? 'with-key' : '']));
}

function savedConnectionsSignature(data) {
  return connectionsSignature(connectionDraftFrom(data));
}

function connectionsDirty() {
  return connectionsSignature(state.conns.draft || []) !== state.conns.base;
}

/* 提交体只包含契约允许的字段；apiKey 仅在本次输入过时带上，
   省略即表示保留已保存的旧值。protocols 仍按后端的数组契约发送，
   但只发唯一值：旧配置的多协议或缺失不会在这里被猜成某一个。 */
function connectionPayload(rows) {
  return (rows || []).filter((row) => !row.legacy).map((row) => {
    const protocol = connectionProtocol(row);
    return {
      id: row.id,
      name: row.name,
      ...(row.providerPresetId ? { providerPresetId: row.providerPresetId } : {}),
      baseURL: row.baseURL,
      ...(row.apiKeyEnv ? { apiKeyEnv: row.apiKeyEnv } : {}),
      ...(protocol ? { protocols: [protocol] } : {}),
      ...(row.apiKey ? { apiKey: row.apiKey } : {}),
    };
  });
}

const CONNECTION_ID_HINT = '标识只能使用字母、数字、点、下划线与连字符，且以字母或数字开头。';

/* 编辑抽屉的本地校验与服务端白名单保持一致，并额外拦下三种会静默丢配置的情况：
   重复标识、重复环境变量名、以及新连接缺 Key。 */
function validateConnectionDraft(draft, rows, { originalId = null } = {}) {
  const errors = [];
  const id = String(draft.id || '').trim();
  const name = String(draft.name || '').trim();
  if (!name) errors.push('请填写显示名称。');
  else if (name.length > 128) errors.push('显示名称最长 128 字符。');
  if (!id) errors.push('请填写连接标识。');
  else if (!CONNECTION_ID_PATTERN.test(id)) errors.push(CONNECTION_ID_HINT);
  else if ((rows || []).some((row) => !row.legacy && row.id === id && row.id !== originalId)) errors.push('已有同名标识的连接。');

  const base = normalizeBaseURL(draft.baseURL);
  if (base.error) errors.push(base.error);

  const keyEnv = String(draft.apiKeyEnv || '').trim();
  if (keyEnv && !/^[A-Z_][A-Z0-9_]{0,127}$/.test(keyEnv)) errors.push('Key 变量名只能使用大写字母、数字与下划线，且不能以数字开头。');
  if (keyEnv && (rows || []).some((row) => !row.legacy && row.id !== originalId && row.apiKeyEnv === keyEnv)) {
    errors.push('另一条连接已使用同一个 Key 变量名。');
  }

  const apiKey = String(draft.apiKey || '');
  if (!apiKey.trim() && !draft.secretSet && !(originalId && (rows || []).some((row) => row.id === originalId && row.apiKeySet))) {
    errors.push('请填写 API Key。留空只用于保持已保存的 Key。');
  }
  if (apiKey && /[\u0000-\u001f]/.test(apiKey)) errors.push('API Key 不能包含换行或控制字符。');

  const rawProtocols = Array.isArray(draft.protocols) ? draft.protocols : [];
  const protocols = protocolList(rawProtocols);
  if (rawProtocols.length !== 1 || protocols.length !== 1) {
    errors.push(rawProtocols.length > 1
      ? '一条连接只能选择一种接口协议；请只保留 Chat Completions 或 Responses。'
      : rawProtocols.length === 1
        ? '接口协议必须是 Chat Completions 或 Responses。'
        : '请选择接口协议：Chat Completions 或 Responses。');
  }
  return {
    errors,
    value: {
      id,
      name,
      ...(draft.providerPresetId ? { providerPresetId: draft.providerPresetId } : {}),
      baseURL: base.value || String(draft.baseURL || '').trim(),
      apiKeyEnv: keyEnv,
      ...(protocols.length === 1 ? { protocols: [protocols[0]] } : {}),
      apiKey: apiKey.trim(),
      legacy: false,
    },
  };
}

/* 每次打开抽屉都从草稿副本开始，取消不会留下半个编辑。 */
function openConnectionEditor(row) {
  const rows = state.conns.draft || [];
  state.composing = false;
  state.conns.editing = {
    originalId: row && !row.legacy ? row.id : null,
    baseRevision: state.conns.baseRevision ?? state.data?.revision ?? null,
    baseRow: row ? connectionDraftRow(row) : null,
    draft: row ? { ...connectionDraftRow(row), apiKey: '', secretSet: row.apiKeySet === true }
      : { id: '', name: '', providerPresetId: '', baseURL: '', apiKeyEnv: '', protocols: null,
        legacy: false, modelCount: null, apiKeySet: false, credentialError: '', apiKey: '', secretSet: false },
    keyEdited: false,
    errors: [],
  };
  renderConnections();
  renderConnectionEditor({ force: true });
  if (dom.ceName && dom.ceName.focus) dom.ceName.focus();
}

function closeConnectionEditor() {
  state.composing = false;
  state.conns.editing = null;
  renderConnections();
}

function readConnectionEditor() {
  const editing = state.conns.editing;
  if (!editing) return;
  editing.draft.name = dom.ceName ? dom.ceName.value : editing.draft.name;
  if (editing.originalId === null && dom.ceId) editing.draft.id = dom.ceId.value;
  editing.draft.providerPresetId = dom.cePreset ? dom.cePreset.value : editing.draft.providerPresetId;
  editing.draft.baseURL = dom.ceBase ? dom.ceBase.value : editing.draft.baseURL;
  editing.draft.apiKey = dom.ceKey ? dom.ceKey.value : editing.draft.apiKey;
  const protocols = [];
  if (dom.ceChat && dom.ceChat.checked) protocols.push('chat');
  if (dom.ceResponses && dom.ceResponses.checked) protocols.push('responses');
  if (protocols.length) editing.draft.protocols = protocols;
  /* 旧配置的多协议只读保留：用户只是在改名称或地址时，不能因为两个单选
     都没选中就把「同时允许两种」悄悄改成「未选择」。 */
  else if (!Array.isArray(editing.draft.protocols) || editing.draft.protocols.length < 2) {
    editing.draft.protocols = [];
  }
  if (editing.draft.apiKey) editing.draft.secretSet = true;
}

function applyConnectionEditor() {
  const editing = state.conns.editing;
  if (!editing) return;
  readConnectionEditor();
  const rows = state.conns.draft || [];
  if (editing.baseRow && editing.baseRevision !== (state.data?.revision ?? null)) {
    const live = connectionRowsOf(state.data).find(row => row.id === editing.originalId);
    if (!live || connectionsSignature([live]) !== connectionsSignature([editing.baseRow])) {
      editing.errors = ['连接已在别处更新，当前抽屉仍保留你的输入。请取消后重新打开，再核对你的修改。'];
      renderConnections();
      return;
    }
  }
  const { errors, value } = validateConnectionDraft(editing.draft, rows, { originalId: editing.originalId });
  if (!errors.length) {
    const protocol = connectionProtocol(value);
    const conflicts = connectionProtocolConflicts(value.id, protocol);
    if (conflicts.length) errors.push(connectionProtocolConflictText(value, conflicts));
  }
  editing.errors = errors;
  if (errors.length) {
    renderConnections();
    return;
  }
  const next = rows.slice();
  const index = editing.originalId ? next.findIndex((row) => !row.legacy && row.id === editing.originalId) : -1;
  const stored = {
    ...value,
    modelCount: index >= 0 ? next[index].modelCount : 0,
    apiKeySet: index >= 0 ? next[index].apiKeySet : false,
    credentialError: value.apiKey ? '' : (index >= 0 ? (next[index].credentialError || '') : ''),
    secretSet: Boolean(value.apiKey) || (index >= 0 && next[index].secretSet),
  };
  if (index >= 0) next[index] = stored;
  else next.push(stored);
  state.conns.draft = next;
  state.conns.editing = null;
  renderConnections();
  renderConnectionsDiff();
}

/* 供应商预设只用来填地址与协议；字段可继续手改，不把预设当成不可推翻的事实。 */
function applyProviderPreset(presetId) {
  const editing = state.conns.editing;
  if (!editing || !presetId) return;
  const preset = providerPreset(presetId);
  if (!preset) return;
  editing.draft.providerPresetId = preset.id;
  editing.draft.apiKey = '';
  editing.draft.secretSet = Boolean(editing.baseRow?.apiKeySet);
  editing.keyEdited = false;
  editing.draft.baseURL = typeof preset.baseURL === 'string' ? preset.baseURL : editing.draft.baseURL;
  const presetProtocols = protocolList(preset.protocols);
  if (presetProtocols.length === 1) {
    editing.draft.protocols = [presetProtocols[0]];
  } else if (presetProtocols.length > 1) {
    /* 多协议预设保留自己的元数据，但当前连接只能选一种：保留用户已经
       选中的那个，没有选过就留空，绝不默认 Responses。 */
    const current = connectionProtocol(editing.draft);
    editing.draft.protocols = current && presetProtocols.includes(current) ? [current] : [];
  }
  if (!editing.draft.name) editing.draft.name = preset.name || '';
  if (!editing.draft.id) editing.draft.id = presetsSuggestedId(preset);
  if (dom.ceBase) dom.ceBase.value = editing.draft.baseURL;
  if (dom.ceKey) dom.ceKey.value = '';
  if (dom.ceChat) dom.ceChat.checked = connectionProtocol(editing.draft) === 'chat';
  if (dom.ceResponses) dom.ceResponses.checked = connectionProtocol(editing.draft) === 'responses';
  if (dom.ceId && editing.originalId === null) dom.ceId.value = editing.draft.id;
  if (dom.ceName) dom.ceName.value = editing.draft.name;
  renderConnections();
}

function presetsSuggestedId(preset) {
  const base = String(preset.id || 'conn').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+|[^a-z0-9]+$/g, '');
  let id = base || 'conn';
  if (!/^[A-Za-z0-9]/.test(id)) id = 'c-' + id;
  return id.slice(0, 64);
}

function providerPresets() {
  return state.providers && Array.isArray(state.providers.presets) ? state.providers.presets : [];
}

function providerPreset(id) {
  if (!id) return null;
  return providerPresets().find((preset) => preset && preset.id === id) || null;
}

/* 后端只接受 `^\d{4}-\d{2}-\d{2}(?:T[\d:.]+Z?)?$` 的检查时间与 https 地址；
   预设资料里的值先按同一规则筛一遍，避免把不合法的来源写进保存请求。 */
const SOURCE_STAMP_PATTERN = /^\d{4}-\d{2}-\d{2}(?:T[\d:.]+Z?)?$/;

function sourceURLOf(value) {
  if (typeof value !== 'string' || !value) return '';
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash) return '';
    return url.href;
  } catch {
    return '';
  }
}

function sourceStampOf(value) {
  return typeof value === 'string' && SOURCE_STAMP_PATTERN.test(value) ? value : '';
}

/* 预设模型自身的 source 没有 kind；这里只取预设资料自己的地址与检查时间，
   绝不把上游 /models 的 URL 当成预设来源。预设没写地址时只保留 kind。 */
function presetMetadataSource(row) {
  const source = row && typeof row === 'object' && row.source && typeof row.source === 'object' ? row.source : {};
  const url = sourceURLOf(source.url);
  const checkedAt = sourceStampOf(source.checkedAt);
  return { kind: 'preset', ...(url ? { url } : {}), ...(checkedAt ? { checkedAt } : {}) };
}

/* 预设目录在 providers.json 里有两种形状：presets[].models 用 id/name，
   details[presetId].models 用 upstreamModelId/label。向导只按同一条上游 ID
   对应，这里统一成 id/upstreamModelId/name，避免两处比较规则各写一遍。 */
function presetOnboardModels(presetId) {
  if (!presetId) return [];
  const preset = providerPreset(presetId);
  const detail = state.providers && state.providers.details ? state.providers.details[presetId] : null;
  const rows = preset && Array.isArray(preset.models) && preset.models.length
    ? preset.models
    : (detail && Array.isArray(detail.models) ? detail.models : []);
  return rows.map((row) => {
    if (!row || typeof row !== 'object') return null;
    const upstreamModelId = typeof row.upstreamModelId === 'string' && row.upstreamModelId
      ? row.upstreamModelId
      : typeof row.id === 'string' && row.id ? row.id : '';
    if (!upstreamModelId) return null;
    return {
      ...row,
      id: typeof row.id === 'string' && row.id ? row.id : upstreamModelId,
      upstreamModelId,
      source: row.source || preset?.source || detail?.source,
      name: typeof row.name === 'string' && row.name
        ? row.name
        : (typeof row.label === 'string' ? row.label : ''),
    };
  }).filter(Boolean);
}

/* 上游 /models 只回答它自己声明的字段；缺的字段回退到当前预设里同一条上游
   ID 的已知资料。上游已有的值（包括 false 与空数组）一律优先，预设里的
   "unknown" 不是数组，因此不会被当成档位。 */
function mergeOnboardPresetMetadata(candidate, preset) {
  if (!candidate || !preset) return candidate;
  const merged = { ...candidate };
  const blank = (value) => typeof value !== 'string' || !value.trim();
  const positive = (value) => Number(value) > 0;
  let used = false;
  const fill = (key, value) => {
    if (value === undefined) return;
    merged[key] = value;
    used = true;
  };
  if (blank(merged.name) && !blank(preset.name)) merged.name = preset.name;
  if (!positive(merged.contextWindow) && positive(preset.contextWindow)) fill('contextWindow', Number(preset.contextWindow));
  const knownContext = positive(merged.contextWindow) ? Number(merged.contextWindow) : 0;
  if (!positive(merged.maxOutputTokens) && positive(preset.maxOutputTokens)
    && (!knownContext || Number(preset.maxOutputTokens) <= knownContext)) {
    fill('maxOutputTokens', Number(preset.maxOutputTokens));
  }
  if (typeof merged.tools !== 'boolean' && typeof preset.tools === 'boolean') fill('tools', preset.tools);
  if (typeof merged.reasoning !== 'boolean' && typeof preset.reasoning === 'boolean') fill('reasoning', preset.reasoning);
  if (!Array.isArray(merged.inputModalities) && Array.isArray(preset.inputModalities) && preset.inputModalities.length) {
    fill('inputModalities', preset.inputModalities.slice());
  }
  /* 上游明确说不支持思考时，不把预设档位带进一条 reasoning:false 的行。 */
  if (merged.reasoning !== false && !Array.isArray(merged.reasoningEfforts)
    && Array.isArray(preset.reasoningEfforts) && preset.reasoningEfforts.length) {
    fill('reasoningEfforts', preset.reasoningEfforts.slice());
  }
  if (blank(merged.description) && !blank(preset.description)) merged.description = preset.description;
  if (used) {
    /* 预设资料自己的来源没有 kind，也绝不能用上游 /models 的地址顶替：
       被补全的字段只能标成 preset，并用预设文档自己的 url/checkedAt。 */
    merged.metadataSource = presetMetadataSource(preset);
    if (!merged.source) {
      const ownURL = sourceURLOf(preset.source?.url);
      const ownStamp = sourceStampOf(preset.source?.checkedAt);
      if (ownURL || ownStamp) {
        merged.source = { ...(ownURL ? { url: ownURL } : {}), ...(ownStamp ? { checkedAt: ownStamp } : {}) };
      }
    }
  }
  return merged;
}

/* 已知原因有中文说明；未知错误码也给一句可读的话，不把内部标识直接甩给用户。 */
function discoveryReasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  if (DISCOVERY_REASON[code]) return DISCOVERY_REASON[code];
  if (code === 'model_search_failed' || code === 'panel_operation_failed') {
    return '候选搜索未完成，请稍后重试；已载入的供应商目录仍可点选。';
  }
  return '操作未完成（' + code + '）；请重试或查看诊断。';
}

function configInfo(data) {
  const config = (data && data.configuration) || {};
  return CONFIG_STATE[config.state] || { tone: 'neutral', word: '未知', note: '' };
}

function configReady(data) {
  return Boolean(data && data.configuration && data.configuration.state === 'ready');
}


/* ---------- onboarding: one visible save-and-connect path ---------- */

/* 向导只做三件可验证的事：把连接写成一份真实配置、读取该上游的模型目录、
   把勾选的模型随目录一起保存。能力默认取上游声明，未声明的一律留空由用户
   确认；任何一步失败都停在该步并如实说明，不跳到“已同步”。 */

/* 渲染入口与写操作共用同一个忙标记：读取中或保存中都不重画表单，
   避免把用户刚输入的 Key 或勾选状态冲掉。 */
function onboardBusy() {
  return Boolean(state.onboard && (state.onboard.busy || state.onboard.saving));
}

function onboardConnectionValues() {
  return {
    id: String(dom.onboardId ? dom.onboardId.value : '').trim(),
    name: String(dom.onboardName ? dom.onboardName.value : '').trim(),
    baseURL: String(dom.onboardBase ? dom.onboardBase.value : '').trim(),
    apiKey: String(dom.onboardKey ? dom.onboardKey.value : ''),
    providerPresetId: String(dom.onboardPreset ? dom.onboardPreset.value : ''),
    protocol: onboardProtocol(),
  };
}

/* 已保存的连接按 id 回填；用户已经开始输入新连接时不覆盖已编辑的字段。 */
function savedOnboardTarget() {
  const rows = connectionRowsOf(state.data);
  if (!Array.isArray(rows) || !rows.length) return null;
  return rows.find((row) => !row.legacy) || null;
}

function fillOnboardForm() {
  if (!dom.onboardId) return;
  const target = savedOnboardTarget();
  if (target) {
    if (!state.onboard.protocolTouched) state.onboard.protocol = connectionProtocol(target);
    if (!isInputActive(dom.onboardId)) dom.onboardId.value = target.id || '';
    if (!isInputActive(dom.onboardName)) dom.onboardName.value = target.name || '';
    if (!isInputActive(dom.onboardBase)) dom.onboardBase.value = target.baseURL || '';
    if (dom.onboardPreset && target.providerPresetId && !isInputActive(dom.onboardPreset)) {
      if (dom.onboardPreset.dataset?.providerFlow === 'managed') {
        dom.onboardPreset.dataset.desiredPreset = target.providerPresetId;
        if (typeof Event === 'function' && typeof dom.onboardPreset.dispatchEvent === 'function') {
          dom.onboardPreset.dispatchEvent(new Event('kwp:sync-provider-flow'));
        }
      } else {
        dom.onboardPreset.value = target.providerPresetId;
      }
    }
    if (dom.onboardKeyHint) {
      setText(dom.onboardKeyHint, target.apiKeySet
        ? '已保存的 Key 不会显示；留空表示保持现有值。'
        : '这个连接还没有保存 Key；填写后保存会写入本机私有凭据文件。');
    }
  }
  /* 已保存的模型会进入目录视图，用户无需重新读取就能继续保存。这份目录属于该
     已保存连接自己的端点，因此记下归属：地址一旦改动就该重新读取。 */
  if (!state.onboard.discovery) {
    const models = (Array.isArray(state.data.models) ? state.data.models : [])
      .filter((model) => model && model.connectionId && (!target || model.connectionId === target.id));
    if (models.length) {
      state.onboard.discoveryFor = onboardEndpointKey();
      state.onboard.discovery = {
        checkedAt: null,
        modelCount: models.length,
        saved: true,
        models: models.map((model) => savedOnboardModelRow(model)),
      };
      if (dom.onboardDiscoverState) setText(dom.onboardDiscoverState, '已从保存的目录载入 ' + fmtCount(models.length) + ' 个模型。');
    }
  }
  fillOnboardPresets();
}

/* 已保存模型回到向导时保留名称、能力、档位、默认档位与来源；协议只作为
   旧配置的兼容显示保留，不再成为新导入行的协议来源。 */
function savedOnboardModelRow(model) {
  const upstreamModelId = model.upstreamModelId || model.id;
  return {
    key: upstreamModelId + (model.protocol ? '#' + model.protocol : ''),
    id: upstreamModelId,
    name: model.name || '',
    upstreamModelId,
    protocol: protocolList([model.protocol])[0] || null,
    contextWindow: model.contextWindow,
    maxOutputTokens: model.maxOutputTokens,
    /* 已保存行自己的声明照原样带入：没写过就仍是未知，不补 false。 */
    tools: typeof model.tools === 'boolean' ? model.tools : undefined,
    reasoning: typeof model.reasoning === 'boolean' ? model.reasoning : undefined,
    vision: Array.isArray(model.inputModalities) ? model.inputModalities.includes('image') : undefined,
    efforts: Array.isArray(model.reasoningEfforts) ? model.reasoningEfforts : [],
    defaultEffort: typeof model.defaultEffort === 'string' && model.defaultEffort ? model.defaultEffort : undefined,
    /* 已保存行自己的上限必须带回：没有它，重新保存会把它当成「清空」写 null。 */
    contextLimit: positiveInteger(model.contextLimit),
    checkedAt: model.metadataSource && model.metadataSource.checkedAt ? model.metadataSource.checkedAt : null,
    sourceURL: model.metadataSource && model.metadataSource.url ? model.metadataSource.url : null,
    metadataKind: model.metadataSource?.kind || 'manual',
    selected: true,
  };
}

function onboardModelRows() {
  const discovery = state.onboard.discovery;
  return discovery && Array.isArray(discovery.models) ? discovery.models : [];
}

function selectedOnboardModels() {
  return onboardModelRows().filter((row) => row && row.selected === true);
}

function filteredOnboardModels() {
  const query = String(state.onboard.query || '').trim().toLowerCase();
  if (!query) return onboardModelRows();
  return onboardModelRows().filter((row) => [row.id, row.name, row.description]
    .some((value) => String(value || '').toLowerCase().includes(query)));
}

const STANDARD_ONBOARD_EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

function protocolList(value) {
  return Array.isArray(value)
    ? value.filter((entry) => entry === 'chat' || entry === 'responses')
    : [];
}

/* 向导的连接协议来自供应商页（连接编辑器或向导里的连接字段），不来自
   某一行模型，也不按上游模型名或本机路由名猜。 */
function onboardProtocol() {
  const value = state.onboard && state.onboard.protocol;
  return value === 'chat' || value === 'responses' ? value : null;
}

function setOnboardProtocol(protocol) {
  if (protocol !== 'chat' && protocol !== 'responses') return;
  state.onboard.protocol = protocol;
  state.onboard.protocolTouched = true;
  markOnboardEdited();
  renderOnboard();
}

function toggleFilteredOnboardModels() {
  const rows = filteredOnboardModels();
  if (!rows.length) return;
  const selected = !rows.every((row) => row.selected === true);
  rows.forEach((row) => { row.selected = selected; });
  markOnboardEdited();
  renderOnboard();
}

function applyOnboardBatchContext() {
  const value = positiveInteger(dom.onboardBatchContext?.value);
  if (!value) {
    showToast('请输入正整数的上下文窗口；不知道时请留空并向上游确认。', 'error');
    return;
  }
  const rows = selectedOnboardModels();
  if (!rows.length) return;
  rows.forEach((row) => {
    row.contextWindow = value;
    row.metadataKind = 'manual';
  });
  markOnboardEdited();
  renderOnboard();
  showToast('已将 ' + fmtCount(rows.length) + ' 个已选模型的上下文窗口设为 ' + fmtTokens(value) + '。', 'ok');
}

/* 目录属于一个确切的端点：地址或预设一变，上一轮读到的模型就不再是同一个上游的
   答案。这里按规范化后的地址与预设记下归属，变化时清掉旧目录，避免用户拿旧目录
   去保存一条指向新地址的路由。没有归属的目录（刚由一次成功读取或保存的目录载入）
   在第一次看到时认领当前端点，之后才参与比较。 */
function onboardEndpointKey() {
  const values = onboardConnectionValues();
  const base = normalizeBaseURL(values.baseURL);
  return JSON.stringify([base.value || values.baseURL, values.providerPresetId || null]);
}

function syncOnboardEndpoint() {
  const key = onboardEndpointKey();
  if (state.onboard.discovery && state.onboard.discoveryFor == null) state.onboard.discoveryFor = key;
  else if (state.onboard.discovery && state.onboard.discoveryFor !== key) {
    state.onboard.discovery = null;
    state.onboard.discoveryFor = null;
    if (dom.onboardDiscoverState) {
      setText(dom.onboardDiscoverState, '地址或预设已改变；请重新读取模型后再勾选。');
    }
  }
  return key;
}

function savedOnboardRow(id) {
  const rows = connectionRowsOf(state.data);
  if (!Array.isArray(rows) || !id) return null;
  return rows.find((row) => row && !row.legacy && row.id === id) || null;
}

/* 同一上游模型可以同时有 Chat 与 Responses 两条已保存路由；DOM 属性与草稿
   查找不能用上游 ID 当键，否则编辑一行会写到另一行。key 在行创建时固定，
   之后不再随编辑变化。 */
function onboardRowKey(row) {
  if (!row) return '';
  return row.key || row.id || row.upstreamModelId || '';
}

function onboardRowByKey(key) {
  return onboardModelRows().find((entry) => onboardRowKey(entry) === key) || null;
}

function onboardRowSource(candidate) {
  const meta = candidate && candidate.metadataSource && typeof candidate.metadataSource === 'object'
    ? candidate.metadataSource : null;
  if (meta) {
    return {
      checkedAt: sourceStampOf(meta.checkedAt) || null,
      sourceURL: sourceURLOf(meta.url) || null,
      metadataKind: meta.kind === 'preset' || meta.kind === 'manual' ? meta.kind : 'discovery',
    };
  }
  const source = candidate && candidate.source && typeof candidate.source === 'object' ? candidate.source : null;
  return {
    checkedAt: source ? (sourceStampOf(source.checkedAt) || null) : null,
    sourceURL: source ? (sourceURLOf(source.url) || null) : null,
    metadataKind: 'discovery',
  };
}

function onboardRowFrom(candidate, { connection = null } = {}) {
  /* 协议由连接决定；这一行只保留候选自己的名称、预算与能力。上游声明的
     protocol 不再变成逐行选择，也不再决定新导入模型的协议。 */
  return {
    key: candidate.id,
    id: candidate.id,
    name: candidate.name || '',
    description: candidate.description || '',
    upstreamModelId: candidate.id,
    protocol: null,
    connectionProtocol: connectionProtocol(connection),
    contextWindow: candidate.contextWindow,
    maxOutputTokens: candidate.maxOutputTokens,
    tools: typeof candidate.tools === 'boolean' ? candidate.tools : undefined,
    reasoning: typeof candidate.reasoning === 'boolean' ? candidate.reasoning : undefined,
    vision: Array.isArray(candidate.inputModalities) ? candidate.inputModalities.includes('image') : undefined,
    efforts: Array.isArray(candidate.reasoningEfforts) ? candidate.reasoningEfforts : [],
    /* 有 metadataSource 时来源只认它：否则预设补全的字段会被写成上游
       /models 的地址，看起来像上游亲自声明了这些值。 */
    ...onboardRowSource(candidate),
    selected: false,
  };
}

/* 重新读取上游时保留用户自己改过的显示名称与默认档位；同一连接下同一
   上游 ID 只应有一条模型，出现多条时不猜该沿用哪一个。 */
function preserveOnboardModelNames(models, savedModels, connectionId) {
  const saved = new Map();
  for (const model of Array.isArray(savedModels) ? savedModels : []) {
    if (!model || model.connectionId !== connectionId || !(model.name || model.defaultEffort)) continue;
    const upstream = model.upstreamModelId || model.id;
    const rows = saved.get(upstream) || [];
    rows.push(model);
    saved.set(upstream, rows);
  }
  return models.map((row) => {
    const candidates = saved.get(row.upstreamModelId) || [];
    if (candidates.length !== 1) return row;
    const previous = candidates[0];
    const next = { ...row };
    if (previous.name) next.name = previous.name;
    if (previous.defaultEffort && Array.isArray(next.efforts) && next.efforts.includes(previous.defaultEffort)) {
      next.defaultEffort = previous.defaultEffort;
    }
    return next;
  });
}

/* 显示名称只用于本机展示，改它不改上游能力的来源标记；输入过程中的空白
   原样留在草稿里，提交构建时再 trim，避免打断中文输入法。 */
function setOnboardModelName(row, value) {
  if (!row) return false;
  const name = typeof value === 'string' ? value.slice(0, 200) : '';
  if ((row.name || '') === name) return false;
  row.name = name;
  return true;
}

/* 档位只改这条本地草稿；移除当前默认档位时一并清空，避免保存出不在列表里的值。 */
function toggleOnboardModelEffort(row, value) {
  if (!row || row.reasoning !== true || typeof value !== 'string' || !value) return false;
  const list = Array.isArray(row.efforts) ? row.efforts.slice() : [];
  const index = list.indexOf(value);
  if (index >= 0) list.splice(index, 1);
  else list.push(value);
  row.efforts = list;
  if (row.defaultEffort && !list.includes(row.defaultEffort)) delete row.defaultEffort;
  row.metadataKind = 'manual';
  return true;
}

function setOnboardModelDefaultEffort(row, value) {
  if (!row || row.reasoning !== true) return false;
  const list = Array.isArray(row.efforts) ? row.efforts : [];
  const next = list.includes(value) ? value : undefined;
  if (row.defaultEffort === next) return false;
  if (next === undefined) delete row.defaultEffort;
  else row.defaultEffort = next;
  return true;
}

function positiveInteger(value) {
  const number = typeof value === 'number' ? value : Number(String(value ?? '').trim());
  return Number.isSafeInteger(number) && number > 0 ? number : undefined;
}

function currentConnectionLimit(connectionId) {
  const row = savedOnboardRow(connectionId);
  return positiveInteger(row && row.contextLimit);
}

/* 新模型只写已知字段；保存已存在的路由（existing）时对清空项显式写 null：
   后端 MergeModels 只把 null 当作删除，省略会保留旧值，取消推理或默认档位
   后就会因为旧档位对不上而 400，或把旧值带回界面。 */
function buildOnboardRow(row, { existing = false, previousLimit = undefined, protocol = null, id = '' } = {}) {
  const context = positiveInteger(row.contextWindow);
  /* 协议来自连接的唯一选择；这里不接受行内协议，也不按上游名或路由名猜。
     已保存模型则由调用方传入它原来的 ID 和协议，保持旧会话绑定。 */
  const chosen = protocol === 'chat' || protocol === 'responses' ? protocol : null;
  /* 上下文上限只取真实已知值：目录声明的上限或连接上记录的上限，未知不编造；
     上限小于本次上下文时不能写入，否则后端会因为窗口超过上限而拒绝保存。 */
  const knownLimit = positiveInteger(row.contextLimit) ?? currentConnectionLimit(row.connectionId);
  const contextLimit = context && knownLimit && knownLimit >= context ? knownLimit : undefined;
  const hadLimit = positiveInteger(row.contextLimit) !== undefined || positiveInteger(previousLimit) !== undefined;
  const efforts = row.reasoning === true && Array.isArray(row.efforts) ? row.efforts.slice() : [];
  const defaultEffort = efforts.includes(row.defaultEffort) ? row.defaultEffort : '';
  const name = (typeof row.name === 'string' ? row.name.trim() : '') || '';
  const output = positiveInteger(row.maxOutputTokens);
  const optional = (key, value) => (value !== undefined ? { [key]: value } : (existing ? { [key]: null } : {}));
  return {
    id: id || (chosen ? routeIdFor(row.connectionId, row.upstreamModelId, chosen) : ''),
    upstreamModelId: row.upstreamModelId,
    ...optional('name', name && name !== row.upstreamModelId ? name : undefined),
    protocol: chosen,
    contextWindow: context,
    /* 只有用户原本确实有上限时才把「清空」写成 null；没有上限的模型不加这个字段。 */
    ...(contextLimit !== undefined ? { contextLimit } : (existing && hadLimit ? { contextLimit: null } : {})),
    ...optional('maxOutputTokens', output),
    ...(row.tools === true ? { tools: true } : row.tools === false ? { tools: false } : (existing ? { tools: null } : {})),
    ...(typeof row.reasoning === 'boolean' ? { reasoning: row.reasoning } : (existing ? { reasoning: null } : {})),
    inputModalities: row.vision === true ? ['text', 'image'] : ['text'],
    ...optional('reasoningEfforts', efforts.length ? efforts : undefined),
    ...optional('defaultEffort', defaultEffort || undefined),
    connectionId: row.connectionId,
    ...(row.metadataKind !== 'manual' && row.checkedAt
      ? { metadataSource: { kind: row.metadataKind || 'discovery', ...(row.sourceURL ? { url: row.sourceURL } : {}), checkedAt: row.checkedAt } }
      : { metadataSource: { kind: 'manual' } }),
  };
}

function onboardExistingModels(connectionId) {
  return (Array.isArray(state.data?.models) ? state.data.models : [])
    .filter((model) => model && model.connectionId === connectionId);
}

function onboardProblems({ models = [] } = {}) {
  const problems = [];
  const values = onboardConnectionValues();
  if (!values.baseURL) problems.push('请填写 API 基础地址。');
  else {
    const base = normalizeBaseURL(values.baseURL);
    if (base.error) problems.push(base.error);
  }
  if (!values.id) problems.push('请填写连接标识。');
  else if (!CONNECTION_ID_PATTERN.test(values.id)) problems.push(CONNECTION_ID_HINT);
  if (!values.name) problems.push('请填写显示名称。');
  /* 保存时才需要 Key：只有同一个已保存连接、同一个地址可以留空以保留原值。
     读目录是另一件事，它在保存前就允许只带本次填写的 Key。 */
  const saved = savedOnboardRow(values.id);
  const keepKey = Boolean(saved && saved.apiKeySet && normalizeBaseURL(values.baseURL).value === saved.baseURL);
  if (!values.apiKey.trim() && !keepKey) problems.push('请填写 API Key。');
  const protocol = values.protocol;
  if (protocol !== 'chat' && protocol !== 'responses') {
    problems.push('请选择这条连接的接口协议：Chat Completions 或 Responses。');
  }
  if (!models.length) problems.push('至少勾选 1 个模型。');
  /* 已有模型不能被这次保存悄悄改成新协议：它们的模型 ID 与旧会话绑定
     必须保持。这里列出具体模型名并阻止保存。 */
  if (protocol === 'chat' || protocol === 'responses') {
    const conflicts = connectionProtocolConflicts(values.id, protocol);
    if (conflicts.length) {
      problems.push(connectionProtocolConflictText({ id: values.id, name: values.name, protocols: [protocol] }, conflicts));
    }
  }
  /* 正整数校验与 buildOnboardRow 用同一个函数：能通过校验的值一定能被
     安全地写进 contextWindow，不会出现界面可保存、后端 400 的组合。 */
  const invalidContext = models.filter(row => row.contextWindow !== undefined
    && row.contextWindow !== null && row.contextWindow !== '' && !positiveInteger(row.contextWindow));
  if (invalidContext.length) {
    problems.push('上下文窗口必须是正整数：' + invalidContext.map(row => row.upstreamModelId).join('、') + '。');
  }
  const invalidOutput = models.filter(row => row.maxOutputTokens !== undefined
    && row.maxOutputTokens !== null && row.maxOutputTokens !== '' && !positiveInteger(row.maxOutputTokens));
  if (invalidOutput.length) {
    problems.push('最大输出必须是正整数或留空：' + invalidOutput.map(row => row.upstreamModelId).join('、') + '。');
  }
  const missingContext = models.filter(row => !positiveInteger(row.contextWindow));
  if (missingContext.length) {
    problems.push('请为这些模型填写上下文窗口；接口没有返回时不能用猜测值：'
      + missingContext.map(row => row.upstreamModelId).join('、') + '。');
  }
  /* 同一个连接里同一上游 ID 只对应一条模型；出现多条时无法判断该保留
     哪一条的旧 ID，列出上游 ID 并停下。 */
  const previousByUpstream = new Map();
  const ambiguous = new Set();
  for (const model of onboardExistingModels(values.id)) {
    const upstream = model.upstreamModelId || model.id;
    if (!upstream) continue;
    if (previousByUpstream.has(upstream)) ambiguous.add(upstream);
    else previousByUpstream.set(upstream, model);
  }
  if (ambiguous.size) {
    problems.push('连接「' + (values.name || values.id) + '」里同一上游模型有多条已保存记录：'
      + [...ambiguous].join('、') + '。为避免覆盖旧模型 ID 与旧会话，请先在模型管理中整理这些记录。');
  }
  const ids = new Set();
  const built = [];
  for (const row of models) {
    const draft = { ...row, connectionId: values.id };
    const previousRow = previousByUpstream.get(row.upstreamModelId);
    const keepPrevious = previousRow && protocol && previousRow.protocol === protocol;
    const model = keepPrevious
      ? buildOnboardRow(draft, { existing: true, protocol, id: previousRow.id, previousLimit: previousRow.contextLimit })
      : buildOnboardRow(draft, { protocol });
    if (model.id && ids.has(model.id)) problems.push('同一个上游模型被重复导入：' + row.upstreamModelId + '。');
    if (model.id) ids.add(model.id);
    built.push(model);
  }
  return { problems, built, values };
}

/* 向导只会重新声明连接与模型能力，不会谈定价与备用连接。用向导重存一条已有
   模型时，这两项属于用户自己的配置：原样带回，否则一次向导保存就把它们抹掉。 */
function mergeOnboardModels(existing, built, connectionId) {
  const replacements = new Map(built.map((model) => [model.id, model]));
  const retained = existing.filter((model) => !(model && model.connectionId === connectionId && replacements.has(model.id)));
  const merged = built.map((model) => {
    const previous = existing.find((row) => row && row.id === model.id && row.connectionId === connectionId);
    if (!previous) return model;
    const next = { ...model };
    if (!next.pricing && previous.pricing) next.pricing = previous.pricing;
    if (!next.failover && Array.isArray(previous.failover) && previous.failover.length) next.failover = previous.failover;
    return next;
  });
  return [...retained, ...merged];
}

/* 行内改动的统一入口：只标 touched，不重建 DOM。输入框在输入过程中重建会
   打断中文输入法的候选与选择区。 */
function markOnboardEdited() {
  state.onboard.touched = true;
  clearOnboardSaveErrors();
}

function onboardEditingText() {
  const active = document.activeElement;
  return Boolean(active && dom.onboardModels?.contains?.(active)
    && (active.dataset?.onboardName || active.dataset?.onboardBudget));
}

function renderOnboardModels(rows) {
  if (!dom.onboardModels || onboardEditingText()) return;
  const connectionId = onboardConnectionValues().id;
  const attachedIds = new Set((Array.isArray(state.data?.models) ? state.data.models : [])
    .filter((model) => model && model.connectionId === connectionId)
    .map((model) => model.upstreamModelId || model.id));
  const signature = JSON.stringify([rows, connectionId, [...attachedIds]]);
  if (dom.onboardModels.dataset.signature === signature) return;
  const active = document.activeElement;
  const focusedInside = Boolean(active && dom.onboardModels.contains && dom.onboardModels.contains(active));
  const focusKey = focusedInside ? (active.dataset?.onboardModel || '') : '';
  const focusControl = focusedInside ? (active.dataset?.onboardFocus || '') : '';
  dom.onboardModels.dataset.signature = signature;
  if (!rows.length) {
    dom.onboardModels.replaceChildren();
    return;
  }
  const nodes = rows.map((row) => {
    const key = onboardRowKey(row);
    const li = document.createElement('li');
    li.className = 'onboard__model';
    li.dataset.onboardRow = key;
    const label = document.createElement('label');
    label.className = 'check check--model';
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = row.selected === true;
    box.dataset.onboardModel = key;
    box.dataset.onboardFocus = 'selected';
    box.addEventListener('change', () => {
      const target = onboardRowByKey(key);
      if (target) target.selected = box.checked === true;
      markOnboardEdited();
      renderOnboard();
    });
    const body = document.createElement('span');
    body.className = 'onboard__model-body';
    const head = document.createElement('span');
    head.className = 'onboard__model-head';
    const id = document.createElement('span');
    id.className = 'mono onboard__model-id';
    id.textContent = row.id;
    head.append(id);
    if (row.name && row.name !== row.id) {
      const name = document.createElement('span');
      name.className = 'onboard__model-name';
      name.textContent = row.name;
      head.append(name);
    }
    const attached = attachedIds.has(row.upstreamModelId || row.id);
    if (attached) {
      const badge = document.createElement('span');
      badge.className = 'onboard__model-name';
      badge.textContent = '已接入';
      head.append(badge);
    }
    const facts = document.createElement('span');
    facts.className = 'onboard__model-facts';
    const parts = [];
    const protocol = onboardProtocol();
    if (Number(row.contextWindow) > 0) parts.push('上下文 ' + fmtK(row.contextWindow));
    if (Number(row.maxOutputTokens) > 0) parts.push('输出 ' + fmtK(row.maxOutputTokens));
    if (row.protocol && protocol && row.protocol !== protocol) {
      parts.push('已保存 ' + protocolLabel(row.protocol) + '，与当前连接协议冲突');
    } else if (protocol) {
      parts.push('按 ' + protocolLabel(protocol) + ' 接入');
    } else {
      parts.push('请先选择连接协议');
    }
    facts.textContent = parts.join(' · ');
    body.append(head, facts);
    if (row.description) {
      const description = document.createElement('span');
      description.className = 'onboard__model-facts';
      description.textContent = row.description;
      body.append(description);
    }
    label.append(box, body);
    li.append(label);
    if (row.selected !== true) return li;

    const identity = document.createElement('label');
    identity.className = 'field field--inline';
    const identityLabel = document.createElement('span');
    identityLabel.className = 'field__label';
    identityLabel.textContent = '显示名称';
    const nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.autocomplete = 'off';
    nameInput.spellcheck = false;
    nameInput.value = row.name || '';
    nameInput.placeholder = row.upstreamModelId || row.id;
    nameInput.dataset.onboardName = key;
    nameInput.dataset.onboardModel = key;
    nameInput.dataset.onboardFocus = 'name';
    /* 输入过程中不写回显示值：用户正在输入半截名称时改写 value 会打断 IME。
       保存与渲染都读草稿里的原始字符串，提交前才 trim。 */
    nameInput.addEventListener('input', () => {
      const target = onboardRowByKey(key);
      if (!target) return;
      if (setOnboardModelName(target, nameInput.value)) markOnboardEdited();
    });
    identity.append(identityLabel, nameInput);
    li.append(identity);

    const budget = document.createElement('div');
    budget.className = 'onboard__budget';
    const capabilityText = value => typeof value === 'boolean' ? value ? '支持' : '不支持' : '待确认';
    const provenance = row.metadataKind === 'manual' ? '用户修改' : row.metadataKind === 'preset' ? '厂商资料（补全接口未声明项）' : '接口返回';
    budget.append(document.createTextNode(`来源：${provenance}。工具${capabilityText(row.tools)}，推理${capabilityText(row.reasoning)}，图片${capabilityText(row.vision)}。`));
    for (const [field, labelText] of [['contextWindow', '上下文窗口'], ['maxOutputTokens', '最大输出（可选）']]) {
      const fieldWrap = document.createElement('label');
      fieldWrap.className = 'field field--inline';
      const caption = document.createElement('span');
      caption.className = 'field__label';
      caption.textContent = labelText;
      const input = document.createElement('input');
      input.type = 'number';
      input.min = '1';
      input.step = '1';
      input.placeholder = '待确认';
      input.value = Number(row[field]) > 0 ? String(row[field]) : '';
      input.dataset.onboardBudget = field;
      input.dataset.onboardModel = key;
      input.dataset.onboardFocus = field;
      /* 输入时立即写草稿并刷新保存可用状态；不重建这一行，避免输入中丢焦点。 */
      input.addEventListener('input', () => {
        const target = onboardRowByKey(key);
        if (!target) return;
        const raw = input.value.trim();
        target[field] = raw === '' ? undefined : Number(raw);
        target.metadataKind = 'manual';
        markOnboardEdited();
        refreshOnboardSaveState();
      });
      fieldWrap.append(caption, input);
      budget.append(fieldWrap);
    }
    li.append(budget);

    if (row.reasoning === true) {
      const efforts = document.createElement('div');
      efforts.className = 'field onboard__efforts';
      const effortLabel = document.createElement('span');
      effortLabel.className = 'field__label';
      effortLabel.textContent = '思考档位';
      const choices = document.createElement('div');
      choices.className = 'chips';
      const selectedEfforts = Array.isArray(row.efforts) ? row.efforts : [];
      const known = STANDARD_ONBOARD_EFFORTS.concat(selectedEfforts)
        .filter((value, index, list) => list.indexOf(value) === index);
      for (const value of known) {
        const choice = document.createElement('label');
        choice.className = 'check';
        const check = document.createElement('input');
        check.type = 'checkbox';
        check.checked = selectedEfforts.includes(value);
        check.dataset.onboardEffort = value;
        check.dataset.onboardModel = key;
        check.dataset.onboardFocus = 'effort:' + value;
        check.addEventListener('change', () => {
          toggleOnboardModelEffort(onboardRowByKey(key), value);
          markOnboardEdited();
          renderOnboard();
        });
        const text = document.createElement('span');
        text.textContent = value;
        choice.append(check, text);
        choices.append(choice);
      }
      const hint = document.createElement('span');
      hint.className = 'field__hint';
      hint.textContent = '只勾选上游真实支持的档位；不勾选时不声明 reasoningEfforts。';
      efforts.append(effortLabel, choices, hint);
      li.append(efforts);

      const defaultField = document.createElement('label');
      defaultField.className = 'field field--inline';
      const defaultLabel = document.createElement('span');
      defaultLabel.className = 'field__label';
      defaultLabel.textContent = '默认档位';
      const select = document.createElement('select');
      select.dataset.onboardDefault = key;
      select.dataset.onboardModel = key;
      select.dataset.onboardFocus = 'default';
      select.disabled = selectedEfforts.length === 0;
      syncSelectOptions(select, [{ value: '', label: '自动' }]
        .concat(selectedEfforts.map((value) => ({ value, label: value }))), row.defaultEffort || '');
      select.addEventListener('change', () => {
        setOnboardModelDefaultEffort(onboardRowByKey(key), select.value);
        markOnboardEdited();
        renderOnboard();
      });
      defaultField.append(defaultLabel, select);
      li.append(defaultField);
    }

    const caps = document.createElement('div');
    caps.className = 'onboard__caps';
    const toggle = (cap, text, disabled = false) => {
      const capLabel = document.createElement('label');
      capLabel.className = 'check';
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.checked = row[cap] === true;
      input.disabled = disabled;
      input.dataset.onboardCap = cap;
      input.dataset.onboardModel = key;
      input.dataset.onboardFocus = cap;
      input.addEventListener('change', () => {
        const target = onboardRowByKey(key);
        if (target) {
          target[cap] = input.checked === true;
          target.metadataKind = 'manual';
          if (cap === 'reasoning' && target[cap] !== true) {
            target.efforts = [];
            delete target.defaultEffort;
          }
        }
        markOnboardEdited();
        renderOnboard();
      });
      const span = document.createElement('span');
      span.textContent = text;
      capLabel.append(input, span);
      return capLabel;
    };
    caps.append(toggle('tools', '工具调用'), toggle('reasoning', '推理'), toggle('vision', '图片输入'));
    li.append(caps);
    return li;
  });
  dom.onboardModels.replaceChildren(...nodes);
  if (focusControl) {
    const target = Array.from(dom.onboardModels.querySelectorAll('[data-onboard-focus]')).find((node) =>
      node.dataset.onboardModel === focusKey && node.dataset.onboardFocus === focusControl);
    if (target && target.focus) {
      target.focus({ preventScroll: true });
      if (typeof target.setSelectionRange === 'function' && active.selectionStart !== undefined) {
        const start = active.selectionStart;
        const end = active.selectionEnd;
        if (start !== null && end !== null) {
          try { target.setSelectionRange(start, end); } catch { /* 非文本输入忽略 */ }
        }
      }
    }
  }
}

/* 只刷新「能不能保存」与错误列表，不重建模型行：数字与名称输入在输入过程中
   调用它，重建会丢焦点和 IME 状态，也会让第一次点击落在已失效的按钮上。 */
function refreshOnboardSaveState({ selected = null } = {}) {
  const data = state.data;
  if (!data || !dom.onboardSave) return;
  const draft = state.onboard;
  const rows = onboardModelRows();
  const chosen = Array.isArray(selected) ? selected : selectedOnboardModels();
  const ready = configReady(data);
  const busy = draft.busy || draft.saving;
  const preview = chosen.length ? onboardProblems({ models: chosen }).problems : [];
  const blocking = preview.filter((text) => !text.startsWith('至少勾选'));
  setHidden(dom.onboardErrors, blocking.length === 0);
  if (dom.onboardErrors) {
    dom.onboardErrors.replaceChildren(...blocking.map((message) => {
      const p = document.createElement('p');
      p.textContent = message;
      return p;
    }));
  }
  dom.onboardSave.disabled = !ready || busy || !rows.length || !chosen.length || blocking.length > 0;
  setText(dom.onboardSave, draft.saving ? '正在保存…' : '保存');
  setText(dom.onboardDraft, rows.length
    ? (chosen.length ? '将保存 ' + fmtCount(chosen.length) + ' 个模型' : '尚未勾选模型')
    : '先读取模型，再勾选要接入的模型');
}

function renderOnboard() {
  const data = state.data;
  if (!data || !dom.onboardSave) return;
  const draft = state.onboard;
  /* 端点在输入过程中可能被改过：目录只对读它时的那个地址与预设成立。这里在
     每次渲染前对齐一次，旧目录不会随新地址继续显示。 */
  syncOnboardEndpoint();
  const rows = onboardModelRows();
  const visibleRows = filteredOnboardModels();
  const selected = selectedOnboardModels();
  const ready = configReady(data);
  const busy = draft.busy || draft.saving;
  setHidden(dom.onboardCatalogue, rows.length === 0);
  if (rows.length) {
    setText(dom.onboardCatalogueCount, '已勾选 ' + fmtCount(selected.length) + ' / ' + fmtCount(rows.length)
      + (draft.query ? '；当前显示 ' + fmtCount(visibleRows.length) : ''));
  }
  renderOnboardModels(visibleRows);
  setHidden(dom.onboardModelsEmpty, !rows.length || visibleRows.length > 0);
  if (dom.onboardSelectAll) {
    const allVisible = visibleRows.length > 0 && visibleRows.every((row) => row.selected === true);
    dom.onboardSelectAll.disabled = busy || !visibleRows.length;
    dom.onboardSelectAll.setAttribute('aria-pressed', allVisible ? 'true' : 'false');
    setText(dom.onboardSelectAll, allVisible ? '取消全选当前结果' : '全选当前结果');
  }
  const protocol = onboardProtocol();
  const protocolButtons = dom.onboardProtocolGroup?.querySelectorAll
    ? Array.from(dom.onboardProtocolGroup.querySelectorAll('[data-onboard-protocol]'))
    : [];
  protocolButtons.forEach((button) => {
    button.setAttribute('aria-pressed', button.dataset.onboardProtocol === protocol ? 'true' : 'false');
    button.disabled = busy;
  });
  if (dom.onboardProtocolHint) {
    const target = savedOnboardTarget();
    const savedProtocols = protocolList(target && target.protocols);
    const presetProtocols = protocolList(providerPreset(dom.onboardPreset?.value || '')?.protocols);
    setText(dom.onboardProtocolHint, protocol
      ? '本次新导入的模型按 ' + protocolLabel(protocol) + ' 调用上游；已保存模型保持原模型 ID 与旧会话绑定。'
      : savedProtocols.length > 1
        ? '已保存的连接同时允许 ' + savedProtocols.map(protocolLabel).join(' 与 ') + '；请选择一种后保存。'
        : presetProtocols.length > 1
          ? '该预设同时提供 ' + presetProtocols.map(protocolLabel).join(' 与 ') + '；请选择一种接口协议。'
          : '请选择 Chat Completions 或 Responses；一条连接只使用一种接口协议。');
  }
  if (dom.onboardBatchContext) dom.onboardBatchContext.disabled = busy || !selected.length;
  if (dom.onboardBatchContextApply) dom.onboardBatchContextApply.disabled = busy || !selected.length;

  refreshOnboardSaveState({ selected });

  renderOnboardOverview(data);
  const stepsDone = renderOnboardSteps(data, { selected: selected.length, rows: rows.length });
  if (dom.onboardDisclosure && dom.onboardDisclosure.dataset.initialized !== 'true') {
    const allDone = Boolean(stepsDone && stepsDone.connection && stepsDone.catalogue && stepsDone.apply && stepsDone.sync);
    dom.onboardDisclosure.open = !allDone && (!savedOnboardTarget() || !(data.models || []).length);
    dom.onboardDisclosure.dataset.initialized = 'true';
  }
  if (dom.onboardDiscover) {
    dom.onboardDiscover.disabled = !ready || busy;
    setText(dom.onboardDiscover, draft.busy ? '正在读取…' : '读取模型');
  }
  if (dom.onboardRecommended) {
    const presetId = dom.onboardPreset?.value || '';
    const available = presetOnboardModels(presetId).length > 0;
    dom.onboardRecommended.disabled = !ready || busy || !available;
    setHidden(dom.onboardRecommended, !available);
    dom.onboardRecommended.title = busy ? '请等待当前读取完成。' : ready ? '' : '配置当前不可读，暂时不能使用推荐模型。';
    if (dom.onboardRecommendedHint) {
      setHidden(dom.onboardRecommendedHint, available);
      setText(dom.onboardRecommendedHint, presetId
        ? state.providers === null
          ? '推荐目录尚未加载，请稍候。'
          : '这个预设没有内置推荐目录，请填写地址和 Key 后读取模型。'
        : '自定义连接没有内置推荐目录，请填写地址和 Key 后读取模型。');
    }
  }
}

/* 总览主卡只有一个渲染入口：renderOverviewStatus 同时维护 verdict、主动作、
   #ob-retry-apply 与 #handoff。这个旧名字保留为委托，避免模型页与向导里的
   既有调用绕过状态机再写一遍 #ob-verdict-*。 */
function renderOnboardOverview(data = state.data) {
  if (!data) return;
  renderOverviewStatus(data);
}


/* 四步进度只表达“已经真的发生了什么”，不根据按钮是否被点过来推进：
   连接已保存、目录已读回、应用已排队、原生目录已核对。 */
function renderOnboardSteps(data, { selected, rows }) {
  const saved = savedOnboardTarget();
  const applied = !connectionsChanged(data);
  const sync = syncState(data);
  const done = {
    connection: Boolean(saved),
    catalogue: mountedSavedModels(data, saved) || (rows > 0 && selected > 0),
    apply: Boolean(saved) && applied,
    sync: sync.code === 'synced' || (data?.integrationMode === 'direct' && sync.code === 'ready'),
  };
  if (!dom.onboardSteps) return done;
  const items = dom.onboardSteps.querySelectorAll ? dom.onboardSteps.querySelectorAll('[data-step]') : [];
  for (const item of items) {
    const step = item.dataset ? item.dataset.step : null;
    if (!step) continue;
    item.dataset.done = String(done[step] === true);
  }
  return done;
}

function mountedSavedModels(data, connection) {
  if (!connection) return false;
  const models = Array.isArray(data.models) ? data.models : [];
  return models.some((model) => model && model.connectionId === connection.id);
}

function openOnboardWizard() {
  if (state.editor && editorDirty()) {
    showToast('请先保存或取消正在编辑的模型，再打开添加向导。', 'error');
    return;
  }
  setView('models');
  closeEditor(true);
  if (dom.onboardDisclosure) {
    dom.onboardDisclosure.open = true;
    dom.onboardDisclosure.dataset.initialized = 'true';
    scrollMainToElement(dom.onboardDisclosure);
  }
  dom.onboardPreset?.focus({ preventScroll: true });
}

function discoveryBody(values, saved, revision) {
  if (!values.baseURL) return { error: '请先填写上游地址。' };
  const base = normalizeBaseURL(values.baseURL);
  if (base.error) return { error: base.error };

  /* 读取只需要地址与 Key。只有地址不变的已保存连接才可省略 Key；
     新连接或改过地址的连接不能借用旧凭据。 */
  const sameEndpoint = Boolean(saved && saved.baseURL === base.value);
  const typedKey = String(values.apiKey || '').trim();
  if (!typedKey && !sameEndpoint) {
    return { error: '请填写该上游的 API Key：读取只使用这一次填写的 Key，不会复用其它连接的凭据。' };
  }
  const body = { revision, baseURL: base.value };
  if (typedKey) body.apiKey = typedKey;
  if (values.providerPresetId) body.presetId = values.providerPresetId;
  if (sameEndpoint) body.connectionId = saved.id;
  return { body, sameEndpoint };
}

async function discoverConnectionEditorModels() {
  const editing = state.conns.editing;
  const data = state.data;
  if (!editing || !data || editing.discovering) return null;
  readConnectionEditor();
  const saved = editing.originalId
    ? (connectionRowsOf(data) || []).find(row => !row.legacy && row.id === editing.originalId) || null
    : null;
  const target = discoveryBody(editing.draft, saved, data.revision);
  if (target.error) { showToast(target.error, 'error'); return null; }
  editing.discovering = true;
  renderConnectionEditor();
  try {
    const res = await request(API.discovery, { method: 'POST', body: target.body });
    if (state.conns.editing !== editing) return null;
    const discovery = res && res.discovery;
    if (!discovery || !Array.isArray(discovery.models)) {
      showToast('上游目录读取结果格式不被接受。', 'error');
      return null;
    }
    if (discovery.state !== 'passed') {
      showToast('读取上游目录失败：' + (discoveryReasonText(discovery.reason) || '请稍后再试'), 'error');
      return discovery;
    }
    showToast('连接可达，读到 ' + fmtCount(discovery.modelCount) + ' 个模型；读取不会保存连接或更改已有 Key。', 'ok');
    return discovery;
  } catch (error) {
    if (state.conns.editing === editing) showToast(discoveryReasonText(error.code) || error.message || '读取上游目录失败。', 'error');
    return null;
  } finally {
    if (state.conns.editing === editing) {
      editing.discovering = false;
      renderConnectionEditor();
    }
  }
}

async function discoverOnboardModels() {
  const data = state.data;
  if (!data || state.onboard.busy) return null;
  clearOnboardSaveErrors();
  setHidden(dom.onboardErrors, true);
  const values = onboardConnectionValues();
  const saved = savedOnboardRow(values.id);
  const target = discoveryBody(values, saved, data.revision);
  if (target.error) { showToast(target.error, 'error'); return null; }
  const { body, sameEndpoint } = target;

  const key = syncOnboardEndpoint();
  const token = (state.onboard.seq || 0) + 1;
  state.onboard.seq = token;
  state.onboard.busy = true;
  if (dom.onboardDiscoverState) setText(dom.onboardDiscoverState, '正在请求上游模型目录…');
  renderOnboard();
  try {
    const res = await request(API.discovery, { method: 'POST', body });
    /* 迟到的回包不属于当前端点：地址、预设或连接已被改过，这份目录不能再写进
       向导，否则旧 endpoint 的结果会冒充新 endpoint 的答案。 */
    if (state.onboard.seq !== token || onboardEndpointKey() !== key) return null;
    const discovery = res && res.discovery;
    if (!discovery || !Array.isArray(discovery.models)) {
      showToast('上游目录读取结果格式不被接受。', 'error');
      return null;
    }
    /* 重新读取不保留上一轮的勾选：目录可能已经变了，沿用旧选择会保存一条
       上游并不存在的路由。 */
    /* 同一条上游 ID 的预设资料只补上游缺的字段；真实返回值优先。 */
    const presetModels = new Map(presetOnboardModels(values.providerPresetId)
      .map((row) => [row.upstreamModelId, row]));
    state.onboard.discoveryFor = key;
    state.onboard.discovery = {
      checkedAt: discovery.checkedAt || null,
      modelCount: discovery.modelCount,
      models: preserveOnboardModelNames(discovery.models.map((row) => onboardRowFrom(
        mergeOnboardPresetMetadata(row, presetModels.get(row && row.id)),
        { connection: sameEndpoint ? saved : null },
      )), sameEndpoint ? state.data?.models : [], values.id),
    };
    if (discovery.state !== 'passed') {
      if (dom.onboardDiscoverState) {
        setText(dom.onboardDiscoverState, discoveryReasonText(discovery.reason) || '读取上游目录失败，请检查地址与 Key。');
      }
      showToast('读取上游模型目录失败：' + (discoveryReasonText(discovery.reason) || '请稍后再试'), 'error');
      return discovery;
    }
    if (dom.onboardDiscoverState) {
      setText(dom.onboardDiscoverState, '读取到 ' + fmtCount(discovery.modelCount) + ' 个模型；勾选要接入 Kimi Work 的模型。');
    }
    showToast('读取到 ' + fmtCount(discovery.modelCount) + ' 个模型。', 'ok');
    return discovery;
  } catch (error) {
    /* 端点已被改过时，这份失败也不属于它：不要用旧请求的结论改写新端点的提示。 */
    if (state.onboard.seq !== token) return null;
    if (dom.onboardDiscoverState) setText(dom.onboardDiscoverState, discoveryReasonText(error.code) || error.message || '读取失败。');
    showToast(discoveryReasonText(error.code) || error.message || '读取上游模型目录失败。', 'error');
    return null;
  } finally {
    if (state.onboard.seq === token) state.onboard.busy = false;
    renderOnboard();
  }
}

function useRecommendedOnboardModels() {
  const presetId = dom.onboardPreset?.value;
  const models = presetOnboardModels(presetId);
  if (!models.length) {
    showToast('请先选择厂商、地区与服务类型；自定义服务请读取模型或手动添加。', 'error');
    return null;
  }
  clearOnboardSaveErrors();
  const connection = savedOnboardRow(onboardConnectionValues().id);
  state.onboard.discoveryFor = onboardEndpointKey();
  state.onboard.discovery = {
    checkedAt: null,
    modelCount: models.length,
    models: models.map((row) => onboardRowFrom({
      ...row,
      metadataSource: presetMetadataSource(row),
    }, { connection })),
  };
  renderOnboard();
  return models;
}

/* 保存时连接是整组替换：同 ID 的已保存连接必须带回它的 apiKeyEnv（Key 引用），
   否则后端会按默认为它生成新的环境变量名，已保存的 Key 就不再被这个连接引用。
   协议只发送向导里明确选中的唯一值；没有选择时由校验拦下，不发默认值。 */
function onboardConnectionPayload(values, saved) {
  const protocol = values.protocol;
  return {
    id: values.id,
    name: values.name,
    ...(values.providerPresetId ? { providerPresetId: values.providerPresetId } : {}),
    baseURL: values.baseURL,
    ...(saved && saved.apiKeyEnv ? { apiKeyEnv: saved.apiKeyEnv } : {}),
    ...(protocol === 'chat' || protocol === 'responses' ? { protocols: [protocol] } : {}),
    ...(values.apiKey.trim() ? { apiKey: values.apiKey.trim() } : {}),
  };
}

async function saveOnboard() {
  const data = state.data;
  if (!data || state.onboard.saving) return false;
  /* 与模型保存同一条规则：连接抽屉里的输入先并入草稿，否则整组提交会把它丢掉。 */
  if (state.conns.editing) {
    applyConnectionEditor();
    if (state.conns.editing) {
      showToast('还有一条连接没有通过校验。请先在连接编辑器里修正或取消，再保存。', 'error');
      return false;
    }
  }
  const draftOthers = (state.conns.draft || [])
    .filter((row) => row && !row.legacy && row.id !== onboardConnectionValues().id);
  const savedOthers = (connectionRowsOf(data) || [])
    .filter((row) => row && !row.legacy && row.id !== onboardConnectionValues().id);
  const otherProtocolProblems = connectionProtocolErrors(draftOthers.length ? draftOthers : savedOthers);
  if (otherProtocolProblems.length) {
    renderErrorList(dom.onboardErrors, otherProtocolProblems);
    return false;
  }
  const selected = selectedOnboardModels();
  const { problems, built, values } = onboardProblems({ models: selected });
  if (problems.length) {
    renderErrorList(dom.onboardErrors, problems);
    return false;
  }
  state.onboard.saving = true;
  clearOnboardSaveErrors();
  renderOnboard();
  renderActions(data);
  const epoch = state.stateReadEpoch;
  const key = state.key;
  const revision = state.data.revision;
  const current = () => epoch === state.stateReadEpoch && key === state.key;

  try {
    const saved = savedOnboardRow(values.id);
    const connection = onboardConnectionPayload(values, saved);
    /* 已保存的其它连接必须原样带回：这个端点是整组替换，漏掉就把别的上游删了。
       草稿里对其它连接的未保存修改优先保留，避免它们被服务端旧值覆盖。 */
    const drafts = state.conns.draft || [];
    const others = connectionPayload(drafts.filter((row) => row && !row.legacy && row.id !== values.id));
    if (!others.length) {
      for (const row of connectionRowsOf(state.data) || []) {
        if (row.legacy || row.id === values.id) continue;
        others.push(...connectionPayload([connectionDraftRow(row)]));
      }
    }
    const existing = Array.isArray(state.data.models) ? state.data.models : [];
    const catalogue = mergeOnboardModels(existing, built, values.id);
    const result = await request(API.workspace, { method: 'POST', body: {
      revision, connections: [...others, connection], models: catalogue,
    } });
    if (!result?.state) throw new ApiError('保存没有返回状态', 'STATE', 200);
    if (!current()) return false;
    if (dom.onboardKey) dom.onboardKey.value = '';
    if (dom.onboardDisclosure) dom.onboardDisclosure.open = false;
    state.conns.base = savedConnectionsSignature(result.state);
    state.conns.baseRevision = result.state.revision ?? null;
    state.conns.baseEntity = connectionDraftFrom(result.state);
    state.conns.draft = connectionDraftFrom(result.state);
    state.conns.dirty = false;
    state.onboard.touched = false;
    state.onboard.saving = false;
    adoptState(result.state, { external: true });
    state.onboard.discovery = null;
    state.onboard.discoveryFor = null;
    state.onboard.seq = 0;
    fillOnboardForm();
    state.lastSync = Date.now();
    renderAll();
    showToast('已保存 ' + fmtCount(built.length) + ' 个模型和服务商；后台会自动同步到 Kimi Work。', 'ok');
    return true;
  } catch (error) {
    if (current()) {
      state.onboard.saving = false;
      await handleSaveFailure(error, 'onboard');
    }
    return false;
  } finally {
    if (current()) {
      state.onboard.saving = false;
      if (state.data) {
        renderOnboard();
        renderActions(state.data);
      }
    }
  }
}

/* 预设建议值只有在字段仍等于「上一预设/已保存回填值」时才覆盖：用户手工
   改过的标识与名称必须保留，否则加第二条服务商时会写回同一条旧连接。 */
function onboardSuggestedId(preset) {
  return preset ? presetsSuggestedId(preset) : '';
}

function onboardSuggestedName(preset) {
  return preset ? (preset.name || preset.id || '') : '';
}

function staleSuggested(value, candidates) {
  return String(value || '').trim() === '' || candidates.some((candidate) => candidate !== '' && value === candidate);
}

/* 预设只填地址与已知协议，绝不猜 Key；也不覆盖用户手改过的标识与名称。 */
function applyOnboardPreset(presetId) {
  if (!dom.onboardBase) return;
  state.onboard.touched = true;
  clearOnboardSaveErrors();
  const preset = providerPreset(presetId);
  const previousId = dom.onboardPreset.dataset.presetId || '';
  const previousPreset = providerPreset(previousId);
  const savedId = savedOnboardTarget()?.id || '';
  const savedName = savedOnboardTarget()?.name || '';
  if (previousId !== presetId && dom.onboardKey) dom.onboardKey.value = '';
  dom.onboardPreset.dataset.presetId = presetId;
  /* 切到自定义时立刻按当前地址重画：目录归属会随预设变化而清空。 */
  if (!preset) {
    if (dom.onboardPresetHint) setText(dom.onboardPresetHint, '已选自定义；地址仍由你填写。');
    renderOnboard();
    return;
  }
  if (preset.baseURL) dom.onboardBase.value = preset.baseURL;
  const presetProtocols = protocolList(preset.protocols);
  if (presetProtocols.length === 1) state.onboard.protocol = presetProtocols[0];
  else if (presetProtocols.length > 1 && !presetProtocols.includes(state.onboard.protocol)) state.onboard.protocol = null;
  else if (!presetProtocols.length && !state.onboard.protocol) state.onboard.protocol = null;
  state.onboard.protocolTouched = true;
  const nextId = onboardSuggestedId(preset);
  if (dom.onboardId && nextId
    && staleSuggested(dom.onboardId.value, [savedId, onboardSuggestedId(previousPreset)])) {
    dom.onboardId.value = nextId;
  }
  const nextName = onboardSuggestedName(preset);
  if (dom.onboardName && nextName
    && staleSuggested(dom.onboardName.value, [savedName, onboardSuggestedName(previousPreset)])) {
    dom.onboardName.value = nextName;
  }
  if (dom.onboardPresetHint) setText(dom.onboardPresetHint, preset.name
    ? '已套用「' + preset.name + '」的地址与已知协议；仍可改为自定义地址。'
    : '已套用预设地址；仍可改为自定义地址。');
  renderOnboard();
}

function providerPresetLabel(preset) {
  const region = preset.region === 'cn' ? '国内' : preset.region === 'global' ? '海外' : '';
  const plan = preset.kind === 'coding' ? 'Coding Plan' : '按量 API';
  return [preset.familyName || preset.name || preset.id, region, plan, preset.name]
    .filter((value, index, list) => value && list.indexOf(value) === index).join(' · ');
}

function providerPresetOptions() {
  return [{ value: '', label: '自定义（不套用预设）' }].concat(providerPresets().map(preset => ({
    value: preset.id, label: providerPresetLabel(preset),
  })));
}

function fillOnboardPresets() {
  if (!dom.onboardPreset) return;
  const presets = providerPresets();
  if (dom.onboardPreset.dataset?.providerFlow !== 'managed') {
    const current = dom.onboardPreset.value || '';
    syncSelectOptions(dom.onboardPreset, providerPresetOptions(), current);
  }
  if (dom.onboardPresetHint) {
    setText(dom.onboardPresetHint, presets.length
      ? '共 ' + fmtCount(presets.length) + ' 个供应商预设；选择后填入官方地址与已知协议。'
      : '预设列表不可用；仍可直接填写地址。');
  }
}


/* ---------- render: connection ---------- */

/* 重启说明区分受管后台与前台运行；页面不会猜测新会话密钥。
   桌面窗口由托盘读取最新私有入口，Kimi 的正常退出与受管重启只由显式动作触发。 */
/* 应用连接的主路径：先由用户点一次，之后由 M63 的 runner 在安全时应用。
   页面自身不猜新会话密钥：窗口版读取私有入口换页，浏览器版只如实说明连接状态。

   M63 的契约：POST /api/actions 返回 202 {operation:{state}}，state 只有
   requested / applied / waiting / failed；真实进展在 GET /api/state 的
   connections.apply.state（accepted|waiting|applied|expired|superseded|failed）。
   缺受管 runner 时是 409 apply_unavailable，附 error.reason。 */
const APPLY_COPY = {
  idle: { tone: 'ok', word: '已应用', note: '当前网关正在使用已保存的连接与凭据。' },
  pending: { tone: 'warn', word: '等待应用',
    note: '连接已保存，后台会自动应用到网关；若长时间未生效，请到「设置 → 接入与更新 → 高级排障」重新同步模型目录。' },
  applying: { tone: 'warn', word: '正在应用',
    note: '后台服务正在应用连接；Kimi 忙时会等待空闲，不会打断正在进行的请求。' },
  waiting: { tone: 'warn', word: '等待空闲',
    note: '后台服务会继续重试，条件恢复后自动应用；不会结束正在运行的任务。' },
  restarting: { tone: 'warn', word: '面板重启中',
    note: '面板正在重启以使用新连接。窗口会自动重新加载，无需操作。' },
  expired: { tone: 'bad', word: '应用已超时',
    note: '一直没有等到安全的空闲窗口，连接仍未应用；可稍后重试。' },
  superseded: { tone: 'warn', word: '已被更新的保存取代',
    note: '这次应用的目标已被后来的保存取代；按当前保存值重新应用即可。' },
  unsupported: { tone: 'bad', word: '自动应用不可用',
    note: '连接已保存，当前网关尚未应用；没有可自动应用的受管服务。' },
  failed: { tone: 'bad', word: '应用失败', note: '连接已保存，但应用未完成；可重试，不会结束正在运行的任务。' },
  /* 旧版本凭据条目仍在钥匙串，当前版本尚未获得迁移读取许可。
     这里不能说成 Key 丢失，也不能要求重新输入 Key。 */
  authorization: { tone: 'bad', word: '连接凭据待授权', credential: true,
    note: '旧版本保存的 Key 仍在系统钥匙串里，只是当前版本还没获得读取许可。点「重新授权并应用连接」并按系统提示允许一次即可迁移；成功后正常重启和后续升级不再需要钥匙串授权。取消或拒绝不会改动已保存的连接，可随时重试。' },
  authorizationCancelled: { tone: 'warn', word: '已取消系统授权', credential: true,
    note: '你取消了系统授权窗口：已保存的 Key 与连接配置都没有改动，当前网关仍读不到它们。可再次点「重新授权并应用连接」，按系统提示允许一次即可完成迁移。' },
  authorizationDenied: { tone: 'bad', word: '系统未允许读取凭据', credential: true,
    note: '系统拒绝了这次读取已保存 Key 的授权，连接配置未被改动。若在钥匙串窗口点过「拒绝」，可在「登录钥匙串」里重新允许本应用，或再次点「重新授权并应用连接」。' },
  keychainLocked: { tone: 'warn', word: '登录钥匙串已锁定', credential: true,
    note: '系统读不到登录钥匙串里的已保存 Key。请先在 macOS 中解锁登录钥匙串，再点「重新授权并应用连接」；这一步不会改写已保存的连接。' },
  keychainUnavailable: { tone: 'bad', word: '登录钥匙串不可用', credential: true,
    note: '请先在 macOS 的「钥匙串访问」中确认登录钥匙串可用，然后重新授权。' },
  sessionUnavailable: { tone: 'bad', word: '当前会话无法授权', credential: true,
    note: '当前进程不在可显示系统授权窗口的登录会话里，读不到已保存 Key。请从应用窗口操作；连接配置未被改动。' },
};

const APPLY_REFUSAL = {
  service_not_installed: '没有可自动应用的后台服务（当前面板以前台方式运行，或未安装服务）。',
  service_foreign: '检测到的后台服务不属于当前配置，未使用它应用连接。',
  service_stopped: '后台服务当前没有运行，无法自动应用连接。',
};

/* 旧版本钥匙串中的条目可能仍受签名 ACL 或锁屏状态限制。迁移时网关照常运行、
   只是读不到旧 Key：必须说成“待授权”，不能说成 Key 丢失或需要重新输入。
   本地密文已经可读时不会进入这条路径；缺条目仍按缺失处理。 */
const CREDENTIAL_VAULT_BLOCKED = ['credential_keychain_locked', 'credential_keychain_unavailable',
  'credential_session_unavailable', 'credential_authorization_required', 'credential_authorization_failed'];

function credentialAuthPending(data) {
  const rows = connectionRowsOf(data) || [];
  return rows.some((row) => CREDENTIAL_VAULT_BLOCKED.includes(row.credentialError));
}

/* 最近一次 apply-connections 的具体失败原因。轮询里 connections.apply 只会
   说「凭据不可读」，取消/拒绝/锁定这些真正影响下一步的原因只在 operation 上，
   所以两处都要看，并且 operation 的说法优先。 */
function credentialApplyReason(data) {
  const op = data?.operation;
  if (op && op.action === 'apply-connections' && typeof op.reason === 'string' && op.reason) return op.reason;
  const reason = data?.connections?.apply?.reason;
  return typeof reason === 'string' ? reason : '';
}

function isManagedBackend(service) {
  if (!service || typeof service !== 'object') return false;
  return service.mode === 'service' || (service.mode === 'release' && service.state === 'running');
}

/* 是否需要应用，不能只看连接对象是否相同：只换 Key、地址不变时，
   connections.restartRequired 仍是判断“尚未生效”的唯一权威；对象比较会漏掉
   这种情况并把旧进程说成已经应用。服务端没有给出该标志时，退回对象比较。 */
function applyAvailability(data) {
  const connections = data && data.connections;
  const explicit = connections && typeof connections.restartRequired === 'boolean'
    ? connections.restartRequired : null;
  if (explicit === false) return 'idle';
  if (explicit === true) {
    if (!isManagedBackend(data && data.service)) return 'unsupported';
    return 'pending';
  }
  if (!connectionsChanged(data)) return 'idle';
  if (!isManagedBackend(data && data.service)) return 'unsupported';
  return 'pending';
}

/* 优先读 runner 写回的真实进展；没有时按本地可判断的事实降级。 */
function applyState(data) {
  if (data?.operation?.action === 'apply-connections' && isInFlight(data.operation)) return APPLY_COPY.applying;
  /* 同一批连接读不到时，runner 只记 credentials_unavailable；取消、拒绝、
     钥匙串锁定这些决定下一步的原因只在 operation.reason 上。先看具体原因，
     避免把用户主动取消说成「需要授权」或「密钥未保存」。 */
  if (data?.connections?.apply?.state === 'failed' && data.connections.apply.reason === 'credentials_unavailable') {
    const reason = credentialApplyReason(data);
    if (reason && reason !== 'credentials_unavailable') {
      if (reason === 'credential_authorization_cancelled') return APPLY_COPY.authorizationCancelled;
      if (reason === 'credential_authorization_failed') return APPLY_COPY.authorizationDenied;
      if (reason === 'credential_keychain_locked') return APPLY_COPY.keychainLocked;
      if (reason === 'credential_session_unavailable') return APPLY_COPY.sessionUnavailable;
      if (reason === 'credential_keychain_unavailable') return APPLY_COPY.keychainUnavailable;
    }
    return APPLY_COPY.authorization;
  }
  const code = applyAvailability(data);
  /* 后端明确拒绝过（缺受管 runner）：在真正落地之前不得改回“等待应用”，
     也不得因为随后一次没有 service 字段的渲染就翻成“已应用”。落地后
     restartRequired 会变 false，此时以数据为准并清除该标记。 */
  if (state.applyUnsupported) {
    const apply = data && data.connections && data.connections.apply;
    if (apply && (apply.state === 'applied' || apply.state === 'superseded')) {
      state.applyUnsupported = false;
      state.applyRefusal = null;
    } else if (code !== 'idle' || !data || !data.service) {
      return state.applyRefusal
        ? { ...APPLY_COPY.unsupported, note: state.applyRefusal }
        : APPLY_COPY.unsupported;
    } else {
      state.applyUnsupported = false;
      state.applyRefusal = null;
    }
  }
  if (code === 'idle') return APPLY_COPY.idle;
  const apply = data && data.connections && data.connections.apply;
  const view = apply && typeof apply === 'object' ? apply : null;
  if (view && typeof view.state === 'string') {
    if (view.state === 'applied') return APPLY_COPY.idle;
    if (view.state === 'accepted') return APPLY_COPY.applying;
    if (view.state === 'waiting') return APPLY_COPY.waiting;
    if (view.state === 'expired') return APPLY_COPY.expired;
    if (view.state === 'superseded') return APPLY_COPY.superseded;
    if (view.state === 'failed') return APPLY_COPY.failed;
  }
  const op = data && data.operation;
  if (op && op.action === 'apply-connections') {
    if (isInFlight(op)) return APPLY_COPY.applying;
    if (op.state === 'failed' || op.state === 'unavailable' || op.state === 'interrupted') return APPLY_COPY.failed;
    if (op.state === 'waiting') return APPLY_COPY.waiting;
    /* requested 已交给 runner，但新进程还没证明生效：这是“重启中”，不是“已应用”。 */
    if (op.state === 'requested' || op.state === 'applied') return APPLY_COPY.restarting;
  }
  return code === 'pending' ? APPLY_COPY.pending : APPLY_COPY.unsupported;
}

function renderApply(data) {
  const running = isManagedBackend(data && data.service);
  const stateInfo = applyState(data);
  /* 待授权、已取消、被拒绝、钥匙串锁定都要保留同一个重授权入口：
     这些状态共用一条恢复路径，不能因为措辞不同就把按钮藏起来。 */
  const credentialPrompt = stateInfo.credential === true;
  if (dom.restartHead) {
    setText(dom.restartHead, credentialPrompt
      ? '由当前窗口向系统申请读取已保存的连接凭据，后台不会自动弹出授权。'
      : running ? '本面板已连接本机后台服务；保存连接后会自动或按需应用，不需要终端命令。'
        : '本面板当前未连接运行中的后台服务；连接保存后需要由后台服务应用。');
  }
  if (dom.applyVerdict) {
    setDot(dom.applyVerdictDot, stateInfo.tone);
    setText(dom.applyVerdictWord, stateInfo.word);
    setText(dom.applyVerdictNote, stateInfo.note);
  }
  /* 保存后网关已热应用，日常不再提供「应用已保存的连接」。只有旧版凭据
     需要系统授权迁移时，才在这里保留同一个重授权入口。 */
  if (dom.applyButton) {
    setHidden(dom.applyButton, !credentialPrompt);
    dom.applyButton.disabled = !credentialPrompt || Boolean(state.pendingAction);
    setText(dom.applyButton, '重新授权并应用连接');
  }
}

async function applyConnections() {
  const data = state.data;
  if (!data || state.pendingAction) return null;
  if (isInFlight(data.operation)) {
    showToast('已有操作正在进行，等待它结束后再试。', 'error');
    return null;
  }
  state.pendingAction = 'apply-connections';
  state.applyUnsupported = false;
  state.applyRefusal = null;
  if (dom.applyButton) dom.applyButton.disabled = true;
  renderApply(data);
  try {
    const res = await request(API.actions, { method: 'POST', body: { id: actionUUID(), at: Date.now(), action: 'apply-connections' } });
    const op = res && res.operation ? res.operation : null;
    if (op) {
      state.pendingOpId = op.id || null;
      adoptState({ ...state.data, operation: op });
      renderApply(state.data);
    }
    const isWindows = typeof document !== 'undefined' && document.documentElement?.dataset?.platform === 'windows';
    showToast(credentialAuthPending(data)
      ? (isWindows
        ? '正在向系统申请读取待迁移的旧版连接 Key，请按系统提示完成授权。'
        : '正在向系统申请读取待迁移的旧版连接 Key：如弹出 macOS「登录钥匙串」窗口，请选择「始终允许」以完成一次迁移。')
      : '应用请求已提交，后台服务正在按已保存连接安全应用。', 'ok');
    await loadState({ silent: true });
    return op;
  } catch (error) {
    if (error.code === 'NO_KEY' || error.code === 'unauthorized') {
      clearKey();
      stopPolling();
      showGate('expired');
      return null;
    }
    if (error.status === 0) {
      showToast('无法连接本机面板服务，应用结果尚未确认。请先刷新状态再决定是否重试。', 'error');
      return null;
    }
    /* 缺受管 runner 是 M63 的 409 apply_unavailable；旧后端则是 invalid_panel_action。
       两种情况都必须说“已保存、尚未应用”，不能把保存当成生效。 */
    const unsupported = ['invalid_panel_action', 'apply_unavailable', 'apply_connections_unavailable',
      'service_unavailable', 'unmanaged_service', 'method_not_allowed', 'not_found'].includes(error.code);
    if (unsupported) {
      /* 记在状态里而不是只写一次文案：轮询与后续渲染都要保持同一个结论。 */
      state.applyUnsupported = true;
      const refusal = APPLY_REFUSAL[error.reason] || null;
      state.applyRefusal = refusal;
      setDot(dom.applyVerdictDot, APPLY_COPY.unsupported.tone);
      setText(dom.applyVerdictWord, APPLY_COPY.unsupported.word);
      setText(dom.applyVerdictNote, refusal || APPLY_COPY.unsupported.note);
      showToast(refusal
        ? '连接已保存，但当前没有可自动应用的受管服务。'
        : '连接已保存，当前网关尚未应用；自动应用不可用。', 'error');
      return null;
    }
    showToast(error.message || '应用请求未完成，请稍后重试。', 'error');
    await loadState({ silent: true });
    return null;
  } finally {
    if (state.pendingAction === 'apply-connections') state.pendingAction = null;
    if (state.data) renderApply(state.data);
  }
}


function renderConnection() {
  const data = state.data;
  if (!data) return;

  const config = data.configuration || {};
  if (dom.restartState) {
    const restartRequired = connectionsChanged(data);
    setText(dom.restartState, config.state === 'unavailable'
      ? '无法读取配置文件。修正后再保存。'
      : restartRequired
        ? '连接或模型已有保存的修改，但当前网关仍在用启动时读取的配置。模型保存已暂停；应用连接后继续。'
        : config.state === 'ready'
          ? '当前配置文件已生效，没有待应用的修改。'
          : '配置文件已修改，当前网关仍在使用启动时读取的连接。应用连接后生效。');
  }
  renderApply(data);
  setText(dom.restartFile, (config.file || '—') + ' · 修订 ' + shortRev(data.revision));

  renderConnections();
}

/* ---------- render: named connections ---------- */

function renderConnections() {
  const data = state.data;
  if (!data) return;
  const available = connectionsAvailable(data);
  setHidden(dom.connsUnavailable, available);
  if (!available) {
    setHidden(dom.connsList, true);
    setHidden(dom.connsEmpty, true);
    setHidden(dom.connEditor, true);
    setHidden(dom.connsSave, true);
    setHidden(dom.connsAdd, true);
    setHidden(dom.connsReset, true);
    setHidden(dom.connsDiff, true);
    return;
  }

  const rows = state.conns.draft || [];
  const named = rows.filter((row) => !row.legacy);
  setHidden(dom.connsList, false);
  setHidden(dom.connsEmpty, named.length > 0);
  setHidden(dom.connsSave, false);
  setHidden(dom.connsAdd, false);
  setHidden(dom.connsReset, false);

  const config = data.configuration || {};
  const blocked = config.state === 'unavailable';
  const dirty = connectionsDirty();
  state.conns.dirty = dirty;

  if (dom.connsSave) {
    dom.connsSave.disabled = !dirty || state.connsSaving || blocked;
    setText(dom.connsSave, state.connsSaving ? '正在保存…' : '保存');
  }
  if (dom.connsAdd) dom.connsAdd.disabled = state.connsSaving || blocked;
  if (dom.connsReset) dom.connsReset.disabled = !dirty || state.connsSaving;
  if (dom.connsDraft) {
    setText(dom.connsDraft, blocked ? '配置文件不可读，无法保存连接'
      : dirty ? '草稿尚未保存；保存后需重启面板才会生效' : '草稿与配置文件一致');
    dom.connsDraft.dataset.dirty = String(dirty);
  }

  if (dom.connsErrors) {
    /* 抽屉里的行内校验与会话级错误分开保存：轮询触发的重渲染不能把
       「凭据文件不可用」这类提示抹掉，否则用户会反复重试同一个错误。 */
    renderErrorList(dom.connsErrors, state.conns.errors || []);
  }

  if (dom.connsList) {
    const signature = JSON.stringify([rows, state.connsSaving, config.state, state.connChecks]);
    if (dom.connsList.dataset.signature !== signature) {
      dom.connsList.dataset.signature = signature;
      dom.connsList.replaceChildren(...rows.map((row, index) => buildConnectionRow(row, index)));
    }
  }

  renderConnectionEditor();
  renderConnectionsDiff();
}

/* 连接行的 Key 状态必须和下方授权卡片说同一件事：条目还在、只是当前版本
   读不到时，让用户去点重授权，而不是让用户重输一遍 Key。 */
function connectionKeyText(row) {
  if (row.apiKey) return '本次已输入新 Key（未保存）';
  if (row.apiKeySet) return 'Key 已在运行环境中';
  const code = row.credentialError;
  if (!code) return 'Key 未在运行环境中';
  if (CREDENTIAL_VAULT_BLOCKED.includes(code)) {
    return '已保存 Key 待系统授权：请在下方点「重新授权并应用连接」';
  }
  if (code === 'credential_master_key_invalid') {
    return '本机凭据主密钥缺失或无效，请保留应用数据并按下方提示恢复';
  }
  if (code === 'credential_store_corrupt' || code === 'credential_store_failed') {
    return '已保存 Key 无法读取，请编辑并重新输入 Key 后保存';
  }
  return '凭据不可读，请编辑并重新输入 Key 后保存';
}

function buildConnectionRow(row, index) {
  const li = document.createElement('li');
  li.className = 'conn-row';
  li.dataset.connectionId = row.id || '';
  if (row.legacy) li.dataset.legacy = 'true';

  const main = document.createElement('div');
  main.className = 'conn-row__main';
  const title = document.createElement('p');
  title.className = 'conn-row__name';
  const name = document.createElement('span');
  name.textContent = row.legacy ? '旧版单上游' : row.name || row.id;
  title.append(name);
  if (row.legacy) {
    const tag = document.createElement('span');
    tag.className = 'tag';
    tag.textContent = '兼容';
    title.append(tag);
  }
  const url = document.createElement('p');
  url.className = 'conn-row__url mono';
  url.textContent = row.baseURL || '—';
  url.setAttribute('title', row.baseURL || '');
  main.append(title, url);

  const meta = document.createElement('div');
  meta.className = 'conn-row__meta';
  const keyState = document.createElement('p');
  keyState.className = 'conn-row__line';
  const dot = document.createElement('span');
  dot.className = 'dot';
  dot.dataset.tone = row.apiKeySet || row.apiKey ? 'ok' : 'warn';
  const keyText = document.createElement('span');
  keyText.textContent = connectionKeyText(row);
  keyState.append(dot, keyText);
  const models = document.createElement('p');
  models.className = 'conn-row__line muted';
  models.textContent = (row.modelCount === null ? '模型数未知' : row.modelCount + ' 个模型')
    + ' · ' + connectionProtocolText(row);
  meta.append(keyState, models);

  /* 行内检查只探测本行连接；结果与按钮留在同一行，不新增任何导航或诊断页。 */
  const check = state.connChecks[row.id];
  const checkLine = document.createElement('p');
  checkLine.className = 'conn-row__line conn-row__check';
  const checkDot = document.createElement('span');
  checkDot.className = 'dot';
  const checkText = document.createElement('span');
  if (!check) {
    checkDot.dataset.tone = 'neutral';
    checkText.textContent = '尚未检查本行连接';
  } else if (check.busy) {
    checkDot.dataset.tone = 'warn';
    checkText.textContent = '正在检查本行连接…';
  } else if (check.state === 'passed') {
    checkDot.dataset.tone = 'ok';
    checkText.textContent = '连接可用' + (Number.isInteger(check.modelCount) ? '：' + check.modelCount + ' 个模型'
      : '') + (Number.isFinite(check.ms) ? ' · ' + fmtMs(check.ms) : '');
  } else {
    checkDot.dataset.tone = 'bad';
    checkText.textContent = '检查未通过：' + (upstreamReasonText(check.reason) || '无法连接本行上游');
  }
  checkLine.append(checkDot, checkText);
  meta.append(checkLine);

  const actions = document.createElement('div');
  actions.className = 'conn-row__actions';
  if (!row.legacy) {
    const checkButton = document.createElement('button');
    checkButton.type = 'button';
    checkButton.className = 'btn btn--quiet btn--sm';
    checkButton.dataset.connectionCheck = row.id;
    checkButton.disabled = Boolean(check && check.busy) || state.connsSaving;
    checkButton.textContent = check && check.busy ? '正在检查…' : '检查连接';
    const edit = document.createElement('button');
    edit.type = 'button';
    edit.className = 'btn btn--quiet btn--sm';
    edit.dataset.connectionEdit = row.id;
    edit.textContent = '编辑';
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'btn btn--quiet btn--sm';
    remove.dataset.connectionRemove = row.id;
    remove.textContent = '移除';
    actions.append(checkButton, edit, remove);
  } else {
    const note = document.createElement('span');
    note.className = 'hint hint--inline';
    note.textContent = '旧配置仍在使用';
    actions.append(note);
  }

  li.append(main, meta, actions);
  return li;
}

function protocolLabel(value) {
  return value === 'chat' ? 'Chat' : value === 'responses' ? 'Responses' : value;
}

/* 连接行内检查的失败说明：沿用 discovery 的错误码表，再补连接级拒绝。
   result 里带回了 connectionId，只有与所在行匹配的结果才被采纳。 */
function upstreamReasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  return UPSTREAM_REASON[code] || DIAG_REASON[code] || code;
}

function renderConnectionEditor({ force = false } = {}) {
  const editing = state.conns.editing;
  if (!editing) {
    setHidden(dom.connEditor, true);
    return;
  }
  setHidden(dom.connEditor, false);
  setText(dom.connEditorTitle, editing.originalId ? '编辑连接' : '添加连接');
  const draft = editing.draft;
  if (dom.ceName && (force || !isInputActive(dom.ceName)) && dom.ceName.value !== (draft.name || '')) dom.ceName.value = draft.name || '';
  if (dom.ceId) {
    if ((force || !isInputActive(dom.ceId)) && dom.ceId.value !== (draft.id || '')) dom.ceId.value = draft.id || '';
    dom.ceId.disabled = Boolean(editing.originalId);
  }
  if (dom.ceBase && (force || !isInputActive(dom.ceBase)) && dom.ceBase.value !== (draft.baseURL || '')) dom.ceBase.value = draft.baseURL || '';
  if (dom.ceKeyEnv && (force || !isInputActive(dom.ceKeyEnv)) && dom.ceKeyEnv.value !== (draft.apiKeyEnv || '')) dom.ceKeyEnv.value = draft.apiKeyEnv || '';
  const draftProtocol = connectionProtocol(draft);
  if (dom.ceChat) dom.ceChat.checked = draftProtocol === 'chat';
  if (dom.ceResponses) dom.ceResponses.checked = draftProtocol === 'responses';
  if (dom.ceProtocolHint) {
    const listed = protocolList(draft.protocols);
    setText(dom.ceProtocolHint, listed.length > 1
      ? '已保存的连接同时允许 ' + listed.map(protocolLabel).join(' 与 ') + '；请选择一种后保存。'
      : draftProtocol
        ? '当前使用 ' + protocolLabel(draftProtocol) + '；新模型会按它调用上游。'
        : '请选择 Chat Completions 或 Responses；一条连接只使用一种接口协议。');
  }
  if (dom.cePreset) {
    const desired = draft.providerPresetId || '';
    if (dom.cePreset.dataset?.providerFlow === 'managed') {
      if (force || (!isInputActive(dom.cePreset) && dom.cePreset.value !== desired)) {
        dom.cePreset.dataset.desiredPreset = desired;
        if (typeof Event === 'function' && typeof dom.cePreset.dispatchEvent === 'function') {
          dom.cePreset.dispatchEvent(new Event('kwp:sync-provider-flow'));
        }
      }
    } else {
      syncSelectOptions(dom.cePreset, providerPresetOptions(), desired);
    }
  }
  if (dom.cePresetHint) {
    const preset = providerPreset(draft.providerPresetId);
    setText(dom.cePresetHint, preset
      ? '预设来源：' + (preset.source && preset.source.url ? preset.source.url : '未提供')
        + (preset.notes ? ' · ' + preset.notes : '')
      : '选择预设会填入官方地址与已知能力；仍可改为自定义地址。');
  }
  if (dom.ceKeyHint) {
    setText(dom.ceKeyHint, editing.originalId
      ? '已保存的 Key 不会显示。留空表示保持现有值。'
      : '新连接必须填写 API Key；只写入本机私有凭据文件，不回显。');
  }
  if (dom.ceKey) {
    if (force || (!editing.keyEdited && !isInputActive(dom.ceKey))) dom.ceKey.value = '';
    dom.ceKey.placeholder = editing.originalId ? '留空保持现有 Key' : 'sk-…';
  }
  if (dom.connEditorTest) {
    dom.connEditorTest.disabled = editing.discovering === true;
    setText(dom.connEditorTest, editing.discovering ? '正在读取…' : '读取上游目录');
    setHidden(dom.connEditorTest, false);
  }
}

/* 差异用「已保存 → 草稿」的成对值展示，让「改了但没重启」无法被误读成已生效。 */
function renderConnectionsDiff() {
  if (!dom.connsDiff) return;
  const available = connectionsAvailable(state.data);
  if (!available) {
    setHidden(dom.connsDiff, true);
    return;
  }
  const saved = connectionDraftFrom(state.data) || [];
  const draft = state.conns.draft || [];
  const savedNamed = saved.filter((row) => !row.legacy);
  const draftNamed = draft.filter((row) => !row.legacy);
  const lines = [];
  const savedById = new Map(savedNamed.map((row) => [row.id, row]));
  for (const row of draftNamed) {
    const before = savedById.get(row.id);
    if (!before) {
      lines.push(['新增连接', '—', row.name + ' · ' + row.baseURL]);
      continue;
    }
    if (before.name !== row.name) lines.push(['连接名称', before.name, row.name]);
    if (before.baseURL !== row.baseURL) lines.push(['上游地址', before.baseURL, row.baseURL]);
    if ((before.protocols || []).join(',') !== (row.protocols || []).join(',')) {
      lines.push(['接口协议', connectionProtocolText(before), connectionProtocolText(row)]);
    }
    if (row.apiKey) lines.push(['API Key', '已保存（不回显）', '本次将替换']);
  }
  const draftIds = new Set(draftNamed.map((row) => row.id));
  for (const row of savedNamed) {
    if (!draftIds.has(row.id)) lines.push(['移除连接', row.name + ' · ' + row.baseURL, '—']);
  }
  const removedLegacy = saved.some((row) => row.legacy) && !draft.some((row) => row.legacy);
  if (removedLegacy) lines.push(['旧版单上游', '仍保留', '已移除']);
  if (!lines.length) {
    setHidden(dom.connsDiff, true);
    return;
  }
  setHidden(dom.connsDiff, false);
  const title = document.createElement('p');
  title.className = 'diff__title';
  title.textContent = '待重启生效的差异（已保存 → 草稿）';
  const list = document.createElement('dl');
  list.className = 'diff__list';
  lines.forEach(([label, before, after]) => {
    const dt = document.createElement('dt');
    dt.textContent = label;
    const dd = document.createElement('dd');
    dd.className = 'mono';
    dd.textContent = (before || '—') + ' → ' + (after || '—');
    list.append(dt, dd);
  });
  dom.connsDiff.replaceChildren(title, list);
}


async function saveConnections() {
  const data = state.data;
  if (!data || state.connsSaving) return;
  const config = data.configuration || {};
  if (config.state === 'unavailable') {
    showToast('配置文件不可读，无法保存连接。', 'error');
    return;
  }
  if (state.conns.editing) {
    applyConnectionEditor();
    if (state.conns.editing) return;
  }
  /* 整组连接都会重写：任何一条旧连接没有唯一协议，或仍有模型按另一协议
     路由时，都不能靠「省略字段」或「随新协议重写」蒙混过去。 */
  const protocolProblems = [];
  for (const row of state.conns.draft || []) {
    if (!row || row.legacy) continue;
    const protocolError = connectionProtocolError(row);
    if (protocolError) {
      protocolProblems.push(protocolError);
      continue;
    }
    const conflicts = connectionProtocolConflicts(row.id, connectionProtocol(row));
    if (conflicts.length) protocolProblems.push(connectionProtocolConflictText(row, conflicts));
  }
  if (protocolProblems.length) {
    setConnErrors(protocolProblems);
    showToast('连接接口协议或已有模型协议需要先确认，未保存。', 'error');
    return;
  }
  const payload = connectionPayload(state.conns.draft);
  if (!payload.length) {
    showToast('至少保留一条命名连接；如需退回旧配置，请先添加连接再移除旧上游。', 'error');
    return;
  }
  const missingKey = payload.filter((row) => !row.apiKey);
  if (missingKey.length) {
    const savedIds = new Set((connectionDraftFrom(data) || []).filter((row) => row.secretSet || row.apiKeySet).map((row) => row.id));
    const needsKey = missingKey.filter((row) => !savedIds.has(row.id));
    if (needsKey.length) {
      setConnErrors(['连接「' + needsKey.map((row) => row.name || row.id).join('、')
        + '」还没有 API Key。新连接必须填写，已保存的连接可以留空保留原值。']);
      return;
    }
  }

  state.connsSaving = true;
  const epoch = state.stateReadEpoch;
  const key = state.key;
  const current = () => epoch === state.stateReadEpoch && key === state.key;
  renderConnections();
  renderActions(data);

  try {
    const body = { revision: state.conns.baseRevision ?? data.revision, connections: payload, models: data.models };
    const res = await request(API.workspace, { method: 'POST', body });
    if (!current()) return;
    if (res && res.state) {
      state.connsSaving = false;
      adoptState(res.state, { external: true });
      state.lastSync = Date.now();
      state.conns.base = savedConnectionsSignature(state.data);
      state.conns.baseRevision = state.data.revision ?? null;
      state.conns.baseEntity = connectionDraftFrom(state.data);
      state.conns.draft = connectionDraftFrom(state.data);
      state.conns.dirty = false;
      renderAll();
      showToast('已保存，后台将自动同步到 Kimi Work。', 'ok');
    } else {
      state.connsSaving = false;
      await loadState({ silent: true });
      if (!current()) return;
      showToast('连接已提交，状态已刷新。', 'ok');
    }
  } catch (error) {
    if (!current()) return;
    state.connsSaving = false;
    if (error.code === 'credentials_file_unavailable') {
      /* 只有写入私有凭据文件才需要它；这条错误不能当成「已保存」，
         也不能把用户输入的 Key 打到日志或提示里。 */
      setConnErrors(['当前启动方式没有可写的私有凭据文件，因此没有保存任何 API Key。'
        + '请用 setup 生成的凭据文件启动面板后重试；已填写的 Key 不会被记录。']);
      if (dom.ceKey) dom.ceKey.value = '';
      showToast('无法保存 API Key：当前没有可写的私有凭据文件。', 'error');
      renderConnections();
      return;
    }
    if (error.code === 'connection_in_use') {
      setConnErrors(['仍有模型引用被移除的连接：' + (Array.isArray(error.models) ? error.models.join('、') : '')
        + '。请先在模型管理改到其他连接，再移除该连接。']);
      showToast('连接仍被模型引用，未保存。', 'error');
      renderConnections();
      return;
    }
    await handleSaveFailure(error, 'connections');
  } finally {
    if (!current()) return;
    state.connsSaving = false;
    if (state.data) {
      renderConnections();
      renderActions(state.data);
    }
  }
}


/* ---------- exports ---------- */

export {
  applyConnectionEditor,
  applyConnections,
  applyOnboardBatchContext,
  applyOnboardPreset,
  applyProviderPreset,
  applyState,
  closeConnectionEditor,
  configInfo,
  credentialAuthPending,
  configReady,
  connectionDraftFrom,
  connectionPayload,
  connectionProtocol,
  connectionProtocolErrors,
  connectionsDirty,
  discoverConnectionEditorModels,
  discoverOnboardModels,
  discoveryReasonText,
  fillOnboardForm,
  fillOnboardPresets,
  fillServiceForm,
  onboardBusy,
  onboardEditingText,
  openConnectionEditor,
  openOnboardWizard,
  protocolLabel,
  providerPreset,
  readConnectionEditor,
  readServiceForm,
  renderConnection,
  renderConnectionEditor,
  renderConnections,
  renderOnboard,
  renderOnboardOverview,
  renderServices,
  saveConnections,
  saveOnboard,
  saveSearchServices,
  savedConnectionsSignature,
  savedOnboardTarget,
  searchAvailable,
  serviceDraftFrom,
  serviceRowOf,
  serviceSignature,
  setOnboardProtocol,
  toggleFilteredOnboardModels,
  upstreamReasonText,
  useRecommendedOnboardModels,
};

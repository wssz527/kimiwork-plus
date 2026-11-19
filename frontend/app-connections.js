import { $, ACTION_LABEL, API, ApiError, CONFIG_STATE, CONNECTION_ID_PATTERN, DIAG_REASON, DISCOVERY_REASON, LOOPBACK, UPSTREAM_REASON, actionUUID, clearKey, displayModelId, dom, fmtCount, fmtK, fmtMs, fmtTokens, renderErrorList, request, setConnErrors, setDot, setHidden, setText, shortRev, showGate, showToast, state, syncSelectOptions } from './app-core.js';
import { closeEditor, connectionRowsOf, connectionsAvailable, connectionsChanged, editorDirty, preferredProtocol, routeIdFor } from './app-models.js';
import { isInFlight, renderActions, syncState } from './app-overview.js';
import { adoptState, handleSaveFailure, loadState, renderAll, setView, stopPolling } from './app.js';

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
  if (dom.svcSearchUrl && !state.services.urlEditing?.search) dom.svcSearchUrl.value = draft.searchUrl;
  if (dom.svcFetchUrl && !state.services.urlEditing?.fetch) dom.svcFetchUrl.value = draft.fetchUrl;
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
  if (dom.svcSearchKey && !state.services.searchKey) dom.svcSearchKey.value = '';
  if (dom.svcFetchKey && !state.services.fetchKey) dom.svcFetchKey.value = '';

  const verdict = evaluateServices(data);
  setDot(dom.svcVerdictDot, verdict.tone);
  setText(dom.svcVerdictWord, verdict.word);
  setText(dom.svcVerdictNote, verdict.note);
  if (dom.svcVerdict) dom.svcVerdict.dataset.status = verdict.status || '';

  const { errors } = validateServiceDraft(draft);
  renderErrorList(dom.svcErrors, errors.slice(0, 2));
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
  renderServices(data);
  try {
    const res = await request(API.context, { method: 'POST', body: { services: payload } });
    const search = res && res.search;
    if (search && typeof search === 'object' && search.services) {
      state.data = { ...state.data, search };
      state.services.saving = false;
      state.services.searchKey = '';
      state.services.fetchKey = '';
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
      renderErrorList(dom.svcErrors, [error.message]);
      renderServices(state.data);
      return;
    }
    await handleSaveFailure(error, 'services');
  } finally {
    state.services.saving = false;
    if (state.data) renderServices(state.data);
  }
}

/* ---------- named connections ---------- */

/* 草稿只保存可回传的字段。apiKey 只在用户本次输入时存在，且不会回填到任何
   输入框；签名比较同样排除它，避免把「刚输入但未保存的 key」当成未变更。 */
function connectionDraftRow(row) {
  return {
    id: typeof row.id === 'string' ? row.id : '',
    name: typeof row.name === 'string' ? row.name : '',
    providerPresetId: typeof row.providerPresetId === 'string' ? row.providerPresetId : '',
    baseURL: typeof row.baseURL === 'string' ? row.baseURL : '',
    apiKeyEnv: typeof row.apiKeyEnv === 'string' ? row.apiKeyEnv : '',
    protocols: Array.isArray(row.protocols) ? row.protocols.filter((value) => value === 'chat' || value === 'responses') : null,
    legacy: row.legacy === true,
    modelCount: Number.isInteger(row.modelCount) ? row.modelCount : null,
    apiKeySet: row.apiKeySet === true,
    apiKey: '',
    secretSet: false,
  };
}

function connectionDraftFrom(data) {
  const rows = connectionRowsOf(data);
  if (!rows) return null;
  return rows.map(connectionDraftRow);
}

function connectionsSignature(rows) {
  if (!Array.isArray(rows)) return '';
  return JSON.stringify(rows.map((row) => [row.legacy ? 'legacy' : 'named', row.id, row.name,
    row.providerPresetId || '', row.baseURL, row.apiKeyEnv || '',
    (row.protocols || []).slice().sort().join(','), row.apiKey ? 'with-key' : '']));
}

function savedConnectionsSignature(data) {
  return connectionsSignature(connectionDraftFrom(data));
}

function connectionsDirty() {
  return connectionsSignature(state.conns.draft || []) !== state.conns.base;
}

/* 提交体只包含契约允许的字段；apiKey 仅在本次输入过时带上，
   省略即表示保留已保存的旧值。 */
function connectionPayload(rows) {
  return (rows || []).filter((row) => !row.legacy).map((row) => ({
    id: row.id,
    name: row.name,
    ...(row.providerPresetId ? { providerPresetId: row.providerPresetId } : {}),
    baseURL: row.baseURL,
    ...(row.apiKeyEnv ? { apiKeyEnv: row.apiKeyEnv } : {}),
    ...(Array.isArray(row.protocols) && row.protocols.length ? { protocols: row.protocols } : {}),
    ...(row.apiKey ? { apiKey: row.apiKey } : {}),
  }));
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

  const protocols = Array.isArray(draft.protocols) ? draft.protocols.filter((value) => value === 'chat' || value === 'responses') : [];
  return {
    errors,
    value: {
      id,
      name,
      ...(draft.providerPresetId ? { providerPresetId: draft.providerPresetId } : {}),
      baseURL: base.value || String(draft.baseURL || '').trim(),
      apiKeyEnv: keyEnv,
      ...(protocols.length ? { protocols } : {}),
      apiKey: apiKey.trim(),
      legacy: false,
    },
  };
}

/* 每次打开抽屉都从草稿副本开始，取消不会留下半个编辑。 */
function openConnectionEditor(row) {
  const rows = state.conns.draft || [];
  state.conns.editing = {
    originalId: row && !row.legacy ? row.id : null,
    baseRevision: state.conns.baseRevision ?? state.data?.revision ?? null,
    baseRow: row ? connectionDraftRow(row) : null,
    draft: row ? { ...connectionDraftRow(row), apiKey: '', secretSet: row.apiKeySet === true }
      : { id: '', name: '', providerPresetId: '', baseURL: '', apiKeyEnv: '', protocols: null,
        legacy: false, modelCount: null, apiKeySet: false, apiKey: '', secretSet: false },
    errors: [],
  };
  renderConnections();
  if (dom.ceName && dom.ceName.focus) dom.ceName.focus();
}

function closeConnectionEditor() {
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
  editing.draft.protocols = protocols.length ? protocols : null;
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
  editing.keyEdited = false;
  editing.draft.baseURL = typeof preset.baseURL === 'string' ? preset.baseURL : editing.draft.baseURL;
  if (Array.isArray(preset.protocols) && preset.protocols.length) {
    editing.draft.protocols = preset.protocols.filter((value) => value === 'chat' || value === 'responses');
  }
  if (!editing.draft.name) editing.draft.name = preset.name || '';
  if (!editing.draft.id) editing.draft.id = presetsSuggestedId(preset);
  if (dom.ceBase) dom.ceBase.value = editing.draft.baseURL;
  if (dom.ceChat) dom.ceChat.checked = (editing.draft.protocols || []).includes('chat');
  if (dom.ceResponses) dom.ceResponses.checked = (editing.draft.protocols || []).includes('responses');
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

/* 已知原因有中文说明；未知错误码也给一句可读的话，不把内部标识直接甩给用户。 */
function discoveryReasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  if (DISCOVERY_REASON[code]) return DISCOVERY_REASON[code];
  if (code === 'model_search_failed' || code === 'panel_operation_failed') {
    return '候选搜索未完成，请稍后重试；已载入的供应商目录仍可点选。';
  }
  return '操作未完成（' + code + '）；请重试或查看诊断。';
}

/* update 的 patch 只能包含用户真正改过的字段：省略的秘密字段由后端保留，
   因此未填写的 env/headers 绝不能被序列化成空对象而把已存值清空。 */
function mcpServerPayload(draft) {
  const errors = [];
  const name = String(draft.name || '');
  if (!MCP_NAME_PATTERN.test(name)) errors.push('名称需以字母或数字开头，可用点、下划线与连字符，最长 64 字符。');
  const transport = ['stdio', 'http', 'sse'].includes(draft.transport) ? draft.transport : null;
  if (!transport) errors.push('请选择传输方式。');
  const server = { name, transport, enabled: draft.enabled !== false };
  if (transport === 'stdio') {
    const command = String(draft.command || '').trim();
    if (!command) errors.push('stdio 服务器需要填写命令。');
    if (command.length > 1024) errors.push('命令最长 1024 字符。');
    server.command = command;
    const args = (draft.args || []).filter((value) => typeof value === 'string');
    if (args.length > 64) errors.push('参数最多 64 个。');
    if (args.length) server.args = args;
    if (draft.env && Object.keys(draft.env).length) server.env = { ...draft.env };
  } else if (transport) {
    const url = String(draft.url || '').trim();
    let parsed;
    try { parsed = new URL(url); } catch { errors.push('服务地址不是合法的 URL。'); }
    if (parsed && !['http:', 'https:'].includes(parsed.protocol)) errors.push('服务地址只接受 http 或 https。');
    if (parsed && (parsed.username || parsed.password)) errors.push('服务地址不能包含账号或密码。');
    if (!url) errors.push('远程服务器需要填写服务地址。');
    server.url = url;
    if (draft.headers && Object.keys(draft.headers).length) server.headers = { ...draft.headers };
  }
  return { errors, server };
}

function parseLines(text) {
  return String(text || '').split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
}

function parseEnvLines(text) {
  const env = {};
  const invalid = [];
  for (const line of parseLines(text)) {
    const index = line.indexOf('=');
    if (index <= 0) { invalid.push(line); continue; }
    const key = line.slice(0, index).trim();
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) { invalid.push(line); continue; }
    env[key] = line.slice(index + 1);
  }
  return { env, invalid };
}

function parseHeaderLines(text) {
  const headers = {};
  const invalid = [];
  for (const line of parseLines(text)) {
    const index = line.indexOf(':');
    if (index <= 0) { invalid.push(line); continue; }
    const key = line.slice(0, index).trim();
    if (!/^[A-Za-z0-9-]+$/.test(key)) { invalid.push(line); continue; }
    headers[key] = line.slice(index + 1).trim();
  }
  return { headers, invalid };
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
    dom.onboardId.value = target.id || '';
    dom.onboardName.value = target.name || '';
    dom.onboardBase.value = target.baseURL || '';
    if (dom.onboardPreset && target.providerPresetId) dom.onboardPreset.value = target.providerPresetId;
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
      const limits = connectionProtocolLimits(target);
      state.onboard.discovery = {
        checkedAt: null,
        modelCount: models.length,
        saved: true,
        models: models.map((model) => {
          const upstreamModelId = model.upstreamModelId || model.id;
          /* 已保存行自己的协议声明优先；后端裁剪掉时（保存契约只保留单选
             protocol）按同一个上游 ID 从供应商预设恢复真实声明，绝不再缩成
             「保存时选的那一个协议」。 */
          const declared = declaredProtocolsFor(model, target, upstreamModelId);
          const savedProtocol = protocolList([model.protocol])[0] || null;
          return {
            id: upstreamModelId,
            name: model.name || '',
            upstreamModelId,
            protocols: declared,
            protocolOptions: protocolChoicesFor(declared, limits),
            protocol: savedProtocol || (declared.length ? preferredProtocol(declared) : null),
            contextWindow: model.contextWindow,
            maxOutputTokens: model.maxOutputTokens,
            /* 已保存行自己的声明照原样带入：没写过就仍是未知，不补 false。 */
            tools: typeof model.tools === 'boolean' ? model.tools : undefined,
            reasoning: typeof model.reasoning === 'boolean' ? model.reasoning : undefined,
            vision: Array.isArray(model.inputModalities) ? model.inputModalities.includes('image') : undefined,
            efforts: Array.isArray(model.reasoningEfforts) ? model.reasoningEfforts : [],
            checkedAt: model.metadataSource && model.metadataSource.checkedAt ? model.metadataSource.checkedAt : null,
            sourceURL: model.metadataSource && model.metadataSource.url ? model.metadataSource.url : null,
            metadataKind: model.metadataSource?.kind || 'manual',
            selected: true,
          };
        }),
      };
      if (dom.onboardDiscoverState) setText(dom.onboardDiscoverState, '已从保存的目录载入 ' + fmtCount(models.length) + ' 个模型。');
    }
  }
  fillOnboardPresets();
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

/* 两个协议是本产品唯一支持的两种接口；可选集合按这个顺序展示，与首设页一致。 */
const PROTOCOL_CHOICES = ['chat', 'responses'];

function protocolList(value) {
  return Array.isArray(value)
    ? value.filter((entry) => entry === 'chat' || entry === 'responses')
    : [];
}

/* 连接可以限制自己只服务哪些协议；空数组在连接契约里表示「不限」，因此只有
   非空列表才构成限制。 */
function connectionProtocolLimits(connection) {
  const list = protocolList(connection && connection.protocols);
  return list.length ? list : null;
}

/* 供应商预设是候选目录的另一份真实声明：按连接的 presetId 与同一个上游 ID
   找到厂商写明的协议。找不到就返回空，绝不按路由 ID 末段猜。 */
function presetProtocolsFor(connection, upstreamModelId) {
  const presetId = connection && connection.providerPresetId;
  if (!presetId || !upstreamModelId) return [];
  const preset = providerPresets().find((row) => row && row.id === presetId);
  const model = preset && Array.isArray(preset.models)
    ? preset.models.find((row) => row && row.id === upstreamModelId)
    : null;
  return protocolList(model && model.protocols);
}

/* 只返回「真实声明」：已保存行自己的 protocols 字段（包括显式空数组，那是
   权威的「不支持」，不回退预设），否则用同一上游 ID 的预设声明。 */
function declaredProtocolsFor(row, connection, upstreamModelId) {
  if (row && Array.isArray(row.protocols)) return protocolList(row.protocols);
  return presetProtocolsFor(connection, upstreamModelId);
}

/* 可选项是声明与连接限制的交集；没有声明时才允许在连接允许范围内由用户
   显式确认，且不会因此把未声明的能力写进已保存行。 */
function protocolChoicesFor(declared, allowed) {
  const base = allowed || PROTOCOL_CHOICES;
  if (!declared || !declared.length) return base.slice();
  return declared.filter((value) => allowed ? allowed.includes(value) : true);
}

/* 用户显式确认协议：只写回这一行的 protocol，真实的 upstreamModelId 与上游 ID
   都不动；本机路由按既有 route-id 规则在构建时重算。 */
function assignOnboardProtocol(target, protocol) {
  if (!target) return false;
  if (protocol !== 'chat' && protocol !== 'responses') {
    target.protocol = null;
    return true;
  }
  const choices = Array.isArray(target.protocolOptions)
    ? target.protocolOptions
    : Array.isArray(target.protocols) && target.protocols.length ? target.protocols : PROTOCOL_CHOICES;
  if (!choices.includes(protocol)) return false;
  target.protocol = protocol;
  return true;
}

function chooseOnboardProtocol(id, protocol) {
  const target = onboardModelRows().find((entry) => entry.id === id);
  if (!assignOnboardProtocol(target, protocol)) return;
  state.onboard.touched = true;
  renderOnboard();
}

function toggleFilteredOnboardModels() {
  const rows = filteredOnboardModels();
  if (!rows.length) return;
  const selected = !rows.every((row) => row.selected === true);
  rows.forEach((row) => { row.selected = selected; });
  state.onboard.touched = true;
  renderOnboard();
}

function applyOnboardBatchProtocol(protocol) {
  const rows = selectedOnboardModels();
  let changed = 0;
  rows.forEach((row) => {
    if (assignOnboardProtocol(row, protocol)) changed++;
  });
  if (!changed) {
    showToast('已选模型都不允许这个接口协议；请按各行声明调整。', 'error');
    return;
  }
  state.onboard.touched = true;
  renderOnboard();
  const skipped = rows.length - changed;
  showToast('已为 ' + fmtCount(changed) + ' 个已选模型选择 ' + protocolLabel(protocol)
    + (skipped ? '；' + fmtCount(skipped) + ' 个因声明或连接限制未改动。' : '。'), skipped ? 'error' : 'ok');
}

function applyOnboardBatchContext() {
  const value = Number(dom.onboardBatchContext?.value || '');
  if (!Number.isSafeInteger(value) || value <= 0) {
    showToast('请输入正整数的上下文窗口；不知道时请留空并向上游确认。', 'error');
    return;
  }
  const rows = selectedOnboardModels();
  if (!rows.length) return;
  rows.forEach((row) => {
    row.contextWindow = value;
    row.metadataKind = 'manual';
  });
  state.onboard.touched = true;
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

function onboardRowFrom(candidate, { connection = null } = {}) {
  const protocols = protocolList(candidate && candidate.protocols);
  const protocol = protocols.length ? preferredProtocol(protocols) : null;
  return {
    id: candidate.id,
    name: candidate.name || '',
    description: candidate.description || '',
    upstreamModelId: candidate.id,
    protocols,
    protocolOptions: protocolChoicesFor(protocols, connectionProtocolLimits(connection)),
    protocol,
    contextWindow: candidate.contextWindow,
    maxOutputTokens: candidate.maxOutputTokens,
    tools: typeof candidate.tools === 'boolean' ? candidate.tools : undefined,
    reasoning: typeof candidate.reasoning === 'boolean' ? candidate.reasoning : undefined,
    vision: Array.isArray(candidate.inputModalities) ? candidate.inputModalities.includes('image') : undefined,
    efforts: Array.isArray(candidate.reasoningEfforts) ? candidate.reasoningEfforts : [],
    checkedAt: candidate.source && candidate.source.checkedAt ? candidate.source.checkedAt : null,
    sourceURL: candidate.source && candidate.source.url ? candidate.source.url : null,
    metadataKind: candidate.metadataSource?.kind || candidate.source?.kind || 'discovery',
    selected: false,
  };
}

function preserveOnboardModelNames(models, savedModels, connectionId) {
  const names = new Map((Array.isArray(savedModels) ? savedModels : [])
    .filter((model) => model && model.connectionId === connectionId && model.name)
    .map((model) => [model.upstreamModelId, model.name]));
  return models.map((row) => names.has(row.upstreamModelId)
    ? { ...row, name: names.get(row.upstreamModelId) }
    : row);
}

function buildOnboardRow(row) {
  const context = Number(row.contextWindow) > 0 ? Number(row.contextWindow) : undefined;
  /* 协议只来自上游声明或用户显式确认；两者都没有时保持为空，由调用方拒绝，
     绝不按末段规则补一个猜测值。 */
  const protocol = row.protocol === 'chat' || row.protocol === 'responses'
    ? row.protocol
    : (Array.isArray(row.protocols) && row.protocols.length ? preferredProtocol(row.protocols) : null);
  return {
    id: protocol ? routeIdFor(row.connectionId, row.upstreamModelId, protocol) : '',
    upstreamModelId: row.upstreamModelId,
    ...(row.name && row.name !== row.upstreamModelId ? { name: row.name } : {}),
    protocol,
    contextWindow: context,
    ...(Number(row.maxOutputTokens) > 0 ? { maxOutputTokens: Number(row.maxOutputTokens) } : {}),
    ...(row.tools === true ? { tools: true } : row.tools === false ? { tools: false } : {}),
    ...(typeof row.reasoning === 'boolean' ? { reasoning: row.reasoning } : {}),
    inputModalities: row.vision === true ? ['text', 'image'] : ['text'],
    ...(row.reasoning === true && Array.isArray(row.efforts) && row.efforts.length ? { reasoningEfforts: row.efforts } : {}),
    connectionId: row.connectionId,
    ...(row.metadataKind !== 'manual' && row.checkedAt
      ? { metadataSource: { kind: row.metadataKind || 'discovery', ...(row.sourceURL ? { url: row.sourceURL } : {}), checkedAt: row.checkedAt } }
      : { metadataSource: { kind: 'manual' } }),
  };
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
  if (!models.length) problems.push('至少勾选 1 个模型。');
  const blocked = models.filter((row) => !row.protocol);
  if (blocked.length) {
    problems.push('这些模型的上游没有声明协议，请为它们各选一个接口协议：'
      + blocked.map((row) => row.upstreamModelId).join('、') + '。');
  }
  const missingContext = models.filter(row => !(Number(row.contextWindow) > 0));
  if (missingContext.length) {
    problems.push('请为这些模型填写上下文窗口；接口没有返回时不能用猜测值：'
      + missingContext.map(row => row.upstreamModelId).join('、') + '。');
  }
  /* 连接限制是保存前已知的真实约束：路由会按所选协议生成，但请求最终会被
     连接拒绝，所以这里先按同一个限制拦下，不等到保存后才发现。 */
  const limits = connectionProtocolLimits(saved);
  if (limits) {
    const conflicts = models.filter((row) => row.protocol && !limits.includes(row.protocol));
    if (conflicts.length) {
      problems.push('连接「' + (saved.name || saved.id) + '」只允许 ' + limits.map(protocolLabel).join('/')
        + '；' + conflicts.map((row) => row.upstreamModelId
          + ' 当前为 ' + protocolLabel(row.protocol)).join('、')
        + ' 不在允许范围内。');
    }
  }
  const ids = new Set();
  const built = [];
  for (const row of models) {
    const model = buildOnboardRow({ ...row, connectionId: values.id });
    if (model.id && ids.has(model.id)) problems.push('模型路由 ID 重复：' + model.id + '。');
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

function renderOnboardModels(rows, { connectionId = '' } = {}) {
  if (!dom.onboardModels) return;
  const attachedIds = new Set((Array.isArray(state.data?.models) ? state.data.models : [])
    .filter((model) => model && model.connectionId === connectionId)
    .map((model) => model.upstreamModelId || model.id));
  const signature = JSON.stringify([rows, connectionId, [...attachedIds]]);
  if (dom.onboardModels.dataset.signature === signature) return;
  const active = document.activeElement;
  const focusModel = active?.dataset?.onboardModel || '';
  const focusControl = active?.dataset?.onboardFocus || '';
  dom.onboardModels.dataset.signature = signature;
  if (!rows.length) {
    dom.onboardModels.replaceChildren();
    return;
  }
  const nodes = rows.map((row) => {
    const li = document.createElement('li');
    li.className = 'onboard__model';
    const label = document.createElement('label');
    label.className = 'check check--model';
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = row.selected === true;
    box.dataset.onboardModel = row.id;
    box.dataset.onboardFocus = 'selected';
    box.addEventListener('change', () => {
      const target = onboardModelRows().find((entry) => entry.id === row.id);
      if (target) target.selected = box.checked === true;
      state.onboard.touched = true;
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
    const declared = Array.isArray(row.protocols) ? row.protocols : [];
    if (Number(row.contextWindow) > 0) parts.push('上下文 ' + fmtK(row.contextWindow));
    if (Number(row.maxOutputTokens) > 0) parts.push('输出 ' + fmtK(row.maxOutputTokens));
    if (declared.length) parts.push(declared.map(protocolLabel).join('/'));
    else parts.push('服务商未声明协议');
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

    const budget = document.createElement('div');
    budget.className = 'onboard__budget';
    const capabilityText = value => typeof value === 'boolean' ? value ? '支持' : '不支持' : '待确认';
    const provenance = row.metadataKind === 'manual' ? '用户修改' : row.metadataKind === 'preset' ? '厂商资料' : '接口返回';
    budget.append(document.createTextNode(`来源：${provenance}。工具${capabilityText(row.tools)}，推理${capabilityText(row.reasoning)}，图片${capabilityText(row.vision)}。`));
    for (const [key, labelText] of [['contextWindow', '上下文窗口'], ['maxOutputTokens', '最大输出（可选）']]) {
      const field = document.createElement('label');
      field.className = 'field field--inline';
      const caption = document.createElement('span');
      caption.className = 'field__label';
      caption.textContent = labelText;
      const input = document.createElement('input');
      input.type = 'number';
      input.min = '1';
      input.step = '1';
      input.placeholder = '待确认';
      input.value = Number(row[key]) > 0 ? String(row[key]) : '';
      input.dataset.onboardBudget = key;
      input.dataset.onboardModel = row.id;
      input.dataset.onboardFocus = key;
      input.addEventListener('change', () => {
        const target = onboardModelRows().find(entry => entry.id === row.id);
        if (target) {
          target[key] = input.value.trim() ? Number(input.value) : undefined;
          target.metadataKind = 'manual';
        }
        renderOnboard();
      });
      field.append(caption, input);
      budget.append(field);
    }
    li.append(budget);

    /* 可选项 = 真实声明 ∩ 连接限制；声明过协议时不能换成一个上游没声明的
       协议，没有声明时才允许用户在连接允许范围内显式确认。已保存但不在
       声明里的协议保留为「已保存」，不悄悄改成别的协议。 */
    const protocolField = document.createElement('label');
    protocolField.className = 'field field--inline onboard__protocol';
    const protocolLabelNode = document.createElement('span');
    protocolLabelNode.className = 'field__label';
    protocolLabelNode.textContent = '接口协议';
    const select = document.createElement('select');
    select.dataset.onboardProtocol = row.id;
    select.dataset.onboardModel = row.id;
    select.dataset.onboardFocus = 'protocol';
    const choices = Array.isArray(row.protocolOptions)
      ? row.protocolOptions
      : (declared.length ? declared : PROTOCOL_CHOICES);
    const options = [{ value: '', label: '请选择…' }]
      .concat(choices.map((value) => ({ value, label: protocolLabel(value) })));
    if (row.protocol && !choices.includes(row.protocol)) {
      options.push({ value: row.protocol, label: protocolLabel(row.protocol) + '（已保存）' });
    }
    syncSelectOptions(select, options, row.protocol || '');
    select.addEventListener('change', () => {
      chooseOnboardProtocol(row.id, select.value);
    });
    protocolField.append(protocolLabelNode, select);
    if (!choices.length) {
      const conflict = document.createElement('span');
      conflict.className = 'field__hint onboard__route';
      conflict.textContent = '连接不允许该上游声明的协议；请先在「连接」页调整允许的接口协议。';
      protocolField.append(conflict);
    }
    if (connectionId && row.upstreamModelId) {
      const route = document.createElement('span');
      route.className = 'field__hint onboard__route mono';
      route.textContent = row.protocol
        ? '本机路由 ' + displayModelId(routeIdFor(connectionId, row.upstreamModelId, row.protocol))
        : '选择协议后生成本机路由；上游 ID 保持原样';
      protocolField.append(route);
    }
    li.append(protocolField);

    const caps = document.createElement('div');
    caps.className = 'onboard__caps';
    const toggle = (key, text, disabled = false) => {
      const cap = document.createElement('label');
      cap.className = 'check';
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.checked = row[key] === true;
      input.disabled = disabled;
      input.dataset.onboardCap = key;
      input.dataset.onboardModel = row.id;
      input.dataset.onboardFocus = key;
      input.addEventListener('change', () => {
        const target = onboardModelRows().find((entry) => entry.id === row.id);
        if (target) target[key] = input.checked === true;
        renderOnboard();
      });
      const span = document.createElement('span');
      span.textContent = text;
      cap.append(input, span);
      return cap;
    };
    caps.append(toggle('tools', '工具调用'), toggle('reasoning', '推理'), toggle('vision', '图片输入'));
    li.append(caps);
    return li;
  });
  dom.onboardModels.replaceChildren(...nodes);
  if (focusControl) {
    const target = Array.from(dom.onboardModels.querySelectorAll('[data-onboard-focus]')).find((node) =>
      node.dataset.onboardModel === focusModel && node.dataset.onboardFocus === focusControl);
    if (target) target.focus({ preventScroll: true });
  }
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
  renderOnboardModels(visibleRows, { connectionId: onboardConnectionValues().id });
  setHidden(dom.onboardModelsEmpty, !rows.length || visibleRows.length > 0);
  if (dom.onboardSelectAll) {
    const allVisible = visibleRows.length > 0 && visibleRows.every((row) => row.selected === true);
    dom.onboardSelectAll.disabled = busy || !visibleRows.length;
    dom.onboardSelectAll.setAttribute('aria-pressed', allVisible ? 'true' : 'false');
    setText(dom.onboardSelectAll, allVisible ? '取消全选当前结果' : '全选当前结果');
  }
  if (dom.onboardBatchChat) dom.onboardBatchChat.disabled = busy || !selected.length;
  if (dom.onboardBatchContext) dom.onboardBatchContext.disabled = busy || !selected.length;
  if (dom.onboardBatchContextApply) dom.onboardBatchContextApply.disabled = busy || !selected.length;

  const preview = selected.length ? onboardProblems({ models: selected }).problems : [];
  const blocking = preview.filter((text) => !text.startsWith('至少勾选'));
  setHidden(dom.onboardErrors, blocking.length === 0);
  if (dom.onboardErrors) {
    dom.onboardErrors.replaceChildren(...blocking.map((message) => {
      const p = document.createElement('p');
      p.textContent = message;
      return p;
    }));
  }

  const canApply = Boolean(connectionsChanged(data));
  if (dom.onboardRetryApply) {
    setHidden(dom.onboardRetryApply, !(canApply && applyAvailability(data) === 'pending'));
    dom.onboardRetryApply.disabled = !canApply || busy;
  }
  dom.onboardSave.disabled = !ready || busy || !rows.length || !selected.length || blocking.length > 0;
  setText(dom.onboardSave, draft.saving ? '正在保存…' : '保存');
  setText(dom.onboardDraft, rows.length
    ? (selected.length ? '将保存 ' + fmtCount(selected.length) + ' 个模型' : '尚未勾选模型')
    : '先读取模型，再勾选要接入的模型');

  const sync = syncState(data);
  if (dom.onboardVerdict) {
    setDot(dom.onboardVerdictDot, sync.tone);
    setText(dom.onboardVerdictWord, sync.word);
    setText(dom.onboardVerdictNote, sync.note);
  }
  if (dom.onboardDisclosure && dom.onboardDisclosure.dataset.initialized !== 'true') {
    dom.onboardDisclosure.open = !savedOnboardTarget() || !(data.models || []).length;
    dom.onboardDisclosure.dataset.initialized = 'true';
  }
  if (dom.handoff) {
    const operation = data.operation;
    const activeAction = operation && ['launch-kimi', 'restart-kimi', 'apply-connections', 'connect-kimi', 'prepare-native'].includes(operation.action);
    dom.handoff.dataset.state = (activeAction && isInFlight(operation)) || ['authorizing', 'busy', 'importing'].includes(sync.code) ? 'running' : 'idle';
    dom.handoff.dataset.tone = sync.tone;
  }
  renderOnboardSteps(data, { selected: selected.length, rows: rows.length });
  if (dom.onboardRetryApply) {
    setHidden(dom.onboardRetryApply, !(canApply && applyAvailability(data) === 'pending'));
    dom.onboardRetryApply.disabled = !canApply || busy;
  }
  if (dom.onboardDiscover) {
    dom.onboardDiscover.disabled = !ready || busy;
    setText(dom.onboardDiscover, draft.busy ? '正在读取…' : '读取模型');
  }
  if (dom.onboardRecommended) {
    const presetId = dom.onboardPreset?.value || '';
    const recommended = state.providers?.details?.[presetId]?.models || [];
    const available = recommended.length > 0;
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
  renderLaunch(data);
}

function renderLaunch(data) {
  if (!dom.onboardLaunch) return;
  const launchActions = ['launch-kimi', 'open-work', 'restart-kimi'];
  const running = isInFlight(data?.operation) && launchActions.includes(data.operation.action);
  const blocked = isInFlight(data?.operation) && !running;
  const sync = syncState(data);
  const alreadyRunning = data?.kimiProcess?.running === true;
  const integration = data?.integration;
  const needsRestart = integration?.desktop?.managed === false || ['manual-exit', 'restart'].includes(sync.code);
  const action = !alreadyRunning ? 'launch-kimi' : needsRestart ? 'restart-kimi' : 'open-work';
  dom.onboardLaunch.dataset.action = action;
  dom.onboardLaunch.dataset.profileMode = '';
  dom.onboardLaunch.hidden = false;
  dom.onboardLaunch.disabled = running || blocked || Boolean(state.pendingAction);
  setText(dom.onboardLaunch, ACTION_LABEL[action]);
  dom.onboardLaunch.setAttribute('aria-busy', running ? 'true' : 'false');
  if (dom.onboardLaunchHint) {
    const hint = action === 'restart-kimi' ? '点击后会正常退出 Kimi，再沿用当前登录和 Work 数据重新启动。' : action === 'launch-kimi' ? recoveryCopy(sync) : null;
    setHidden(dom.onboardLaunchHint, !hint);
    setText(dom.onboardLaunchHint, hint || '');
  }
}

function recoveryCopy(sync) {
  return {
    login: '请先在 Kimi Work 登录，再点「打开 / 启动 Kimi」。',
    authorization: '需要一次系统授权；点「打开 / 启动 Kimi」继续，不会反复弹窗。',
    busy: 'Kimi 正在处理任务，空闲后会自动继续；现在不需要重复点击。',
    conflict: '检测到其他来源的接入，未覆盖；请先核对 Kimi 设置中的现有配置。',
    'manual-exit': '点击「正常退出并重启 Kimi」，由产品请求系统正常退出并受管重启。',
    'waiting-quit': '退出请求已发出；关闭完成后会自动受管重启，请等待运行状态读回。',
    daimon: '模型暂时还不能在 Kimi 里选择；后台会自动对齐，不用重复点击或重启桌面。',
    synced: '已核对模型目录。启动后可在 Work 中选择模型。',
    imported: '已导入模型目录，后台还会核对；需要时仍可点「打开 / 启动 Kimi」。',
  }[sync.code] || null;
}


/* 四步进度只表达“已经真的发生了什么”，不根据按钮是否被点过来推进：
   连接已保存、目录已读回、应用已排队、原生目录已核对。 */
function renderOnboardSteps(data, { selected, rows }) {
  if (!dom.onboardSteps) return;
  const saved = savedOnboardTarget();
  const applied = !connectionsChanged(data);
  const sync = syncState(data);
  const done = {
    connection: Boolean(saved),
    catalogue: mountedSavedModels(data, saved) || (rows > 0 && selected > 0),
    apply: Boolean(saved) && applied,
    sync: sync.code === 'synced',
  };
  const items = dom.onboardSteps.querySelectorAll ? dom.onboardSteps.querySelectorAll('[data-step]') : [];
  for (const item of items) {
    const step = item.dataset ? item.dataset.step : null;
    if (!step) continue;
    item.dataset.done = String(done[step] === true);
  }
}

function mountedSavedModels(data, connection) {
  if (!connection) return false;
  const models = Array.isArray(data.models) ? data.models : [];
  return models.some((model) => model && model.connectionId === connection.id);
}

function openOnboardWizard() {
  setView('models');
  if (state.editor && editorDirty()) {
    showToast('请先保存或取消正在编辑的模型，再打开添加向导。', 'error');
    return;
  }
  closeEditor(true);
  if (dom.onboardDisclosure) {
    dom.onboardDisclosure.open = true;
    dom.onboardDisclosure.dataset.initialized = 'true';
    dom.onboardDisclosure.scrollIntoView({ block: 'start' });
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
    state.onboard.discoveryFor = key;
    state.onboard.discovery = {
      checkedAt: discovery.checkedAt || null,
      modelCount: discovery.modelCount,
      models: preserveOnboardModelNames(discovery.models.map((row) => onboardRowFrom(row, {
        connection: sameEndpoint ? saved : null,
      })), sameEndpoint ? state.data?.models : [], values.id),
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
  const data = state.data;
  const presetId = dom.onboardPreset?.value;
  const detail = state.providers?.details?.[presetId];
  if (!detail) {
    showToast('请先选择厂商、地区与服务类型；自定义服务请读取模型或手动添加。', 'error');
    return null;
  }
  const connection = savedOnboardRow(onboardConnectionValues().id);
  state.onboard.discoveryFor = onboardEndpointKey();
  state.onboard.discovery = {
    checkedAt: null,
    modelCount: detail.models.length,
    models: detail.models.map(row => onboardRowFrom({
      ...row, id: row.upstreamModelId, name: row.label, inputModalities: row.inputModalities,
    }, { connection })),
  };
  renderOnboard();
  return detail.models;
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
  const selected = selectedOnboardModels();
  const { problems, built, values } = onboardProblems({ models: selected });
  if (problems.length) {
    renderErrorList(dom.onboardErrors, problems);
    return false;
  }
  state.onboard.saving = true;
  renderOnboard();
  renderActions(data);

  try {
    const connection = {
      id: values.id,
      name: values.name,
      ...(values.providerPresetId ? { providerPresetId: values.providerPresetId } : {}),
      baseURL: values.baseURL,
      ...(values.apiKey.trim() ? { apiKey: values.apiKey.trim() } : {}),
    };
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
      revision: state.data.revision, connections: [...others, connection], models: catalogue,
    } });
    if (!result?.state) throw new ApiError('保存没有返回状态', 'STATE', 200);
    if (dom.onboardKey) dom.onboardKey.value = '';
    state.onboard.touched = false;
    adoptState(result.state, { external: true });
    state.lastSync = Date.now();
    renderAll();
    showToast('已保存 ' + fmtCount(built.length) + ' 个模型和服务商。请按同步状态同步到 Work。', 'ok');
    return true;
  } catch (error) {
    state.onboard.saving = false;
    await handleSaveFailure(error, 'onboard');
    return false;
  } finally {
    state.onboard.saving = false;
    if (state.data) {
      renderOnboard();
      renderActions(state.data);
    }
  }
}

/* 预设只填地址与已知协议，绝不猜 Key、也绝不覆盖用户已经改过的地址以外字段。 */
function applyOnboardPreset(presetId) {
  if (!dom.onboardBase) return;
  state.onboard.touched = true;
  const preset = providerPreset(presetId);
  const previous = dom.onboardPreset.dataset.presetId || '';
  if (previous !== presetId && dom.onboardKey) dom.onboardKey.value = '';
  dom.onboardPreset.dataset.presetId = presetId;
  if (!preset) {
    if (dom.onboardPresetHint) setText(dom.onboardPresetHint, '已选自定义；地址仍由你填写。');
    return;
  }
  if (preset.baseURL) {
    dom.onboardBase.value = preset.baseURL;
    if (dom.onboardId && !dom.onboardId.value) dom.onboardId.value = presetsSuggestedId(preset);
    if (dom.onboardName && !dom.onboardName.value) dom.onboardName.value = preset.name || preset.id;
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
  const current = dom.onboardPreset.value || '';
  syncSelectOptions(dom.onboardPreset, providerPresetOptions(), current);
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
    note: '连接已保存，当前网关尚未应用。点「应用已保存的连接」由后台服务安全应用。' },
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
  authorization: { tone: 'bad', word: '连接需要系统授权',
    note: '当前预览无法读取已保存的 Key。点「重新授权并应用连接」并响应系统提示，不会显示或改写 Key。' },
};

const APPLY_REFUSAL = {
  service_not_installed: '没有可自动应用的后台服务（当前面板以前台方式运行，或未安装服务）。',
  service_foreign: '检测到的后台服务不属于当前配置，未使用它应用连接。',
  service_stopped: '后台服务当前没有运行，无法自动应用连接。',
};

/* 是否需要应用，不能只看连接对象是否相同：只换 Key、地址不变时，
   connections.restartRequired 仍是判断“尚未生效”的唯一权威；对象比较会漏掉
   这种情况并把旧进程说成已经应用。服务端没有给出该标志时，退回对象比较。 */
function applyAvailability(data) {
  const connections = data && data.connections;
  const explicit = connections && typeof connections.restartRequired === 'boolean'
    ? connections.restartRequired : null;
  if (explicit === false) return 'idle';
  if (explicit === true) {
    const service = data && data.service;
    if (!service || service.mode !== 'service') return 'unsupported';
    return 'pending';
  }
  if (!connectionsChanged(data)) return 'idle';
  const service = data && data.service;
  if (!service || service.mode !== 'service') return 'unsupported';
  return 'pending';
}

/* 优先读 runner 写回的真实进展；没有时按本地可判断的事实降级。 */
function applyState(data) {
  if (data?.operation?.action === 'apply-connections' && isInFlight(data.operation)) return APPLY_COPY.applying;
  if (data?.connections?.apply?.state === 'failed' && data.connections.apply.reason === 'credentials_unavailable') {
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
  const service = data && data.service;
  const running = service && service.mode === 'service';
  const stateInfo = applyState(data);
  if (dom.restartHead) {
    setText(dom.restartHead, stateInfo === APPLY_COPY.authorization
      ? '由当前预览向系统申请读取已保存的连接凭据，后台不会自动弹出授权。'
      : running ? '本面板由后台服务运行；保存连接后在这里应用，不需要终端命令。'
        : '本面板不是由受管后台服务运行；连接保存后需要由受管服务应用。');
  }
  if (dom.applyVerdict) {
    setDot(dom.applyVerdictDot, stateInfo.tone);
    setText(dom.applyVerdictWord, stateInfo.word);
    setText(dom.applyVerdictNote, stateInfo.note);
  }
  if (dom.applyButton) {
    const pending = stateInfo === APPLY_COPY.pending || stateInfo === APPLY_COPY.failed || stateInfo === APPLY_COPY.authorization;
    setHidden(dom.applyButton, !pending);
    dom.applyButton.disabled = !pending || Boolean(state.pendingAction);
    setText(dom.applyButton, stateInfo === APPLY_COPY.authorization ? '重新授权并应用连接'
      : stateInfo === APPLY_COPY.failed ? '重试应用连接' : '应用已保存的连接');
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
    showToast('应用请求已提交；后台服务会在安全时应用连接。', 'ok');
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
  renderServices(data);
}

/* ---------- render: named connections ---------- */

function renderConnections() {
  const data = state.data;
  if (!data) return;
  const available = connectionsAvailable(data);
  setHidden(dom.connsUnavailable, available);
  setHidden(dom.legacyPanel, available);
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
  keyText.textContent = row.apiKey ? '本次已输入新 Key（未保存）'
    : row.apiKeySet ? 'Key 已在运行环境中' : row.credentialError ? '凭据不可读，请编辑并重新输入 Key 后保存' : 'Key 未在运行环境中';
  keyState.append(dot, keyText);
  const models = document.createElement('p');
  models.className = 'conn-row__line muted';
  models.textContent = (row.modelCount === null ? '模型数未知' : row.modelCount + ' 个模型')
    + (row.protocols && row.protocols.length ? ' · ' + row.protocols.map(protocolLabel).join('/') : '');
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

function renderConnectionEditor() {
  const editing = state.conns.editing;
  if (!editing) {
    setHidden(dom.connEditor, true);
    return;
  }
  setHidden(dom.connEditor, false);
  setText(dom.connEditorTitle, editing.originalId ? '编辑连接' : '添加连接');
  const draft = editing.draft;
  if (dom.ceName) dom.ceName.value = draft.name || '';
  if (dom.ceId) {
    dom.ceId.value = draft.id || '';
    dom.ceId.disabled = Boolean(editing.originalId);
  }
  if (dom.ceBase) dom.ceBase.value = draft.baseURL || '';
  if (dom.ceKeyEnv) dom.ceKeyEnv.value = draft.apiKeyEnv || '';
  if (dom.ceChat) dom.ceChat.checked = (draft.protocols || []).includes('chat');
  if (dom.ceResponses) dom.ceResponses.checked = (draft.protocols || []).includes('responses');
  if (dom.cePreset) {
    syncSelectOptions(dom.cePreset, providerPresetOptions(), draft.providerPresetId || '');
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
    if (!editing.keyEdited) dom.ceKey.value = '';
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
      lines.push(['协议限制', (before.protocols || []).map(protocolLabel).join('/') || '不限',
        (row.protocols || []).map(protocolLabel).join('/') || '不限']);
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
      showToast('已保存。请按同步状态同步到 Work。', 'ok');
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


/* 预设列表与面板状态是两条并行请求。已保存目录若先到，协议声明只能按当时
   已知的连接限制填；预设到达后补一次真实声明（只动声明与可选项，不改用户
   已选的协议、地址或 Key）。行自己的 protocols 字段仍然优先。 */
function refreshSavedProtocolDeclarations() {
  const discovery = state.onboard && state.onboard.discovery;
  if (!discovery || discovery.saved !== true || !Array.isArray(discovery.models)) return;
  const target = savedOnboardTarget();
  if (!target) return;
  const limits = connectionProtocolLimits(target);
  const models = Array.isArray(state.data && state.data.models) ? state.data.models : [];
  let changed = false;
  discovery.models.forEach((row) => {
    if (!row || !row.upstreamModelId) return;
    const model = models.find((entry) => entry
      && (entry.upstreamModelId || entry.id) === row.upstreamModelId
      && (!target || entry.connectionId === target.id));
    if (!model || Array.isArray(model.protocols)) return;
    const declared = presetProtocolsFor(target, row.upstreamModelId);
    if (!declared.length) return;
    row.protocols = declared;
    row.protocolOptions = protocolChoicesFor(declared, limits);
    changed = true;
  });
  if (changed) renderOnboard();
}

export {
  applyConnectionEditor,
  applyConnections,
  applyOnboardBatchContext,
  applyOnboardBatchProtocol,
  applyOnboardPreset,
  applyProviderPreset,
  applyState,
  closeConnectionEditor,
  configInfo,
  configReady,
  connectionDraftFrom,
  connectionPayload,
  connectionsDirty,
  discoverConnectionEditorModels,
  discoverOnboardModels,
  discoveryReasonText,
  fillOnboardForm,
  fillOnboardPresets,
  fillServiceForm,
  onboardBusy,
  openConnectionEditor,
  openOnboardWizard,
  protocolLabel,
  providerPreset,
  readConnectionEditor,
  readServiceForm,
  refreshSavedProtocolDeclarations,
  renderConnection,
  renderConnectionEditor,
  renderConnections,
  renderOnboard,
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
  toggleFilteredOnboardModels,
  upstreamReasonText,
  useRecommendedOnboardModels,
};

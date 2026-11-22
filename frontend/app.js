import { applyConnections, applyOnboardBatchContext, applyOnboardBatchProtocol, applyOnboardPreset, applyProviderPreset, closeConnectionEditor, configReady, connectionDraftFrom, connectionsDirty, discoverConnectionEditorModels, discoverOnboardModels, fillOnboardForm, fillOnboardPresets, fillServiceForm, openConnectionEditor, openOnboardWizard, readConnectionEditor, readServiceForm, refreshSavedProtocolDeclarations, renderConnection, renderConnectionEditor, renderConnections, renderOnboard, renderServices, saveConnections, saveOnboard, saveSearchServices, savedConnectionsSignature, savedOnboardTarget, searchAvailable, serviceDraftFrom, serviceRowOf, serviceSignature, toggleFilteredOnboardModels, useRecommendedOnboardModels } from './app-connections.js';
import { $, ACTION_LABEL, ACTION_STATE, API, ApiError, KEY_STORE, POLL_MS, SUBTAB_STORE, VIEW_ALIASES, VIEW_COPY, VIEW_STORE, actionUUID, applyTheme, clearKey, dom, fmtTime, initTheme, initWindowChrome, mountUsageRequests, observeMasthead, readKey, reasonText, renderErrorList, request, revealWorkspaceSection, setConnErrors, setHidden, setSubtab, setText, showGate, showToast, state } from './app-core.js';
import { applyBudgetPreset, catalogueWith, chooseProtocol, chooseUpstreamProtocol, clearModelQuery, closeEditor, closePicker, diffValue, findModel, modelThreeWayDiff, onModelFilter, openEditor, openPicker, pickCandidate, pickerScope, readEditorFields, renderEditor, renderFailoverRows, renderModels, renderPicker, resetModelSignature, routeIdFor, runConnectionCheck, runDiscovery, saveCatalogue, scheduleSearch, searchCandidates, toggleEffortChoice, validateEditor } from './app-models.js';
import { chooseIntegrationMode, dismissQuickstartGuide, isInFlight, previewServiceAction, refreshOverviewToday, renderActions, renderIntegrationMode, renderOperation, renderOverview, renderPreviewLifecycle, renderShell, renderUpdates, renderWorkspaceSync, reopenQuickstartGuide, resetIntegrationModeSignature, resetRecentSignature, runOverviewIssueAction, toggleQuickstartGuide, updateAction } from './app-overview.js';
import { appendPoolModel, discardDraft, draftFromSaved, pickEffort, pickModel, pickPoolDefault, pickRoleDefault, pickRoleModel, recomputeDraftDirty, removePoolModel, renderSubagents, resetSubagentSignatures, saveSubagentTarget, setMode } from './app-work.js';

let renderGrowth = () => {};
let usagePanel = { show() {}, reload: async () => {}, reset() {} };
let plusPanel = { render() {}, refresh: async () => {} };


const DEFAULT_SUBTABS = { models: 'catalog', subagents: 'models', maintenance: 'accounts' };
const VALID_SUBTABS = {
  models: new Set(['catalog', 'connections']),
  subagents: new Set(['models', 'swarm']),
  maintenance: new Set(['accounts', 'connection', 'extensions', 'advanced']),
};

function initSubtabs() {
  let stored = null;
  try {
    const raw = sessionStorage.getItem(SUBTAB_STORE);
    stored = raw ? JSON.parse(raw) : null;
  } catch {
    stored = null;
  }
  for (const [group, fallback] of Object.entries(DEFAULT_SUBTABS)) {
    const candidate = stored && typeof stored[group] === 'string' ? stored[group] : '';
    const tab = VALID_SUBTABS[group].has(candidate) ? candidate : fallback;
    setSubtab(group, tab, { persist: false });
  }
}

function initView() {
  let stored = null;
  try {
    stored = sessionStorage.getItem(VIEW_STORE);
  } catch {
    stored = null;
  }
  initSubtabs();
  setView(VIEW_COPY[VIEW_ALIASES[stored] || stored] ? stored : 'overview', { persist: false });
}

function setView(view, { persist = true } = {}) {
  view = VIEW_ALIASES[view] || view;
  if (!VIEW_COPY[view]) view = 'overview';
  const apply = () => {
    state.view = view;
    if (document.body) document.body.dataset.view = view;
    Object.keys(VIEW_COPY).forEach((name) => {
      const section = $('view-' + name);
      if (section) { section.hidden = name !== view; if (name === view) section.scrollTop = 0; }
      const nav = $('nav-' + name);
      if (nav) nav.setAttribute('aria-current', name === view ? 'page' : 'false');
    });
    const main = $('main');
    if (main) main.scrollTop = 0;
    if (dom.viewTitle) dom.viewTitle.textContent = VIEW_COPY[view].title;
    if (dom.viewLede) dom.viewLede.textContent = VIEW_COPY[view].lede;
    if (persist) {
      try {
        sessionStorage.setItem(VIEW_STORE, view);
      } catch {
        /* 忽略 */
      }
    }
    if (view === 'models') { renderModels(); renderConnection(); }
    if (view === 'subagents') renderSubagents();
    /* 门禁页没有会话密钥：此时再展示用量面板，挂载期那笔请求只会带空密钥失败。 */
    if (view === 'usage') { if (state.key) usagePanel.show(); void plusPanel.refresh(); }
    if (state.data) renderUpdates(state.data);
  };
  apply();
}


/* ---------- state ---------- */

function adoptState(next, { initial = false, external = false } = {}) {
  /* A save's returned state is newer than any poll that started before it.
     Otherwise a late response could roll the view back to the pre-save revision. */
  if (external) state.stateReadSeq++;
  state.data = next;

  /* 子代理草稿同一规则：默认继承只用于展示，绝不自动提交；轮询不覆盖
     正在编辑的草稿，非脏时才用已保存值回填。 */
  if (initial || !state.draft || !state.draftDirty) {
    state.draft = draftFromSaved(next);
    state.draftBaseEntity = draftFromSaved(next);
    state.draftBaseRevision = next.revision ?? null;
    state.draftDirty = false;
  } else {
    recomputeDraftDirty();
  }

  /* 轮询只更新 live snapshot；编辑中的 revision 与比较基线保持不变。 */
  /* An open drawer owns the old group revision even before it changes a field. */
  if (initial || (!state.conns.dirty && !state.conns.editing)) {
    state.conns.base = savedConnectionsSignature(next);
    state.conns.baseRevision = next.revision ?? null;
    state.conns.baseEntity = connectionDraftFrom(next);
    state.conns.draft = connectionDraftFrom(next);
    state.conns.dirty = false;
  } else {
    state.conns.dirty = connectionsDirty();
  }

  /* 接入向导同一规则：已保存的连接第一次出现时回填一次，之后保留用户已输入的地址与勾选。 */
  if (initial || !onboardTouched()) fillOnboardForm();

  /* 搜索与抓取服务遵守同一规则：非脏状态才回填，密钥草稿永不回填。 */
  if (searchAvailable(next)) {
    if (initial) {
      state.services.draft = serviceDraftFrom(next);
      state.services.baseRow = { search: serviceRowOf(next, 'search'), fetch: serviceRowOf(next, 'fetch') };
      state.services.base = serviceSignature(state.services.draft);
      state.services.dirty = false;
    } else if (!state.services.dirty) {
      state.services.base = serviceSignature(serviceDraftFrom(next));
    }
  }

  if (external && !initial) state.stateReadSeq++;
  settlePending(next);
}

function onboardTouched() {
  return state.onboard?.touched === true;
}

function settlePending(next) {
  const op = next && next.operation;
  if (!state.pendingOpId || !op || op.id !== state.pendingOpId || isInFlight(op)) return;
  const action = state.pendingAction || op.action;
  state.pendingOpId = null;
  state.pendingAction = null;
  void confirmActionResult(action, op.id);
}

function renderAll() {
  if (!state.data) return;
  renderWorkspaceSync(state.data);
  renderIntegrationMode(state.data);
  renderShell(state.data);
  renderOverview(state.data);
  renderOnboard();
  renderOperation(state.data);
  renderModels();
  renderSubagents();
  renderConnection();
  renderPicker();
  renderActions(state.data);
  renderGrowth(state.data);
  plusPanel.render(state.data);
  renderPreviewLifecycle(state.data);
}


async function loadState({ silent = false } = {}) {
  if (!state.key) {
    showGate('missing');
    return false;
  }
  const seq = ++state.stateReadSeq;
  const epoch = state.stateReadEpoch;
  const key = state.key;
  const current = () => seq === state.stateReadSeq && epoch === state.stateReadEpoch && key === state.key;
  if (!silent && !state.started) renderLoading();

  try {
    const next = await request(API.state);
    if (!current()) return false;
    if (!next || next.schemaVersion !== 1) throw new ApiError('面板返回了无法识别的状态格式', 'SCHEMA', 200);
    adoptState(next, { initial: !state.data });
    state.reachable = true;
    state.lastSync = Date.now();
    setHidden(dom.conn, true);
    if (!state.started) {
      state.started = true;
      dom.shell.hidden = false;
      dom.gate.hidden = true;
    }
    renderAll();
    return true;
  } catch (error) {
    if (!current()) return false;
    state.reachable = false;
    handleApiFailure(error);
    return false;
  }
}

function renderLoading() {
  if (dom.modelBody) {
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    /* 模型表共 8 列（ID、路由、协议、预算、输出、能力、档位、操作）。 */
    td.colSpan = 8;
    td.className = 'muted';
    td.textContent = '正在读取模型目录…';
    tr.append(td);
    dom.modelBody.replaceChildren(tr);
  }
  setText(dom.recentEmpty, '正在读取运行状态…');
  setText(dom.factGateway, '读取中');
  setText(dom.factModels, '读取中');
  setText(dom.factConfig, '读取中');
  setText(dom.saVerdictWord, '读取中');
}

function handleApiFailure(error) {
  if (error.code === 'NO_KEY' || error.code === 'unauthorized') {
    clearKey();
    stopPolling();
    showGate(error.code === 'NO_KEY' ? 'missing' : 'expired');
    return;
  }
  if (error.code === 'origin_refused') {
    stopPolling();
    showGate('refused');
    return;
  }
  setHidden(dom.conn, false);
  setText(dom.connText, error.code === 'NETWORK'
    ? '无法连接本机面板服务。服务可能已停止或端口已变化，页面上的状态停留在最后一次读取。'
    : error.message || '读取状态失败，页面上的状态停留在最后一次读取。');
  setText(dom.sync, state.lastSync ? '最后同步 ' + fmtTime(state.lastSync) + ' · 连接中断' : '尚未连接');
  if (!state.data) {
    renderLoading();
    if (!state.started) dom.shell.hidden = false;
  }
}


/* ---------- polling ---------- */

function startPolling() {
  stopPolling();
  state.timer = window.setInterval(() => {
    if (document.hidden) return;
    if (state.modelsSaving || state.connsSaving || state.services.saving || state.subagentSaving) return;
    loadState({ silent: true });
  }, POLL_MS);
}

function stopPolling() {
  if (state.timer !== null) {
    window.clearInterval(state.timer);
    state.timer = null;
  }
}


async function runAction(action, profileMode) {
  const data = state.data;
  if (!data || state.pendingAction) return;
  if (isInFlight(data.operation)) {
    showToast('已有操作正在进行，等待它结束后再试。', 'error');
    return;
  }

  state.pendingAction = action;
  renderActions(data);

  try {
    const res = await request(API.actions, { method: 'POST', body: { id: actionUUID(), at: Date.now(), action, ...(profileMode ? { profileMode } : {}) } });
    const op = res && res.operation ? res.operation : null;
    const opId = op && op.id ? op.id : null;
    if (op) {
      state.pendingOpId = opId;
      adoptState({ ...state.data, operation: op }, { external: true });
      renderOperation(state.data);
      renderActions(state.data);
    }
    const submitted = action === 'launch-kimi'
      ? '启动请求已提交，等待运行时状态。'
      : action === 'restart-kimi'
        ? '已请求正常退出 Kimi，等待退出完成后受管重启。'
        : '操作已提交，等待结果。';
    showToast(submitted, 'ok');
    /* 终态由 settlePending 在轮询读取到匹配 operation 时确认，避免重复提示。 */
    await loadState({ silent: true });
  } catch (error) {
    if (error.code === 'NO_KEY' || error.code === 'unauthorized') {
      clearKey();
      stopPolling();
      showGate('expired');
      return;
    }
    if (error.code === 'origin_refused') {
      stopPolling();
      showGate('refused');
      return;
    }
    if (error.status === 0) {
      setHidden(dom.conn, false);
      setText(dom.connText, '无法连接本机面板服务，操作结果尚未确认。请先刷新状态再决定是否重试。');
      showToast('操作结果尚未确认，请先刷新状态；本面板不会自动重试。', 'error');
      return;
    }
    showToast(error.message || '操作失败，请重试。', 'error');
    await loadState({ silent: true });
  } finally {
    if (state.pendingAction === action) state.pendingAction = null;
    if (state.data) renderActions(state.data);
  }
}

async function confirmActionResult(action, opId) {
  if (!opId) return;
  const op = state.data && state.data.operation;
  if (!op || op.id !== opId) return;
  if (isInFlight(op)) return;

  const failed = op.state === 'failed' || op.state === 'unavailable' || op.state === 'interrupted';

  if (action === 'copy-token') {
    if (op.state === 'copied') showToast('Token 已由服务端写入系统剪贴板。', 'ok');
    else if (failed) showToast('Token 未复制：' + (ACTION_STATE[op.state] || op.state) + (op.reason ? '（' + reasonText(op.reason) + '）' : ''), 'error');
    return;
  }

  if (action === 'check-upstream') {
    if (op.state === 'passed') {
      const diag = (state.data.diagnostics && state.data.diagnostics.upstream) || {};
      showToast('上游检查通过'
        + (typeof diag.modelCount === 'number' ? '，读到 ' + diag.modelCount + ' 个模型' : '') + '。', 'ok');
    } else if (op.state === 'failed') {
      showToast('上游检查未通过：' + (reasonText(op.reason) || '无法连接上游') + '。', 'error');
    }
    return;
  }

  if (action === 'check-compat') {
    if (op.state === 'verified') showToast('兼容检查通过：当前目录可被本机 Kimi 解析。', 'ok');
    else if (op.state === 'incompatible') showToast('兼容检查未通过：当前目录不被本机 Kimi 接受。', 'error');
    else if (op.state === 'unverified' || op.state === 'unavailable') showToast('兼容检查未能完成：' + (reasonText(op.reason) || '本机目录不可读') + '。', 'error');
    return;
  }

  if (failed && action === 'open-work') {
    showToast('未能打开 Work，请从 Kimi 窗口进入 Work，或在「接入与恢复」检查连接。', 'error');
    return;
  }
  if (failed) {
    showToast((ACTION_LABEL[action] || action) + '未完成：' + (ACTION_STATE[op.state] || op.state) + (op.reason ? '（' + reasonText(op.reason) + '）' : ''), 'error');
    return;
  }

  if (op.state === 'waiting-kimi-exit') {
    showToast('检测到另一个 Kimi 实例。请点击「正常退出并重启 Kimi」，由产品完成正常退出和受管重启。', 'error');
  }
}


async function handleSaveFailure(error, context) {
  if (error.code === 'NO_KEY' || error.code === 'unauthorized') {
    clearKey();
    stopPolling();
    showGate('expired');
    return;
  }
  if (error.code === 'origin_refused') {
    stopPolling();
    showGate('refused');
    return;
  }
  if (error.status === 0) {
    setHidden(dom.conn, false);
    setText(dom.connText, '无法连接本机面板服务，修改结果尚未确认。请先刷新状态再决定是否重试。');
    showToast('修改结果尚未确认，请先刷新状态；本面板不会自动重试。', 'error');
    return;
  }

  if (error.status === 409 && (error.code === 'configuration_conflict' || error.code === 'configuration_changed' || error.code === 'configuration_unavailable')) {
    await loadState({ silent: true });
    if (error.code === 'configuration_conflict') {
      if (context === 'models' && state.editor) {
        const differences = modelThreeWayDiff(state.editor);
        state.editor.conflict = { differences };
        state.editor.serverErrors = ['配置已在别处更新。请核对下面字段，再选择是否按最新配置重新应用你的修改。',
          ...differences.map(row => row.label + '：原值 ' + diffValue(row.base)
            + '；最新值 ' + diffValue(row.live) + '；你的草稿 ' + diffValue(row.draft))];
        renderEditor();
      }
      showToast('配置在别处被修改，已刷新状态；你的编辑仍然保留，请确认后重新保存。', 'error');
      return;
    }
    showToast(error.message, 'error');
    return;
  }

  if (error.status === 400) {
    if (context === 'subagents') {
      /* 后端拒绝（池非法、模型缺失、档位未声明等）保留用户草稿与
         已填的用途说明，只把真实错误码的中文回读显示在本视图。 */
      state.subagentErrors = [error.message];
      renderSubagents();
      showToast(error.message, 'error');
      return;
    }
    if (context === 'services' && dom.svcErrors) {
      setHidden(dom.svcErrors, false);
      dom.svcErrors.replaceChildren(...[error.message].map((message) => {
        const p = document.createElement('p');
        p.textContent = message;
        return p;
      }));
      return;
    }
    if (context === 'models' && state.editor) {
      state.editor.serverErrors = [error.message];
      renderEditor();
      return;
    }
    if (context === 'connection' && dom.connErrors) {
      setHidden(dom.connErrors, false);
      dom.connErrors.replaceChildren(...[error.message].map((message) => {
        const p = document.createElement('p');
        p.textContent = message;
        return p;
      }));
      return;
    }
    showToast(error.message, 'error');
    return;
  }

  if (error.saved) {
    await loadState({ silent: true });
    showToast(error.message + '（修改已保存）', 'error');
    return;
  }

  showToast(error.message || '保存失败，请重试。', 'error');
  await loadState({ silent: true });
}


function bindEvents() {
  Object.keys(VIEW_COPY).forEach(view => {
    const button = $('nav-' + view);
    if (button) button.addEventListener('click', () => setView(view));
  });
  document.querySelectorAll('[data-subtab-group][data-subtab]').forEach((button) => {
    button.addEventListener('click', () => {
      setSubtab(button.dataset.subtabGroup, button.dataset.subtab);
    });
  });

  if (dom.restartBannerLink) dom.restartBannerLink.addEventListener('click', () => {
    setView('models');
    setSubtab('models', 'connections');
  });
  $('budget-banner-link')?.addEventListener('click', () => {
    setView('usage');
    const tab = $('usage-tab-budget');
    tab?.click();
    tab?.focus();
  });
  $('overview-details')?.addEventListener('click', () => {
    setView('maintenance');
    setSubtab('maintenance', 'connection');
  });
  $('compat-details-link')?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('compat-diagnostics-panel');
  });
  dom.overviewOpenModels?.addEventListener('click', () => setView('models'));
  dom.overviewOpenSubagents?.addEventListener('click', () => setView('subagents'));
  dom.overviewOpenUsage?.addEventListener('click', () => setView('usage'));
  dom.overviewIssueAction?.addEventListener('click', runOverviewIssueAction);
  dom.quickstartToggle?.addEventListener('click', toggleQuickstartGuide);
  dom.quickstartDismiss?.addEventListener('click', dismissQuickstartGuide);
  $('quickstart-step-1-action')?.addEventListener('click', () => {
    const hasModels = Array.isArray(state.data?.models) && state.data.models.length > 0;
    if (hasModels) {
      setView('models');
      setSubtab('models', 'catalog');
    } else {
      openOnboardWizard();
    }
  });
  $('quickstart-step-2-mode')?.addEventListener('click', () => {
    setView('maintenance');
    setSubtab('maintenance', 'connection');
    revealWorkspaceSection('native-panel');
  });
  $('quickstart-step-3-action')?.addEventListener('click', () => {
    setView('subagents');
    setSubtab('subagents', 'models');
  });
  $('settings-reopen-guide')?.addEventListener('click', () => {
    reopenQuickstartGuide();
    setView('overview');
  });
  $('conns-usage')?.addEventListener('click', () => setView('usage'));
  document.querySelectorAll('[data-section]').forEach(button => {
    button.addEventListener('click', () => revealWorkspaceSection(button.dataset.section));
  });
  const brandHome = $('brand-home');
  if (brandHome) brandHome.addEventListener('click', () => setView('overview'));

  if (dom.refresh) dom.refresh.addEventListener('click', async () => {
    dom.refresh.disabled = true;
    /* 顶栏刷新是用户明确要求「现在就是最新的」：额度卡片与用量一起重读，
       否则停在用量页时点刷新，Kimi Code 与 Magpie 的读数仍是上一轮的。 */
    try {
      const tasks = [loadState(), refreshOverviewToday({ force: true })];
      if (state.view === 'usage') tasks.push(usagePanel.reload());
      if (plusPanel && typeof plusPanel.refresh === 'function') tasks.push(plusPanel.refresh({ live: state.view === 'usage' }));
      await Promise.all(tasks);
    } finally {
      dom.refresh.disabled = false;
    }
  });

  if (dom.connRetry) dom.connRetry.addEventListener('click', async () => {
    dom.connRetry.disabled = true;
    await loadState();
    dom.connRetry.disabled = false;
  });

  if (dom.gateRetry) dom.gateRetry.addEventListener('click', async () => {
    dom.gateRetry.disabled = true;
    const key = readKey();
    if (key) {
      state.key = key;
      const ok = await loadState();
      if (ok) startPolling();
      else showGate(state.reachable === false ? 'missing' : 'expired');
    } else {
      showGate('missing');
    }
    dom.gateRetry.disabled = false;
  });

  for (const [id, action] of Object.entries({
    'daemon-restart': 'restart-daemon', 'service-register': 'register-service',
    'service-enable': 'enable-login-service', 'service-disable': 'disable-login-service',
    'service-stop': 'stop-service', 'service-uninstall': 'uninstall-service',
  })) {
    const button = $(id);
    if (button) button.addEventListener('click', async () => {
      button.disabled = true;
      try { await previewServiceAction(action); }
      catch (error) { showToast(error.message || '后台服务操作未完成，原运行状态未替换。', 'error'); }
      finally { button.disabled = false; }
    });
  }
  const migrationTopLink = $('migration-top-banner-link');
  if (migrationTopLink) migrationTopLink.addEventListener('click', () => {
    setView('maintenance');
    setSubtab('maintenance', 'connection');
    revealWorkspaceSection('migration-banner');
  });
  const openUpdateSection = () => {
    setView('maintenance');
    revealWorkspaceSection('update-panel');
  };
  const updTopLink = $('update-top-banner-link');
  if (updTopLink) updTopLink.addEventListener('click', openUpdateSection);
  const overviewUpdGo = $('overview-update-go');
  if (overviewUpdGo) overviewUpdGo.addEventListener('click', openUpdateSection);
  const updReleaseLink = $('upd-release-link');
  if (updReleaseLink) updReleaseLink.addEventListener('click', async (event) => {
    event.preventDefault();
    if (updReleaseLink.dataset.busy === 'true') return;
    updReleaseLink.dataset.busy = 'true';
    try {
      await request('/api/updates/open-release', { method: 'POST', body: {} });
    } catch (error) {
      showToast(error.message || '无法调起系统默认浏览器，请手动访问发布页。', 'error');
    } finally {
      delete updReleaseLink.dataset.busy;
    }
  });
  const updCheck = $('upd-check');
  if (updCheck) updCheck.addEventListener('click', async () => {
    updCheck.disabled = true;
    try { await updateAction('/api/updates/check', {}); }
    catch (error) { showToast(error.message || '检查更新失败，当前运行未受影响。', 'error'); }
    finally { updCheck.disabled = false; }
  });
  const updDownload = $('upd-download');
  if (updDownload) updDownload.addEventListener('click', async () => {
    updDownload.disabled = true;
    try { await updateAction('/api/updates/download', {}); }
    catch (error) { showToast(error.message || '未能启动更新下载。', 'error'); }
    finally { updDownload.disabled = false; }
  });
  const updCancel = $('upd-cancel');
  if (updCancel) updCancel.addEventListener('click', async () => {
    updCancel.disabled = true;
    try { await updateAction('/api/updates/cancel', {}, '已取消当前更新操作。'); }
    catch (error) { showToast(error.message || '取消更新未成功。', 'error'); }
    finally { updCancel.disabled = false; }
  });
  const updRollback = $('upd-rollback');
  if (updRollback) updRollback.addEventListener('click', async () => {
    updRollback.disabled = true;
    try { await updateAction('/api/updates/rollback', {}); }
    catch (error) { showToast(error.message || '未能启动版本回退。', 'error'); }
    finally { updRollback.disabled = false; }
  });
  const updChannel = $('upd-channel');
  if (updChannel) updChannel.addEventListener('change', async () => {
    try { await updateAction('/api/updates/settings', { channel: updChannel.value }, '更新渠道已保存。'); }
    catch (error) { showToast(error.message || '保存更新渠道失败。', 'error'); }
  });
  const updAutoCheck = $('upd-auto-check');
  if (updAutoCheck) updAutoCheck.addEventListener('change', async () => {
    try { await updateAction('/api/updates/settings', { autoCheck: updAutoCheck.checked }, updAutoCheck.checked ? '已开启自动检查更新。' : '已关闭自动检查更新。'); }
    catch (error) { showToast(error.message || '保存自动检查设置失败。', 'error'); }
  });
  const migrationResume = $('migration-resume');
  if (migrationResume) migrationResume.addEventListener('click', async () => {
    migrationResume.disabled = true;
    try {
      const result = await request('/api/migration/resume', { method: 'POST', body: {} });
      if (result.state) {
        state.data = result.state;
        renderAll(state.data);
      } else {
        await loadState({ silent: true });
      }
      const status = result.migration?.status;
      if (status === 'completed') showToast('旧凭据已迁移并核对完成。', 'ok');
      else showToast('部分旧凭据仍需系统授权，请响应系统提示后重试。', 'error');
    } catch (error) {
      showToast(error.message || '凭据迁移未能完成，原条目保持不变。', 'error');
    } finally {
      migrationResume.disabled = false;
    }
  });
  request('/api/service').then(result => {
    if (!result.service || !state.data) return;
    state.data.service = { ...state.data.service, ...result.service, loginStartup: result.service.enabled ? 'enabled' : 'manual' };
    renderPreviewLifecycle(state.data);
  }).catch(() => {});

  if (dom.theme) dom.theme.addEventListener('change', () => applyTheme(dom.theme.value));

  /* 接入模式：切换只保存偏好，写入官方 Kimi 仍由「同步到 Work」完成。 */
  if (dom.integrationModeGroup) dom.integrationModeGroup.addEventListener('change', (event) => {
    const target = event.target;
    if (!target || target.name !== 'integration-mode') return;
    void chooseIntegrationMode(target.value, {
      onSaved: (next) => {
        /* 成功回执就是最新状态：直接采纳，避免轮询把模式与提示短暂改回旧值。 */
        adoptState(next, { external: true });
        renderAll();
      },
    });
  });

  /* 子代理目标 */
  if (dom.saModes) dom.saModes.addEventListener('change', (event) => {
    const target = event.target;
    if (!target || target.name !== 'mode') return;
    setMode(['force', 'pool'].includes(target.value) ? target.value : 'inherit');
  });
  if (dom.saModel) dom.saModel.addEventListener('change', () => pickModel(dom.saModel.value));
  if (dom.saEffort) dom.saEffort.addEventListener('change', () => pickEffort(dom.saEffort.value));
  if (dom.saRoleDefault) dom.saRoleDefault.addEventListener('change', () => pickRoleDefault(dom.saRoleDefault.value));
  if (dom.saRolePlan) dom.saRolePlan.addEventListener('change', () => pickRoleModel('plan', dom.saRolePlan.value));
  if (dom.saRoleCoder) dom.saRoleCoder.addEventListener('change', () => pickRoleModel('coder', dom.saRoleCoder.value));
  if (dom.saRoleExplore) dom.saRoleExplore.addEventListener('change', () => pickRoleModel('explore', dom.saRoleExplore.value));
  if (dom.saPoolDefault) dom.saPoolDefault.addEventListener('change', () => pickPoolDefault(dom.saPoolDefault.value));
  if (dom.saPoolAdd) dom.saPoolAdd.addEventListener('change', () => {
    const usable = Boolean(state.data && configReady(state.data)) && !isInFlight(state.data.operation);
    if (dom.saPoolAppend) dom.saPoolAppend.disabled = !usable || !dom.saPoolAdd.value || state.subagentSaving;
  });
  if (dom.saPoolAppend) dom.saPoolAppend.addEventListener('click', () => {
    const id = dom.saPoolAdd ? dom.saPoolAdd.value : '';
    const description = dom.saPoolDesc ? dom.saPoolDesc.value : '';
    if (!id) return;
    appendPoolModel(id, description);
    if (dom.saPoolDesc) dom.saPoolDesc.value = '';
  });
  if (dom.saPoolList) dom.saPoolList.addEventListener('click', (event) => {
    const node = event.target && event.target.closest ? event.target.closest('[data-pool-remove]') : null;
    if (!node) return;
    removePoolModel(node.dataset.poolRemove);
  });
  if (dom.saReset) dom.saReset.addEventListener('click', discardDraft);
  if (dom.saSave) dom.saSave.addEventListener('click', saveSubagentTarget);
  if (dom.saCapabilityNext) dom.saCapabilityNext.addEventListener('click', () => setView('models'));

  /* 模型目录 */
  if (dom.modelQ) dom.modelQ.addEventListener('input', () => {
    state.query = dom.modelQ.value;
    setHidden(dom.modelQClear, !state.query);
    renderModels();
  });
  if (dom.modelQ) dom.modelQ.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && state.query) {
      event.stopPropagation();
      clearModelQuery();
    }
  });
  if (dom.modelQClear) dom.modelQClear.addEventListener('click', clearModelQuery);
  if (dom.filterAll) dom.filterAll.addEventListener('click', () => onModelFilter('all'));
  if (dom.filterVision) dom.filterVision.addEventListener('click', () => onModelFilter('vision'));
  if (dom.filterTools) dom.filterTools.addEventListener('click', () => onModelFilter('tools'));
  if (dom.filterReasoning) dom.filterReasoning.addEventListener('click', () => onModelFilter('reasoning'));
  if (dom.modelEmptyReset) dom.modelEmptyReset.addEventListener('click', () => {
    state.query = '';
    if (dom.modelQ) dom.modelQ.value = '';
    setHidden(dom.modelQClear, true);
    onModelFilter('all');
    if (dom.modelQ && dom.modelQ.focus) dom.modelQ.focus();
  });
  if (dom.modelNew) dom.modelNew.addEventListener('click', openOnboardWizard);

  if (dom.modelBody) dom.modelBody.addEventListener('click', (event) => {
    const node = event.target && event.target.closest ? event.target.closest('button') : null;
    if (!node) return;
    if (node.dataset.rowEdit) {
      const model = findModel(node.dataset.rowEdit);
      if (model) openEditor(model);
      return;
    }
    if (node.dataset.rowDelete === 'ask') {
      state.deleteId = node.dataset.modelId;
      renderModels();
      return;
    }
    if (node.dataset.rowDelete === 'cancel') {
      state.deleteId = null;
      renderModels();
      return;
    }
    if (node.dataset.rowDelete === 'confirm') {
      const id = node.dataset.modelId;
      const models = ((state.data && state.data.models) || []).filter((model) => model.id !== id);
      saveCatalogue(models, { close: false, message: '模型 ' + id + ' 已从目录删除并应用。' });
    }
  });

  if (dom.inspectorClose) dom.inspectorClose.addEventListener('click', () => closeEditor(false));
  if (dom.modelCancel) dom.modelCancel.addEventListener('click', () => {
    if (state.editor && state.editor.confirmClose) {
      state.editor.confirmClose = false;
      renderEditor();
      return;
    }
    closeEditor(false);
  });
  if (dom.mFailoverAdd) {
    dom.mFailoverAdd.addEventListener('click', () => {
      const editor = state.editor;
      if (!editor) return;
      const rows = editor.draft.failover;
      if (rows.length >= 8) return;
      rows.push({ connectionId: '', upstreamModelId: '', upstreamProtocol: '' });
      editor.draft.touched.add('failover');
      renderFailoverRows(editor);
      renderEditor();
      const last = dom.mFailoverRows && dom.mFailoverRows.querySelector('.failover-row:last-child select');
      if (last && last.focus) last.focus();
    });
  }
  if (dom.mFailoverRows) {
    dom.mFailoverRows.addEventListener('change', (event) => {
      const control = event.target;
      if (!control || !control.dataset || !control.dataset.failoverField) return;
      const editor = state.editor;
      if (!editor) return;
      const host = control.closest('[data-failover-index]');
      if (!host) return;
      const index = Number(host.dataset.failoverIndex);
      const row = editor.draft.failover[index];
      if (!row) return;
      row[control.dataset.failoverField] = control.value;
      editor.draft.touched.add('failover');
      renderEditor();
    });
    dom.mFailoverRows.addEventListener('input', (event) => {
      const control = event.target;
      if (!control || !control.dataset || control.dataset.failoverField !== 'upstreamModelId') return;
      const editor = state.editor;
      if (!editor) return;
      const host = control.closest('[data-failover-index]');
      if (!host) return;
      const row = editor.draft.failover[Number(host.dataset.failoverIndex)];
      if (row) {
        row.upstreamModelId = control.value;
        editor.draft.touched.add('failover');
      }
    });
    dom.mFailoverRows.addEventListener('click', (event) => {
      const remove = event.target && event.target.closest ? event.target.closest('[data-failover-remove]') : null;
      if (!remove) return;
      const editor = state.editor;
      if (!editor) return;
      editor.draft.failover.splice(Number(remove.dataset.failoverRemove), 1);
      editor.draft.touched.add('failover');
      renderFailoverRows(editor);
      renderEditor();
    });
  }
  if (dom.modelForm) {
    const fields = {
      mId: 'id', mName: 'name', mContext: 'contextWindow', mOutput: 'maxOutputTokens',
      mTools: 'tools', mReasoning: 'reasoning', mVision: 'inputModalities',
      mEfforts: 'reasoningEfforts', mDefault: 'defaultEffort',
      mConnection: 'connectionId', mUpstream: 'upstreamModelId',
    };
    for (const [name, field] of Object.entries(fields)) {
      const element = dom[name];
      if (!element) continue;
      const touch = () => { if (state.editor) state.editor.draft.touched.add(field); };
      element.addEventListener('input', touch);
      element.addEventListener('change', touch);
    }
    dom.modelForm.addEventListener('input', () => {
      if (!state.editor) return;
      state.editor.serverErrors = [];
      readEditorFields();
      renderEditor();
    });
    dom.modelForm.addEventListener('change', () => {
      if (!state.editor) return;
      state.editor.serverErrors = [];
      readEditorFields();
      renderEditor();
    });
    dom.modelForm.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && state.editor) {
        event.preventDefault();
        closeEditor(false);
      }
      if (event.key === 'Enter' && (event.metaKey || event.ctrlKey) && dom.modelSave && !dom.modelSave.disabled) {
        event.preventDefault();
        const models = catalogueWith(state.editor, state.data.models);
        saveCatalogue(models);
      }
    });
  }
  if (dom.modelSave) dom.modelSave.addEventListener('click', () => {
    if (!state.editor || dom.modelSave.disabled) return;
    const { errors } = validateEditor(state.editor);
    if (errors.length) {
      renderEditor();
      return;
    }
    saveCatalogue(catalogueWith(state.editor, state.data.models));
  });

  /* 命名连接 */
  if (dom.connsAdd) dom.connsAdd.addEventListener('click', openOnboardWizard);
  if (dom.connsReset) dom.connsReset.addEventListener('click', () => {
    state.conns.draft = connectionDraftFrom(state.data);
    state.conns.base = savedConnectionsSignature(state.data);
    state.conns.baseRevision = state.data.revision ?? null;
    state.conns.baseEntity = connectionDraftFrom(state.data);
    state.conns.editing = null;
    renderConnections();
  });
  if (dom.connsSave) dom.connsSave.addEventListener('click', saveConnections);
  if (dom.connsList) dom.connsList.addEventListener('click', (event) => {
    const node = event.target && event.target.closest ? event.target.closest('button') : null;
    if (!node) return;
    if (node.dataset.connectionCheck) {
      void runConnectionCheck(node.dataset.connectionCheck);
      return;
    }
    if (node.dataset.connectionEdit) {
      const row = (state.conns.draft || []).find((entry) => entry.id === node.dataset.connectionEdit);
      if (row) openConnectionEditor(row);
      return;
    }
    if (node.dataset.connectionRemove) {
      const id = node.dataset.connectionRemove;
      const used = ((state.data && state.data.models) || []).filter((model) => model.connectionId === id);
      if (used.length) {
        setConnErrors(['仍有 ' + used.length + ' 个模型使用该连接（'
          + used.slice(0, 3).map((model) => model.id).join('、') + (used.length > 3 ? ' 等' : '')
          + '）。请先在模型管理改到其他连接，再移除。']);
        return;
      }
      state.conns.draft = (state.conns.draft || []).filter((entry) => entry.id !== id);
      if (state.conns.editing && state.conns.editing.originalId === id) state.conns.editing = null;
      setConnErrors([]);
      renderConnections();
    }
  });
  if (dom.connEditorClose) dom.connEditorClose.addEventListener('click', closeConnectionEditor);
  if (dom.connEditorCancel) dom.connEditorCancel.addEventListener('click', closeConnectionEditor);
  if (dom.connEditorApply) dom.connEditorApply.addEventListener('click', saveConnections);
  if (dom.connEditorTest) dom.connEditorTest.addEventListener('click', () => { void discoverConnectionEditorModels(); });
  if (dom.cePreset) dom.cePreset.addEventListener('change', () => {
    if (!state.conns.editing) return;
    applyProviderPreset(dom.cePreset.value);
  });
  [dom.ceName, dom.ceId, dom.ceBase, dom.ceKey, dom.ceKeyEnv].forEach((field) => {
    if (!field) return;
    field.addEventListener('input', () => {
      if (!state.conns.editing) return;
      if (field === dom.ceKey) state.conns.editing.keyEdited = true;
      readConnectionEditor();
      if (state.conns.editing.errors && state.conns.editing.errors.length) {
        state.conns.editing.errors = [];
        setConnErrors([]);
      }
    });
  });
  [dom.ceChat, dom.ceResponses].forEach((field) => {
    if (!field) return;
    field.addEventListener('change', () => {
      if (!state.conns.editing) return;
      readConnectionEditor();
    });
  });

  /* 候选选择器 */
  if (dom.mCandidates) dom.mCandidates.addEventListener('click', openPicker);
  if (dom.pickerClose) dom.pickerClose.addEventListener('click', closePicker);
  if (dom.pickerQ) dom.pickerQ.addEventListener('input', () => {
    state.picker.query = dom.pickerQ.value;
    renderPicker();
    scheduleSearch();
  });
  if (dom.pickerScan) dom.pickerScan.addEventListener('click', () => {
    const editor = state.editor;
    if (!editor) return;
    void runDiscovery(editor.draft.connectionId);
  });
  if (dom.pickerList) dom.pickerList.addEventListener('click', (event) => {
    const node = event.target && event.target.closest ? event.target.closest('[data-candidate-id]') : null;
    if (!node) return;
    pickCandidate(node.dataset.candidateId);
  });
  if (dom.mConnection) dom.mConnection.addEventListener('change', () => {
    const editor = state.editor;
    if (!editor) return;
    editor.draft.connectionId = dom.mConnection.value;
    editor.draft.metadataSource = null;
    if (editor.draft.upstreamModelId) {
      editor.draft.id = routeIdFor(editor.draft.connectionId, editor.draft.upstreamModelId);
      if (dom.mId) dom.mId.value = editor.draft.id;
    }
    state.picker.models = null;
    state.picker.reason = null;
    state.picker.state = null;
    state.picker.checkedAt = null;
    state.picker.source = null;
    state.picker.scope = pickerScope();
    renderEditor();
    renderPicker();
    /* A saved connection already has a catalog, so changing the connection with
       the picker open reloads it instead of leaving an empty list. */
    if (state.picker.open) void searchCandidates(state.picker.query);
  });
  if (dom.mUpstream) dom.mUpstream.addEventListener('input', () => {
    const editor = state.editor;
    if (!editor) return;
    editor.draft.upstreamModelId = dom.mUpstream.value;
    editor.draft.metadataSource = null;
    const suggested = routeIdFor(editor.draft.connectionId, editor.draft.upstreamModelId);
    if (suggested && !editor.draft.touched.has('id')) {
      editor.draft.id = suggested;
      if (dom.mId) dom.mId.value = suggested;
    }
    renderEditor();
  });
  if (dom.mBudgetGroup) dom.mBudgetGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-budget]') : null;
    if (!button || button.disabled) return;
    applyBudgetPreset(button.dataset.budget);
  });
  if (dom.mProtocolGroup) dom.mProtocolGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-protocol]') : null;
    if (!button || button.disabled) return;
    chooseProtocol(button.dataset.protocol);
  });
  if (dom.mUpstreamProtocolGroup) dom.mUpstreamProtocolGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-upstream-protocol]') : null;
    if (!button || button.disabled) return;
    chooseUpstreamProtocol(button.dataset.upstreamProtocol);
  });
  if (dom.mEffortGroup) dom.mEffortGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-effort-choice]') : null;
    if (!button || button.disabled) return;
    toggleEffortChoice(button.dataset.effortChoice);
  });

  /* 接入向导：字段变化只更新本地提示，不改配置；两个主按钮才发请求。 */
  if (dom.onboardPreset) dom.onboardPreset.addEventListener('change', () => {
    applyOnboardPreset(dom.onboardPreset.value);
  });
  [dom.onboardBase, dom.onboardId, dom.onboardName].forEach((field) => {
    if (!field) return;
    field.addEventListener('input', () => {
      state.onboard.touched = true;
      setHidden(dom.onboardErrors, true);
      renderOnboard();
    });
  });
  if (dom.onboardKey) dom.onboardKey.addEventListener('input', () => {
    state.onboard.touched = true;
    if (dom.onboardId && !dom.onboardId.value) {
      const saved = savedOnboardTarget();
      if (saved && saved.id) dom.onboardId.value = saved.id;
    }
    setHidden(dom.onboardErrors, true);
    renderOnboard();
  });
  if (dom.onboardDiscover) dom.onboardDiscover.addEventListener('click', () => { void discoverOnboardModels(); });
  if (dom.onboardRecommended) dom.onboardRecommended.addEventListener('click', useRecommendedOnboardModels);
  if (dom.onboardModelQuery) dom.onboardModelQuery.addEventListener('input', () => {
    state.onboard.query = dom.onboardModelQuery.value;
    renderOnboard();
  });
  if (dom.onboardSelectAll) dom.onboardSelectAll.addEventListener('click', toggleFilteredOnboardModels);
  if (dom.onboardBatchChat) dom.onboardBatchChat.addEventListener('click', () => applyOnboardBatchProtocol('chat'));
  if (dom.onboardBatchContextApply) dom.onboardBatchContextApply.addEventListener('click', applyOnboardBatchContext);
  if (dom.onboardSave) dom.onboardSave.addEventListener('click', () => { void saveOnboard(); });
  if (dom.onboardRetryApply) dom.onboardRetryApply.addEventListener('click', () => { void applyConnections(); });
  if (dom.applyButton) dom.applyButton.addEventListener('click', () => { void applyConnections(); });
  if (dom.onboardAdvancedToggle) dom.onboardAdvancedToggle.addEventListener('click', () => {
    state.onboard.advanced = !state.onboard.advanced;
    setHidden(dom.onboardAdvanced, !state.onboard.advanced);
    dom.onboardAdvancedToggle.setAttribute('aria-expanded', state.onboard.advanced ? 'true' : 'false');
    setText(dom.onboardAdvancedToggle, state.onboard.advanced ? '收起高级恢复选项' : '高级恢复选项');
  });

  document.querySelectorAll('[data-action]').forEach((button) => {
    button.addEventListener('click', () => runAction(button.dataset.action, button.dataset.profileMode));
  });

  /* 原生搜索与抓取服务 */
  [dom.svcSearchOn, dom.svcFetchOn].forEach((field) => {
    if (!field) return;
    field.addEventListener('change', () => {
      readServiceForm();
      renderErrorList(dom.svcErrors, []);
      renderServices(state.data);
    });
  });
  [dom.svcSearchUrl, dom.svcFetchUrl, dom.svcSearchKey, dom.svcFetchKey].forEach((field) => {
    if (!field) return;
    field.addEventListener('input', () => {
      readServiceForm();
      renderErrorList(dom.svcErrors, []);
      renderServices(state.data);
    });
  });
  if (dom.svcReset) dom.svcReset.addEventListener('click', () => {
    state.services.draft = serviceDraftFrom(state.data);
    state.services.searchKey = '';
    state.services.fetchKey = '';
    state.services.base = serviceSignature(state.services.draft);
    state.services.dirty = false;
    fillServiceForm();
    renderErrorList(dom.svcErrors, []);
    renderServices(state.data);
  });
  if (dom.svcSave) dom.svcSave.addEventListener('click', saveSearchServices);

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && state.key && !state.modelsSaving && !state.connsSaving && !state.services.saving) {
      loadState({ silent: true });
    }
  });
  window.addEventListener('online', () => {
    if (state.key) loadState({ silent: true });
  });
}

/* ---------- key rotation ---------- */

function adoptLocationKey() {
  let key = null;
  const raw = window.location.hash || '';
  if (raw.length > 1) {
    try {
      key = new URLSearchParams(raw.slice(1)).get('key');
    } catch {
      key = null;
    }
  }
  if (!key || key === state.key) return false;

  try {
    sessionStorage.setItem(KEY_STORE, key);
  } catch {
    /* 无存储时仅本次内存使用 */
  }
  history.replaceState(null, '', window.location.pathname + window.location.search);

  state.key = key;
  state.stateReadEpoch++;
  state.data = null;
  state.editor = null;
  state.deleteId = null;
  state.started = false;
  state.modelsSaving = false;
  state.connsSaving = false;
  state.pendingAction = null;
  state.pendingOpId = null;
  state.lastSync = null;
  /* 新会话属于另一份配置：旧草稿与已读候选必须丢弃，否则会把上一会话的
     连接、Key 或上下文设置带进这一份。 */
  state.conns = { base: null, baseRevision: null, baseEntity: null, dirty: false, editing: null, draft: null, errors: [] };
  state.services = { base: null, dirty: false, saving: false, searchKey: '', fetchKey: '' };
  state.draft = null;
  state.draftDirty = false;
  state.draftBaseRevision = null;
  state.draftBaseEntity = null;
  state.subagentModeCache = { force: null, pool: null };
  state.subagentSaving = false;
  state.subagentErrors = [];
  state.picker = { open: false, models: null, query: '', state: null, checkedAt: null, source: null, reason: null,
    busy: false, seq: 0, hint: null, scope: null };
  /* 新会话属于另一份配置：向导里读到的目录与 Key 不能跨会话沿用。 */
  state.onboard = { discovery: null, discoveryFor: null, seq: 0, busy: false, saving: false, advanced: state.onboard.advanced === true, touched: false, query: '' };
  if (dom.onboardKey) dom.onboardKey.value = '';
  if (dom.onboardModelQuery) dom.onboardModelQuery.value = '';
  state.applyUnsupported = false;
  state.applyRefusal = null;
  state.providers = null;
  state.overviewToday = { key: null, requests: null, totalTokens: null, loading: false, fetchedAt: 0, requestCount: -1 };
  state.overviewIssueAction = null;
  if (typeof usagePanel !== 'undefined' && usagePanel && typeof usagePanel.reset === 'function') usagePanel.reset();
  resetModelSignature();
  resetRecentSignature();
  resetSubagentSignatures();
  resetIntegrationModeSignature();
  stopPolling();
  setHidden(dom.conn, true);
  dom.toast.hidden = true;
  dom.shell.hidden = true;
  showGate('checking');
  void loadProviders();
  loadState().then((ok) => {
    if (!ok) return;
    if (typeof usagePanel !== 'undefined' && usagePanel && state.view === 'usage') usagePanel.show();
    startPolling();
  });
  return true;}

/* ---------- boot ---------- */

/* 供应商预设只读取一次，用于连接编辑器与首设页的引导。
   接口不可用时留空数组：表单退回纯手填，不阻塞其它功能。 */
async function loadProviders() {
  if (!state.key || state.providers) return;
  try {
    const res = await request(API.providers);
    if (res && Array.isArray(res.presets)) {
      state.providers = res;
      renderConnectionEditor();
      fillOnboardPresets();
      renderOnboard();
      refreshSavedProtocolDeclarations();
    }
  } catch {
    state.providers = { presets: [] };
  }
}


function boot() {
  initTheme();
  initWindowChrome();
  if (typeof globalThis.KimiWorkGrowth?.mountGrowth === 'function') {
    renderGrowth = globalThis.KimiWorkGrowth.mountGrowth({ request, getState: () => state.data, getProviders: () => state.providers,
      refresh: loadState, notify: showToast, runAction });
  }
  if (typeof globalThis.KimiWorkUsage?.mount === 'function') {
    usagePanel = globalThis.KimiWorkUsage.mount({ request, notify: showToast });
  }
  mountUsageRequests();
  if (typeof globalThis.KimiWorkPlus?.mount === 'function') {
    plusPanel = globalThis.KimiWorkPlus.mount({ request, notify: showToast, getState: () => state.data, setState: (data) => adoptState(data),
      onAccounts: () => { setView('maintenance'); setSubtab('maintenance', 'accounts'); revealWorkspaceSection('plus-accounts-panel'); } });
  }
  /* 会话密钥必须先就绪：停留在「用量」等视图时，挂载就会立刻发出第一笔请求，
     先挂视图再读密钥会让那次请求带着空密钥失败，界面上闪现「缺少面板会话密钥」。 */
  state.key = readKey();
  initView();
  bindEvents();
  observeMasthead();
  state.actionButtons = Array.from(document.querySelectorAll('[data-action]'));

  /* 面板重启后粘贴新链接只改变 hash，浏览器不会重新执行脚本，
     因此在 hash 变化与页面重新可见时都重新取一次密钥。密钥已更换时
     adoptLocationKey 会自己重读状态；同一次可见事件不再并发补一次。 */
  window.addEventListener('hashchange', adoptLocationKey);
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    if (adoptLocationKey()) return;
    if (!state.key || state.started) return;
    loadState().then((ok) => {
      if (ok && !state.timer) startPolling();
    });
  });

  if (!state.key) {
    showGate('missing');
    return;
  }

  void loadProviders();
  showGate('checking');
  loadState().then((ok) => {
    if (!ok) return;
    startPolling();
  });
}

boot();

export {
  adoptState,
  handleSaveFailure,
  loadState,
  renderAll,
  runAction,
  setView,
  stopPolling,
};

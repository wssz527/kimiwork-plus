import { applyConnections, applyOnboardBatchContext, applyOnboardPreset, applyProviderPreset, cancelConnectionDelete, closeConnectionEditor, closeConnectionModelPicker, configReady, confirmConnectionDelete, connectionDefaultUpstreamProtocol, connectionDraftFrom, connectionsDirty, discoverConnectionEditorModels, discoverOnboardModels, draftConnectionProtocol, fillOnboardForm, fillOnboardPresets, formatApiErrorWithModels, openConnectionEditor, onboardEditingText, openOnboardWizard, readConnectionEditor, reloadConnectionModelPicker, renderConnection, renderConnectionEditor, renderConnections, renderOnboard, requestConnectionDelete, saveConnectionModelPicker, saveConnections, saveOnboard, savedConnectionsSignature, savedOnboardTarget, setConnectionPickerQuery, setOnboardProtocol, toggleConnectionModelPicker, toggleConnectionPickerItem, toggleConnectionPickerSelectAll, toggleFilteredOnboardModels, useRecommendedOnboardModels } from './app-connections.js';
import { $, ACTION_LABEL, ACTION_STATE, API, ApiError, KEY_STORE, POLL_MS, SUBTAB_STORE, VIEWS, VIEW_ALIASES, VIEW_COPY, VIEW_STORE, actionUUID, applyTheme, clearKey, dom, fmtTime, initTheme, initWindowChrome, mountUsageRequests, observeMasthead, postWailsMessage, readKey, reasonText, renderErrorList, request, revealWorkspaceSection, scrollMainToElement, setConnErrors, setHidden, setSubtab, setText, showGate, showToast, state } from './app-core.js';
import { applyBudgetPreset, applyInferredModelDefaults, catalogueWith, chooseUpstreamProtocol, clearModelQuery, closeEditor, closePicker, diffValue, findModel, modelThreeWayDiff, onModelFilter, openEditor, openPicker, pickCandidate, pickerScope, readEditorFields, renderEditor, renderFailoverRows, renderModels, renderPicker, resetModelSignature, runConnectionCheck, runDiscovery, saveCatalogue, scheduleSearch, searchCandidates, suggestedRouteId, toggleEffortChoice, validateEditor } from './app-models.js';
import { cancelIntegrationModeConfirm, confirmIntegrationMode, requestIntegrationModeConfirm, dismissQuickstartGuide, exportDiagnostics, isInFlight, previewServiceAction, refreshOverviewToday, renderActions, renderIntegrationMode, renderOperation, renderOverview, renderPreviewLifecycle, renderShell, renderUpdates, renderWorkspaceSync, reopenQuickstartGuide, resetIntegrationModeSignature, resetRecentSignature, runOverviewIssueAction, toggleQuickstartGuide, updateAction } from './app-overview.js';
import { appendPoolModel, discardDraft, draftFromSaved, pickEffort, pickModel, pickPoolDefault, pickRoleDefault, pickRoleModel, recomputeDraftDirty, removePoolModel, renderSubagents, resetSubagentSignatures, saveSubagentTarget, setMode } from './app-work.js';
import { mountConversationsPage } from './app-conversations.js';

let renderGrowth = () => {};
let usagePanel = { show() {}, reload: async () => {}, reset() {} };
let plusPanel = { render() {}, refresh: async () => {} };
let conversationsPanel = { show() {}, refresh: async () => {}, onState() {} };

function initSubtabs() {
  let stored = {};
  try {
    const raw = sessionStorage.getItem(SUBTAB_STORE);
    const parsed = raw ? JSON.parse(raw) : null;
    if (parsed && typeof parsed === 'object') stored = parsed;
  } catch {
    stored = {};
  }
  state.subtabs = { ...(state.subtabs || {}), ...stored };
  if (typeof document === 'undefined' || typeof document.querySelectorAll !== 'function') return;
  const groups = new Map();
  document.querySelectorAll('[data-subtab-group][data-subtab]').forEach((btn) => {
    const group = btn.dataset.subtabGroup;
    const tab = btn.dataset.subtab;
    if (!group || !tab) return;
    if (!groups.has(group)) groups.set(group, []);
    groups.get(group).push(tab);
  });
  for (const [group, tabs] of groups.entries()) {
    const preferred = state.subtabs[group];
    const active = tabs.includes(preferred) ? preferred : tabs[0];
    if (active) setSubtab(group, active, { persist: false });
  }
}

function initView() {
  let stored = null;
  try {
    stored = sessionStorage.getItem(VIEW_STORE);
  } catch {
    stored = null;
  }
  const resolved = VIEW_ALIASES[stored] || stored;
  setView(VIEWS.includes(resolved) ? resolved : 'overview', { persist: false });
}

function setView(view, { persist = true } = {}) {
  view = VIEW_ALIASES[view] || view;
  if (!VIEWS.includes(view)) view = 'overview';
  const apply = () => {
    if (state.integrationModeConfirm && view !== 'maintenance') cancelIntegrationModeConfirm();
    state.view = view;
    if (document.body) document.body.dataset.view = view;
    VIEWS.forEach((name) => {
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
    state.composing = false;
    clearDisconnectConfirm();
    if (state.data && view === 'overview') renderOverview(state.data, { view: 'overview' });
    if (view === 'models') {
      if (state.data) {
        renderWorkspaceSync(state.data);
        if (!state.onboard?.busy && !state.onboard?.saving) renderOnboard();
      }
      renderModels();
      renderConnection();
      renderPicker();
      renderSubagents();
      if (state.data) renderGrowth(state.data, { view: 'models' });
    }
    if (view === 'conversations') {
      if (state.key) conversationsPanel.show();
    }
    if (view === 'accounts') {
      if (state.data) plusPanel.render(state.data, { view: 'accounts' });
    }
    if (view === 'maintenance') {
      if (state.data) {
        renderOverview(state.data, { view: 'maintenance' });
        renderIntegrationMode(state.data);
        renderGrowth(state.data, { view: 'maintenance' });
      }
    }
    /* 门禁页没有会话密钥：此时再展示用量面板，挂载期那笔请求只会带空密钥失败。 */
    if (view === 'usage') { if (state.key) usagePanel.show(); void plusPanel.refresh(); }
    if (state.data) {
      plusPanel.render(state.data, { view });
      renderUpdates(state.data);
    }
  };
  apply();
}


/* ---------- state ---------- */

function adoptState(next, { initial = false, external = false } = {}) {
  /* A save's returned state is newer than any poll that started before it.
     Otherwise a late response could roll the view back to the pre-save revision. */
  if (state.pendingService && next && typeof next === 'object') {
    next = {
      ...next,
      service: { ...next.service, ...state.pendingService, loginStartup: state.pendingService.enabled ? 'enabled' : 'manual' },
    };
    state.pendingService = null;
  }
  state.data = next;
  state.conversationsStateChanged?.(next);

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

  if (external && !initial) state.stateReadSeq++;
  settlePending(next);
}

function onboardTouched() {
  return state.onboard?.touched === true;
}

/* 向导的保存错误由 handleSaveFailure 写进会话状态，用户改过输入就清掉：
   同一条后端拒绝在改完字段后继续挂着，会被读成「刚才那次修改也没用」。 */
function clearOnboardSaveErrors() {
  if (!state.onboard || !Array.isArray(state.onboard.saveErrors) || !state.onboard.saveErrors.length) return;
  state.onboard.saveErrors = [];
  renderErrorList(dom.onboardSaveErrors, []);
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
  const view = state.view || 'overview';
  renderShell(state.data);
  renderOperation(state.data);
  renderActions(state.data);
  plusPanel.render(state.data, { view });
  renderPreviewLifecycle(state.data);
  if (view === 'overview') {
    renderOverview(state.data, { view: 'overview' });
  } else if (view === 'models') {
    renderWorkspaceSync(state.data);
    renderModels();
    renderConnection();
    renderPicker();
    renderSubagents();
    renderGrowth(state.data, { view: 'models' });
  } else if (view === 'conversations') {
    conversationsPanel.show();
  } else if (view === 'accounts') {
    plusPanel.render(state.data, { view: 'accounts' });
  } else if (view === 'maintenance') {
    renderOverview(state.data, { view: 'maintenance' });
    renderIntegrationMode(state.data);
    renderGrowth(state.data, { view: 'maintenance' });
  }
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
    if (current()) adoptState(next, { initial: !state.data });
    state.reachable = true;
    state.lastSync = Date.now();
    setHidden(dom.conn, true);
    if (!state.started) {
      state.started = true;
      dom.shell.hidden = false;
      dom.gate.hidden = true;
    }
    renderAll();
    flushPendingUpdatesPanelReveal();
    if (!state.initialUpdateCheckDone) {
      state.initialUpdateCheckDone = true;
      maybeCheckUpdatesOnFocus();
    }
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
    td.colSpan = 7;
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
    if (!state.started) {
      if (dom.shell) dom.shell.hidden = false;
      setHidden(dom.gate, true);
    }
  }
}


/* ---------- polling ---------- */

function startPolling() {
  stopPolling();
  state.timer = window.setInterval(() => {
    if (document.hidden) return;
    if (editingBusy()) return;
    loadState({ silent: true });
  }, POLL_MS);
}

/* 任何正在进行的保存或向导请求都拥有输入焦点：状态轮询只该刷新快照，
   不能让一次 2 秒后的读取覆盖用户刚敲进去的连接或模型。 */
function editingBusy() {
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  const connEditing = Boolean(state.conns && state.conns.editing && active
    && ((dom.connEditor && typeof dom.connEditor.contains === 'function' && dom.connEditor.contains(active))
      || ['ce-name', 'ce-id', 'ce-base', 'ce-key', 'ce-keyenv'].includes(active.id)));
  const connPickerActive = Boolean(state.connPicker && state.connPicker.connectionId
    && (state.connPicker.loading || state.connPicker.saving
      || (active && active.dataset && ('connPickerQuery' in active.dataset || 'connPickerItem' in active.dataset))));
  return Boolean(state.composing || state.modelsSaving || state.connsSaving || state.connDeletingId || state.subagentSaving
    || connPickerActive
    || (state.onboard && (state.onboard.saving || state.onboard.busy)) || onboardEditingText()
    || (state.conns && state.conns.editing && (state.conns.editing.discovering || connEditing)));
}

function stopPolling() {
  if (state.timer !== null) {
    window.clearInterval(state.timer);
    state.timer = null;
  }
}


async function runAction(action, profileMode) {
  clearDisconnectConfirm();
  const data = state.data;
  if (!data || state.pendingAction) return;
  if (isInFlight(data.operation)) {
    showToast('已有操作正在进行，等待它结束后再试。', 'error');
    return;
  }

  state.pendingAction = action;
  renderActions(data);

  try {
    const body = {
      id: actionUUID(),
      at: Date.now(),
      action,
      ...(action === 'disconnect-models' ? { removeDns: true } : {}),
      ...(profileMode ? { profileMode } : {}),
    };
    const res = await request(API.actions, { method: 'POST', body });
    const op = res && res.operation ? res.operation : null;
    const opId = op && op.id ? op.id : null;
    if (op) {
      state.pendingOpId = opId;
      adoptState({ ...state.data, operation: op }, { external: true });
      renderOperation(state.data);
      renderActions(state.data);
    }
    const needsAutoLaunch = data?.integration?.desktop?.managed !== true && data?.kimiProcess?.running !== true;
    const submitted = action === 'launch-kimi'
      ? '启动请求已提交，等待运行时状态。'
      : action === 'restart-kimi'
        ? '已请求正常退出 Kimi，等待退出完成后受管重启。'
        : action === 'disconnect-models'
          ? (needsAutoLaunch
            ? '正在自动启动 Kimi Work 并移除本产品接管的模型来源，请稍候。'
            : '正在通过官方接口移除本产品接管的模型来源。')
          : (action === 'connect-kimi' || action === 'prepare-native')
            ? (needsAutoLaunch
              ? '正在自动启动 Kimi Work 并同步模型配置，请稍候。'
              : '正在同步模型配置到 Kimi Work，请稍候。')
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
  if (action === 'disconnect-models' && op.state === 'removed') {
    /* 移除模型成功但本机解析记录没清干净：不能报成完全成功，也不能沿用
       「尚未继续原生接入」这类面向接入方向的原文案。 */
    const dnsWarning = op.result && op.result.dnsWarning;
    if (dnsWarning === 'setup_cancelled') {
      showToast('已移除本产品接管的模型来源；系统授权已取消，本机域名解析记录未一并清理。服务商与模型仍保留，可随时重新同步接管。', 'warn');
    } else if (dnsWarning) {
      showToast('已移除本产品接管的模型来源；本机域名解析记录未一并清理（'
        + (reasonText(dnsWarning) || dnsWarning) + '）。服务商与模型仍保留，可随时重新同步接管。', 'warn');
    } else {
      showToast('已移除本产品接管的模型来源。服务商与模型仍保留，可随时重新同步接管。', 'ok');
    }
    return;
  }
  if (failed) {
    showToast((ACTION_LABEL[action] || action) + '未完成：' + (ACTION_STATE[op.state] || op.state) + (op.reason ? '（' + reasonText(op.reason) + '）' : ''), 'error');
    return;
  }

  if (op.state === 'waiting-kimi-exit') {
    showToast('检测到另一个未受托管的官方 Kimi 实例。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。', 'error');
    return;
  }
  if (op.state === 'waiting-login') {
    showToast('已打开官方 Kimi，但尚未确认登录账号。请先在官方窗口完成登录，再重新操作。', 'warn');
    return;
  }
  if (op.state === 'requires-restart') {
    showToast('模型配置已写入，请等当前任务结束后点击「正常退出并重启 Kimi」使配置生效。', 'warn');
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

  if (error.status === 400 || (error.status === 409 && Array.isArray(error.models) && error.models.length)) {
    if (context === 'subagents') {
      /* 后端拒绝（池非法、模型缺失、档位未声明等）保留用户草稿与
         已填的用途说明，只把真实错误码的中文回读显示在本视图。 */
      state.subagentErrors = [error.message];
      renderSubagents();
      showToast(error.message, 'error');
      return;
    }
    if (context === 'models' && state.editor) {
      state.editor.serverErrors = [formatApiErrorWithModels(error)];
      renderEditor();
      return;
    }
    if (context === 'connections' && dom.connsErrors) {
      /* 后端拒绝（连接标识冲突、Key 缺失、协议变更冲突等）要留在连接页，草稿与已填的 Key
         都不能因为一次 400/409 被清掉。 */
      const msg = formatApiErrorWithModels(error);
      setConnErrors([msg]);
      renderConnections();
      showToast(msg, 'error');
      return;
    }
    if (context === 'onboard') {
      /* 向导同理：错误显示在向导自己的保存错误区，连接草稿与勾选保持原样，
         用户改完还能直接重试。renderOnboard 会重画 ob-errors，所以保存类的
         错误必须放在它不会覆盖的位置，并存进会话状态活过状态轮询。 */
      const msg = formatApiErrorWithModels(error);
      state.onboard.saveErrors = [msg];
      renderErrorList(dom.onboardSaveErrors, state.onboard.saveErrors);
      showToast(msg, 'error');
      return;
    }
    if (context === 'connection' && dom.connErrors) {
      setHidden(dom.connErrors, false);
      dom.connErrors.replaceChildren(...[formatApiErrorWithModels(error)].map((message) => {
        const p = document.createElement('p');
        p.textContent = message;
        return p;
      }));
      return;
    }
    showToast(formatApiErrorWithModels(error), 'error');
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


function clearDisconnectConfirm() {
  const active = state.disconnectConfirm;
  if (active && active.timer != null) window.clearTimeout(active.timer);
  state.disconnectConfirm = null;
  const targets = new Set(['workspace-disconnect-confirm', 'native-disconnect-confirm']);
  if (active && active.targetId) targets.add(active.targetId);
  for (const id of targets) {
    const box = $(id);
    if (!box) continue;
    setHidden(box, true);
    if (typeof box.replaceChildren === 'function') box.replaceChildren();
  }
  if (typeof document !== 'undefined' && typeof document.querySelectorAll === 'function') {
    document.querySelectorAll('[data-action="disconnect-models"]').forEach((btn) => {
      if (typeof btn.setAttribute === 'function') btn.setAttribute('aria-expanded', 'false');
    });
  }
}

function requestDisconnectConfirm(button) {
  const targetId = (button && button.dataset && button.dataset.confirmTarget) || 'workspace-disconnect-confirm';
  const box = $(targetId);
  clearDisconnectConfirm();
  if (!box) return false;
  const directNote = state.data?.integrationMode === 'direct' ? '直连模式下的静态 hosts 也会保留。' : '';
  const text = document.createElement('p');
  text.className = 'inline-confirm__text';
  text.textContent = '确定移除 KimiWork + 接管的模型来源吗？这会从官方 Kimi 移除本产品同步的模型，保留登录、对话、其他模型来源以及本软件中的服务商配置。' + directNote + '之后可随时重新同步接管。';
  const actions = document.createElement('div');
  actions.className = 'inline-confirm__actions';
  const confirmBtn = document.createElement('button');
  confirmBtn.type = 'button';
  confirmBtn.className = 'btn btn--danger btn--sm';
  confirmBtn.dataset.disconnectConfirm = 'confirm';
  confirmBtn.textContent = '确认移除接管';
  confirmBtn.addEventListener('click', () => {
    const profileMode = button?.dataset?.profileMode;
    clearDisconnectConfirm();
    void runAction('disconnect-models', profileMode);
  });
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn--quiet btn--sm';
  cancelBtn.dataset.disconnectConfirm = 'cancel';
  cancelBtn.textContent = '取消';
  cancelBtn.addEventListener('click', () => clearDisconnectConfirm());
  actions.append(confirmBtn, cancelBtn);
  box.replaceChildren(text, actions);
  setHidden(box, false);
  if (button && typeof button.setAttribute === 'function') button.setAttribute('aria-expanded', 'true');
  const timer = window.setTimeout(() => clearDisconnectConfirm(), 10000);
  state.disconnectConfirm = { targetId, timer };
  if (typeof cancelBtn.focus === 'function') cancelBtn.focus({ preventScroll: true });
  return true;
}

function refreshServiceStatus() {
  if (!state.key) return;
  const epoch = state.stateReadEpoch;
  const key = state.key;
  request('/api/service').then(result => {
    if (!result?.service || epoch !== state.stateReadEpoch || key !== state.key) return;
    if (!state.data) {
      state.pendingService = result.service;
      return;
    }
    state.pendingService = null;
    state.data.service = { ...state.data.service, ...result.service, loginStartup: result.service.enabled ? 'enabled' : 'manual' };
    renderPreviewLifecycle(state.data);
  }).catch(() => {});
}

/* ---------- 更新：窗口聚焦时的静默过期复查 ---------- */

/* 后台守护进程每 2 小时自动检查一次；用户把窗口切回前台时，如果距上次成功
   检查已超过 30 分钟，就静默补查一次。60 秒内不重复发请求，避免频繁切焦点
   造成成串检查；检查失败不弹打扰式 Toast，设置页的手动检查仍保留明确反馈。 */
const FOCUS_UPDATE_MIN_INTERVAL_MS = 60_000;
const FOCUS_UPDATE_STALE_MS = 30 * 60 * 1000;
const FOCUS_UPDATE_BUSY_STATES = ['checking', 'downloading', 'verifying', 'downloaded', 'applying', 'rolling_back', 'waiting_idle'];

function focusUpdateDue(updates, now) {
  if (!updates || typeof updates !== 'object') return false;
  if (updates.autoCheck === false) return false;
  /* 'local' 是没有更新器的本地构建：后端对每次检查都回 503，
     每 60 秒聚焦就发一次只会制造失败日志。 */
  if (updates.state === 'local') return false;
  if (FOCUS_UPDATE_BUSY_STATES.includes(updates.state)) return false;
  if (now - Number(state.lastFocusUpdateCheckAt || 0) <= FOCUS_UPDATE_MIN_INTERVAL_MS) return false;
  const last = Date.parse(updates.lastCheckedAt || '');
  if (Number.isFinite(last) && (now - last) <= FOCUS_UPDATE_STALE_MS) return false;
  return true;
}

async function runFocusUpdateCheck() {
  try {
    const result = await request(API.updatesCheck, { method: 'POST', body: {} });
    if (result?.updates && state.data) {
      state.data.updates = result.updates;
      renderUpdates(state.data);
    }
    await loadState({ silent: true });
  } catch {
    /* 静默检查不打扰用户：网络或更新源失败只在设置页手动检查时提示。 */
  }
}

function maybeCheckUpdatesOnFocus({ now = Date.now() } = {}) {
  if (!state.key || !state.data?.updates) return false;
  if (!focusUpdateDue(state.data.updates, now)) return false;
  state.lastFocusUpdateCheckAt = now;
  void runFocusUpdateCheck();
  return true;
}

/* 托盘「前往更新」与总览「查看更新说明」共用的定位入口：切到设置 → 接入与更新，
   展开折叠区，只滚动 .main 并聚焦手动检查按钮，绝不调用 scrollIntoView。
   首次 #shell 揭开前布局还不能滚动：记下挂起标记，等 shell 显示后补做一次。 */
let pendingUpdatesPanelReveal = false;

function openUpdatesPanel() {
  setView('maintenance');
  if (typeof setSubtab === 'function') setSubtab('maintenance', 'updates');
  const revealed = typeof dom.shell === 'undefined' || !dom.shell || dom.shell.hidden !== true;
  if (!revealed) {
    pendingUpdatesPanelReveal = true;
    return;
  }
  const panel = $('update-panel');
  if (panel) {
    for (let node = panel; node; node = node.parentElement) {
      if (node.dataset && node.dataset.subtabGroup && node.dataset.subtabPane && typeof setSubtab === 'function') {
        setSubtab(node.dataset.subtabGroup, node.dataset.subtabPane);
      }
      if (node.tagName === 'DETAILS') node.open = true;
    }
    scrollMainToElement(panel);
  }
  const check = $('upd-check');
  if (check && typeof check.focus === 'function') check.focus({ preventScroll: true });
}

/* #shell 揭开后补做挂起的那次定位，保证托盘入口在冷启动时不丢。 */
function flushPendingUpdatesPanelReveal() {
  if (!pendingUpdatesPanelReveal) return;
  if (!dom.shell || dom.shell.hidden === true) return;
  pendingUpdatesPanelReveal = false;
  openUpdatesPanel();
}

function bindEvents() {
  conversationsPanel = mountConversationsPage();
  state.conversationsStateChanged = conversationsPanel.onState;
  if (state.view === 'conversations' && state.key) conversationsPanel.show();
  VIEWS.forEach(view => {
    const button = $('nav-' + view);
    if (button) button.addEventListener('click', () => setView(view));
  });
  document.querySelectorAll('[data-subtab-group][data-subtab]').forEach((button) => {
    button.addEventListener('click', () => {
      setSubtab(button.dataset.subtabGroup, button.dataset.subtab);
    });
    button.addEventListener('keydown', (event) => {
      const group = button.dataset.subtabGroup;
      const tabs = Array.from(document.querySelectorAll(`[data-subtab-group="${group}"][data-subtab]`));
      const index = tabs.indexOf(button);
      if (index < 0 || !tabs.length) return;
      let nextIndex = -1;
      if (event.key === 'ArrowRight') nextIndex = (index + 1) % tabs.length;
      else if (event.key === 'ArrowLeft') nextIndex = (index - 1 + tabs.length) % tabs.length;
      else if (event.key === 'Home') nextIndex = 0;
      else if (event.key === 'End') nextIndex = tabs.length - 1;
      if (nextIndex < 0) return;
      event.preventDefault();
      const target = tabs[nextIndex];
      setSubtab(group, target.dataset.subtab);
      if (typeof target.focus === 'function') target.focus();
    });
  });

  if (dom.restartBannerLink) dom.restartBannerLink.addEventListener('click', () => {
    const action = dom.restartBannerLink.dataset.bannerAction || 'restart-kimi';
    if (action === 'models') {
      setView('models');
      revealWorkspaceSection('restart-panel');
      return;
    }
    void runAction('restart-kimi');
  });
  $('budget-banner-link')?.addEventListener('click', () => {
    setView('usage');
    const tab = $('usage-tab-budget');
    tab?.click();
    tab?.focus();
  });
  $('overview-details')?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('native-panel');
  });
  dom.overviewSwitchMode?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('native-panel');
  });
  $('diagnostics-export')?.addEventListener('click', () => { void exportDiagnostics(); });
  $('compat-details-link')?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('compat-diagnostics-panel');
  });
  dom.overviewOpenModels?.addEventListener('click', () => setView('models'));
  dom.overviewOpenSubagents?.addEventListener('click', () => {
    setView('models');
    revealWorkspaceSection('sa-target-panel');
  });
  dom.overviewOpenUsage?.addEventListener('click', () => setView('usage'));
  dom.overviewIssueAction?.addEventListener('click', runOverviewIssueAction);
  dom.quickstartToggle?.addEventListener('click', toggleQuickstartGuide);
  dom.quickstartDismiss?.addEventListener('click', dismissQuickstartGuide);
  $('quickstart-step-1-action')?.addEventListener('click', () => {
    const hasModels = Array.isArray(state.data?.models) && state.data.models.length > 0;
    if (hasModels) {
      setView('models');
      revealWorkspaceSection('catalog-panel');
    } else {
      openOnboardWizard();
    }
  });
  $('quickstart-step-2-action')?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('native-panel');
  });
  $('quickstart-step-2-mode')?.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('native-panel');
  });
  $('quickstart-step-3-action')?.addEventListener('click', () => {
    setView('models');
    revealWorkspaceSection('sa-target-panel');
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
    /* 顶栏刷新是用户明确要求「现在就是最新的」：Kimi Code、已配置的可选额度与
       用量一起重读，否则停在用量页时点刷新仍会保留上一轮读数。 */
    try {
      const tasks = [loadState(), refreshOverviewToday({ force: true })];
      if (state.view === 'usage') tasks.push(usagePanel.reload());
      if (state.view === 'conversations') tasks.push(conversationsPanel.refresh());
      if (plusPanel && typeof plusPanel.refresh === 'function') tasks.push(plusPanel.refresh({ live: state.view === 'usage' }));
      await Promise.all(tasks);
    } finally {
      dom.refresh.disabled = false;
    }
  });

  if (dom.connRetry) dom.connRetry.addEventListener('click', async () => {
    dom.connRetry.disabled = true;
    const ok = await loadState();
    if (ok && !state.timer) startPolling();
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
      finally {
        if (state.data) renderPreviewLifecycle(state.data);
        else button.disabled = false;
      }
    });
  }
  const migrationTopLink = $('migration-top-banner-link');
  if (migrationTopLink) migrationTopLink.addEventListener('click', () => {
    setView('maintenance');
    revealWorkspaceSection('migration-banner');
  });
  const openUpdateSection = () => openUpdatesPanel();
  const updTopLink = $('update-top-banner-link');
  if (updTopLink) updTopLink.addEventListener('click', openUpdateSection);
  if (dom.updateBannerPrimary) dom.updateBannerPrimary.addEventListener('click', async () => {
    const action = dom.updateBannerPrimary.dataset.updateAction || 'download';
    dom.updateBannerPrimary.disabled = true;
    try { await updateAction(action, {}); }
    catch (error) { showToast(error.message || '更新操作未能启动，当前运行未受影响。', 'error'); }
    finally {
      if (state.data) renderUpdates(state.data);
      else dom.updateBannerPrimary.disabled = false;
    }
  });
  if (dom.updateBannerNotes) dom.updateBannerNotes.addEventListener('click', () => {
    if (typeof window.__kwpOpenUpdates === 'function') window.__kwpOpenUpdates();
    else openUpdatesPanel();
  });
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
    finally {
      if (state.data) renderUpdates(state.data);
      else updCheck.disabled = false;
    }
  });
  const updDownload = $('upd-download');
  if (updDownload) updDownload.addEventListener('click', async () => {
    updDownload.disabled = true;
    try { await updateAction('/api/updates/download', {}); }
    catch (error) { showToast(error.message || '未能启动更新下载。', 'error'); }
    finally {
      if (state.data) renderUpdates(state.data);
      else updDownload.disabled = false;
    }
  });
  const updCancel = $('upd-cancel');
  if (updCancel) updCancel.addEventListener('click', async () => {
    updCancel.disabled = true;
    try { await updateAction('/api/updates/cancel', {}, '已取消当前更新操作。'); }
    catch (error) { showToast(error.message || '取消更新未成功。', 'error'); }
    finally {
      if (state.data) renderUpdates(state.data);
      else updCancel.disabled = false;
    }
  });
  const updRollback = $('upd-rollback');
  if (updRollback) updRollback.addEventListener('click', async () => {
    updRollback.disabled = true;
    try { await updateAction('/api/updates/rollback', {}); }
    catch (error) { showToast(error.message || '未能启动版本回退。', 'error'); }
    finally {
      if (state.data) renderUpdates(state.data);
      else updRollback.disabled = false;
    }
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
        adoptState(result.state, { external: true });
        renderAll();
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

  if (dom.theme) dom.theme.addEventListener('change', () => applyTheme(dom.theme.value));

  /* 选择卡片只请求确认，确认后的保存回执使旧轮询结果失效。 */
  dom.integrationModeGroup?.addEventListener('change', (event) => {
    const target = event.target;
    if (target?.name === 'integration-mode') requestIntegrationModeConfirm(target.value);
  });
  const cancelModeChange = () => cancelIntegrationModeConfirm({ restoreFocus: true });
  const cancelModeOnEscape = (event) => {
    if (event.key !== 'Escape' || !state.integrationModeConfirm) return;
    event.preventDefault();
    event.stopPropagation();
    cancelModeChange();
  };
  dom.integrationModeGroup?.addEventListener('keydown', cancelModeOnEscape);
  dom.integrationModeConfirm?.addEventListener('keydown', cancelModeOnEscape);
  dom.integrationModeCancel?.addEventListener('click', cancelModeChange);
  dom.integrationModeSubmit?.addEventListener('click', () => {
    void confirmIntegrationMode({
      onSaved: (next) => {
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
  if (dom.saCapabilityNext) dom.saCapabilityNext.addEventListener('click', () => {
    setView('models');
    revealWorkspaceSection('catalog-panel');
  });

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
  if (dom.modelNew) dom.modelNew.addEventListener('click', () => {
    const named = (state.conns?.draft || []).filter((row) => row && !row.legacy);
    if (named.length) openEditor(null);
    else openOnboardWizard();
  });

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
      const target = ((state.data && state.data.models) || []).find((model) => model.id === id);
      const label = target ? (target.name || target.upstreamModelId || '该模型') : '该模型';
      const models = ((state.data && state.data.models) || []).filter((model) => model.id !== id);
      saveCatalogue(models, { close: false, message: '模型「' + label + '」已从目录删除并应用。' });
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
      mName: 'name', mContext: 'contextWindow', mOutput: 'maxOutputTokens',
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
  if (dom.connsList) {
    dom.connsList.addEventListener('click', (event) => {
      const node = event.target && event.target.closest ? event.target.closest('button') : null;
      if (!node) return;
      if (node.dataset.connectionCheck) {
        void runConnectionCheck(node.dataset.connectionCheck);
        return;
      }
      if (node.dataset.connectionModels) {
        toggleConnectionModelPicker(node.dataset.connectionModels);
        return;
      }
      if (node.dataset.connectionEdit) {
        const row = (state.conns.draft || []).find((entry) => entry.id === node.dataset.connectionEdit);
        if (row) openConnectionEditor(row);
        return;
      }
      if (node.dataset.connectionRemove) {
        requestConnectionDelete(node.dataset.connectionRemove);
        return;
      }
      if (node.dataset.connectionRemoveConfirm) {
        void confirmConnectionDelete(node.dataset.connectionRemoveConfirm);
        return;
      }
      if (node.dataset.connectionRemoveCancel) {
        cancelConnectionDelete();
        return;
      }
      if (node.dataset.connPickerReload) {
        reloadConnectionModelPicker(node.dataset.connPickerReload);
        return;
      }
      if (node.dataset.connPickerClose) {
        closeConnectionModelPicker();
        return;
      }
      if (node.dataset.connPickerSelectAll) {
        toggleConnectionPickerSelectAll();
        return;
      }
      if (node.dataset.connPickerSave) {
        void saveConnectionModelPicker();
      }
    });
    dom.connsList.addEventListener('input', (event) => {
      const target = event.target;
      if (target && target.dataset && 'connPickerQuery' in target.dataset) {
        setConnectionPickerQuery(target.value);
      }
    });
    dom.connsList.addEventListener('change', (event) => {
      const target = event.target;
      if (target && target.dataset && target.dataset.connPickerItem) {
        toggleConnectionPickerItem(target.dataset.connPickerItem, target.checked === true);
      }
    });
  }
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
  [dom.ceProtoAuto, dom.ceChat, dom.ceResponses, dom.ceAnthropic].forEach((field) => {
    if (!field) return;
    field.addEventListener('change', () => {
      if (!state.conns.editing) return;
      readConnectionEditor();
      renderConnections();
      renderConnectionEditor();
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
    /* 旧连接的上限不属于新连接：留着会让预算预设按错误的上限禁用或放行。 */
    editor.draft.contextLimit = '';
    editor.budgetNote = null;
    if (!editor.originalId) {
      /* 新模型的协议与 ID 跟随连接的当前草稿，包括尚未保存的协议修改。 */
      editor.draft.protocol = draftConnectionProtocol(editor.draft.connectionId);
      editor.draft.id = suggestedRouteId(editor.draft);
      if (!editor.draft.touched?.has('upstreamProtocol')) {
        editor.draft.upstreamProtocol = connectionDefaultUpstreamProtocol(editor.draft.connectionId);
      }
      applyInferredModelDefaults(editor);
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
    /* 换了上游模型，旧模型声明的上限就不再成立。 */
    editor.draft.contextLimit = '';
    editor.budgetNote = null;
    /* 已保存模型保持原 ID；新模型按连接协议重新生成内部 ID，并自动推断默认能力。 */
    if (!editor.originalId) {
      editor.draft.id = suggestedRouteId(editor.draft);
      applyInferredModelDefaults(editor);
    }
    renderEditor();
  });
  if (dom.mBudgetGroup) dom.mBudgetGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-budget]') : null;
    if (!button || button.disabled) return;
    applyBudgetPreset(button.dataset.budget);
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
      clearOnboardSaveErrors();
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
    clearOnboardSaveErrors();
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
  if (dom.onboardProtocolGroup) dom.onboardProtocolGroup.addEventListener('click', (event) => {
    const button = event.target && event.target.closest ? event.target.closest('[data-onboard-protocol]') : null;
    if (!button || button.disabled) return;
    setOnboardProtocol(button.dataset.onboardProtocol);
  });
  if (dom.onboardBatchContextApply) dom.onboardBatchContextApply.addEventListener('click', applyOnboardBatchContext);
  if (dom.onboardSave) dom.onboardSave.addEventListener('click', () => { void saveOnboard(); });
  if (dom.onboardRetryApply) dom.onboardRetryApply.addEventListener('click', () => { void applyConnections(); });
  if (dom.applyButton) dom.applyButton.addEventListener('click', () => { void applyConnections(); });

  document.querySelectorAll('[data-action]').forEach((button) => {
    button.addEventListener('click', () => {
      const action = button.dataset.action;
      if (action === 'disconnect-models') {
        requestDisconnectConfirm(button);
        return;
      }
      /* 总览主卡在没有模型或直连不受支持时引导去对应的页面，而不是发一次
         后端不认识的操作。 */
      if (action === 'open-models') {
        setView('models');
        revealWorkspaceSection('catalog-panel');
        return;
      }
      if (action === 'open-connection') {
        setView('maintenance');
        revealWorkspaceSection('native-panel');
        return;
      }
      void runAction(action, button.dataset.profileMode);
    });
  });

  document.addEventListener('compositionstart', () => { state.composing = true; });
  document.addEventListener('compositionend', () => { state.composing = false; });
  document.addEventListener('focusout', () => { state.composing = false; });
  window.addEventListener('blur', () => { state.composing = false; });
  window.addEventListener('online', () => {
    if (state.key && !editingBusy()) {
      loadState({ silent: true }).then((ok) => {
        if (ok && !state.timer) startPolling();
      });
    }
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
  state.connDeleteId = null;
  state.connDeletingId = null;
  state.connPicker = { connectionId: null, loading: false, saving: false, query: '', models: [], selectedIds: [], error: '', sourceNote: '', seq: 0, detectedProtocol: null, detectedUpstreamProtocol: '' };
  state.connUpstreamProtocols = {};
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
  state.onboard = { discovery: null, discoveryFor: null, seq: 0, busy: false, saving: false, touched: false, query: '', saveErrors: [], protocol: null, protocolMode: 'auto', upstreamProtocol: '', protocolTouched: false, probeResult: null };
  if (dom.onboardKey) dom.onboardKey.value = '';
  if (dom.onboardModelQuery) dom.onboardModelQuery.value = '';
  state.applyUnsupported = false;
  state.applyRefusal = null;
  state.diagnosticsExporting = false;
  state.diagnosticsExportError = '';
  state.diagnosticsExportPath = '';
  state.pendingService = null;
  state.composing = false;
  state.lastFocusUpdateCheckAt = 0;
  state.initialUpdateCheckDone = false;
  state.conversations = { list: null, loading: false, error: '', query: '', stale: false, pending: false,
    targets: {}, efforts: {}, selected: [], noConfirm: false, result: '', confirm: null, rowConfirm: null };
  conversationsPanel?.reset?.();
  clearDisconnectConfirm();
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
  refreshServiceStatus();
  loadState().then((ok) => {
    if (ok && typeof usagePanel !== 'undefined' && usagePanel && state.view === 'usage') usagePanel.show();
    if (ok || (state.key && dom.gate && dom.gate.hidden === true)) startPolling();
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
      if (state.editor && !state.editor.originalId) {
        applyInferredModelDefaults(state.editor);
        renderEditor();
      }
      if (state.data && state.view === 'models') renderGrowth(state.data, { view: 'models' });
    }
  } catch {
    state.providers = { presets: [] };
  }
}


function boot() {
  initTheme();
  initWindowChrome();
  const openAccountsTab = () => {
    setView('accounts');
  };
  window.__kwpOpenAccounts = openAccountsTab;
  window.__kwpOpenConversations = () => {
    setView('conversations');
    void conversationsPanel.refresh();
  };
  window.__kwpOpenUpdates = () => openUpdatesPanel();
  if (typeof globalThis.KimiWorkGrowth?.mountGrowth === 'function') {
    renderGrowth = globalThis.KimiWorkGrowth.mountGrowth({ request, getState: () => state.data, getProviders: () => state.providers,
      refresh: loadState, notify: showToast, runAction });
  }
  if (typeof globalThis.KimiWorkUsage?.mount === 'function') {
    usagePanel = globalThis.KimiWorkUsage.mount({ request, notify: showToast });
  }
  mountUsageRequests();
  if (typeof globalThis.KimiWorkPlus?.mount === 'function') {
    plusPanel = globalThis.KimiWorkPlus.mount({ request, notify: showToast, getState: () => state.data,
      setState: (data, opts) => adoptState(data, { external: true, ...opts }),
      onAccounts: openAccountsTab });
  }
  /* 会话密钥必须先就绪：停留在「用量」等视图时，挂载就会立刻发出第一笔请求，
     先挂视图再读密钥会让那次请求带着空密钥失败，界面上闪现「缺少面板会话密钥」。 */
  state.key = readKey();
  initSubtabs();
  initView();
  bindEvents();
  observeMasthead();
  state.actionButtons = Array.from(document.querySelectorAll('[data-action]'));

  /* 面板重启后粘贴新链接只改变 hash，浏览器不会重新执行脚本，
     因此在 hash 变化与页面重新可见时都重新取一次密钥。密钥已更换时
     adoptLocationKey 会自己重读状态；同一次可见事件不再并发补一次。 */
  window.addEventListener('hashchange', adoptLocationKey);
  window.addEventListener('focus', () => { maybeCheckUpdatesOnFocus(); });
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    maybeCheckUpdatesOnFocus();
    if (adoptLocationKey()) return;
    if (!state.key) return;
    if (!state.started) {
      loadState().then((ok) => {
        if (ok && !state.timer) startPolling();
      });
      return;
    }
    if (!editingBusy()) {
      loadState({ silent: true }).then((ok) => {
        if (ok && !state.timer) startPolling();
      });
    }
  });

  if (!state.key) {
    showGate('missing');
    return;
  }

  void loadProviders();
  refreshServiceStatus();
  showGate('checking');
  loadState().then((ok) => {
    if (ok || (state.key && dom.gate && dom.gate.hidden === true)) startPolling();
  });
}

boot();
// Wails queues ExecJS until the document announces that its hooks are ready.
postWailsMessage('wails:runtime:ready');

export {
  adoptState,
  clearDisconnectConfirm,
  clearOnboardSaveErrors,
  handleSaveFailure,
  loadState,
  maybeCheckUpdatesOnFocus,
  openUpdatesPanel,
  renderAll,
  runAction,
  setView,
  stopPolling,
};

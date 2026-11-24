import { $, API, dom, request, setHidden, setText, showToast, state, stripModelAliasPrefix } from './app-core.js';
import { savedIntegrationMode } from './app-overview.js';
import { loadState } from './app.js';

/* ---------- 可读模型名 ---------- */

/* Kimi 3.2.28 的内置模型别名是内部值；会话列表不能把 `k3d1-agent` 或
   `byok-kth-kimi/kimi-k3-kwp-<sha256>` 直接摆给用户。官方别名有固定译名，
   第三方模型优先用 /api/work/conversations 的 displayName，其次按兼容 ID
   反查目录里的显示名。 */
const OFFICIAL_MODEL_NAMES = {
  'k3-agent': '官方 · K3',
  'k3d1-agent': '官方 · K3.1 Pro',
  'k2d8-preview': '官方 · K2.8 Preview',
  'kimi-k3': '官方 · K3',
  'kimi-k3.1': '官方 · K3.1 Pro',
  'kimi-k2.8': '官方 · K2.8 Preview',
};

const KIMI_COMPATIBLE_ID = /^kimi-k3-kwp-[0-9a-f]{64}$/i;
const EFFORT_ORDER = ['off', 'low', 'medium', 'high', 'xhigh', 'max'];

const CONVERSATION_REASONS = {
  conversation_database_missing: '还没有 Kimi Work 会话库。请先在 Kimi Work 中创建会话。',
  conversation_database_schema_unsupported: '当前 Kimi Work 会话库格式暂不支持。',
  conversation_node_unavailable: '当前找不到支持读取会话库的 Kimi Node 运行时。',
  conversation_database_busy: 'Kimi 会话库正在使用，请稍后刷新。',
  conversation_profile_unsupported: '诊断隔离模式不可切换会话模型，请使用默认方式打开 Kimi。',
  conversation_rollback_failed: '会话绑定恢复尚未完成。请检查 Kimi 状态后刷新。',
  conversation_kimi_restart_required: '写入会话绑定需要先正常退出并重启 Kimi，请重试并允许自动重启。',
  conversation_kimi_stop_failed: '未能正常退出 Kimi，请先在官方窗口退出后重试。',
  invalid_conversation_target_model: '目标模型不在当前已同步的模型列表中，请刷新会话后重试。',
  invalid_conversation_thinking_level: '目标模型不支持所选思考档位，请换一个档位。',
  invalid_conversation_migration_target: '该模型没有可迁移的旧别名。',
  invalid_conversation_rebind: '会话改绑请求格式不被接受，请刷新后重试。',
  invalid_conversation_keys: '所选会话已变化，请刷新会话后重试。',
};

function conversationReasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  return CONVERSATION_REASONS[code] || '';
}

function sourceWord(sourceKind) {
  return sourceKind === 'kth' ? '网关' : '直连';
}

function displayNameOf(model) {
  if (!model) return '';
  return model.displayName || model.name || model.id || '';
}

function withSource(name, sourceKind, previousAlias) {
  const base = String(name || '').trim();
  if (!base) return '';
  if (base.startsWith('官方')) return base;
  return base + ' · ' + sourceWord(sourceKind) + (previousAlias ? '（旧别名）' : '');
}

function directoryNameOf(model) {
  if (!model) return '';
  const name = model.name && model.name !== model.id ? model.name : '';
  return name || model.id || '';
}

function humanizeConversationModel(alias, knownModels, stateModels) {
  if (typeof alias !== 'string' || !alias.trim()) return 'Kimi 官方模型';
  const raw = alias.trim();
  if (OFFICIAL_MODEL_NAMES[raw]) return OFFICIAL_MODEL_NAMES[raw];
  const known = Array.isArray(knownModels) ? knownModels : [];
  const current = known.find((model) => model && model.alias === raw);
  if (current && displayNameOf(current)) return withSource(displayNameOf(current), current.sourceKind, false);
  const previous = known.find((model) => model && model.previousAlias === raw);
  if (previous && displayNameOf(previous)) return withSource(displayNameOf(previous), previous.sourceKind, true);
  const stripped = stripModelAliasPrefix(raw);
  if (typeof stripped === 'string' && stripped && stripped !== raw) {
    const catalogue = Array.isArray(stateModels) ? stateModels : [];
    const match = catalogue.find((model) => model && (model.id === stripped || model.alias === stripped
      || model.upstreamModelId === stripped));
    if (match) {
      const gateway = savedIntegrationMode(state.data) !== 'direct';
      return withSource(directoryNameOf(match), gateway ? 'kth' : 'custom', false);
    }
    if (KIMI_COMPATIBLE_ID.test(stripped)) return '网关模型（目录未列出）';
    return stripped;
  }
  if (KIMI_COMPATIBLE_ID.test(raw)) return '网关模型（目录未列出）';
  return raw;
}

/* ---------- 展示辅助 ---------- */

function relativeTime(value, now = Date.now()) {
  const ms = Number(value);
  if (!Number.isFinite(ms) || ms <= 0) return '时间未知';
  const delta = Math.max(0, now - ms);
  if (delta < 60_000) return '刚刚';
  if (delta < 3_600_000) return Math.floor(delta / 60_000) + ' 分钟前';
  if (delta < 86_400_000) return Math.floor(delta / 3_600_000) + ' 小时前';
  if (delta < 7 * 86_400_000) return Math.floor(delta / 86_400_000) + ' 天前';
  const date = new Date(ms);
  if (Number.isNaN(date.getTime())) return '时间未知';
  return date.toLocaleDateString('zh-CN');
}

function workspaceTail(path) {
  if (typeof path !== 'string' || !path.trim()) return '未记录工作目录';
  const parts = path.replace(/\\/g, '/').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : path;
}

function conversationListStatus(list) {
  if (!list) return '正在读取 Kimi Work 会话…';
  if (list.reason) return conversationReasonText(list.reason) || '无法读取 Kimi Work 会话库，请刷新后重试。';
  if (list.application === 'unknown') return '无法确认 Kimi 的运行状态，请刷新后再切换。';
  if (!list.conversations.length) return '当前没有可切换的 Kimi Work 会话。';
  if (!list.models.length) return '当前账号没有可用的第三方模型。请先到「模型与服务商」保存并同步模型。';
  if (list.kimiRunning) return 'Kimi 正在运行：切换模型会先确认，再自动退出并重开 Kimi（约 2–3 秒）写入会话绑定。';
  return 'Kimi 当前未运行：切换会直接写入绑定，打开 Kimi 后继续原对话即可。';
}

function effortChoices(model) {
  if (!model) return { values: [], labels: [], fallback: '' };
  const supported = Array.isArray(model.supportEfforts) ? model.supportEfforts : [];
  const ordered = [
    ...EFFORT_ORDER.filter((level) => supported.includes(level)),
    ...supported.filter((level) => !EFFORT_ORDER.includes(level)).sort(),
  ];
  const values = [];
  const labels = [];
  if (model.defaultEffort) {
    values.push('');
    labels.push('模型默认（' + model.defaultEffort + '）');
  } else if (!ordered.length) {
    values.push('');
    labels.push('自动');
  }
  for (const level of ordered) {
    values.push(level);
    labels.push(level === model.defaultEffort ? level + '（默认）' : level);
  }
  return { values, labels, fallback: defaultEffortFor(model, ordered) };
}

function defaultEffortFor(model, ordered) {
  if (!model) return '';
  if (model.defaultEffort) return '';
  const list = ordered || (Array.isArray(model.supportEfforts) ? model.supportEfforts : []);
  return list.find((level) => level !== 'off') || (list.length ? list[0] : '');
}

/* ---------- 页面 ---------- */

function mountConversationsPage() {
  const ui = {
    guide: dom.convGuide,
    guideText: dom.convGuideText,
    refresh: dom.convRefresh,
    status: dom.convStatus,
    result: dom.convResult,
    query: dom.convQ,
    queryClear: $('conv-q-clear'),
    count: dom.convCount,
    list: dom.convList,
    empty: dom.convEmpty,
    emptyNote: dom.convEmptyNote,
    migration: dom.convMigrationTip,
    migrationText: dom.convMigrationText,
    migrateAll: dom.convMigrateAll,
    batchModel: dom.convBatchModel,
    batchEffort: dom.convBatchEffort,
    batchGo: dom.convBatchGo,
    batchMigrate: dom.convBatchMigrate,
    batchNote: dom.convBatchNote,
    confirm: dom.convConfirm,
    confirmText: dom.convConfirmText,
    confirmSubmit: dom.convConfirmSubmit,
    confirmCancel: dom.convConfirmCancel,
  };

  let lastSignature = '';

  function setStatus(text) {
    setText(ui.status, text);
  }

  function setResult(text) {
    state.conversations.result = typeof text === 'string' ? text : '';
    setText(ui.result, state.conversations.result);
    setHidden(ui.result, !state.conversations.result);
  }

  function currentList() {
    return state.conversations.list;
  }

  function modelsOf(list) {
    return list && Array.isArray(list.models) ? list.models : [];
  }

  function targetModelFor(key) {
    const list = currentList();
    const models = modelsOf(list);
    const alias = state.conversations.targets[key] || (models[0] ? models[0].alias : '');
    return models.find((model) => model.alias === alias) || null;
  }

  function targetOptions() {
    const models = modelsOf(currentList());
    return models.map((model) => ({
      value: model.alias,
      label: humanizeConversationModel(model.alias, models, state.data?.models),
    }));
  }

  function fillSelect(select, entries, current) {
    if (!select) return;
    const signature = JSON.stringify(entries) + '#' + (current || '');
    if (select.dataset && select.dataset.signature === signature) {
      select.value = current || '';
      return;
    }
    if (select.dataset) select.dataset.signature = signature;
    const options = entries.map((entry) => {
      const node = document.createElement('option');
      node.value = entry.value;
      node.textContent = entry.label;
      return node;
    });
    select.replaceChildren(...options);
    select.value = current || '';
  }

  function filteredConversations() {
    const list = currentList();
    if (!list || !Array.isArray(list.conversations)) return [];
    const needle = String(state.conversations.query || '').trim().toLowerCase();
    if (!needle) return list.conversations;
    return list.conversations.filter((row) => {
      const title = typeof row.title === 'string' ? row.title.toLowerCase() : '';
      const workspace = typeof row.workspacePath === 'string' ? row.workspacePath.toLowerCase() : '';
      return title.includes(needle) || workspace.includes(needle);
    });
  }

  function selectedKeys() {
    return new Set(state.conversations.selected);
  }

  function renderGuide() {
    const direct = savedIntegrationMode(state.data) === 'direct';
    if (ui.guide) ui.guide.dataset.mode = direct ? 'direct' : 'gateway';
    setText(ui.guideText, direct
      ? '当前为零网关直连模式：官方 Kimi 限制跨服务商切换模型会新开对话。如需保留上下文换模型，请在下方直接为对应会话选择新模型（将自动退出并重开 Kimi，约 2–3 秒）。'
      : '当前为本机网关模式：K3、K3.1 Pro 与所有已启用模型可直接在 Kimi 聊天窗口的模型下拉框自由切换，无需在此操作。下方列表仅用于切换最初由 K2.8 Preview 创建的对话（也可在 Kimi 内先点 K3.1 Pro 再点目标模型免重启切换），或一键迁移旧版历史会话。');
    const migrating = modelsOf(currentList()).filter((model) => model && model.previousAlias);
    setHidden(ui.migration, migrating.length === 0);
    if (migrating.length) {
      setText(ui.migrationText, '发现旧版模型别名会话，可一键迁移至新兼容别名。');
      if (ui.migrateAll) ui.migrateAll.disabled = state.conversations.pending;
    }
  }

  function optionEntries(values, labels) {
    return values.map((value, index) => ({ value, label: labels[index] }));
  }

  function buildEffortSelect(model, current) {
    const select = document.createElement('select');
    select.setAttribute('aria-label', '思考档位');
    select.dataset.convEffort = 'true';
    const choices = effortChoices(model);
    fillSelect(select, optionEntries(choices.values, choices.labels), current === undefined ? choices.fallback : current);
    select.disabled = !model || state.conversations.pending;
    return select;
  }

  function hideConfirm() {
    state.conversations.confirm = null;
    setHidden(ui.confirm, true);
  }

  function showPageConfirm(text, confirmState) {
    state.conversations.confirm = confirmState;
    setText(ui.confirmText, text);
    setHidden(ui.confirm, false);
    if (ui.confirmSubmit && ui.confirmSubmit.focus) ui.confirmSubmit.focus({ preventScroll: true });
  }

  function rowConfirmHost(row) {
    return row.querySelector ? row.querySelector('[data-conv-confirm]') : null;
  }

  function showRowConfirm(row, conversation, model, thinkingLevel) {
    const host = rowConfirmHost(row);
    if (!host) return;
    host.replaceChildren();
    const text = document.createElement('p');
    text.className = 'inline-confirm__text';
    text.textContent = '将《' + (conversation.title || '未命名会话') + '》切换到「' + displayNameOf(model)
      + '」，并自动关闭重开 Kimi Work（约 2–3 秒）以写入会话绑定。请确认当前没有正在生成的回复。';
    const actions = document.createElement('div');
    actions.className = 'inline-confirm__actions';
    const remember = document.createElement('label');
    remember.className = 'check';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.dataset.convNoconfirm = 'true';
    const rememberText = document.createElement('span');
    rememberText.textContent = '本次面板会话不再重复确认';
    remember.append(checkbox, rememberText);
    const submit = document.createElement('button');
    submit.type = 'button';
    submit.className = 'btn btn--primary btn--sm';
    submit.dataset.convConfirmSubmit = conversation.conversationKey;
    submit.textContent = '确认切换并重启 Kimi';
    const cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'btn btn--quiet btn--sm';
    cancel.dataset.convConfirmCancel = conversation.conversationKey;
    cancel.textContent = '取消';
    submit.addEventListener('click', () => {
      if (checkbox.checked) state.conversations.noConfirm = true;
      void submitSwitch([conversation.conversationKey], model, thinkingLevel);
    });
    cancel.addEventListener('click', () => { host.hidden = true; });
    actions.append(remember, submit, cancel);
    host.append(text, actions);
    host.hidden = false;
    if (submit.focus) submit.focus({ preventScroll: true });
  }

  function renderRow(conversation, { recent = false, models = [] } = {}) {
    const key = conversation.conversationKey;
    const row = document.createElement('article');
    row.className = 'conv-row' + (recent ? ' conv-row--recent' : '');
    row.dataset.conversationKey = key;

    const checkLabel = document.createElement('label');
    checkLabel.className = 'check conv-select';
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.dataset.convSelect = key;
    checkbox.checked = selectedKeys().has(key);
    checkbox.setAttribute('aria-label', '选择会话 ' + (conversation.title || ''));
    checkbox.addEventListener('change', () => {
      const selected = selectedKeys();
      if (checkbox.checked) selected.add(key);
      else selected.delete(key);
      state.conversations.selected = Array.from(selected);
      syncBatchState();
    });
    checkLabel.append(checkbox);

    const main = document.createElement('div');
    main.className = 'conv-main';
    const titleRow = document.createElement('div');
    titleRow.className = 'conv-title-row';
    const title = document.createElement('strong');
    title.className = 'conv-title';
    title.textContent = conversation.title || '未命名会话';
    titleRow.append(title);
    if (recent) {
      const tag = document.createElement('span');
      tag.className = 'tag tag--saved';
      tag.textContent = '最近活跃会话';
      titleRow.append(tag);
    }
    const meta = document.createElement('p');
    meta.className = 'conv-meta';
    const workspace = document.createElement('span');
    workspace.className = 'mono conv-workspace';
    workspace.textContent = workspaceTail(conversation.workspacePath);
    workspace.title = conversation.workspacePath || '';
    const time = document.createElement('time');
    time.textContent = relativeTime(conversation.updatedAtMs);
    if (Number.isFinite(Number(conversation.updatedAtMs)) && Number(conversation.updatedAtMs) > 0) {
      const date = new Date(Number(conversation.updatedAtMs));
      if (!Number.isNaN(date.getTime())) time.dateTime = date.toISOString();
    }
    const separator = document.createElement('span');
    separator.className = 'conv-sep';
    separator.textContent = ' · ';
    meta.append(workspace, separator, time);
    const modelLine = document.createElement('p');
    modelLine.className = 'conv-model';
    const modelCaption = document.createElement('span');
    modelCaption.className = 'conv-model__caption';
    modelCaption.textContent = '当前模型：';
    modelLine.append(modelCaption);
    const badge = document.createElement('span');
    badge.className = 'tag conv-badge';
    badge.dataset.convCurrent = key;
    badge.textContent = humanizeConversationModel(conversation.modelAlias, models, state.data?.models);
    modelLine.append(badge);
    main.append(titleRow, meta, modelLine);

    const actions = document.createElement('div');
    actions.className = 'conv-actions';
    const targetLabel = document.createElement('label');
    targetLabel.className = 'field field--inline conv-target';
    const targetCaption = document.createElement('span');
    targetCaption.className = 'field__label';
    targetCaption.textContent = '目标模型';
    const target = document.createElement('select');
    target.setAttribute('aria-label', '目标模型');
    target.dataset.convTarget = key;
    const selectedAlias = state.conversations.targets[key] || (models[0] ? models[0].alias : '');
    fillSelect(target, targetOptions(), selectedAlias);
    target.disabled = models.length === 0 || state.conversations.pending;
    targetLabel.append(targetCaption, target);

    const effortLabel = document.createElement('label');
    effortLabel.className = 'field field--inline conv-effort';
    const effortCaption = document.createElement('span');
    effortCaption.className = 'field__label';
    effortCaption.textContent = '思考档位';
    const effort = buildEffortSelect(models.find((model) => model.alias === selectedAlias) || null,
      state.conversations.efforts[key]);
    effort.dataset.convEffortKey = key;
    effortLabel.append(effortCaption, effort);

    target.addEventListener('change', () => {
      state.conversations.targets[key] = target.value;
      delete state.conversations.efforts[key];
      const model = models.find((entry) => entry.alias === target.value) || null;
      const choices = effortChoices(model);
      fillSelect(effort, optionEntries(choices.values, choices.labels), choices.fallback);
      effort.disabled = !model || state.conversations.pending;
    });
    effort.addEventListener('change', () => {
      state.conversations.efforts[key] = effort.value;
    });

    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'btn btn--primary btn--sm conv-switch';
    button.dataset.convSwitch = key;
    button.textContent = '切换模型';
    button.disabled = models.length === 0 || state.conversations.pending;
    button.addEventListener('click', () => {
      const model = targetModelFor(key);
      if (!model) {
        showToast('请先选择目标模型。', 'error');
        return;
      }
      if (currentList()?.kimiRunning === true && !state.conversations.noConfirm) {
        showRowConfirm(row, conversation, model, effort.value);
        return;
      }
      void submitSwitch([key], model, effort.value);
    });
    actions.append(targetLabel, effortLabel, button);

    const confirm = document.createElement('div');
    confirm.className = 'inline-confirm conv-row-confirm';
    confirm.dataset.convConfirm = key;
    confirm.hidden = true;

    row.append(checkLabel, main, actions, confirm);
    return row;
  }

  function renderList() {
    if (!ui.list) return;
    const list = currentList();
    const rows = filteredConversations();
    const recentKey = list && Array.isArray(list.conversations) && list.conversations.length
      ? list.conversations[0].conversationKey : '';
    const models = modelsOf(list);
    const nodes = rows.map((conversation) => renderRow(conversation, {
      recent: conversation.conversationKey === recentKey,
      models,
    }));
    ui.list.replaceChildren(...nodes);
    const total = list && Array.isArray(list.conversations) ? list.conversations.length : 0;
    setText(ui.count, list ? '显示 ' + rows.length + ' / 共 ' + total + ' 个会话' : '—');
    const empty = Boolean(list) && (rows.length === 0);
    setHidden(ui.empty, !empty);
    if (empty && ui.emptyNote) {
      setText(ui.emptyNote, total ? '没有匹配的会话，换一个关键词试试。' : '还没有可切换的会话，请先在 Kimi Work 中开始一段对话。');
    }
  }

  function syncBatchState() {
    const list = currentList();
    const models = modelsOf(list);
    const selected = selectedKeys();
    if (ui.batchGo) {
      ui.batchGo.disabled = !list || !selected.size || !models.length || state.conversations.pending;
      setText(ui.batchGo, state.conversations.pending ? '正在切换…' : '切换所选会话');
    }
    const migrating = models.some((model) => model && model.previousAlias);
    if (ui.batchMigrate) {
      ui.batchMigrate.disabled = !list || !migrating || state.conversations.pending;
    }
    if (ui.batchNote) {
      setText(ui.batchNote, !models.length
        ? '当前没有可用的第三方模型；请先在「模型与服务商」保存并同步。'
        : selected.size
          ? '已选择 ' + selected.size + ' 个会话。'
          : '勾选上方会话后，可为它们统一选择目标模型与思考档位；旧别名迁移会把使用旧版兼容别名的历史会话更新为当前别名。');
    }
  }

  function renderBatch() {
    const list = currentList();
    const models = modelsOf(list);
    const entries = models.map((model) => ({
      value: model.alias,
      label: humanizeConversationModel(model.alias, models, state.data?.models),
    }));
    const current = state.conversations.batchModel || (models[0] ? models[0].alias : '');
    fillSelect(ui.batchModel, entries, current);
    if (ui.batchModel) ui.batchModel.disabled = !models.length || state.conversations.pending;
    const model = models.find((entry) => entry.alias === ui.batchModel?.value) || models[0] || null;
    const choices = effortChoices(model);
    const effortCurrent = state.conversations.batchEffort === undefined ? choices.fallback : state.conversations.batchEffort;
    fillSelect(ui.batchEffort, optionEntries(choices.values, choices.labels), effortCurrent);
    if (ui.batchEffort) ui.batchEffort.disabled = !model || state.conversations.pending;
    state.conversations.batchModel = ui.batchModel?.value || current;
    state.conversations.batchEffort = ui.batchEffort?.value ?? effortCurrent;
    syncBatchState();
  }

  function render() {
    renderGuide();
    renderList();
    renderBatch();
  }

  async function load({ preserveResult = false } = {}) {
    const c = state.conversations;
    if (c.loading || !state.key) return;
    const key = state.key;
    c.loading = true;
    c.error = '';
    if (!preserveResult) setResult('');
    setStatus('正在读取 Kimi Work 会话…');
    renderList();
    try {
      const next = await request(API.workConversations);
      if (state.key !== key) return;
      if (!Array.isArray(next?.conversations) || !Array.isArray(next?.models)) {
        throw new Error('无法读取 Kimi Work 会话。');
      }
      c.list = next;
      c.stale = false;
      c.targets = {};
      c.efforts = {};
      c.selected = [];
      c.confirm = null;
      for (const conversation of next.conversations) {
        if (next.models.length) c.targets[conversation.conversationKey] = next.models[0].alias;
      }
      setStatus(conversationListStatus(next));
    } catch (error) {
      if (state.key !== key) return;
      c.list = null;
      c.error = error?.message || '无法读取 Kimi Work 会话，请刷新后重试。';
      setStatus(c.error);
    } finally {
      c.loading = false;
      hideConfirm();
      render();
    }
  }

  async function submitSwitch(keys, model, thinkingLevel, { migrate = false } = {}) {
    const c = state.conversations;
    const list = currentList();
    if (!list || c.pending || !model) return;
    const kimiRunning = list.kimiRunning === true;
    const payload = migrate
      ? { targetModelAlias: model.alias, thinkingLevel, restartKimi: kimiRunning, migratePreviousAlias: true }
      : { conversationKeys: keys, targetModelAlias: model.alias, thinkingLevel, restartKimi: kimiRunning };
    c.pending = true;
    c.confirm = null;
    hideConfirm();
    setResult('');
    setStatus(kimiRunning ? '正在退出 Kimi、写入会话绑定并重新打开…' : '正在写入会话绑定…');
    render();
    try {
      const reply = await request(API.rebindConversations, { method: 'POST', body: payload });
      const count = Array.isArray(reply?.conversationKeys) ? reply.conversationKeys.length : keys.length;
      const parts = [];
      if (migrate) {
        parts.push(count ? '已迁移 ' + count + ' 个旧别名会话。' : '没有找到使用旧别名的会话。');
      } else {
        parts.push('已将 ' + count + ' 个会话切换到「' + displayNameOf(model) + '」。');
      }
      parts.push(reply?.kimiRestarted
        ? 'Kimi 已自动重启，打开后即可继续原对话。'
        : kimiRunning
          ? 'Kimi 未能自动重启，请手动打开 Kimi 后继续。'
          : '打开 Kimi 后可继续原对话。');
      if (reply?.directAnthropicHistoryWarning) {
        parts.push('提示：直连 Anthropic 可能因历史中的官方思考块拒绝继续，如遇报错请新建对话。');
      }
      if (Array.isArray(reply?.warnings)) {
        for (const warning of reply.warnings) {
          if (typeof warning === 'string' && warning) parts.push(warning);
        }
      }
      if (reply?.hasMore) parts.push('还有旧别名会话待迁移，可再次点「一键迁移旧别名会话」。');
      if (reply?.restartReason) parts.push('重启提示：' + reply.restartReason);
      setResult(parts.join(''));
      showToast(parts[0], 'ok');
    } catch (error) {
      const detail = conversationReasonText(error?.code) || error?.message || '切换未能完成，请刷新会话后重试。';
      setResult('切换未完成：' + detail);
      showToast('切换未完成：' + detail, 'error');
    } finally {
      c.pending = false;
      if (!state.key) return;
      try { await loadState({ silent: true }); } catch {}
      await load({ preserveResult: true });
      setStatus(conversationListStatus(currentList()));
    }
  }

  function requestBatch(model, thinkingLevel, keys, { migrate = false } = {}) {
    const list = currentList();
    if (!list || !model) return;
    if (!migrate && !keys.length) {
      showToast('请先勾选要切换的会话。', 'error');
      return;
    }
    if (list.kimiRunning === true && !state.conversations.noConfirm) {
      const label = migrate
        ? '将把旧别名会话迁移到「' + displayNameOf(model) + '」'
        : '将把 ' + keys.length + ' 个会话切换到「' + displayNameOf(model) + '」';
      showPageConfirm(label + '，并自动关闭重开 Kimi Work（约 2–3 秒）以写入会话绑定。请确认当前没有正在生成的回复。',
        { keys, model, thinkingLevel, migrate });
      return;
    }
    void submitSwitch(keys, model, thinkingLevel, { migrate });
  }

  if (ui.refresh) ui.refresh.addEventListener('click', () => { void load(); });
  if (ui.query) ui.query.addEventListener('input', () => {
    state.conversations.query = ui.query.value;
    setHidden(ui.queryClear, !state.conversations.query);
    renderList();
  });
  if (ui.query) ui.query.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && state.conversations.query) {
      event.stopPropagation();
      state.conversations.query = '';
      ui.query.value = '';
      setHidden(ui.queryClear, true);
      renderList();
    }
  });
  if (ui.queryClear) ui.queryClear.addEventListener('click', () => {
    state.conversations.query = '';
    if (ui.query) ui.query.value = '';
    setHidden(ui.queryClear, true);
    renderList();
    ui.query?.focus?.({ preventScroll: true });
  });
  if (ui.batchModel) ui.batchModel.addEventListener('change', () => {
    state.conversations.batchModel = ui.batchModel.value;
    state.conversations.batchEffort = undefined;
    renderBatch();
  });
  if (ui.batchEffort) ui.batchEffort.addEventListener('change', () => {
    state.conversations.batchEffort = ui.batchEffort.value;
  });
  if (ui.batchGo) ui.batchGo.addEventListener('click', () => {
    const list = currentList();
    const models = modelsOf(list);
    const model = models.find((entry) => entry.alias === ui.batchModel?.value) || null;
    requestBatch(model, ui.batchEffort?.value ?? '', state.conversations.selected);
  });
  if (ui.batchMigrate) ui.batchMigrate.addEventListener('click', () => {
    const list = currentList();
    const model = modelsOf(list).find((entry) => entry && entry.previousAlias);
    if (!model) {
      showToast('当前没有可迁移的旧别名模型。', 'warn');
      return;
    }
    requestBatch(model, defaultEffortFor(model), [], { migrate: true });
  });
  if (ui.migrateAll) ui.migrateAll.addEventListener('click', () => {
    const model = modelsOf(currentList()).find((entry) => entry && entry.previousAlias);
    if (!model) return;
    requestBatch(model, defaultEffortFor(model), [], { migrate: true });
  });
  if (ui.confirmSubmit) ui.confirmSubmit.addEventListener('click', () => {
    const pending = state.conversations.confirm;
    if (!pending) return;
    hideConfirm();
    void submitSwitch(pending.keys, pending.model, pending.thinkingLevel, { migrate: pending.migrate });
  });
  if (ui.confirmCancel) ui.confirmCancel.addEventListener('click', () => hideConfirm());

  function onState(next) {
    const signature = JSON.stringify([
      next?.revision, next?.accounts?.currentId, next?.accounts?.state, next?.profile?.mode,
      next?.kimiProcess?.running, next?.kimiProcess?.state, next?.integrationMode,
      next?.integration?.state, next?.integration?.imported,
    ]);
    if (signature === lastSignature) return;
    const changed = lastSignature !== '';
    lastSignature = signature;
    const list = currentList();
    if (list) list.kimiRunning = next?.kimiProcess?.running === true;
    renderGuide();
    if (changed && state.view === 'conversations' && !state.conversations.loading && !state.conversations.pending) {
      setStatus('账号、接入配置或 Kimi 状态已变化，正在刷新会话…');
      void load({ preserveResult: true });
    }
  }

  return {
    show() {
      renderGuide();
      if (!currentList() && !state.conversations.loading) void load();
      else syncBatchState();
    },
    refresh: () => load(),
    onState,
    reset() {
      lastSignature = '';
    },
  };
}

export {
  conversationListStatus,
  defaultEffortFor,
  humanizeConversationModel,
  mountConversationsPage,
  relativeTime,
  workspaceTail,
};

const $ = id => document.getElementById(id);
const text = (id, value) => { const node = $(id); if (node) node.textContent = value; };
const hidden = (id, value) => { const node = $(id); if (node) node.hidden = value; };
const showError = (id, error) => { const node = $(id); if (!node) return; node.hidden = !error; node.textContent = error || ''; };
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const escapeMap = {
  translation_connection_responses_only: '这个连接只支持 Responses，不能作为 Chat 翻译端点。',
  translation_connection_anthropic_only: '这个连接使用 Anthropic Messages，不能作为 Chat 翻译端点。',
  translation_connection_chat_undeclared: '该连接尚未声明 Chat Completions 支持，不能据此生成翻译端点。',
  translation_connection_key_missing: '该连接的 API Key 尚未就绪，请先在模型页应用该连接后再复制。',
  translation_connection_endpoint_not_base: '该连接地址已经是推理端点，不是可追加 Chat 路径的基础地址。',
  translation_connection_unavailable: '请选择一个已生效、有上游连接的模型。',
  translation_invalid_config: '翻译配置无效，请检查目标语言、接口地址和 API Key。',
  invalid_translation_config: '翻译配置无效，请检查目标语言、接口地址和 API Key。',
  translation_endpoint_required: '请填写自定义翻译接口地址后再保存。',
  invalid_translation_endpoint: '自定义翻译接口地址格式无效，请使用完整的 HTTPS 地址。',
  translation_preview_failed: '试译未完成，原文保持不变。请检查翻译服务与目标语言。',
  translation_timeout: '翻译服务超时，原文保持不变。',
  translation_http_error: '翻译服务返回错误，请检查地址、Key 和模型。',
  native_mcp_timeout: '原生 MCP 操作超时，未自动重试。',
};

/* 翻译运行状态来自内核，state 与 reason 都是英文内部值；面向用户只说结论，
   原始码留在 title 里供排查。 */
const translationStateWords = { inactive: '未启用', active: '已生效', attaching: '正在接入页面', failed: '接入失败' };
const translationStateReasons = {
  translation_disabled: '翻译已在设置里关闭，Kimi 页面不会被注入。',
  no_managed_child: '重启一次 Kimi 即可启用页面翻译。',
  translation_waiting_view: '已准备好，等待 Work 页面出现后接入。',
};
const mcpTransportWords = {
  stdio: '本地命令（stdio）',
  http: '远程 HTTP',
  sse: '远程 SSE',
};
const mcpResultWords = {
  added: '已添加',
  updated: '已更新',
  removed: '已移除',
  passed: '连接成功',
  ok: '已完成',
  failed: '未通过',
  error: '操作失败',
  unavailable: '接口不可用',
  unsupported: '暂不支持',
};

function errorText(error) {
  return escapeMap[error?.code] || error?.message || '操作未完成，请检查当前状态。';
}

function relocate(id, parent, beforeId) {
  const node = $(id);
  if (!node || !parent) return;
  let anchor = beforeId ? $(beforeId) : null;
  /* 按钮藏在说明区里；插到父级视图中必须对准它在视图里的直属祖先。 */
  while (anchor && anchor.parentNode && anchor.parentNode !== parent) anchor = anchor.parentNode;
  if (anchor && anchor.parentNode === parent) parent.insertBefore(node, anchor);
  else parent.append(node);
}

function mountLegacy() {
  const maintenance = $('view-maintenance');
  /* 新布局下接入与更新已在 settings-pane-connection 中；仅兼容旧版无分栏 DOM。
     扁平化后的页面没有 #ob-advanced-toggle 锚点，此时 relocate 会退化成追加到
     末尾，把面板挤到诊断区之后；没有旧锚点就不该走这段兼容。 */
  if (!$('settings-pane-connection') && $('ob-advanced-toggle')) {
    relocate('native-panel', maintenance, 'ob-advanced-toggle');
    relocate('update-panel', maintenance, 'ob-advanced-toggle');
  }
  const diagnostics = $('sa-diagnostics-body');
  /* 挂载可能被重复调用；诊断区里只留一份契约说明。 */
  if (!diagnostics || $('role-contract')) return;
  const roles = document.createElement('details');
  roles.className = 'growth-disclosure';
  roles.id = 'role-contract';
  const summaryRole = document.createElement('summary');
  summaryRole.textContent = '角色默认模型说明';
  const body = document.createElement('div');
  body.className = 'growth-disclosure__body';
  body.innerHTML = '<p>当前 Kimi Work 内核中，plan / coder / explore 等子代理角色不提供单独的模型硬锁字段；上方「按角色分配」保存时会自动生成标准 <code>[secondary_model]</code> 模型池并写入对应角色用途说明，由主模型按角色选用，已有会话继续保留原模型。</p>';
  roles.append(summaryRole, body);
  diagnostics.append(roles);
}

function normalizeTranslationDraft(config) {
  return {
    enabled: Boolean(config?.enabled),
    provider: config?.provider || 'bing',
    targetLanguage: (config?.targetLanguage || '').trim(),
    view: config?.view === 'original' ? 'original' : 'translated',
    customEndpoint: (config?.customEndpoint || '').trim(),
    customModel: (config?.customModel || '').trim(),
  };
}

function fieldFromDraft(draft) {
  if (!$('tr-enabled')) return;
  const normalized = normalizeTranslationDraft(draft);
  $('tr-enabled').checked = normalized.enabled;
  if ($('tr-provider')) $('tr-provider').value = normalized.provider;
  if ($('tr-language')) $('tr-language').value = normalized.targetLanguage;
  if ($('tr-endpoint')) $('tr-endpoint').value = normalized.customEndpoint;
  if ($('tr-model')) $('tr-model').value = normalized.customModel;
  const viewTranslated = $('tr-view-translated');
  const viewOriginal = $('tr-view-original');
  if (viewTranslated) viewTranslated.checked = normalized.view !== 'original';
  if (viewOriginal) viewOriginal.checked = normalized.view === 'original';
  const customDetails = $('tr-custom-details');
  if (customDetails) {
    const showCustom = normalized.enabled && normalized.provider === 'custom';
    customDetails.hidden = !showCustom;
    customDetails.open = showCustom;
  }
}

function translationDraft(fallback = null) {
  return {
    enabled: Boolean($('tr-enabled')?.checked),
    provider: $('tr-provider') ? ($('tr-provider').value || 'bing') : (fallback?.provider || 'bing'),
    targetLanguage: $('tr-language') ? ($('tr-language').value || '').trim() : (fallback?.targetLanguage || 'zh'),
    view: $('tr-view-original')?.checked ? 'original' : 'translated',
    customEndpoint: $('tr-endpoint') ? ($('tr-endpoint').value || '').trim() : (fallback?.customEndpoint || ''),
    customModel: $('tr-model') ? ($('tr-model').value || '').trim() : (fallback?.customModel || ''),
  };
}

function canAutoSaveTranslation(draft) {
  if (draft.provider !== 'custom') return true;
  return Boolean(draft.customEndpoint && draft.customModel);
}

function mcpRows(value, pattern) {
  const result = {};
  for (const line of value.split('\n')) {
    if (!line.trim()) continue;
    const at = line.indexOf('=');
    if (at < 1 || !pattern.test(line.slice(0, at))) throw Error('每行使用 NAME=value；名称需使用合法的变量名或请求头名称。');
    result[line.slice(0, at)] = line.slice(at + 1);
  }
  return result;
}

function mcpNameValid(value) {
  return /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(value);
}

function providerDocument(selectId, hintId, providers, openExternalUrl) {
  const select = $(selectId);
  const hint = $(hintId);
  if (!select) return;
  const preset = providers?.presets?.find(row => row.id === select.value);
  const id = 'provider-document-' + selectId;
  let note = $(id);
  if (!note) {
    if (!hint) return;
    note = document.createElement('p');
    note.id = id;
    note.className = 'hint provider-document';
    hint.after(note);
  }
  note.replaceChildren();
  note.hidden = !preset;
  if (!preset) return;
  const region = preset.region === 'cn' ? '国内' : preset.region === 'global' ? '海外' : '';
  const plan = preset.kind === 'coding' ? 'Coding Plan' : '按量 API';
  note.append(document.createTextNode(`${preset.familyName || preset.name} · ${[region, plan].filter(Boolean).join(' · ')} · ${preset.models.length} 个有来源的推荐模型。`));
  if (typeof preset.keyUrl === 'string') {
    try {
      const url = new URL(preset.keyUrl);
      if (url.protocol !== 'https:') return;
      const link = document.createElement('a');
      link.href = url.href;
      link.rel = 'noopener noreferrer';
      link.textContent = '获取 API Key（官网）';
      link.addEventListener('click', event => {
        event.preventDefault();
        if (typeof openExternalUrl === 'function') void openExternalUrl(url.href, link);
      });
      note.append(document.createElement('br'), link);
    } catch {}
  }
}

function mountGrowth({ request, getState, getProviders = () => null, refresh, notify, runAction }) {
  mountLegacy();
  const local = {
    trKey: '', trDraft: null, trCopyKey: '', trCustomVisible: false,
    trSaveSeq: 0, trSaving: false, trQueuedSave: null, trSavePromise: null,
    trRestartBtn: null, trRestartBound: false, trRestarting: false,
    mcp: null, mcpConfirm: null, mcpConfirmTimer: null, busy: false,
    webDraft: null, webResult: null, webSaveSeq: 0, webSaving: false, webQueuedSave: null, webSavePromise: null,
  };
  const trBody = $('tr-body');
  const trErrors = $('tr-errors');
  if (trBody && trErrors && trErrors.parentNode === trBody && typeof trBody.after === 'function') {
    trBody.after(trErrors);
  }
  async function openExternalUrl(href, anchor) {
    let parsed;
    try {
      parsed = new URL(href);
    } catch {
      notify('链接地址无效，无法打开。', 'error');
      return;
    }
    if (parsed.protocol !== 'https:') {
      notify('仅允许在系统浏览器打开 HTTPS 链接。', 'error');
      return;
    }
    if (anchor && anchor.dataset.busy === 'true') return;
    if (anchor) anchor.dataset.busy = 'true';
    try {
      await request('/api/open-url', { method: 'POST', body: { url: parsed.href } });
    } catch (error) {
      notify(error?.message || '无法在系统默认浏览器中打开链接，请手动访问官网。', 'error');
    } finally {
      if (anchor) delete anchor.dataset.busy;
    }
  }
  const copyLabel = document.createElement('label');
  copyLabel.className = 'field';
  copyLabel.htmlFor = 'tr-copy-model';
  const copyText = document.createElement('span');
  copyText.className = 'field__label';
  copyText.textContent = '复制来源模型';
  const copySelect = document.createElement('select');
  copySelect.id = 'tr-copy-model';
  copyLabel.append(copyText, copySelect);
  $('tr-copy')?.before(copyLabel);
  copyLabel.hidden = true;

  function trDirty() {
    return Boolean((local.trDraft && !same(local.trDraft, translationDraft(local.trDraft))) || $('tr-key')?.value);
  }

  function ensureRestartEnableButton() {
    let button = $('tr-restart-enable') || local.trRestartBtn;
    if (!button) {
      const runtimeNode = $('tr-runtime');
      if (!runtimeNode) return null;
      button = document.createElement('button');
      button.id = 'tr-restart-enable';
      button.type = 'button';
      button.className = 'btn btn--primary btn--sm';
      button.textContent = '重启并启用';
      button.hidden = true;
      if (typeof runtimeNode.after === 'function') runtimeNode.after(button);
      else runtimeNode.parentNode?.append?.(button);
    }
    local.trRestartBtn = button;
    if (!local.trRestartBound) {
      local.trRestartBound = true;
      button.addEventListener('click', restartAndEnableTranslation);
    }
    return button;
  }

  function syncTranslationUI(data) {
    if (!$('tr-enabled')) return;
    const draft = translationDraft(local.trDraft);
    const enabled = draft.enabled;
    const dirty = trDirty();
    const body = $('tr-body');
    if (body) {
      body.hidden = !enabled;
    } else {
      for (const id of ['tr-provider', 'tr-language', 'tr-copy', 'tr-copy-note', 'tr-view-translated',
        'tr-view-original', 'tr-runtime', 'tr-save-row', 'tr-check', 'tr-preview-box',
        'tr-test', 'tr-preview-original', 'tr-preview-result']) {
        hidden(id, !enabled);
      }
    }
    copyLabel.hidden = !enabled || !copySelect.options.length;
    const customDetails = $('tr-custom-details');
    if (customDetails) {
      const showCustom = enabled && draft.provider === 'custom';
      if (!showCustom) {
        customDetails.hidden = true;
        customDetails.open = false;
        local.trCustomVisible = false;
      } else {
        if (!local.trCustomVisible || customDetails.hidden) customDetails.open = true;
        customDetails.hidden = false;
        local.trCustomVisible = true;
      }
    }
    hidden('tr-save', !(enabled && (draft.provider === 'custom' || dirty)));
    if (!enabled) {
      hidden('tr-draft', true);
      text('tr-draft', '');
    } else if (local.trSaving) {
      hidden('tr-draft', false);
      text('tr-draft', '正在保存…');
    } else if (dirty) {
      hidden('tr-draft', false);
      text('tr-draft', '有未保存的修改');
    } else {
      hidden('tr-draft', true);
      text('tr-draft', '');
    }
    const runtime = data?.translation?.runtime || {};
    const restartBtn = ensureRestartEnableButton();
    if (restartBtn) {
      const showRestart = enabled && runtime.reason === 'no_managed_child';
      restartBtn.hidden = !showRestart;
      restartBtn.disabled = local.trRestarting;
      restartBtn.textContent = local.trRestarting ? '正在重启 Kimi…' : '重启并启用';
    }
  }

  function rollbackTranslation() {
    const data = getState();
    const configured = data?.translation?.configured;
    if (configured) {
      const normalized = normalizeTranslationDraft(configured);
      local.trDraft = normalized;
      local.trKey = JSON.stringify([configured, data.translation?.revision]);
      fieldFromDraft(normalized);
    } else if (local.trDraft) {
      fieldFromDraft(local.trDraft);
    }
    if (data) renderTranslation(data);
    else syncTranslationUI(null);
  }

  function renderTranslation(data) {
    const config = data?.translation?.configured;
    if (!config || !$('tr-enabled')) return;
    const key = JSON.stringify([config, data.translation.revision]);
    if (!local.trDraft || (local.trKey !== key && !trDirty() && !local.trSaving)) {
      local.trDraft = normalizeTranslationDraft(config);
      local.trKey = key;
      fieldFromDraft(local.trDraft);
    }
    const hasKey = Boolean(config.hasApiKey || data?.translation?.environment?.keySet);
    text('tr-key-state', hasKey ? '已保存 API Key，留空则沿用现有密钥' : '尚未填写 API Key');
    const runtime = data.translation.runtime || {};
    const stateWord = translationStateWords[runtime.state] || '状态未报告';
    const stateNote = runtime.warning || translationStateReasons[runtime.reason]
      || (runtime.reason ? '翻译服务返回了未识别的状态，详见诊断信息。' : '保存设置与运行采用是分开的状态。');
    text('tr-runtime', '运行状态：' + stateWord + '。' + stateNote);
    const runtimeNode = $('tr-runtime');
    if (runtimeNode) {
      runtimeNode.title = [runtime.state, runtime.reason].filter(Boolean).join(' · ');
    }
    const trCopyBtn = $('tr-copy');
    if (trCopyBtn) trCopyBtn.disabled = !copySelect.value;
    const models = Array.isArray(data.models) ? data.models : [];
    const modelKey = JSON.stringify(models.map(row => [row.id, row.name, row.connectionId, row.protocol]));
    if (local.trCopyKey !== modelKey) {
      local.trCopyKey = modelKey;
      copySelect.replaceChildren();
      for (const model of models.filter(row => row.connectionId)) {
        const option = document.createElement('option');
        option.value = model.id;
        option.textContent = `${model.name || model.id} · ${model.id}`;
        copySelect.append(option);
      }
      if (trCopyBtn) trCopyBtn.disabled = !copySelect.value;
    }
    syncTranslationUI(data);
  }

  function saveTranslation(opts = {}) {
    const seq = ++local.trSaveSeq;
    local.trQueuedSave = {
      seq,
      auto: Boolean(opts.auto),
      silentSuccess: Boolean(opts.silentSuccess),
      overrideDraft: opts.overrideDraft ? normalizeTranslationDraft(opts.overrideDraft) : null,
    };
    if (local.trSavePromise) return local.trSavePromise;
    local.trSavePromise = (async () => {
      local.trSaving = true;
      let ok = false;
      try {
        while (local.trQueuedSave) {
          const job = local.trQueuedSave;
          local.trQueuedSave = null;
          const draft = job.overrideDraft || translationDraft(local.trDraft);
          if (job.auto && !canAutoSaveTranslation(draft)) {
            syncTranslationUI(getState());
            ok = false;
            continue;
          }
          showError('tr-errors', '');
          syncTranslationUI(getState());
          const apiKey = $('tr-key')?.value || '';
          const beforeData = getState();
          const body = {
            enabled: draft.enabled,
            provider: draft.provider,
            targetLanguage: draft.targetLanguage,
            view: draft.view,
            customEndpoint: draft.customEndpoint,
            customModel: draft.customModel,
            revision: beforeData?.revision,
            translationRevision: beforeData?.translation?.revision,
            ...(apiKey ? { apiKey } : {}),
          };
          try {
            const res = await request('/api/translation', { method: 'POST', body });
            if (res?.state?.translation) {
              local.trKey = JSON.stringify([res.state.translation.configured, res.state.translation.revision]);
              local.trDraft = normalizeTranslationDraft(res.state.translation.configured);
              if (beforeData && getState() === beforeData) {
                if (res.state.revision !== undefined) beforeData.revision = res.state.revision;
                beforeData.translation = res.state.translation;
              }
            } else {
              local.trDraft = { ...draft };
            }
            if (job.overrideDraft && !local.trQueuedSave && job.seq === local.trSaveSeq) {
              fieldFromDraft(local.trDraft);
            }
            if (apiKey && $('tr-key') && $('tr-key').value === apiKey) $('tr-key').value = '';
            if (typeof refresh === 'function') await refresh({ silent: true });
            ok = true;
            if (!local.trQueuedSave && job.seq === local.trSaveSeq) {
              const latest = getState();
              if (latest) renderTranslation(latest);
              else syncTranslationUI(null);
              if (!job.silentSuccess) {
                notify(job.auto ? '翻译设置已自动保存。' : '翻译设置已保存；请按运行状态确认采用情况。', 'ok');
              }
            }
          } catch (error) {
            ok = false;
            if (!local.trQueuedSave && job.seq === local.trSaveSeq) {
              rollbackTranslation();
              showError('tr-errors', errorText(error));
            }
          }
        }
        return ok;
      } finally {
        local.trSaving = false;
        local.trSavePromise = null;
        syncTranslationUI(getState());
      }
    })();
    return local.trSavePromise;
  }

  async function restartAndEnableTranslation() {
    if (local.trRestarting) return;
    local.trRestarting = true;
    showError('tr-errors', '');
    syncTranslationUI(getState());
    try {
      if (local.trSavePromise) await local.trSavePromise;
      const savedConfig = getState()?.translation?.configured;
      if (!savedConfig?.enabled || trDirty()) {
        const saved = await saveTranslation({ auto: false, silentSuccess: true });
        if (!saved) return;
      }
      await request('/api/actions', { method: 'POST', body: { action: 'restart-kimi' } });
      if (typeof refresh === 'function') await refresh({ silent: true });
      notify('已请求重启 Kimi，启动完成后将自动启用页面翻译。', 'ok');
    } catch (error) {
      showError('tr-errors', errorText(error));
    } finally {
      local.trRestarting = false;
      const latest = getState();
      if (latest) renderTranslation(latest);
      else syncTranslationUI(null);
    }
  }

  async function preview() {
    showError('tr-errors', '');
    const previewBox = $('tr-preview-box');
    if (previewBox && 'open' in previewBox) previewBox.open = true;
    const testBtn = $('tr-test');
    if (testBtn) testBtn.disabled = true;
    text('tr-preview-result', '正在发送固定短示例…');
    try {
      const res = await request('/api/translation/preview', { method: 'POST', body: { settings: translationDraft(local.trDraft) } });
      text('tr-preview-original', res.preview.original);
      text('tr-preview-result', res.preview.translated);
      notify('固定示例已返回译文。Kimi 内的显示范围仍以运行状态为准。', 'ok');
    } catch (error) {
      text('tr-preview-result', '试译未成功；没有替换原文。');
      showError('tr-errors', errorText(error));
    } finally { if (testBtn) testBtn.disabled = false; }
  }

  async function copyConnection() {
    showError('tr-errors', '');
    try {
      const res = await request('/api/translation/from-connection', { method: 'POST', body: { modelId: copySelect.value } });
      fieldFromDraft(res.settings);
      text('tr-copy-note', `已从 ${res.source.connectionName || res.source.connectionId} 的 ${res.source.modelId} 复制一次。连接以后修改不会自动跟随；请点击保存后应用。`);
      syncTranslationUI(getState());
    } catch (error) { showError('tr-errors', errorText(error)); }
  }

  function clearMcpConfirm() {
    if (local.mcpConfirmTimer) {
      clearTimeout(local.mcpConfirmTimer);
      local.mcpConfirmTimer = null;
    }
    local.mcpConfirm = null;
  }

  function renderMcp(data) {
    const tools = data.tools || { available: false, servers: [] };
    text('mcp-state', tools.available ? `原生接口可用，共 ${tools.servers.length} 个服务。读取列表不会启动服务。`
      : `MCP 配置当前不可读取：${tools.reason ? errorText({ code: tools.reason }) : '等待原生接口'}。其他功能不受影响。`);
    const list = $('mcp-list');
    if (!list) return;
    const signature = JSON.stringify([tools.available, tools.reason, tools.servers, local.busy, local.mcpConfirm]);
    if (local.mcpKey === signature) return;
    local.mcpKey = signature;
    list.replaceChildren();
    for (const server of tools.servers || []) {
      const row = document.createElement('article');
      row.className = 'mcp-row';
      const title = document.createElement('h3');
      title.textContent = server.name;
      const detail = document.createElement('p');
      const transportLabel = mcpTransportWords[server.transport] || server.transport || '未知方式';
      detail.textContent = `${transportLabel} · ${server.enabled ? '已启用' : '已停用'} · ${server.command || server.url || '未公开连接细节'}`;
      const meta = document.createElement('p');
      meta.className = 'hint';
      meta.textContent = `参数 ${server.argsCount ?? '—'} 项；环境变量 ${(server.envNames || []).join('、') || '无／未公开'}；请求头 ${(server.headerNames || []).join('、') || '无／未公开'}`;
      const actions = document.createElement('div');
      actions.className = 'action-line action-line--start';
      for (const [label, action] of [['编辑', 'edit'], [server.enabled ? '停用' : '启用', 'toggle'], ['删除', 'remove']]) {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'btn btn--quiet btn--sm';
        button.textContent = label;
        button.dataset.mcpAction = action;
        button.dataset.mcpName = server.name;
        button.disabled = local.busy;
        actions.append(button);
      }
      row.append(title, detail, meta, actions);
      if (local.mcpConfirm === server.name) {
        const confirmBar = document.createElement('div');
        confirmBar.className = 'inline-confirm';
        confirmBar.dataset.mcpConfirmRow = server.name;
        const prompt = document.createElement('p');
        prompt.className = 'inline-confirm__text';
        prompt.textContent = `确认从 Kimi 全局 MCP 配置中移除 ${server.name}？此操作不会卸载软件包。`;
        const confirmActions = document.createElement('div');
        confirmActions.className = 'inline-confirm__actions';
        const confirmBtn = document.createElement('button');
        confirmBtn.type = 'button';
        confirmBtn.className = 'btn btn--danger btn--sm';
        confirmBtn.textContent = '确认移除';
        confirmBtn.dataset.mcpAction = 'remove-confirm';
        confirmBtn.dataset.mcpName = server.name;
        confirmBtn.disabled = local.busy;
        const cancelBtn = document.createElement('button');
        cancelBtn.type = 'button';
        cancelBtn.className = 'btn btn--quiet btn--sm';
        cancelBtn.textContent = '取消';
        cancelBtn.dataset.mcpAction = 'remove-cancel';
        cancelBtn.dataset.mcpName = server.name;
        confirmActions.append(confirmBtn, cancelBtn);
        confirmBar.append(prompt, confirmActions);
        row.append(confirmBar);
      }
      list.append(row);
    }
  }

  function editMcp(server = null) {
    local.mcp = server;
    $('mcp-form').hidden = false;
    $('mcp-form-title').textContent = server ? `编辑 ${server.name}` : '添加 MCP 服务';
    $('mcp-name').value = server?.name || '';
    $('mcp-name').readOnly = Boolean(server);
    $('mcp-transport').value = server?.transport === 'sse' ? 'sse' : server?.transport === 'http' ? 'http' : 'stdio';
    $('mcp-transport').disabled = Boolean(server);
    $('mcp-command').value = server?.command || '';
    $('mcp-args').value = '';
    $('mcp-url').value = server?.url || '';
    $('mcp-secrets').value = '';
    const secretNames = server ? (server.transport === 'stdio' ? server.envNames : server.headerNames) || [] : [];
    text('mcp-secrets-hint', server
      ? `已有名称：${secretNames.join('、') || '无'}。完整留空会保留原值；填写会整组替换，必须包含所有已有名称的非空值，缺项会拒绝保存。秘密值不会回显。`
      : '每行一个 NAME=value。秘密值不会回显；已有服务的空字段不会清空旧值。');
    $('mcp-enabled').checked = server?.enabled !== false;
    showError('mcp-errors', '');
    syncMcpTransport();
    $('mcp-name').focus();
  }

  function syncMcpTransport() {
    const stdio = $('mcp-transport').value === 'stdio';
    hidden('mcp-command-field', !stdio);
    hidden('mcp-args-field', !stdio);
    hidden('mcp-url-field', stdio);
    text('mcp-secrets-label', stdio ? '环境变量（可选）' : '请求头（可选）');
  }

  async function mcpAction(action, name) {
    if (local.busy) return;
    const server = getState()?.tools?.servers?.find(row => row.name === name);
    if (action === 'edit') { clearMcpConfirm(); renderMcp(getState()); editMcp(server); return; }
    if (action === 'remove') {
      clearMcpConfirm();
      local.mcpConfirm = name;
      local.mcpConfirmTimer = setTimeout(() => {
        if (local.mcpConfirm === name) {
          local.mcpConfirm = null;
          local.mcpConfirmTimer = null;
          const data = getState();
          if (data) renderMcp(data);
        }
      }, 10000);
      renderMcp(getState());
      return;
    }
    if (action === 'remove-cancel') {
      clearMcpConfirm();
      renderMcp(getState());
      return;
    }
    if (action === 'test') return;
    clearMcpConfirm();
    const apiAction = action === 'remove-confirm' ? 'remove' : action;
    local.busy = true;
    showError('mcp-errors', '');
    renderMcp(getState());
    try {
      const body = apiAction === 'toggle'
        ? { action: 'update', name, patch: { enabled: !server.enabled } } : { action: apiAction, name };
      const res = await request('/api/tools', { method: 'POST', body });
      const data = getState();
      if (data && res.tools) data.tools = res.tools;
      renderMcp(getState());
      const resultBox = $('mcp-result');
      if (resultBox) {
        hidden('mcp-result', false);
        resultBox.replaceChildren();
        const heading = document.createElement('strong');
        const stateWord = mcpResultWords[res.result?.state] || '已处理';
        heading.textContent = `${name}：${stateWord}`;
        const names = document.createElement('p');
        const reasonWord = res.result?.reason ? (escapeMap[res.result.reason] || mcpResultWords[res.result.reason] || '详见诊断信息') : '';
        names.textContent = res.result?.tools?.length ? `工具：${res.result.tools.join('、')}` : `状态：${reasonWord || stateWord}`;
        names.title = [res.result?.state, res.result?.reason].filter(Boolean).join(' · ');
        resultBox.append(heading, names);
      }
      if (apiAction === 'remove') notify(`已从全局 MCP 配置中移除 ${name}。`, 'ok');
    } catch (error) { showError('mcp-errors', errorText(error)); }
    finally { local.busy = false; renderMcp(getState()); }
  }

  async function saveMcp(event) {
    event.preventDefault();
    showError('mcp-errors', '');
    const previous = local.mcp;
    const transport = $('mcp-transport').value;
    const name = $('mcp-name').value;
    if (!mcpNameValid(name)) { showError('mcp-errors', '名称须以字母或数字开头，只能含字母、数字、点、下划线与连字符，最长 64 字符。'); return; }
    try {
      const stdio = transport === 'stdio';
      const payload = { name, transport, enabled: $('mcp-enabled').checked };
      if (stdio) {
        if (!$('mcp-command').value.trim()) throw Error('请填写本地命令。');
        payload.command = $('mcp-command').value.trim();
        if ($('mcp-args').value.trim()) payload.args = $('mcp-args').value.split('\n').filter(line => line.length > 0);
      } else if (!previous || $('mcp-url').value.trim() !== previous.url) payload.url = $('mcp-url').value.trim();
      if ($('mcp-secrets').value.trim()) {
        const values = mcpRows($('mcp-secrets').value, stdio ? /^[A-Za-z_][A-Za-z0-9_]*$/ : /^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/);
        const existingNames = previous ? (stdio ? previous.envNames : previous.headerNames) || [] : [];
        const missing = existingNames.filter(name => !Object.hasOwn(values, name) || values[name] === '');
        if (missing.length) {
          throw Error(`为避免误删已有秘密，本次保存必须填写所有已有名称的非空值：${missing.join('、')}。若不准备整组替换，请清空此输入框后保存，原有秘密会保留。`);
        }
        payload[stdio ? 'env' : 'headers'] = values;
      }
      if (!previous && stdio) payload.args ||= [];
      const res = await request('/api/tools', { method: 'POST', body: previous
        ? { action: 'update', name: previous.name, patch: payload } : { action: 'add', server: payload } });
      if (!res.result?.ok) throw Error(res.result?.reason || '原生 MCP 未接受此设置。');
      const data = getState();
      if (data && res.tools) data.tools = res.tools;
      hidden('mcp-form', true);
      local.mcp = null;
      renderMcp(getState());
      notify('MCP 设置已保存，尚未启动服务。', 'ok');
    } catch (error) { showError('mcp-errors', errorText(error)); }
  }

  function renderWeb(data) {
    const web = data?.webTools;
    if (!web) { text('web-state', '本机适配服务尚未接入，请刷新面板状态。'); return; }
    const providerSelect = $('web-provider');
    if (!providerSelect) return;
    const providerWords = { official: '沿用 Kimi 官方服务', tavily: 'Tavily（本机适配）' };
    const probeWords = { passed: '正常', ok: '正常', ready: '就绪', failed: '失败', error: '失败', timeout: '超时', unavailable: '不可用', unsupported: '不支持', not_configured: '未配置', skipped: '已跳过' };
    const savedProvider = web.provider === 'tavily' ? 'tavily' : 'official';
    const activeProvider = local.webDraft === 'tavily' ? 'tavily' : local.webDraft === 'official' ? 'official' : savedProvider;
    providerSelect.value = activeProvider;
    const isTavily = activeProvider === 'tavily';
    hidden('web-key-field', !isTavily);
    hidden('web-save', !isTavily);
    text('web-key-state', web.keySet ? '已保存 Tavily Key，留空则沿用现有密钥。' : 'Tavily Key 尚未保存，请填写后保存。');
    const providerLabel = providerWords[savedProvider];
    text('web-state', local.webResult?.message || web.message || `当前方式：${providerLabel}。`);
    const searchWord = local.webResult?.search?.state ? (probeWords[local.webResult.search.state] || '已检查') : '—';
    const fetchWord = local.webResult?.fetch?.state ? (probeWords[local.webResult.fetch.state] || '已检查') : '—';
    let draftNote = '';
    if (local.webSaving) {
      draftNote = '正在保存…';
    } else if (local.webResult) {
      draftNote = `搜索：${searchWord}；正文读取：${fetchWord}。这不是当前会话已重新加载的证明。`;
    } else if (isTavily && !web.keySet) {
      draftNote = '请填写 Tavily API Key 后点击「保存服务方式」。';
    } else if (local.webDraft && local.webDraft !== savedProvider) {
      draftNote = '服务方式有未保存修改。';
    } else if (web.pendingRestart) {
      draftNote = '已保存；重新打开 Kimi 会话后生效。';
    }
    hidden('web-draft', !draftNote);
    text('web-draft', draftNote);
  }

  async function webTest() {
    showError('web-errors', '');
    const testBtn = $('web-test');
    if (testBtn) testBtn.disabled = true;
    try {
      const provider = $('web-provider')?.value === 'tavily' ? 'tavily' : 'official';
      const apiKey = provider === 'tavily' ? ($('web-key')?.value || '') : '';
      const res = await request('/api/web-tools', { method: 'POST', body: { action: 'test', provider,
        ...(apiKey ? { apiKey } : {}) } });
      local.webResult = res;
      renderWeb(getState());
    } catch (error) { showError('web-errors', errorText(error)); }
    finally { if (testBtn) testBtn.disabled = false; }
  }

  function webSave(opts = {}) {
    const seq = ++local.webSaveSeq;
    const provider = opts.provider || ($('web-provider')?.value === 'tavily' ? 'tavily' : 'official');
    const apiKey = provider === 'tavily' ? ($('web-key')?.value || '') : '';
    local.webQueuedSave = { seq, auto: Boolean(opts.auto), provider, apiKey };
    if (local.webSavePromise) return local.webSavePromise;
    local.webSavePromise = (async () => {
      local.webSaving = true;
      renderWeb(getState());
      let ok = false;
      try {
        while (local.webQueuedSave) {
          const job = local.webQueuedSave;
          local.webQueuedSave = null;
          showError('web-errors', '');
          try {
            const res = await request('/api/web-tools', {
              method: 'POST',
              body: {
                action: 'save',
                provider: job.provider,
                ...(job.provider === 'tavily' && job.apiKey ? { apiKey: job.apiKey } : {}),
              },
            });
            if (res?.state && getState()) getState().webTools = res.state;
            if (job.apiKey && $('web-key') && $('web-key').value === job.apiKey) $('web-key').value = '';
            ok = true;
            if (!local.webQueuedSave && job.seq === local.webSaveSeq) {
              local.webDraft = null;
              local.webResult = null;
              renderWeb(getState());
              notify(job.auto ? '联网搜索服务已自动保存。' : '服务方式已保存；测试和当前会话采用请分别查看。', 'ok');
            }
          } catch (error) {
            ok = false;
            if (!local.webQueuedSave && job.seq === local.webSaveSeq) {
              local.webDraft = null;
              renderWeb(getState());
              showError('web-errors', errorText(error));
            }
          }
        }
        return ok;
      } finally {
        local.webSaving = false;
        local.webSavePromise = null;
        renderWeb(getState());
      }
    })();
    return local.webSavePromise;
  }

  ensureRestartEnableButton();
  $('tr-enabled')?.addEventListener('change', () => {
    showError('tr-errors', '');
    const draft = translationDraft(local.trDraft);
    syncTranslationUI(getState());
    if (!draft.enabled && !canAutoSaveTranslation(draft) && local.trDraft && canAutoSaveTranslation(local.trDraft)) {
      void saveTranslation({ auto: true, overrideDraft: { ...local.trDraft, enabled: false } });
      return;
    }
    if (canAutoSaveTranslation(draft)) {
      void saveTranslation({ auto: true });
    }
  });
  $('tr-save')?.addEventListener('click', () => { void saveTranslation({ auto: false }); });
  $('tr-test')?.addEventListener('click', preview);
  $('tr-copy')?.addEventListener('click', copyConnection);
  $('tr-provider')?.addEventListener('change', () => {
    showError('tr-errors', '');
    syncTranslationUI(getState());
    const draft = translationDraft(local.trDraft);
    if (canAutoSaveTranslation(draft)) {
      void saveTranslation({ auto: true });
    }
  });
  for (const id of ['tr-view-translated', 'tr-view-original']) {
    $(id)?.addEventListener('change', () => {
      showError('tr-errors', '');
      syncTranslationUI(getState());
      const draft = translationDraft(local.trDraft);
      if (canAutoSaveTranslation(draft)) {
        void saveTranslation({ auto: true });
      }
    });
  }
  $('tr-check')?.addEventListener('click', () => runAction('check-translation'));
  $('translation-panel')?.addEventListener('input', () => { syncTranslationUI(getState()); });
  $('mcp-add')?.addEventListener('click', () => editMcp());
  $('mcp-close')?.addEventListener('click', () => { hidden('mcp-form', true); local.mcp = null; });
  $('mcp-cancel')?.addEventListener('click', () => { hidden('mcp-form', true); local.mcp = null; });
  $('mcp-transport')?.addEventListener('change', syncMcpTransport);
  $('mcp-form')?.addEventListener('submit', saveMcp);
  $('mcp-list')?.addEventListener('click', event => {
    const button = event.target.closest('[data-mcp-action]');
    if (button) void mcpAction(button.dataset.mcpAction, button.dataset.mcpName);
  });
  $('web-provider')?.addEventListener('change', () => {
    const nextProvider = $('web-provider')?.value === 'tavily' ? 'tavily' : 'official';
    local.webDraft = nextProvider;
    local.webResult = null;
    showError('web-errors', '');
    renderWeb(getState());
    const web = getState()?.webTools;
    const keyInput = $('web-key')?.value || '';
    if (nextProvider === 'official' || (nextProvider === 'tavily' && web?.keySet === true && !keyInput)) {
      void webSave({ auto: true, provider: nextProvider });
    }
  });
  $('web-test')?.addEventListener('click', webTest);
  $('web-save')?.addEventListener('click', () => { void webSave({ auto: false }); });
  const providerDocPairs = [['ob-preset', 'ob-preset-hint'], ['ce-preset', 'ce-preset-hint']];
  for (const [select, hint] of providerDocPairs) {
    $(select)?.addEventListener('change', () => providerDocument(select, hint, getProviders(), openExternalUrl));
  }

  return function render(data, { view } = {}) {
    if (!data) return;
    if (!view || view === 'maintenance') {
      renderTranslation(data);
      renderMcp(data);
      renderWeb(data);
    }
    if (!view || view === 'models') {
      for (const [select, hint] of providerDocPairs) {
        providerDocument(select, hint, getProviders(), openExternalUrl);
      }
    }
  };
}

globalThis.KimiWorkGrowth = Object.freeze({ mountGrowth });

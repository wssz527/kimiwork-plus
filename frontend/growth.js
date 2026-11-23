const $ = id => document.getElementById(id);
const text = (id, value) => { const node = $(id); if (node) node.textContent = value; };
const hidden = (id, value) => { const node = $(id); if (node) node.hidden = value; };
const showError = (id, error) => { const node = $(id); if (!node) return; node.hidden = !error; node.textContent = error || ''; };
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const escapeMap = {
  translation_connection_responses_only: '这个连接只支持 Responses，不能作为 Chat 翻译端点。',
  translation_connection_anthropic_only: '这个连接使用 Anthropic Messages，不能作为 Chat 翻译端点。',
  translation_connection_chat_undeclared: '该连接尚未声明 Chat Completions 支持，不能据此生成翻译端点。',
  translation_connection_key_missing: '该连接的 Key 尚未进入当前翻译宿主；请通过现有连接应用流程准备后再复制。',
  translation_connection_endpoint_not_base: '该连接地址已经是推理端点，不是可追加 Chat 路径的基础地址。',
  translation_connection_unavailable: '请选择一个已生效、有上游连接的模型。',
  translation_invalid_config: '翻译配置无效，请检查语言、端点和凭据变量名。',
  translation_preview_failed: '试译未完成，原文保持不变。请检查翻译服务与目标语言。',
  translation_timeout: '翻译服务超时，原文保持不变。',
  translation_http_error: '翻译服务返回错误，请检查地址、Key 和模型。',
  native_mcp_test_failed: '连接未成功；请检查命令、地址和服务日志。',
  native_mcp_timeout: '原生 MCP 操作超时，未自动重试。',
  native_mcp_test_requires_kernel_runtime: '当前版本暂不支持连接测试；仍可管理已有服务配置。',
};

/* 翻译宿主状态来自内核，state 与 reason 都是英文内部值；面向用户只说结论，
   原始码留在 title 里供排查。 */
const translationStateWords = { inactive: '未启用', active: '已生效', attaching: '正在接入页面', failed: '接入失败' };
const translationStateReasons = {
  translation_disabled: '翻译已在设置里关闭，Kimi 页面不会被注入。',
  no_managed_child: '还没有受管启动的 Kimi；启动后才会应用翻译。',
  translation_waiting_view: '已准备好，等待 Work 页面出现后接入。',
};
const contextStatusWords = {
  inherit: '沿用 Kimi 默认',
  applied: '已写入配置',
  unchanged: '与已保存一致',
  repaired: '已修复并写入',
  restored: '已恢复原生默认',
  conflict: '存在配置冲突（未覆盖）',
  unavailable: '当前不可读取',
  unsupported: '当前版本不支持',
  waiting: '等待应用',
  pending: '待写入',
  error: '读取失败',
};
const contextReasonWords = {
  config_not_found: '尚未找到 Kimi 配置文件',
  config_unreadable: '配置文件不可读',
  conflict: '存在外来配置块，未覆盖',
  foreign_block: '存在外来配置块，未覆盖',
  invalid_config: '配置内容无效',
  read_failed: '读取失败',
  write_failed: '写入失败',
  unsupported: '当前版本不支持',
  unavailable: '当前不可用',
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
  /* 新布局下接入与更新已在 settings-pane-connection 中；仅兼容旧版无分栏 DOM。 */
  if (!$('settings-pane-connection')) {
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

function fieldFromDraft(draft) {
  $('tr-enabled').checked = draft.enabled;
  $('tr-provider').value = draft.provider;
  $('tr-language').value = draft.targetLanguage;
  $('tr-endpoint').value = draft.customEndpoint;
  $('tr-model').value = draft.customModel;
  $('tr-keyenv').value = draft.apiKeyEnv;
  $(draft.view === 'original' ? 'tr-view-original' : 'tr-view-translated').checked = true;
  $('tr-custom-details').open = draft.provider === 'custom';
}

function translationDraft() {
  return {
    enabled: $('tr-enabled').checked,
    provider: $('tr-provider').value,
    targetLanguage: $('tr-language').value.trim(),
    view: $('tr-view-original').checked ? 'original' : 'translated',
    customEndpoint: $('tr-endpoint').value.trim(),
    customModel: $('tr-model').value.trim(),
    apiKeyEnv: $('tr-keyenv').value.trim(),
  };
}

function contextDraft() {
  return $('context-custom').checked ? {
    mode: 'custom', triggerRatio: Number($('context-ratio').value), reservedTokens: Number($('context-reserved').value),
  } : { mode: 'inherit' };
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

function capabilityText(value) {
  return value === 'unknown' ? '待确认' : value === true ? '支持' : value === false ? '不支持' : value ?? '待确认';
}

function providerDocument(selectId, hintId, providers, openExternalUrl) {
  const select = $(selectId);
  const preset = providers?.presets?.find(row => row.id === select?.value);
  const id = 'provider-document-' + selectId;
  let note = $(id);
  if (!note) {
    note = document.createElement('p');
    note.id = id;
    note.className = 'hint provider-document';
    $(hintId)?.after(note);
  }
  note.replaceChildren();
  note.hidden = !preset;
  if (!preset) return;
  const region = preset.region === 'cn' ? '国内' : preset.region === 'global' ? '海外' : '';
  const plan = preset.kind === 'coding' ? 'Coding Plan' : '按量 API';
  note.append(document.createTextNode(`${preset.familyName || preset.name} · ${[region, plan].filter(Boolean).join(' · ')} · ${preset.models.length} 个有来源的推荐模型。`));
  const models = providers?.details?.[preset.id]?.models || [];
  if (models.length) {
    const summary = models.slice(0, 3).map(model => `${model.label}：工具${capabilityText(model.tools)}，图片${capabilityText(model.imageInput)}，推理${capabilityText(model.reasoning)}，上下文${capabilityText(model.contextWindow)}，最大输出${capabilityText(model.maxOutputTokens)}`).join('；');
    note.append(document.createElement('br'), document.createTextNode(summary));
  }
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

function providerFlowField(id, labelText, before) {
  const label = document.createElement('label');
  label.className = 'field';
  label.htmlFor = id;
  const caption = document.createElement('span');
  caption.className = 'field__label';
  caption.textContent = labelText;
  const select = document.createElement('select');
  select.id = id;
  label.append(caption, select);
  before.before(label);
  return select;
}

function fillProviderOptions(select, rows, value) {
  const signature = JSON.stringify([rows, value]);
  if (select.dataset.providerOptions === signature) return;
  select.dataset.providerOptions = signature;
  select.replaceChildren();
  for (const row of rows) {
    const option = document.createElement('option');
    option.value = row.value;
    option.textContent = row.label;
    select.append(option);
  }
  select.value = value;
}

function mountProviderFlow(selectId) {
  const select = $(selectId);
  if (!select) return () => {};
  const label = select.closest('label');
  const family = providerFlowField(selectId + '-family', '厂商', label);
  const region = providerFlowField(selectId + '-region', '地区', label);
  const plan = providerFlowField(selectId + '-plan', '服务类型', label);
  const caption = label.querySelector('.field__label');
  if (caption) caption.textContent = '接口与端点（预设）';
  const state = { family: '', region: '', plan: '' };
  let providers = null;
  let syncing = false;

  function choose(value) {
    if (select.value === value) return;
    syncing = true;
    select.value = value;
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncing = false;
  }

  function render(nextProviders) {
    providers = nextProviders || providers;
    const families = providers?.menu?.families || [];
    if (!families.length) {
      delete select.dataset.providerFlow;
      family.closest('label').hidden = true;
      region.closest('label').hidden = true;
      plan.closest('label').hidden = true;
      select.disabled = false;
      return;
    }
    select.dataset.providerFlow = 'managed';
    family.closest('label').hidden = false;
    region.closest('label').hidden = false;
    plan.closest('label').hidden = false;
    const desired = select.dataset.desiredPreset;
    if (desired !== undefined) delete select.dataset.desiredPreset;
    const targetPreset = desired !== undefined ? desired : select.value;
    const current = providers?.presets?.find(row => row.id === targetPreset);
    if (current) {
      state.family = current.family || current.id;
      state.region = current.region || 'null';
      state.plan = current.kind;
    } else if (desired === '') {
      state.family = '';
      state.region = '';
      state.plan = '';
    }
    fillProviderOptions(family, [{ value: '', label: '自定义（不套用预设）' },
      ...families.map(row => ({ value: row.family, label: row.name }))], state.family);
    const selectedFamily = families.find(row => row.family === state.family);
    const regions = selectedFamily?.regions || [];
    if (!regions.some(row => String(row.region) === state.region)) state.region = regions.length === 1 ? String(regions[0].region) : '';
    fillProviderOptions(region, [{ value: '', label: regions.length ? '选择地区' : '先选择厂商' },
      ...regions.map(row => ({ value: String(row.region), label: row.name || '通用' }))], state.region);
    region.disabled = !selectedFamily || regions.length < 2;
    const selectedRegion = regions.find(row => String(row.region) === state.region);
    const plans = selectedRegion?.plans || [];
    if (!plans.some(row => row.plan === state.plan)) state.plan = plans.length === 1 ? plans[0].plan : '';
    fillProviderOptions(plan, [{ value: '', label: plans.length ? '选择服务类型' : '先选择地区' },
      ...plans.map(row => ({ value: row.plan, label: row.name }))], state.plan);
    plan.disabled = !selectedRegion || plans.length < 2;
    const variants = plans.find(row => row.plan === state.plan)?.variants || [];
    const nextValue = variants.some(row => row.presetId === targetPreset) ? targetPreset : '';
    fillProviderOptions(select, [{ value: '', label: variants.length ? '选择接口与端点' : '先选择厂商、地区与服务类型' },
      ...variants.map(row => ({ value: row.presetId, label: row.name }))], nextValue);
    select.disabled = !selectedFamily || !state.plan;
  }

  family.addEventListener('change', () => {
    state.family = family.value;
    state.region = '';
    state.plan = '';
    choose('');
    render(providers);
  });
  region.addEventListener('change', () => {
    state.region = region.value;
    state.plan = '';
    choose('');
    render(providers);
  });
  plan.addEventListener('change', () => {
    state.plan = plan.value;
    choose('');
    render(providers);
  });
  select.addEventListener('change', () => { if (!syncing) render(providers); });
  select.addEventListener('kwp:sync-provider-flow', () => render(providers));
  return render;
}

function mountGrowth({ request, getState, getProviders = () => null, refresh, notify, runAction }) {
  mountLegacy();
  const local = { trKey: '', trDraft: null, trCopyKey: '', ctxKey: '', ctxDraft: null, mcp: null, mcpConfirm: null, mcpConfirmTimer: null, busy: false, webDraft: null, webResult: null };
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
  $('tr-copy').before(copyLabel);
  copyLabel.hidden = true;

  function trDirty() { return Boolean(local.trDraft && !same(local.trDraft, translationDraft())); }
  function renderTranslation(data) {
    const config = data.translation?.configured;
    if (!config) return;
    const key = JSON.stringify([config, data.translation.revision]);
    if (!local.trDraft || local.trKey !== key && !trDirty()) {
      local.trDraft = { ...config };
      local.trKey = key;
      fieldFromDraft(config);
    }
    const envReady = data.translation.environment?.keySet === true;
    text('tr-key-state', config.provider === 'custom'
      ? envReady ? '当前宿主已提供此凭据变量；Key 不会回显。' : '当前宿主未提供此变量；请在下方重新输入 Key 并保存。'
      : 'Bing / Google 使用现有翻译服务；不需要模型连接 Key。');
    const runtime = data.translation.runtime || {};
    const stateWord = translationStateWords[runtime.state] || '状态未报告';
    const stateNote = runtime.warning || translationStateReasons[runtime.reason]
      || (runtime.reason ? '宿主报告了未识别的状态，详见诊断信息。' : '保存设置与运行采用是分开的状态。');
    text('tr-runtime', '运行状态：' + stateWord + '。' + stateNote);
    const runtimeNode = $('tr-runtime');
    if (runtimeNode) {
      runtimeNode.title = [runtime.state, runtime.reason].filter(Boolean).join(' · ');
    }
    text('tr-draft', trDirty() ? '有未保存的修改' : '与已保存设置一致');
    $('tr-copy').disabled = !copySelect.value;
    const modelKey = JSON.stringify(data.models.map(row => [row.id, row.name, row.connectionId, row.protocol]));
    if (local.trCopyKey !== modelKey) {
      local.trCopyKey = modelKey;
      copySelect.replaceChildren();
      for (const model of data.models.filter(row => row.connectionId)) {
        const option = document.createElement('option');
        option.value = model.id;
        option.textContent = `${model.name || model.id} · ${model.id}`;
        copySelect.append(option);
      }
      copyLabel.hidden = !copySelect.options.length;
      $('tr-copy').disabled = !copySelect.value;
    }
  }

  async function saveTranslation() {
    const draft = translationDraft();
    showError('tr-errors', '');
    try {
      const data = getState();
      const res = await request('/api/translation', { method: 'POST', body: {
        ...draft, revision: data.revision, translationRevision: data.translation.revision,
        ...($('tr-key').value ? { apiKey: $('tr-key').value } : {}),
      } });
      if (res.state) {
        local.trKey = JSON.stringify([res.state.translation.configured, res.state.translation.revision]);
        local.trDraft = { ...res.state.translation.configured };
      }
      await refresh({ silent: true });
      $('tr-key').value = '';
      notify('翻译设置已保存；请按运行状态确认采用情况。', 'ok');
    } catch (error) { showError('tr-errors', errorText(error)); }
  }

  async function preview() {
    showError('tr-errors', '');
    $('tr-test').disabled = true;
    text('tr-preview-result', '正在发送固定短示例…');
    try {
      const res = await request('/api/translation/preview', { method: 'POST', body: { settings: translationDraft() } });
      text('tr-preview-original', res.preview.original);
      text('tr-preview-result', res.preview.translated);
      notify('固定示例已返回译文。Kimi 内的显示范围仍以运行状态为准。', 'ok');
    } catch (error) {
      text('tr-preview-result', '试译未成功；没有替换原文。');
      showError('tr-errors', errorText(error));
    } finally { $('tr-test').disabled = false; }
  }

  async function copyConnection() {
    showError('tr-errors', '');
    try {
      const res = await request('/api/translation/from-connection', { method: 'POST', body: { modelId: copySelect.value } });
      fieldFromDraft(res.settings);
      text('tr-copy-note', `已从 ${res.source.connectionName || res.source.connectionId} 的 ${res.source.modelId} 复制一次。连接以后修改不会自动跟随；请点击保存后应用。`);
      text('tr-draft', '有未保存的修改');
    } catch (error) { showError('tr-errors', errorText(error)); }
  }

  function renderContext(data) {
    const ctx = data.context;
    if (!ctx) return;
    const config = ctx.policy || { mode: 'inherit' };
    const key = JSON.stringify(config);
    if (!local.ctxDraft || local.ctxKey !== key && same(local.ctxDraft, contextDraft())) {
      local.ctxDraft = { ...config };
      local.ctxKey = key;
      $(config.mode === 'custom' ? 'context-custom' : 'context-inherit').checked = true;
      if (config.mode === 'custom') {
        $('context-ratio').value = config.triggerRatio;
        $('context-reserved').value = config.reservedTokens;
      }
    }
    $('context-ratio').disabled = !$('context-custom').checked;
    $('context-reserved').disabled = !$('context-custom').checked;
    const statusLabel = contextStatusWords[ctx.status] || (ctx.status ? '状态未确认' : '未报告');
    const reasonLabel = ctx.reason ? (contextReasonWords[ctx.reason] || escapeMap[ctx.reason] || '详见诊断信息') : '';
    text('context-state', `原生配置状态：${statusLabel}。${reasonLabel ? `原因：${reasonLabel}。` : ''} 保存成功不代表当前会话已经采用。`);
    const ctxStateNode = $('context-state');
    if (ctxStateNode) ctxStateNode.title = [ctx.status, ctx.reason].filter(Boolean).join(' · ');
    text('context-draft', same(local.ctxDraft, contextDraft()) ? '与已保存策略一致' : '有未保存的修改');
  }

  async function saveContext() {
    const nextPolicy = contextDraft();
    showError('context-errors', '');
    if (nextPolicy.mode === 'custom' && (!Number.isFinite(nextPolicy.triggerRatio)
      || nextPolicy.triggerRatio < 0.5 || nextPolicy.triggerRatio > 0.99
      || !Number.isSafeInteger(nextPolicy.reservedTokens) || nextPolicy.reservedTokens < 0)) {
      showError('context-errors', '触发比例须为 0.50–0.99，预留 token 须为非负整数。');
      return;
    }
    try {
      const res = await request('/api/context', { method: 'POST', body: { policy: nextPolicy } });
      const data = getState();
      if (data && res.context) data.context = res.context;
      const saved = res.context?.policy || nextPolicy;
      local.ctxDraft = { ...saved };
      local.ctxKey = JSON.stringify(saved);
      renderContext(getState());
      notify('长对话设置已保存；新建会话或重新加载后使用。', 'ok');
    } catch (error) { showError('context-errors', errorText(error)); }
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
      for (const [label, action] of [['编辑', 'edit'], [server.enabled ? '停用' : '启用', 'toggle'], ['测试服务', 'test'], ['删除', 'remove']]) {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = action === 'test' ? 'btn btn--outline btn--sm' : 'btn btn--quiet btn--sm';
        button.textContent = label;
        button.dataset.mcpAction = action;
        button.dataset.mcpName = server.name;
        button.disabled = local.busy || (action === 'test' && tools.probe?.available === false);
        if (action === 'test' && tools.probe?.available === false) button.title = '当前版本暂不支持连接测试；配置管理仍可使用。';
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
        confirmBtn.className = 'btn btn--primary btn--sm';
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
    clearMcpConfirm();
    const apiAction = action === 'remove-confirm' ? 'remove' : action;
    local.busy = true;
    showError('mcp-errors', '');
    try {
      const body = apiAction === 'test' ? { action: apiAction, name } : apiAction === 'toggle'
        ? { action: 'update', name, patch: { enabled: !server.enabled } } : { action: apiAction, name };
      const res = await request('/api/tools', { method: 'POST', body });
      const data = getState();
      if (data && res.tools) data.tools = res.tools;
      renderMcp(getState());
      hidden('mcp-result', false);
      $('mcp-result').replaceChildren();
      const heading = document.createElement('strong');
      const stateWord = mcpResultWords[res.result?.state] || '已处理';
      heading.textContent = apiAction === 'test' ? res.result?.ok ? `${name} 连接成功` : `${name} 测试未完成` : `${name}：${stateWord}`;
      const names = document.createElement('p');
      const reasonWord = res.result?.reason ? (escapeMap[res.result.reason] || mcpResultWords[res.result.reason] || '详见诊断信息') : '';
      names.textContent = res.result?.tools?.length ? `工具：${res.result.tools.join('、')}` : `状态：${reasonWord || stateWord || '未返回工具名单'}`;
      names.title = [res.result?.state, res.result?.reason].filter(Boolean).join(' · ');
      $('mcp-result').append(heading, names);
      if (apiAction === 'test') notify(res.result?.ok ? 'MCP 显式测试已通过。' : 'MCP 测试未通过，请查看原因。', res.result?.ok ? 'ok' : 'error');
      else if (apiAction === 'remove') notify(`已从全局 MCP 配置中移除 ${name}。`, 'ok');
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
      $('mcp-form').hidden = true;
      local.mcp = null;
      renderMcp(getState());
      notify('MCP 设置已保存，尚未测试或启动服务。', 'ok');
    } catch (error) { showError('mcp-errors', errorText(error)); }
  }

  function renderWeb(data) {
    const web = data.webTools;
    if (!web) { text('web-state', '本机适配服务尚未接入，请刷新面板状态。'); return; }
    const providerWords = { official: '沿用 Kimi 官方服务', tavily: 'Tavily（本机适配）', native: '自定义原生服务' };
    const probeWords = { passed: '正常', ok: '正常', ready: '就绪', failed: '失败', error: '失败', timeout: '超时', unavailable: '不可用', unsupported: '不支持', not_configured: '未配置', skipped: '已跳过' };
    $('web-provider').value = local.webDraft || web.provider || 'official';
    hidden('web-key-field', $('web-provider').value !== 'tavily');
    text('web-key-state', web.keySet ? '已保存 Tavily Key；留空保留，不会回显。' : 'Tavily Key 尚未保存或当前构建不可读，请重新输入并保存。');
    const providerLabel = providerWords[web.provider || 'official'] || '自定义服务';
    text('web-state', local.webResult?.message || web.message || `当前方式：${providerLabel}。测试与当前会话采用分开报告。`);
    hidden('native-search-details', $('web-provider').value !== 'native');
    const searchWord = local.webResult?.search?.state ? (probeWords[local.webResult.search.state] || '已检查') : '—';
    const fetchWord = local.webResult?.fetch?.state ? (probeWords[local.webResult.fetch.state] || '已检查') : '—';
    text('web-draft', local.webResult
      ? `搜索：${searchWord}；正文读取：${fetchWord}。这不是当前会话已重新加载的证明。`
      : local.webDraft && local.webDraft !== web.provider ? '服务方式有未保存修改。'
        : web.pendingRestart ? '已保存；需要重新加载 Kimi 会话后使用。' : '默认官方服务不会写入接管配置。');
  }

  async function webTest() {
    showError('web-errors', '');
    $('web-test').disabled = true;
    try {
      const res = await request('/api/web-tools', { method: 'POST', body: { action: 'test', provider: $('web-provider').value,
        ...($('web-key').value ? { apiKey: $('web-key').value } : {}) } });
      local.webResult = res;
      renderWeb(getState());
    } catch (error) { showError('web-errors', errorText(error)); }
    finally { $('web-test').disabled = false; }
  }

  async function webSave() {
    showError('web-errors', '');
    try {
      const res = await request('/api/web-tools', { method: 'POST', body: { action: 'save', provider: $('web-provider').value,
        ...($('web-key').value ? { apiKey: $('web-key').value } : {}) } });
      if (res.state && getState()) getState().webTools = res.state;
      local.webDraft = null;
      local.webResult = null;
      $('web-key').value = '';
      renderWeb(getState());
      notify('服务方式已保存；测试和当前会话采用请分别查看。', 'ok');
    } catch (error) { showError('web-errors', errorText(error)); }
  }

  $('tr-save').addEventListener('click', saveTranslation);
  $('tr-test').addEventListener('click', preview);
  $('tr-copy').addEventListener('click', copyConnection);
  $('tr-provider').addEventListener('change', () => { $('tr-custom-details').open = $('tr-provider').value === 'custom'; renderTranslation(getState()); });
  $('tr-check').addEventListener('click', () => runAction('check-translation'));
  $('translation-panel').addEventListener('input', () => { text('tr-draft', trDirty() ? '有未保存的修改' : '与已保存设置一致'); });
  $('context-save').addEventListener('click', saveContext);
  for (const id of ['context-inherit', 'context-custom']) $(id).addEventListener('change', () => renderContext(getState()));
  $('context-panel').addEventListener('input', () => { text('context-draft', same(local.ctxDraft, contextDraft()) ? '与已保存策略一致' : '有未保存的修改'); });
  $('mcp-add').addEventListener('click', () => editMcp());
  $('mcp-close').addEventListener('click', () => { $('mcp-form').hidden = true; local.mcp = null; });
  $('mcp-cancel').addEventListener('click', () => { $('mcp-form').hidden = true; local.mcp = null; });
  $('mcp-transport').addEventListener('change', syncMcpTransport);
  $('mcp-form').addEventListener('submit', saveMcp);
  $('mcp-list').addEventListener('click', event => {
    const button = event.target.closest('[data-mcp-action]');
    if (button) void mcpAction(button.dataset.mcpAction, button.dataset.mcpName);
  });
  $('web-provider').addEventListener('change', () => {
    local.webDraft = $('web-provider').value;
    local.webResult = null;
    renderWeb(getState());
  });
  $('web-test').addEventListener('click', webTest);
  $('web-save').addEventListener('click', webSave);
  const providerFlows = [['ob-preset', 'ob-preset-hint'], ['ce-preset', 'ce-preset-hint']]
    .map(([select, hint]) => ({ select, hint, render: mountProviderFlow(select) }));
  for (const { select, hint } of providerFlows) {
    $(select)?.addEventListener('change', () => providerDocument(select, hint, getProviders(), openExternalUrl));
  }

  return function render(data, { view } = {}) {
    if (!data) return;
    if (!view || view === 'maintenance') {
      renderTranslation(data);
      renderContext(data);
      renderMcp(data);
      renderWeb(data);
    }
    if (!view || view === 'models') {
      for (const { select, hint, render } of providerFlows) {
        render(getProviders());
        providerDocument(select, hint, getProviders(), openExternalUrl);
      }
    }
  };
}

globalThis.KimiWorkGrowth = Object.freeze({ mountGrowth });

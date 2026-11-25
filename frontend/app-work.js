import { configInfo, configReady } from './app-connections.js';
import { API, POLICY_MODE_WORD, POLICY_TRANSPORT_WORD, RUNTIME_STATE, catalogModelLabel, dom, fmtCount, fmtTime, iconUse, modelLabel, renderErrorList, request, setDot, setHidden, setText, showToast, state, syncSelectOptions } from './app-core.js';
import { declaredEfforts, findModel, isCapable, modelIdFromAlias, modelSignature, resolveAlias } from './app-models.js';
import { isInFlight } from './app-overview.js';
import { adoptState, handleSaveFailure, loadState, renderAll } from './app.js';

/* ---------- render: subagents ----------
   子代理目标复用 POST /api/subagents 的原有契约：
   { revision, mode, model, effort?, models? }，mode 只有 inherit/force/pool。
   state.subagents 分三层：configured 是已保存目标，policy 是本机策略文件，
   runtime 是内核运行时；只有 runtime.observed.state === 'observed-schema'
   才证明工具定义里真的出现了模型池，写入文件不等于内核已采用。 */

function capableModels() {
  const list = state.data && Array.isArray(state.data.models) ? state.data.models : [];
  return list.filter(isCapable);
}

const SUBAGENT_ROLES = [
  { key: 'plan', label: '规划（plan）', phrase: '用于 plan 架构规划、方案设计与任务拆解' },
  { key: 'coder', label: '编码（coder）', phrase: '用于 coder 代码编写、修改与调试' },
  { key: 'explore', label: '探索（explore）', phrase: '用于 explore 只读代码检索与资料查阅' },
];

function emptyRoles() {
  return { plan: '', coder: '', explore: '' };
}

function normalizeRoles(raw) {
  const out = emptyRoles();
  if (!raw || typeof raw !== 'object') return out;
  for (const role of SUBAGENT_ROLES) {
    if (typeof raw[role.key] === 'string' && raw[role.key].trim()) {
      out[role.key] = raw[role.key].trim();
    }
  }
  return out;
}

function roleDescriptionFor(modelId, defaultId, roles) {
  const phrases = [];
  if (modelId === defaultId) phrases.push('默认子代理');
  for (const role of SUBAGENT_ROLES) {
    if (roles && roles[role.key] === modelId) phrases.push(role.phrase);
  }
  return phrases.join('；');
}

function isGeneratedRoleDescription(desc) {
  const text = String(desc || '').trim();
  if (!text) return true;
  if (text === '默认子代理') return true;
  return text.startsWith('默认子代理；用于 ') || SUBAGENT_ROLES.some((role) => text.startsWith(role.phrase));
}

function syncRoleAssignmentsToPool(draft, prevDefault, prevRoles) {
  if (!draft || draft.mode !== 'pool') return draft;
  const roles = normalizeRoles(draft.roles);
  const nextDefault = draft.model || '';
  const assignedIds = new Set();
  if (nextDefault) assignedIds.add(nextDefault);
  for (const role of SUBAGENT_ROLES) {
    if (roles[role.key]) assignedIds.add(roles[role.key]);
  }
  const prevAssigned = new Set();
  if (prevDefault) prevAssigned.add(prevDefault);
  if (prevRoles) {
    for (const role of SUBAGENT_ROLES) {
      if (prevRoles[role.key]) prevAssigned.add(prevRoles[role.key]);
    }
  }

  const existing = Array.isArray(draft.models) ? draft.models : [];
  const byId = new Map();
  const order = [];
  for (const row of existing) {
    if (!row || !row.id) continue;
    const stillAssigned = assignedIds.has(row.id);
    const wasAutoOnly = !stillAssigned && prevAssigned.has(row.id) && (row.autoRole || isGeneratedRoleDescription(row.description));
    if (wasAutoOnly) continue;
    const copy = { id: row.id, description: row.description, ...(row.autoRole ? { autoRole: true } : {}) };
    if (stillAssigned) {
      const autoDesc = roleDescriptionFor(row.id, nextDefault, roles);
      if (autoDesc && (copy.autoRole || isGeneratedRoleDescription(copy.description))) {
        copy.description = autoDesc;
        copy.autoRole = true;
      }
    }
    byId.set(row.id, copy);
    order.push(row.id);
  }

  for (const id of assignedIds) {
    if (byId.has(id)) continue;
    const autoDesc = roleDescriptionFor(id, nextDefault, roles) || '默认子代理';
    byId.set(id, { id, description: autoDesc, autoRole: true });
    order.push(id);
  }

  return {
    ...draft,
    model: nextDefault || null,
    roles,
    models: order.map((id) => byId.get(id)).filter(Boolean),
  };
}

function cacheDraftMode(draft) {
  if (!draft || !state.subagentModeCache) return;
  if (draft.mode === 'force') {
    state.subagentModeCache.force = {
      model: draft.model || null,
      effort: draft.effort || null,
    };
  } else if (draft.mode === 'pool') {
    state.subagentModeCache.pool = {
      model: draft.model || null,
      models: Array.isArray(draft.models) ? draft.models.map((row) => ({ ...row })) : [],
      roles: normalizeRoles(draft.roles),
    };
  }
}

/* 已保存目标来自 configured；model 缺失时按 modelAlias 反查目录 ID，
   pool 只保留带单行用途说明的候选，外来或缺失的字段不会被猜成默认值。 */
function savedTarget(data) {
  const configured = data && data.subagents && data.subagents.configured;
  const mode = configured && ['force', 'pool'].includes(configured.mode) ? configured.mode : 'inherit';
  return {
    mode,
    model: mode !== 'inherit'
      ? typeof configured.model === 'string' && configured.model
        ? configured.model
        : typeof configured.modelAlias === 'string' && configured.modelAlias
          ? modelIdFromAlias(configured.modelAlias) || resolveAlias(configured.modelAlias)
          : null
      : null,
    effort: mode === 'force' && typeof configured.effort === 'string' ? configured.effort : null,
    models: mode === 'pool' && Array.isArray(configured.models)
      ? configured.models
        .filter((row) => row && typeof row.id === 'string' && typeof row.description === 'string')
        .map((row) => ({ id: row.id, description: row.description }))
      : [],
    roles: mode === 'pool' ? normalizeRoles(configured && configured.roles) : emptyRoles(),
  };
}

function draftFromSaved(data) {
  const saved = savedTarget(data);
  return { mode: saved.mode, model: saved.model, effort: saved.effort,
    models: saved.models.map((row) => ({ ...row })),
    roles: { ...saved.roles } };
}

function sameTarget(a, b) {
  if (!a || !b || a.mode !== b.mode) return false;
  if (a.mode === 'inherit') return true;
  if ((a.model || null) !== (b.model || null)) return false;
  if (a.mode === 'pool') {
    const left = Array.isArray(a.models) ? a.models : [];
    const right = Array.isArray(b.models) ? b.models : [];
    const sameModels = left.length === right.length && left.every((row, index) => row && right[index]
      && row.id === right[index].id && row.description === right[index].description);
    if (!sameModels) return false;
    const lr = normalizeRoles(a.roles);
    const rr = normalizeRoles(b.roles);
    return SUBAGENT_ROLES.every((role) => lr[role.key] === rr[role.key]);
  }
  return (a.effort || null) === (b.effort || null);
}

function recomputeDraftDirty() {
  const saved = state.draftBaseEntity ?? (state.data ? savedTarget(state.data) : null);
  state.draftDirty = Boolean(state.data && state.draft) && !sameTarget(state.draft, saved);
}

/* 候选只能是目录里已声明且 tools:true 的模型；默认模型必须在池内；
   池成员用途说明必填、单行、≤1000 字符；1–32 个成员。任何一项不满足都
   禁用保存并给出原因，绝不把外来/缺失模型悄悄替换掉。 */
const SUBAGENT_CAPABILITY_MISSING = '目录中没有声明支持工具调用的模型（tools:true）；'
  + '固定模型与模型池只能使用这类模型。请到「模型」核实上游是否真的支持工具调用，'
  + '确认后再勾选「支持工具调用」。';

function evaluateSubagentDraft(draft, data) {
  const problems = [];
  const notes = [];
  if (!data) return { problems, notes };
  if (!configReady(data)) problems.push('配置当前不是就绪状态，保存已禁用。重启面板后可再次保存。');
  if (!draft || draft.mode === 'inherit') return { problems, notes };

  const list = Array.isArray(data.models) ? data.models : [];
  const capable = list.filter((row) => isCapable(row));
  const model = draft.model ? list.find((row) => row && row.id === draft.model) || null : null;
  if (!draft.model && draft.mode !== 'pool') {
    problems.push(capable.length ? '尚未选择模型。' : SUBAGENT_CAPABILITY_MISSING);
  } else if (draft.model && !model) problems.push('所选模型已不在目录中，请重新选择。');
  else if (draft.model && !isCapable(model)) problems.push('所选模型未声明工具调用，不能作为子代理。');

  if (draft.mode === 'pool') {
    const pool = Array.isArray(draft.models) ? draft.models : [];
    const ids = new Set(pool.map((row) => row && row.id));
    if (!capable.length) problems.push(SUBAGENT_CAPABILITY_MISSING);
    else if (!pool.length) problems.push('候选列表为空，至少选择默认子代理或加入 1 个模型。');
    if (pool.length > 32) problems.push('候选模型最多 32 个。');
    if (pool.some((row) => !row || typeof row.description !== 'string' || !row.description.trim()
      || row.description.length > 1000 || /[\r\n\t]/.test(row.description))) {
      problems.push('每个候选模型都需要单行用途说明，最长 1000 字符。');
    }
    if (pool.some((row) => row && row.id && !list.some((entry) => entry && entry.id === row.id))) {
      problems.push('候选列表中有模型已不在目录中，请移除后重新选择。');
    } else if (pool.some((row) => row && row.id && !isCapable(list.find((entry) => entry && entry.id === row.id)))) {
      problems.push('候选列表中有模型未声明工具调用，不能作为子代理。');
    }
    if (!draft.model || !ids.has(draft.model)) problems.push('默认模型必须在候选列表中。');
    const roles = normalizeRoles(draft.roles);
    for (const role of SUBAGENT_ROLES) {
      const assigned = roles[role.key];
      if (!assigned) continue;
      const entry = list.find((item) => item && item.id === assigned) || null;
      if (!entry) problems.push(role.label + ' 所选模型已不在目录中，请重新选择。');
      else if (!isCapable(entry)) problems.push(role.label + ' 所选模型未声明工具调用，不能作为子代理。');
      else if (!ids.has(assigned)) problems.push(role.label + ' 所选模型不在候选池中，请重新选择。');
    }
    return { problems, notes };
  }

  if (draft.effort && model && !declaredEfforts(model).includes(draft.effort)) {
    problems.push('该模型未声明档位 ' + draft.effort + '。请到「模型」补充该档位，或改选其他档位。');
  }
  if (!draft.effort && model && !model.reasoning) notes.push('所选模型未声明推理能力，保存时将使用自动档位。');
  return { problems, notes };
}

function poolRow(id, description) {
  return { id, description: String(description || '').replace(/[\r\n\t ]+/g, ' ').trim().slice(0, 1000) };
}

function poolDraftIds(draft) {
  const pool = draft && draft.mode === 'pool' && Array.isArray(draft.models) ? draft.models : [];
  return new Set(pool.map((row) => row && row.id));
}

/* 已保存、文件/策略、运行时三层分开描述；未观察到 observed-schema 时
   绝不写「已生效」。 */
function evaluateBinding(data) {
  if (!data) return { tone: 'neutral', word: '读取中', note: '' };
  const saved = savedTarget(data);
  const policy = (data.subagents && data.subagents.policy) || {};
  const runtime = (data.subagents && data.subagents.runtime) || {};
  const config = data.configuration || {};
  const observed = runtime.observed && runtime.observed.state === 'observed-schema';
  const runtimeState = typeof runtime.state === 'string' ? runtime.state : 'unavailable';
  const info = RUNTIME_STATE[runtimeState] || { tone: 'neutral', word: runtimeState, note: '' };

  if (config.state !== 'ready') {
    return { tone: 'bad', word: '配置' + configInfo(data).word,
      note: '配置文件当前不可用，保存已禁用。重启面板后重新载入。' };
  }
  if (runtimeState === 'conflict' || policy.mode === 'blocked') {
    return { tone: 'bad', word: policy.mode === 'blocked' ? '子代理被阻止' : '策略冲突',
      note: policy.reason || runtime.reason || '配置或外来区块冲突，未被覆盖；请检查后重试。' };
  }
  if (saved.mode === 'inherit') {
    if (runtimeState === 'loaded' && policy.mode === 'inherit' && runtime.mode === 'inherit') {
      return { tone: 'ok', word: '继承已生效', note: '运行时以继承模式加载，跟随主模型。' };
    }
    if (runtimeState === 'file-applied' && policy.mode === 'inherit') {
      return { tone: 'ok', word: '继承已下发', note: '继承设置已写入本机配置文件；是否采用以 Kimi 重新载入的结果为准。' };
    }
    return { tone: info.tone, word: info.word, note: info.note };
  }
  /* force/pool 一律走 native-config：文件写入只说明配置已落盘。 */
  if (runtimeState === 'file-applied' || runtimeState === 'waiting-config' || runtimeState === 'unverified') {
    const target = saved.mode === 'pool' ? '模型池（' + saved.models.length + ' 个候选）' : '固定模型';
    if (observed) {
      return { tone: saved.mode === 'pool' ? 'ok' : 'warn', word: '工具配置已识别',
        note: target + '已出现在 Kimi 的子代理工具配置中；这表示结构已更新，'
          + '实际选用哪个模型仍由 Kimi 决定。' };
    }
    if (runtimeState !== 'file-applied') {
      return { tone: 'warn', word: info.word,
        note: '本次目标尚未确认写入本机配置：' + (info.note || '等待 Kimi 完成兼容性检查。') };
    }
    return { tone: 'warn', word: '已写入，待 Kimi 采用',
      note: target + '已写入本机配置文件；Kimi 重新载入后，工具配置和实际选用才会反映新目标。' };
  }
  if (runtimeState === 'inherit' || runtimeState === 'restored') {
    return { tone: 'warn', word: '尚未接管', note: '本次目标尚未写入本机配置；保存后请等待配置更新。' };
  }
  if (runtimeState === 'loaded' && runtime.mode === saved.mode) {
    return { tone: observed ? 'ok' : 'warn', word: observed ? 'Kimi 已加载目标' : '待 Kimi 采用',
      note: observed
        ? 'Kimi 已加载与保存目标一致的模式，且已识别子代理工具中的模型池；具体选用仍以 Kimi 为准。'
        : 'Kimi 已加载配置，但尚未识别子代理工具中的模型池，不能确认已生效。' };
  }
  return { tone: info.tone, word: info.word, note: info.note };
}

function renderPoolList(draft, capable) {
  if (!dom.saPoolList) return;
  const pool = draft.mode === 'pool' && Array.isArray(draft.models) ? draft.models : [];
  const signature = JSON.stringify([
    pool, draft.model || null, state.subagentSaving,
    (state.data && Array.isArray(state.data.models) ? state.data.models : [])
      .map((model) => [model.id, model.name || '', model.upstreamModelId || '', model.tools === true]),
  ]);
  if (dom.saPoolList.dataset.signature === signature) return;
  dom.saPoolList.dataset.signature = signature;
  const rows = pool.map((row) => {
    const item = document.createElement('div');
    item.className = 'pool__row';
    const model = capable.find((entry) => entry.id === row.id) || null;
    const head = document.createElement('p');
    head.className = 'pool__head';
    const id = document.createElement('span');
    id.className = 'mono cell-id';
    const shownName = catalogModelLabel(state.data, row.id);
    id.textContent = shownName;
    id.setAttribute('title', shownName);
    head.append(id);
    if (row.id === draft.model) {
      const flag = document.createElement('span');
      flag.className = 'tag tag--saved';
      flag.textContent = '默认';
      head.append(flag);
    }
    if (!model) {
      const missing = document.createElement('span');
      missing.className = 'pool__name';
      missing.textContent = '不在目录';
      head.append(missing);
    }
    const note = document.createElement('p');
    note.className = 'pool__note';
    note.textContent = row.description || '—';
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'iconbtn iconbtn--danger pool__remove';
    remove.dataset.poolRemove = row.id;
    remove.setAttribute('aria-label', '从池中移除 ' + shownName);
    remove.disabled = state.subagentSaving;
    remove.append(iconUse('i-trash'));
    item.append(head, note, remove);
    return item;
  });
  const empty = document.createElement('p');
  empty.className = 'pool__empty';
  empty.textContent = pool.length
    ? (pool.length >= 32 ? '候选已达上限 32 个。' : '')
    : capable.length
      ? '还没有候选模型。选择模型、填写用途后点「加入池」。'
      : '目录中没有声明支持工具调用的模型；请先到「模型」页确认能力。';
  if (empty.textContent) rows.push(empty);
  dom.saPoolList.replaceChildren(...rows);
}

let saModelSignature = '';
let saEffortSignature = '';

function renderSubagents() {
  const data = state.data;
  if (!data) return;
  const draft = state.draft || draftFromSaved(data);
  const ready = configReady(data);
  const busy = isInFlight(data.operation);
  const capable = capableModels();
  const catalogue = Array.isArray(data.models) ? data.models : [];
  const missingCapable = capable.length === 0;
  const { problems, notes } = evaluateSubagentDraft(draft, data);

  /* 「没有可选模型」不是空白：说明目录里确实没有声明工具能力的模型，并把
     下一步指到「模型」页核实。绝不能把纯文本模型自动标成工具模型来凑列表。 */
  if (dom.saCapabilityNote) {
    setHidden(dom.saCapabilityNote, !missingCapable);
    setHidden(dom.saCapabilityNextRow, !missingCapable);
    setText(dom.saCapabilityNote, missingCapable
      ? (catalogue.length
        ? '模型目录中的 ' + fmtCount(catalogue.length) + ' 个模型都没有声明支持工具调用（tools:true）；'
          + '固定模型与模型池只列出这类模型。请到「模型」核实上游是否真的支持工具调用，'
          + '确认后再勾选「支持工具调用」，不要为了让它出现在这里而把纯文本模型标成工具模型。'
        : '模型目录还是空的，固定模型与模型池没有可选模型。请先到「模型」添加或导入模型，'
          + '并在上游确实声明工具调用时勾选「支持工具调用」。')
      : '');
  }

  if (dom.modeInherit) dom.modeInherit.checked = draft.mode === 'inherit';
  if (dom.modeForce) dom.modeForce.checked = draft.mode === 'force';
  if (dom.modePool) dom.modePool.checked = draft.mode === 'pool';
  for (const radio of [dom.modeInherit, dom.modeForce, dom.modePool]) {
    if (radio) radio.disabled = !ready || busy || state.subagentSaving;
  }
  setHidden(dom.saForceFields, draft.mode !== 'force');
  setHidden(dom.saPoolFields, draft.mode !== 'pool');

  if (draft.mode === 'pool') {
    const poolIds = poolDraftIds(draft);
    const roles = normalizeRoles(draft.roles);
    const modelOptions = (emptyLabel, currentValue) => {
      const opts = [{ value: '', label: emptyLabel }].concat(capable.map((model) => ({
        value: model.id, label: modelLabel(model),
      })));
      if (currentValue && !capable.some((model) => model.id === currentValue)) {
        opts.push({ value: currentValue, label: catalogModelLabel(state.data, currentValue) + '（不在目录）' });
      }
      return opts;
    };
    const roleDisabled = !ready || busy || state.subagentSaving || missingCapable;
    if (dom.saRoleDefault) {
      syncSelectOptions(dom.saRoleDefault, modelOptions(
        missingCapable ? '目录中没有声明支持工具的模型' : '选择默认子代理模型',
        draft.model || '',
      ), draft.model || '');
      dom.saRoleDefault.disabled = roleDisabled;
    }
    const roleSelects = { plan: dom.saRolePlan, coder: dom.saRoleCoder, explore: dom.saRoleExplore };
    for (const role of SUBAGENT_ROLES) {
      const sel = roleSelects[role.key];
      if (!sel) continue;
      syncSelectOptions(sel, modelOptions('跟随默认子代理', roles[role.key]), roles[role.key]);
      sel.disabled = roleDisabled;
    }
    const remaining = capable.filter((model) => !poolIds.has(model.id));
    const pendingAdd = dom.saPoolAdd && remaining.some((model) => model.id === dom.saPoolAdd.value)
      ? dom.saPoolAdd.value : '';
    syncSelectOptions(dom.saPoolAdd, [{ value: '',
      label: missingCapable ? '目录中没有声明支持工具的模型' : '选择要加入的模型',
    }].concat(remaining.map((model) => ({
      value: model.id, label: modelLabel(model),
    }))), pendingAdd);
    if (dom.saPoolAdd) dom.saPoolAdd.disabled = !ready || busy || state.subagentSaving
      || missingCapable || poolIds.size >= 32;
    if (dom.saPoolAppend) {
      dom.saPoolAppend.disabled = !ready || busy || state.subagentSaving
        || !dom.saPoolAdd || !dom.saPoolAdd.value || poolIds.size >= 32;
    }
    if (dom.saPoolDesc) dom.saPoolDesc.disabled = !ready || busy || state.subagentSaving || poolIds.size >= 32;
    const defaultOptions = poolIds.size
      ? Array.from(poolIds).map((id) => {
        const model = capable.find((entry) => entry.id === id) || null;
        return { value: id, label: catalogModelLabel(state.data, id) + (model ? '' : '（不在目录）') };
      })
      : [{ value: '', label: '先选择默认子代理或加入候选模型' }];
    syncSelectOptions(dom.saPoolDefault, defaultOptions, draft.model || '');
    if (dom.saPoolDefault) dom.saPoolDefault.disabled = !ready || busy || state.subagentSaving || !poolIds.size;
    setText(dom.saPoolCount, poolIds.size + ' / 32');
    renderPoolList(draft, capable);
    const poolProblems = [];
    if (draft.model && !poolIds.has(draft.model)) poolProblems.push('默认模型必须在候选列表中。');
    if (poolIds.size > 32) poolProblems.push('候选模型最多 32 个。');
    if (Array.isArray(draft.models) && draft.models.some((row) => !row || !String(row.description || '').trim())) {
      poolProblems.push('每个候选模型都需要用途说明。');
    }
    renderErrorList(dom.saPoolErrors, poolProblems);
    if (dom.saModelHint) setText(dom.saModelHint, missingCapable
      ? '目录中没有声明支持工具调用的模型；先到「模型」页确认能力。'
      : '候选 ' + poolIds.size + ' / 32 · 只能加入声明支持工具调用的模型。');
    if (dom.saEffortHint) setText(dom.saEffortHint, '模型池不设统一思考档位，各模型使用自身默认档位。');
  } else {
    const modelSignature = JSON.stringify([
      capable.map((model) => [model.id, modelLabel(model)]), draft.model, catalogModelLabel(data, draft.model),
    ]);
    if (modelSignature !== saModelSignature) {
      saModelSignature = modelSignature;
      const options = [{ value: '',
        label: capable.length ? '选择模型' : '目录中没有声明支持工具的模型',
      }].concat(capable.map((model) => ({
        value: model.id, label: modelLabel(model),
      })));
      if (draft.model && !capable.some((model) => model.id === draft.model)) {
        options.push({ value: draft.model, label: catalogModelLabel(state.data, draft.model) + '（不在目录）' });
      }
      if (dom.saModel) {
        dom.saModel.replaceChildren(...options.map((option) => {
          const node = document.createElement('option');
          node.value = option.value;
          node.textContent = option.label;
          return node;
        }));
      }
    }
    if (dom.saModel) {
      dom.saModel.value = draft.model || '';
      dom.saModel.disabled = draft.mode !== 'force' || !ready || busy || state.subagentSaving;
    }
    const model = draft.model ? findModel(draft.model) : null;
    const list = declaredEfforts(model);
    const extra = draft.effort && !list.includes(draft.effort) ? draft.effort : null;
    const effortSignature = JSON.stringify([list, model && model.defaultEffort, extra, draft.mode, draft.model]);
    if (effortSignature !== saEffortSignature) {
      saEffortSignature = effortSignature;
      const options = [{ value: '', label: model && model.defaultEffort ? '自动（默认 ' + model.defaultEffort + '）' : '自动' }];
      list.forEach((value) => {
        options.push({ value, label: value === (model && model.defaultEffort) ? value + '（默认）' : value });
      });
      if (extra) options.push({ value: extra, label: extra + '（未声明）' });
      if (dom.saEffort) {
        dom.saEffort.replaceChildren(...options.map((option) => {
          const node = document.createElement('option');
          node.value = option.value;
          node.textContent = option.label;
          return node;
        }));
      }
    }
    if (dom.saEffort) {
      dom.saEffort.value = draft.effort || '';
      dom.saEffort.disabled = draft.mode !== 'force' || !model || !ready || busy || state.subagentSaving;
    }
    if (dom.saModelHint) setText(dom.saModelHint, missingCapable
      ? '目录中没有声明支持工具调用的模型；先到「模型」页确认能力。'
      : '只列出声明支持工具调用的模型，共 ' + capable.length + ' 个。');
    if (dom.saEffortHint) setText(dom.saEffortHint, notes[0] || (model
      ? (list.length ? '来自该模型声明的 ' + list.length + ' 个档位。' : '该模型未声明档位，只能使用自动。')
      : (missingCapable ? '目录中没有声明支持工具调用的模型。' : '先选择模型。')));
  }

  renderErrorList(dom.saWarning, problems);
  renderErrorList(dom.saErrors, state.subagentErrors);
  const dirty = state.draftDirty;
  if (dom.saSave) dom.saSave.disabled = !dirty || problems.length > 0 || !ready || state.subagentSaving || busy;
  if (dom.saSave) setText(dom.saSave, state.subagentSaving ? '正在保存…' : '保存目标');
  if (dom.saReset) dom.saReset.disabled = !dirty || state.subagentSaving;
  if (dom.saDraft) {
    let text;
    if (!ready) text = '配置不是就绪状态，保存已禁用';
    else if (problems.length) text = problems[0];
    else if (!dirty) text = '草稿与已保存一致';
    else text = '草稿尚未保存';
    setText(dom.saDraft, text);
    dom.saDraft.dataset.dirty = String(dirty);
    dom.saDraft.dataset.blocked = String(problems.length > 0);
  }

  const saved = savedTarget(data);
  const policy = (data.subagents && data.subagents.policy) || {};
  const runtime = (data.subagents && data.subagents.runtime) || {};
  const configPath = data.subagents && typeof data.subagents.configPath === 'string' ? data.subagents.configPath : '';
  const savedRoles = SUBAGENT_ROLES.filter((role) => saved.roles && saved.roles[role.key])
    .map((role) => role.key + '=' + catalogModelLabel(state.data, saved.roles[role.key]));
  setText(dom.saSavedMode, POLICY_MODE_WORD[saved.mode] || saved.mode);
  setText(dom.saSavedSub, saved.mode === 'inherit' ? '跟随主模型 · 未写入目标'
    : saved.mode === 'pool'
      ? [saved.model ? '默认 ' + catalogModelLabel(state.data, saved.model) : '未指定默认',
        savedRoles.length ? savedRoles.join(' · ') : '',
        saved.models.length + ' 个候选'].filter(Boolean).join(' · ')
      : (saved.model ? catalogModelLabel(state.data, saved.model) : '未选择') + (saved.effort ? ' · 档位 ' + saved.effort : ' · 档位自动'));
  setText(dom.saPolicyMode, POLICY_MODE_WORD[policy.mode] || policy.mode || '—');
  setText(dom.saPolicySub, policy.mode === 'blocked'
    ? '策略文件里存在阻止项或外来归属冲突，未被覆盖，保存已拒绝'
    : (POLICY_TRANSPORT_WORD[policy.transport || 'native-config'] || '原生配置文件')
      + (configPath ? ' · ' + configPath : ' · 策略文件已读取'));
  if (dom.saPolicySub) dom.saPolicySub.title = [policy.transport, configPath, policy.reason].filter(Boolean).join(' · ');
  const runtimeInfo = RUNTIME_STATE[runtime.state] || { tone: 'neutral', word: runtime.state || '未知', note: '' };
  setDot(dom.rtStateDot, runtimeInfo.tone);
  setText(dom.rtState, runtimeInfo.word);
  setText(dom.rtSub, [POLICY_MODE_WORD[runtime.mode] || '', runtime.version ? 'v' + runtime.version : '',
    runtime.lastSpawn && typeof runtime.lastSpawn.modelAlias === 'string' ? '最近启动 ' + runtime.lastSpawn.modelAlias : '']
    .filter(Boolean).join(' · ') || '—');

  const verdict = evaluateBinding(data);
  setDot(dom.saVerdictDot, verdict.tone);
  setText(dom.saVerdictWord, verdict.word);
  setText(dom.saVerdictNote, verdict.note);
  const observed = runtime.observed && runtime.observed.state === 'observed-schema';
  const compat = runtime.compatibility && runtime.compatibility.state === 'source-compatible';
  setText(dom.saObservedNote, observed
    ? '真实观察：Kimi 发来的工具定义已包含模型池与默认模型（' + fmtTime(runtime.observed.at) + '）；这不代表内核已选定某个模型。'
    : '未观察到工具定义中的模型池结构：配置文件写入或版本兼容都不足以证明已生效。'
      + (compat ? ' 与当前版本兼容，等待实际调用确认。' : ''));
}

/* ---------- subagent draft mutations ---------- */

function setMode(mode) {
  if (!state.draft || !configReady(state.data)) return;
  if (!['inherit', 'force', 'pool'].includes(mode) || mode === state.draft.mode) return;
  cacheDraftMode(state.draft);
  const saved = state.data ? savedTarget(state.data) : null;
  if (mode === 'inherit') {
    state.draft = { mode: 'inherit', model: null, effort: null, models: [], roles: emptyRoles() };
  } else if (mode === 'pool') {
    const cached = state.subagentModeCache && state.subagentModeCache.pool;
    const seen = new Set();
    const seed = [];
    const push = (id, description, autoRole = false) => {
      if (typeof id !== 'string' || !id || seen.has(id)) return;
      const model = findModel(id);
      if (!model || !isCapable(model)) return;
      const row = poolRow(id, description);
      if (!row.description) return;
      if (autoRole) row.autoRole = true;
      seen.add(id);
      seed.push(row);
    };
    if (cached) {
      (Array.isArray(cached.models) ? cached.models : []).forEach((row) => push(row && row.id, row && row.description, Boolean(row && row.autoRole)));
    } else {
      (Array.isArray(state.draft.models) ? state.draft.models : []).forEach((row) => push(row && row.id, row && row.description, Boolean(row && row.autoRole)));
      if (saved && Array.isArray(saved.models)) saved.models.forEach((row) => push(row.id, row.description));
    }
    const rawRoles = normalizeRoles(cached ? cached.roles : (saved && saved.mode === 'pool' ? saved.roles : null));
    const cleanRoles = emptyRoles();
    for (const role of SUBAGENT_ROLES) {
      if (rawRoles[role.key] && seen.has(rawRoles[role.key])) cleanRoles[role.key] = rawRoles[role.key];
    }
    const preferredDefault = (cached && cached.model && seen.has(cached.model) && cached.model)
      || (saved && saved.model && seen.has(saved.model) && saved.model)
      || (seed.length ? seed[0].id : null);
    state.draft = { mode: 'pool', effort: null, models: seed, model: preferredDefault, roles: cleanRoles };
  } else {
    const cached = state.subagentModeCache && state.subagentModeCache.force;
    const cachedModel = cached && cached.model && isCapable(findModel(cached.model)) ? cached.model : null;
    const seed = cachedModel
      || (saved && saved.mode !== 'inherit' && saved.model && isCapable(findModel(saved.model)) && saved.model)
      || (state.draft.model && isCapable(findModel(state.draft.model)) && state.draft.model)
      || (capableModels()[0] ? capableModels()[0].id : null);
    const cachedEffort = cachedModel && cached.effort ? cached.effort : null;
    state.draft = { mode: 'force', model: seed || null, effort: cachedEffort, models: [], roles: emptyRoles() };
  }
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickModel(id) {
  if (!state.draft || state.draft.mode !== 'force') return;
  const model = findModel(id);
  if (!model || !isCapable(model)) return;
  state.draft = { ...state.draft, model: id, effort: state.draft.model === id ? state.draft.effort : null };
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickEffort(value) {
  if (!state.draft || state.draft.mode !== 'force') return;
  state.draft = { ...state.draft, effort: value || null };
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickRoleDefault(id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (id) {
    const model = findModel(id);
    if (!model || !isCapable(model)) return;
  }
  const prevDefault = state.draft.model || '';
  const prevRoles = normalizeRoles(state.draft.roles);
  state.draft = syncRoleAssignmentsToPool({ ...state.draft, model: id || null, roles: prevRoles }, prevDefault, prevRoles);
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickRoleModel(roleKey, id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (!SUBAGENT_ROLES.some((role) => role.key === roleKey)) return;
  if (id) {
    const model = findModel(id);
    if (!model || !isCapable(model)) return;
  }
  const prevDefault = state.draft.model || '';
  const prevRoles = normalizeRoles(state.draft.roles);
  const nextRoles = { ...prevRoles, [roleKey]: id || '' };
  const nextDefault = state.draft.model || id || null;
  state.draft = syncRoleAssignmentsToPool({ ...state.draft, model: nextDefault, roles: nextRoles }, prevDefault, prevRoles);
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickPoolDefault(id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (!id || !poolDraftIds(state.draft).has(id)) return;
  const prevDefault = state.draft.model || '';
  const prevRoles = normalizeRoles(state.draft.roles);
  state.draft = syncRoleAssignmentsToPool({ ...state.draft, model: id, roles: prevRoles }, prevDefault, prevRoles);
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function appendPoolModel(id, description) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  const model = findModel(id);
  if (!model || !isCapable(model)) return;
  const row = poolRow(id, description);
  if (!row.description) return;
  if (poolDraftIds(state.draft).has(id)) return;
  if (state.draft.models.length >= 32) return;
  const models = state.draft.models.concat([row]);
  state.draft = { ...state.draft, models, model: state.draft.model || id };
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function removePoolModel(id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (!poolDraftIds(state.draft).has(id)) return;
  const models = state.draft.models.filter((row) => row.id !== id);
  const nextRoles = normalizeRoles(state.draft.roles);
  for (const role of SUBAGENT_ROLES) {
    if (nextRoles[role.key] === id) nextRoles[role.key] = '';
  }
  const nextDefault = state.draft.model === id ? (models[0] ? models[0].id : null) : state.draft.model;
  state.draft = { ...state.draft, model: nextDefault, models, roles: nextRoles };
  cacheDraftMode(state.draft);
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function discardDraft() {
  if (!state.data) return;
  if (state.subagentModeCache) state.subagentModeCache = { force: null, pool: null };
  state.draft = draftFromSaved(state.data);
  state.draftBaseEntity = draftFromSaved(state.data);
  state.draftBaseRevision = state.data.revision ?? null;
  state.draftDirty = false;
  state.subagentErrors = [];
  renderSubagents();
}

async function saveSubagentTarget() {
  const data = state.data;
  if (!data || state.subagentSaving || !state.draft) return;
  if (!configReady(data)) {
    state.subagentErrors = ['配置当前不是就绪状态，无法保存子代理目标。请先重启面板。'];
    renderSubagents();
    return;
  }
  const { problems } = evaluateSubagentDraft(state.draft, data);
  if (problems.length) {
    state.subagentErrors = problems;
    renderSubagents();
    return;
  }

  state.subagentSaving = true;
  const epoch = state.stateReadEpoch;
  const key = state.key;
  const current = () => epoch === state.stateReadEpoch && key === state.key;
  renderSubagents();
  try {
    const payload = { revision: state.draftBaseRevision ?? data.revision, mode: state.draft.mode };
    if (state.draft.mode === 'force') {
      payload.model = state.draft.model;
      if (state.draft.effort) payload.effort = state.draft.effort;
    } else if (state.draft.mode === 'pool') {
      payload.model = state.draft.model;
      payload.models = state.draft.models.map((row) => ({ id: row.id, description: row.description }));
      const roles = normalizeRoles(state.draft.roles);
      const activeRoles = {};
      for (const role of SUBAGENT_ROLES) {
        if (roles[role.key]) activeRoles[role.key] = roles[role.key];
      }
      if (Object.keys(activeRoles).length) payload.roles = activeRoles;
    }
    const res = await request(API.subagents, { method: 'POST', body: payload });
    if (!current()) return;
    state.subagentSaving = false;
    if (res && res.state) {
      adoptState(res.state, { external: true });
      state.lastSync = Date.now();
    } else {
      state.subagentSaving = false;
      await loadState({ silent: true });
      if (!current()) return;
    }
    /* 只有成功保存才重置草稿；错误回读时保留用户已编辑的 pool/description。 */
    if (state.subagentModeCache) state.subagentModeCache = { force: null, pool: null };
    state.draft = draftFromSaved(state.data);
    cacheDraftMode(state.draft);
    state.draftBaseEntity = draftFromSaved(state.data);
    state.draftBaseRevision = state.data.revision ?? null;
    state.draftDirty = false;
    state.subagentErrors = [];
    renderAll();
    const runtimeState = state.data?.subagents?.runtime?.state;
    const policyState = state.data?.subagents?.policy?.state;
    const policyMode = state.data?.subagents?.policy?.mode;
    const conflicted = runtimeState === 'conflict' || policyState === 'conflict' || policyMode === 'blocked';
    const written = runtimeState === 'file-applied' || runtimeState === 'loaded';
    showToast(state.draft.mode === 'inherit'
      ? (conflicted ? '已保存为继承主模型，但配置文件存在冲突未被覆盖；请查看当前状态。' : '已保存为继承主模型并同步到 Kimi Work 配置。')
      : state.draft.mode === 'pool'
        ? (written ? '角色分配已写入 config.toml 模型池用途说明，供主模型派发 plan / coder / explore 子代理时参考选用，新建会话后生效。' : '模型池已保存；配置写入与内核采用情况见当前状态。')
        : (written ? '固定子代理模型已保存并写入 config.toml，新建会话后生效。' : '目标已保存；配置写入与内核采用情况见当前状态。'), conflicted ? 'warn' : 'ok');
  } catch (error) {
    if (!current()) return;
    state.subagentSaving = false;
    state.subagentErrors = [error.message || '保存失败，请重试。'];
    /* 冲突/外来区块/模型缺失都只回读状态，不覆盖草稿、不默认切继承。 */
    await handleSaveFailure(error, 'subagents');
    if (current()) renderSubagents();
  } finally {
    if (!current()) return;
    state.subagentSaving = false;
    if (state.data) renderSubagents();
  }
}


function resetSubagentSignatures() { saModelSignature = ''; saEffortSignature = ''; }

export {
  appendPoolModel,
  discardDraft,
  draftFromSaved,
  pickEffort,
  pickModel,
  pickPoolDefault,
  pickRoleDefault,
  pickRoleModel,
  recomputeDraftDirty,
  removePoolModel,
  renderSubagents,
  resetSubagentSignatures,
  saveSubagentTarget,
  savedTarget,
  setMode,
  syncRoleAssignmentsToPool,
};

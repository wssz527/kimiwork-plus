import { applyConnectionEditor, configReady, connectionDraftFrom, connectionPayload, discoveryReasonText, protocolLabel, providerPreset, renderConnections, savedConnectionsSignature, upstreamReasonText } from './app-connections.js';
import { API, BUDGET_PRESETS, CONNECTION_ID_PATTERN, EFFORT_PATTERN, MODEL_ALIAS_PREFIXES, actionUUID, displayModelId, dom, fmtK, fmtTime, fmtTokens, iconUse, request, setHidden, setText, showToast, state, syncDefaultOptions, syncSelectOptions, truncate } from './app-core.js';
import { renderActions } from './app-overview.js';
import { adoptState, handleSaveFailure, loadState, renderAll } from './app.js';

/* ---------- catalogue helpers ---------- */

function modelIndex() {
  const map = new Map();
  const list = state.data && Array.isArray(state.data.models) ? state.data.models : [];
  list.forEach((m) => {
    if (m && typeof m.id === 'string') map.set(m.id, m);
  });
  return map;
}

function findModel(id) {
  if (typeof id !== 'string' || !id) return null;
  return modelIndex().get(id) || null;
}

function modelIdFromAlias(alias) {
  if (typeof alias !== 'string' || !alias) return null;
  const index = modelIndex();
  if (index.has(alias)) return alias;
  for (const prefix of MODEL_ALIAS_PREFIXES) {
    if (alias.startsWith(prefix)) {
      const rest = alias.slice(prefix.length);
      if (index.has(rest)) return rest;
    }
  }
  return null;
}

function modelFamilyFor(id) {
  const text = String(id || '').trim();
  if (!text) return null;
  const last = text.slice(text.lastIndexOf('/') + 1).toLowerCase();
  return last.startsWith('kimi-') ? 'chat' : 'responses';
}

function isCapable(model) {
  return Boolean(model && model.tools === true);
}

function hasVision(model) {
  return Boolean(model && Array.isArray(model.inputModalities) && model.inputModalities.includes('image'));
}

function declaredEfforts(model) {
  if (!model || model.reasoning !== true || !Array.isArray(model.reasoningEfforts)) return [];
  return model.reasoningEfforts.filter((value) => typeof value === 'string' && value);
}

function canonicalModel(model) {
  if (!model) return '';
  return JSON.stringify([
    model.id || '',
    model.name || '',
    model.protocol || '',
    model.contextWindow || 0,
    model.maxOutputTokens || 0,
    /* 未知与明确 false 是不同的两件事，比较时也要分开，否则「没表态」和
       「明确不支持」会被判成同一个值。 */
    model.tools === true ? 'true' : model.tools === false ? 'false' : 'unknown',
    model.reasoning === true,
    (model.inputModalities || ['text']).join(','),
    (model.reasoningEfforts || []).join(','),
    model.defaultEffort || '',
    model.connectionId || '',
    model.upstreamModelId || '',
    model.contextLimit || 0,
    JSON.stringify(metadataSourceOf(model.metadataSource)),
    model.upstreamProtocol || '',
    JSON.stringify(canonicalPrice(model.pricing)),
    JSON.stringify(canonicalFailover(model.failover)),
  ]);
}

/* 价格与备用连接按固定顺序展开再比较：对象键序不该让「同样的一行」看起来像
   改过，否则每次打开编辑器都会被判成脏的。 */
function canonicalPrice(pricing) {
  if (!pricing || typeof pricing !== 'object') return null;
  const out = {};
  for (const field of PRICE_FIELDS) {
    const value = pricing[field];
    if (typeof value === 'number' && Number.isFinite(value)) out[field] = value;
  }
  return out.inputPerMillion === undefined && out.outputPerMillion === undefined
    && out.cachedInputPerMillion === undefined && out.cacheWritePerMillion === undefined ? null : out;
}

function canonicalFailover(failover) {
  if (!Array.isArray(failover)) return [];
  return failover.filter((row) => row && typeof row === 'object' && row.connectionId).map((row) => [
    row.connectionId, row.upstreamModelId || '', row.upstreamProtocol === 'anthropic' ? 'anthropic' : '',
  ]);
}

function parseEfforts(text) {
  /* 输入既可以是编辑器隐藏字段里的逗号分隔文本，也可以是草稿里已解析的数组。 */
  const parts = Array.isArray(text) ? text.map((part) => String(part).trim()) : String(text || '').split(',').map((part) => part.trim());
  const list = [];
  const invalid = [];
  for (const part of parts.filter(Boolean)) {
    const value = part.toLowerCase();
    if (!EFFORT_PATTERN.test(value)) invalid.push(part);
    else if (!list.includes(value)) list.push(value);
  }
  return { list, invalid };
}

function buildModel(input) {
  const errors = [];
  const id = String(input.id || '').trim();
  if (!id) errors.push('模型 ID 不能为空。');
  else if (/[\u0000-\u001f]/.test(id)) errors.push('模型 ID 不能包含控制字符。');
  else if (id.length > 2048) errors.push('模型 ID 过长。');

  const contextWindow = Number(input.context);
  if (!Number.isSafeInteger(contextWindow) || contextWindow <= 0) errors.push('上下文窗口必须是正整数。');

  const limitText = String(input.contextLimit === undefined || input.contextLimit === null ? '' : input.contextLimit).trim();
  let contextLimit;
  if (limitText) {
    contextLimit = Number(limitText);
    if (!Number.isSafeInteger(contextLimit) || contextLimit <= 0) errors.push('已知上下文上限必须是正整数，或留空表示未知。');
    else if (Number.isSafeInteger(contextWindow) && contextWindow > contextLimit) {
      errors.push('上下文窗口 ' + fmtTokens(contextWindow) + ' 超过已知上限 ' + fmtTokens(contextLimit) + '，不能保存。');
    }
  }

  const outputText = String(input.output || '').trim();
  let maxOutputTokens;
  if (outputText) {
    maxOutputTokens = Number(outputText);
    if (!Number.isSafeInteger(maxOutputTokens) || maxOutputTokens <= 0) errors.push('最大输出必须是正整数，或留空。');
  }

  const parsed = parseEfforts(input.efforts);
  if (parsed.invalid.length) errors.push('思考档位包含无法保留的值：' + parsed.invalid.join('、') + '。');

  const reasoning = input.reasoning === true;
  let defaultEffort;
  if (reasoning && parsed.list.length) {
    const wanted = String(input.defaultEffort || '').trim();
    if (wanted) {
      if (!parsed.list.includes(wanted)) errors.push('默认档位必须是已列出的档位之一。');
      else defaultEffort = wanted;
    }
  }

  const connectionId = String(input.connectionId || '').trim();
  if (connectionId && !CONNECTION_ID_PATTERN.test(connectionId)) errors.push('连接标识格式无效，请重新选择连接。');
  const upstreamModelId = String(input.upstreamModelId || '').trim();
  if (upstreamModelId && /[\u0000-\u001f]/.test(upstreamModelId)) errors.push('上游模型 ID 不能包含控制字符。');
  if (upstreamModelId.length > 2048) errors.push('上游模型 ID 过长。');
  const metadata = metadataSourceOf(input.metadataSource) || metadataSourceOf(input.baseEntity?.metadataSource);
  const priced = buildPrice(input.pricing, errors);
  const standbys = buildFailover(input.failover, connectionId, errors);
  const candidate = {
    id,
    ...(String(input.name || '').trim() ? { name: String(input.name).trim() } : {}),
    protocol: modelFamilyFor(id) || 'responses',
    contextWindow,
    ...(maxOutputTokens ? { maxOutputTokens } : {}),
    ...(typeof input.tools === 'boolean' ? { tools: input.tools } : {}),
    reasoning,
    inputModalities: input.vision === true ? ['text', 'image'] : ['text'],
    ...(reasoning && parsed.list.length ? { reasoningEfforts: parsed.list } : {}),
    ...(defaultEffort ? { defaultEffort } : {}),
    ...(connectionId ? { connectionId } : {}),
    ...(upstreamModelId ? { upstreamModelId } : {}),
    ...(input.upstreamProtocol === 'anthropic' ? { upstreamProtocol: 'anthropic' } : {}),
    ...(contextLimit ? { contextLimit } : {}),
    ...(metadata ? { metadataSource: metadata } : {}),
    ...(priced ? { pricing: priced } : {}),
    ...(standbys.length ? { failover: standbys } : {}),
  };

  const base = input.baseEntity && typeof input.baseEntity === 'object' && !Array.isArray(input.baseEntity)
    ? input.baseEntity : null;
  const values = {
    name: String(input.name || '').trim() || null,
    maxOutputTokens: maxOutputTokens ?? null,
    tools: typeof input.tools === 'boolean' ? input.tools : null,
    reasoning,
    inputModalities: candidate.inputModalities,
    reasoningEfforts: reasoning && parsed.list.length ? parsed.list : null,
    defaultEffort: defaultEffort || null,
    connectionId: connectionId || null,
    upstreamModelId: upstreamModelId || null,
    upstreamProtocol: input.upstreamProtocol === 'anthropic' ? 'anthropic' : null,
    contextLimit: contextLimit ?? null,
    metadataSource: metadata,
    pricing: priced,
    failover: standbys.length ? standbys : null,
  };
  const touched = new Set(input.touched instanceof Set ? input.touched : Array.isArray(input.touched) ? input.touched : []);
  if (base) {
    for (const [field, value] of Object.entries(values)) {
      let before = base[field] ?? null;
      let after = value;
      if (field === 'metadataSource') { before = metadataSourceOf(before); after = metadataSourceOf(after); }
      if (field === 'failover') { before = canonicalFailover(before); after = canonicalFailover(after); }
      if (field === 'reasoning') before = base.reasoning === true;
      if (field === 'inputModalities') { before = hasVision(base); after = input.vision === true; }
      if (field === 'reasoningEfforts') {
        before = base.reasoning === true && Array.isArray(base.reasoningEfforts) && base.reasoningEfforts.length
          ? base.reasoningEfforts : null;
        after = value;
      }
      if (JSON.stringify(before) !== JSON.stringify(after)) touched.add(field);
    }
    if (id !== String(base.id || '')) touched.add('id');
    if (contextWindow !== Number(base.contextWindow || 0)) touched.add('contextWindow');
  }

  const model = { id, protocol: candidate.protocol, contextWindow };
  if (!base) {
    for (const [key, value] of Object.entries(candidate)) {
      if (!['id', 'protocol', 'contextWindow'].includes(key)) model[key] = value;
    }
  } else {
    for (const field of touched) if (Object.hasOwn(values, field)) model[field] = values[field];
    if (id !== String(base.id || '')) model.originalId = String(base.id || '');
  }
  input.touched = new Set(touched);
  return { model, candidate, errors, touched: [...touched].sort() };
}

/* 元数据来源只保留契约里的三种，且只在带 url/checkedAt 时才带这些字段：
   来源不可考时宁可不写，也不伪造一个 checkedAt。 */
function metadataSourceOf(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  if (!['preset', 'discovery', 'manual'].includes(value.kind)) return null;
  const source = { kind: value.kind };
  if (typeof value.url === 'string' && /^https?:\/\//.test(value.url)) source.url = value.url;
  if (typeof value.checkedAt === 'string' && value.checkedAt) source.checkedAt = value.checkedAt;
  return source;
}

/* 定价是用户自填的花费表，四条价格都要么是有限的非负数要么为空。输入与输出
   必须成对：只有一半的价格无法给一次请求定价，留一半会让预算把请求当成免费。 */
function buildPrice(input, errors) {
  const price = {};
  let declared = false;
  for (const field of PRICE_FIELDS) {
    const raw = input && input[field];
    const text = String(raw === undefined || raw === null ? '' : raw).trim();
    if (!text) continue;
    const value = Number(text);
    if (!Number.isFinite(value) || value < 0) {
      errors.push('定价的每百万 token 价格必须是非负数，或留空。');
      return null;
    }
    if (value > 1e6) {
      errors.push('定价超出允许范围（每百万 token 最多 1000000）。');
      return null;
    }
    price[field] = value;
    declared = true;
  }
  if (!declared) return null;
  if (price.inputPerMillion === undefined || price.outputPerMillion === undefined) {
    errors.push('定价必须同时填写输入与输出价格；只有一半无法估算花费。');
    return null;
  }
  return price;
}

/* 备用连接最多 8 条，各自必须指向一条真实连接；指向主连接的备用项没有意义，
   等于把同一个上游试两次。重复的备用项也只保留第一条。 */
function buildFailover(input, primaryId, errors) {
  if (!Array.isArray(input)) return [];
  const rows = [];
  const seen = new Set();
  for (const raw of input) {
    if (!raw || typeof raw !== 'object') continue;
    const connectionId = String(raw.connectionId || '').trim();
    if (!connectionId) {
      if (String(raw.upstreamModelId || '').trim() || String(raw.upstreamProtocol || '').trim()) {
        errors.push('备用连接必须选择一条连接。');
      }
      continue;
    }
    if (!CONNECTION_ID_PATTERN.test(connectionId)) {
      errors.push('备用连接标识格式无效，请重新选择。');
      continue;
    }
    if (primaryId && connectionId === primaryId) {
      errors.push('备用连接不能与主连接相同。');
      continue;
    }
    if (seen.has(connectionId)) continue;
    seen.add(connectionId);
    const upstream = String(raw.upstreamModelId || '').trim();
    if (upstream && /[\u0000-\u001f]/.test(upstream)) {
      errors.push('备用连接的服务商模型 ID 不能包含控制字符。');
      continue;
    }
    if (upstream.length > 2048) {
      errors.push('备用连接的服务商模型 ID 过长。');
      continue;
    }
    const row = { connectionId };
    if (upstream) row.upstreamModelId = upstream;
    if (raw.upstreamProtocol === 'anthropic') row.upstreamProtocol = 'anthropic';
    rows.push(row);
    if (rows.length > 8) {
      errors.push('备用连接最多 8 条。');
      return [];
    }
  }
  return rows;
}

/* `metadataSource` 只描述 contextWindow 这一个字段的真实出处：保存预算时原样
   沿用，所以既不能默认写 discovery，也不能拿别的字段的来源顶替。判定顺序：
   后端合并后的 metadataSource（专指上下文来源）→ 合并前的 sources（只有一方
   贡献过才能归因）→ 候选自己的 parser 来源 → manual。 */
function candidateMetadataSource(model) {
  if (!model || typeof model !== 'object') return { kind: 'manual' };
  const stated = metadataSourceOf(model.metadataSource);
  if (stated) return stated;
  const sources = model.sources && typeof model.sources === 'object' ? model.sources : null;
  if (sources) {
    const sides = ['discovery', 'preset'].map((key) => metadataSourceOf(sources[key])).filter(Boolean);
    /* 两方都贡献过字段时无法判断预算出自哪边：宁可标 manual，也不猜一个。 */
    if (sides.length === 1) return sides[0];
    if (sides.length > 1) return { kind: 'manual' };
  }
  /* parser 的 per-model `source` 只有 url/checkedAt，没有 kind：它来自上游目录
     读取，且只有这条候选确实读到了 contextWindow 才谈得上预算来源。 */
  if (model.source && typeof model.source === 'object' && Number(model.contextWindow) > 0) {
    const own = metadataSourceOf({ ...model.source, kind: 'discovery' });
    if (own) return own;
  }
  return { kind: 'manual' };
}

function catalogueWith(editor, models) {
  const built = buildModel(editor.draft).model;
  const list = models.map((model) => ({ ...model }));
  const index = editor.originalId ? list.findIndex((model) => model.id === editor.originalId) : -1;
  if (index >= 0) list[index] = built;
  else list.push(built);
  return list;
}

/* ---------- routing helpers ---------- */

/* 建议的本机路由 ID：连接 + 协议前缀 + 上游 ID 的百分号编码。
   末段决定协议，因此前缀同时是这份契约的自解释形式；用户仍可手改。 */
function routeIdFor(connectionId, upstreamModelId, protocol) {
  const connection = String(connectionId || '').trim();
  const upstream = String(upstreamModelId || '').trim();
  if (!connection || !upstream) return '';
  const family = protocol || modelFamilyFor(upstream) || 'responses';
  const prefix = family === 'chat' ? 'kimi-' : 'resp-';
  return connection + '/' + prefix + encodeURIComponent(upstream);
}

function connectionRowsOf(data) {
  const connections = data && data.connections;
  if (!connections) return null;
  const rows = Array.isArray(connections.saved) ? connections.saved : Array.isArray(connections.active) ? connections.active : [];
  return rows.filter((row) => row && typeof row === 'object');
}

function connectionsAvailable(data) {
  return Array.isArray(connectionRowsOf(data));
}

function findConnection(data, id) {
  const rows = connectionRowsOf(data) || [];
  return rows.find((row) => row && row.id === id) || null;
}

function connectionLabel(row) {
  if (!row) return '—';
  if (row.legacy) return '旧版单上游';
  return row.name || row.id || '—';
}

function connectionsChanged(data) {
  return Boolean(data && data.connections && data.connections.restartRequired);
}

/* 路由与上游 ID 都按原样展示：任何大小写或分隔符改写都会让请求打到不存在的模型。 */
function routingOf(model) {
  if (!model) return { connection: null, connectionId: null, upstream: null, legacy: true };
  const connectionId = typeof model.connectionId === 'string' && model.connectionId ? model.connectionId : null;
  return {
    connectionId,
    connection: connectionId,
    upstream: typeof model.upstreamModelId === 'string' && model.upstreamModelId ? model.upstreamModelId : model.id,
    legacy: !connectionId,
  };
}



/* ---------- render: model catalogue ---------- */

function matchesQuery(model, needle) {
  if (!needle) return true;
  const fields = [model.id, displayModelId(model.id), model.name, model.protocol, ...(model.reasoningEfforts || [])]
    .filter((value) => typeof value === 'string');
  return fields.some((value) => value.toLowerCase().includes(needle));
}

function filterModels() {
  const data = state.data;
  const all = data && Array.isArray(data.models) ? data.models : [];
  const needle = state.query.trim().toLowerCase();
  return {
    all,
    filtered: all
      .filter((model) => model && typeof model.id === 'string')
      .filter((model) => (state.filter === 'vision' ? hasVision(model)
        : state.filter === 'tools' ? isCapable(model)
          : state.filter === 'reasoning' ? model.reasoning === true
            : true))
      .filter((model) => matchesQuery(model, needle)),
  };
}

let modelSignature = '';

function renderModels() {
  const data = state.data;
  const { all, filtered } = filterModels();
  const signature = JSON.stringify([
    filtered.map(canonicalModel),
    state.editor ? state.editor.originalId : null,
    state.deleteId,
    state.modelsSaving,
    configReady(data),
  ]);

  const total = all.filter((model) => model && typeof model.id === 'string').length;
  setText(dom.modelCount, filtered.length + ' / ' + total);

  const empty = filtered.length === 0;
  setHidden(dom.modelEmpty, !empty);
  setHidden(dom.modelTableWrap, empty);
  if (empty) {
    if (!total) {
      setText(dom.modelEmptyTitle, '目录为空');
      setText(dom.modelEmptyNote, '配置中的模型目录没有模型。用「新增模型」添加，或检查配置文件后重启面板。');
      setHidden(dom.modelEmptyReset, true);
    } else if (state.filter !== 'all' && !all.some((model) => (state.filter === 'vision' ? hasVision(model)
      : state.filter === 'tools' ? isCapable(model) : model.reasoning === true))) {
      setText(dom.modelEmptyTitle, '没有符合此筛选的模型');
      setText(dom.modelEmptyNote, '当前目录中没有模型声明该能力。清除筛选可查看全部模型。');
      setHidden(dom.modelEmptyReset, false);
    } else {
      setText(dom.modelEmptyTitle, '没有匹配的模型');
      setText(dom.modelEmptyNote, state.query ? '没有模型匹配「' + truncate(state.query.trim(), 24) + '」。' : '清除筛选以查看全部模型。');
      setHidden(dom.modelEmptyReset, false);
    }
  }

  if (signature === modelSignature) return;
  modelSignature = signature;

  if (!data) {
    if (dom.modelBody) dom.modelBody.replaceChildren();
    return;
  }

  const scrollTop = dom.modelTableWrap ? dom.modelTableWrap.scrollTop : 0;
  const rows = filtered.map((model) => buildModelRow(model));
  if (dom.modelBody) dom.modelBody.replaceChildren(...rows);
  if (dom.modelTableWrap) dom.modelTableWrap.scrollTop = scrollTop;
}

function buildModelRow(model) {
  const tr = document.createElement('tr');
  tr.dataset.modelId = model.id;
  if (state.editor && state.editor.originalId === model.id) tr.dataset.editing = 'true';

  const idCell = document.createElement('td');
  const id = document.createElement('span');
  id.className = 'mono cell-id';
  const shownId = displayModelId(model.id);
  id.textContent = shownId;
  id.setAttribute('title', shownId);
  idCell.append(id);
  if (model.name && model.name !== model.id) {
    const name = document.createElement('span');
    name.className = 'cell-name';
    name.textContent = model.name;
    idCell.append(name);
  }
  /* 路由列把「连接」和「真实上游 ID」放在同一个格里：这两个值一起才说明
     请求实际去了哪里，缺一个都会让用户以为本机路由 ID 就是上游 ID。 */
  const route = document.createElement('td');
  route.className = 'col-route';
  const routing = routingOf(model);
  const upstream = document.createElement('span');
  upstream.className = 'mono cell-id';
  upstream.textContent = routing.upstream || '—';
  upstream.setAttribute('title', routing.upstream || '');
  const connection = document.createElement('span');
  connection.className = 'cell-name';
  connection.textContent = routing.connectionId
    ? connectionLabel(findConnection(state.data, routing.connectionId)) + ' · ' + routing.connectionId
    : '旧版单上游';
  route.append(upstream, connection);

  const proto = document.createElement('td');
  proto.className = 'col-proto';
  const protoTag = document.createElement('span');
  protoTag.className = 'tag mono';
  protoTag.textContent = model.protocol === 'chat' ? 'chat' : 'responses';
  proto.append(protoTag);

  const context = document.createElement('td');
  context.className = 'num mono';
  context.textContent = fmtK(model.contextWindow);
  if (model.contextLimit) {
    const limit = document.createElement('span');
    limit.className = 'cell-name';
    limit.textContent = '上限 ' + fmtK(model.contextLimit);
    context.append(limit);
  } else {
    const limit = document.createElement('span');
    limit.className = 'cell-name';
    limit.textContent = '上限未知';
    context.append(limit);
  }

  const output = document.createElement('td');
  output.className = 'num mono col-out';
  output.textContent = model.maxOutputTokens ? fmtK(model.maxOutputTokens) : '—';

  const caps = document.createElement('td');
  caps.className = 'col-cap';
  const capTags = [];
  const addTag = (text) => {
    const tag = document.createElement('span');
    tag.className = 'tag';
    tag.textContent = text;
    capTags.push(tag);
  };
  if (model.tools) addTag('工具');
  if (model.reasoning) addTag('推理');
  if (hasVision(model)) addTag('视觉');
  if (!capTags.length) {
    const none = document.createElement('span');
    none.className = 'muted';
    none.textContent = '—';
    caps.append(none);
  } else {
    caps.append(...capTags);
  }

  const efforts = document.createElement('td');
  efforts.className = 'col-efforts mono cell-efforts';
  const list = declaredEfforts(model);
  efforts.textContent = list.length ? list.join(', ') : '—';
  if (list.length) efforts.setAttribute('title', list.join(', '));

  const actions = document.createElement('td');
  actions.className = 'col-actions';

  if (state.deleteId === model.id) {
    tr.dataset.confirm = 'true';
    const confirmText = document.createElement('span');
    confirmText.className = 'confirm__text';
    confirmText.textContent = '删除？';
    const yes = document.createElement('button');
    yes.type = 'button';
    yes.className = 'btn btn--danger btn--sm';
    yes.dataset.rowDelete = 'confirm';
    yes.dataset.modelId = model.id;
    yes.textContent = '删除';
    const no = document.createElement('button');
    no.type = 'button';
    no.className = 'btn btn--quiet btn--sm';
    no.dataset.rowDelete = 'cancel';
    no.dataset.modelId = model.id;
    no.textContent = '取消';
    actions.append(confirmText, yes, no);
  } else {
    const edit = document.createElement('button');
    edit.type = 'button';
    edit.className = 'iconbtn';
    edit.dataset.rowEdit = model.id;
    edit.setAttribute('aria-label', '编辑 ' + model.id);
    edit.append(iconUse('i-edit'));
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'iconbtn iconbtn--danger';
    remove.dataset.rowDelete = 'ask';
    remove.dataset.modelId = model.id;
    remove.setAttribute('aria-label', '删除 ' + model.id);
    remove.append(iconUse('i-trash'));
    actions.append(edit, remove);
  }

  tr.append(idCell, route, proto, context, output, caps, efforts, actions);
  return tr;
}

/* ---------- render: candidate picker ---------- */

/* 近似匹配只在本地做，且只用于排序，不改变候选本身：上游 ID 一律按原样展示和保存。 */
function normalizeNeedle(value) {
  return String(value || '').toLowerCase().replace(/[\s\-_./]+/g, '');
}

function subsequenceScore(needle, haystack) {
  if (!needle || !haystack) return 0;
  let index = 0;
  let gaps = 0;
  for (const char of haystack) {
    if (char === needle[index]) index++;
    else if (index > 0) gaps++;
    if (index === needle.length) break;
  }
  if (index < needle.length) return 0;
  return Math.max(1, 100 - gaps);
}

function connectionScore(model, needle, rawNeedle) {
  if (!needle) return 1;
  const id = normalizeNeedle(model && model.id);
  const name = normalizeNeedle(model && model.name);
  /* 原文（保留分隔符）直接包含查询时加权：把 glm5.3 和 glm-5.3 归一化后是同一个
     串，但用户敲的是哪一种写法，说明他更可能指哪一个。 */
  const rawId = String(model && model.id || '').toLowerCase();
  const rawName = String(model && model.name || '').toLowerCase();
  let bonus = 0;
  if (rawNeedle) {
    if (rawId.includes(rawNeedle)) bonus += 60;
    if (rawName.includes(rawNeedle)) bonus += 40;
  }
  if (id === needle) return 1000 + bonus;
  if (id.startsWith(needle)) return 900 + bonus;
  if (id.includes(needle)) return 800 + bonus;
  if (name === needle) return 700 + bonus;
  if (name.startsWith(needle)) return 650 + bonus;
  if (name.includes(needle)) return 600 + bonus;
  const idScore = subsequenceScore(needle, id);
  const nameScore = subsequenceScore(needle, name);
  return Math.max(idScore, nameScore) * 5 + bonus;
}

function rankedCandidates(models, query) {
  const needle = normalizeNeedle(query);
  const rawNeedle = String(query || '').trim().toLowerCase();
  const list = (models || []).filter((model) => model && typeof model.id === 'string');
  if (!needle) return list.slice(0, 60);
  return list
    .map((model) => ({ model, score: connectionScore(model, needle, rawNeedle) }))
    .filter((entry) => entry.score > 0)
    .sort((left, right) => right.score - left.score
      || String(left.model.id).localeCompare(String(right.model.id)))
    .slice(0, 60)
    .map((entry) => entry.model);
}

function renderPicker() {
  const picker = state.picker;
  if (!picker.open || !state.editor) {
    setHidden(dom.picker, true);
    return;
  }
  setHidden(dom.picker, false);
  const draft = state.editor.draft;
  const connection = draft.connectionId ? findConnection(state.data, draft.connectionId) : null;
  setText(dom.pickerLede, connection
    ? '候选来自连接「' + connectionLabel(connection) + '」的供应商目录与最近一次上游读取；'
      + '输入关键词由服务端按别名与片段检索，点选后才会写入模型。'
    : '未选择命名连接：候选来自所选供应商预设的目录。选择一个连接可同时看到它上次读取到的模型。');

  if (dom.pickerQ) dom.pickerQ.value = picker.query;
  if (dom.pickerScan) {
    /* The explicit read only makes sense for a saved connection; the catalog
       itself never depends on it. */
    dom.pickerScan.disabled = picker.busy || !draft.connectionId;
    dom.pickerScan.hidden = !draft.connectionId;
    setText(dom.pickerScan, picker.busy ? '正在读取…' : '读取上游目录');
  }
  if (dom.pickerState) {
    if (picker.busy) setText(dom.pickerState, '正在搜索…');
    else if (picker.reason) setText(dom.pickerState, discoveryReasonText(picker.reason));
    else if (picker.state === 'searched') {
      const count = picker.models ? picker.models.length : 0;
      const parts = [count + ' 个候选'];
      if (picker.checkedAt) parts.push('上游读取于 ' + fmtTime(picker.checkedAt));
      setText(dom.pickerState, parts.join(' · '));
    } else setText(dom.pickerState, '尚未搜索');
  }
  if (dom.pickerNote) {
    /* Truthful about where the list came from: a preset-backed list is the
       vendor's documented catalog, not a live read of this endpoint. */
    const note = picker.state === 'searched' && picker.models && picker.models.length
      ? '列表来自供应商目录' + (picker.checkedAt ? '与最近一次上游读取' : '')
        + '；模型 ID 原样保留，能力与预算在你确认后才写入。'
      : '';
    setHidden(dom.pickerNote, !note);
    setText(dom.pickerNote, note);
  }

  /* The server already ranked matches; the local list is only a bounded view of
     what came back, so no second ranking algorithm can disagree with it. */
  const candidates = (picker.models || []).filter((model) => model && typeof model.id === 'string');
  const empty = candidates.length === 0;
  setHidden(dom.pickerEmpty, !empty);
  setHidden(dom.pickerList, empty);
  if (empty) {
    setText(dom.pickerEmptyTitle, picker.busy ? '正在搜索候选'
      : picker.state === 'searched' ? '没有匹配的候选' : '尚未载入候选');
    setText(dom.pickerEmptyNote, picker.busy
      ? '稍候；供应商目录会先返回，已保存连接的上次读取结果会一并合并。'
      : picker.state === 'searched'
        ? '换一个关键词，或清空搜索查看该连接的全部候选。'
        : '选择一条连接或供应商预设后，候选会自动载入。');
    return;
  }
  if (dom.pickerList) {
    const signature = JSON.stringify(candidates.map((model) => model.id));
    if (dom.pickerList.dataset.signature !== signature) {
      dom.pickerList.dataset.signature = signature;
      dom.pickerList.replaceChildren(...candidates.map(buildCandidateRow));
    }
  }
}

function buildCandidateRow(model) {
  const li = document.createElement('li');
  li.className = 'candidate';
  const button = document.createElement('button');
  button.type = 'button';
  button.className = 'candidate__btn';
  button.dataset.candidateId = model.id;

  const head = document.createElement('span');
  head.className = 'candidate__head';
  const id = document.createElement('span');
  id.className = 'candidate__id mono';
  id.textContent = model.id;
  head.append(id);
  if (model.name && model.name !== model.id) {
    const name = document.createElement('span');
    name.className = 'candidate__name';
    name.textContent = model.name;
    head.append(name);
  }

  const facts = document.createElement('span');
  facts.className = 'candidate__facts';
  const parts = [];
  if (model.contextWindow) parts.push('上下文 ' + fmtK(model.contextWindow));
  if (model.maxOutputTokens) parts.push('输出 ' + fmtK(model.maxOutputTokens));
  if (model.tools === true) parts.push('工具');
  if (model.reasoning === true) parts.push('推理');
  if (Array.isArray(model.inputModalities) && model.inputModalities.includes('image')) parts.push('视觉');
  if (Array.isArray(model.protocols) && model.protocols.length) parts.push(model.protocols.map(protocolLabel).join('/'));
  facts.textContent = parts.join(' · ') || '上游未提供能力信息';

  button.append(head, facts);
  li.append(button);
  return li;
}

/* 协议状态必须区分三种情况，M29 的 parser 已经把其中两种解析清楚了：
   1. 数组（一个或多个）：上游/preset 的真实声明，多值时默认偏好 Responses；
   2. 显式空数组：端点明确声明不支持任何协议（`supported:false` 或空列表），
      这是权威判定，UI 不得回退到 preset 再猜一次；
   3. 字段缺失：parser 已按 M29 合同继承已验证的 preset 协议，缺失只表示
      「这条公开行没带」，仍按未知处理并要求用户显式选择。
   把 1 与 2、3 混为一谈会推翻 M29 三轮修正。 */
function candidateProtocolState(model) {
  if (!model || !Array.isArray(model.protocols)) return { kind: 'absent', list: [] };
  const list = model.protocols.filter((value) => value === 'chat' || value === 'responses');
  return list.length ? { kind: 'declared', list } : { kind: 'explicit-empty', list: [] };
}

/* 保留旧名，返回声明过的协议（空/缺失都返回 null），调用方需区分时用上面的状态。 */
function candidateProtocols(model) {
  const state = candidateProtocolState(model);
  return state.kind === 'declared' ? state.list : null;
}

/* 多协议时默认 Responses：与网关默认族一致，用户仍可改选。 */
function preferredProtocol(protocols) {
  if (!protocols || !protocols.length) return null;
  return protocols.includes('responses') ? 'responses' : protocols[0];
}

function pickCandidate(upstreamId) {
  const editor = state.editor;
  if (!editor || !upstreamId) return;
  const model = (state.picker.models || []).find((row) => row && row.id === upstreamId) || null;
  const draft = editor.draft;
  const protocolState = candidateProtocolState(model);
  draft.upstreamModelId = upstreamId;
  draft.name = model && model.name ? model.name : draft.name;

  const preset = providerPreset(draft.providerPresetId) || providerPreset(
    (findConnection(state.data, draft.connectionId) || {}).providerPresetId);
  const presetProtocols = preset && Array.isArray(preset.protocols) && preset.protocols.length ? preset.protocols : null;
  /* 只有「字段缺失」才允许沿用 preset；显式空数组是权威的「不支持」，不 fallback。 */
  const chosen = protocolState.kind === 'declared'
    ? preferredProtocol(protocolState.list)
    : protocolState.kind === 'absent'
      ? preferredProtocol(presetProtocols)
      : null;
  draft.protocolExplicitEmpty = protocolState.kind === 'explicit-empty';

  if (chosen) {
    /* 声明过协议（一个或多个）就自动生成路由；多协议按偏好选一个，
       用户可通过协议按钮或直接改路由 ID 覆盖。 */
    const declared = protocolState.kind === 'declared';
    draft.id = routeIdFor(draft.connectionId, upstreamId, chosen);
    draft.protocolConfirmed = true;
    draft.protocolSource = declared ? 'discovery' : 'preset';
    draft.protocolOptions = declared ? protocolState.list : presetProtocols;
    draft.protocolHint = declared && protocolState.list.length > 1
      ? '上游声明 ' + protocolState.list.map(protocolLabel).join(' 与 ') + '，已按 ' + protocolLabel(chosen)
        + ' 生成路由；可切换协议。'
      : declared ? null
        : '上游目录没有声明协议，当前按预设「' + (preset ? preset.name || preset.id : '') + '」的 '
          + protocolLabel(chosen) + ' 填写；请确认后再保存。';
  } else {
    draft.id = '';
    draft.protocolConfirmed = false;
    draft.protocolSource = null;
    draft.protocolOptions = null;
    /* 显式空与缺失要用不同的话说清楚，否则用户会以为只是「目录没写」。 */
    draft.protocolHint = protocolState.kind === 'explicit-empty'
      ? '该端点明确声明不支持 Chat 或 Responses 协议，因此没有生成路由。'
        + '请确认上游能力后手动填写，或改用其他连接。'
      : '上游目录没有声明协议，因此没有自动生成路由 ID。'
        + '请选择协议或手动填写路由 ID；协议决定调用 Chat 还是 Responses 接口。';
  }
  if (model) {
    if (model.contextWindow) draft.context = String(model.contextWindow);
    draft.contextLimit = model.contextWindow ? String(model.contextWindow) : '';
    if (typeof model.tools === 'boolean') draft.tools = model.tools;
    if (typeof model.reasoning === 'boolean') draft.reasoning = model.reasoning;
    if (Array.isArray(model.inputModalities)) draft.vision = model.inputModalities.includes('image');
    if (model.maxOutputTokens) draft.output = String(model.maxOutputTokens);
    if (Array.isArray(model.reasoningEfforts) && model.reasoningEfforts.length) {
      draft.efforts = model.reasoningEfforts.join(', ');
    }
    /* 来源必须是这条候选自己的，不能一律写成 discovery：model-search 会把
       静态 preset 与最近一次成功 discovery 合并，两者的来源不同。优先用后端
       给出的 model.metadataSource / model.source，都没有时按「这次读取有没有
       真实目录」判断，仍无法确定就标 manual 而不是编造 discovery。 */
    draft.metadataSource = candidateMetadataSource(model);
  }
  editor.serverErrors = [];
  editor.budgetNote = null;
  if (!chosen && dom.mAdvanced) dom.mAdvanced.open = true;
  fillEditorFields();
  state.picker.open = false;
  renderPicker();
  renderEditor();
  renderModels();
  const focusTarget = chosen ? dom.mName : dom.mId;
  if (focusTarget && focusTarget.focus) focusTarget.focus();
}

/* 显式切换协议：只重写路由 ID 的家族前缀，上游 ID 原样保留。
   手写路由 ID 仍是高级覆盖手段，不是多协议的必经步骤。 */
function chooseProtocol(protocol) {
  const editor = state.editor;
  if (!editor || !['chat', 'responses'].includes(protocol)) return;
  const draft = editor.draft;
  const upstream = String(draft.upstreamModelId || '').trim();
  draft.protocolSource = 'manual';
  draft.protocolConfirmed = true;
  if (upstream && draft.connectionId) {
    draft.id = routeIdFor(draft.connectionId, upstream, protocol);
    draft.protocolHint = '已按 ' + protocolLabel(protocol) + ' 生成路由；上游模型 ID 保持不变。';
    if (dom.mId) dom.mId.value = draft.id;
  } else {
    draft.protocolHint = upstream
      ? '请先选择所属连接，再由协议生成路由。'
      : '请先填写上游模型 ID，再由协议生成路由。';
  }
  renderEditor();
  renderModels();
}

/* ---------- candidate search ---------- */

/* Picking a provider must not require a working `GET /models`. The unified
   search endpoint answers from the shipped preset for that connection and adds
   the last successful discovery, so an unsaved or offline upstream still shows
   its documented catalog. `runDiscovery` stays the explicit "read upstream now"
   action and never replaces what the search returns. */

const PICKER_DEBOUNCE_MS = 250;

function pickerScope() {
  const draft = state.editor && state.editor.draft;
  return draft ? String(draft.connectionId || '').trim() : '';
}

/* One in-flight request per picker: a fast connection or keyword change must not
   let an older answer land on newer state. */
async function searchCandidates(query, { connectionId = pickerScope(), keepReason = false } = {}) {
  const picker = state.picker;
  const seq = ++picker.seq;
  picker.busy = true;
  if (!keepReason) picker.reason = null;
  renderPicker();
  try {
    const body = { query: String(query || '') };
    if (connectionId) body.connectionId = connectionId;
    else if (state.editor && state.editor.draft.providerPresetId) {
      body.presetId = state.editor.draft.providerPresetId;
    }
    const res = await request(API.modelSearch, { method: 'POST', body });
    if (seq !== state.picker.seq) return null;
    const models = res && Array.isArray(res.models) ? res.models : [];
    picker.models = models;
    picker.state = 'searched';
    picker.hint = models.length ? null : 'no-match';
    picker.reason = null;
    return res;
  } catch (error) {
    if (seq !== state.picker.seq) return null;
    picker.reason = error.code || 'model_search_failed';
    picker.state = 'failed';
    return null;
  } finally {
    if (seq === state.picker.seq) {
      picker.busy = false;
      renderPicker();
    }
  }
}

let pickerTimer = null;
function scheduleSearch() {
  const picker = state.picker;
  if (pickerTimer) clearTimeout(pickerTimer);
  const query = picker.query;
  pickerTimer = setTimeout(() => {
    pickerTimer = null;
    if (!state.picker.open) return;
    void searchCandidates(query);
  }, PICKER_DEBOUNCE_MS);
}

/* 行内连接检查：按行携带该行的 connectionId，结果也按 connectionId 认领。
   后端绝不隐式挑选连接；这里同样只检查用户点的那一行。 */
async function runConnectionCheck(connectionId) {
  const data = state.data;
  if (!data || !connectionId) return null;
  if (state.pendingAction) {
    showToast('已有操作正在进行，等待它结束后再试。', 'error');
    return null;
  }
  state.connChecks = { ...state.connChecks, [connectionId]: { ...(state.connChecks[connectionId] || {}), busy: true } };
  renderConnections();
  const id = actionUUID();
  try {
    const res = await request(API.actions, {
      method: 'POST',
      body: { id, at: Date.now(), action: 'check-upstream', connectionId },
    });
    const op = res && res.operation ? res.operation : null;
    if (op && typeof op.connectionId === 'string' && op.connectionId !== connectionId) {
      showToast('检查结果与所在连接不符，已忽略。', 'error');
      return null;
    }
    const diag = (state.data && state.data.diagnostics && state.data.diagnostics.upstream) || {};
    const result = op && op.state ? op
      : diag && diag.connectionId === connectionId ? diag : null;
    const recorded = result ? { state: result.state,
      ...(Number.isInteger(result.httpStatus) ? { httpStatus: result.httpStatus } : {}),
      ...(Number.isInteger(result.modelCount) ? { modelCount: result.modelCount } : {}),
      ...(Number.isFinite(result.ms) ? { ms: result.ms } : {}),
      ...(result.reason ? { reason: result.reason } : {}), connectionId } : { state: 'interrupted', connectionId };
    state.connChecks = { ...state.connChecks, [connectionId]: recorded };
    if (result && result.state === 'passed') showToast('连接「' + connectionId + '」可用。', 'ok');
    else showToast('连接「' + connectionId + '」检查未通过：'
      + (upstreamReasonText(result && result.reason) || '请稍后再试') + '。', 'error');
    await loadState({ silent: true });
    return { ...state.connChecks[connectionId], busy: false };
  } catch (error) {
    state.connChecks = { ...state.connChecks, [connectionId]: { state: 'failed', reason: error.code || null, connectionId } };
    showToast(error.message || '连接检查未完成，请稍后再试。', 'error');
    return null;
  } finally {
    const current = state.connChecks[connectionId];
    if (current) {
      state.connChecks = { ...state.connChecks, [connectionId]: { ...current, busy: false } };
    }
    renderConnections();
  }
}

/* The explicit read is still available for a saved connection. A pass proves the
   endpoint answers; a failure keeps the preset-backed search usable. */
async function runDiscovery(connectionId) {
  if (!connectionId) {
    showToast('请选择一条已保存的连接。', 'error');
    return null;
  }
  const data = state.data;
  if (!data) return null;
  const seq = ++state.picker.seq;
  state.picker.busy = true;
  state.picker.reason = null;
  renderPicker();
  try {
    const res = await request(API.discovery, { method: 'POST', body: { revision: data.revision, connectionId } });
    const discovery = res && res.discovery;
    if (seq !== state.picker.seq) return null;
    if (!discovery || !Array.isArray(discovery.models)) {
      showToast('上游目录读取结果格式不被接受。', 'error');
      return null;
    }
    state.picker.checkedAt = discovery.checkedAt || null;
    state.picker.source = discovery.sourceURL || null;
    if (discovery.state !== 'passed') {
      /* A failed read is reported, not turned into an empty catalog: whatever the
         preset-backed search already showed stays on screen. */
      state.picker.reason = discovery.reason || null;
      showToast(discoveryReasonText(discovery.reason) || '读取上游目录失败。', 'error');
      return discovery;
    }
    showToast('读取到 ' + discovery.modelCount + ' 个候选模型。', 'ok');
    return discovery;
  } catch (error) {
    if (seq !== state.picker.seq) return null;
    state.picker.reason = error.code || null;
    showToast(discoveryReasonText(error.code) || error.message || '读取上游目录失败。', 'error');
    return null;
  } finally {
    if (seq === state.picker.seq) {
      state.picker.busy = false;
      renderPicker();
    }
    /* Either way the catalog now comes from the unified search, which merges the
       fresh discovery with the preset and keeps aliases and provenance. */
    if (state.picker.open) await searchCandidates(state.picker.query);
  }
}

function openPicker() {
  const editor = state.editor;
  if (!editor) return;
  state.picker.open = true;
  state.picker.query = '';
  state.picker.models = null;
  state.picker.state = null;
  state.picker.reason = null;
  state.picker.hint = null;
  state.picker.scope = pickerScope();
  renderPicker();
  if (dom.pickerQ && dom.pickerQ.focus) dom.pickerQ.focus();
  /* Opening the picker is the moment the catalog should be there: the preset
     answers immediately, and a saved connection's last discovery is merged in. */
  void searchCandidates('', { connectionId: state.picker.scope });
}

function closePicker() {
  state.picker.open = false;
  renderPicker();
}


/* ---------- render: editor ---------- */

/* 定价与故障转移是模型行上的两个可选对象。草稿必须原样带上它们：少了任何
   一项，一次与它们无关的编辑保存就会把已保存的值抹掉。 */
const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];

function priceDraftFrom(pricing) {
  const draft = {};
  for (const field of PRICE_FIELDS) {
    const value = pricing && typeof pricing[field] === 'number' && Number.isFinite(pricing[field]) && pricing[field] >= 0
      ? pricing[field] : null;
    draft[field] = value === null ? '' : String(value);
  }
  return draft;
}

function failoverDraftFrom(failover) {
  if (!Array.isArray(failover)) return [];
  return failover.slice(0, 8).filter((row) => row && typeof row === 'object').map((row) => ({
    connectionId: typeof row.connectionId === 'string' ? row.connectionId : '',
    upstreamModelId: typeof row.upstreamModelId === 'string' ? row.upstreamModelId : '',
    upstreamProtocol: row.upstreamProtocol === 'anthropic' ? 'anthropic' : '',
  }));
}

function modelDraftFrom(model) {
  const draft = model ? {
    id: model.id,
    name: model.name || '',
    context: String(model.contextWindow || ''),
    output: model.maxOutputTokens ? String(model.maxOutputTokens) : '',
    tools: typeof model.tools === 'boolean' ? model.tools : undefined,
    reasoning: model.reasoning === true,
    vision: hasVision(model),
    efforts: (model.reasoningEfforts || []).slice(),
    defaultEffort: model.defaultEffort || '',
    connectionId: model.connectionId || '',
    upstreamModelId: model.upstreamModelId || '',
    upstreamProtocol: model.upstreamProtocol || '',
    upstreamProtocolChoices: upstreamProtocolSupported(state.data) ? ['', 'anthropic'] : null,
    contextLimit: model.contextLimit ? String(model.contextLimit) : '',
    metadataSource: model.metadataSource || null,
    pricing: priceDraftFrom(model.pricing),
    failover: failoverDraftFrom(model.failover),
  } : {
    id: '', name: '', context: '', output: '', tools: undefined, reasoning: false, vision: false,
    efforts: [], defaultEffort: '', connectionId: defaultConnectionId(), upstreamModelId: '', upstreamProtocol: '',
    upstreamProtocolChoices: upstreamProtocolSupported(state.data) ? ['', 'anthropic'] : null,
    contextLimit: '', metadataSource: null,
    pricing: priceDraftFrom(null), failover: [],
  };
  draft.baseEntity = model ? { ...model, inputModalities: model.inputModalities?.slice(),
    reasoningEfforts: model.reasoningEfforts?.slice(), pricing: model.pricing || null,
    failover: failoverDraftFrom(model.failover) } : null;
  draft.touched = new Set();
  return draft;
}

function openEditor(model) {
  state.deleteId = null;
  if (dom.mAdvanced) dom.mAdvanced.open = false;
  state.editor = {
    originalId: model ? model.id : null,
    baseRevision: state.data?.revision ?? null,
    confirmClose: false,
    conflict: null,
    serverErrors: [],
    draft: modelDraftFrom(model),
  };
  fillEditorFields();
  renderEditor();
  renderModels();
  const focusTarget = state.editor.originalId ? dom.mName : dom.mId;
  if (focusTarget && focusTarget.focus) focusTarget.focus();
}

function defaultConnectionId() {
  const rows = (connectionRowsOf(state.data) || []).filter((row) => !row.legacy);
  return rows.length ? rows[0].id : '';
}

function closeEditor(force) {
  if (!state.editor) return;
  if (!force && editorDirty() && !state.editor.confirmClose) {
    state.editor.confirmClose = true;
    renderEditor();
    return;
  }
  state.editor = null;
  state.picker = { open: false, models: null, query: '', state: null, checkedAt: null, source: null, reason: null,
    busy: false, seq: 0, hint: null, scope: null };
  renderEditor();
  renderPicker();
  renderModels();
}

function fillEditorFields() {
  const draft = state.editor && state.editor.draft;
  if (!draft) return;
  if (dom.mId) dom.mId.value = draft.id;
  if (dom.mName) dom.mName.value = draft.name;
  if (dom.mUpstream) dom.mUpstream.value = draft.upstreamModelId || '';
  if (dom.mContext) dom.mContext.value = draft.context;
  if (dom.mOutput) dom.mOutput.value = draft.output;
  if (dom.mTools) dom.mTools.checked = draft.tools;
  if (dom.mReasoning) dom.mReasoning.checked = draft.reasoning;
  if (dom.mVision) dom.mVision.checked = draft.vision;
  if (dom.mEfforts) dom.mEfforts.value = draft.efforts.join(', ');
  const price = draft.pricing || priceDraftFrom(null);
  if (dom.mPriceInput) dom.mPriceInput.value = price.inputPerMillion;
  if (dom.mPriceOutput) dom.mPriceOutput.value = price.outputPerMillion;
  if (dom.mPriceCached) dom.mPriceCached.value = price.cachedInputPerMillion;
  if (dom.mPriceWrite) dom.mPriceWrite.value = price.cacheWritePerMillion;
}

function readEditorFields() {
  const draft = state.editor && state.editor.draft;
  if (!draft) return;
  draft.id = dom.mId ? dom.mId.value : draft.id;
  /* 手写路由 ID 就是用户显式选择了协议：末段决定 Chat 还是 Responses。 */
  if (draft.protocolConfirmed === false && String(draft.id || '').trim()) draft.protocolConfirmed = true;
  draft.name = dom.mName ? dom.mName.value : draft.name;
  draft.upstreamModelId = dom.mUpstream ? dom.mUpstream.value : draft.upstreamModelId;
  draft.context = dom.mContext ? dom.mContext.value : draft.context;
  draft.output = dom.mOutput ? dom.mOutput.value : draft.output;
  /* 工具能力是三态：勾上=声明支持，取消勾选=明确不支持，从未表态=未知。
     这里只在用户真的改过时落成布尔值——把一个从未表态的勾选框读成 false，
     就等于替上游声明它不支持工具调用。 */
  if (dom.mTools) {
    draft.tools = dom.mTools.checked === true ? true : (draft.tools === true ? false : draft.tools);
  }
  draft.reasoning = Boolean(dom.mReasoning && dom.mReasoning.checked);
  draft.vision = Boolean(dom.mVision && dom.mVision.checked);
  draft.efforts = dom.mEfforts ? parseEfforts(dom.mEfforts.value).list : draft.efforts;
  draft.defaultEffort = dom.mDefault ? dom.mDefault.value : draft.defaultEffort;
  if (dom.mPriceInput) draft.pricing.inputPerMillion = dom.mPriceInput.value;
  if (dom.mPriceOutput) draft.pricing.outputPerMillion = dom.mPriceOutput.value;
  if (dom.mPriceCached) draft.pricing.cachedInputPerMillion = dom.mPriceCached.value;
  if (dom.mPriceWrite) draft.pricing.cacheWritePerMillion = dom.mPriceWrite.value;
  buildModel(draft);
}

function editorDirty() {
  const editor = state.editor;
  if (!editor) return false;
  if (!editor.originalId) {
    const draft = editor.draft;
    return Boolean(draft.id || draft.name || draft.context || draft.output || draft.efforts.length || draft.reasoning
      || draft.vision || draft.upstreamModelId || draft.upstreamProtocol || typeof draft.tools === 'boolean'
      || (draft.failover && draft.failover.length)
      || PRICE_FIELDS.some((field) => draft.pricing && draft.pricing[field]));
  }
  const built = buildModel(editor.draft);
  return canonicalModel(built.candidate) !== canonicalModel(editor.draft.baseEntity);
}

const MODEL_DIFF_FIELDS = {
  id: '模型 ID', name: '名称', contextWindow: '上下文窗口', maxOutputTokens: '最大输出', tools: '工具能力',
  reasoning: '思考能力', inputModalities: '图片输入', reasoningEfforts: '思考档位', defaultEffort: '默认档位',
  connectionId: '所属连接', upstreamModelId: '上游模型 ID', upstreamProtocol: '上游 API 格式',
  contextLimit: '上游上下文上限', metadataSource: '元数据来源',
  pricing: '定价', failover: '备用连接',
};
const diffValue = value => value === undefined ? '（未声明）' : JSON.stringify(value);
function modelThreeWayDiff(editor) {
  const base = editor.draft.baseEntity;
  const live = findModel(editor.originalId);
  const draft = buildModel(editor.draft).candidate;
  return Object.entries(MODEL_DIFF_FIELDS).filter(([field]) =>
    JSON.stringify(base?.[field]) !== JSON.stringify(live?.[field])
    || JSON.stringify(base?.[field]) !== JSON.stringify(draft?.[field]))
    .map(([field, label]) => ({ field, label, base: base?.[field], live: live?.[field], draft: draft?.[field] }));
}

function rebaseEditor() {
  const editor = state.editor;
  if (!editor?.conflict || !state.data) return;
  const live = findModel(editor.originalId);
  if (!live) {
    editor.serverErrors = ['原模型已在别处删除，不能重新应用；请关闭编辑器后新增模型。'];
    editor.conflict = null;
    renderEditor();
    return;
  }
  const previous = editor.draft;
  const touched = buildModel(previous).touched;
  const candidate = buildModel(previous).candidate;
  const next = modelDraftFrom(live);
  const assign = {
    id: () => { next.id = candidate.id; },
    name: () => { next.name = previous.name; },
    contextWindow: () => { next.context = previous.context; },
    maxOutputTokens: () => { next.output = previous.output; },
    tools: () => { next.tools = previous.tools; },
    reasoning: () => { next.reasoning = previous.reasoning; },
    inputModalities: () => { next.vision = previous.vision; },
    reasoningEfforts: () => { next.efforts = Array.isArray(previous.efforts) ? previous.efforts.slice() : parseEfforts(previous.efforts).list; },
    defaultEffort: () => { next.defaultEffort = previous.defaultEffort; },
    connectionId: () => { next.connectionId = previous.connectionId; },
    upstreamModelId: () => { next.upstreamModelId = previous.upstreamModelId; },
    upstreamProtocol: () => { next.upstreamProtocol = previous.upstreamProtocol; },
    contextLimit: () => { next.contextLimit = previous.contextLimit; },
    metadataSource: () => { next.metadataSource = previous.metadataSource; },
    pricing: () => { next.pricing = { ...previous.pricing }; },
    failover: () => { next.failover = failoverDraftFrom(previous.failover); },
  };
  for (const field of touched) assign[field]?.();
  editor.draft = next;
  editor.baseRevision = state.data.revision ?? null;
  editor.conflict = null;
  editor.serverErrors = [];
  buildModel(next);
  fillEditorFields();
  renderEditor();
  showToast('已按最新配置保留你的修改，请核对后再次保存。', 'ok');
}

function validateEditor(editor) {
  const { model, errors } = buildModel(editor.draft);
  const models = (state.data && state.data.models) || [];
  const duplicate = models.some((row) => row.id === model.id && row.id !== editor.originalId);
  if (duplicate) errors.push('已存在相同 ID 的模型。');
  /* 连接若限制了协议，这里提前说明：服务端同样会拒绝，避免用户以为只是保存慢。 */
  const connection = model.connectionId ? findConnection(state.data, model.connectionId) : null;
  if (model.connectionId && !connection) errors.push('所选连接不在已保存的连接列表中。');
  if (connection && Array.isArray(connection.protocols) && connection.protocols.length
    && !connection.protocols.includes(model.protocol)) {
    errors.push('连接「' + connectionLabel(connection) + '」只允许 ' + connection.protocols.map(protocolLabel).join('/')
      + '，当前模型路由 ID 会被解析为 ' + protocolLabel(model.protocol) + '。');
  }
  return { model, errors };
}

/* 上下文窗口只做三种可信换算：固定预设、已知上限、手填。
   上限未知时「完整」不可用，避免把猜测当成上游真实能力。 */
function budgetStateFor(draft) {
  const contextLimit = Number(draft && draft.contextLimit) > 0 ? Number(draft.contextLimit) : null;
  const current = Number(draft && draft.context);
  return {
    contextLimit,
    current: Number.isSafeInteger(current) && current > 0 ? current : null,
    full: contextLimit,
  };
}

function applyBudgetPreset(presetId) {
  const editor = state.editor;
  if (!editor) return;
  readEditorFields();
  const budget = budgetStateFor(editor.draft);
  const preset = BUDGET_PRESETS.find((entry) => entry.id === presetId);
  if (!preset) return;
  if (preset.id === 'full') {
    if (!budget.full) {
      editor.budgetNote = '未知上游上限，无法选择「完整」。请先填写已知的上限或手动输入预算。';
      renderEditor();
      return;
    }
    editor.draft.context = String(budget.full);
    editor.budgetNote = null;
  } else {
    if (budget.contextLimit && preset.tokens > budget.contextLimit) {
      editor.budgetNote = preset.label + ' 超过该模型已知上限 ' + fmtTokens(budget.contextLimit) + '，不能选择。';
      renderEditor();
      return;
    }
    editor.draft.context = String(preset.tokens);
    editor.budgetNote = null;
  }
  if (dom.mContext) dom.mContext.value = editor.draft.context;
  renderEditor();
}


/* 思考档位的常用选择。上游目录声明过其它标识符时，它们以「自定义」标签保留，
   不被静默丢弃；预设按钮只覆盖常见档位。 */
const STANDARD_EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

/* 档位选择是编辑器里唯一的思考控件：点选即写入草稿，「关闭」清空档位并保留
   「支持思考档位」的声明。已保存的自定义档位仍在 chips 里可见、可点掉。 */
function renderEffortChoices(list) {
  if (!dom.mEffortGroup) return;
  Array.from(dom.mEffortGroup.querySelectorAll('[data-effort-choice]')).forEach((button) => {
    const value = button.dataset.effortChoice;
    const active = value === 'off' ? list.length === 0 : list.includes(value);
    button.setAttribute('aria-pressed', active ? 'true' : 'false');
    button.disabled = state.modelsSaving;
  });
}

/* 备用连接行只渲染选择的连接与两项可选覆盖：服务商模型 ID、上游格式。行自己
   不复制主模型的能力，因为故障转移后仍然是同一个本地模型在服务调用方。 */
function renderFailoverRows(editor) {
  if (!dom.mFailoverRows) return;
  const draft = editor.draft;
  const rows = Array.isArray(draft.failover) ? draft.failover : [];
  const connections = (connectionRowsOf(state.data) || [])
    .filter((row) => row && !row.legacy && row.id !== draft.connectionId);
  const connectionOptions = [{ value: '', label: '选择连接' }].concat(connections.map((connection) => ({
    value: connection.id,
    label: connection.name && connection.name !== connection.id
      ? connection.name + ' · ' + connection.id : connection.id,
  })));
  const signature = JSON.stringify([rows.length, connectionOptions]);
  const existing = Array.from(dom.mFailoverRows.children || []);
  const active = typeof document !== 'undefined' ? document.activeElement : null;

  if (dom.mFailoverRows.dataset.signature === signature && existing.length === rows.length) {
    rows.forEach((row, index) => {
      const li = existing[index];
      const select = li.querySelector('[data-failover-field="connectionId"]');
      if (select) {
        select.disabled = state.modelsSaving;
        const nextConn = row.connectionId || '';
        if (active !== select && select.value !== nextConn) select.value = nextConn;
      }
      const input = li.querySelector('[data-failover-field="upstreamModelId"]');
      if (input) {
        input.disabled = state.modelsSaving;
        const nextModel = row.upstreamModelId || '';
        if (active !== input && input.value !== nextModel) input.value = nextModel;
      }
      const protocolSelect = li.querySelector('[data-failover-field="upstreamProtocol"]');
      if (protocolSelect) {
        protocolSelect.disabled = state.modelsSaving;
        const nextProto = row.upstreamProtocol === 'anthropic' ? 'anthropic' : '';
        if (active !== protocolSelect && protocolSelect.value !== nextProto) protocolSelect.value = nextProto;
      }
      const remove = li.querySelector('[data-failover-remove]');
      if (remove) remove.disabled = state.modelsSaving;
    });
  } else {
    let focusIndex = null;
    let focusField = null;
    let selStart = null;
    let selEnd = null;
    if (active && dom.mFailoverRows.contains && dom.mFailoverRows.contains(active)) {
      const host = active.closest ? active.closest('[data-failover-index]') : null;
      if (host) focusIndex = Number(host.dataset.failoverIndex);
      if (active.dataset && active.dataset.failoverField) focusField = active.dataset.failoverField;
      if (typeof active.selectionStart === 'number' && typeof active.selectionEnd === 'number') {
        selStart = active.selectionStart;
        selEnd = active.selectionEnd;
      }
    }

    const nodes = rows.map((row, index) => {
      const li = document.createElement('div');
      li.className = 'failover-row';
      li.dataset.failoverIndex = String(index);

      const id = document.createElement('label');
      id.className = 'field';
      const idLabel = document.createElement('span');
      idLabel.className = 'field__label';
      idLabel.textContent = '备用 ' + (index + 1);
      const select = document.createElement('select');
      select.dataset.failoverField = 'connectionId';
      connectionOptions.forEach((choice) => {
        const option = document.createElement('option');
        option.value = choice.value;
        option.textContent = choice.label;
        select.append(option);
      });
      select.value = row.connectionId || '';
      select.disabled = state.modelsSaving;
      id.append(idLabel, select);

      const upstream = document.createElement('label');
      upstream.className = 'field';
      const upstreamLabel = document.createElement('span');
      upstreamLabel.className = 'field__label';
      upstreamLabel.textContent = '服务商模型 ID（可选）';
      const input = document.createElement('input');
      input.className = 'mono';
      input.dataset.failoverField = 'upstreamModelId';
      input.autocomplete = 'off';
      input.spellcheck = false;
      input.placeholder = '与该连接上的模型相同';
      input.value = row.upstreamModelId || '';
      input.disabled = state.modelsSaving;
      upstream.append(upstreamLabel, input);

      const protocol = document.createElement('label');
      protocol.className = 'field';
      const protocolLabelNode = document.createElement('span');
      protocolLabelNode.className = 'field__label';
      protocolLabelNode.textContent = '上游格式（可选）';
      const protocolSelect = document.createElement('select');
      protocolSelect.dataset.failoverField = 'upstreamProtocol';
      for (const [value, label] of [['', '与主连接相同'], ['anthropic', 'Anthropic Messages']]) {
        const option = document.createElement('option');
        option.value = value;
        option.textContent = label;
        protocolSelect.append(option);
      }
      protocolSelect.value = row.upstreamProtocol === 'anthropic' ? 'anthropic' : '';
      protocolSelect.disabled = state.modelsSaving;
      protocol.append(protocolLabelNode, protocolSelect);

      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'iconbtn';
      remove.dataset.failoverRemove = String(index);
      remove.setAttribute('aria-label', '移除第 ' + (index + 1) + ' 条备用连接');
      remove.disabled = state.modelsSaving;
      remove.append(iconUse('i-x'));

      li.append(id, upstream, protocol, remove);
      return li;
    });
    dom.mFailoverRows.dataset.signature = signature;
    dom.mFailoverRows.replaceChildren(...nodes);
    if (focusField && Number.isInteger(focusIndex) && focusIndex >= 0 && focusIndex < rows.length) {
      const target = dom.mFailoverRows.querySelector(
        '[data-failover-index="' + focusIndex + '"] [data-failover-field="' + focusField + '"]',
      );
      if (target && target.focus) {
        target.focus({ preventScroll: true });
        if (selStart !== null && selEnd !== null && target.setSelectionRange) {
          target.setSelectionRange(selStart, selEnd);
        }
      }
    }
  }
  if (dom.mFailoverCount) dom.mFailoverCount.textContent = rows.length + ' / 8';
  if (dom.mFailoverAdd) dom.mFailoverAdd.disabled = rows.length >= 8 || state.modelsSaving;
}

function toggleEffortChoice(value) {
  const editor = state.editor;
  if (!editor || state.modelsSaving) return;
  if (value === 'off') {
    editor.draft.efforts = [];
    editor.draft.defaultEffort = '';
  } else {
    const list = editor.draft.efforts.slice();
    const index = list.indexOf(value);
    if (index >= 0) list.splice(index, 1);
    else list.push(value);
    editor.draft.efforts = list;
    if (editor.draft.defaultEffort && !list.includes(editor.draft.defaultEffort)) editor.draft.defaultEffort = '';
  }
  if (dom.mEfforts) dom.mEfforts.value = editor.draft.efforts.join(', ');
  readEditorFields();
  renderEditor();
}

/* 是否显示协议编辑由服务端显式能力声明决定，不能从现有目录反推。 */
function upstreamProtocolSupported(data) {
  return data?.capabilities?.modelEditor?.upstreamProtocol === true;
}

function renderUpstreamProtocol(editor) {
  if (!dom.mUpstreamProtocolField) return;
  const choices = editor.draft.upstreamProtocolChoices;
  setHidden(dom.mUpstreamProtocolField, !Array.isArray(choices) || !choices.includes('anthropic'));
  if (!Array.isArray(choices) || !choices.includes('anthropic')) return;
  const current = editor.draft.upstreamProtocol || '';
  Array.from(dom.mUpstreamProtocolGroup.querySelectorAll('[data-upstream-protocol]')).forEach((button) => {
    button.setAttribute('aria-pressed', (button.dataset.upstreamProtocol || '') === current ? 'true' : 'false');
    button.disabled = state.modelsSaving;
  });
}

function chooseUpstreamProtocol(value) {
  const editor = state.editor;
  if (!editor || state.modelsSaving) return;
  if (value !== '' && value !== 'anthropic') return;
  editor.draft.upstreamProtocol = value;
  renderEditor();
}

function renderEditor() {
  const editor = state.editor;
  if (!editor) {
    setHidden(dom.inspector, true);
    if (dom.workbench) dom.workbench.dataset.open = 'false';
    return;
  }
  setHidden(dom.inspector, false);
  if (dom.workbench) dom.workbench.dataset.open = 'true';
  setText(dom.inspectorTitle, editor.originalId ? '编辑模型' : '新增模型');

  const id = String(editor.draft.id || '').trim();
  const family = modelFamilyFor(id);
  const protoButtons = dom.mProtocolGroup ? Array.from(dom.mProtocolGroup.querySelectorAll('[data-protocol]')) : [];
  protoButtons.forEach((button) => {
    const active = button.dataset.protocol === family;
    button.setAttribute('aria-pressed', active ? 'true' : 'false');
    const allowed = !editor.draft.protocolOptions
      || editor.draft.protocolOptions.includes(button.dataset.protocol);
    button.disabled = state.modelsSaving || !allowed;
    button.setAttribute('title', allowed ? '按 ' + protocolLabel(button.dataset.protocol) + ' 生成路由'
      : '上游未声明此协议');
  });
  renderEditorRouting(editor);
  renderBudgetControls(editor);

  const parsed = parseEfforts(editor.draft.efforts);
  const chips = [];
  parsed.list.forEach((value) => {
    const chip = document.createElement('span');
    chip.className = 'chip mono' + (STANDARD_EFFORTS.includes(value) ? '' : ' chip--custom');
    chip.textContent = STANDARD_EFFORTS.includes(value) ? value : value + ' · 自定义';
    chips.push(chip);
  });
  parsed.invalid.forEach((value) => {
    const chip = document.createElement('span');
    chip.className = 'chip chip--bad mono';
    chip.textContent = value;
    chips.push(chip);
  });
  if (dom.mEffortChips) dom.mEffortChips.replaceChildren(...chips);

  renderEffortChoices(parsed.list);
  renderUpstreamProtocol(editor);
  renderFailoverRows(editor);

  syncDefaultOptions(parsed.list, editor.draft.defaultEffort);

  const reasoning = editor.draft.reasoning === true;
  if (dom.mEffortGroup) dom.mEffortGroup.disabled = !reasoning;
  if (dom.mDefault) dom.mDefault.disabled = !reasoning || !parsed.list.length;
  setHidden(dom.mEffortField, !reasoning);

  const { errors } = validateEditor(editor);
  const ready = configReady(state.data);
  const dirty = editorDirty();
  /* 协议未确认的候选不能被当作已确认能力保存：只有在用户填写了路由 ID
     （即显式选择了协议）之后才允许保存。 */
  const protocolPending = editor.draft.protocolConfirmed === false;

  if (dom.modelErrors) {
    const messages = (editor.serverErrors || []).concat(errors);
    if (protocolPending) {
      messages.unshift(editor.draft.protocolHint
        || '上游没有声明协议，请手动填写路由 ID 确认协议后再保存。');
    }
    if (!ready && state.data) messages.unshift('配置当前不是就绪状态，保存已禁用。请先重启面板。');
    if (editor.confirmClose) messages.unshift('关闭会丢弃未保存的修改。');
    setHidden(dom.modelErrors, messages.length === 0);
    const nodes = messages.map((message) => {
      const p = document.createElement('p');
      p.textContent = message;
      return p;
    });
    if (editor.confirmClose) {
      const discard = document.createElement('button');
      discard.type = 'button';
      discard.className = 'btn btn--danger btn--sm';
      discard.textContent = '放弃并关闭';
      discard.addEventListener('click', () => closeEditor(true));
      nodes.push(discard);
    }
    if (editor.conflict) {
      const reapply = document.createElement('button');
      reapply.type = 'button';
      reapply.className = 'btn btn--outline btn--sm';
      reapply.textContent = '按最新配置重新应用我的修改';
      reapply.addEventListener('click', rebaseEditor);
      nodes.push(reapply);
    }
    dom.modelErrors.replaceChildren(...nodes);
  }

  if (dom.modelSave) dom.modelSave.disabled = errors.length > 0 || !ready || !dirty || state.modelsSaving || protocolPending;
  if (dom.modelSave) setText(dom.modelSave, state.modelsSaving ? '正在保存…' : '保存');
  if (dom.modelCancel) setText(dom.modelCancel, editor.confirmClose ? '继续编辑' : '取消');
  setText(dom.modelSaveNote, dirty ? '保存会提交整个模型目录并写入本机网关；之后按顶部同步状态同步到 Kimi Work。' : '没有未保存的修改。');
}

/* 路由区：连接下拉 + 上游 ID + 由两者推导的建议路由 ID。
   建议值只在用户还没手改 ID 时自动填入，避免覆盖手写值。 */
function renderEditorRouting(editor) {
  const draft = editor.draft;
  /* 协议推导的可见依据就是路由 ID 末段；这里是唯一推导点，提示与按钮共用。 */
  const family = modelFamilyFor(draft.id);
  const rows = (connectionRowsOf(state.data) || []).filter((row) => !row.legacy);
  const options = rows.length
    ? rows.map((row) => ({ value: row.id, label: row.name + ' · ' + row.id }))
    : [{ value: '', label: '旧版单上游（无命名连接）' }];
  if (draft.connectionId && !rows.some((row) => row.id === draft.connectionId)) {
    options.push({ value: draft.connectionId, label: draft.connectionId + '（未在已保存连接中）' });
  }
  syncSelectOptions(dom.mConnection, options, draft.connectionId || '');
  if (dom.mConnection) dom.mConnection.disabled = !rows.length || state.modelsSaving;

  const suggested = routeIdFor(draft.connectionId, draft.upstreamModelId);
  /* 只有用户明确给了上游 ID 且协议已被确认时，才把建议值自动写进路由 ID。
     协议待确认（picker 命中未声明协议的候选）时留空，交给用户手填。 */
  if (suggested && draft.protocolConfirmed !== false && !String(draft.id || '').trim()) {
    draft.id = suggested;
    if (dom.mId) dom.mId.value = suggested;
  }
  if (dom.mUpstreamHint) {
    const upstream = String(draft.upstreamModelId || '').trim();
    const id = String(draft.id || '').trim();
    setText(dom.mUpstreamHint, upstream
      ? '发给上游的 model 值按原样保存：' + upstream
      : id ? '留空表示与路由 ID 相同（' + id + '）。' : '实际发给上游的 model 值，按原样保留。');
  }
  if (dom.mProtocolHint) {
    const connection = draft.connectionId ? findConnection(state.data, draft.connectionId) : null;
    const restricted = connection && Array.isArray(connection.protocols) && connection.protocols.length
      ? connection.protocols : null;
    /* 说明改成「当前可见推导结论 + 原因」：读到什么（路由 ID 末段/上游声明/
       预设/连接限制）就说什么，不写无条件的规则句。连接限制与当前推导冲突时，
       在本行内给出该连接实际需要的协议。 */
    if (draft.protocolHint) setText(dom.mProtocolHint, draft.protocolHint);
    else if (family && restricted && !restricted.includes(family)) {
      setText(dom.mProtocolHint, '当前路由 ID 末段推导为 ' + protocolLabel(family)
        + '，但连接「' + connectionLabel(connection) + '」只允许 '
        + restricted.map(protocolLabel).join('/') + '；保存前请改用 '
        + restricted.map(protocolLabel).join(' 或 ') + '。');
    } else if (family) {
      setText(dom.mProtocolHint, '由当前路由 ID 末段推导为 ' + protocolLabel(family)
        + (restricted ? '；连接「' + connectionLabel(connection) + '」允许 ' + restricted.map(protocolLabel).join('/') + '。' : '。'));
    } else if (draft.protocolSource === 'preset') {
      setText(dom.mProtocolHint, '上游目录未声明协议；当前值来自供应商预设「'
        + (providerPreset(draft.providerPresetId) ? providerPreset(draft.providerPresetId).name || providerPreset(draft.providerPresetId).id : '')
        + '」，保存前请确认。');
    } else if (restricted) {
      setText(dom.mProtocolHint, '路由 ID 尚未推导出协议；连接「' + connectionLabel(connection) + '」只允许 '
        + restricted.map(protocolLabel).join('/') + '。');
    } else {
      setText(dom.mProtocolHint, '填写路由 ID 后按末段推导协议；当前连接未限制协议。');
    }
  }
  if (dom.mConnectionHint) {
    setText(dom.mConnectionHint, rows.length
      ? '连接决定请求发往哪个上游；上游模型 ID 决定上游看到的 model 值。'
      : '还没有命名连接。先在「连接与工具」添加连接，或使用旧版单上游设置。');
  }
}

/* 预算控件：只高亮真实匹配的预设；上限未知时「完整」禁用并说明，
   任何预设都不会被标成「推荐」，因为套餐限制只能由上游决定。 */
function renderBudgetControls(editor) {
  const draft = editor.draft;
  const budget = budgetStateFor(draft);
  const group = dom.mBudgetGroup;
  if (group) {
    const buttons = Array.from(group.querySelectorAll('[data-budget]'));
    buttons.forEach((button) => {
      const presetId = button.dataset.budget;
      const preset = BUDGET_PRESETS.find((entry) => entry.id === presetId);
      /* 一个认不出的按钮值不能让整块渲染中断：那会连带让保存按钮、提示与
         默认档位停在旧状态。认不出就如实禁用并说明，其余控件照常渲染。 */
      if (!preset) {
        button.setAttribute('aria-pressed', 'false');
        button.disabled = true;
        button.setAttribute('title', '未识别的预算预设：' + String(presetId));
        return;
      }
      const active = preset.id === 'full'
        ? Boolean(budget.full && budget.current === budget.full)
        : Boolean(budget.current === preset.tokens);
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
      const limited = preset.id !== 'full' && budget.contextLimit && preset.tokens > budget.contextLimit;
      const unknownFull = preset.id === 'full' && !budget.full;
      button.disabled = Boolean(limited || unknownFull) || state.modelsSaving;
      button.setAttribute('title', limited
        ? preset.label + ' 超过已知上限 ' + fmtTokens(budget.contextLimit)
        : unknownFull ? '上游上限未知，无法选择完整上下文' : '设为 ' + preset.label);
    });
  }
  if (dom.mContext) dom.mContext.disabled = state.modelsSaving;
  if (dom.mContextHint) {
    if (budget.contextLimit) {
      setText(dom.mContextHint, '已知上游上限 ' + fmtTokens(budget.contextLimit)
        + '；预算不得超过它，超出的预设已禁用。');
    } else {
      setText(dom.mContextHint, '上游上限未知：预设只是起点，保存后请按套餐实际能力确认。');
    }
  }
  if (editor.budgetNote && dom.modelErrors && dom.modelErrors.hidden) {
    /* 预设被拒的说明单独呈现，不与保存错误混在一起。 */
    setHidden(dom.modelErrors, false);
    const p = document.createElement('p');
    p.textContent = editor.budgetNote;
    dom.modelErrors.replaceChildren(p);
  }
}


/* ---------- saving ---------- */

async function saveCatalogue(models, { close = true, message = '已保存。请按同步状态同步到 Work。' } = {}) {
  const data = state.data;
  if (!data || state.modelsSaving) return false;
  if (!configReady(data)) {
    showToast('配置当前不是就绪状态，无法保存模型目录。请先重启面板。', 'error');
    return false;
  }
  /* 连接抽屉开着时，里面的输入还没有并入草稿。模型保存要整组提交连接，
     因此先把抽屉结算掉：校验失败就停在抽屉里，绝不静默丢弃用户输入。 */
  if (state.conns.editing) {
    applyConnectionEditor();
    if (state.conns.editing) {
      showToast('还有一条连接没有通过校验。请先在连接编辑器里修正或取消，再保存模型。', 'error');
      return false;
    }
  }

  state.modelsSaving = true;
  const epoch = state.stateReadEpoch;
  const key = state.key;
  const current = () => epoch === state.stateReadEpoch && key === state.key;
  renderEditor();
  renderModels();
  renderActions(data);

  try {
    const revision = state.editor?.baseRevision ?? data.revision;
    const res = await request(API.workspace, { method: 'POST', body: {
      revision, models, connections: connectionPayload(state.conns.draft),
    } });
    if (!current()) return false;
    if (res && res.state) {
      state.modelsSaving = false;
      adoptState(res.state, { external: true });
      state.conns.base = savedConnectionsSignature(state.data);
      state.conns.baseRevision = state.data.revision ?? null;
      state.conns.baseEntity = connectionDraftFrom(state.data);
      state.conns.draft = connectionDraftFrom(state.data);
      state.conns.dirty = false;
      state.lastSync = Date.now();
      if (close) state.editor = null;
      state.deleteId = null;
      renderAll();
      renderEditor();
      showToast(message, 'ok');
      return true;
    }
    state.modelsSaving = false;
    await loadState({ silent: true });
    if (!current()) return false;
    showToast('模型目录已提交，状态已刷新。', 'ok');
    return true;
  } catch (error) {
    if (!current()) return false;
    state.modelsSaving = false;
    await handleSaveFailure(error, 'models');
    return false;
  } finally {
    if (!current()) return;
    state.modelsSaving = false;
    renderEditor();
    renderModels();
    if (state.data) renderActions(state.data);
  }
}


/* ---------- events ---------- */

function onModelFilter(filter) {
  state.filter = filter;
  [['all', dom.filterAll], ['vision', dom.filterVision], ['tools', dom.filterTools], ['reasoning', dom.filterReasoning]]
    .forEach(([name, button]) => {
      if (button) button.setAttribute('aria-pressed', name === filter ? 'true' : 'false');
    });
  renderModels();
}


function clearModelQuery() {
  state.query = '';
  if (dom.modelQ) dom.modelQ.value = '';
  setHidden(dom.modelQClear, true);
  renderModels();
  if (dom.modelQ && dom.modelQ.focus) dom.modelQ.focus();
}


function resetModelSignature() { modelSignature = ''; }

export {
  applyBudgetPreset,
  catalogueWith,
  chooseProtocol,
  chooseUpstreamProtocol,
  clearModelQuery,
  closeEditor,
  closePicker,
  connectionRowsOf,
  connectionsAvailable,
  connectionsChanged,
  declaredEfforts,
  diffValue,
  editorDirty,
  findModel,
  isCapable,
  modelIdFromAlias,
  modelSignature,
  modelThreeWayDiff,
  onModelFilter,
  openEditor,
  openPicker,
  pickCandidate,
  pickerScope,
  preferredProtocol,
  readEditorFields,
  renderEditor,
  renderFailoverRows,
  renderModels,
  renderPicker,
  resetModelSignature,
  routeIdFor,
  runConnectionCheck,
  runDiscovery,
  saveCatalogue,
  scheduleSearch,
  searchCandidates,
  toggleEffortChoice,
  validateEditor,
};

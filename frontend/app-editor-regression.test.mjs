import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

/* 这一组用例只覆盖模型编辑器与共享前端事件里已经出现过的回归：
   候选选择写坏档位、默认档位回写、默认档位标签未隐藏、换连接留下的旧上限、
   预算提示被覆盖、轮询覆盖向导输入、保存被拒绝后草稿被静默丢弃。
   全部从真实源码里抽函数执行，避免测试复制一份会漂移的实现。 */

const modelsSource = readFileSync(new URL('./app-models.js', import.meta.url), 'utf8');
const coreSource = readFileSync(new URL('./app-core.js', import.meta.url), 'utf8');
const appSource = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const connectionsSource = readFileSync(new URL('./app-connections.js', import.meta.url), 'utf8');
const htmlSource = readFileSync(new URL('./index.html', import.meta.url), 'utf8');

function extractFunction(text, name) {
  const declaration = [`\nfunction `, `\nasync function `].map(prefix => prefix + name + '(').find(token => text.includes(token));
  assert.notEqual(declaration, undefined, 'missing source function: ' + name);
  const start = text.indexOf(declaration);
  const end = text.indexOf('\n}', start);
  assert.notEqual(end, -1, 'unterminated source function: ' + name);
  return text.slice(start + 1, end + 2);
}

const modelFunction = name => extractFunction(modelsSource, name);
const appFunction = name => extractFunction(appSource, name);
const connectionFunction = name => extractFunction(connectionsSource, name);
/* 通用控件同步住在 app-core.js：编辑器只调用，不重复实现。 */
const coreFunction = name => extractFunction(coreSource, name);

const MODEL_HELPERS = ['hasVision', 'parseEfforts', 'metadataSourceOf', 'priceDraftFrom',
  'failoverDraftFrom', 'buildPrice', 'buildFailover', 'buildModel', 'canonicalPrice', 'canonicalFailover',
  'canonicalModel', 'routeIdFor', 'suggestedRouteId'].map(modelFunction);

/* 候选选择与连接协议共用这一整套真实实现：预设与连接只在此处注入。 */
const PICKER_ENV = `
  const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
  const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
  const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];
  ${MODEL_HELPERS.join('\n')}
  ${['candidateMetadataSource', 'connectionRowsOf', 'findConnection', 'fillEditorFields'].map(modelFunction).join('\n')}
  ${['connectionProtocol', 'draftConnection', 'draftConnectionProtocol'].map(connectionFunction).join('\n')}
  const setHidden = () => {};
  const setText = () => {};
  const renderPicker = () => {};
  const renderModels = () => {};
  const renderEditor = () => {};
  const connectionLabel = row => row ? (row.name || row.id || '—') : '—';
  ${modelFunction('pickCandidate')}
  return pickCandidate;
`;

function makePickerEnv({ models = [], presets = [], connections = [{ id: 'conn', protocols: ['responses'] }] } = {}) {
  const element = () => ({ value: '', hidden: false, focused: false, focus() { this.focused = true; } });
  const state = {
    data: { models: [], connections: { saved: connections } },
    providers: { presets },
    picker: { models, open: true, query: '' },
    editor: { originalId: null, draft: null, serverErrors: [], budgetNote: null },
  };
  state.editor.draft = {
    id: '', name: '', protocol: 'responses', context: '', output: '', tools: undefined, reasoning: false, vision: false,
    efforts: [], defaultEffort: '', connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], touched: new Set(),
  };
  const dom = { mName: element(), mUpstream: element(), mAdvanced: { open: false }, mEfforts: element() };
  const pickCandidate = new Function('state', 'dom', PICKER_ENV)(state, dom);
  return { state, dom, pickCandidate };
}

const DESCRIBED_MODEL = {
  id: 'model-a', name: 'Model A', contextWindow: 262144, maxOutputTokens: 8192,
  tools: true, reasoning: true, inputModalities: ['text', 'image'],
  reasoningEfforts: ['low', 'high'], protocols: ['responses'],
  metadataSource: { kind: 'discovery' },
};

test('picking a candidate keeps efforts as an array so the editor can render it', () => {
  const env = makePickerEnv({ models: [DESCRIBED_MODEL] });
  env.pickCandidate('model-a');
  const draft = env.state.editor.draft;
  assert.deepEqual(draft.efforts, ['low', 'high']);
  /* 关键回归：字符串会让 fillEditorFields 的 join 抛错。 */
  assert.deepEqual(parseEffortsForTest(draft.efforts), ['low', 'high']);
  assert.equal(draft.name, 'Model A');
  assert.equal(draft.context, '262144');
  assert.equal(draft.contextLimit, '262144');
  assert.equal(draft.output, '8192');
  assert.equal(draft.tools, true);
  assert.equal(draft.reasoning, true);
  assert.equal(draft.vision, true);
  assert.equal(draft.id, 'conn/resp-model-a');
});

/* 用真实 parseEfforts 复算一次：候选写入的值必须能被同一份解析器接受。 */
function parseEffortsForTest(value) {
  const parseEfforts = new Function(
    'const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;' + modelFunction('parseEfforts') + '\nreturn parseEfforts;')();
  return parseEfforts(value).list;
}

test('picking a new candidate drops values the previous candidate owned', () => {
  const env = makePickerEnv({ models: [
    { ...DESCRIBED_MODEL, id: 'model-a', name: 'Model A' },
    { id: 'model-b', protocols: ['responses'] },
  ] });
  env.pickCandidate('model-a');
  /* 先模拟用户在第一候选上填过值。 */
  const draft = env.state.editor.draft;
  draft.efforts = ['low', 'high'];
  draft.defaultEffort = 'high';
  draft.contextLimit = '262144';
  draft.vision = true;
  draft.tools = true;

  env.pickCandidate('model-b');
  assert.equal(draft.upstreamModelId, 'model-b');
  /* 第二候选什么都没声明：上一候选的名称、预算、能力与档位都不能留下。 */
  assert.equal(draft.name, '');
  assert.equal(draft.context, '');
  assert.equal(draft.contextLimit, '');
  assert.equal(draft.output, '');
  assert.equal(draft.tools, undefined);
  assert.equal(draft.reasoning, false);
  assert.equal(draft.vision, false);
  assert.deepEqual(draft.efforts, []);
  assert.equal(draft.defaultEffort, '');
});

test('candidate protocol declarations do not override the connection protocol', () => {
  const env = makePickerEnv({ models: [
    { id: 'model-a', contextWindow: 1000, protocols: ['chat'] },
    { id: 'model-b', contextWindow: 2000, protocols: ['chat', 'responses'] },
  ], connections: [{ id: 'conn', protocols: ['responses'] }] });
  env.pickCandidate('model-a');
  const draft = env.state.editor.draft;
  assert.equal(draft.protocol, 'responses');
  assert.equal(draft.id, 'conn/resp-model-a');

  env.pickCandidate('model-b');
  assert.equal(draft.protocol, 'responses', 'the connection protocol must survive a candidate change');
  assert.equal(draft.id, 'conn/resp-model-b');
});

test('an explicit-empty candidate declaration still uses the connection protocol', () => {
  const env = makePickerEnv({ models: [{ id: 'model-a', contextWindow: 1000, protocols: [] }] });
  env.pickCandidate('model-a');
  const draft = env.state.editor.draft;
  assert.equal(draft.protocol, 'responses');
  assert.equal(draft.id, 'conn/resp-model-a');
});

test('the generated model id follows the single connection protocol without guessing from the name', () => {
  const makeSuggested = (connections = []) => new Function('connections', `
    const state = { data: { connections: { saved: connections } } };
    const dom = {};
    ${modelFunction('routeIdFor')}
    ${modelFunction('connectionRowsOf')}
    ${modelFunction('findConnection')}
    ${['connectionProtocol', 'draftConnection', 'draftConnectionProtocol'].map(connectionFunction).join('\n')}
    ${modelFunction('suggestedRouteId')}
    return suggestedRouteId;
  `)(connections);

  /* 只允许 Chat 的连接上手输模型名：必须生成 kimi- 前缀。 */
  const chatOnly = makeSuggested([{ id: 'conn-a', protocols: ['chat'], apiKeySet: true }]);
  assert.equal(chatOnly({ connectionId: 'conn-a', upstreamModelId: 'deepseek-v4', id: '' }), 'conn-a/kimi-deepseek-v4');
  /* 只允许 Responses 时反过来。 */
  const respOnly = makeSuggested([{ id: 'conn-b', protocols: ['responses'], apiKeySet: true }]);
  assert.equal(respOnly({ connectionId: 'conn-b', upstreamModelId: 'deepseek-v4', id: '' }), 'conn-b/resp-deepseek-v4');
  /* 旧配置没有唯一协议时不生成，也不按上游名猜一个。 */
  const ambiguous = makeSuggested([{ id: 'conn-c', protocols: ['chat', 'responses'], apiKeySet: true }]);
  assert.equal(ambiguous({ connectionId: 'conn-c', upstreamModelId: 'model-a', id: '' }), '');
  const unrestricted = makeSuggested([{ id: 'conn-d', protocols: [], apiKeySet: true }]);
  assert.equal(unrestricted({ connectionId: 'conn-d', upstreamModelId: 'deepseek-v4', id: '' }), '');
});

test('toggling an effort off also clears a default that pointed at it', async () => {
  const toggleEffortChoice = await buildToggleEffortChoice();
  const env = toggleEffortChoice({ efforts: ['low', 'high'], defaultEffort: 'high' });
  env.toggle('high');
  assert.deepEqual(env.state.editor.draft.efforts, ['low']);
  assert.equal(env.state.editor.draft.defaultEffort, '', 'the removed default must not come back');
  assert.equal(env.dom.mEfforts.value, 'low');
  assert.equal(env.dom.mDefault.value, '');
});

test('turning all efforts off clears the default and the hidden field', async () => {
  const toggleEffortChoice = await buildToggleEffortChoice();
  const env = toggleEffortChoice({ efforts: ['low', 'high'], defaultEffort: 'low' });
  env.toggle('off');
  assert.deepEqual(env.state.editor.draft.efforts, []);
  assert.equal(env.state.editor.draft.defaultEffort, '');
  assert.equal(env.dom.mEfforts.value, '');
  assert.equal(env.dom.mDefault.value, '');
});

async function buildToggleEffortChoice() {
  const code = `
    const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
    ${modelFunction('parseEfforts')}
    const readEditorFields = () => {
      /* 真实实现会从 select 读回默认档位：这里保留同一条读取路径。 */
      state.editor.draft.defaultEffort = dom.mDefault.value;
      state.editor.draft.efforts = parseEfforts(dom.mEfforts.value).list;
    };
    const renderEditor = () => {};
    ${modelFunction('toggleEffortChoice')}
    return toggleEffortChoice;
  `;
  return ({ efforts, defaultEffort }) => {
    const state = { editor: { draft: { efforts: efforts.slice(), defaultEffort } }, modelsSaving: false };
    const dom = { mEfforts: { value: efforts.join(', ') }, mDefault: { value: defaultEffort } };
    const toggle = new Function('state', 'dom', code)(state, dom);
    return { state, dom, toggle };
  };
}

test('the whole default-effort field is hidden with reasoning, not just the select', () => {
  const form = htmlSource.slice(htmlSource.indexOf('id="model-form"'), htmlSource.indexOf('id="model-errors"'));
  assert.match(form, /<label class="field" id="m-default-field" for="m-default">/);
  const fieldStart = form.indexOf('id="m-default-field"');
  const fieldEnd = form.indexOf('</label>', fieldStart);
  assert.notEqual(fieldEnd, -1);
  /* label 必须包含标题、下拉框与提示：隐藏它等于同时隐藏三者。 */
  const field = form.slice(fieldStart, fieldEnd);
  for (const id of ['m-default', '默认档位']) assert.ok(field.includes(id), 'default field missing ' + id);

  const hidden = appCallsForDefaultField();
  assert.match(hidden, /setHidden\(dom\.mDefaultField \|\| dom\.mDefault/);
  /* 只隐藏 select 会让「默认档位」四个字留在表单里指向空处。 */
  assert.equal(/setHidden\(dom\.mDefault, !reasoning\)/.test(modelsSource), false);
});

function appCallsForDefaultField() {
  const start = modelsSource.indexOf('const reasoning = editor.draft.reasoning === true;');
  assert.notEqual(start, -1);
  return modelsSource.slice(start, modelsSource.indexOf('\n\n', start));
}

test('editor hints match what saving actually does', () => {
  const form = htmlSource.slice(htmlSource.indexOf('id="model-form"'), htmlSource.indexOf('id="model-errors"'));
  /* 最大输出留空在 buildModel 里是 null 补丁，会把旧值删掉，不是「保留原值」。 */
  assert.match(form, /留空表示不声明；编辑已有模型时清空会移除原来声明的值。/);
  assert.equal(form.includes('保存已有模型时保留原值'), false);
  /* 思考档位可以一档都不选：buildModel 只在 list 非空时才写 reasoningEfforts。 */
  assert.match(form, /不选表示只声明支持思考、不列出档位/);
  assert.equal(form.includes('至少选择一档'), false);
});

test('a cleared max output really removes the stored value', () => {
  const buildModel = new Function(`
    const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
    const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
    const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];
    ${MODEL_HELPERS.join('\n')}
    return buildModel;
  `)();
  const base = { id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000, connectionId: 'conn', maxOutputTokens: 8192 };
  const draft = {
    id: base.id, name: '', context: '200000', output: '', tools: undefined, reasoning: false, vision: false,
    efforts: [], defaultEffort: '', connectionId: 'conn', upstreamModelId: '', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], baseEntity: base, touched: new Set(),
  };
  const { model, errors } = buildModel(draft);
  assert.deepEqual(errors, []);
  assert.equal(model.maxOutputTokens, null);
});

test('editing an existing model keeps its saved id and protocol', () => {
  const buildModel = new Function(`
    const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
    const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
    const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];
    ${MODEL_HELPERS.join('\n')}
    return buildModel;
  `)();
  const base = {
    id: 'conn/legacy-route-a', protocol: 'chat', contextWindow: 100000, connectionId: 'conn', name: '旧名称',
  };
  const draft = {
    id: 'conn/resp-new-model', name: '新名称', protocol: 'responses', context: '100000', output: '',
    tools: undefined, reasoning: false, vision: false, efforts: [], defaultEffort: '',
    connectionId: 'conn', upstreamModelId: 'new-model', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], baseEntity: base, touched: new Set(),
  };
  const { model, errors } = buildModel(draft);
  assert.deepEqual(errors, []);
  assert.equal(model.id, base.id, 'the saved model id must not be regenerated');
  assert.equal(model.protocol, 'chat', 'the saved protocol must not be rewritten');
  assert.equal(model.name, '新名称');
  assert.equal(model.upstreamModelId, 'new-model');
});

/* 连接换了协议不改写已保存模型：编辑名称、能力或定价时，编辑器不能因为
   model.protocol 与连接当前协议不同而拒绝保存；新导入仍按连接协议生成。 */
test('validateEditor saves an existing model across a connection protocol change', () => {
  const code = `
    const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
    const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
    const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];
    ${MODEL_HELPERS.join('\n')}
    ${['connectionRowsOf', 'findConnection', 'connectionLabel'].map(modelFunction).join('\n')}
    ${['connectionProtocol', 'draftConnection', 'draftConnectionProtocol'].map(connectionFunction).join('\n')}
    ${modelFunction('validateEditor')}
    return validateEditor;
  `;
  const base = { id: 'conn/resp-old', protocol: 'responses', contextWindow: 200000, connectionId: 'conn', name: '旧模型' };
  const draft = {
    id: base.id, name: '新名称', context: '200000', output: '', tools: true, reasoning: false, vision: false,
    efforts: [], defaultEffort: '', connectionId: 'conn', upstreamModelId: 'old-model', upstreamProtocol: '',
    contextLimit: '', metadataSource: null,
    pricing: { inputPerMillion: '1', outputPerMillion: '2', cachedInputPerMillion: '', cacheWritePerMillion: '' },
    failover: [], baseEntity: base, touched: new Set(),
  };
  const state = {
    data: {
      models: [base],
      connections: { saved: [{ id: 'conn', name: 'Conn', protocols: ['chat'], apiKeySet: true }] },
    },
    conns: { draft: [{ id: 'conn', name: 'Conn', baseURL: 'https://api.example.com/v1', protocols: ['chat'], legacy: false }] },
    onboard: {},
  };
  const validate = new Function('state', 'dom', code)(state, {});
  const { model, errors } = validate({ originalId: base.id, draft });
  assert.deepEqual(errors, []);
  assert.equal(model.id, base.id);
  assert.equal(model.protocol, 'responses');
  assert.equal(model.pricing.inputPerMillion, 1);

  /* 模型行引用了列表里没有的连接时仍然拦下。 */
  const orphan = validate({ originalId: null, draft: { ...draft, connectionId: 'gone', baseEntity: null } });
  assert.equal(orphan.errors.some((text) => text.includes('不在连接列表中')), true);
});

/* 同页草稿：连接编辑器还没保存的协议修改必须被模型编辑器读到，而不是
   退回 state.data 里的旧协议。 */
test('the model editor follows an unsaved connection protocol change', () => {
  const code = `
    const dom = { onboardId: { value: '' } };
    ${modelFunction('routeIdFor')}
    ${modelFunction('connectionRowsOf')}
    ${modelFunction('findConnection')}
    ${['connectionProtocol', 'draftConnection', 'draftConnectionProtocol'].map(connectionFunction).join('\n')}
    ${modelFunction('suggestedRouteId')}
    return { draftConnectionProtocol, suggestedRouteId };
  `;
  const state = {
    data: { connections: { saved: [{ id: 'conn', name: 'Conn', protocols: ['chat'] }] } },
    conns: { draft: [{ id: 'conn', name: 'Conn', protocols: ['responses'], legacy: false }] },
    onboard: {},
  };
  const api = new Function('state', code)(state);
  assert.equal(api.draftConnectionProtocol('conn'), 'responses');
  assert.equal(api.suggestedRouteId({ connectionId: 'conn', upstreamModelId: 'model-a' }), 'conn/resp-model-a');
  /* 草稿里协议被清空后不再猜一个值。 */
  state.conns.draft = [{ id: 'conn', name: 'Conn', protocols: [], legacy: false }];
  assert.equal(api.draftConnectionProtocol('conn'), null);
  assert.equal(api.suggestedRouteId({ connectionId: 'conn', upstreamModelId: 'model-a' }), '');
});

/* renderEditor 的真实调用顺序：预算控件先跑，随后的统一 errors 渲染是唯一
   写 model-errors 的地方。预算提示必须在最终节点里出现。 */
function makeRenderEnv({ reasoning = true, budgetNote = null, serverErrors = [], validationErrors = [] } = {}) {
  const nodes = [];
  const element = (id) => ({ id, hidden: true, textContent: '', disabled: false, dataset: {}, children: nodes,
    replaceChildren(...next) { nodes.length = 0; nodes.push(...next); },
    querySelectorAll: () => [],
    setAttribute(name, value) { this[name] = value; } });
  const dom = {
    inspector: element('inspector'), workbench: { dataset: {} }, inspectorTitle: element('title'),
    mEffortChips: element('chips'), mEffortGroup: element('group'),
    mEffortField: element('effort-field'), mDefault: element('default'), mDefaultField: element('default-field'),
    modelErrors: element('model-errors'), modelSave: element('save'), modelCancel: element('cancel'),
    modelSaveNote: element('save-note'), mUpstreamHint: element('upstream-hint'),
    mConnection: null, mConnectionHint: element('conn-hint'),
    mBudgetGroup: element('budget'), mContext: element('context'), mContextHint: element('context-hint'),
    mFailoverRows: element('failover'), mFailoverCount: element('failover-count'), mFailoverAdd: element('add'),
    mUpstreamProtocolField: element('upstream-proto'), mUpstreamProtocolGroup: null,
  };
  const state = {
    data: { configuration: { state: 'ready' }, models: [], connections: { saved: [] } },
    modelsSaving: false,
  };
  const editor = {
    originalId: 'conn/resp/model-a',
    serverErrors,
    budgetNote,
    draft: {
      id: 'conn/resp/model-a', name: '', protocol: 'responses', context: '200000', output: '', tools: undefined,
      reasoning, vision: false, efforts: reasoning ? ['low', 'high'] : [], defaultEffort: '',
      connectionId: 'conn', upstreamModelId: 'model-a', upstreamProtocol: '',
      contextLimit: '', metadataSource: null,
      pricing: { inputPerMillion: '', outputPerMillion: '', cachedInputPerMillion: '', cacheWritePerMillion: '' },
      failover: [], touched: new Set(),
      baseEntity: { id: 'conn/resp/model-a', protocol: 'responses', contextWindow: 200000, connectionId: 'conn' },
    },
  };
  const code = `
    const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
    const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
    const BUDGET_PRESETS = [
      { id: '256k', label: '256K', tokens: 262144 },
      { id: '350k', label: '350K', tokens: 350000 },
      { id: '600k', label: '600K', tokens: 600000 },
      { id: 'full', label: '完整', tokens: null },
    ];
    const PRICE_FIELDS = ['inputPerMillion', 'outputPerMillion', 'cachedInputPerMillion', 'cacheWritePerMillion'];
    const STANDARD_EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];
    ${MODEL_HELPERS.join('\n')}
    ${['connectionRowsOf', 'findConnection', 'connectionLabel', 'routingOf', 'budgetStateFor',
      'renderBudgetControls', 'renderEditorRouting', 'renderEffortChoices', 'renderUpstreamProtocol',
      'upstreamProtocolSupported', 'candidateMetadataSource',
      'renderFailoverRows'].map(modelFunction).join('\n')}
    ${['syncDefaultOptions', 'syncSelectOptions'].map(coreFunction).join('\n')}
    ${['connectionProtocol', 'draftConnection', 'draftConnectionProtocol'].map(connectionFunction).join('\n')}
    const protocolLabel = value => value === 'chat' ? 'Chat' : value === 'responses' ? 'Responses' : value;
    const fmtK = String;
    const fmtTokens = String;
    const setHidden = (el, hidden) => { if (el) el.hidden = Boolean(hidden); };
    const setText = (el, value) => { if (el) el.textContent = value; };
    const configReady = data => data?.configuration?.state === 'ready';
    const validateEditor = () => ({ model: state.editor.draft, errors: VALIDATION_ERRORS });
    const editorDirty = () => true;
    const renderAll = () => {};
    const document = { createElement: tag => ({ tag, textContent: '', className: '', dataset: {}, children: [],
      append(...nodes) { this.children.push(...nodes); },
      addEventListener() {}, setAttribute() {} }) };
    let VALIDATION_ERRORS = [];
    const parseEffortsReal = parseEfforts;
    const renderEditor = ${modelFunction('renderEditor')}
    return { renderEditor, setValidationErrors: errors => { VALIDATION_ERRORS = errors; } };
  `;
  const api = new Function('state', 'dom', code)(state, dom);
  api.setValidationErrors(validationErrors);
  state.editor = editor;
  return { state, dom, api, editor, nodes };
}

test('the budget note is merged into the single errors list instead of being overwritten', () => {
  const env = makeRenderEnv({ budgetNote: '350K 超过该模型已知上限 262,144，不能选择。' });
  env.api.renderEditor();
  assert.equal(env.dom.modelErrors.hidden, false);
  const texts = env.nodes.map(node => node.textContent);
  assert.equal(texts.includes('350K 超过该模型已知上限 262,144，不能选择。'), true,
    'budget note must survive the final replaceChildren');
});

test('a rejected budget preset leaves the note visible alongside save errors', () => {
  const env = makeRenderEnv({
    budgetNote: '未知上游上限，无法选择「完整」。',
    serverErrors: ['invalid_models'],
  });
  env.api.renderEditor();
  const texts = env.nodes.map(node => node.textContent);
  assert.deepEqual(texts.slice(0, 2), ['未知上游上限，无法选择「完整」。', 'invalid_models']);
});

test('renderBudgetControls no longer writes model-errors itself', () => {
  const body = modelFunction('renderBudgetControls');
  assert.equal(/modelErrors/.test(body), false, 'budget note must be rendered by renderEditor only');
});

test('the default-effort field is hidden whenever reasoning is off', () => {
  const off = makeRenderEnv({ reasoning: false });
  off.api.renderEditor();
  assert.equal(off.dom.mDefaultField.hidden, true);

  const on = makeRenderEnv({ reasoning: true });
  on.api.renderEditor();
  assert.equal(on.dom.mDefaultField.hidden, false);
  assert.equal(on.dom.mEffortField.hidden, false);
});

/* --- shared events -------------------------------------------------------- */

function makeFailureEnv() {
  const errors = [];
  const element = { hidden: true, textContent: '', children: [],
    replaceChildren(...next) { errors.length = 0; errors.push(...next); } };
  const dom = { conn: element, connText: element, svcErrors: element, connsErrors: element,
    connErrors: element, onboardSaveErrors: element };
  const state = {
    key: 'sess', data: { configuration: { state: 'ready' } },
    editor: null, onboard: { saving: false, busy: false, saveErrors: [] }, conns: { errors: [], draft: [], editing: null },
    modelsSaving: false, connsSaving: false, services: { saving: false }, subagentSaving: false,
  };
  const toasts = [];
  globalThis.document = { createElement: () => ({ textContent: '' }) };
  const code = `
    const setHidden = (el, hidden) => { if (el) el.hidden = Boolean(hidden); };
    const setText = (el, value) => { if (el) el.textContent = value; };
    const renderErrorList = (el, messages) => {
      const list = (messages || []).filter(message => typeof message === 'string' && message);
      el.hidden = list.length === 0;
      el.replaceChildren(...list.map(message => ({ textContent: message })));
    };
    const setConnErrors = messages => {
      state.conns.errors = (messages || []).filter(message => typeof message === 'string' && message);
      renderErrorList(dom.connsErrors, state.conns.errors);
    };
    const showToast = (message, tone) => toasts.push([message, tone]);
    const clearKey = () => {};
    const showGate = () => {};
    const renderSubagents = () => {};
    const renderServices = () => {};
    const renderConnections = () => {};
    const renderEditor = () => {};
    const renderOnboard = () => {};
    const stopPolling = () => {};
    const loadState = async () => true;
    const modelThreeWayDiff = () => [];
    const diffValue = value => JSON.stringify(value);
    ${appFunction('handleSaveFailure')}
    return handleSaveFailure;
  `;
  const handle = new Function('state', 'dom', 'toasts', code)(state, dom, toasts);
  return { state, dom, toasts, handle, errors };
}

const badRequest = (message, code = 'invalid_models') => ({
  status: 400, code, message, saved: false,
});

test('a rejected connection save keeps the draft and shows the error on the connection tab', async () => {
  const env = makeFailureEnv();
  env.state.conns.draft = [{ id: 'conn', name: 'Draft', apiKey: 'sk-draft-typed' }];
  env.state.conns.editing = { originalId: null };
  await env.handle(badRequest('invalid_connections'), 'connections');
  assert.deepEqual(env.state.conns.errors, ['invalid_connections']);
  assert.equal(env.dom.connsErrors.hidden, false);
  assert.deepEqual(env.errors.map(node => node.textContent), ['invalid_connections']);
  /* 草稿与已输入的 Key 都不能因为一次 400 被清掉。 */
  assert.equal(env.state.conns.draft.length, 1);
  assert.equal(env.state.conns.draft[0].apiKey, 'sk-draft-typed');
});

test('a rejected onboarding save is visible outside the list renderOnboard redraws', async () => {
  const env = makeFailureEnv();
  env.state.onboard.discovery = { models: [{ upstreamModelId: 'model-a', selected: true }] };
  await env.handle(badRequest('invalid_models'), 'onboard');
  assert.deepEqual(env.state.onboard.saveErrors, ['invalid_models']);
  assert.equal(env.dom.onboardSaveErrors.hidden, false);
  assert.deepEqual(env.errors.map(node => node.textContent), ['invalid_models']);
  /* 向导草稿必须原样保留，用户改完能直接重试。 */
  assert.equal(env.state.onboard.discovery.models.length, 1);
});

test('editing the wizard clears a previous server rejection', () => {
  const clear = appFunction('clearOnboardSaveErrors');
  const element = { hidden: false, replaceChildren() { this.hidden = true; } };
  const state = { onboard: { saveErrors: ['invalid_models'] } };
  const dom = { onboardSaveErrors: element };
  const renderErrorList = (target, messages) => { target.hidden = messages.length === 0; };
  /* 源码里的函数声明被包成 IIFE，返回值才是函数本身。 */
  new Function('state', 'dom', 'renderErrorList', 'return ' + clear.trim() + ';')(state, dom, renderErrorList)();
  assert.deepEqual(state.onboard.saveErrors, []);
  assert.equal(element.hidden, true);
});

function makePollingEnv() {
  const state = {
    hidden: false,
    modelsSaving: false, connsSaving: false, services: { saving: false }, subagentSaving: false,
    onboard: { saving: false, busy: false },
    conns: { editing: null },
    timer: null, loads: 0,
  };
  const code = `
    const POLL_MS = 2000;
    const window = { setInterval: (fn) => { state.tick = fn; return 1; }, clearInterval: () => {} };
    const document = { hidden: false, activeElement: null };
    const dom = { onboardModels: { contains: (node) => node === document.activeElement } };
    const loadState = () => { state.loads++; return Promise.resolve(true); };
    ${connectionFunction('onboardEditingText')}
    ${appFunction('stopPolling')}
    ${appFunction('editingBusy')}
    ${appFunction('startPolling')}
    return { startPolling, editingBusy, setTextFocus: (focused = true) => {
      document.activeElement = focused ? { dataset: { onboardName: 'model-a' } } : null;
    } };
  `;
  const api = new Function('state', code)(state);
  return { state, api, setTextFocus: api.setTextFocus };
}

test('polling skips a read while the wizard is saving or discovering', () => {
  const env = makePollingEnv();
  env.api.startPolling();
  env.state.tick();
  assert.equal(env.state.loads, 1);
  env.state.onboard.saving = true;
  env.state.tick();
  assert.equal(env.state.loads, 1, 'a save in progress owns the form');
  env.state.onboard.saving = false;
  env.state.onboard.busy = true;
  env.state.tick();
  assert.equal(env.state.loads, 1, 'a discovery in progress owns the form');
  env.state.onboard.busy = false;
  env.state.tick();
  assert.equal(env.state.loads, 2);
  env.state.conns.editing = { discovering: true };
  env.state.tick();
  assert.equal(env.state.loads, 2, 'a connection discovery in progress owns its inputs');
  env.state.conns.editing.discovering = false;
  env.state.tick();
  assert.equal(env.state.loads, 3);
  env.setTextFocus();
  env.state.tick();
  assert.equal(env.state.loads, 3, 'a focused model input owns its IME and caret');
  assert.equal(env.api.editingBusy(), true);
  env.setTextFocus(false);
  env.state.tick();
  assert.equal(env.state.loads, 4, 'polling resumes after editing leaves the input');
});

test('the visibility handler uses the same busy rule as the poller', () => {
  const block = appFunctionSpan(appSource, "document.addEventListener('visibilitychange', () => {");
  assert.match(block, /!editingBusy\(\)/);
  /* 旧实现只挡了三个保存标志，向导保存期间仍会被轮询覆盖输入。 */
  assert.equal(block.includes('state.modelsSaving &&'), false);
  assert.equal(block.includes('state.connsSaving &&'), false);
});

test('changing the connection or the upstream id drops the stale context limit', () => {
  const connectionBlock = appFunctionSpan(appSource, "if (dom.mConnection) dom.mConnection.addEventListener('change'");
  assert.match(connectionBlock, /contextLimit = ''/);
  assert.match(connectionBlock, /suggestedRouteId/);
  const upstreamBlock = appFunctionSpan(appSource, "if (dom.mUpstream) dom.mUpstream.addEventListener('input'");
  assert.match(upstreamBlock, /contextLimit = ''/);
  assert.match(upstreamBlock, /suggestedRouteId/);
});

test('editingBusy pauses polling during connection editor typing or IME composition', () => {
  const state = {
    composing: false, modelsSaving: false, connsSaving: false, services: { saving: false }, subagentSaving: false,
    onboard: { saving: false, busy: false }, conns: { editing: { discovering: false } },
  };
  const ceName = { id: 'ce-name' };
  const documentStub = { activeElement: null };
  const dom = {
    onboardModels: { contains: () => false },
    connEditor: { contains: (node) => node === ceName },
  };
  const editingBusy = new Function('state', 'document', 'dom', `
    const onboardEditingText = () => false;
    ${appFunction('editingBusy')}
    return editingBusy;
  `)(state, documentStub, dom);

  assert.equal(editingBusy(), false);
  documentStub.activeElement = ceName;
  assert.equal(editingBusy(), true, 'focused connection editor input must pause polling');
  documentStub.activeElement = null;
  state.composing = true;
  assert.equal(editingBusy(), true, 'IME composition must pause polling');
  state.composing = false;
  assert.equal(editingBusy(), false);
});

test('runDiscovery failure preserves picker reason through subsequent searchCandidates', async () => {
  const state = {
    data: { revision: 'rev-1', connections: { saved: [{ id: 'ds', name: 'DeepSeek' }] } },
    picker: { open: true, busy: false, seq: 0, query: 'deepseek', models: null, state: null, checkedAt: null, source: null, reason: null, hint: null, scope: null },
  };
  const fn = new Function('state', 'request', `
    const API = { discovery: '/api/discovery', modelSearch: '/api/models/search' };
    const connectionRowsOf = (d) => d.connections.saved;
    const pickerScope = () => 'ds';
    const discoveryReasonText = (code) => code;
    const renderPicker = () => {};
    const showToast = () => {};
    const adoptState = () => {};
    ${modelFunction('searchCandidates')}
    ${modelFunction('runDiscovery')}
    return { runDiscovery };
  `)(state, async (path) => {
    if (path === '/api/discovery') return { discovery: { state: 'failed', reason: 'upstream_unauthorized', models: [] } };
    return { models: [{ id: 'deepseek-chat', upstreamModelId: 'deepseek-chat' }], source: 'preset' };
  });

  await fn.runDiscovery('ds');
  assert.equal(state.picker.reason, 'upstream_unauthorized', 'discovery failure reason must not be cleared by fallback searchCandidates');
  assert.equal(state.picker.models.length, 1);
});

/* 事件回调在源码里是内联的，用花括号配对截出整段。 */
function appFunctionSpan(text, token) {
  const start = text.indexOf(token);
  assert.notEqual(start, -1, 'missing handler: ' + token);
  let depth = 0;
  let index = text.indexOf('{', start);
  const opened = index;
  for (; index < text.length; index += 1) {
    if (text[index] === '{') depth += 1;
    else if (text[index] === '}') {
      depth -= 1;
      if (depth === 0) return text.slice(opened, index + 1);
    }
  }
  assert.fail('unterminated handler: ' + token);
}

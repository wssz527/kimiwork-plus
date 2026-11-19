const API = {
  state: '/api/state',
  workspace: '/api/workspace',
  models: '/api/models',
  connection: '/api/connection',
  connections: '/api/connections',
  subagents: '/api/subagents',
  translation: '/api/translation',
  actions: '/api/actions',
  providers: '/api/providers',
  modelSearch: '/api/model-search',
  discovery: '/api/discovery',
  context: '/api/context',
  tools: '/api/tools',
};

const POLL_MS = 2000;
const KEY_STORE = 'kwg.panel.key';
const THEME_STORE = 'kwg.panel.theme';
const VIEW_STORE = 'kwg.panel.view';
const MODEL_ALIAS_PREFIXES = ['byok-kth-kimi/', 'byok-kth-resp/'];
const EFFORT_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
const ENV_PATTERN = /^[A-Z_][A-Z0-9_]*$/;
const CONNECTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const LOOPBACK = new Set(['127.0.0.1', '[::1]', 'localhost', '::1']);

/* 上下文窗口预设：256K/350K/600K 是固定 token 数，"完整"取已知真实上限。
   任何预设都不得把套餐上限抬高，因此上限未知时只作手填起点并明确标注。 */
const BUDGET_PRESETS = [
  { id: '256k', label: '256K', tokens: 262144 },
  { id: '350k', label: '350K', tokens: 350000 },
  { id: '600k', label: '600K', tokens: 600000 },
  { id: 'full', label: '完整', tokens: null },
];

const DISCOVERY_REASON = {
  discovery_unavailable: '无法连接该上游，请检查地址与网络。',
  discovery_timeout: '读取上游目录超时。',
  discovery_http_error: '上游 /models 返回了错误状态。',
  discovery_too_large: '上游目录超出允许大小，已中止。',
  discovery_invalid_catalog: '上游返回的目录格式不被接受。',
  connection_unavailable: '该连接已不在配置中。',
  discovery_busy: '上一次读取仍在进行，请稍候。',
};

/* check-upstream 失败原因与 discovery 共用同一族错误码；这里补齐它自己的
   连接级拒绝，避免把「连接不在当前配置」显示成原始代码。 */
const UPSTREAM_REASON = {
  ...DISCOVERY_REASON,
  connection_unavailable: '该连接不在当前已配置的连接中；检查按行进行，不会隐式挑选。',
};

let renderGrowth = () => {};
let usagePanel = { show() {}, reload: async () => {}, reset() {} };
let plusPanel = { render() {}, refresh: async () => {} };

const VIEW_COPY = {
  overview: { title: '总览', lede: '网关、Kimi Work 接入、当前账号与今日用量。' },
  models: { title: '模型', lede: '在这里管理服务商与模型，保存后同步到 Kimi Work。' },
  usage: { title: '用量', lede: '查看真实请求、服务商余额与 Kimi Code 额度。' },
  work: { title: 'Work 能力', lede: '管理子代理、并行、MCP、联网与长对话整理。' },
  extensions: { title: '扩展', lede: '独立管理思考翻译，关闭扩展不会影响网关与 Work。' },
  maintenance: { title: '设置', lede: '账号管理、后台服务、版本与 Kimi 接入诊断。' },
};

const VIEW_ALIASES = { connection: 'models', enhancements: 'work', subagents: 'work', tools: 'work', settings: 'work', translation: 'extensions' };

/* 旧的单页区块在 HTML 里仍按功能分组，但一级导航已经合并：这里把已有 section
   物理移动到目标视图，避免同一份标记维护两处。移动只做一次，重复调用是幂等的。 */
function mountWorkspace() {
  const move = (from, to) => {
    const source = $('view-' + from);
    const target = $(to);
    if (!source || !target) return;
    target.append(...source.children);
    source.remove();
  };
  $('models-services-body')?.append($('connections-panel'));
  $('view-maintenance')?.append($('restart-panel'));
  for (const view of ['subagents', 'tools', 'settings']) move(view, 'view-work');
  move('translation', 'view-extensions');
  $('view-connection')?.remove();
  /* 被合并的旧视图各自带一个 sr-only 标题；视图已经没有 aria-labelledby，
     留在页面里会让读屏读到三个属于别处的标题。 */
  for (const id of ['subagents-heading', 'tools-heading', 'settings-heading']) $(id)?.remove();
  mountUsageRequests();
}

/* 用量面板整体重写 #usage-root，因此「最近请求」只能在面板 mount 之后
   移入它的请求页；按父节点判断，重复调用不会产生第二份表。 */
function mountUsageRequests() {
  const pane = $('usage-pane-requests');
  const section = $('recent-table')?.closest('section');
  if (!pane || !section || section.parentElement === pane) return;
  pane.append(section);
}

/* Work 能力页的分类入口：目标可能在折叠区里（MCP 面板就在 mcp-details 内），
   只展开目标本身不够，祖先 details 必须依次打开，否则滚动会落在隐藏节点上。 */
function revealWorkspaceSection(id) {
  const target = id ? $(id) : null;
  if (!target) return null;
  if (target.tagName === 'DETAILS') target.open = true;
  for (let node = target.parentElement; node; node = node.parentElement) {
    if (node.tagName === 'DETAILS') node.open = true;
  }
  target.scrollIntoView({ block: 'start' });
  /* 快捷入口也把焦点交给区块标题，键盘用户才不会停在页首。 */
  const heading = target.querySelector('h2, h3, summary') || target;
  if (typeof heading.focus === 'function') {
    if (!heading.hasAttribute('tabindex')) heading.setAttribute('tabindex', '-1');
    heading.focus({ preventScroll: true });
  }
  return target;
}

const ACTION_LABEL = {
  'launch-kimi': '启动 Kimi Work',
  'open-work': '打开 Kimi Work',
  'restart-kimi': '正常退出并重启 Kimi',
  'prepare-native': '准备原生设置',
  'remove-dns': '移除单域名 DNS',
  'connect-kimi': '重新准备接入',
  'check-upstream': '上游检查',
  'apply-connections': '应用连接',
  'account-capture': '保存当前账号',
  'account-begin-add': '添加账号',
  'account-complete-add': '保存新账号',
  'account-cancel-add': '恢复原账号',
  'account-switch': '切换账号',
  'account-remove': '移除账号',
  'set-swarm': '设置并行模式',
};

const ACTION_STATE = {
  running: '进行中',
  prepared: '已准备',
  starting: '启动请求已发出',
  'waiting-kimi-exit': '等待 Kimi 退出',
  'waiting-idle': '等待 Kimi 空闲',
  'waiting-quit': '等待 Kimi 正常退出',
  active: '已连接',
  inactive: '等待 Kimi 启动',
  cancelled: '已取消',
  done: '已完成',
  removed: '已移除',
  copied: '已复制',
  passed: '通过',
  verified: '兼容',
  incompatible: '不兼容',
  unavailable: '不可用',
  unverified: '未验证',
  failed: '失败',
  interrupted: '已中断',
  'not-checked': '尚未检查',
};

const CONFIG_STATE = {
  ready: { tone: 'ok', word: '就绪', note: '' },
  changed: { tone: 'warn', word: '已变更，待重启', note: '配置文件已被修改，当前进程仍在使用启动时读取的连接。' },
  unavailable: { tone: 'bad', word: '不可读', note: '无法读取配置文件，保存已禁用。' },
};

const RUNTIME_STATE = {
  'not-prepared': { tone: 'neutral', word: '待准备', note: '还没有本机运行时副本。' },
  'waiting-launch': { tone: 'warn', word: '待启动', note: '副本已准备，等待 Kimi 用该副本启动。' },
  'pending-policy': { tone: 'warn', word: '待采用', note: '运行时尚未加载当前策略。' },
  loaded: { tone: 'ok', word: '已加载', note: '运行时已加载策略。' },
  'heartbeat-stale': { tone: 'warn', word: '心跳过期', note: '运行时曾加载策略，但心跳已过期。' },
  'policy-unavailable': { tone: 'bad', word: '策略不可用', note: '运行时无法读取策略，子代理请求会被拒绝。' },
  unavailable: { tone: 'bad', word: '不可用', note: '运行时当前不可用。' },
  unsupported: { tone: 'bad', word: '启动接口不支持', note: '当前应用无法确认适配副本的加载能力；主模型 BYOK 与普通翻译可独立使用。' },
  'scope-conflict': { tone: 'bad', word: '作用域不匹配', note: '实际运行时与策略所属的 Kimi 目录不一致。' },
  outdated: { tone: 'warn', word: '版本过旧', note: '当前 Kimi 版本与已保存配置不兼容。' },
  conflict: { tone: 'bad', word: '冲突', note: '运行时与配置存在冲突，未被覆盖。' },
  'file-applied': { tone: 'ok', word: '配置已写入', note: '子代理配置文件已更新，等待 Kimi 重新载入；是否采用以 Kimi 为准。' },
  'waiting-config': { tone: 'warn', word: '配置待写入', note: '当前目标尚未写入本机配置文件。' },
  restored: { tone: 'neutral', word: '已恢复原状', note: '本面板管理的配置已撤销，Kimi 恢复自身配置。' },
  inherit: { tone: 'neutral', word: '未接管', note: '子代理模型未被接管，Kimi 使用自己的配置。' },
  blocked: { tone: 'bad', word: '已阻止', note: '子代理策略处于阻止状态，子代理请求会被拒绝。' },
  unverified: { tone: 'warn', word: '未验证', note: '无法确认当前 Kimi 版本支持模型池配置；尚未写入任何配置。' },
};

const DIAG_REASON = {
  account_session_unavailable: '未连接到 Kimi 登录窗口，请在账号管理中正常重启并继续。',
  account_session_page_timeout: 'Kimi 登录窗口没有及时响应，请在账号管理中重试。',
  account_session_unsupported: '当前 Kimi 登录接口未能确认账号，请在账号管理中恢复原账号。',
  account_session_sign_out_incomplete: '尚未确认 Kimi 已打开登录页，请先恢复原账号。',
  account_switch_restore_failed: '添加或切换未完成，原账号仍待恢复，请在账号管理中继续。',
  account_identity_unverified: '尚未确认当前登录，请在 Kimi 窗口完成登录后重试。',
  account_session_work_busy: 'Work 正在执行任务，请结束任务后再操作账号。',
  account_work_busy: 'Work 正在执行任务，请结束任务后再操作账号。',
  account_login_pending: '已有账号添加流程，请先完成或取消。',
  account_login_unchanged: '当前仍是原账号，请登录另一个账号后再保存。',
  upstream_http_error: '上游返回了错误状态。',
  upstream_connection_failed: '无法连接上游地址。',
  upstream_catalog_invalid: '上游 /models 返回的格式不被接受。',
  catalog_parser_missing: '本机目录解析器未找到。',
  bundle_unavailable: '本机 Kimi 目录不可读。',
  probe_failed: '兼容检查未能执行。',
  native_byok_dns_unavailable: '单域名 DNS 不可用，请检查端口占用。',
  native_byok_dns_stop_failed: '单域名 DNS 未能正常停止。',
  native_byok_intranet_unavailable: '指定域名的私网解析尚未通过。',
  native_byok_reveal_failed: '官方功能标记未能写入或读回。',
  setup_timeout: '原生接入准备超时；请检查系统授权窗口后重试。',
  setup_authorization_failed: '系统管理员授权失败；尚未写入新的接入配置。',
  setup_cancelled: '系统授权已取消；尚未继续原生接入。',
  invalid_setup_flag: '官方功能标记文件不可读或格式无效。',
  kimi_data_missing: '未找到官方 Kimi 数据目录，请先正常启动官方 Kimi。',
  setup_report_missing: '系统解析操作没有返回可验证的结果。',
  unsupported_hosts_encoding: '系统 hosts 文件编码不受支持，未修改原文件。',
  hosts_verify_failed: '系统 hosts 文件写入后校验失败。',
  resolver_conflict: '解析配置存在冲突，未覆盖外来内容。',
  native_setup_failed: '系统解析准备未完成。',
  kimi_quit_refused: 'Kimi 未接受退出请求。请在官方窗口处理确认提示，然后重新启动。',
  kimi_quit_unavailable: '系统无法发送正常退出请求。请先手动正常退出 Kimi，再从面板启动。',
  kimi_quit_timeout: '等待 Kimi 正常退出超时。请检查官方窗口是否有任务或确认提示，再重试。',
  process_identity_unknown: '无法确认当前 Kimi 进程身份。请先正常退出 Kimi，再从面板启动。',
  official_profile_mismatch: '当前数据目录与官方 Kimi 目录不一致，启动已暂停。请检查诊断信息。',
  work_storage_restore_conflict: 'Work 数据位置已被其他操作改动，未覆盖。请正常退出 Kimi，并核对数据位置。',
  profile_mode_change_requires_exit: '切换登录方式前，请先正常退出当前 Kimi，再选择所需方式启动。',
  kimi_application_missing: '未找到官方 Kimi 应用。请安装官方桌面版后重试。',
  existing_login_profile_invalid: '官方登录目录不可用。请先正常打开官方 Kimi，再从面板重新启动。',
  existing_login_profile_unavailable: '未找到官方登录目录。请先正常打开官方 Kimi。',
};

const ERROR_HINT = {
  configuration_conflict: '配置在别处被修改，已刷新状态；你的编辑仍然保留，请确认后重新保存。',
  configuration_changed: '配置已在外部修改，模型保存已禁用，请重启面板。',
  configuration_unavailable: '无法读取配置文件，请检查文件后重启面板。',
  configuration_apply_failed: '目录已保存，但运行策略未能更新，请检查状态后重启。',
  invalid_models: '模型声明无效：请检查 ID、上下文窗口、能力与思考档位。',
  protocol_conflict: '模型 ID 末段以 kimi- 开头时必须使用 Chat，其他必须使用 Responses。',
  invalid_base_url: '上游地址需使用 HTTPS（本机回环可用 HTTP），且不能包含账号、查询参数或片段。',
  invalid_config: '配置无效，请检查端口和环境变量名称。',
  invalid_connection_config: '连接配置不完整或包含不支持的字段。密钥由系统凭据存储保存，配置文件只记录引用。',
  credentials_unavailable: '系统凭据存储未接受保存，配置未提交。请检查预览权限后重试；不会写入明文。',
  credentials_readback_failed: '新密钥未能从系统凭据存储读回，未切换现有引用。请重试或继续使用稳定版。',
  credential_keychain_locked: '登录钥匙串已锁定，请在 macOS 中解锁后从本窗口重新保存；密钥未写入。',
  credential_keychain_unavailable: '当前登录会话无法使用系统钥匙串，请检查预览签名和会话权限；密钥未写入。',
  credential_session_unavailable: '当前后台会话不能显示授权窗口，请从预览窗口重新保存。',
  credential_authorization_required: '此条目需要系统授权，请从预览窗口重新保存并响应系统提示。',
  credential_authorization_cancelled: '系统授权已取消，密钥未保存；原配置仍保留。',
  credential_authorization_failed: '系统未接受授权，密钥未保存；原配置仍保留。',
  credential_store_corrupt: '凭据条目内容无法识别，请从预览窗口重新输入并保存。',
  credential_interaction_not_restored: '系统凭据交互状态未能恢复，请重启预览服务后重试。',
  credential_store_failed: '系统凭据操作未完成，请查看诊断状态；密钥未保存。',
  panel_action_busy: '上一次操作仍在执行，请等待结果。',
  panel_action_conflict: '操作标识已被使用，请刷新后重试。',
  panel_action_limit: '短时间内操作过多，请稍后再试。',
  panel_receipt_conflict: '操作记录有冲突，请检查状态目录。',
  panel_stopping: '面板正在停止。',
  origin_refused: '请求来源不匹配，请从本机面板链接操作。',
  json_required: '请求格式不被接受。',
  request_too_large: '请求过大，请减少一次提交的模型数量。',
  invalid_panel_action: '操作请求格式不被接受，请刷新后重试。',
  apply_connections_unavailable: '当前后端不支持自动应用连接；连接已保存，但网关尚未应用。',
  apply_unavailable: '当前后端不支持自动应用连接；连接已保存，但网关尚未应用。',
  service_unavailable: '没有可用于自动应用的后台服务。连接已保存，但网关尚未应用。',
  unmanaged_service: '当前面板不是由受管后台服务运行，无法自动应用连接。',
  invalid_subagent_policy: '提交的目标格式不被接受，请重新选择模式与模型。',
  invalid_subagent_pool: '模型池需要 1–32 个不重复、均已声明用途说明的候选模型。',
  invalid_subagent_description: '用途说明必须是单行文本，最长 1000 字符。',
  subagent_pool_default_required: '默认模型必须在候选列表中。',
  subagent_model_unavailable: '所选模型已不在目录中，请重新选择。',
  subagent_model_tools_required: '此模型未声明工具调用，不能作为子代理。',
  subagent_model_in_use: '当前子代理仍在使用此模型或档位，请先到「子代理」更改目标，再修改模型能力。',
  subagent_policy_owner_conflict: '另一实例已接管子代理策略，请停止重复运行的服务。',
  subagent_policy_unavailable: '无法读取当前子代理策略或配置归属存在冲突，请检查状态目录后重启面板。',
  subagent_policy_apply_failed: '目标已保存，但当前策略未能应用，请查看运行时状态。',
  invalid_subagent_effort: '此模型未声明所选思考档位，请重新选择。',
};

/* configured（已保存）/ policy（文件与策略）/ runtime（内核运行时）三层，
   加上 observed-schema 真实观察，各自用词不能互相冒充。 */
const POLICY_MODE_WORD = { inherit: '继承主模型', force: '固定一个模型', pool: '模型池', blocked: '已阻止' };
/* 传输标识是内部值，用户可见处只写中文；原始值留在 title 里供排查。 */
const POLICY_TRANSPORT_WORD = { 'native-config': '原生配置文件' };

const $ = (id) => document.getElementById(id);

const dom = {
  shell: $('shell'),
  gate: $('gate'),
  gateTitle: $('gate-title'),
  gateLede: $('gate-lede'),
  gateNote: $('gate-note'),
  gateActions: $('gate-actions'),
  gateRetry: $('gate-retry'),

  conn: $('conn'),
  connText: $('conn-text'),
  connRetry: $('conn-retry'),
  restartBanner: $('restart-banner'),
  restartBannerText: $('restart-banner-text'),
  restartBannerLink: $('restart-banner-link'),
  budgetBanner: $('budget-banner'),
  budgetBannerText: $('budget-banner-text'),

  opbar: $('opbar'),
  opbarDot: $('opbar-dot'),
  opbarName: $('opbar-name'),
  opbarNote: $('opbar-note'),

  viewTitle: $('view-title'),
  viewLede: $('view-lede'),
  sync: $('sync'),
  refresh: $('refresh'),

  navOverview: $('nav-overview'),
  navModels: $('nav-models'),
  navUsage: $('nav-usage'),
  navWork: $('nav-work'),
  navExtensions: $('nav-extensions'),
  navMaintenance: $('nav-maintenance'),
  navModelCount: $('nav-model-count'),

  railGatewayDot: $('rail-gateway-dot'),
  railGatewayText: $('rail-gateway-text'),
  railGatewayUrl: $('rail-gateway-url'),
  theme: $('theme'),

  factGatewayDot: $('fact-gateway-dot'),
  factGateway: $('fact-gateway'),
  factGatewayUrl: $('fact-gateway-url'),
  factModels: $('fact-models'),
  factModelsSub: $('fact-models-sub'),
  factConfigDot: $('fact-config-dot'),
  factConfig: $('fact-config'),
  factConfigFile: $('fact-config-file'),

  factConnections: $('fact-connections'),
  factConnectionsSub: $('fact-connections-sub'),
  factRoute: $('fact-route'),
  factRouteSub: $('fact-route-sub'),
  factContextDot: $('fact-context-dot'),
  factContext: $('fact-context'),
  factContextSub: $('fact-context-sub'),
  factToolsDot: $('fact-tools-dot'),
  factTools: $('fact-tools'),
  factToolsSub: $('fact-tools-sub'),


  metricRequests: $('metric-requests'),
  metricFailures: $('metric-failures'),
  metricUptime: $('metric-uptime'),
  recentBody: $('recent-body'),
  recentEmpty: $('recent-empty'),

  modelQ: $('model-q'),
  modelQClear: $('model-q-clear'),
  filterAll: $('filter-all'),
  filterVision: $('filter-vision'),
  filterTools: $('filter-tools'),
  filterReasoning: $('filter-reasoning'),
  modelCount: $('model-count'),
  modelNew: $('model-new'),
  workbench: $('workbench'),
  modelBody: $('model-body'),
  modelTableWrap: $('model-table-wrap'),
  modelEmpty: $('model-empty'),
  modelEmptyTitle: $('model-empty-title'),
  modelEmptyNote: $('model-empty-note'),
  modelEmptyReset: $('model-empty-reset'),
  inspector: $('inspector'),
  inspectorTitle: $('inspector-title'),
  inspectorClose: $('inspector-close'),
  modelForm: $('model-form'),
  mId: $('m-id'),
  mIdHint: $('m-id-hint'),
  mUpstream: $('m-upstream'),
  mUpstreamHint: $('m-upstream-hint'),
  mConnection: $('m-connection'),
  mConnectionHint: $('m-connection-hint'),
  mCandidates: $('m-candidates'),
  mSourceField: $('m-source-field'),
  mAdvanced: $('m-advanced'),
  mProtocolHint: $('m-protocol-hint'),
  mContextHint: $('m-context-hint'),
  mBudgetGroup: $('m-budget-group'),
  mName: $('m-name'),
  mProtocol: $('m-protocol'),
  mProtocolGroup: $('m-protocol-group'),
  mContext: $('m-context'),
  mOutput: $('m-output'),
  mTools: $('m-tools'),
  mReasoning: $('m-reasoning'),
  mVision: $('m-vision'),
  mEffortField: $('m-effort-field'),
  mEffortGroup: $('m-effort-group'),
  mEfforts: $('m-efforts'),
  mEffortChips: $('m-effort-chips'),
  mUpstreamProtocolField: $('m-upstream-protocol-field'),
  mUpstreamProtocolGroup: $('m-upstream-protocol-group'),
  mDefault: $('m-default'),
  mPriceInput: $('m-price-input'),
  mPriceOutput: $('m-price-output'),
  mPriceCached: $('m-price-cached'),
  mPriceWrite: $('m-price-write'),
  mFailoverRows: $('m-failover-rows'),
  mFailoverAdd: $('m-failover-add'),
  mFailoverCount: $('m-failover-count'),
  modelErrors: $('model-errors'),
  modelCancel: $('model-cancel'),
  modelSave: $('model-save'),
  modelSaveNote: $('model-save-note'),


  restartState: $('restart-state'),
  restartFile: $('restart-file'),
  restartHead: $('restart-head'),
  applyButton: $('apply-connections'),
  applyVerdict: $('apply-verdict'),
  applyVerdictDot: $('apply-verdict-dot'),
  applyVerdictWord: $('apply-verdict-word'),
  applyVerdictNote: $('apply-verdict-note'),

  onboardPreset: $('ob-preset'),
  onboardPresetHint: $('ob-preset-hint'),
  onboardKey: $('ob-key'),
  onboardKeyHint: $('ob-key-hint'),
  onboardBase: $('ob-base'),
  onboardName: $('ob-name'),
  onboardId: $('ob-id'),
  onboardDiscover: $('ob-discover'),
  onboardRecommended: $('ob-recommended'),
  onboardRecommendedHint: $('ob-recommended-hint'),
  onboardDiscoverState: $('ob-discover-state'),
  onboardCatalogue: $('ob-catalogue'),
  onboardCatalogueCount: $('ob-catalogue-count'),
  onboardCatalogueNote: $('ob-catalogue-note'),
  onboardModels: $('ob-models'),
  onboardModelQuery: $('ob-model-q'),
  onboardSelectAll: $('ob-select-all'),
  onboardModelsEmpty: $('ob-models-empty'),
  onboardBatchChat: $('ob-batch-chat'),
  onboardBatchContext: $('ob-batch-context'),
  onboardBatchContextApply: $('ob-batch-context-apply'),
  onboardErrors: $('ob-errors'),
  onboardDraft: $('ob-draft'),
  onboardSave: $('ob-save'),
  onboardSteps: $('onboard-steps'),
  onboardVerdict: $('ob-verdict'),
  onboardVerdictDot: $('ob-verdict-dot'),
  onboardVerdictWord: $('ob-verdict-word'),
  onboardVerdictNote: $('ob-verdict-note'),
  onboardLaunch: $('ob-launch'),
  onboardLaunchHint: $('ob-launch-hint'),
  onboardRetryApply: $('ob-retry-apply'),
  onboardAdvancedToggle: $('ob-advanced-toggle'),
  onboardAdvanced: $('ob-advanced'),
  onboardDisclosure: $('onboard-disclosure'),
  handoff: $('handoff'),

  connsUnavailable: $('conns-unavailable'),
  connsList: $('conns-list'),
  connsEmpty: $('conns-empty'),
  connsDiff: $('conns-diff'),
  connsErrors: $('conns-errors'),
  connsDraft: $('conns-draft'),
  connsAdd: $('conns-add'),
  connsReset: $('conns-reset'),
  connsSave: $('conns-save'),
  connEditor: $('conn-editor'),
  connEditorTitle: $('conn-editor-title'),
  connEditorClose: $('conn-editor-close'),
  connEditorCancel: $('conn-editor-cancel'),
  connEditorApply: $('conn-editor-apply'),
  connEditorTest: $('conn-editor-test'),
  ceName: $('ce-name'),
  ceId: $('ce-id'),
  cePreset: $('ce-preset'),
  cePresetHint: $('ce-preset-hint'),
  ceBase: $('ce-base'),
  ceKey: $('ce-key'),
  ceKeyHint: $('ce-key-hint'),
  ceKeyEnv: $('ce-keyenv'),
  ceChat: $('ce-chat'),
  ceResponses: $('ce-responses'),


  svcPanel: $('svc-panel'),
  svcUnavailable: $('svc-unavailable'),
  svcSearchOn: $('svc-search-on'),
  svcSearchUrl: $('svc-search-url'),
  svcSearchKey: $('svc-search-key'),
  svcSearchKeyHint: $('svc-search-key-hint'),
  svcFetchOn: $('svc-fetch-on'),
  svcFetchUrl: $('svc-fetch-url'),
  svcFetchKey: $('svc-fetch-key'),
  svcSearchSaved: $('svc-search-saved'),
  svcSearchSavedSub: $('svc-search-saved-sub'),
  svcFetchSaved: $('svc-fetch-saved'),
  svcFetchSavedSub: $('svc-fetch-saved-sub'),
  svcVerdict: $('svc-verdict'),
  svcVerdictDot: $('svc-verdict-dot'),
  svcVerdictWord: $('svc-verdict-word'),
  svcVerdictNote: $('svc-verdict-note'),
  svcErrors: $('svc-errors'),
  svcDraft: $('svc-draft'),
  svcReset: $('svc-reset'),
  svcSave: $('svc-save'),

  picker: $('picker'),
  pickerTitle: $('picker-title'),
  pickerLede: $('picker-lede'),
  pickerClose: $('picker-close'),
  pickerQ: $('picker-q'),
  pickerScan: $('picker-scan'),
  pickerState: $('picker-state'),
  pickerNote: $('picker-note'),
  pickerList: $('picker-list'),
  pickerEmpty: $('picker-empty'),
  pickerEmptyTitle: $('picker-empty-title'),
  pickerEmptyNote: $('picker-empty-note'),

  saModes: $('sa-modes'),
  modeInherit: $('mode-inherit'),
  modeForce: $('mode-force'),
  modePool: $('mode-pool'),
  saCapabilityNote: $('sa-capability-note'),
  saCapabilityNextRow: $('sa-capability-next-row'),
  saCapabilityNext: $('sa-capability-next'),
  saForceFields: $('sa-force-fields'),
  saPoolFields: $('sa-pool-fields'),
  saModel: $('sa-model'),
  saModelHint: $('sa-model-hint'),
  saEffort: $('sa-effort'),
  saEffortHint: $('sa-effort-hint'),
  saPoolDefault: $('sa-pool-default'),
  saPoolAdd: $('sa-pool-add'),
  saPoolDesc: $('sa-pool-desc'),
  saPoolAppend: $('sa-pool-append'),
  saPoolCount: $('sa-pool-count'),
  saPoolErrors: $('sa-pool-errors'),
  saPoolList: $('sa-pool-list'),
  saWarning: $('sa-warning'),
  saErrors: $('sa-errors'),
  saDraft: $('sa-draft'),
  saReset: $('sa-reset'),
  saSave: $('sa-save'),
  saSavedMode: $('sa-saved-mode'),
  saSavedSub: $('sa-saved-sub'),
  saPolicyMode: $('sa-policy-mode'),
  saPolicySub: $('sa-policy-sub'),
  rtStateDot: $('rt-state-dot'),
  rtState: $('rt-state'),
  rtSub: $('rt-sub'),
  saVerdictDot: $('sa-verdict-dot'),
  saVerdictWord: $('sa-verdict-word'),
  saVerdictNote: $('sa-verdict-note'),
  saObservedNote: $('sa-observed-note'),

  nativeRemote: $('native-remote'),
  nativeRemoteDot: $('native-remote-dot'),
  nativeRemoteWord: $('native-remote-word'),
  nativeRemoteNote: $('native-remote-note'),

  updCurrent: $('upd-current'),
  updCurrentSub: $('upd-current-sub'),

  toast: $('toast'),
};

const state = {
  key: null,
  data: null,
  view: 'overview',
  query: '',
  filter: 'all',
  editor: null,
  deleteId: null,
  conns: { base: null, baseRevision: null, baseEntity: null, dirty: false, editing: null, draft: null, errors: [] },
  services: { base: null, dirty: false, saving: false, searchKey: '', fetchKey: '' },
  connChecks: {},
  picker: { open: false, models: null, query: '', state: null, checkedAt: null, source: null, reason: null,
    busy: false, seq: 0, hint: null, scope: null },
  /* 接入向导：连接字段、上游目录候选与被勾选的模型留在本地草稿里，
     直到用户点「保存」才写配置；轮询不得覆盖正在输入的 Key。 */
  onboard: { discovery: null, discoveryFor: null, seq: 0, busy: false, saving: false, advanced: false, touched: false, query: '' },
  /* 后端明确拒绝过 apply-connections 后记住结论：轮询不得把“自动应用不可用”
     改回“等待应用”，否则界面会在两份互相矛盾的结论之间来回跳。 */
  applyUnsupported: false,
  applyRefusal: null,
  providers: null,
  modelsSaving: false,
  connsSaving: false,
  /* 子代理目标草稿只在用户显式操作时改变；轮询不覆盖、默认继承不自动保存。
     保存错误留在本地，直到下一次显式编辑或成功保存才清除。 */
  draft: null,
  draftDirty: false,
  draftBaseRevision: null,
  draftBaseEntity: null,
  subagentSaving: false,
  subagentErrors: [],
  reachable: null,
  pendingAction: null,
  pendingOpId: null,
  lastSync: null,
  timer: null,
  stateReadSeq: 0,
  stateReadEpoch: 0,
  toastTimer: null,
  started: false,
};

let actionButtons = [];

/* ---------- key ---------- */

function readKey() {
  let key = null;
  const raw = window.location.hash || '';
  if (raw.length > 1) {
    const params = new URLSearchParams(raw.slice(1));
    key = params.get('key');
  }
  if (key) {
    try {
      sessionStorage.setItem(KEY_STORE, key);
    } catch {
      /* 无存储时仅本次会话可用 */
    }
    history.replaceState(null, '', window.location.pathname + window.location.search);
    return key;
  }
  try {
    return sessionStorage.getItem(KEY_STORE);
  } catch {
    return null;
  }
}

function clearKey() {
  try {
    sessionStorage.removeItem(KEY_STORE);
  } catch {
    /* 忽略 */
  }
  state.key = null;
  state.stateReadEpoch++;
}

/* ---------- api ---------- */

class ApiError extends Error {
  constructor(message, code, status) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

async function request(path, { method = 'GET', body } = {}) {
  if (!state.key) throw new ApiError('缺少面板会话密钥', 'NO_KEY', 0);
  const headers = { Authorization: 'Bearer ' + state.key };
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let res;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
      redirect: 'error',
    });
  } catch {
    throw new ApiError('无法连接本机面板服务', 'NETWORK', 0);
  }

  if (res.status === 401) throw new ApiError('面板会话密钥无效或已过期', 'unauthorized', 401);

  let payload = null;
  const type = res.headers.get('content-type') || '';
  if (type.includes('application/json')) {
    try {
      payload = await res.json();
    } catch {
      payload = null;
    }
  }

  if (!res.ok) {
    const code = payload && payload.error && payload.error.code;
    const message = payload && payload.error && payload.error.message;
    const error = new ApiError(ERROR_HINT[code] || message || '请求失败（HTTP ' + res.status + '）', code || 'http_' + res.status, res.status);
    error.saved = Boolean(payload && payload.error && payload.error.saved);
    error.reason = payload && payload.error && payload.error.reason;
    throw error;
  }

  return payload;
}

/* ---------- theme and view ---------- */

const THEME_PREFIX = 'kimi-work-plus:theme:';
const THEME_TOKENS = Object.freeze({ light: 'light', dark: 'dark', auto: 'auto' });

/* The desktop shell reads one fixed token from the document title. The title is
   already suppressed by the shell, so this costs no visible text, no IPC, no
   preload and no extra renderer capability; anything else leaves the title
   exactly as the product wrote it. `auto` stays a distinct token so the shell
   can follow its own nativeTheme when the OS scheme changes. */
function reportTheme(mode) {
  const title = THEME_PREFIX + (THEME_TOKENS[mode] || 'light');
  if (document.title !== title) document.title = title;
}

function applyTheme(value) {
  const mode = value === 'light' || value === 'dark' ? value : 'auto';
  document.documentElement.dataset.theme = mode;
  if (dom.theme) dom.theme.value = mode;
  reportTheme(mode);
  try {
    localStorage.setItem(THEME_STORE, mode);
  } catch {
    /* 忽略 */
  }
}

function closeWorkIndex({ restoreFocus = false } = {}) {
  const workIndex = $('work-index');
  if (!workIndex) return;
  workIndex.open = false;
  if (restoreFocus) workIndex.querySelector('summary')?.focus();
}

/* The open index is a fixed panel anchored to the sticky masthead's bottom
   edge. That edge moves when the header wraps in a narrow window and when the
   shell reserves its window-chrome band, so the 66px design value alone leaves
   either a cut first entry or a strip of the page above the panel. Measure the
   real row and publish it as --kwg-masthead-height; the CSS value stays as the
   first-paint fallback for the moment before this runs. */
function syncMastheadHeight() {
  if (typeof document.querySelector !== 'function') return;
  const masthead = document.querySelector('.masthead');
  if (!masthead || typeof masthead.getBoundingClientRect !== 'function') return;
  const height = Math.round(masthead.getBoundingClientRect().height);
  if (!Number.isFinite(height) || height <= 0) return;
  const style = document.documentElement && document.documentElement.style;
  if (!style || typeof style.setProperty !== 'function') return;
  style.setProperty('--kwg-masthead-height', height + 'px');
}

let mastheadObserver = null;

function observeMasthead() {
  syncMastheadHeight();
  if (typeof window.addEventListener === 'function') window.addEventListener('resize', syncMastheadHeight);
  if (typeof ResizeObserver !== 'function' || typeof document.querySelector !== 'function') return;
  const masthead = document.querySelector('.masthead');
  if (!masthead || mastheadObserver) return;
  mastheadObserver = new ResizeObserver(syncMastheadHeight);
  mastheadObserver.observe(masthead);
}

function initTheme() {
  let stored = null;
  try {
    stored = localStorage.getItem(THEME_STORE);
  } catch {
    stored = null;
  }
  applyTheme(stored || 'light');
}

function initView() {
  let stored = null;
  try {
    stored = sessionStorage.getItem(VIEW_STORE);
  } catch {
    stored = null;
  }
  setView(VIEW_COPY[VIEW_ALIASES[stored] || stored] ? stored : 'overview', { persist: false });
}

function setView(view, { persist = true } = {}) {
  view = VIEW_ALIASES[view] || view;
  if (!VIEW_COPY[view]) view = 'overview';
  // The index is a full-screen overlay, so it is closed before the view swaps:
  // a direct switch never leaves an old page (or half of the index) painted
  // over the new one, and the masthead never shows a stale secondary title.
  closeWorkIndex();
  const apply = () => {
    state.view = view;
    if (document.body) document.body.dataset.view = view;
    Object.keys(VIEW_COPY).forEach((name) => {
      const section = $('view-' + name);
      if (section) { section.hidden = name !== view; if (name === view) section.scrollTop = 0; }
      const nav = $('nav-' + name);
      if (nav) nav.setAttribute('aria-current', name === view ? 'page' : 'false');
    });
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
    if (view === 'work') renderSubagents();
    /* 门禁页没有会话密钥：此时再展示用量面板，挂载期那笔请求只会带空密钥失败。 */
    if (view === 'usage') { if (state.key) usagePanel.show(); void plusPanel.refresh(); }
  };
  apply();
}

/* ---------- format ---------- */

function setText(el, value) {
  if (el && el.textContent !== value) el.textContent = value;
}

function setDot(el, tone) {
  if (el) el.dataset.tone = tone || 'neutral';
}

function setHidden(el, hidden) {
  if (el) el.hidden = Boolean(hidden);
}

/* 错误列表是界面里唯一展示后端拒绝原因的位置；空列表即隐藏，
   避免留下上一次的过期错误被误读成当前状态。 */
function renderErrorList(el, messages) {
  if (!el) return;
  const list = (messages || []).filter((message) => typeof message === 'string' && message);
  setHidden(el, list.length === 0);
  el.replaceChildren(...list.map((message) => {
    const p = document.createElement('p');
    p.textContent = message;
    return p;
  }));
}

/* 连接区的提示必须活过轮询重渲染：把它存进会话状态再画，
   只画不存会导致下一次 state 刷新把用户的错误提示悄悄抹掉。 */
function setConnErrors(messages) {
  state.conns.errors = (messages || []).filter((message) => typeof message === 'string' && message);
  renderErrorList(dom.connsErrors, state.conns.errors);
}

function fmtCount(n) {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '—';
  return n.toLocaleString('zh-CN');
}

function fmtK(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n <= 0) return '—';
  if (n >= 1000) return (n / 1000).toLocaleString('zh-CN', { maximumFractionDigits: 1 }) + 'K';
  return String(n);
}

/* token 预算按真实数值展示；不四舍五入到「看起来像整 K」的数，
   避免把一个手填的 262000 显示成 262K 而掩盖实际值。 */
function fmtTokens(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n <= 0) return '未知';
  return n.toLocaleString('zh-CN') + ' tokens';
}

function fmtMs(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1000) return Math.round(ms) + ' ms';
  return (ms / 1000).toLocaleString('zh-CN', { maximumFractionDigits: 2 }) + ' s';
}

function fmtTime(value) {
  if (value === null || value === undefined || value === '') return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (v) => String(v).padStart(2, '0');
  return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function fmtDuration(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '—';
  const total = Math.floor(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (hours) return hours + ' 小时 ' + String(minutes).padStart(2, '0') + ' 分';
  if (minutes) return minutes + ' 分 ' + String(total % 60).padStart(2, '0') + ' 秒';
  return total + ' 秒';
}

function shortRev(rev) {
  if (typeof rev !== 'string' || !rev) return '未提供';
  return rev.slice(0, 8);
}

function truncate(value, max) {
  if (typeof value !== 'string') return '—';
  return value.length > max ? value.slice(0, max - 1) + '…' : value;
}

function displayModelId(id) {
  const text = typeof id === 'string' ? id : '';
  if (!text || !text.includes('%')) return text;
  try {
    return decodeURIComponent(text);
  } catch {
    return text;
  }
}

function modelLabel(model, alias) {
  if (!model) return typeof alias === 'string' && alias ? displayModelId(alias) : '—';
  if (model.name && model.name !== model.id) return model.name;
  return displayModelId(model.id);
}

function reasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  return DIAG_REASON[code] || code;
}

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

/* ---------- render: shell ---------- */

function renderShell(data) {
  const gw = (data && data.gateway) || {};
  const running = gw.state === 'running';
  setDot(dom.railGatewayDot, running ? 'ok' : 'bad');
  setText(dom.railGatewayText, running ? '网关运行中' : '网关已停止');
  setText(dom.railGatewayUrl, typeof gw.url === 'string' && gw.url ? gw.url : '—');
  setText(dom.sync, state.lastSync ? '同步于 ' + fmtTime(state.lastSync) : '尚未同步');

  const config = (data && data.configuration) || {};
  setHidden(dom.restartBanner, config.state !== 'changed');
  if (config.state === 'changed') {
    setText(dom.restartBannerText, '连接配置已修改，当前网关仍在使用启动时的设置。请到「连接」页点「应用已保存的连接」，由后台服务在安全时应用；页面会如实显示是否生效。');
  }
  setText(dom.navModelCount, fmtCount((data.models || []).length));
  const budgetMessage = budgetNotice(data.budget);
  setHidden(dom.budgetBanner, !budgetMessage);
  setText(dom.budgetBannerText, budgetMessage);
}

function budgetNotice(budget) {
  if (budget?.enabled !== true) return '';
  const notices = [];
  for (const [period, label] of [['daily', '今日'], ['monthly', '本月']]) {
    const window = budget.spend?.[period];
    if (window?.reached !== true) continue;
    notices.push(label + '已达预算上限' + (window.action === 'block' ? '，新请求会被拒绝' : '，此上限仅提醒'));
  }
  if (notices.length && budget.warning === 'unpriced_models_not_limited') notices.push('未定价费用未计入');
  return notices.length ? notices.join('。') + '。' : '';
}

function desktopScopeInfo(data) {
  const info = (data && data.integration) || {};
  if (!info.desktop && info.imported !== true) return null;
  return { tone: 'neutral', word: '仅桌面', note: '只接入本机官方 Kimi 桌面 Work，不同步模型或登录态到 Kimi 手机端。' };
}

/* 接入是否完成只能由原生导入回执与原生目录证据说明。文件已保存、目录已拉取
   都不是同步完成；这里把每种真实状态收敛成同一个 verdict，供概览卡片与连接
   页共用，避免两处各写一套判断而给出互相矛盾的结论。 */
function syncState(data) {
  const info = (data && data.integration) || {};
  const preparation = info.preparation || {};
  const receipt = info.nativeImport || {};
  const models = Array.isArray(data && data.models) ? data.models : [];
  if (preparation.state === 'preparing') {
    return { code: 'authorizing', tone: 'warn', word: '等待系统授权',
      note: '正在准备首次接入，请在本机窗口处理系统授权。' };
  }
  if (preparation.state === 'cancelled') {
    return { code: 'authorization', tone: 'warn', word: '需要授权',
      note: '上次系统授权已取消。点「打开 / 启动 Kimi」或「重新准备接入」可继续；不会反复弹窗。' };
  }
  if (preparation.state === 'failed' || preparation.state === 'interrupted') {
    return { code: 'authorization', tone: 'bad', word: '接入准备失败',
      note: '准备本地接入时中断。检查系统授权后可重试，已保存的连接不受影响。' };
  }
  const reasons = [receipt.reason, info.onboarding?.reason, info.desktop?.reason, info.reason]
    .filter((value) => typeof value === 'string');
  const phases = [info.onboarding?.state, info.onboarding?.stage, info.stage, info.state]
    .filter((value) => typeof value === 'string');
  const has = (...values) => values.some((value) => reasons.includes(value) || phases.includes(value));
  /* Kimi 由系统直接启动时，产品没有它的子进程：这时说「受管进程已加载」是错的，
     即使磁盘上的模型配置确实已经同步过。 */
  const unmanagedRunning = data?.kimiProcess?.running === true && info.desktop?.managed === false;
  if (info.readback && info.applied === true) {
    return { code: 'synced', tone: 'ok', word: '目录与配置已读回',
      note: unmanagedRunning
        ? '已同步的模型配置仍保留在磁盘；当前 Kimi 由系统直接启动，未受本产品托管。点「正常退出并重启 Kimi」可恢复完整受管能力。'
        : info.adopted === true ? '受管进程已加载对应配置代次；官方菜单实际采用仍需在 Kimi 窗口确认。' : '模型目录、账号、地址和配置代次一致；官方菜单实际采用尚未确认。' };
  }
  if (info.desktop?.state === 'failed' && /^(work_storage_|existing_login_|profile_mode_)/.test(info.desktop.reason || '')) {
    return { code: 'conflict', tone: 'bad', word: '隔离启动未完成', note: nativeConnectionText(data) };
  }
  if (info.desktop?.state === 'needs_user_close') {
    return { code: 'manual-exit', tone: 'warn', word: '可以受管重启 Kimi',
      note: '点击「正常退出并重启 Kimi」，产品会请求系统正常退出当前实例，再使用官方登录和真实 Work 数据受管启动。' };
  }
  if (has('account_changed', 'native_import_account_changed') && info.readback?.accountVerified === true) {
    return { code: 'pending', tone: 'warn', word: '当前账号待同步',
      note: '当前账号已登录，模型目录还未与此账号完成核对。请在模型页同步到 Work。' };
  }
  if (has('native_import_waiting_login', 'waiting-login', 'account-unverified', 'account_changed',
    'native_import_account_changed')) {
    if (data?.profile?.canReuseLogin === true && !has('account_changed', 'native_import_account_changed')) {
      if (info.desktop?.state === 'ready' && (data.profile.mode === 'official' || data.profile.mode === 'existing-authorized')) {
        return { code: 'login', tone: 'warn', word: '尚未确认官方登录',
          note: '请查看官方窗口。已有登录有效时无需扫码；当前尚未读到账号，模型接入还没有继续。' };
      }
      const opening = data.operation?.action === 'launch-kimi' && isInFlight(data.operation);
      return { code: 'ready-launch', tone: opening ? 'warn' : 'neutral', word: opening ? '正在打开官方 Work' : '可沿用已有登录',
        note: opening ? '正在沿用官方登录和真实 Work 数据，请等待启动完成。' : '点击「打开 / 启动 Kimi」，沿用官方登录、对话、技能和已导入模型。' };
    }
    return { code: 'login', tone: 'warn', word: '需要登录',
      note: '请先在 Kimi Work 登录并确认当前账户，然后点「打开 / 启动 Kimi」继续。' };
  }
  /* 先回应后台回执里可识别的下一步。state=pending 只是容器，不能盖过
     manual-exit / daimon 各自的动作；这些文案也不把内部 reason code 直接摆给用户。 */
  if (has('native_import_manual_exit_required', 'manual-exit', 'translation_kimi_running', 'waiting-kimi-exit')) {
    return { code: 'manual-exit', tone: 'warn', word: '等待完全退出 Kimi',
      note: '只需从 Kimi 托盘（macOS 为应用菜单）完全退出一次，关窗口不算；退出后本软件会自动重新打开并继续接入，不需要再点或重启桌面。' };
  }
  if (has('waiting-quit', 'translation_waiting_quit')) {
    return { code: 'waiting-quit', tone: 'warn', word: '等待 Kimi 退出',
      note: '退出请求已发出；关闭完成后本软件会自动重新打开并继续，不用再退出一次或重复点击。' };
  }
  if (has('daimon-pending', 'native_import_daimon_pending', 'native_import_daimon_unreconciled',
    'native_profile_pending')) {
    return { code: 'daimon', tone: 'warn', word: '后台正在对齐模型',
      note: '模型目录已交给 Kimi，后台还没有对齐完成、暂时不能选择；后台会自动继续，不必重复点击或重启桌面。' };
  }
  if (receipt.requiresRestart === true || receipt.restartDeferred === true) {
    return { code: 'restart', tone: 'warn', word: '等待 Kimi 重启',
      note: '模型已写入原生目录，但 Kimi Work 需要重启后才会显示；空闲后会自动完成。' };
  }
  /* 只有 synchronized 声明原生 profile 已核对。已保存、已导入和已核对是三件
     不同的事：目录证据也不能被夸大成已经真实选模或已经收到回复。 */
  if (has('synchronized')) {
    return { code: 'synced', tone: 'ok', word: '已核对目录',
      note: models.length
        ? '已核对 Kimi Work 原生模型目录，共 ' + fmtCount(models.length) + ' 个模型；这不等同于已经选模或收到回复。'
        : '已核对 Kimi Work 原生模型目录；这不等同于已经选模或收到回复。' };
  }
  if (has('imported') || (info.stage === undefined && info.imported === true)) {
    return { code: 'imported', tone: 'warn', word: '已导入，待核对',
      note: '原生接口已确认导入且不欠重启，但还没有完成原生目录核对；这一步由后台继续。' };
  }
  if (has('requires-restart')) {
    return { code: 'restart', tone: 'warn', word: '等待 Kimi 重启',
      note: '模型已写入原生目录，Kimi Work 重启后才会显示。' };
  }
  if (has('login')) {
    return { code: 'login', tone: 'warn', word: '需要登录',
      note: '请先在 Kimi Work 登录并确认当前账户，然后点「打开 / 启动 Kimi」继续。' };
  }
  if (has('waiting-kimi', 'launch', 'apply')) {
    return { code: 'busy', tone: 'warn', word: '等待 Kimi',
      note: '后台正在等待 Kimi 就绪；不会打断正在进行的任务。' };
  }
  if (has('paused', 'cancelled', 'disabled')) {
    return { code: 'authorization', tone: 'warn', word: '接入已暂停',
      note: '自动接入已暂停。点「打开 / 启动 Kimi」或「重新准备接入」可恢复；不会反复请求授权。' };
  }
  if (has('importing')) {
    return { code: 'importing', tone: 'warn', word: '正在导入', note: '正在把模型写入 Kimi 原生目录。' };
  }
  if (has('models')) {
    return { code: 'pending', tone: 'neutral', word: '等待模型',
      note: '还没有可导入的模型；先保存至少一个真实模型。' };
  }
  const busyReason = has('native_bridge_busy', 'waiting-idle') || info.desktop?.state === 'waiting-idle'
    || info.desktop?.state === 'waiting-quit' || info.state === 'pending';
  if (busyReason) {
    return { code: 'busy', tone: 'warn', word: '等待 Kimi 空闲',
      note: 'Kimi 正在处理任务或尚未退出；空闲后会自动同步，不会打断正在进行的请求。' };
  }
  if (info.state === 'conflict') {
    return { code: 'conflict', tone: 'bad', word: '接入冲突',
      note: '原生配置中已有其他来源的接入，未覆盖。请核对后重试；本产品不会替你覆盖。' };
  }
  if (!models.length) {
    return { code: 'empty', tone: 'neutral', word: '等待模型',
      note: '还没有保存任何模型。先加入至少一个真实模型，同步才有内容可导入。' };
  }
  return { code: 'pending', tone: 'neutral', word: '已保存，待接入',
    note: '模型目录已保存，但还没有交给 Kimi 完成核对；用「打开 / 启动 Kimi」继续，后台会在空闲时接入。' };
}

function nativeConnectionText(data) {
  const info = data.integration || {};
  const preparation = info.preparation || {};
  if (preparation.state === 'preparing') return '正在准备首次接入，请处理系统授权窗口。';
  if (preparation.state === 'cancelled') return '上次系统授权已取消。点「打开 / 启动 Kimi」或「重新准备接入」可继续。';
  if (preparation.state === 'failed' || preparation.state === 'interrupted') return '接入准备未完成，请检查系统授权后重试。';
  const importReceipt = info.nativeImport || {};
  if (importReceipt.requiresRestart === true || importReceipt.restartDeferred === true) {
    return '模型已接入 Kimi 原生选择器，但需要重启 Kimi Work 后才会生效。';
  }
  /* 系统直接启动的 Kimi 不是首次使用：模型配置可能已在磁盘上，只是没有受管进程。
     落到末尾兜底会把它说成「首次使用」，反而让用户重复点启动。 */
  if (data?.kimiProcess?.running === true && info.desktop?.managed === false) {
    return info.imported && info.readback
      ? '当前 Kimi 由系统直接启动，未受本产品托管；已同步的模型配置仍保留在磁盘。点「正常退出并重启 Kimi」可恢复完整受管能力。'
      : '当前 Kimi 由系统直接启动，未受本产品托管。点「正常退出并重启 Kimi」后，模型接入会在受管启动中继续。';
  }
  if (info.imported && info.readback) return info.adopted ? '目录、账号与配置代次已读回；官方菜单采用请在 Kimi 窗口确认。' : '目录、账号与地址已读回，等待官方运行时采用。';
  if (info.imported) return '原生接口已接受模型目录，等待目录、账号与地址读回。';
  const reason = info.nativeImport?.reason || info.desktop?.reason || info.reason;
  const reasons = {
    work_storage_contract_unverified: '当前官方版本的不扫码隔离尚未验证，已恢复原数据位置。请先正常使用官方 Kimi，等待兼容更新。',
    work_storage_target_conflict: '独立 Work 目录与原数据目录重叠，未更改原数据位置。请使用独立的增强服务数据目录。',
    work_storage_restore_conflict: 'Work 数据位置已被其他操作改动，未覆盖。请先正常退出 Kimi，并核对数据位置后再继续。',
    work_storage_isolation_busy: '另一份增强服务正在使用当前登录，请先结束那次隔离会话。',
    work_storage_restore_failed: '原来的 Work 数据位置尚未恢复。请正常退出 Kimi 后重新打开本增强服务；恢复记录已保留。',
    work_storage_journal_invalid: 'Work 数据位置的恢复记录无法验证，已暂停启动，请检查诊断信息。',
    native_byok_dns_unavailable: '本机单域名 DNS 端口被占用或不可用。请停止占用该端口的旧预览或其他接入实例，再重新准备。',
    native_byok_dns_stop_failed: '单域名 DNS 未能正常停止。请查看诊断并重新检查端口状态。',
    native_byok_intranet_unavailable: '指定域名的私网解析尚未生效。请重新准备原生入口；若仍失败，请检查诊断中的 DNS 状态。',
    native_byok_reveal_failed: '官方功能标记未能写入或读回，未继续导入。请检查诊断信息。',
    invalid_setup_flag: '官方功能标记文件不可读或格式无效，未继续导入。',
    kimi_data_missing: '未找到官方 Kimi 数据目录。请先正常启动官方 Kimi，再重新准备。',
    setup_report_missing: '系统解析操作没有返回可验证的结果，未继续导入。',
    unsupported_hosts_encoding: '系统 hosts 文件编码不受支持，未修改原文件。',
    hosts_verify_failed: '系统 hosts 文件写入后校验失败，请查看诊断信息。',
    resolver_conflict: '检测到不是本工具写入的解析配置，未覆盖或移除。请先核对现有配置。',
    setup_cancelled: '系统管理员授权已取消，未继续接入。需要时可重新点击准备。',
    native_setup_failed: '系统解析准备未完成，原配置未被故意覆盖。请查看诊断后重试。',
    existing_login_profile_invalid: '无法安全使用已有的官方登录目录，已暂停启动。',
    existing_login_profile_unavailable: '未找到可用的官方登录目录，请先正常打开官方 Kimi。',
    profile_mode_change_requires_exit: '切换登录方式前，请先正常退出当前 Kimi，再选择所需方式启动。',
    native_bridge_unavailable: '正在等待 Kimi 接入通道，空闲后会自动恢复。',
    native_bridge_waiting_settings: '正在等待 Kimi 设置页完成原生导入。',
    native_bridge_waiting_launch: '等待 Kimi 启动并建立接入通道。',
    native_import_waiting_login: '请先在 Kimi Work 登录并确认当前账户，然后点「打开 / 启动 Kimi」继续。',
    native_import_disconnected: '检测到原生接入已断开。确认需要恢复后，点「打开 / 启动 Kimi」或「重新准备接入」。',
    native_scope_unverified: '当前自定义 Kimi 目录无法确认归属，自动接管已暂停。',
    account_changed: '当前 Kimi 账户已变化，请核对账户后点「打开 / 启动 Kimi」继续。',
    native_import_account_changed: '当前 Kimi 账户已变化，请核对账户后点「打开 / 启动 Kimi」继续。',
    native_bridge_busy: 'Kimi 正在处理任务，空闲后会自动同步。',
    native_import_retry_pending: '正在等待上一次接入完成。',
  };
  if (reason === 'native_import_manual_exit_required') {
    return '只需从 Kimi 托盘完全退出一次；退出后会自动重拉并继续，不用再点一次。';
  }
  if (reason === 'native_import_daimon_pending' || reason === 'native_import_daimon_unreconciled'
    || reason === 'native_profile_pending') {
    return '模型暂时还不能选择；后台会自动对齐，不用重复点击或重启桌面。';
  }
  if (reasons[reason]) return reasons[reason];
  if (reason === 'waiting-login') return '请在刚打开的当前 Kimi 窗口登录；本产品不会复制原账号 Cookie 或令牌。';
  if (info.state === 'account-unverified') return '等待在当前 Kimi 窗口登录并确认账号，再点「重新准备接入」。';
  if (info.state === 'pending') return '原生目录已接受，等待 Kimi 内核完成切换。';
  if (info.state === 'conflict') return '接入配置存在冲突，已保留当前内容；请检查配置后重试。';
  if (info.state === 'account-unverified') return '请登录 Kimi Work 并确认当前账户，然后点「打开 / 启动 Kimi」继续。';
  if (info.desktop?.state === 'waiting-idle' || info.desktop?.state === 'waiting-quit') return '等待 Kimi 空闲后恢复接入。';
  return '正在检查模型接入；首次使用请点「打开 / 启动 Kimi」，在打开的 Kimi Work 中登录。';
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
      showToast('读取上游目录失败：' + (discoveryReasonText(discovery.reason) || '请稍后再试')
        + (Number.isInteger(discovery.httpStatus) ? '（HTTP ' + discovery.httpStatus + '）' : ''), 'error');
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
        setText(dom.onboardDiscoverState, discoveryReasonText(discovery.reason)
          + (Number.isInteger(discovery.httpStatus) ? '（HTTP ' + discovery.httpStatus + '）' : ''));
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

function renderOverview(data) {
  setText($('native-state'), nativeConnectionText(data));
  const scope = desktopScopeInfo(data);
  setHidden(dom.nativeRemote, !scope);
  if (scope) {
    setDot(dom.nativeRemoteDot, scope.tone);
    setText(dom.nativeRemoteWord, scope.word);
    setText(dom.nativeRemoteNote, scope.note);
  }
  const gw = (data && data.gateway) || {};
  const running = gw.state === 'running';
  setDot(dom.factGatewayDot, running ? 'ok' : 'bad');
  setText(dom.factGateway, running ? '运行中' : '已停止');
  setText(dom.factGatewayUrl, gw.url || '—');

  const models = Array.isArray(data.models) ? data.models : [];
  setText(dom.factModels, fmtCount(models.length) + ' 个模型');
  const rows = connectionRowsOf(data);
  setText(dom.factModelsSub, '来自 ' + fmtCount(rows ? rows.filter((row) => !row.legacy).length : 0) + ' 条模型服务');

  const info = configInfo(data);
  setDot(dom.factConfigDot, info.tone);
  setText(dom.factConfig, info.word);
  setText(dom.factConfigFile, (data.configuration && data.configuration.file) || '—');

  const metrics = data.metrics || {};
  setText(dom.metricRequests, fmtCount(metrics.requests || 0));
  setText(dom.metricFailures, fmtCount(metrics.failures || 0));
  setText(dom.metricUptime, typeof metrics.startedAt === 'number' ? fmtDuration(Date.now() - metrics.startedAt) : '—');
  renderRecent(Array.isArray(metrics.recent) ? metrics.recent : []);
  renderRoutingFacts(data);
}

/* 概览只汇报可从状态中直接读到的路由事实：连接数、当前子代理路由与上游 ID、
   上下文策略状态、原生工具可用性。任何未知值都写「未知」而不是留空。 */
function renderRoutingFacts(data) {
  const rows = connectionRowsOf(data);
  const modelCount = Array.isArray(data.models) ? data.models.length : 0;
  if (!rows) {
    setText(dom.factConnections, '旧版单上游');
    setText(dom.factConnectionsSub, modelCount + ' 个模型路由到同一个上游');
  } else {
    const named = rows.filter((row) => !row.legacy);
    setText(dom.factConnections, named.length + ' 条连接' + (rows.some((row) => row.legacy) ? ' + 旧版' : ''));
    const unset = rows.filter((row) => !row.apiKeySet).length;
    setText(dom.factConnectionsSub, unset ? unset + ' 条连接的 Key 未在运行环境中' : '所有连接的 Key 已就绪');
  }
}

function requestErrorText(row) {
  const code = row?.error || row?.failure?.reason || '';
  const status = row?.failure?.status ?? row?.status;
  const side = row?.failure?.side ?? row?.side;
  const type = row?.upstreamErrorType || row?.failure?.upstreamType || '';
  const upstream = side === 'upstream' || code.startsWith('upstream_');
  let text = '';
  if (code === 'budget_limit_reached') {
    return '本机网关已拒绝请求。请在「用量 → 预算保护」查看上限与重置时间。';
  }
  if (type === 'insufficient_quota' || code === 'upstream_rate_limited' || (upstream && status === 429)) {
    text = '上游限流或账户额度不足，请稍后重试或检查账户额度';
  } else if (code === 'upstream_unauthorized' || code === 'upstream_forbidden' || (upstream && (status === 401 || status === 403))) {
    text = '上游 API 密钥无效、已过期或无权访问，请重新保存连接密钥';
  } else if (code === 'upstream_connection_failed' || code === 'upstream_timeout' || code === 'response_stream_interrupted' || side === 'connection') {
    text = '无法连接上游或响应中断，请检查连接地址、网络和服务状态';
  } else if (upstream && status >= 500) {
    text = '上游服务暂时异常，请稍后重试';
  }
  return text && code ? text + '（' + code + '）' : code;
}

let recentSignature = '';

function renderRecent(recent) {
  setText(dom.recentEmpty, '尚无推理请求。Kimi 开始调用模型后，这里会出现最近的 20 条记录。');
  const signature = JSON.stringify(recent);
  if (signature === recentSignature) {
    setHidden(dom.recentEmpty, recent.length > 0);
    return;
  }
  recentSignature = signature;
  const rows = recent.map((row) => {
    const tr = document.createElement('tr');
    const time = document.createElement('td');
    time.className = 'mono';
    time.textContent = fmtTime(row && row.at);
    const model = document.createElement('td');
    model.className = 'mono cell-id cell-model';
    model.textContent = (row && (row.upstreamModelId || row.model)) || '—';
    if (row && row.connectionId) {
      const connection = document.createElement('span');
      connection.className = 'cell-name';
      connection.textContent = row.connectionId;
      model.append(connection);
    }
    const protocol = document.createElement('td');
    protocol.className = 'col-proto';
    protocol.textContent = row && row.protocol === 'chat' ? 'chat' : 'responses';
    const status = document.createElement('td');
    status.className = 'num mono';
    status.textContent = row && typeof row.status === 'number' ? String(row.status) : '—';
    status.dataset.tone = row && row.status >= 400 ? 'bad' : 'neutral';
    /* 首字节不是首 token：流式首个事件才算 TTFT，这里只报告实际测到的耗时。 */
    const firstByte = document.createElement('td');
    firstByte.className = 'num mono col-firstbyte';
    firstByte.textContent = row && typeof row.firstByteMs === 'number' ? fmtMs(row.firstByteMs) : '未测';
    const total = document.createElement('td');
    total.className = 'num mono col-total';
    total.textContent = row && typeof row.ms === 'number' ? fmtMs(row.ms) : '—';
    const usage = document.createElement('td');
    usage.className = 'num mono col-usage';
    usage.textContent = usageText(row && row.usage);
    usage.setAttribute('title', usage.textContent);
    /* 平均输出速率是整段请求的平均值，含等待首字节的时间；不是 TPOT，也不是
       首 token 速度。没有可测的完成请求就没有速率，写「未提供」而不是 0。 */
    const rate = document.createElement('td');
    rate.className = 'num mono col-rate';
    rate.textContent = rateText(row && row.rate);
    rate.setAttribute('title', rateText(row && row.rate));
    /* 上下文占用是**上一次请求**的输入量，不是运行中会话的实时占用。 */
    const requestContext = document.createElement('td');
    requestContext.className = 'num mono col-reqctx';
    requestContext.textContent = requestContextText(row && row.context);
    requestContext.setAttribute('title', requestContextText(row && row.context));
    if (row && (row.error || row.failure?.reason)) {
      const error = document.createElement('span');
      error.className = 'cell-error';
      error.textContent = requestErrorText(row);
      status.append(error);
    }
    tr.append(time, model, protocol, status, firstByte, total, rate, usage, requestContext);
    return tr;
  });
  if (dom.recentBody) dom.recentBody.replaceChildren(...rows);
  setHidden(dom.recentEmpty, rows.length > 0);
}

/* 速率只认契约里的字段与口径；`basis` 不认识时不猜单位，直接说明未提供。 */
function rateText(rate) {
  if (!rate || typeof rate !== 'object') return '未提供';
  if (rate.basis !== 'total_request_time') return '未提供';
  const value = Number(rate.averageOutputTokensPerSecond);
  if (!Number.isFinite(value) || value <= 0) return '未提供';
  return (value >= 100 ? String(Math.round(value)) : value.toFixed(1)) + ' tok/s';
}

/* `context.limitTokens` 是这条模型**已设定的上下文窗口**，不是上游硬上限
   `contextLimit`：所以这里只说「占预算多少」，不写成「距上限还剩多少」。
   比例优先用后端算好的值，缺失时才用两个实测数相除。 */
function requestContextText(context) {
  if (!context || typeof context !== 'object') return '未提供';
  const used = Number(context.usedTokens);
  const budget = Number(context.limitTokens);
  if (!Number.isFinite(used) || used < 0) return '未提供';
  const head = fmtCount(used) + ' tokens';
  if (!Number.isFinite(budget) || budget <= 0) return head + ' · 预算未知';
  const ratio = Number.isFinite(Number(context.ratio)) ? Number(context.ratio) : used / budget;
  return head + ' · 占预算 ' + Math.round(ratio * 100) + '%（' + fmtK(budget) + '）';
}

/* 用量只在收到上游真实 usage 时显示；缺失时写「未提供」，绝不当成 0。 */
function usageText(usage) {
  if (!usage || typeof usage !== 'object') return '未提供';
  const input = Number.isFinite(usage.inputTokens) ? usage.inputTokens
    : Number.isFinite(usage.prompt_tokens) ? usage.prompt_tokens : null;
  const output = Number.isFinite(usage.outputTokens) ? usage.outputTokens
    : Number.isFinite(usage.completion_tokens) ? usage.completion_tokens : null;
  if (input === null && output === null) return '未提供';
  return (input === null ? '?' : fmtCount(input)) + ' / ' + (output === null ? '?' : fmtCount(output));
}

/* ---------- render: operation ---------- */

function isInFlight(op) {
  return Boolean(op && op.state === 'running');
}

function operationNote(op) {
  if (op && op.state === 'waiting-kimi-exit') {
    return '检测到另一个 Kimi 实例。点击「正常退出并重启 Kimi」后，产品会发送正常退出请求并完成受管重启。';
  }
  if (op?.action === 'open-work' && op.state === 'failed') return '请从官方 Kimi 窗口进入 Work，或检查接入状态。';
  const code = op && typeof op.reason === 'string' && op.reason ? op.reason : '';
  if (!code) return '';
  return reasonText(code);
}

function renderOperation(data) {
  const op = data && data.operation;
  if (!op) {
    setHidden(dom.opbar, true);
    return;
  }
  const label = ACTION_LABEL[op.action] || op.action || '操作';
  const word = ACTION_STATE[op.state] || op.state || '未知';
  const running = isInFlight(op);
  const bad = op.state === 'failed' || op.state === 'interrupted' || op.state === 'unavailable' || op.state === 'incompatible';
  setHidden(dom.opbar, false);
  setDot(dom.opbarDot, running ? 'warn' : bad ? 'bad' : 'ok');
  setText(dom.opbarName, label + ' · ' + word);
  setText(dom.opbarNote, operationNote(op) || '');
  dom.opbar.dataset.running = running ? 'true' : 'false';
}

function renderActions(data) {
  if (data && !onboardBusy()) renderOnboard();
  if (!actionButtons.length) return;
  const running = isInFlight(data && data.operation);
  const busy = running || state.modelsSaving || state.connsSaving;
  const ready = configReady(data);
  const alreadyRunning = data?.kimiProcess?.running === true;
  for (const button of actionButtons) {
    const reuseLogin = button.dataset.profileMode === 'existing-authorized';
    if (button.id === 'native-launch') {
      button.dataset.action = alreadyRunning ? 'restart-kimi' : 'launch-kimi';
    }
    const action = button.dataset.action;
    if (reuseLogin) setHidden(button, data?.profile?.canReuseLogin !== true);
    const needsReady = action === 'launch-kimi' || action === 'restart-kimi';
    const pending = state.pendingAction === action;
    button.disabled = busy || (needsReady && (!ready || (reuseLogin && alreadyRunning)));
    if (button.id === 'native-launch') setText(button, ACTION_LABEL[action]);
    if (reuseLogin) setText(button, '诊断：沿用登录，隔离 Work');
    button.classList.toggle('is-busy', pending);
    button.setAttribute('aria-busy', pending ? 'true' : 'false');
  }
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
      showToast(discoveryReasonText(discovery.reason)
        + (Number.isInteger(discovery.httpStatus) ? '（HTTP ' + discovery.httpStatus + '）' : ''), 'error');
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

function iconUse(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#' + name);
  svg.append(use);
  return svg;
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

function syncSelectOptions(select, options, current) {
  if (!select) return;
  const signature = JSON.stringify(options) + '#' + (current || '');
  if (select.dataset.signature === signature) {
    select.value = current || '';
    return;
  }
  select.dataset.signature = signature;
  select.replaceChildren(...options.map((option) => {
    const node = document.createElement('option');
    node.value = option.value;
    node.textContent = option.label;
    return node;
  }));
  select.value = current || '';
}

function syncDefaultOptions(list, current) {
  if (!dom.mDefault) return;
  const options = [{ value: '', label: '自动' }].concat(list.map((value) => ({ value, label: value })));
  if (current && !list.includes(current)) options.push({ value: current, label: current + '（未声明）' });
  const signature = JSON.stringify(options) + '#' + current;
  if (dom.mDefault.dataset.signature === signature) {
    dom.mDefault.value = current || '';
    return;
  }
  dom.mDefault.dataset.signature = signature;
  dom.mDefault.replaceChildren(...options.map((option) => {
    const node = document.createElement('option');
    node.value = option.value;
    node.textContent = option.label;
    return node;
  }));
  dom.mDefault.value = current || '';
}

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
          ? modelIdFromAlias(configured.modelAlias) || configured.modelAlias
          : null
      : null,
    effort: mode === 'force' && typeof configured.effort === 'string' ? configured.effort : null,
    models: mode === 'pool' && Array.isArray(configured.models)
      ? configured.models
        .filter((row) => row && typeof row.id === 'string' && typeof row.description === 'string')
        .map((row) => ({ id: row.id, description: row.description }))
      : [],
  };
}

function draftFromSaved(data) {
  const saved = savedTarget(data);
  return { mode: saved.mode, model: saved.model, effort: saved.effort,
    models: saved.models.map((row) => ({ ...row })) };
}

function sameTarget(a, b) {
  if (!a || !b || a.mode !== b.mode) return false;
  if (a.mode === 'inherit') return true;
  if ((a.model || null) !== (b.model || null)) return false;
  if (a.mode === 'pool') {
    const left = Array.isArray(a.models) ? a.models : [];
    const right = Array.isArray(b.models) ? b.models : [];
    return left.length === right.length && left.every((row, index) => row && right[index]
      && row.id === right[index].id && row.description === right[index].description);
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
  else if (!isCapable(model)) problems.push('所选模型未声明工具调用，不能作为子代理。');

  if (draft.mode === 'pool') {
    const pool = Array.isArray(draft.models) ? draft.models : [];
    const ids = new Set(pool.map((row) => row && row.id));
    if (!capable.length) problems.push(SUBAGENT_CAPABILITY_MISSING);
    else if (!pool.length) problems.push('候选列表为空，至少加入 1 个模型。');
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
  const signature = JSON.stringify([pool, state.subagentSaving]);
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
    const shownId = displayModelId(row.id);
    id.textContent = shownId;
    id.setAttribute('title', shownId);
    head.append(id);
    if (row.id === draft.model) {
      const flag = document.createElement('span');
      flag.className = 'tag tag--saved';
      flag.textContent = '默认';
      head.append(flag);
    }
    if (model && model.name && model.name !== row.id) {
      const name = document.createElement('span');
      name.className = 'pool__name';
      name.textContent = model.name;
      head.append(name);
    }
    const note = document.createElement('p');
    note.className = 'pool__note';
    note.textContent = row.description || '—';
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'iconbtn iconbtn--danger pool__remove';
    remove.dataset.poolRemove = row.id;
    remove.setAttribute('aria-label', '从池中移除 ' + row.id);
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
    const remaining = capable.filter((model) => !poolIds.has(model.id));
    const pendingAdd = dom.saPoolAdd && remaining.some((model) => model.id === dom.saPoolAdd.value)
      ? dom.saPoolAdd.value : '';
    syncSelectOptions(dom.saPoolAdd, [{ value: '',
      label: missingCapable ? '目录中没有声明支持工具的模型' : '选择要加入的模型',
    }].concat(remaining.map((model) => ({
      value: model.id, label: modelLabel(model, model.id) + ' · ' + displayModelId(model.id),
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
        return { value: id, label: modelLabel(model, id) + ' · ' + displayModelId(id) };
      })
      : [{ value: '', label: '先加入候选模型' }];
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
    const modelSignature = JSON.stringify([capable.map((model) => [model.id, model.name || '']), draft.model]);
    if (modelSignature !== saModelSignature) {
      saModelSignature = modelSignature;
      const options = [{ value: '',
        label: capable.length ? '选择模型' : '目录中没有声明支持工具的模型',
      }].concat(capable.map((model) => ({
        value: model.id, label: modelLabel(model, model.id) + ' · ' + displayModelId(model.id),
      })));
      if (draft.model && !capable.some((model) => model.id === draft.model)) {
        options.push({ value: draft.model, label: '（不在目录）' + displayModelId(draft.model) });
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
  setText(dom.saSavedMode, POLICY_MODE_WORD[saved.mode] || saved.mode);
  setText(dom.saSavedSub, saved.mode === 'inherit' ? '跟随主模型 · 未写入目标'
    : saved.mode === 'pool' ? (saved.model || '未指定默认') + ' · ' + saved.models.length + ' 个候选'
      : (saved.model || '未选择') + (saved.effort ? ' · 档位 ' + saved.effort : ' · 档位自动'));
  setText(dom.saPolicyMode, POLICY_MODE_WORD[policy.mode] || policy.mode || '—');
  setText(dom.saPolicySub, policy.mode === 'blocked'
    ? '策略文件里存在阻止项或外来归属冲突，未被覆盖，保存已拒绝'
    : (POLICY_TRANSPORT_WORD[policy.transport || 'native-config'] || '原生配置文件') + ' · 策略文件已读取');
  if (dom.saPolicySub) dom.saPolicySub.title = [policy.transport, policy.reason].filter(Boolean).join(' · ');
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
      + (compat ? ' 内核源码契约一致，仍需等待真实观察。' : ''));
}

/* ---------- subagent draft mutations ---------- */

function setMode(mode) {
  if (!state.draft || !configReady(state.data)) return;
  if (!['inherit', 'force', 'pool'].includes(mode) || mode === state.draft.mode) return;
  const saved = state.data ? savedTarget(state.data) : null;
  if (mode === 'inherit') {
    state.draft = { mode: 'inherit', model: null, effort: null, models: [] };
  } else if (mode === 'pool') {
    /* 从已有草稿与已保存池热身：只保留仍可用且带用途说明的成员，
       不覆盖用户已有的 pool/description，也不默认切继承。 */
    const seen = new Set();
    const seed = [];
    const push = (id, description) => {
      if (typeof id !== 'string' || !id || seen.has(id)) return;
      const model = findModel(id);
      if (!model || !isCapable(model)) return;
      const row = poolRow(id, description);
      if (!row.description) return;
      seen.add(id);
      seed.push(row);
    };
    (Array.isArray(state.draft.models) ? state.draft.models : []).forEach((row) => push(row && row.id, row && row.description));
    if (saved && Array.isArray(saved.models)) saved.models.forEach((row) => push(row.id, row.description));
    state.draft = { mode: 'pool', effort: null, models: seed,
      model: seed.some((row) => row.id === (saved && saved.model)) ? saved.model : seed.length ? seed[0].id : null };
  } else {
    const seed = (saved && saved.mode !== 'inherit' && saved.model && isCapable(findModel(saved.model)) && saved.model)
      || (state.draft.model && isCapable(findModel(state.draft.model)) && state.draft.model)
      || (capableModels()[0] ? capableModels()[0].id : null);
    state.draft = { mode: 'force', model: seed || null, effort: null, models: [] };
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
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickEffort(value) {
  if (!state.draft || state.draft.mode !== 'force') return;
  state.draft = { ...state.draft, effort: value || null };
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function pickPoolDefault(id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (!id || !poolDraftIds(state.draft).has(id)) return;
  state.draft = { ...state.draft, model: id };
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
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function removePoolModel(id) {
  if (!state.draft || state.draft.mode !== 'pool') return;
  if (!poolDraftIds(state.draft).has(id)) return;
  const models = state.draft.models.filter((row) => row.id !== id);
  state.draft = { ...state.draft, model: state.draft.model === id ? null : state.draft.model, models };
  state.subagentErrors = [];
  recomputeDraftDirty();
  renderSubagents();
}

function discardDraft() {
  if (!state.data) return;
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
      payload.models = state.draft.models;
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
    state.draft = draftFromSaved(state.data);
    state.draftBaseEntity = draftFromSaved(state.data);
    state.draftBaseRevision = state.data.revision ?? null;
    state.draftDirty = false;
    state.subagentErrors = [];
    renderAll();
    showToast(state.draft.mode === 'inherit'
      ? '已保存为继承主模型；是否采用见运行时状态。'
      : state.draft.mode === 'pool'
        ? '模型池已保存；配置写入与内核采用情况见运行时状态。'
        : '目标已保存；配置写入与内核采用情况见运行时状态。', 'ok');
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
    checkText.textContent = '检查未通过' + (Number.isInteger(check.httpStatus) ? '（HTTP ' + check.httpStatus + '）' : '')
      + '：' + (upstreamReasonText(check.reason) || '无法连接本行上游');
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
function renderWorkspaceSync(data) {
  const integration = data.integration || {};
  const models = Array.isArray(data.models) ? data.models : [];
  const imported = Object.keys(integration.aliases || {}).sort();
  const saved = models.map(model => model.id).sort();
  const matches = JSON.stringify(imported) === JSON.stringify(saved);
  const restart = matches && integration.applied && !integration.adopted;
  const synced = matches && integration.adopted;
  setText($('workspace-sync-state'), !models.length ? '尚未添加模型' : synced ? '已同步到 Work' : restart ? '需要重启 Kimi' : '待同步到 Work');
  setText($('workspace-sync-note'), !models.length ? '添加服务商并选择模型，保存后同步。' : synced ? '已读回当前模型配置，可在 Work 的模型选择器中使用。' : restart ? '模型配置已写入。正常重启 Kimi 后，在选择器中确认。' : '本机网关已保存模型。点击「同步到 Work」更新官方模型选择器。');
  setHidden($('workspace-restart'), !restart);
}

function renderAll() {
  if (!state.data) return;
  renderWorkspaceSync(state.data);
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

let trackedServiceOperation = '';

function renderPreviewLifecycle(data) {
  const current = $('upd-current');
  if (current) current.textContent = data.updates?.currentVersion || '未知预览版本';
  setText($('upd-current-sub'), '独立 Go/Wails 预览 · ' + (data.service?.buildId || 'development'));
  setText($('preview-service-state'), data.service?.state === 'running' ? '后台运行中' : '后台状态未确认');
  const operation = data.service?.operation;
  const busy = operation && ['pending', 'running'].includes(operation.phase);
  setText($('preview-service-sub'), busy ? '后台操作进行中，正在读回结果；登录启动与当前运行状态分开管理。' : data.service?.loginStartup === 'enabled' ? '已允许登录时启动；更改此偏好不会重启当前后台' : '未开启登录时启动；窗口关闭不停止网关');
  const register = $('service-register');
  if (register) register.textContent = data.service?.registered ? '更新服务注册' : '注册后台服务';
  const enable = $('service-enable');
  if (enable) enable.disabled = Boolean(busy) || data.service?.loginStartup === 'enabled';
  const disable = $('service-disable');
  if (disable) disable.disabled = Boolean(busy) || data.service?.loginStartup !== 'enabled';
  if (busy && trackedServiceOperation !== operation.id) {
    trackedServiceOperation = operation.id;
    void trackServiceOperation(operation.id, operation.action, operation);
  }
}

function applyPreviewServiceResult(result) {
  const operation = result.operation;
  if (!state.data) return operation;
  const service = result.service || operation?.state;
  if (service) {
    state.data.service = { ...state.data.service, ...service, loginStartup: service.enabled ? 'enabled' : 'manual' };
  }
  if (operation) state.data.service.operation = operation;
  renderPreviewLifecycle(state.data);
  return operation;
}

async function trackServiceOperation(id, action, initial) {
  let operation = initial;
  const deadline = Date.now() + 30000;
  try {
    while (!operation || !['succeeded', 'failed'].includes(operation.phase)) {
      if (Date.now() >= deadline) {
        showToast('后台操作尚未完成，请稍后从设置页读回结果；配置与账本未删除。', 'error');
        return;
      }
      await new Promise(resolve => setTimeout(resolve, 250));
      try {
        const result = await request('/api/service', { method: 'POST', body: { action: 'read-service-operation', operationId: id } });
        operation = applyPreviewServiceResult(result);
      } catch (error) {
        if (action === 'stop-service' || action === 'uninstall-service') {
          showToast('后台连接已断开，停止结果尚未由本窗口读回。重新打开窗口后可核对服务状态；官方 Kimi 不会因此被关闭。');
          return;
        }
        if (Date.now() >= deadline) throw error;
      }
    }
    applyPreviewServiceResult({ operation });
    if (operation.phase === 'failed') {
      showToast('后台操作未完成，请在设置页查看诊断并核对实际状态；未确认的更改不会显示为成功。', 'error');
      return;
    }
    if (action === 'enable-login-service' || action === 'disable-login-service') {
      showToast(operation.loginStart ? '已允许下次登录时启动；当前后台没有因此重启。' : '已禁止下次登录时启动；当前后台和模型请求未因此中断。', 'ok');
    } else if (action === 'register-service') {
      showToast('后台注册定义已保存；注册不等于已开启登录启动。', 'ok');
    } else {
      showToast('后台操作已完成并读回状态；官方 Kimi 不会因此被关闭。', 'ok');
    }
    try { await loadState({ silent: true }); } catch {}
  } finally {
    if (trackedServiceOperation === id) trackedServiceOperation = '';
  }
}

async function previewServiceAction(action) {
  const result = await request('/api/service', { method: 'POST', body: { action } });
  const operation = applyPreviewServiceResult(result);
  if (action === 'restart-daemon') {
    showToast('后台正在同地址重启，页面将在健康读回后恢复。官方 Kimi 不会因此被关闭。', 'ok');
    const deadline = Date.now() + 15000;
    for (;;) {
      await new Promise(resolve => setTimeout(resolve, 250));
      try {
        if (await loadState({ silent: true })) return;
      } catch {}
      if (Date.now() >= deadline) {
        showToast('后台未在限定时间内恢复；请查看设置页诊断，原配置与账本未删除。', 'error');
        return;
      }
    }
  }
  if (operation?.id) {
    trackedServiceOperation = operation.id;
    await trackServiceOperation(operation.id, operation.action, operation);
    return;
  }
  showToast('预览后台状态已更新；登录启动与进程运行分开显示。', 'ok');
  await loadState({ silent: true });
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

/* ---------- gate ---------- */

const GATE_COPY = {
  checking: {
    title: '正在读取会话',
    lede: '正在确认面板会话密钥并读取网关状态。',
    note: '',
    retry: false,
  },
  missing: {
    title: '请从应用窗口重新打开',
    lede: '请使用 Kimi Work+ 桌面应用或菜单栏托盘打开面板；应用会自动读取当前本机地址和会话密钥。',
    note: '如果窗口已打开但会话缺失，请从托盘选择「打开 Kimi Work+」刷新入口。',
  },
  expired: {
    title: '会话密钥已失效',
    lede: '后台重启后旧会话会失效。请从菜单栏托盘选择「打开 Kimi Work+」，应用会读取最新入口。',
    note: '不要手动猜测或复制其他会话的密钥。',
  },
  refused: {
    title: '无法确认面板会话',
    lede: '服务拒绝了这次请求，可能是页面来源不正确，或后台服务已停止。',
    note: '请从托盘重新打开；若持续出现，请在设置页检查后台服务和诊断信息。',
  },
};

function showGate(kind) {
  const copy = GATE_COPY[kind] || GATE_COPY.missing;
  setHidden(dom.gate, false);
  if (dom.shell) dom.shell.hidden = true;
  setText(dom.gateTitle, copy.title);
  setText(dom.gateLede, copy.lede);
  setHidden(dom.gateNote, !copy.note);
  setText(dom.gateNote, copy.note);
  setHidden(dom.gateActions, copy.retry === false);
  if (copy.retry !== false && dom.gateRetry) dom.gateRetry.focus({ preventScroll: true });
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

/* ---------- toast ---------- */

function showToast(message, tone) {
  if (state.toastTimer !== null) {
    window.clearTimeout(state.toastTimer);
    state.toastTimer = null;
  }
  dom.toast.replaceChildren();
  const icon = iconUse(tone === 'error' ? 'i-alert' : 'i-check');
  const text = document.createElement('span');
  text.textContent = message;
  dom.toast.dataset.tone = tone || 'ok';
  dom.toast.hidden = false;
  dom.toast.append(icon, text);
  requestAnimationFrame(() => {
    dom.toast.dataset.open = 'true';
  });
  state.toastTimer = window.setTimeout(() => {
    dom.toast.dataset.open = 'false';
    state.toastTimer = window.setTimeout(() => {
      dom.toast.hidden = true;
      state.toastTimer = null;
    }, 220);
  }, tone === 'error' ? 6000 : 3200);
}

/* ---------- actions ---------- */

function actionUUID() {
  if (typeof crypto !== 'undefined' && crypto && typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const hex = () => Math.floor(Math.random() * 0x100000000).toString(16).padStart(8, '0');
  return hex() + '-' + hex() + '-' + hex() + '-' + hex();
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
      showToast('上游检查通过' + (typeof diag.httpStatus === 'number' ? '：HTTP ' + diag.httpStatus : '')
        + (typeof diag.modelCount === 'number' ? '，' + diag.modelCount + ' 个模型' : '') + '。', 'ok');
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

/* ---------- events ---------- */

function onModelFilter(filter) {
  state.filter = filter;
  [['all', dom.filterAll], ['vision', dom.filterVision], ['tools', dom.filterTools], ['reasoning', dom.filterReasoning]]
    .forEach(([name, button]) => {
      if (button) button.setAttribute('aria-pressed', name === filter ? 'true' : 'false');
    });
  renderModels();
}

function bindEvents() {
  Object.keys(VIEW_COPY).forEach(view => {
    const button = $('nav-' + view);
    if (button) button.addEventListener('click', () => setView(view));
  });

  if (dom.restartBannerLink) dom.restartBannerLink.addEventListener('click', () => setView('models'));
  $('budget-banner-link')?.addEventListener('click', () => {
    setView('usage');
    const tab = $('usage-tab-budget');
    tab?.click();
    tab?.focus();
  });
  $('overview-details')?.addEventListener('click', () => setView('maintenance'));
  $('quick-models')?.addEventListener('click', () => setView('models'));
  $('quick-usage')?.addEventListener('click', () => setView('usage'));
  $('conns-usage')?.addEventListener('click', () => setView('usage'));
  document.querySelectorAll('[data-section]').forEach(button => {
    button.addEventListener('click', () => revealWorkspaceSection(button.dataset.section));
  });
  const brandHome = $('brand-home');
  if (brandHome) brandHome.addEventListener('click', () => setView('overview'));
  document.addEventListener('keydown', (event) => {
    const workIndex = $('work-index');
    if (event.key === 'Escape' && workIndex?.open) {
      event.preventDefault();
      event.stopPropagation();
      closeWorkIndex({ restoreFocus: true });
    }
  });
  const workIndex = $('work-index');
  if (workIndex) {
    // A click on the empty part of the index (or anywhere outside it) dismisses
    // it; Escape does the same and returns focus to the summary.
    workIndex.addEventListener('toggle', () => {
      if (workIndex.open) { syncMastheadHeight(); const nav = workIndex.querySelector('.nav'); if (nav) nav.scrollTop = 0; }
    });
    const nav = workIndex.querySelector('.nav');
    nav?.addEventListener('click', (event) => {
      if (event.target === nav) closeWorkIndex();
    });
    document.addEventListener('click', (event) => {
      if (!workIndex.open || workIndex.contains(event.target)) return;
      closeWorkIndex();
    });
  }

  if (dom.refresh) dom.refresh.addEventListener('click', async () => {
    dom.refresh.disabled = true;
    /* 顶栏刷新是用户明确要求「现在就是最新的」：额度卡片与用量一起重读，
       否则停在用量页时点刷新，Kimi Code 与 Magpie 的读数仍是上一轮的。 */
    try {
      const tasks = [loadState()];
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
      catch (error) { showToast(error.message || '预览后台操作未完成，原运行状态未替换。', 'error'); }
      finally { button.disabled = false; }
    });
  }
  request('/api/service').then(result => {
    if (!result.service || !state.data) return;
    state.data.service = { ...state.data.service, ...result.service, loginStartup: result.service.enabled ? 'enabled' : 'manual' };
    renderPreviewLifecycle(state.data);
  }).catch(() => {});

  if (dom.theme) dom.theme.addEventListener('change', () => applyTheme(dom.theme.value));

  /* 子代理目标 */
  if (dom.saModes) dom.saModes.addEventListener('change', (event) => {
    const target = event.target;
    if (!target || target.name !== 'mode') return;
    setMode(['force', 'pool'].includes(target.value) ? target.value : 'inherit');
  });
  if (dom.saModel) dom.saModel.addEventListener('change', () => pickModel(dom.saModel.value));
  if (dom.saEffort) dom.saEffort.addEventListener('change', () => pickEffort(dom.saEffort.value));
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
function clearModelQuery() {
  state.query = '';
  if (dom.modelQ) dom.modelQ.value = '';
  setHidden(dom.modelQClear, true);
  renderModels();
  if (dom.modelQ && dom.modelQ.focus) dom.modelQ.focus();
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
  if (typeof usagePanel !== 'undefined' && usagePanel && typeof usagePanel.reset === 'function') usagePanel.reset();
  modelSignature = '';
  recentSignature = '';
  saModelSignature = '';
  saEffortSignature = '';
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

function boot() {
  initTheme();
  if (typeof globalThis.KimiWorkGrowth?.mountGrowth === 'function') {
    renderGrowth = globalThis.KimiWorkGrowth.mountGrowth({ request, getState: () => state.data, getProviders: () => state.providers,
      refresh: loadState, notify: showToast, runAction });
  }
  if (typeof globalThis.KimiWorkUsage?.mount === 'function') {
    usagePanel = globalThis.KimiWorkUsage.mount({ request, notify: showToast });
  }
  mountWorkspace();
  /* plus 追加的容器要落在 mountWorkspace 搬完板块之后：它把旧的子页面整体移进
     view-work，先挂载会被一起搬走或与搬移目标交叉。 */
  if (typeof globalThis.KimiWorkPlus?.mount === 'function') {
    plusPanel = globalThis.KimiWorkPlus.mount({ request, notify: showToast, getState: () => state.data, setState: (data) => adoptState(data),
      onAccounts: () => { setView('maintenance'); revealWorkspaceSection('plus-accounts-panel'); } });
  }
  /* 会话密钥必须先就绪：停留在「用量」等视图时，挂载就会立刻发出第一笔请求，
     先挂视图再读密钥会让那次请求带着空密钥失败，界面上闪现「缺少面板会话密钥」。 */
  state.key = readKey();
  initView();
  bindEvents();
  observeMasthead();
  actionButtons = Array.from(document.querySelectorAll('[data-action]'));

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

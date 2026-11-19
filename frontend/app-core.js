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


const VIEW_COPY = {
  overview: { title: '总览', lede: '网关、Kimi Work 接入、当前账号与今日用量。' },
  models: { title: '模型', lede: '在这里管理服务商与模型，保存后同步到 Kimi Work。' },
  usage: { title: '用量', lede: '查看真实请求、服务商余额与 Kimi Code 额度。' },
  work: { title: 'Work 能力', lede: '管理子代理、并行、MCP、联网与长对话整理。' },
  extensions: { title: '扩展', lede: '独立管理思考翻译，关闭扩展不会影响网关与 Work。' },
  maintenance: { title: '设置', lede: '账号管理、后台服务、版本与 Kimi 接入诊断。' },
};

const VIEW_ALIASES = { connection: 'models', enhancements: 'work', subagents: 'work', tools: 'work', settings: 'work', translation: 'extensions' };

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
  'check-compatibility': '官方版本自检',
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
  compat_model_sync_blocked: '当前官方 Kimi 版本未通过模型同步兼容性检查，已停止写入模型配置；本机网关和其他功能照常运行。',
  compat_account_switch_blocked: '当前官方 Kimi 版本未通过账号接口检查，已停止账号切换写入；本机网关和其他功能照常运行。',
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
  credentials_unavailable: '系统凭据存储未接受保存，配置未提交。请检查应用权限后重试；不会写入明文。',
  credentials_readback_failed: '新密钥未能从系统凭据存储读回，未切换现有引用。请重试。',
  credential_keychain_locked: '登录钥匙串已锁定，请在 macOS 中解锁后从本窗口重新保存；密钥未写入。',
  credential_keychain_unavailable: '当前登录会话无法使用系统钥匙串，请检查应用签名和会话权限；密钥未写入。',
  credential_session_unavailable: '当前后台会话不能显示授权窗口，请从应用窗口重新保存。',
  credential_authorization_required: '此条目需要系统授权，请从应用窗口重新保存并响应系统提示。',
  credential_authorization_cancelled: '系统授权已取消，密钥未保存；原配置仍保留。',
  credential_authorization_failed: '系统未接受授权，密钥未保存；原配置仍保留。',
  credential_store_corrupt: '凭据条目内容无法识别，请从应用窗口重新输入并保存。',
  credential_interaction_not_restored: '系统凭据交互状态未能恢复，请重启后台服务后重试。',
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
  overviewOpenModels: $('overview-open-models'),
  overviewOpenUsage: $('overview-open-usage'),
  overviewTodayRequests: $('overview-today-requests'),
  overviewTodayTokens: $('overview-today-tokens'),
  overviewIssueCard: $('overview-issue-card'),
  overviewIssueDot: $('overview-issue-dot'),
  overviewIssueTitle: $('overview-issue-title'),
  overviewIssueNote: $('overview-issue-note'),
  overviewIssueAction: $('overview-issue-action'),
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
  overviewToday: { key: null, requests: null, totalTokens: null, loading: false, fetchedAt: 0, requestCount: -1 },
  overviewIssueAction: null,
  toastTimer: null,
  started: false,
  actionButtons: [],
};



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
    const error = new ApiError(ERROR_HINT[code] || message || '请求失败，请稍后重试。', code || 'http_' + res.status, res.status);
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
function postWailsMessage(message) {
  if (typeof message !== 'string' || !message) return false;
  try {
    if (globalThis.webkit && globalThis.webkit.messageHandlers && globalThis.webkit.messageHandlers.external
      && typeof globalThis.webkit.messageHandlers.external.postMessage === 'function') {
      globalThis.webkit.messageHandlers.external.postMessage(message);
      return true;
    }
    if (globalThis.chrome && globalThis.chrome.webview && typeof globalThis.chrome.webview.postMessage === 'function') {
      globalThis.chrome.webview.postMessage(message);
      return true;
    }
  } catch {
    /* ignore */
  }
  return false;
}

function reportTheme(mode) {
  const token = THEME_TOKENS[mode] || 'auto';
  const title = THEME_PREFIX + token;
  if (document.title !== title) document.title = title;
  postWailsMessage('wails:event:emit:kwp:theme:' + token);
}

function isMastheadDragTarget(target) {
  if (!target || typeof target.closest !== 'function') return false;
  if (!target.closest('.masthead')) return false;
  return !target.closest('button, a, input, select, textarea, summary, label, .plus-account-bar, .theme-field, [data-no-drag="true"]');
}

function initWindowChrome() {
  const root = document.documentElement;
  if (root && root.dataset && !root.dataset.platform) {
    const plat = (typeof navigator !== 'undefined' && (navigator.platform || navigator.userAgent)) || '';
    if (/mac/i.test(plat)) root.dataset.platform = 'mac';
    else if (/win/i.test(plat)) root.dataset.platform = 'windows';
  }
  globalThis.__kwpSyncTheme = () => {
    const current = (root && root.dataset && root.dataset.theme) || 'auto';
    reportTheme(current);
  };
  const masthead = typeof document.querySelector === 'function' ? document.querySelector('.masthead') : null;
  if (!masthead || masthead.dataset.dragBound === 'true') return;
  masthead.dataset.dragBound = 'true';
  masthead.addEventListener('mousedown', (event) => {
    if (event.button !== 0 || event.detail !== 1) return;
    if (!isMastheadDragTarget(event.target)) return;
    postWailsMessage('wails:drag');
  });
  masthead.addEventListener('dblclick', (event) => {
    if (event.button !== 0) return;
    if (!isMastheadDragTarget(event.target)) return;
    postWailsMessage('wails:drag:doubleclick');
  });
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


function iconUse(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#' + name);
  svg.append(use);
  return svg;
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

export {
  $,
  ACTION_LABEL,
  ACTION_STATE,
  API,
  ApiError,
  BUDGET_PRESETS,
  CONFIG_STATE,
  CONNECTION_ID_PATTERN,
  DIAG_REASON,
  DISCOVERY_REASON,
  EFFORT_PATTERN,
  KEY_STORE,
  LOOPBACK,
  MODEL_ALIAS_PREFIXES,
  POLICY_MODE_WORD,
  POLICY_TRANSPORT_WORD,
  POLL_MS,
  RUNTIME_STATE,
  UPSTREAM_REASON,
  VIEW_ALIASES,
  VIEW_COPY,
  VIEW_STORE,
  actionUUID,
  applyTheme,
  clearKey,
  displayModelId,
  dom,
  fmtCount,
  fmtDuration,
  fmtK,
  fmtMs,
  fmtTime,
  fmtTokens,
  iconUse,
  initTheme,
  initWindowChrome,
  modelLabel,
  mountUsageRequests,
  observeMasthead,
  readKey,
  reasonText,
  renderErrorList,
  request,
  revealWorkspaceSection,
  setConnErrors,
  setDot,
  setHidden,
  setText,
  shortRev,
  showGate,
  showToast,
  state,
  syncDefaultOptions,
  syncSelectOptions,
  truncate,
};

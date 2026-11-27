const API = {
  state: '/api/state',
  workspace: '/api/workspace',
  models: '/api/models',
  connection: '/api/connection',
  connections: '/api/connections',
  connectionsDelete: '/api/connections/delete',
  connectionsProbe: '/api/connections/probe',
  subagents: '/api/subagents',
  translation: '/api/translation',
  actions: '/api/actions',
  workConversations: '/api/work/conversations',
  rebindConversations: '/api/work/conversations/rebind',
  providers: '/api/providers',
  modelSearch: '/api/model-search',
  discovery: '/api/discovery',
  context: '/api/context',
  tools: '/api/tools',
  integrationMode: '/api/integration-mode',
  autoTakeover: '/api/auto-takeover',
  diagnosticsExport: '/api/diagnostics/export',
  openUrl: '/api/open-url',
  updatesCheck: '/api/updates/check',
  updatesDownload: '/api/updates/download',
  updatesSettings: '/api/updates/settings',
  updatesCancel: '/api/updates/cancel',
  updatesRollback: '/api/updates/rollback',
};

const POLL_MS = 2000;
const KEY_STORE = 'kwg.panel.key';
const THEME_STORE = 'kwg.panel.theme';
const VIEW_STORE = 'kwg.panel.view';
const SUBTAB_STORE = 'kwg.panel.subtabs';
const GUIDE_DISMISSED_STORE = 'kwg.panel.guide.dismissed';
const MODEL_ALIAS_PREFIXES = ['byok-kth-kimi/', 'byok-kth-resp/'];
/* Kimi 3.2.28 的自定义源别名是 `byok-<uuid>/<modelId>`；UUID 每台机器不同，
   所以只能按形状匹配，不能写死成一个值。 */
const MODEL_ALIAS_UUID_PREFIX = /^byok-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\//i;
const INTEGRATION_MODES = ['gateway', 'direct'];
const DIRECT_SKIP_REASON = {
  tools_required: '未声明工具调用',
  missing_connection: '未绑定连接',
  invalid_base_url: '地址不支持直连',
  invalid_context_window: '上下文窗口无效',
  missing_api_key: '未配置密钥',
  duplicate_endpoint: '端点与协议重复',
  unsupported_protocol: '协议不支持直连',
};
const DIRECT_BYPASS_WORD = {
  budget: '预算限制',
  failover: '备用故障转移',
  shared_instructions: '共享指令',
  web_tools: '网关侧联网搜索',
};

/* 别名（`byok-kth-kimi/x`、`byok-<uuid>/x`）在显示与查找时都应还原成路由 ID；
   其他 `byok-` 开头的名字（例如 `byok-custom/`）属于用户自己的命名，原样保留。 */
function stripModelAliasPrefix(value) {
  if (typeof value !== 'string' || !value) return value;
  for (const prefix of MODEL_ALIAS_PREFIXES) {
    if (value.startsWith(prefix)) {
      const rest = value.slice(prefix.length);
      if (rest) return rest;
    }
  }
  return value.replace(MODEL_ALIAS_UUID_PREFIX, '');
}
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


const VIEWS = ['overview', 'models', 'conversations', 'accounts', 'usage', 'maintenance'];

const VIEW_COPY = {
  overview: { title: '总览', lede: '网关与 Kimi Work 接入状态、当前账号、子代理与今日用量。' },
  models: { title: '模型', lede: '管理服务商连接、已启用模型与子代理角色分配；保存后立即生效并自动同步到 Kimi Work。' },
  conversations: { title: '会话', lede: '查看 Kimi Work 本地会话，直接为已有对话切换模型或批量迁移历史模型记录。' },
  accounts: { title: '账号', lede: '保存、切换与管理本机官方 Kimi 多账号凭据。' },
  usage: { title: '用量', lede: '查看本机网关与 Kimi Work 会话用量、服务商余额与预算保护。' },
  maintenance: { title: '设置', lede: 'Kimi 接入模式、扩展增强、版本更新与诊断排障。' },
};

const VIEW_ALIASES = {
  connection: 'models',
  connections: 'models',
  provider: 'models',
  providers: 'models',
  subagents: 'models',
  work: 'models',
  enhancements: 'models',
  accounts: 'accounts',
  account: 'accounts',
  conversations: 'conversations',
  conversation: 'conversations',
  sessions: 'conversations',
  session: 'conversations',
  rebind: 'conversations',
  tools: 'maintenance',
  extensions: 'maintenance',
  translation: 'maintenance',
  settings: 'maintenance',
};

/* 用量面板整体重写 #usage-root，因此「最近请求」只能在面板 mount 之后
   移入它的请求页；按父节点判断，重复调用不会产生第二份表。 */
function mountUsageRequests() {
  const pane = $('usage-pane-requests');
  const section = $('recent-table')?.closest('section');
  if (!pane || !section || section.parentElement === pane) return;
  pane.append(section);
}

/* 显式滚动 .main 容器，禁止调用 scrollIntoView()，避免 macOS WKWebView 将外层视口
   连同顶栏 .masthead 一起向上滚出视野。 */
function scrollMainToElement(target) {
  const main = $('main');
  if (!main || !target) return;
  if (typeof target.getBoundingClientRect === 'function' && typeof main.getBoundingClientRect === 'function') {
    const mainRect = main.getBoundingClientRect();
    const targetRect = target.getBoundingClientRect();
    const top = Math.max(0, (main.scrollTop || 0) + (targetRect.top - mainRect.top) - 16);
    main.scrollTop = top;
  } else {
    main.scrollTop = 0;
  }
}

function setSubtab(group, tab, { persist = true } = {}) {
  if (!group || !tab) return;
  state.subtabs = { ...(state.subtabs || {}), [group]: tab };
  if (typeof document !== 'undefined' && typeof document.querySelectorAll === 'function') {
    document.querySelectorAll(`[data-subtab-group="${group}"][data-subtab]`).forEach((btn) => {
      const selected = btn.dataset.subtab === tab;
      btn.setAttribute('aria-selected', String(selected));
      btn.setAttribute('tabindex', selected ? '0' : '-1');
    });
    document.querySelectorAll(`[data-subtab-group="${group}"][data-subtab-pane]`).forEach((pane) => {
      pane.hidden = pane.dataset.subtabPane !== tab;
    });
  }
  const main = $('main');
  if (main) main.scrollTop = 0;
  if (persist) {
    try {
      sessionStorage.setItem(SUBTAB_STORE, JSON.stringify(state.subtabs));
    } catch {
      /* 忽略 */
    }
  }
}

/* 跨区块定位：若目标位于某个二级标签页或折叠区中，先激活对应二级页签并展开所有祖先
   details，再只滚动 .main 内部容器。 */
function revealWorkspaceSection(id) {
  const target = id ? $(id) : null;
  if (!target) return null;
  for (let node = target; node; node = node.parentElement) {
    if (node.dataset && node.dataset.subtabGroup && node.dataset.subtabPane) {
      setSubtab(node.dataset.subtabGroup, node.dataset.subtabPane);
    }
    if (node.tagName === 'DETAILS') node.open = true;
  }
  scrollMainToElement(target);
  const heading = (typeof target.querySelector === 'function' && target.querySelector('h2, h3, summary')) || target;
  if (typeof heading.focus === 'function') {
    if (typeof heading.hasAttribute === 'function' && !heading.hasAttribute('tabindex')) {
      heading.setAttribute('tabindex', '-1');
    }
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
  'disconnect-models': '移除接管并还原',
  'check-upstream': '上游检查',
  'apply-connections': '应用连接',
  'account-capture': '保存当前账号',
  'account-begin-add': '添加账号',
  'account-complete-add': '保存新账号',
  'account-cancel-add': '恢复原账号',
  'account-switch': '切换账号',
  'account-remove': '移除账号',
  'set-swarm': '设置并行模式',
  'rebind-conversations': '改绑旧会话模型',
  'auto-sync': '自动同步模型目录',
  'auto-takeover': '自动重新接管 Kimi',
  'check-compatibility': '官方版本自检',
};

const ACTION_STATE = {
  running: '进行中',
  prepared: '已准备',
  starting: '启动请求已发出',
  'waiting-kimi-exit': '等待 Kimi 退出',
  'waiting-idle': '等待 Kimi 空闲',
  'waiting-quit': '等待 Kimi 正常退出',
  'waiting-login': '等待登录 Kimi',
  waiting_login: '等待登录 Kimi',
  'waiting-work': '等待 Kimi Work 就绪',
  'waiting-settings': '等待 Kimi 设置页',
  'waiting-unmanaged': '待重启接管后同步',
  waiting_unmanaged: '待重启接管后同步',
  'requires-restart': '需正常重启 Kimi',
  requires_restart: '待重启 Kimi 生效',
  /* 后台 auto-sync 的收敛态：完成后 action 仍是 auto-sync，状态是这些值。 */
  synced: '已同步',
  scheduled: '等待同步',
  syncing: '正在同步',
  deferred_offline: '待打开 Kimi 时同步',
  idle: '已就绪',
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
  configuration_unavailable: '尚未读取到本机配置，请刷新状态后重试。',
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
  not_installed: '尚未安装官方 Kimi。请先安装并正常打开官方 Kimi，再回到本面板继续。',
  application_not_found: '官方 Kimi 应用入口不可用。请重新安装官方 Kimi 后再试。',
  base_url_changed: '检测到本机网关地址变化，正在通过官方接口恢复已有接入。',
  existing_login_profile_invalid: '官方登录目录不可用。请先正常打开官方 Kimi，再从面板重新启动。',
  existing_login_profile_unavailable: '未找到官方登录目录。请先正常打开官方 Kimi。',
  compat_model_sync_blocked: '当前官方 Kimi 版本未通过模型同步兼容性检查，已停止写入模型配置；本机网关和其他功能照常运行。',
  compat_account_switch_blocked: '当前官方 Kimi 版本未通过账号接口检查，已停止账号切换写入；本机网关和其他功能照常运行。',
  invalid_integration_mode: '接入模式不被接受，只支持本机网关或零网关直连；当前模式未更改。',
  model_disconnect_failed: '模型来源未能移除，请刷新状态后重试。',
  model_disconnect_feature_disabled: '官方 Kimi 的自定义模型入口当前未就绪，暂时无法读取官方模型来源。请先点「重新同步模型目录」准备本机解析，或重启官方 Kimi 后再试。',
  model_disconnect_no_account: '官方 Kimi 当前尚未确认登录账号，未继续移除模型来源。请先在官方窗口完成登录后重试。',
  model_disconnect_byok_not_ready: '官方 Kimi 的模型来源服务尚未就绪，请稍候或正常重启官方 Kimi 后再试。',
  model_disconnect_unsupported: '当前官方 Kimi 不支持移除模型来源；请升级官方 Kimi 后重试。',
  model_disconnect_contract_changed: '官方模型来源接口返回了无法确认的结果，未继续操作。请查看诊断信息。',
  model_disconnect_source_conflict: '接管记录对应的模型来源已被其他配置替换，未移除。请先核对官方模型来源。',
  model_disconnect_not_verified: '移除请求已发出，但读回仍发现本产品接管的来源。请刷新状态后重试。',
  model_disconnect_local_state_failed: '官方来源已处理，但本地接管记录未能完成更新。请重试以完成还原。',
  model_disconnect_deferred: '官方模型来源已提交移除，但 Kimi Work 正在处理任务，尚未生效。请等任务结束或正常重启 Kimi 后，再点一次「移除接管并还原」完成确认。',
  model_disconnect_apply_failed: '官方模型来源已从列表移除，但写入 Kimi 配置失败。请刷新状态或正常重启 Kimi 后重试。',
  model_disconnect_byok_source_stale: '官方模型来源状态已变化，未继续移除。请刷新状态后重试。',
  unmanaged_kimi_running: '检测到另一个未受托管的官方 Kimi 正在运行。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。',
  official_kimi_already_running: '检测到另一个未受托管的官方 Kimi 正在运行。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。',
  native_bridge_unavailable: '未连接到受管 Kimi 通道。请重试操作，或点击「正常退出并重启 Kimi」重新建立受管连接。',
  native_bridge_waiting_work: '官方 Kimi 已运行，但 Work 页面尚未就绪。请确认 Kimi 已登录并切换到 Work 视图，或点击「打开 Kimi Work」后重试。',
  native_bridge_waiting_settings: 'Kimi Work 设置页面未能及时就绪。请确认 Kimi 窗口无阻塞弹窗后重试。',
  native_bridge_settings_open_refused: '官方 Kimi 拒绝打开 Work 设置页。请确认 Kimi 窗口已就绪且无阻塞提示后重试。',
  native_bridge_activation_refused: '未能自动唤起 Kimi Work 视图。请手动切到官方 Kimi 的 Work 页面后重试。',
  native_bridge_timeout: '等待官方 Kimi 页面响应超时。请确认 Kimi 窗口正常运行且未卡在登录或弹窗后重试。',
  native_bridge_busy: 'Kimi 正在处理其他同步或页面操作，请稍候再试。',
  native_bridge_cancelled: '本次 Kimi 页面操作已取消。',
  native_bridge_contract_changed: '官方 Kimi 页面接口与预期不一致，未继续写入。请检查官方版本兼容性或更新本软件。',
  native_prepare_required: '本机原生接入解析尚未准备，需要一次系统授权。请点击「准备并同步模型」或到「设置」页手动重新同步完成准备。',
  native_import_waiting_login: '尚未在官方 Kimi 确认登录账号。请先在打开的 Kimi 窗口完成登录，再重试操作。',
  native_import_no_account: '官方 Kimi 当前尚未确认登录账号，未继续导入模型目录。请先在官方窗口完成登录，再点「重新同步模型目录」。',
  native_import_byok_not_ready: '官方 Kimi 的模型来源服务尚未就绪，未继续导入模型目录。请稍候或正常重启官方 Kimi 后再点「重新同步模型目录」。',
  waiting_login: '尚未在官方 Kimi 确认登录账号。请先在打开的 Kimi 窗口完成登录，再重试操作。',
  account_changed: '当前官方 Kimi 登录账号已变化。请核对账号并刷新状态后重试。',
  native_import_account_changed: '当前官方 Kimi 登录账号已变化。请核对账号并刷新状态后重试。',
  native_import_failed: '模型目录同步未能写入官方 Kimi，请稍后重试或查看诊断信息。',
  'native_import_feature-disabled': '官方 Kimi 的自定义模型入口尚未就绪（本机解析未生效或官方进程仍保留旧缓存）。请点击「重新准备并同步」自动准备解析并同步。',
  native_import_feature_disabled: '官方 Kimi 的自定义模型入口尚未就绪（本机解析未生效或官方进程仍保留旧缓存）。请点击「重新准备并同步」自动准备解析并同步。',
  native_import_account_unverified: '无法确认当前官方 Kimi 账号身份。请先在官方窗口完成登录后重试。',
  native_import_page_reloaded: 'Kimi Work 页面在操作期间重新加载，请重试。',
  native_import_busy: '已有模型同步或移除操作正在进行，请稍候再试。',
  native_import_not_verified: '请求已发出，但未从官方配置读回确认。请刷新状态后重试。',
  native_import_requires_restart: '模型配置已写入，请等当前任务结束后正常重启 Kimi 以生效。',
  native_import_store_changed: '官方模型配置在你操作的同时被其他进程改写，本次未继续写入；稍后会自动重试。',
  native_import_foreign_source: '官方模型来源不属于本产品，未断开也未改写；请先到官方 Kimi 核对模型来源。',
  native_import_provider_reset_failed: '官方模型来源在修复过程中发生变化，本次未继续写入；请重试一次，仍失败请查看诊断信息。',
  native_import_non_canonical_provider: '官方 Kimi 仍保留旧的随机模型来源 ID，原位切换修复未完成；本次未写入重复来源。请稍后重新同步，仍失败请查看诊断信息。',
  native_import_apply_failed: '模型配置已提交，但官方 Kimi 未完成生效操作；请稍后重新同步，仍失败请正常重启 Kimi。',
  model_provider_reset_pending: '官方模型来源已提交重建，但 Kimi Work 正在处理任务，尚未生效；请正常重启 Kimi 完成修复。',
  kimi_process_state_unknown: '无法确认官方 Kimi 当前运行状态。请先手动退出官方 Kimi，再从面板重试。',
  kimi_not_running: '官方 Kimi 当前未运行。请先打开 Kimi Work 后再继续。',
  kimi_start_failed: '未能自动启动官方 Kimi。请确认官方 Kimi 安装正常，或手动打开后再试。',
  conversation_rollback_failed: '会话绑定恢复尚未完成。请在「设置」页的会话维护区刷新。',
  conversation_kimi_restart_failed: 'Kimi 未能自动重新打开，请手动打开 Kimi Work 后继续。',
  model_sync_write_blocked: '当前官方 Kimi 版本未通过模型同步兼容性检查，已暂停自动写入模型配置。',
  disconnected: '本产品接管的模型来源已从官方 Kimi 移除；本地服务商与模型仍保留，可随时重新同步接管。',
  kimi_start_cancelled: '自动启动官方 Kimi 已取消。',
  kimi_stop_failed: '未能正常停止当前受管 Kimi 进程，请先在官方窗口退出后再试。',
  kimi_user_data_dir_invalid: '官方 Kimi 数据目录路径无效，请检查诊断信息。',
  kimi_user_data_dir_unavailable: '无法访问官方 Kimi 数据目录，请检查目录权限后重试。',
  platform_cannot_manage_child: '当前系统环境不支持自动托管启动官方 Kimi，请手动打开官方 Kimi。',
  translation_attach_failed: '官方 Kimi 启动后未能连接页面通道，请重试或重启 Kimi。',
  injection_payload_unavailable: '页面注入资源不可用，请重新安装或重启本软件后重试。',
  managed_child_unattached: '当前 Kimi 进程未建立受管连接。请点击「正常退出并重启 Kimi」后重试。',
  native_bridge_failed: '与官方 Kimi 的受管通信未完成，请确认 Kimi 正常运行后重试。',
  native_bridge_invalid_request: '发送给官方 Kimi 的操作请求无效，请刷新状态后重试。',
  native_bridge_unauthorized: '受管 Kimi 通道会话校验未通过，请点击「正常退出并重启 Kimi」重建连接。',
  work_activity_recent: 'Kimi Work 刚有活跃写入，等待空闲',
  gateway_requests_active: '网关仍有活跃请求，等待完成',
  settling: '等待官方 Kimi 启动稳定',
  cooldown: '冷却中，稍后再试',
  invalid_auto_takeover: '自动重新接管开关参数无效',
  operation_failed: '操作未能完成，请刷新状态或查看诊断信息后重试。',
};

/* 直连失败码有三个来源：官方页面给连字符代码，Go 侧再包一层
   native_import_/native_ 前缀，主程序自己用下划线代码。三者指向同一张表，
   归一化后查一次，文案才不会按调用路径变来变去。 */
const DIRECT_REASON = {
  direct_import_required: '直连配置已保存，等待后台写入官方 Kimi。',
  direct_prepare_required: '直连模式需要先完成一次静态 hosts 系统授权；请在设置页点击「重新同步模型目录」完成授权。',
  direct_mode_unsupported: '当前官方 Kimi 版本不支持原生直连（需要 3.2.28+ 的 byokSources v3 存储）；可改用本机网关模式。',
  direct_preview_refused: '官方 Kimi 拒绝了这次直连写入，现有配置未被改动；请确认官方窗口没有正在同步或编辑模型源，再重新同步。',
  direct_no_eligible_models: '没有可直连的模型：至少需要一个已声明工具调用、已绑定连接且有密钥的模型。',
  direct_duplicate_endpoint: '多个模型与同一个服务商端点重复，直连模式只保留其中一个；请检查服务商地址后重新同步。',
  direct_missing_api_key: '有模型缺少可用的 API Key，直连模式无法写入；请在模型页补齐密钥后重新同步。',
  direct_import_failed: '官方 Kimi 没有接受这次直连写入，现有配置未被改动；请稍后重新同步，仍失败时查看诊断信息。',
  direct_gateway_disconnect_failed: '没能断开旧的网关接入源，官方 Kimi 可能仍指向本机网关；请重新同步，确认直连已生效后再退出后台服务。',
  direct_disconnect_failed: '没能断开旧的接入源，官方 Kimi 可能仍走原来的路由；请重新同步，并在官方窗口确认模型来源。',
  direct_foreign_source: '官方 Kimi 中已有不是本产品写入的模型源，未覆盖；请先核对现有来源后再同步。',
  direct_catalog_too_large: '这次要导入的模型目录超过了官方 Kimi 单次接受的上限，本次未写入；请减少单次导入的模型，或拆分连接后分批同步。',
  models_removed: '官方 Kimi 里已经没有这次要导入的部分模型，可能是官方更新或来源被改动；请点「重新同步模型目录」重试。',
  route_cleanup_pending: '新的直连配置已写入并读回，但旧的服务商来源还没清理，切换收尾未完成；点「重新同步模型目录」可重试清理。在此之前不要退出后台服务，也不要当作旧路由已清理。',
  direct_spec_missing: '当前没有可直连的模型或连接；请在模型页补齐后重新同步。',
  /* 旧 Go 版本与页面直接给出的代码；归一化后和上面的下划线代码指向同一句。 */
  foreign_source: '官方 Kimi 中已有不是本产品写入的模型源，未覆盖；请先核对现有来源后再同步。',
  gateway_disconnect_failed: '没能断开旧的网关接入源，官方 Kimi 可能仍指向本机网关；请重新同步，确认直连已生效后再退出后台服务。',
  import_failed: '官方 Kimi 没有接受这次直连写入，现有配置未被改动；请稍后重新同步，仍失败时查看诊断信息。',
  models_missing: '官方 Kimi 里还缺这次要导入的模型；请点「重新同步模型目录」重试，仍失败时查看诊断信息。',
  contract_changed: '官方 Kimi 的模型源接口与预期不同，本次未写入任何内容；请更新官方 Kimi 或本软件后重试。',
  byok_source_stale: '官方 Kimi 的模型源在写入期间被更新，本次未继续；请重新同步。',
  native_import_direct_mode: '直连模式会把服务商地址与密钥直接写入官方 Kimi，不再经过本机网关。',
  native_import_gateway_mode: '已恢复本机网关模式，同步会把官方 Kimi 改回通过本机网关访问模型。',
};

/* `native_import_` 前缀后面可能还是连字符形式（`direct-preview-refused`），
   所以先剥前缀，再把连字符还原成统一的下划线代码。 */
function normalizeReasonCode(code) {
  if (typeof code !== 'string' || !code) return '';
  let value = code;
  if (value.startsWith('native_import_')) value = value.slice('native_import_'.length);
  else if (value.startsWith('native_')) value = value.slice('native_'.length);
  return value.replace(/-/g, '_');
}

function directReasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  if (DIRECT_REASON[code]) return DIRECT_REASON[code];
  const normalized = normalizeReasonCode(code);
  return normalized && DIRECT_REASON[normalized] ? DIRECT_REASON[normalized] : '';
}

const ERROR_HINT = {
  configuration_conflict: '配置在别处被修改，已刷新状态；你的编辑仍然保留，请确认后重新保存。',
  configuration_changed: '配置已在外部修改，模型保存已禁用，请重启面板。',
  configuration_unavailable: '尚未读取到本机配置，请刷新状态后重试。',
  configuration_apply_failed: '目录已保存，但运行策略未能更新，请检查状态后重启。',
  invalid_models: '模型声明无效：请检查 ID、上下文窗口、能力与思考档位。',
  protocol_conflict: '模型 ID 末段以 kimi- 开头时必须使用 Chat，其他必须使用 Responses。',
  invalid_base_url: '上游地址需使用 HTTPS（本机回环可用 HTTP），且不能包含账号、查询参数或片段。',
  invalid_config: '配置无效，请检查端口和环境变量名称。',
  invalid_connection_config: '连接配置不完整或包含不支持的字段。密钥由加密凭据存储保存，配置文件只记录引用。',
  credentials_unavailable: '加密凭据存储未接受保存，配置未提交。请检查应用数据目录权限后重试；不会写入明文。',
  credentials_readback_failed: '新密钥未能从加密凭据存储读回，未切换现有引用。请重试。',
  credential_keychain_locked: '登录钥匙串已锁定，请在 macOS 中解锁后从本窗口重新保存；密钥未写入。',
  credential_keychain_unavailable: '当前登录会话无法使用系统钥匙串，请检查应用签名和会话权限；密钥未写入。',
  credential_session_unavailable: '当前后台会话不能显示授权窗口，请从应用窗口重新保存。',
  credential_authorization_required: '此条目需要系统授权；请从应用窗口响应系统提示。若在读取已保存 Key，可到模型页点「重新授权并应用连接」。',
  credential_authorization_cancelled: '系统授权已取消，密钥未保存；原配置仍保留。',
  credential_authorization_failed: '系统未接受授权，密钥未保存；原配置仍保留。',
  credential_store_corrupt: '凭据条目内容无法识别，请从应用窗口重新输入并保存。',
  credential_master_key_invalid: '本机凭据主密钥缺失或无效，原文件未被覆盖。请保留应用数据并从备份恢复；不要删除后重建，否则已有凭据无法恢复。',
  credential_readback_mismatch: '新凭据写入后未通过读回验证，未验证的值已清理。请重试；若仍失败，请重启后台服务。',
  credential_interaction_not_restored: '系统凭据交互状态未能恢复，请重启后台服务后重试。',
  credential_store_failed: '系统凭据操作未完成，请查看诊断状态；密钥未保存。',
  panel_action_busy: '上一次操作仍在执行，请等待结果。',
  operation_busy: '上一次操作仍在进行，请稍后重试。',
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
  invalid_subagent_roles: '角色模型分配无效，请确认所选模型均支持工具调用且在候选池中。',
  invalid_subagent_description: '用途说明必须是单行文本，最长 1000 字符。',
  subagent_pool_default_required: '默认模型必须在候选列表中。',
  subagent_model_unavailable: '所选模型已不在目录中，请重新选择。',
  subagent_model_tools_required: '此模型未声明工具调用，不能作为子代理。',
  subagent_model_in_use: '当前子代理仍在使用此模型或档位，请先在「模型」页底部的子代理设置中更改目标，再修改模型能力。',
  subagent_policy_owner_conflict: '另一实例已接管子代理策略，请停止重复运行的服务。',
  subagent_policy_unavailable: '无法读取当前子代理策略或配置归属存在冲突，请检查状态目录后重启面板。',
  subagent_policy_apply_failed: '目标已保存，但当前策略未能应用，请查看运行时状态。',
  invalid_subagent_effort: '此模型未声明所选思考档位，请重新选择。',
  invalid_integration_mode: '接入模式不被接受，只支持本机网关或零网关直连；当前模式未更改。',
  invalid_auto_takeover: '自动重新接管开关参数无效',
  model_disconnect_failed: '模型来源未能移除，请刷新状态后重试。',
  model_disconnect_feature_disabled: '官方 Kimi 的自定义模型入口当前未就绪，暂时无法读取官方模型来源。请先点「重新同步模型目录」准备本机解析，或重启官方 Kimi 后再试。',
  model_disconnect_no_account: '官方 Kimi 当前尚未确认登录账号，未继续移除模型来源。请先在官方窗口完成登录后重试。',
  model_disconnect_byok_not_ready: '官方 Kimi 的模型来源服务尚未就绪，请稍候或正常重启官方 Kimi 后再试。',
  model_disconnect_unsupported: '当前官方 Kimi 不支持移除模型来源；请升级官方 Kimi 后重试。',
  model_disconnect_contract_changed: '官方模型来源接口返回了无法确认的结果，未继续操作。请查看诊断信息。',
  model_disconnect_source_conflict: '接管记录对应的模型来源已被其他配置替换，未移除。请先核对官方模型来源。',
  model_disconnect_not_verified: '移除请求已发出，但读回仍发现本产品接管的来源。请刷新状态后重试。',
  model_disconnect_local_state_failed: '官方来源已处理，但本地接管记录未能完成更新。请重试以完成还原。',
  model_disconnect_deferred: '官方模型来源已提交移除，但 Kimi Work 正在处理任务，尚未生效。请等任务结束或正常重启 Kimi 后，再点一次「移除接管并还原」完成确认。',
  model_disconnect_apply_failed: '官方模型来源已从列表移除，但写入 Kimi 配置失败。请刷新状态或正常重启 Kimi 后重试。',
  model_disconnect_byok_source_stale: '官方模型来源状态已变化，未继续移除。请刷新状态后重试。',
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
  updateBanner: $('update-banner'),
  updateBannerText: $('update-banner-text'),
  updateBannerPrimary: $('update-banner-primary'),
  updateBannerNotes: $('update-banner-notes'),

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
  navConversations: $('nav-conversations'),
  navAccounts: $('nav-accounts'),
  navUsage: $('nav-usage'),
  navMaintenance: $('nav-maintenance'),
  navModelCount: $('nav-model-count'),
  navUpdateDot: $('nav-update-dot'),

  railGatewayDot: $('rail-gateway-dot'),
  railGatewayText: $('rail-gateway-text'),
  railGatewayUrl: $('rail-gateway-url'),
  theme: $('theme'),

  factGatewayDot: $('fact-gateway-dot'),
  factGateway: $('fact-gateway'),
  factGatewayUrl: $('fact-gateway-url'),
  overviewSwitchMode: $('overview-switch-mode'),
  factModels: $('fact-models'),
  factModelsSub: $('fact-models-sub'),
  overviewOpenModels: $('overview-open-models'),
  overviewOpenSubagents: $('overview-open-subagents'),
  overviewSubagentsDot: $('overview-subagents-dot'),
  overviewSubagentsValue: $('overview-subagents-value'),
  overviewSubagentsNote: $('overview-subagents-note'),
  overviewOpenUsage: $('overview-open-usage'),
  quickstartGuide: $('quickstart-guide'),
  quickstartProgress: $('quickstart-progress'),
  quickstartToggle: $('quickstart-toggle'),
  quickstartDismiss: $('quickstart-dismiss'),
  quickstartSteps: $('quickstart-steps'),
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
  mUpstream: $('m-upstream'),
  mUpstreamHint: $('m-upstream-hint'),
  mConnection: $('m-connection'),
  mConnectionHint: $('m-connection-hint'),
  mCandidates: $('m-candidates'),
  mSourceField: $('m-source-field'),
  mAdvanced: $('model-advanced-details') || $('m-advanced'),
  mContextHint: $('m-context-hint'),
  mBudgetGroup: $('m-budget-group'),
  mName: $('m-name'),
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
  mDefaultField: $('m-default-field'),
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


  restartPanel: $('restart-panel'),
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
  onboardProtocolGroup: $('ob-protocol-group'),
  onboardProtocolHint: $('ob-protocol-hint'),
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
  onboardSaveErrors: $('ob-save-errors'),
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
  onboardAdvancedFields: $('ob-advanced-fields'),
  onboardProbeResult: $('ob-probe-result'),
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
  ceProtoAuto: $('ce-proto-auto'),
  ceChat: $('ce-chat'),
  ceResponses: $('ce-responses'),
  ceAnthropic: $('ce-anthropic'),
  ceAdvanced: $('ce-advanced'),
  ceProbeResult: $('ce-probe-result'),
  ceProtocolHint: $('ce-protocol-hint'),

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
  saRoleGrid: $('sa-role-grid'),
  saRoleDefault: $('sa-role-default'),
  saRolePlan: $('sa-role-plan'),
  saRoleCoder: $('sa-role-coder'),
  saRoleExplore: $('sa-role-explore'),
  saRoleNote: $('sa-role-note'),
  saPoolAdvanced: $('sa-pool-advanced'),
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

  integrationModeGroup: $('integration-mode-group'),
  modeGateway: $('mode-gateway'),
  modeDirect: $('mode-direct'),
  integrationModeState: $('integration-mode-state'),
  integrationModeConfirm: $('integration-mode-confirm'),
  integrationModeConfirmText: $('integration-mode-confirm-text'),
  integrationModeCancel: $('integration-mode-cancel'),
  integrationModeSubmit: $('integration-mode-submit'),
  autoTakeoverEnabled: $('auto-takeover-enabled'),
  autoTakeoverState: $('auto-takeover-state'),
  directModeNotice: $('direct-mode-notice'),
  directModeSupport: $('direct-mode-support'),
  directModeHosts: $('direct-mode-hosts'),
  directModeBypassed: $('direct-mode-bypassed'),
  directModeSkipped: $('direct-mode-skipped'),
  nativeSync: $('native-sync'),
  nativeSyncLabel: $('native-sync-label'),
  factGatewayMode: $('fact-gateway-mode'),

  updCurrent: $('upd-current'),
  updCurrentSub: $('upd-current-sub'),

  convGuide: $('conv-guide'),
  convGuideText: $('conv-guide-text'),
  convRefresh: $('conv-refresh'),
  convStatus: $('conv-status'),
  convResult: $('conv-result'),
  convQ: $('conv-q'),
  convCount: $('conv-count'),
  convList: $('conv-list'),
  convEmpty: $('conv-empty'),
  convEmptyNote: $('conv-empty-note'),
  convMigrationTip: $('conv-migration-tip'),
  convMigrationText: $('conv-migration-text'),
  convMigrateAll: $('conv-migrate-all'),
  convBatch: $('conv-batch'),
  convBatchClear: $('conv-batch-clear'),
  convBatchModel: $('conv-batch-model'),
  convBatchEffort: $('conv-batch-effort'),
  convBatchGo: $('conv-batch-go'),
  convBatchMigrate: $('conv-batch-migrate'),
  convBatchNote: $('conv-batch-note'),
  convConfirm: $('conv-confirm'),
  convConfirmText: $('conv-confirm-text'),
  convConfirmSubmit: $('conv-confirm-submit'),
  convConfirmCancel: $('conv-confirm-cancel'),

  toast: $('toast'),
};

const state = {
  key: null,
  data: null,
  view: 'overview',
  subtabs: {},
  subagentModeCache: { force: null, pool: null },
  guideCollapsed: null,
  query: '',
  filter: 'all',
  editor: null,
  deleteId: null,
  conns: { base: null, baseRevision: null, baseEntity: null, dirty: false, editing: null, draft: null, errors: [] },
  connDeleteId: null,
  connDeletingId: null,
  connPicker: { connectionId: null, loading: false, saving: false, query: '', models: [], selectedIds: [], baseSavedIds: [], error: '', sourceNote: '', seq: 0 },
  connChecks: {},
  connUpstreamProtocols: {},
  picker: { open: false, models: null, query: '', state: null, checkedAt: null, source: null, reason: null,
    busy: false, seq: 0, hint: null, scope: null },
  /* 接入向导：连接字段、上游目录候选与被勾选的模型留在本地草稿里，
     直到用户点「保存」才写配置；轮询不得覆盖正在输入的 Key。 */
  onboard: { discovery: null, discoveryFor: null, seq: 0, busy: false, saving: false, touched: false, query: '', saveErrors: [], protocol: null, protocolMode: 'auto', upstreamProtocol: '', protocolTouched: false, probeResult: null },
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
  integrationModeConfirm: null,
  autoTakeoverSaving: false,
  pendingAction: null,
  pendingOpId: null,
  lastSync: null,
  diagnosticsExporting: false,
  diagnosticsExportError: '',
  diagnosticsExportPath: '',
  pendingService: null,
  composing: false,
  disconnectConfirm: null,
  timer: null,
  stateReadSeq: 0,
  stateReadEpoch: 0,
  overviewToday: { key: null, requests: null, totalTokens: null, loading: false, fetchedAt: 0, requestCount: -1 },
  overviewIssueAction: null,
  toastTimer: null,
  started: false,
  actionButtons: [],
  /* 窗口聚焦时的更新复查：60 秒内不重复发请求，30 分钟内的检查结果直接复用。 */
  lastFocusUpdateCheckAt: 0,
  initialUpdateCheckDone: false,
  conversations: {
    list: null, loading: false, error: '', query: '', stale: false, pending: false,
    targets: {}, efforts: {}, selected: [], noConfirm: false, result: '', confirm: null,
    /* 展开中的行内确认条（会话键）：轮询据此暂停自动重刷。 */
    rowConfirm: null,
  },
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
    const error = new ApiError(ERROR_HINT[code] || directReasonText(code) || message || '请求失败，请稍后重试。', code || 'http_' + res.status, res.status);
    error.saved = Boolean(payload && payload.error && payload.error.saved);
    error.reason = payload && payload.error && payload.error.reason;
    if (Array.isArray(payload && payload.error && payload.error.models)) error.models = payload.error.models;
    if (payload && payload.error && typeof payload.error.detail === 'string') error.detail = payload.error.detail;
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

function isInputActive(el) {
  if (!el) return false;
  if (state.composing) return true;
  return typeof document !== 'undefined' && document.activeElement === el;
}

function setText(el, value) {
  if (!el) return;
  const label = typeof el.querySelector === 'function' ? el.querySelector('.btn__label') : null;
  const target = label || el;
  if (target.textContent !== value) target.textContent = value;
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
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return '未知';
  if (n === 0) return '0';
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
  return model.name || model.upstreamModelId || model.id || '未命名模型';
}

function catalogModelLabel(data, id) {
  const models = data && Array.isArray(data.models) ? data.models : [];
  const model = models.find((entry) => entry && entry.id === id) || null;
  return modelLabel(model, id);
}

function reasonText(code) {
  if (typeof code !== 'string' || !code) return '';
  return DIAG_REASON[code] || directReasonText(code) || code;
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
    lede: '请使用 KimiWork + 桌面应用或菜单栏托盘打开面板；应用会自动读取当前本机地址和会话密钥。',
    note: '如果窗口已打开但会话缺失，请从托盘选择「打开 KimiWork +」刷新入口。',
  },
  expired: {
    title: '会话密钥已失效',
    lede: '后台重启后旧会话会失效。请从菜单栏托盘选择「打开 KimiWork +」，应用会读取最新入口。',
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
  const icon = iconUse(tone === 'error' || tone === 'warn' ? 'i-alert' : 'i-check');
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
  DIRECT_BYPASS_WORD,
  DIRECT_REASON,
  DIRECT_SKIP_REASON,
  DISCOVERY_REASON,
  EFFORT_PATTERN,
  GUIDE_DISMISSED_STORE,
  INTEGRATION_MODES,
  KEY_STORE,
  LOOPBACK,
  MODEL_ALIAS_PREFIXES,
  MODEL_ALIAS_UUID_PREFIX,
  POLICY_MODE_WORD,
  POLICY_TRANSPORT_WORD,
  POLL_MS,
  RUNTIME_STATE,
  SUBTAB_STORE,
  UPSTREAM_REASON,
  VIEWS,
  VIEW_ALIASES,
  VIEW_COPY,
  VIEW_STORE,
  actionUUID,
  applyTheme,
  catalogModelLabel,
  clearKey,
  displayModelId,
  directReasonText,
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
  isInputActive,
  modelLabel,
  mountUsageRequests,
  observeMasthead,
  postWailsMessage,
  readKey,
  reasonText,
  renderErrorList,
  request,
  revealWorkspaceSection,
  scrollMainToElement,
  setConnErrors,
  setDot,
  setHidden,
  setSubtab,
  setText,
  shortRev,
  showGate,
  showToast,
  state,
  stripModelAliasPrefix,
  syncDefaultOptions,
  syncSelectOptions,
  truncate,
};

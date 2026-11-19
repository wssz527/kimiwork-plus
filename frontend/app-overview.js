import { applyState, configInfo, configReady, onboardBusy, renderOnboard } from './app-connections.js';
import { $, ACTION_LABEL, ACTION_STATE, displayModelId, dom, fmtCount, fmtDuration, fmtK, fmtMs, fmtTime, fmtTokens, reasonText, request, revealWorkspaceSection, setDot, setHidden, setText, showToast, state } from './app-core.js';
import { connectionRowsOf } from './app-models.js';
import { loadState, runAction, setView } from './app.js';

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
    setText(dom.restartBannerText, '连接配置已修改，当前网关仍在使用启动时的设置。请到「模型」页点「应用已保存的连接」，由后台服务在安全时应用；页面会如实显示是否生效。');
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
    native_byok_dns_unavailable: '本机单域名 DNS 端口被占用或不可用。请停止占用该端口的旧版本或其他接入实例，再重新准备。',
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
  renderCompatibility(data);
  renderOverviewIssue(data);
  renderOverviewToday();
  void refreshOverviewToday();
}

function compatToneOf(compat) {
  const st = compat?.status || 'checking';
  if (st === 'verified') return 'ok';
  if (st === 'new_version_ok' || st === 'unavailable') return 'warn';
  if (st === 'degraded') return 'bad';
  return 'neutral';
}

function compatStoreLabel(store) {
  const format = store?.format || '';
  if (format === 'accounts') return 'accounts（3.2.28+ 格式）';
  if (format === 'profiles') return 'profiles（兼容旧格式）';
  if (format === 'absent') return '尚未生成（首次同步后创建）';
  if (format === 'unknown') return '无法识别的新格式';
  if (format === 'invalid') return '文件损坏或不可读';
  return '未检查';
}

function renderCompatibility(data) {
  const compat = data?.compatibility || null;
  const bar = $('overview-compat-bar');
  const dot = $('compat-dot');
  const summary = $('compat-summary');
  const version = $('compat-version');
  const note = $('compat-note');

  if (!compat) {
    if (bar) bar.dataset.tone = 'neutral';
    setDot(dot, 'neutral');
    setText(summary, '尚未检查');
    if (version) setHidden(version, true);
    setText(note, '启动与官方版本变化时自动执行只读自检（模型目录、账号接口、存储格式）。');
    return;
  }

  const tone = compatToneOf(compat);
  const st = compat.status || 'checking';
  if (bar) bar.dataset.tone = tone;
  setDot(dot, tone);

  let headline = compat.summary || '';
  if (!headline) {
    if (st === 'verified') headline = '兼容';
    else if (st === 'new_version_ok') headline = '新版本，基础检查通过';
    else if (st === 'degraded') headline = '部分功能不可用';
    else if (st === 'unavailable') headline = '未检测到官方 Kimi';
    else headline = '正在检查…';
  }
  setText(summary, headline);

  if (version) {
    const verText = compat.officialVersion ? 'Kimi ' + compat.officialVersion : '';
    setHidden(version, !verText);
    if (verText) setText(version, verText);
  }

  let noteText = '';
  if (st === 'degraded') {
    const affected = Array.isArray(compat.affectedFeatures) && compat.affectedFeatures.length
      ? compat.affectedFeatures.map((item) => item.label).filter(Boolean).join('、')
      : headline;
    noteText = `${affected}。已停止受影响功能的写入，本机网关和其他功能照常运行。`;
  } else if (st === 'new_version_ok') {
    noteText = compat.versionChanged && compat.previousVersion
      ? `检测到官方 Kimi 已从 ${compat.previousVersion} 更新至 ${compat.officialVersion}，目录、账号接口与存储格式检查全部通过。`
      : '该官方版本尚未列入已验证清单，但目录、账号接口与存储格式检查已全部通过。';
  } else if (st === 'verified') {
    const format = compat.checks?.storeFormat?.format;
    const formatHint = format === 'accounts' ? 'accounts 格式' : format === 'profiles' ? 'profiles 格式' : '待首次同步';
    noteText = compat.versionChanged && compat.previousVersion
      ? `检测到官方 Kimi 已从 ${compat.previousVersion} 更新至 ${compat.officialVersion}，全部只读检查通过（存储：${formatHint}）。`
      : `已验证兼容版本；模型目录、账号接口与存储格式（${formatHint}）只读检查通过。`;
  } else if (st === 'unavailable') {
    noteText = '未在本机检测到官方 Kimi 桌面版安装；本机网关可继续独立运行。';
  } else {
    noteText = '正在对官方 Kimi 执行只读自检…';
  }
  setText(note, noteText);

  const diagDot = $('diag-compat-dot');
  setDot(diagDot, tone);
  const verLabel = compat.officialVersion
    ? `${compat.officialVersion}（${compat.verifiedVersion ? '已验证兼容' : '新版本'}）`
    : '未检测到安装';
  setText($('diag-compat-version'), verLabel);
  const kernelBits = [];
  if (compat.kernelVersion || compat.kernelCommit) {
    kernelBits.push('内核 ' + [compat.kernelVersion, compat.kernelCommit].filter(Boolean).join(' / '));
  }
  if (Array.isArray(compat.verifiedVersions) && compat.verifiedVersions.length) {
    kernelBits.push('已验证版本：' + compat.verifiedVersions.join('、'));
  }
  if (compat.checkedAt) kernelBits.push('检查于 ' + fmtTime(compat.checkedAt));
  setText($('diag-compat-kernel'), kernelBits.join(' · ') || '—');

  const catalog = compat.checks?.catalog || {};
  setText($('diag-compat-catalog'), catalog.status === 'pass' ? '通过' : catalog.status === 'unavailable' ? '未检测到' : '失败');
  const catalogSub = catalog.status === 'pass'
    ? `已接受 ${fmtCount(catalog.acceptedCount || 0)} / ${fmtCount(catalog.modelCount || 0)} 个样本模型`
    : (catalog.reason ? '原因：' + reasonText(catalog.reason) : '目录解析器未通过验证');
  setText($('diag-compat-catalog-sub'), catalogSub);

  const apis = compat.checks?.accountApi || {};
  setText($('diag-compat-apis'), apis.status === 'pass' ? '通过' : apis.status === 'unavailable' ? '未检测到' : '部分缺失');
  const apiBits = [
    `账号识别(getUserProfile)：${apis.getUserProfile ? '正常' : '缺失'}`,
    `模型读回：${apis.byokSourcesList ? 'byokSources.list' : apis.getByokProfile ? 'getByokProfile' : '缺失'}`,
  ];
  if (apis.source === 'live') apiBits.push('来源：运行中页面');
  else if (apis.source === 'preload') apiBits.push('来源：预加载脚本');
  setText($('diag-compat-apis-sub'), apiBits.join(' · '));

  const store = compat.checks?.storeFormat || {};
  setText($('diag-compat-store'), compatStoreLabel(store));
  const guards = compat.guards || {};
  const blocked = [];
  if (guards.modelSyncWriteBlocked) blocked.push('已停止模型同步写入');
  if (guards.accountSwitchWriteBlocked) blocked.push('已停止账号切换写入');
  setText($('diag-compat-guards'), blocked.length ? blocked.join('；') + '，其余功能照常运行。' : '所有功能正常，无需停止写入。');
}

function renderOverviewToday() {
  const today = state.overviewToday;
  if (!today || today.requests === null) {
    setText(dom.overviewTodayRequests, today?.loading ? '读取中…' : '—');
    setText(dom.overviewTodayTokens, today?.loading ? '今日 Token：读取中…' : '今日 Token：未提供');
    return;
  }
  setText(dom.overviewTodayRequests, fmtCount(today.requests) + ' 次请求');
  const tokensText = typeof today.totalTokens === 'number' && Number.isFinite(today.totalTokens)
    ? fmtTokens(today.totalTokens) : '未提供';
  setText(dom.overviewTodayTokens, '今日 Token：' + tokensText);
}

async function refreshOverviewToday({ force = false } = {}) {
  if (!state.key) return;
  const today = state.overviewToday;
  if (today.loading) return;
  const reqCount = Number(state.data?.metrics?.requests ?? -1);
  const now = Date.now();
  const stale = today.key !== state.key || today.requests === null || today.requestCount !== reqCount;
  if (!force && !stale && (now - today.fetchedAt) < 15000) return;
  if (!force && stale && today.fetchedAt > 0 && (now - today.fetchedAt) < 3000) return;
  const key = state.key;
  const epoch = state.stateReadEpoch;
  today.loading = true;
  if (today.requests === null) renderOverviewToday();
  try {
    const result = await request('/api/usage/query', { method: 'POST', body: { range: 'today' } });
    if (state.key !== key || state.stateReadEpoch !== epoch) return;
    const summary = (result && result.summary) || {};
    const requests = Number(summary.requests ?? summary.requestCount);
    const totalTokens = Number(summary.totalTokens ?? summary.tokens);
    today.key = key;
    today.requests = Number.isFinite(requests) ? requests : 0;
    today.totalTokens = Number.isFinite(totalTokens) ? totalTokens : null;
    today.requestCount = reqCount;
    today.fetchedAt = Date.now();
  } catch {
    if (state.key === key && state.stateReadEpoch === epoch) today.fetchedAt = Date.now();
  } finally {
    today.loading = false;
    renderOverviewToday();
  }
}

function computeOverviewIssue(data) {
  const gw = (data && data.gateway) || {};
  if (gw.state !== 'running') {
    return {
      tone: 'bad',
      title: '本机网关未运行',
      note: '模型请求当前无法发出，可在设置页重启后台服务。',
      actionLabel: '前往设置',
      action: { type: 'view', view: 'maintenance' },
    };
  }
  const compat = data && data.compatibility;
  if (compat && compat.status === 'degraded') {
    return {
      tone: 'bad',
      title: compat.summary || '官方 Kimi 兼容异常',
      note: '已停止受影响功能的写入，本机网关和其他功能照常运行。请在设置页查看版本自检详情。',
      actionLabel: '查看版本自检',
      action: { type: 'section', view: 'maintenance', section: 'compat-diagnostics-panel' },
    };
  }
  const apply = applyState(data);
  if (['连接需要系统授权', '应用失败', '应用已超时', '等待应用', '自动应用不可用'].includes(apply.word)) {
    return {
      tone: apply.tone || 'warn',
      title: apply.word,
      note: apply.note,
      actionLabel: '前往模型页处理',
      action: { type: 'section', view: 'models', section: 'restart-panel' },
    };
  }
  const budgetMessage = budgetNotice(data && data.budget);
  if (budgetMessage) {
    return {
      tone: 'warn',
      title: '已达到预算上限',
      note: budgetMessage,
      actionLabel: '查看预算',
      action: { type: 'budget' },
    };
  }
  const verdict = syncState(data);
  if (verdict.code === 'empty') {
    return {
      tone: 'neutral',
      title: '尚未添加模型',
      note: '先在模型页添加服务商与模型，保存后同步到 Kimi Work。',
      actionLabel: '添加模型',
      action: { type: 'view', view: 'models' },
    };
  }
  if (verdict.code === 'manual-exit' || verdict.code === 'restart') {
    return {
      tone: verdict.tone || 'warn',
      title: verdict.word,
      note: verdict.note,
      actionLabel: '重启 Kimi',
      action: { type: 'action', action: 'restart-kimi' },
    };
  }
  if (verdict.code !== 'synced') {
    return {
      tone: verdict.tone || 'warn',
      title: verdict.word,
      note: verdict.note,
      actionLabel: '接入与恢复',
      action: { type: 'view', view: 'maintenance' },
    };
  }
  const recent = Array.isArray(data?.metrics?.recent) ? data.metrics.recent : [];
  const failed = recent.find((row) => row && ((typeof row.status === 'number' && row.status >= 400) || row.error || row.failure?.reason));
  if (failed) {
    const modelName = displayModelId((failed.upstreamModelId || failed.model) || '模型');
    const detail = requestErrorText(failed) || '请求未完成，请检查模型配置或稍后重试。';
    const budgetHit = failed.error === 'budget_limit_reached' || failed.failure?.reason === 'budget_limit_reached';
    return {
      tone: 'bad',
      title: '最近请求失败',
      note: modelName + '：' + detail,
      actionLabel: budgetHit ? '查看预算' : '检查模型与连接',
      action: budgetHit ? { type: 'budget' } : { type: 'view', view: 'models' },
    };
  }
  return {
    tone: 'ok',
    title: '状态正常',
    note: '最近没有未处理的连接或请求失败。',
    actionLabel: '',
    action: null,
  };
}

function renderOverviewIssue(data) {
  const issue = computeOverviewIssue(data);
  state.overviewIssueAction = issue.action;
  setDot(dom.overviewIssueDot, issue.tone);
  setText(dom.overviewIssueTitle, issue.title);
  setText(dom.overviewIssueNote, issue.note);
  if (dom.overviewIssueAction) {
    setHidden(dom.overviewIssueAction, !issue.action);
    if (issue.actionLabel) setText(dom.overviewIssueAction, issue.actionLabel);
  }
}

function runOverviewIssueAction() {
  const target = state.overviewIssueAction;
  if (!target) return;
  if (target.type === 'budget') {
    setView('usage');
    const tab = $('usage-tab-budget');
    tab?.click();
    tab?.focus();
    return;
  }
  if (target.type === 'section') {
    setView(target.view || 'models');
    revealWorkspaceSection(target.section);
    return;
  }
  if (target.type === 'action' && target.action) {
    void runAction(target.action);
    return;
  }
  if (target.type === 'view' && target.view) {
    setView(target.view);
  }
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
  return text || code;
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
      const rawCode = row.error || row.failure?.reason || '';
      if (rawCode) error.setAttribute('title', rawCode);
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
  if (!state.actionButtons.length) return;
  const running = isInFlight(data && data.operation);
  const busy = running || state.modelsSaving || state.connsSaving;
  const ready = configReady(data);
  const alreadyRunning = data?.kimiProcess?.running === true;
  const modelSyncBlocked = data?.compatibility?.guards?.modelSyncWriteBlocked === true;
  for (const button of state.actionButtons) {
    const reuseLogin = button.dataset.profileMode === 'existing-authorized';
    if (button.id === 'native-launch') {
      button.dataset.action = alreadyRunning ? 'restart-kimi' : 'launch-kimi';
    }
    const action = button.dataset.action;
    if (reuseLogin) setHidden(button, data?.profile?.canReuseLogin !== true);
    const needsReady = action === 'launch-kimi' || action === 'restart-kimi';
    const compatBlocked = modelSyncBlocked && (action === 'connect-kimi' || action === 'prepare-native');
    const pending = state.pendingAction === action;
    button.disabled = busy || compatBlocked || (needsReady && (!ready || (reuseLogin && alreadyRunning)));
    if (button.id === 'native-launch') setText(button, ACTION_LABEL[action]);
    if (reuseLogin) setText(button, '诊断：沿用登录，隔离 Work');
    button.classList.toggle('is-busy', pending);
    button.setAttribute('aria-busy', pending ? 'true' : 'false');
  }
}


function renderWorkspaceSync(data) {
  const integration = data.integration || {};
  const models = Array.isArray(data.models) ? data.models : [];
  if (data?.compatibility?.guards?.modelSyncWriteBlocked === true) {
    setText($('workspace-sync-state'), '模型同步不可用（已停止写入）');
    setText($('workspace-sync-note'), '当前官方 Kimi 版本未通过目录或存储格式自检，已暂停模型同步写入；本机网关与其他功能照常运行。');
    setHidden($('workspace-restart'), true);
    return;
  }
  const imported = Object.keys(integration.aliases || {}).sort();
  const saved = models.map(model => model.id).sort();
  const matches = JSON.stringify(imported) === JSON.stringify(saved);
  const restart = matches && integration.applied && !integration.adopted;
  const synced = matches && integration.adopted;
  setText($('workspace-sync-state'), !models.length ? '尚未添加模型' : synced ? '已同步到 Work' : restart ? '需要重启 Kimi' : '待同步到 Work');
  setText($('workspace-sync-note'), !models.length ? '添加服务商并选择模型，保存后同步。' : synced ? '已读回当前模型配置，可在 Work 的模型选择器中使用。' : restart ? '模型配置已写入。正常重启 Kimi 后，在选择器中确认。' : '本机网关已保存模型。点击「同步到 Work」更新官方模型选择器。');
  setHidden($('workspace-restart'), !restart);
}


let trackedServiceOperation = '';
let updateReconnectActive = false;
let updateFastPollTimer = null;

function fmtBytes(bytes) {
  const n = Number(bytes);
  if (!Number.isFinite(n) || n < 0) return '0 B';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

function updateStateSummary(updates) {
  const st = updates?.state || 'idle';
  switch (st) {
    case 'checking': return { tone: 'warn', word: '正在检查更新…' };
    case 'up_to_date': return { tone: 'ok', word: '已是最新版本' };
    case 'available': return { tone: 'warn', word: '发现新版本 ' + (updates.available?.version || '') };
    case 'downloading': return { tone: 'warn', word: '正在下载更新' };
    case 'verifying': return { tone: 'warn', word: '正在校验签名' };
    case 'waiting_idle': return { tone: 'warn', word: '等待空闲后替换' };
    case 'applying': return { tone: 'warn', word: '正在替换并重启' };
    case 'error': return { tone: 'bad', word: '更新未完成' };
    default: return { tone: 'neutral', word: '尚未检查' };
  }
}

function scheduleUpdateFastPoll(active) {
  if (!active) {
    if (updateFastPollTimer !== null) {
      window.clearTimeout(updateFastPollTimer);
      updateFastPollTimer = null;
    }
    return;
  }
  if (updateFastPollTimer !== null) return;
  updateFastPollTimer = window.setTimeout(async () => {
    updateFastPollTimer = null;
    try { await loadState({ silent: true }); } catch {}
  }, 500);
}

async function waitForUpdateReconnect(prevBuildId, actionLabel) {
  if (updateReconnectActive) return;
  updateReconnectActive = true;
  const deadline = Date.now() + 35000;
  try {
    while (Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, 600));
      try {
        const ok = await loadState({ silent: true });
        if (ok && state.data) {
          const nextBuild = state.data.updates?.buildId || state.data.service?.buildId || '';
          const nextState = state.data.updates?.state || '';
          if (nextState !== 'applying' && nextState !== 'waiting_idle' && (!prevBuildId || nextBuild !== prevBuildId || state.data.updates?.lastRollback)) {
            if (state.data.updates?.lastRollback?.automatic) {
              showToast('新版本未能正常就绪，已自动回退到上一版本 ' + (state.data.updates.currentVersion || '') + '。', 'error');
            } else {
              showToast(actionLabel + '已完成，当前版本 ' + (state.data.updates?.currentVersion || '') + '。', 'ok');
            }
            return;
          }
        }
      } catch {}
    }
  } finally {
    updateReconnectActive = false;
  }
}

function renderUpdates(data) {
  const updates = data?.updates || {};
  const st = updates.state || 'idle';
  const busy = ['checking', 'downloading', 'verifying', 'waiting_idle', 'applying'].includes(st);
  const available = updates.available || null;
  const summary = updateStateSummary(updates);

  const current = $('upd-current');
  if (current) current.textContent = updates.currentVersion || '未知版本';
  setText($('upd-current-sub'), '构建标识 · ' + (updates.buildId || data.service?.buildId || 'development'));
  setDot($('upd-state-dot'), summary.tone);
  setText($('upd-state-word'), summary.word);
  setText($('upd-last-check'), updates.lastCheckedAt ? '上次检查 · ' + fmtTime(updates.lastCheckedAt) : '尚未检查');

  const channelSel = $('upd-channel');
  if (channelSel && document.activeElement !== channelSel) {
    channelSel.value = updates.channel === 'preview' ? 'preview' : 'stable';
    channelSel.disabled = busy;
  }
  const autoBox = $('upd-auto-check');
  if (autoBox && document.activeElement !== autoBox) {
    autoBox.checked = updates.autoCheck !== false;
    autoBox.disabled = busy;
  }

  const releaseBox = $('upd-release-box');
  if (releaseBox) {
    if (available && available.version) {
      releaseBox.hidden = false;
      const chLabel = available.channel === 'preview' ? '预览版' : '稳定版';
      const sizeText = available.size ? ' · ' + fmtBytes(available.size) : '';
      setText($('upd-release-title'), `新版本 ${available.version}（${chLabel}${sizeText}）`);
      setText($('upd-release-notes'), available.notes || '本次更新包含问题修复与体验改进。');
    } else {
      releaseBox.hidden = true;
    }
  }

  const topBanner = $('update-top-banner');
  const topText = $('update-top-banner-text');
  const overviewCard = $('overview-update-card');
  if (available && available.version && ['available', 'downloading', 'verifying', 'waiting_idle', 'applying'].includes(st)) {
    const firstLine = String(available.notes || '').split(/\r?\n/).map(line => line.trim()).find(Boolean) || '';
    const brief = firstLine ? '：' + (firstLine.length > 64 ? firstLine.slice(0, 64) + '…' : firstLine) : '';
    if (topBanner && topText) {
      topBanner.hidden = state.view === 'overview' || state.view === 'maintenance';
      setText(topText, `发现新版本 ${available.version}${brief}`);
    }
    if (overviewCard) {
      overviewCard.hidden = false;
      setText($('overview-update-title'), `发现 Kimi Work+ 新版本 ${available.version}`);
      setText($('overview-update-notes'), available.notes || '包含稳定性修复与新功能，下载并验签后在空闲时替换重启。');
    }
  } else {
    if (topBanner) topBanner.hidden = true;
    if (overviewCard) overviewCard.hidden = true;
  }

  const progressWrap = $('upd-progress-wrap');
  const progressBar = $('upd-progress-bar');
  const progressLabel = $('upd-progress-label');
  if (progressWrap && progressBar && progressLabel) {
    if (['downloading', 'verifying', 'waiting_idle', 'applying'].includes(st)) {
      progressWrap.hidden = false;
      if (st === 'downloading') {
        const pct = Math.max(0, Math.min(100, Number(updates.progress?.percent || 0)));
        progressBar.style.width = pct + '%';
        const written = fmtBytes(updates.progress?.written || 0);
        const total = updates.progress?.total ? ' / ' + fmtBytes(updates.progress.total) : '';
        setText(progressLabel, `正在下载更新包：${pct}%（${written}${total}）`);
      } else if (st === 'verifying') {
        progressBar.style.width = '100%';
        setText(progressLabel, '下载完成，正在校验 Ed25519 签名与包体完整性…');
      } else if (st === 'waiting_idle') {
        progressBar.style.width = '100%';
        setText(progressLabel, updates.idleReason || '正在等待 Kimi Work 与本机网关空闲后再替换…');
      } else {
        progressBar.style.width = '100%';
        setText(progressLabel, '正在替换安装包并重启 Kimi Work+，网关恢复后页面会自动重连…');
      }
    } else {
      progressWrap.hidden = true;
    }
  }

  const errBox = $('upd-error-box');
  if (errBox) {
    const err = updates.error;
    if (err && (err.message || err.code)) {
      errBox.hidden = false;
      setText($('upd-error-msg'), err.message || '更新未能完成，当前版本与网关保持不变。');
      setText($('upd-error-next'), err.nextStep ? '下一步：' + err.nextStep : '');
      const diagDetail = [
        err.stage ? 'stage=' + err.stage : '',
        err.code ? 'code=' + err.code : '',
        err.detail ? 'detail=' + err.detail : '',
      ].filter(Boolean).join('\n');
      setText($('upd-error-detail'), diagDetail || '无附加诊断信息');
    } else {
      errBox.hidden = true;
    }
  }

  const historyNote = $('upd-history-note');
  if (historyNote) {
    const rb = updates.lastRollback;
    const applied = updates.lastApplied;
    const rbTime = rb?.at ? Date.parse(rb.at) : 0;
    const apTime = applied?.at ? Date.parse(applied.at) : 0;
    if (rb && rbTime >= apTime) {
      historyNote.hidden = false;
      historyNote.classList.toggle('note--warn', Boolean(rb.automatic));
      const msg = rb.automatic
        ? `上次尝试更新到 ${rb.fromVersion || '新版本'} 未能正常启动，已自动回退到 ${rb.toVersion || updates.currentVersion}（${fmtTime(rb.at)}）。`
        : `已手动回退到版本 ${rb.toVersion || updates.currentVersion}（${fmtTime(rb.at)}）。`;
      setText(historyNote, msg);
    } else if (applied && applied.toVersion) {
      historyNote.hidden = false;
      historyNote.classList.remove('note--warn');
      setText(historyNote, `最近一次已于 ${fmtTime(applied.at)} 从 ${applied.fromVersion || '旧版本'} 更新到 ${applied.toVersion}。`);
    } else {
      historyNote.hidden = true;
    }
  }

  const checkBtn = $('upd-check');
  if (checkBtn) checkBtn.disabled = busy;
  const downloadBtn = $('upd-download');
  if (downloadBtn) {
    downloadBtn.hidden = !(available && available.version && (st === 'available' || st === 'error'));
    downloadBtn.disabled = busy;
    downloadBtn.textContent = st === 'error' ? '重试下载并更新' : '下载并更新';
  }
  const cancelBtn = $('upd-cancel');
  if (cancelBtn) {
    cancelBtn.hidden = !(st === 'downloading' || st === 'waiting_idle');
    cancelBtn.disabled = st === 'applying';
  }
  const rollbackBtn = $('upd-rollback');
  if (rollbackBtn) {
    const rbAvail = updates.rollback?.available === true;
    rollbackBtn.hidden = !rbAvail;
    rollbackBtn.disabled = busy;
    rollbackBtn.textContent = rbAvail && updates.rollback.version
      ? `回退到上一版本（${updates.rollback.version}）`
      : '回退到上一版本';
  }

  scheduleUpdateFastPoll(['downloading', 'verifying', 'waiting_idle'].includes(st));
  if (st === 'applying') {
    void waitForUpdateReconnect(updates.buildId || data.service?.buildId || '', '版本替换');
  }
}

async function updateAction(endpoint, body = {}, okMessage = '') {
  const prevBuild = state.data?.updates?.buildId || state.data?.service?.buildId || '';
  const result = await request(endpoint, { method: 'POST', body });
  if (result?.updates && state.data) {
    state.data.updates = result.updates;
    renderUpdates(state.data);
  }
  if (endpoint === '/api/updates/check') {
    const u = result?.updates;
    if (u?.state === 'available') showToast('发现新版本 ' + (u.available?.version || '') + '。', 'ok');
    else if (u?.state === 'up_to_date') showToast('当前已是最新版本。', 'ok');
    else if (u?.error?.message) showToast(u.error.message, 'error');
  } else if (endpoint === '/api/updates/download') {
    showToast('已开始下载更新包，验签并等待空闲后会自动替换重启。', 'ok');
  } else if (endpoint === '/api/updates/rollback') {
    showToast('已启动回退到上一版本，等待空闲后将替换并重启。', 'ok');
    void waitForUpdateReconnect(prevBuild, '回退');
  } else if (okMessage) {
    showToast(okMessage, 'ok');
  }
  return result;
}

function renderPreviewLifecycle(data) {
  renderUpdates(data);
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
  const migration = data.migration;
  const banner = $('migration-banner');
  const note = $('migration-note');
  const actions = $('migration-actions');
  const topBanner = $('migration-top-banner');
  const topText = $('migration-top-banner-text');
  if (banner && note && actions) {
    if (migration && (migration.status === 'needs_authorization' || migration.status === 'in_progress')) {
      banner.hidden = false;
      actions.hidden = false;
      const pending = Number(migration.pendingCount || 0);
      const message = pending > 0
        ? `检测到预览版遗留的 ${pending} 项凭据尚未完成迁移，需要系统授权读取旧钥匙串条目。原预览条目与旧目录均已保留。`
        : '预览版数据迁移尚未完成，可点击下方按钮继续迁移并读回核对。';
      setText(note, message);
      if (topBanner && topText) {
        topBanner.hidden = false;
        setText(topText, pending > 0
          ? `检测到预览版 ${pending} 项旧凭据待授权迁移，原数据与旧条目均已保留。`
          : '预览版数据迁移尚未完成，请前往设置页继续迁移。');
      }
    } else if (migration && migration.status === 'completed' && migration.legacyDirPreserved) {
      banner.hidden = false;
      actions.hidden = true;
      setText(note, '已从预览版完成配置与凭据迁移。原预览目录和旧钥匙串条目均已保留，确认无误后再自行清理。');
      if (topBanner) topBanner.hidden = true;
    } else {
      banner.hidden = true;
      actions.hidden = true;
      if (topBanner) topBanner.hidden = true;
    }
  }
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
  showToast('后台服务状态已更新；登录启动与进程运行分开显示。', 'ok');
  await loadState({ silent: true });
}


function resetRecentSignature() { recentSignature = ''; }

export {
  isInFlight,
  previewServiceAction,
  refreshOverviewToday,
  renderActions,
  renderOperation,
  renderOverview,
  renderPreviewLifecycle,
  renderShell,
  renderUpdates,
  renderWorkspaceSync,
  resetRecentSignature,
  runOverviewIssueAction,
  syncState,
  updateAction,
};

import { applyState, configInfo, configReady, credentialAuthPending, onboardBusy, renderOnboard, renderOnboardOverview } from './app-connections.js';
import { $, ACTION_LABEL, ACTION_STATE, API, DIRECT_BYPASS_WORD, DIRECT_SKIP_REASON, GUIDE_DISMISSED_STORE, INTEGRATION_MODES, POLICY_MODE_WORD, directReasonText, displayModelId, dom, fmtCount, fmtDuration, fmtK, fmtMs, fmtTime, fmtTokens, reasonText, request, revealWorkspaceSection, setDot, setHidden, setText, showToast, state } from './app-core.js';
import { connectionRowsOf } from './app-models.js';
import { clearDisconnectConfirm, handleSaveFailure, loadState, runAction, setView } from './app.js';

/* ---------- render: shell ---------- */

/* 保存即自动同步之后，日常不会再出现「应用已保存的连接」这一步。横幅只保留
   三种真实需要用户动手的情况：接管外部启动的 Kimi、重启刷新模型菜单、旧版
   凭据待授权。 */
function autoSyncOf(data) {
  const value = data?.integration?.autoSync;
  return value && typeof value === 'object' ? value : {};
}

function syncNeedsImport(data) {
  const info = data?.integration || {};
  if (info.imported !== true) return true;
  if (Array.isArray(info.missingModels) && info.missingModels.length) return true;
  if (Array.isArray(info.missingProviders) && info.missingProviders.length) return true;
  return false;
}

function restartBannerState(data) {
  const auto = autoSyncOf(data);
  const models = Array.isArray(data?.models) ? data.models : [];
  const running = data?.kimiProcess?.running === true;
  const managed = data?.integration?.desktop?.managed === true;
  const pendingWork = auto.pending === true || syncNeedsImport(data);
  const waitingUnmanaged = auto.state === 'waiting_unmanaged'
    || (running && !managed && models.length > 0 && pendingWork);
  if (waitingUnmanaged) {
    return {
      action: 'restart-kimi',
      label: '重启并接管 Kimi 生效',
      text: '检测到外部启动的官方 Kimi Work 正在运行。需要重启并接管一次，才能把最新模型配置同步到它的模型菜单。',
    };
  }
  const unadopted = running && managed && data?.integration?.applied === true
    && data?.integration?.adopted !== true
    && !['idle', 'scheduled', 'syncing', 'deferred_offline', 'waiting_unmanaged'].includes(auto.state || '');
  if (auto.state === 'requires_restart' || nativeRestartReceipt(data) || unadopted) {
    return {
      action: 'restart-kimi',
      label: '立即重启 Kimi 生效',
      text: '模型菜单已更新，重启 Kimi Work 后即可在下拉框看到最新变化。',
    };
  }
  if (credentialAuthPending(data)) {
    return {
      action: 'models',
      label: '前往模型与服务商处理',
      text: '部分已保存的连接凭据暂时无法读取。请到「模型与服务商」页查看原因并重新授权。',
    };
  }
  return null;
}

function renderShell(data) {
  const gw = (data && data.gateway) || {};
  const running = gw.state === 'running';
  setDot(dom.railGatewayDot, running ? 'ok' : 'bad');
  setText(dom.railGatewayText, running ? '网关运行中' : '网关已停止');
  setText(dom.railGatewayUrl, typeof gw.url === 'string' && gw.url ? gw.url : '—');
  setText(dom.sync, state.lastSync ? '同步于 ' + fmtTime(state.lastSync) : '尚未同步');

  /* 凭据待授权的提示必须活过轮询：升级后用户会停留在任意页面，
     只在模型页说一次会让人以为已经恢复。 */
  const banner = restartBannerState(data);
  setHidden(dom.restartBanner, !banner);
  if (banner) {
    setText(dom.restartBannerText, banner.text);
    setText(dom.restartBannerLink, banner.label);
    if (dom.restartBannerLink) {
      dom.restartBannerLink.dataset.bannerAction = banner.action;
      dom.restartBannerLink.disabled = Boolean(state.pendingAction) || isInFlight(data && data.operation);
    }
  }
  setText(dom.navModelCount, fmtCount((data.models || []).length));
  /* 直连模式下预算保护不参与请求，再显示「已达预算」只会让用户以为被拦住了。 */
  const budgetMessage = savedIntegrationMode(data) === 'direct' ? '' : budgetNotice(data.budget);
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
  if (modelTakeoverRemoved(data)) {
    const blocked = data?.compatibility?.guards?.modelSyncWriteBlocked === true;
    return { code: 'restored', tone: 'neutral', word: blocked ? '已移除接管' : '已移除接管，可重新同步',
      note: blocked
        ? '本产品同步的模型来源已从官方 Kimi 移除；本地服务商和模型仍保留。当前官方 Kimi 未通过模型同步兼容性检查，暂不可重新同步。'
        : '本产品同步的模型来源已从官方 Kimi 移除；本地服务商和模型仍保留，点击「同步到 Work」即可重新接管。' };
  }
  /* 直连模式的判断必须排在网关分支之前：在直连下，官方 Kimi 直接访问服务商，
     网关是否运行、有没有受管子进程都不影响模型可用性。 */
  const direct = directModeState(data);
  if (direct) return direct;
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
  if (has('not_installed', 'application_not_found', 'kimi_application_missing')) {
    return { code: 'not-installed', tone: 'warn', word: '尚未安装官方 Kimi',
      note: '请先安装官方 Kimi 并正常打开一次，再回到本面板继续；已有登录和 Work 数据会保留。' };
  }
  /* 回执要求重启、或切换收尾未完成时，读回成功也不能报成绿色已同步：
     前者要先重启，后者还差一次清理。 */
  const nativePending = nativeRestartReceipt(data) || directRouteCleanupPending(data);
  if (info.readback && info.applied === true && !nativePending) {
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
  /* 回执明确要求重启（含旧后端的 state=requires-restart）时先处理重启：
     此时说「已同步」或先清理旧来源都会让人错过真正要做的下一步。 */
  if (nativeRestartReceipt(data)) {
    return { code: 'restart', tone: 'warn', word: '等待 Kimi 重启',
      note: '模型配置已写入。请等当前任务结束，再正常重启 Kimi 并确认模型。' };
  }
  /* 从直连切回网关后，旧的服务商来源也可能还没清理：这时不能绿色显示
     「已同步」，同步入口要留着让用户重试清理。这句话不假设官方 Kimi 已
     切到哪一侧，只说明收尾欠清理。 */
  if (directRouteCleanupPending(data)) {
    return { code: 'pending', tone: 'warn', word: '切换收尾未完成',
      note: '新配置已写入并读回，但旧的服务商来源仍保留着；请再点一次「同步到 Work」重试清理旧来源并同步。在清理完成前不要退出后台服务，也不要当作旧路由已清理。' };
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

/* ---------- 总览：直白状态与唯一主动作 ---------- */

/* 总览只说用户此刻要做什么：能直接切换、正在自动同步、要重启、要接管，
   或者只差一次登录/授权。原生对账细节仍留在设置页与诊断里。 */
function overviewState(data) {
  const base = { launch: 'launch-kimi', launchLabel: '打开 Kimi Work' };
  if (!data) return { code: 'loading', tone: 'neutral', word: '读取中', note: '正在读取接入状态…', ...base };
  const auto = autoSyncOf(data);
  const models = Array.isArray(data.models) ? data.models : [];
  const running = data?.kimiProcess?.running === true;
  const managed = data?.integration?.desktop?.managed === true;
  const direct = savedIntegrationMode(data) === 'direct';
  const operation = data?.operation || {};

  if (modelTakeoverRemoved(data)) {
    return { code: 'restored', tone: 'neutral', word: '已移除接管',
      note: '官方 Kimi 已恢复自己的模型来源；本地服务商与模型仍保留，可随时重新同步接管。', ...base };
  }
  if (data?.compatibility?.guards?.modelSyncWriteBlocked === true) {
    return { code: 'blocked', tone: 'bad', word: '同步需要处理',
      note: '当前官方 Kimi 版本未通过模型同步兼容性检查，已暂停写入；本机网关照常运行。请更新官方 Kimi 或本软件后重试。', ...base };
  }
  if (auto.state === 'waiting_login') {
    return { code: 'login', tone: 'warn', word: '需要登录 Kimi',
      note: '请先在官方 Kimi 窗口完成登录；保存的模型会在登录后自动同步。', ...base };
  }
  if (!models.length) {
    return { code: 'empty', tone: 'neutral', word: '等待添加模型',
      note: '先在「模型与服务商」添加至少一个服务商与模型；保存后会自动同步。', ...base };
  }
  if (auto.state === 'requires_restart' || nativeRestartReceipt(data)) {
    return { code: 'restart', tone: 'warn', word: '需重启 Kimi 生效',
      note: '模型菜单已更新；重启 Kimi 后即可在下拉框里看到最新模型。',
      launch: 'restart-kimi', launchLabel: '立即重启 Kimi 生效' };
  }
  const waitingUnmanaged = auto.state === 'waiting_unmanaged'
    || (running && !managed && (auto.pending === true || syncNeedsImport(data)));
  if (waitingUnmanaged) {
    return { code: 'takeover', tone: 'warn', word: '需重启并接管 Kimi',
      note: '检测到外部启动的官方 Kimi 正在运行；重启并接管一次即可同步最新模型。',
      launch: 'restart-kimi', launchLabel: '重启并接管 Kimi' };
  }
  if (auto.state === 'scheduled' || auto.state === 'syncing' || operation.action === 'auto-sync') {
    return { code: 'syncing', tone: 'warn', word: '正在自动同步…',
      note: '正在把已保存的模型写入 Kimi Work；完成后即可在模型下拉框看到。', ...base };
  }
  if (auto.state === 'deferred_offline' || (!running && !direct && syncNeedsImport(data))) {
    return { code: 'deferred', tone: 'neutral', word: '已保存，打开 Kimi 时自动同步',
      note: '模型已保存；打开 Kimi Work 时会自动完成同步，不需要再点同步按钮。', ...base };
  }
  if (auto.state === 'failed') {
    if (auto.reason === 'direct_prepare_required') {
      return { code: 'prepare', tone: 'warn', word: '同步需要处理',
        note: '直连模式首次写入静态 hosts 需要一次系统授权。打开「设置 → 接入与更新」，点一次「重新同步模型目录」完成授权即可。', ...base };
    }
    const reason = auto.reason ? reasonText(auto.reason) : '';
    return { code: 'failed', tone: 'bad', word: '同步需要处理',
      note: (reason || '后台自动同步未能完成') + '。可在「设置 → 接入与更新」查看原因并重试。', ...base };
  }
  if (direct) {
    const ready = directImportComplete(data) && directStaticHostsReady(data);
    if (!ready) {
      return { code: 'deferred', tone: 'warn', word: '已保存，打开 Kimi 时自动同步',
        note: '直连配置尚未写入官方 Kimi；打开 Kimi Work 或到设置页重新同步一次即可完成。', ...base };
    }
    return { code: 'ready', tone: 'ok', word: '已就绪',
      note: '官方 Kimi 正在直连第三方模型；退出本软件后也能继续使用。', ...base };
  }
  const info = data?.integration || {};
  if (auto.state === 'synced' || info.adopted === true || (info.applied === true && info.desktop?.managed === false)) {
    return { code: 'ready', tone: 'ok', word: '已就绪',
      note: '可以在 Kimi 的模型下拉框里任意切换官方与第三方模型，当前对话会保留。', ...base };
  }
  return { code: 'pending', tone: 'warn', word: '等待同步',
    note: '模型已保存，后台会在 Kimi 空闲时自动同步；不用重复点击。', ...base };
}

function renderOverviewStatus(data) {
  const info = overviewState(data);
  setDot(dom.onboardVerdictDot, info.tone);
  setText(dom.onboardVerdictWord, info.word);
  setText(dom.onboardVerdictNote, info.note);
  if (dom.onboardLaunch) {
    const busy = isInFlight(data?.operation);
    dom.onboardLaunch.dataset.action = info.launch || 'launch-kimi';
    dom.onboardLaunch.dataset.profileMode = '';
    dom.onboardLaunch.hidden = false;
    dom.onboardLaunch.disabled = busy || Boolean(state.pendingAction);
    setText(dom.onboardLaunch, info.launchLabel || '打开 Kimi Work');
    dom.onboardLaunch.setAttribute('aria-busy', busy ? 'true' : 'false');
  }
  if (dom.onboardLaunchHint) {
    const hint = info.code === 'restart'
      ? '点击后会正常退出 Kimi，再沿用当前登录和 Work 数据重新启动，约 2–3 秒。'
      : info.code === 'takeover'
        ? '将正常退出当前外部启动的 Kimi，并由本产品受管重启后完成同步。'
        : '';
    setHidden(dom.onboardLaunchHint, !hint);
    setText(dom.onboardLaunchHint, hint);
  }
}

/* ---------- 接入模式：本机网关 / 零网关直连 ---------- */

/* 缺省一律按网关处理：老后端不会返回这两个字段，界面必须保持旧行为。 */
function modelTakeoverRemoved(data) {
  return data?.operation?.action === 'disconnect-models'
    && data.operation.state === 'removed' && data?.integration?.imported !== true;
}

function savedIntegrationMode(data) {
  return data?.integrationMode === 'direct' ? 'direct' : 'gateway';
}

function effectiveIntegrationMode(data) {
  return data?.integration?.mode === 'direct' ? 'direct' : 'gateway';
}

function directModeOf(data) {
  const direct = data?.directMode;
  return direct && typeof direct === 'object' ? direct : null;
}

/* 切换收尾：新配置已写入并读回，但旧的受管来源还没清理掉。
   directMode 的布尔值来自最近一次导入回执，回执本身也要看（旧后端只给后者）。
   这时说「直连已就绪」「切换全部完成」都会让人以为可以退出后台。
   这个判断不限于直连模式：从直连切回网关后，旧来源同样可能欠清理，
   所以网关分支必须共用同一个入口。 */
function directRouteCleanupPending(data) {
  if (directModeOf(data)?.routeCleanupPending === true) return true;
  const receipt = data?.integration?.nativeImport;
  return receipt?.routeCleanupPending === true || receipt?.reason === 'route_cleanup_pending';
}

/* 原生导入回执里的重启证据。受管子进程退出后，下次启动会读取已写入配置；
   非托管运行不是永久欠重启，但清理未完成仍必须继续提示。 */
function nativeRestartReceipt(data) {
  const info = data?.integration || {};
  if (savedIntegrationMode(data) !== 'direct' && info.applied === true && info.desktop?.managed === false) return false;
  const receipt = info.nativeImport || {};
  return receipt.requiresRestart === true || receipt.restartDeferred === true || receipt.state === 'requires-restart';
}

/* 直连写入是否已经落到官方 Kimi。skippedModels 里的模型本来就不会被写入，
   所以不能拿别名总数和 totalModels 比；必须按后端给出的可导入模型 ID 逐个
   核对，且回执要同时是 ready 与 applied（applied 才代表官方 store 和生成的
   config 都读到了，不只是请求被接受）。旧后端没有 eligibleModelIds 时用
   readback.modelAliases 作为 spec 已被读回覆盖的证据；两者都没有时只采信
   adopted 回执，不凭数量猜测当前内容。 */
function directImportComplete(data) {
  const info = data?.integration || {};
  if (info.mode !== 'direct') return false;
  if (info.state !== 'ready' || info.applied !== true) return false;
  const aliases = info.aliases && typeof info.aliases === 'object' ? info.aliases : {};
  const hasAlias = (id) => Object.prototype.hasOwnProperty.call(aliases, id);
  const direct = directModeOf(data);
  if (Array.isArray(direct?.eligibleModelIds)) {
    const ids = direct.eligibleModelIds.filter((id) => typeof id === 'string' && id);
    return ids.length > 0 && ids.every(hasAlias);
  }
  const readbackAliases = info.readback?.modelAliases;
  if (readbackAliases && typeof readbackAliases === 'object') {
    const recorded = Object.keys(readbackAliases)
      .filter((id) => typeof readbackAliases[id] === 'string' && readbackAliases[id]);
    return recorded.length > 0 && recorded.every(hasAlias);
  }
  return info.adopted === true;
}

function directRestartRequired(data) {
  const info = data?.integration || {};
  /* 两种重启证据共用同一入口：官方明确 deferred，或配置已读回但受管进程
     尚未采用。只有受管进程才存在「已写入、未采用」：未托管或 Kimi 未运行时
     后台的 adopted 恒为 false，不能据此要求重启。 */
  if (directImportComplete(data)) return info.desktop?.managed === true && info.adopted !== true;
  return nativeRestartReceipt(data);
}

/* directMode 行存在时它就是 hosts 的唯一答案：Windows 非 macOS 的 setup
   默认值是 hosts，拿它回退等于跳过真实检查。只有老后端根本没有 directMode
   行时才允许看 setup。 */
function directStaticHostsReady(data) {
  const direct = directModeOf(data);
  if (direct) return direct.staticHostsReady === true;
  return data?.setup?.dns?.state === 'hosts';
}

/* 直连模式下 syncState 的唯一入口。返回 null 表示「这句话交给网关分支说」。 */
function directModeState(data) {
  if (savedIntegrationMode(data) !== 'direct') return null;
  const info = data?.integration || {};
  const direct = directModeOf(data);
  if (data?.compatibility?.status === 'checking' && direct?.supported !== true) {
    return { code: 'checking', tone: 'neutral', word: '正在检查官方 Kimi 兼容性…',
      note: '正在读取官方 Kimi 版本与直连支持，稍候自动刷新。' };
  }
  /* 官方版本不支持直连时，任何「待写入」都是误导：再同步也不会成功。 */
  if (direct?.supported === false) {
    return { code: 'unsupported', tone: 'bad', word: '当前官方 Kimi 不支持直连',
      note: '原生直连需要 3.2.28+ 的 byokSources v3 存储。请更新官方 Kimi，或改用本机网关模式。' };
  }
  /* directImportComplete 已经要求 integration.mode === 'direct'。 */
  const synced = directImportComplete(data);
  /* 一个可直连模型都没有：无论回执怎么写，模型目录都进不了官方 Kimi。
     新后端给 eligibleModelIds 时它就是更精确的答案（空数组同样是证据）。 */
  const noEligible = Array.isArray(direct?.eligibleModelIds)
    ? direct.eligibleModelIds.every((id) => typeof id !== 'string' || !id)
    : direct?.eligibleModels === 0;
  if (direct && noEligible) {
    return { code: 'pending', tone: 'warn', word: '没有可直连的模型',
      note: '直连只导入已声明工具调用、已绑定连接并配置了密钥的模型；请先在模型页补齐，再同步到 Work。' };
  }
  /* 官方延迟应用时先重启，重启后旧回执不再阻塞清理。此时若先提示清理，
     用户会在尚未采用新配置时反复同步，并看不到真正的下一步。 */
  if (directRestartRequired(data)) {
    return { code: 'restart', tone: 'warn', word: '需要重启 Kimi',
      note: '直连配置已写入，正常重启 Kimi 后完成应用；在此之前请保留后台服务。' };
  }
  /* 切换收尾未完成：新配置已写入，但旧来源还在。必须排在「待写入」之前：
     这时说「待写入直连配置」同样会让人以为还没写，而实际上只差清理。
     说「已就绪」则更糟，会让人直接退出后台并以为旧路由已经清理。 */
  if (directRouteCleanupPending(data)) {
    return { code: 'pending', tone: 'warn', word: '切换收尾未完成',
      note: '新的直连配置已写入并读回，旧的服务商来源仍保留着；请再点一次「同步到 Work」重试清理。在此之前不要退出后台服务，也不要当作旧路由已清理。' };
  }
  /* 还没同步到官方 Kimi：若已有非受管官方 Kimi 运行，自动启动会被安全边界拦截，
     必须先引导用户受管重启，再写入直连配置。 */
  if (!synced) {
    if (info.desktop?.state === 'needs_user_close' || (data?.kimiProcess?.running === true && info.desktop?.managed === false)) {
      return { code: 'manual-exit', tone: 'warn', word: '需先受管重启 Kimi',
        note: '检测到当前官方 Kimi 未受本产品托管，写入直连配置前请先点击「正常退出并重启 Kimi」，完成受管启动后再同步到 Work。' };
    }
    return { code: 'pending', tone: 'warn', word: '待写入直连配置',
      note: '已选择零网关直连模式；点「同步到 Work」会自动启动 Kimi 并写入服务商地址与静态 hosts。' };
  }
  /* directMode 行存在时它就是 hosts 的唯一答案：Windows 非 macOS 的 setup
     默认值是 hosts，拿它回退等于跳过真实检查。 */
  if (directStaticHostsReady(data)) {
    /* 用户直接双击官方 Kimi 时没有受管子进程，但直连本来就不依赖它：
       这时给警告让人「重启成受管进程」是错的。 */
    if (data?.kimiProcess?.running === true && info.desktop?.managed === false) {
      return { code: 'ready', tone: 'ok', word: '直连运行中（免开本软件）',
        note: '官方 Kimi 正直连第三方模型，无需保持 KimiWork + 运行。如需思考翻译或多账号切换，再从这里重启托管。' };
    }
    return { code: 'ready', tone: 'ok', word: '直连已就绪',
      note: '已写入直连模型与静态 hosts，退出本软件及后台服务后也可直接在官方 Kimi 中使用。' };
  }
  /* 模型写进去了、静态 hosts 还没补齐：这时说「已就绪」会让人以为能退出后台。 */
  return { code: 'pending', tone: 'warn', word: '待补齐静态 hosts',
    note: '直连模型已写入官方 Kimi；请再点一次「同步到 Work」写入静态 hosts，之后才能彻底退出后台服务。' };
}

function nativeConnectionText(data) {
  const info = data.integration || {};
  const preparation = info.preparation || {};
  if (modelTakeoverRemoved(data)) {
    return '已移除本产品接管的模型来源，本地服务商和模型仍保留。点「重新准备接入」或模型页「同步到 Work」可重新接管。';
  }
  /* 直连模式下官方 Kimi 不经过网关，所以网关侧的 DNS/桥接文案都不成立。 */
  if (savedIntegrationMode(data) === 'direct') {
    const direct = directModeOf(data);
    if (data?.compatibility?.status === 'checking' && direct?.supported !== true) {
      return '正在检查官方 Kimi 兼容性，稍候自动刷新。';
    }
    if (direct?.supported === false) {
      return '当前官方 Kimi 版本不支持原生直连（需要 3.2.28+ 的 byokSources v3 存储）；可改用本机网关模式。';
    }
    if (directRestartRequired(data)) {
      return '直连配置已写入，正常重启 Kimi 后完成应用；在此之前请保留后台服务。';
    }
    /* 收尾未完成不等同导入失败：新配置已写入，只是旧来源还留着。
       这句话必须排在失败码之前，否则 reason 会把它说成一次失败。 */
    if (directRouteCleanupPending(data)) {
      return '新的直连配置已写入并读回，但旧的服务商来源还保留着，切换收尾未完成；点「同步到 Work」可重试清理。在此之前不要退出后台服务，也不要当作旧路由已清理。';
    }
    const directComplete = directImportComplete(data);
    if (!directComplete && (info.desktop?.state === 'needs_user_close' || (data?.kimiProcess?.running === true && info.desktop?.managed === false))) {
      return '当前官方 Kimi 未受本产品托管，无法写入直连配置。请先点击「正常退出并重启 Kimi」，完成受管启动后再同步到 Work。';
    }
    /* 具体失败/等待码优先于宽泛的 direct_import_required，避免等待 Work、
       等待登录或启动失败被默认待同步提示吞掉。 */
    const specificInfoReason = info.reason && info.reason !== 'direct_import_required' ? info.reason : '';
    const primaryReason = specificInfoReason || info.nativeImport?.reason || (!directComplete ? info.desktop?.reason : '') || '';
    const directText = directReasonText(primaryReason);
    if (directText && primaryReason !== 'direct_import_required') return directText;
    if (!directComplete) {
      for (const candidate of [info.nativeImport?.reason, info.desktop?.reason, specificInfoReason]) {
        if (typeof candidate === 'string' && candidate && candidate !== 'native_import_not_verified') {
          const translated = reasonText(candidate);
          if (translated && translated !== candidate) return translated;
        }
      }
    }
    const fallbackDirectText = directReasonText(info.reason || '');
    if (fallbackDirectText) return fallbackDirectText;
    if (preparation.state === 'preparing') return '正在准备首次接入，请处理系统授权窗口。';
    if (directComplete) {
      return directStaticHostsReady(data)
        ? '已通过直连模式写入官方 Kimi 且静态 hosts 就绪；退出本软件与后台服务后仍可直接使用这些模型。'
        : '直连模型已写入官方 Kimi；点「同步到 Work」补齐静态 hosts 后即可彻底退出后台服务。';
    }
    return '已选择零网关直连模式，但还没有写入官方 Kimi；请在模型页点「同步到 Work」。';
  }
  if (preparation.state === 'preparing') return '正在准备首次接入，请处理系统授权窗口。';
  if (preparation.state === 'cancelled') return '上次系统授权已取消。点「打开 / 启动 Kimi」或「重新准备接入」可继续。';
  if (preparation.state === 'failed' || preparation.state === 'interrupted') return '接入准备未完成，请检查系统授权后重试。';
  if (nativeRestartReceipt(data)) {
    return '模型已接入 Kimi 原生选择器，但需要重启 Kimi Work 后才会生效。';
  }
  /* 从直连切回网关后旧来源仍可能欠清理；这句话必须排在「已读回」之前，
     否则连接页会把未收尾的切换说成已完成。 */
  if (directRouteCleanupPending(data)) {
    return '新配置已写入并读回，但旧的服务商来源仍保留着，切换收尾未完成；点「同步到 Work」可重试清理旧来源并同步。在清理完成前不要退出后台服务，也不要当作旧路由已清理。';
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
    conversation_rollback_failed: '会话绑定恢复尚未完成。请在「旧会话改绑模型」中刷新。',
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
    not_installed: '尚未安装官方 Kimi。请先安装并正常打开官方 Kimi，再回到本面板继续。',
    application_not_found: '官方 Kimi 应用入口不可用。请重新安装官方 Kimi 后再试。',
    base_url_changed: '检测到本机网关地址变化，正在通过官方接口恢复已有接入。',
    profile_mode_change_requires_exit: '切换登录方式前，请先正常退出当前 Kimi，再选择所需方式启动。',
    official_kimi_already_running: '检测到另一个未受托管的官方 Kimi 正在运行。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。',
    unmanaged_kimi_running: '检测到另一个未受托管的官方 Kimi 正在运行。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。',
    native_bridge_unavailable: '当前未连接到受管 Kimi 通道。点「同步到 Work」会自动启动受管 Kimi；若 Kimi 已在运行，请点「正常退出并重启 Kimi」。',
    native_bridge_waiting_work: '官方 Kimi 已运行，但 Work 页面尚未就绪。请确认 Kimi 已登录并切换到 Work 视图，或点「打开 Kimi Work」后重试。',
    native_bridge_waiting_settings: '正在等待 Kimi 设置页完成原生导入。',
    native_bridge_settings_open_refused: '官方 Kimi 拒绝打开 Work 设置页。请确认 Kimi 窗口已就绪且无阻塞提示后重试。',
    native_bridge_activation_refused: '未能自动唤起 Kimi Work 视图。请手动切到官方 Kimi 的 Work 页面后重试。',
    native_bridge_timeout: '等待官方 Kimi 页面响应超时。请确认 Kimi 窗口正常运行后重试。',
    native_bridge_waiting_launch: '等待 Kimi 启动并建立接入通道。',
    kimi_process_state_unknown: '无法确认官方 Kimi 当前运行状态。请先手动退出官方 Kimi，再从面板重试。',
    kimi_start_failed: '未能自动启动官方 Kimi。请确认官方 Kimi 安装正常，或手动打开后再试。',
    native_import_waiting_login: '请先在 Kimi Work 登录并确认当前账户，然后点「打开 / 启动 Kimi」继续。',
    native_import_disconnected: '检测到原生接入已断开。确认需要恢复后，点「打开 / 启动 Kimi」或「重新准备接入」。',
    disconnected: '已移除本产品接管的模型来源，本地服务商和模型仍保留。点「重新准备接入」或模型页「同步到 Work」可重新接管。',
    profile_missing: '当前账号尚未同步本产品模型目录。请在模型页点「同步到 Work」接入。',
    profile_delisted: '官方 Kimi 中本产品模型来源已断开或停用。请在模型页点「同步到 Work」重新接入。',
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


/* 直连模式下网关仍在运行是正常的（翻译、账号切换还要用它），
   但必须说清楚：退出后台不影响第三方模型。 */
function renderGatewayModeFact(data) {
  const el = dom.factGatewayMode;
  if (!el) return;
  const direct = savedIntegrationMode(data) === 'direct';
  if (!direct) {
    setHidden(el, true);
    setText(el, '');
    return;
  }
  setHidden(el, false);
  const ready = directImportComplete(data) && directStaticHostsReady(data);
  setText(el, directRestartRequired(data)
    ? '直连模式 · 需要重启 Kimi'
    : directRouteCleanupPending(data)
      ? '直连模式 · 切换收尾未完成，暂不要退出后台'
      : ready
        ? '直连模式 · 退出后台后第三方模型仍可用'
        : '直连模式 · 尚未写入直连配置');
}


function renderOverview(data, { view } = {}) {
  const showOverview = !view || view === 'overview';
  const showMaintenance = !view || view === 'maintenance';
  if (showMaintenance) {
    setText($('native-state'), nativeConnectionText(data));
    const scope = desktopScopeInfo(data);
    setHidden(dom.nativeRemote, !scope);
    if (scope) {
      setDot(dom.nativeRemoteDot, scope.tone);
      setText(dom.nativeRemoteWord, scope.word);
      setText(dom.nativeRemoteNote, scope.note);
    }
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
    renderDiagnosticsExport(data);
  }
  if (showOverview || showMaintenance) {
    renderCompatibility(data);
  }
  if (showOverview) {
    const gw = (data && data.gateway) || {};
    const running = gw.state === 'running';
    setDot(dom.factGatewayDot, running ? 'ok' : 'bad');
    setText(dom.factGateway, running ? '运行中' : '已停止');
    setText(dom.factGatewayUrl, gw.url || '—');
    renderGatewayModeFact(data);

    const models = Array.isArray(data.models) ? data.models : [];
    setText(dom.factModels, fmtCount(models.length) + ' 个模型');
    const rows = connectionRowsOf(data);
    setText(dom.factModelsSub, '来自 ' + fmtCount(rows ? rows.filter((row) => !row.legacy).length : 0) + ' 条模型服务');

    renderOverviewSubagents(data);
    renderOverviewIssue(data);
    renderOverviewStatus(data);
    renderOverviewToday();
    renderQuickstartGuide(data);
    void refreshOverviewToday();
  }
}

function renderOverviewSubagents(data) {
  const configured = data?.subagents?.configured || {};
  const policy = data?.subagents?.policy || {};
  const runtime = data?.subagents?.runtime || {};
  const mode = configured.mode === 'force' || configured.mode === 'pool' ? configured.mode : 'inherit';
  const runtimeState = typeof runtime.state === 'string' ? runtime.state : '';

  let tone = 'ok';
  if (runtimeState === 'conflict' || policy.mode === 'blocked') tone = 'bad';
  else if (mode !== 'inherit' && runtimeState !== 'file-applied' && runtimeState !== 'loaded') tone = 'warn';
  setDot(dom.overviewSubagentsDot, tone);

  if (mode === 'inherit') {
    setText(dom.overviewSubagentsValue, POLICY_MODE_WORD.inherit || '继承主模型');
    setText(dom.overviewSubagentsNote, '普通子代理跟随主模型；可前往子代理页按角色分配模型。');
    return;
  }
  if (mode === 'force') {
    const model = configured.model ? displayModelId(configured.model) : '已指定';
    setText(dom.overviewSubagentsValue, '固定 · ' + model);
    setText(dom.overviewSubagentsNote, runtimeState === 'file-applied' || runtimeState === 'loaded'
      ? '已写入 Kimi Work config.toml，新建会话生效。'
      : '目标已保存，待确认写入 Kimi Work 配置。');
    return;
  }
  const poolCount = Array.isArray(configured.models) ? configured.models.length : 0;
  setText(dom.overviewSubagentsValue, '按角色 / 模型池（' + fmtCount(poolCount) + ' 个）');
  const roles = configured.roles && typeof configured.roles === 'object' ? configured.roles : {};
  const assigned = ['plan', 'coder', 'explore'].filter((key) => typeof roles[key] === 'string' && roles[key].trim());
  const parts = [];
  if (configured.model) parts.push('默认 ' + displayModelId(configured.model));
  if (assigned.length) parts.push('角色 ' + assigned.join('/'));
  parts.push(runtimeState === 'file-applied' || runtimeState === 'loaded' ? '已写入 config.toml' : '待写入配置');
  setText(dom.overviewSubagentsNote, parts.join(' · '));
}

function isQuickstartDismissed() {
  try {
    return typeof localStorage !== 'undefined' && localStorage.getItem(GUIDE_DISMISSED_STORE) === '1';
  } catch {
    return false;
  }
}

function dismissQuickstartGuide() {
  try {
    if (typeof localStorage !== 'undefined') localStorage.setItem(GUIDE_DISMISSED_STORE, '1');
  } catch {
    /* 忽略 */
  }
  if (state.data) renderQuickstartGuide(state.data);
  else setHidden(dom.quickstartGuide, true);
}

function reopenQuickstartGuide() {
  try {
    if (typeof localStorage !== 'undefined') localStorage.removeItem(GUIDE_DISMISSED_STORE);
  } catch {
    /* 忽略 */
  }
  state.guideCollapsed = false;
  if (state.data) renderQuickstartGuide(state.data);
  else setHidden(dom.quickstartGuide, false);
}

function toggleQuickstartGuide() {
  const current = dom.quickstartGuide?.dataset?.collapsed === 'true';
  state.guideCollapsed = !current;
  if (state.data) renderQuickstartGuide(state.data);
}

function evaluateQuickstartSteps(data) {
  const models = Array.isArray(data?.models) ? data.models : [];
  const verdict = syncState(data);
  const subMode = data?.subagents?.configured?.mode || 'inherit';
  const todayReqs = Number(state.overviewToday?.requests);
  const metricReqs = Number(data?.metrics?.requests);
  const step1 = models.length > 0;
  const step2 = verdict.code === 'synced' || verdict.code === 'ready';
  const step3 = subMode === 'force' || subMode === 'pool';
  const step4 = data?.kimiProcess?.running === true
    || (Number.isFinite(todayReqs) && todayReqs > 0)
    || (Number.isFinite(metricReqs) && metricReqs > 0);
  return [
    { id: 1, done: step1, doneText: '已完成', pendingText: '待完成' },
    { id: 2, done: step2, doneText: '已同步', pendingText: '待完成' },
    { id: 3, done: step3, doneText: '已配置', pendingText: '可选' },
    { id: 4, done: step4, doneText: '已就绪', pendingText: '待完成' },
  ];
}

function renderQuickstartGuide(data) {
  const root = dom.quickstartGuide;
  if (!root) return;
  if (isQuickstartDismissed()) {
    setHidden(root, true);
    return;
  }
  setHidden(root, false);
  const steps = evaluateQuickstartSteps(data);
  const doneCount = steps.filter((step) => step.done).length;
  setText(dom.quickstartProgress, doneCount + ' / ' + steps.length);
  for (const step of steps) {
    const item = $('quickstart-step-' + step.id);
    if (item) item.dataset.done = String(step.done);
    setDot($('quickstart-step-' + step.id + '-dot'), step.done ? 'ok' : 'neutral');
    setText($('quickstart-step-' + step.id + '-tag'), step.done ? step.doneText : step.pendingText);
  }
  const autoCollapse = steps[0].done && steps[1].done && steps[3].done;
  const collapsed = state.guideCollapsed === null ? autoCollapse : Boolean(state.guideCollapsed);
  root.dataset.collapsed = String(collapsed);
  if (dom.quickstartToggle) {
    dom.quickstartToggle.setAttribute('aria-expanded', String(!collapsed));
    setText(dom.quickstartToggle, collapsed ? '展开引导' : '收起');
  }
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

/* operationLog 是可选诊断：缺失或不可用时，导出仍然可用，只说明没有数量。 */
function diagnosticsOperationLogNote(data) {
  const log = data?.diagnostics?.operationLog;
  if (!log || typeof log !== 'object') return '导出仍可用；当前状态未提供运行记录数量。';
  if (log.available === false) {
    const reason = typeof log.reason === 'string' && log.reason ? '（' + reasonText(log.reason) + '）' : '';
    return '运行记录当前不可用' + reason + '；导出仍会生成版本与配置信息。';
  }
  const parts = [];
  if (Number.isFinite(log.retained)) parts.push('已留存 ' + fmtCount(log.retained) + ' 条运行记录');
  if (Number.isFinite(log.limit) && log.limit > 0) parts.push('上限 ' + fmtCount(log.limit) + ' 条');
  if (typeof log.lastAt === 'string' && log.lastAt) parts.push('最近记录 ' + fmtTime(log.lastAt));
  else if (typeof log.lastAt === 'number' && log.lastAt > 0) parts.push('最近记录 ' + fmtTime(log.lastAt));
  return parts.length ? parts.join(' · ') : '导出仍可用；当前状态未提供运行记录数量。';
}

function renderDiagnosticsExport(data) {
  const button = $('diagnostics-export');
  const note = $('diagnostics-export-note');
  const errors = $('diagnostics-export-errors');
  if (!button) return;
  const baseNote = diagnosticsOperationLogNote(data);
  const pathNote = state.diagnosticsExportPath ? ' · 已导出至 ' + state.diagnosticsExportPath : '';
  setText(note, baseNote + pathNote);
  setText(errors, state.diagnosticsExportError || '');
  setHidden(errors, !state.diagnosticsExportError);
  button.disabled = state.diagnosticsExporting === true;
  setText(button, state.diagnosticsExporting ? '正在导出…' : '导出诊断日志');
}

async function exportDiagnostics() {
  if (state.diagnosticsExporting) return false;
  state.diagnosticsExporting = true;
  state.diagnosticsExportError = '';
  renderDiagnosticsExport(state.data);
  try {
    const res = await request(API.diagnosticsExport, { method: 'POST', body: {} });
    const savedPath = typeof res?.path === 'string' ? res.path.trim() : '';
    if (!res || res.ok !== true || !savedPath) {
      throw new Error('服务未返回导出文件路径，请稍后重试。');
    }
    state.diagnosticsExportPath = savedPath;
    showToast('脱敏诊断日志已保存至：' + savedPath + '（不含 API Key、令牌或对话正文）。', 'ok');
    return true;
  } catch (error) {
    state.diagnosticsExportError = '导出失败：' + (error?.message || '请稍后重试。');
    showToast(state.diagnosticsExportError, 'error');
    return false;
  } finally {
    state.diagnosticsExporting = false;
    renderDiagnosticsExport(state.data);
  }
}

function renderOverviewToday() {
  const today = state.overviewToday;
  if (!today || today.requests === null) {
    setText(dom.overviewTodayRequests, today?.loading ? '读取中…' : '—');
    setText(dom.overviewTodayTokens, today?.loading ? '今日 Token：读取中…' : '今日 Token：未提供');
    return;
  }
  setText(dom.overviewTodayRequests, fmtCount(today.requests) + ' 次请求');
  const tokensText = typeof today.totalTokens === 'number' && Number.isFinite(today.totalTokens) && today.totalTokens >= 0
    ? (today.totalTokens === 0 ? '0' : fmtTokens(today.totalTokens))
    : '未提供';
  setText(dom.overviewTodayTokens, '今日 Token：' + tokensText);
  if (state.data) renderQuickstartGuide(state.data);
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
  const directMode = savedIntegrationMode(data) === 'direct';
  const directVerdict = directMode ? directModeState(data) : null;
  /* 直连配置已读回且 hosts 就绪时，旧来源待清理不会让新直连失效。
     此时应展示收尾提示，不能误报成「模型请求当前无法发出」。 */
  const directReady = directMode && directImportComplete(data) && directStaticHostsReady(data);
  /* 官方版本不支持直连时，重起网关解决不了这个限制：先说清版本前提。 */
  const directUnsupported = directVerdict?.code === 'unsupported';
  if (gw.state !== 'running' && !directReady && !directUnsupported) {
    return {
      tone: 'bad',
      title: '本机网关未运行',
      note: '模型请求当前无法发出，可在设置页重启后台服务。',
      actionLabel: '前往设置',
      action: { type: 'section', view: 'maintenance', section: 'update-panel' },
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
  /* 凭据待授权/被拒绝/钥匙串锁定在措辞上各不相同，但下一步都是同一条：
     回模型页重授权。用 credential 标记判断，避免每次改文案都漏掉这里。 */
  if (apply.credential === true) {
    return {
      tone: apply.tone || 'warn',
      title: apply.word,
      note: apply.note,
      actionLabel: '前往模型页重授权',
      action: { type: 'section', view: 'models', section: 'restart-panel' },
    };
  }
  if (['应用失败', '应用已超时', '等待应用', '自动应用不可用'].includes(apply.word)) {
    return {
      tone: apply.tone || 'warn',
      title: apply.word,
      note: apply.note,
      actionLabel: '前往接入排障',
      action: { type: 'section', view: 'maintenance', section: 'native-troubleshoot' },
    };
  }
  /* 直连模式下预算不参与请求：这里报「已达到上限」会让人误以为请求被拒。 */
  const budgetMessage = savedIntegrationMode(data) === 'direct' ? '' : budgetNotice(data && data.budget);
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
  if (verdict.code === 'checking') {
    return {
      tone: 'neutral',
      title: verdict.word,
      note: verdict.note,
      actionLabel: '',
      action: null,
    };
  }
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
  /* 直连成功用的是 ready，网关成功用的是 synced：两者都是「没有待处理
     接入问题」，不能因为码名不同就把正常的直连当成问题。 */
  if (verdict.code !== 'synced' && verdict.code !== 'ready') {
    return {
      tone: verdict.tone || 'warn',
      title: verdict.word,
      note: verdict.note,
      actionLabel: '接入与恢复',
      action: { type: 'section', view: 'maintenance', section: 'native-panel' },
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
    return '检测到另一个未受托管的官方 Kimi 正在运行。请点击「正常退出并重启 Kimi」，由产品正常退出并完成受管重启后再试。';
  }
  if (op?.state === 'waiting-login') {
    return '已打开官方 Kimi，但尚未确认登录账号。请先在官方窗口完成登录，再重新操作。';
  }
  if (op?.state === 'waiting-work') {
    return '官方 Kimi 已运行，但 Work 页面尚未就绪。请确认 Kimi 已登录并进入 Work 视图后重试。';
  }
  if (op?.state === 'requires-restart') {
    return '模型配置已写入，请等当前任务结束后点击「正常退出并重启 Kimi」使配置生效。';
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
  if (op.action === 'auto-sync') {
    setHidden(dom.opbar, false);
    setDot(dom.opbarDot, 'warn');
    setText(dom.opbarName, running ? '正在自动同步模型目录…' : '自动同步模型目录 · ' + word);
    setText(dom.opbarNote, running
      ? '保存的更改由后台自动写入 Kimi Work，不需要手动点同步；需要重启时会在这里提示。'
      : operationNote(op) || '');
    dom.opbar.dataset.running = running ? 'true' : 'false';
    return;
  }
  const bad = op.state === 'failed' || op.state === 'interrupted' || op.state === 'unavailable' || op.state === 'incompatible';
  const warn = running || op.state === 'waiting-kimi-exit' || op.state === 'waiting-login'
    || op.state === 'waiting-work' || op.state === 'waiting-settings' || op.state === 'requires-restart'
    || op.state === 'waiting-idle' || op.state === 'waiting-quit';
  setHidden(dom.opbar, false);
  setDot(dom.opbarDot, warn ? 'warn' : bad ? 'bad' : 'ok');
  setText(dom.opbarName, label + ' · ' + word);
  setText(dom.opbarNote, operationNote(op) || '');
  dom.opbar.dataset.running = running ? 'true' : 'false';
}

function renderActions(data) {
  if (isInFlight(data && data.operation)) clearDisconnectConfirm();
  if (data && !onboardBusy()) {
    if (state.view === 'models') renderOnboard();
    else renderOnboardOverview(data);
  }
  if (!state.actionButtons.length) return;
  const running = isInFlight(data && data.operation);
  /* 保存模式期间也锁定同步按钮：否则 renderActions 会把 renderIntegrationMode
     刚设好的禁用态改回可用，用户能在半保存状态下点同步。 */
  const busy = running || state.modelsSaving || state.connsSaving || integrationModeSaving;
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
    /* 直连重启未完成时，同步与高级恢复都不能再触发一次导入。
       只限直连：网关的回执可能来自更早的一次操作，锁住按钮会挡住重试。 */
    const restartBlocked = savedIntegrationMode(data) === 'direct' && directRestartRequired(data)
      && (action === 'connect-kimi' || action === 'prepare-native');
    button.disabled = busy || compatBlocked || restartBlocked || (needsReady && (!ready || (reuseLogin && alreadyRunning)));
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
  if (modelTakeoverRemoved(data)) {
    setText($('workspace-sync-state'), '已移除接管，可重新同步');
    setText($('workspace-sync-note'), '官方 Kimi 中由本产品接管的模型来源已移除；本地服务商和模型仍保留，点击「同步到 Work」即可重新接管。');
    setHidden($('workspace-restart'), true);
    return;
  }
  const mode = savedIntegrationMode(data);
  const effective = effectiveIntegrationMode(data);
  const cleanupPending = directRouteCleanupPending(data);
  /* 模式改了但官方 Kimi 还停在另一种走法：先说「需要再同步一次」，
     不能因为旧别名还在就显示成已同步。 */
  const modeChanged = models.length > 0 && mode !== effective;
  // Direct mode can legitimately leave models behind (no tools, no key, ...),
  // so completeness is measured against the eligible set, never the total.
  if (mode === 'direct') {
    /* directImportComplete 已经要求官方 Kimi 处于 direct，所以模式改了这里必然是 false。 */
    const synced = directImportComplete(data);
    const restart = directRestartRequired(data);
    const hosts = directStaticHostsReady(data);
    const cleanup = directRouteCleanupPending(data);
    setText($('workspace-sync-state'), restart ? '需要重启 Kimi' : cleanup ? '切换收尾未完成' : synced ? '已直连同步到 Work' : '待写入直连配置');
    setText($('workspace-sync-note'), restart
      ? '直连配置已写入，正常重启 Kimi 后完成应用；在此之前请保留后台服务。'
      : cleanup
        ? '新的直连配置已写入并读回，旧的服务商来源还保留着；点「同步到 Work」可重试清理。在此之前不要退出后台服务，也不要当作旧路由已清理。'
        : synced
          ? (hosts
            ? '已通过直连模式写入官方 Kimi 且静态 hosts 就绪；退出 KimiWork + 与后台服务后仍可直接使用这些模型。'
            : '直连模型已写入官方 Kimi；点「同步到 Work」补齐静态 hosts 后即可彻底退出后台服务。')
          : '已选择零网关直连模式。点击「同步到 Work」将服务商地址与静态 hosts 写入官方 Kimi，之后可退出本软件。');
    setHidden($('workspace-restart'), !restart);
    return;
  }
  /* 回执要求重启时，重启提示必须先于模式/清理提示；否则用户会去重复同步。 */
  if (nativeRestartReceipt(data)) {
    setText($('workspace-sync-state'), '需要重启 Kimi');
    setText($('workspace-sync-note'), '模型配置已写入。正常重启 Kimi 后，在选择器中确认。');
    setHidden($('workspace-restart'), false);
    return;
  }
  /* Direct → Gateway 也共用收尾判断：旧来源没清理就不能显示绿色已同步，
     但同步入口要保留，让用户能用同一次同步重试清理。这条也要排在
     「待切回网关」之前：官方还停在直连时，缺的正是这次清理。 */
  if (cleanupPending) {
    setText($('workspace-sync-state'), '切换收尾未完成');
    setText($('workspace-sync-note'), '已切回本机网关模式，但旧的服务商来源还保留着；点「同步到 Work」可重试清理旧来源并同步。在清理完成前不要退出后台服务，也不要当作旧路由已清理。');
    setHidden($('workspace-restart'), true);
    return;
  }
  if (modeChanged) {
    setText($('workspace-sync-state'), '待切回网关并同步');
    setText($('workspace-sync-note'), '已切回本机网关模式，后台会自动将官方 Kimi 恢复为本机网关路由。');
    setHidden($('workspace-restart'), true);
    return;
  }
  const imported = Object.keys(integration.aliases || {}).sort();
  const saved = models.map(model => model.id).sort();
  const matches = JSON.stringify(imported) === JSON.stringify(saved);
  const unmanaged = integration.desktop?.managed === false;
  const restart = matches && integration.applied && !integration.adopted && !unmanaged;
  const synced = matches && (integration.adopted || (integration.applied && unmanaged));
  let syncedNote = '已读回当前模型配置，可在 Work 的模型选择器中使用。';
  if (unmanaged) {
    syncedNote = data.kimiProcess?.running === true
      ? '已读回当前模型配置。当前 Kimi 非托管运行，请在官方窗口确认模型。'
      : '已读回当前模型配置。打开 Kimi 后可在模型选择器中确认。';
  }
  setText($('workspace-sync-state'), !models.length ? '尚未添加模型' : synced ? '已同步到 Work' : restart ? '需要重启 Kimi' : '待同步到 Work');
  setText($('workspace-sync-note'), !models.length ? '添加服务商并选择模型，保存后会自动同步。' : synced ? syncedNote
    : restart ? '模型配置已写入。正常重启 Kimi 后，在选择器中确认。' : '模型已保存；后台会自动同步到官方模型选择器，需要刷新时顶部会提示重启 Kimi。');
  setHidden($('workspace-restart'), !restart);
}


/* ---------- 设置页：接入模式选择器 ---------- */

let integrationModeSaving = false;

function integrationModeBusy(data) {
  return integrationModeSaving || isInFlight(data?.operation);
}

function renderIntegrationMode(data) {
  const saved = savedIntegrationMode(data);
  const effective = effectiveIntegrationMode(data);
  const busy = integrationModeBusy(data);
  for (const input of [dom.modeGateway, dom.modeDirect]) {
    if (!input) continue;
    /* 保存中不回写选中态：否则用户刚点的选项会在轮询里弹回旧值。 */
    if (!integrationModeSaving) input.checked = input.value === saved;
    input.disabled = busy;
  }
  if (dom.integrationModeGroup) dom.integrationModeGroup.dataset.saving = integrationModeSaving ? 'true' : 'false';

  const direct = directModeOf(data);
  const directSaved = saved === 'direct';
  const directChecking = data?.compatibility?.status === 'checking' && direct?.supported !== true;
  const directUnsupported = !directChecking && direct?.supported === false;
  const cleanupPending = directRouteCleanupPending(data);
  /* 回执要求重启时优先于清理提示；从直连切回网关后同样成立。 */
  const restartRequired = directSaved ? directRestartRequired(data) : nativeRestartReceipt(data);
  /* 未同步时把话说在按钮上：用户点的是同一次同步动作，但结果不同。 */
  const pendingSync = directSaved !== (effective === 'direct') || (directSaved && !directImportComplete(data))
    || cleanupPending;
  if (dom.integrationModeState) {
    /* 网关模式没有直连那张说明卡，重启回执与收尾标记要在这里明说。 */
    const showState = pendingSync || (directSaved && directUnsupported) || (!directSaved && restartRequired);
    setHidden(dom.integrationModeState, !showState);
    dom.integrationModeState.classList.toggle('note--warn', showState);
    setText(dom.integrationModeState, !showState ? ''
      : directUnsupported && directSaved
        ? '模式已更改，尚未同步到 Kimi Work：当前官方 Kimi 版本不支持原生直连，请改用本机网关模式。'
        : restartRequired
          ? directSaved
            ? '直连配置已写入，需要正常重启 Kimi 后完成应用。'
            : '模型已写入原生目录，需要正常重启 Kimi 后完成应用。'
          : cleanupPending
            ? directSaved
              ? '新的直连配置已写入并读回，但旧来源尚未清理，切换收尾未完成。'
              : '已切回本机网关模式，但旧来源尚未清理，切换收尾未完成；请重试清理旧来源并同步。'
            : '模式已更改，尚未同步到 Kimi Work。');
  }
  if (dom.nativeSyncLabel) {
    setText(dom.nativeSyncLabel, restartRequired
      ? '等待重启 Kimi'
      : cleanupPending
        ? '重试清理旧来源并同步'
        : directSaved
          ? '写入直连配置并同步'
          : pendingSync ? '切回网关并同步' : '重新准备接入');
  }
  if (dom.nativeSync) {
    const blocked = data?.compatibility?.guards?.modelSyncWriteBlocked === true;
    /* 只有直连等待重启时才锁住同步：网关模式的回执可能来自更早的操作，
       锁住会挡住重试清理的入口。 */
    dom.nativeSync.disabled = busy || blocked || (directSaved && restartRequired);
    dom.nativeSync.classList.toggle('is-busy', integrationModeSaving);
    dom.nativeSync.setAttribute('aria-busy', integrationModeSaving ? 'true' : 'false');
  }

  const notice = dom.directModeNotice;
  if (!notice) return;
  setHidden(notice, !directSaved);
  if (!directSaved) return;
  notice.dataset.tone = directUnsupported || restartRequired || directRouteCleanupPending(data) ? 'warn' : 'neutral';
  setText(dom.directModeSupport, directChecking
    ? '正在检查官方 Kimi 兼容性，稍候自动刷新。'
    : directUnsupported
      ? '当前官方 Kimi 版本不支持原生直连：需要 3.2.28+ 的 byokSources v3 存储。请更新官方 Kimi，或改用本机网关模式。'
      : restartRequired
        ? '直连配置已写入，正常重启 Kimi 后完成应用。'
        : directRouteCleanupPending(data)
          ? '新的直连配置已写入并读回，但旧的服务商来源还没清理，切换收尾未完成。'
          : directImportComplete(data)
            ? '当前官方 Kimi 版本支持原生直连，直连模型已写入。'
            : '当前官方 Kimi 版本支持原生直连；点「写入直连配置并同步」完成写入。');
  setText(dom.directModeHosts, restartRequired
    ? '请先正常退出并重启 Kimi；重启完成前无需重复同步，也请保留后台服务。'
    : directRouteCleanupPending(data)
      ? '重新点「同步到 Work」可重试清理旧来源；在清理完成前不要退出后台服务，也不要当作旧路由已清理。'
      : directStaticHostsReady(data)
        ? '静态 hosts 已就绪，直连同步完成后可彻底停止后台服务与退出 KimiWork +。'
        : '首次同步直连模式时将写入一次静态 hosts（并清理旧单域名 DNS 配置），之后不再依赖后台 DNS。');

  const bypassed = Array.isArray(direct?.activeFeaturesBypassed) ? direct.activeFeaturesBypassed : [];
  const bypassNames = bypassed.map((key) => DIRECT_BYPASS_WORD[key]).filter(Boolean);
  if (dom.directModeBypassed) {
    setHidden(dom.directModeBypassed, bypassNames.length === 0);
    dom.directModeBypassed.classList.add('direct-notice__line--list');
    setText(dom.directModeBypassed, bypassNames.length
      ? '以下已开启的功能在直连模式下不生效：' + bypassNames.join('、') + '。'
      : '');
  }

  const skipped = Array.isArray(direct?.skippedModels) ? direct.skippedModels : [];
  if (dom.directModeSkipped) {
    setHidden(dom.directModeSkipped, skipped.length === 0);
    dom.directModeSkipped.classList.add('direct-notice__line--list');
    const details = skipped.slice(0, 4).map((row) => {
      const id = typeof row?.id === 'string' && row.id ? row.id : '';
      const reason = DIRECT_SKIP_REASON[row?.reason] || row?.reason || '原因未知';
      return id ? id + '（' + reason + '）' : reason;
    });
    const more = skipped.length > details.length ? ' 等 ' + skipped.length + ' 个' : '';
    setText(dom.directModeSkipped, skipped.length
      ? '有 ' + skipped.length + ' 个模型未能写入直连：' + details.join('、') + more + '。'
      : '');
  }
}

/* 切换模式只是保存偏好；真正写入官方 Kimi 仍是「同步到 Work」那一步。 */
async function chooseIntegrationMode(mode, { onSaved } = {}) {
  if (!INTEGRATION_MODES.includes(mode)) return false;
  const data = state.data;
  if (!data || integrationModeBusy(data)) return false;
  if (savedIntegrationMode(data) === mode) return true;
  clearDisconnectConfirm();
  integrationModeSaving = true;
  renderIntegrationMode(data);
  renderActions(data);
  try {
    const res = await request(API.integrationMode, { method: 'POST', body: { revision: data.revision, mode } });
    if (res && res.state) {
      onSaved?.(res.state);
      showToast(mode === 'direct'
        ? '已切换为直连模式，后台会自动写入服务商地址与静态 hosts；需要刷新时顶部会提示重启 Kimi。'
        : '已切换为本机网关模式，后台会自动同步模型菜单；需要刷新时顶部会提示重启 Kimi。', 'ok');
    }
    return true;
  } catch (error) {
    await handleSaveFailure(error, 'integration-mode');
    return false;
  } finally {
    integrationModeSaving = false;
    if (state.data) {
      renderIntegrationMode(state.data);
      renderActions(state.data);
    }
  }
}

function resetIntegrationModeSignature() {
  integrationModeSaving = false;
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

function normalizeUpdateSourceKind(value) {
  const raw = typeof value === 'string' ? value.trim().toLowerCase() : '';
  if (!raw) return '';
  if (raw === 'cnb' || raw.startsWith('cnb:')) return 'cnb';
  if (raw === 'github' || raw.startsWith('github:')) return 'github';
  if (raw === 'endpoint' || raw.startsWith('http://') || raw.startsWith('https://')) return 'endpoint';
  return '';
}

function updateSourceLabel(updates) {
  const preferred = normalizeUpdateSourceKind(updates?.preferredSource)
    || normalizeUpdateSourceKind(updates?.source);
  const active = normalizeUpdateSourceKind(updates?.activeSource)
    || normalizeUpdateSourceKind(updates?.available?.source);
  if (active === 'cnb') return '国内镜像（CNB）';
  if (active === 'github') return preferred === 'github' ? 'GitHub' : 'GitHub（备用源）';
  if (active === 'endpoint') return '自定义更新源';
  if (preferred === 'cnb') return '优先国内镜像（CNB），失败自动回退 GitHub';
  if (preferred === 'github') return 'GitHub';
  if (preferred === 'endpoint') return '自定义更新源';
  return '';
}

function safeHttpUrl(value) {
  if (typeof value !== 'string') return '';
  const trimmed = value.trim();
  if (!/^https?:\/\//i.test(trimmed)) return '';
  try {
    const parsed = new URL(trimmed);
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : '';
  } catch {
    return '';
  }
}

function updateStateSummary(updates) {
  const st = updates?.state || 'idle';
  switch (st) {
    case 'checking': return { tone: 'warn', word: '正在检查更新…' };
    case 'up_to_date': return { tone: 'ok', word: '已是最新版本' };
    case 'available': return { tone: 'warn', word: '发现新版本 ' + (updates.available?.version || '') };
    case 'downloading': return { tone: 'warn', word: '正在下载更新' };
    case 'downloaded': return { tone: 'warn', word: '已下载，等待安装' };
    case 'verifying': return { tone: 'warn', word: '正在校验签名' };
    case 'waiting_idle': return { tone: 'warn', word: '等待空闲后替换' };
    case 'applying': return { tone: 'warn', word: '正在替换并重启' };
    case 'rolling_back': return { tone: 'warn', word: '正在回退到上一版本' };
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

/* 升级器只看 /api/health 的 PID/版本，所以「进程已换新」会先于「凭据可读」
   成立。自签名升级后 securityd 按 cdhash 拒绝候选访问，网关仍在运行却读不到
   已保存的 Key：这时报「已完成」会让人以为一切正常。只有连接凭据确实可读，
   才算升级完全就绪；否则如实说明只完成了替换。 */
function updateCompletionVerdict(data, actionLabel) {
  const version = data?.updates?.currentVersion || '';
  const credentialPending = credentialAuthPending(data);
  if (!credentialPending) {
    return { tone: 'ok', complete: true,
      text: actionLabel + '已完成，当前版本 ' + version + '。' };
  }
  const isWindows = typeof document !== 'undefined' && document.documentElement?.dataset?.platform === 'windows';
  return { tone: 'warn', complete: false,
    text: isWindows
      ? '当前版本 ' + version + '。部分旧版连接凭据需要系统授权迁移，请到「模型」页点「重新授权并应用连接」。'
      : '当前版本 ' + version + '。部分旧版连接凭据仍待迁移授权，请到「模型」页点「重新授权并应用连接」（若 macOS 弹出钥匙串提示可选择「始终允许」一次完成迁移）。' };
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
              const verdict = updateCompletionVerdict(state.data, actionLabel);
              showToast(verdict.text, verdict.tone);
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
  const sourceEl = $('upd-source');
  if (sourceEl) {
    const sourceLabel = updateSourceLabel(updates);
    sourceEl.hidden = !sourceLabel;
    setText(sourceEl, sourceLabel ? '更新源 · ' + sourceLabel : '');
  }

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

  /* 新版本提醒同时出现在三处：顶栏「设置」圆点、总览精简横条与设置页更新卡片。
     available 时可直接下载；downloaded 时直接安装并重启。 */
  const notice = Boolean(available && available.version) && (st === 'available' || st === 'downloaded');
  const navDot = $('nav-update-dot');
  if (navDot) navDot.hidden = !notice;
  const banner = $('update-banner');
  const bannerText = $('update-banner-text');
  if (banner && bannerText) {
    banner.hidden = !notice;
    if (notice) {
      const versionText = String(available.version).startsWith('v') ? available.version : 'v' + available.version;
      setText(bannerText, `发现新版本 ${versionText}。下载验签后会在空闲时替换并重启本软件，不影响官方 Kimi。`);
      const primary = $('update-banner-primary');
      if (primary) {
        const action = st === 'downloaded' ? 'apply' : 'download';
        primary.dataset.updateAction = action;
        primary.disabled = false;
        setText(primary, st === 'downloaded' ? '立即安装并重启' : '立即下载并更新');
      }
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
      setText($('overview-update-title'), `发现 KimiWork + 新版本 ${available.version}`);
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
        setText(progressLabel, '正在替换安装包并重启 KimiWork +，网关恢复后页面会自动重连…');
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
      /* 升级记录只证明进程换成了新版本；凭据还没获得读取许可时必须说清楚，
         否则这条「已于…更新到…」会显得一切就绪。 */
      const pending = credentialAuthPending(data);
      historyNote.classList.toggle('note--warn', pending);
      setText(historyNote, pending
        ? `已更新到 ${applied.toVersion}（${fmtTime(applied.at)}），但已保存的连接 Key 仍需系统授权：请到「模型」页点「重新授权并应用连接」。`
        : `最近一次已于 ${fmtTime(applied.at)} 从 ${applied.fromVersion || '旧版本'} 更新到 ${applied.toVersion}。`);
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
  const releaseLink = $('upd-release-link');
  if (releaseLink) {
    const releaseUrl = safeHttpUrl(available?.releaseUrl) || safeHttpUrl(updates.releaseUrl);
    releaseLink.hidden = !releaseUrl;
    if (releaseUrl) {
      releaseLink.setAttribute('href', releaseUrl);
    } else {
      releaseLink.removeAttribute('href');
    }
  }

  scheduleUpdateFastPoll(['downloading', 'verifying', 'waiting_idle'].includes(st));
  if (st === 'applying') {
    void waitForUpdateReconnect(updates.buildId || data.service?.buildId || '', '版本替换');
  }
}

const UPDATE_ENDPOINTS = {
  check: '/api/updates/check',
  download: '/api/updates/download',
  apply: '/api/updates/apply',
  cancel: '/api/updates/cancel',
  rollback: '/api/updates/rollback',
  settings: '/api/updates/settings',
};

function updateEndpoint(action) {
  if (typeof action !== 'string' || !action) return '';
  return action.startsWith('/') ? action : (UPDATE_ENDPOINTS[action] || '/api/updates/' + action);
}

async function updateAction(action, body = {}, okMessage = '') {
  const endpoint = updateEndpoint(action);
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
  } else if (endpoint === '/api/updates/apply') {
    showToast('已请求安装已下载的更新；本软件会在空闲时替换并重启。', 'ok');
    void waitForUpdateReconnect(prevBuild, '版本替换');
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
  if (register) {
    register.textContent = data.service?.registered ? '更新服务注册' : '注册后台服务';
    register.disabled = Boolean(busy);
  }
  const enable = $('service-enable');
  if (enable) enable.disabled = Boolean(busy) || data.service?.loginStartup === 'enabled';
  const disable = $('service-disable');
  if (disable) disable.disabled = Boolean(busy) || data.service?.loginStartup !== 'enabled';
  for (const id of ['daemon-restart', 'service-stop', 'service-uninstall']) {
    const el = $(id);
    if (el) el.disabled = Boolean(busy);
  }
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
        ? `检测到预览版遗留的 ${pending} 项凭据尚未完成迁移，需要系统授权读取旧版本凭据条目。原预览条目与旧目录均已保留。`
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
      setText(note, '已从预览版完成配置与凭据迁移。原预览目录和旧版本凭据条目均已保留，确认无误后再自行清理。');
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
  chooseIntegrationMode,
  exportDiagnostics,
  directImportComplete,
  directModeState,
  directRestartRequired,
  directRouteCleanupPending,
  directStaticHostsReady,
  dismissQuickstartGuide,
  effectiveIntegrationMode,
  isInFlight,
  previewServiceAction,
  refreshOverviewToday,
  renderActions,
  renderIntegrationMode,
  renderOperation,
  renderOverview,
  renderPreviewLifecycle,
  renderShell,
  renderUpdates,
  renderWorkspaceSync,
  reopenQuickstartGuide,
  resetIntegrationModeSignature,
  resetRecentSignature,
  runOverviewIssueAction,
  restartBannerState,
  renderOverviewStatus,
  savedIntegrationMode,
  syncNeedsImport,
  syncState,
  overviewState,
  autoSyncOf,
  toggleQuickstartGuide,
  updateAction,
};

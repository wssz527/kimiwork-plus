package main

import (
	"context"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

const (
	trayFetchTimeout         = 3 * time.Second
	trayAccountActionTimeout = 30 * time.Second
	trayOpenWorkTimeout      = 30 * time.Second
	trayRebindTimeout        = 35 * time.Second
	trayHTTPTimeout          = 40 * time.Second
	trayResponseLimit        = 2 << 20
	trayRecentConversations  = 3
	trayConversationTitleLen = 28
	trayUpdatePlaceholder    = "发现新版本 · 前往更新…"
	// trayUnlistedGatewayModel names a conversation bound to a gateway model the
	// active catalog no longer lists, instead of showing its digest id.
	trayUnlistedGatewayModel = "网关模型（目录未列出）"
)

var trayBYOKPrefixes = []string{"byok-kth-kimi/", "byok-kth-resp/"}

var trayBYOKUUIDPrefix = regexp.MustCompile(`^byok-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/`)

type trayUpdateRelease struct {
	Version string `json:"version"`
}

type trayUpdateState struct {
	State     string             `json:"state"`
	Available *trayUpdateRelease `json:"available"`
}

func normalizeTrayVersion(raw string) string {
	version := strings.TrimSpace(raw)
	for strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		version = strings.TrimSpace(version[1:])
	}
	if version == "" {
		return ""
	}
	return "v" + version
}

func trayUpdateLabel(state trayUpdateState) string {
	if state.State != "available" && state.State != "downloaded" {
		return ""
	}
	if state.Available == nil {
		return trayUpdatePlaceholder
	}
	version := normalizeTrayVersion(state.Available.Version)
	if version == "" {
		return trayUpdatePlaceholder
	}
	return "发现新版本 " + version + " · 前往更新…"
}

func trayConversationTitle(conversation kimi.WorkConversation) string {
	text := strings.TrimSpace(conversation.Title)
	if text == "" {
		text = strings.TrimSpace(conversation.FirstUserText)
	}
	if text == "" {
		return "未命名会话"
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > trayConversationTitleLen {
		return string(runes[:trayConversationTitleLen]) + "…"
	}
	return text
}

func trayReadableModelName(alias string, models []kimi.ConversationModel) string {
	alias = strings.TrimSpace(alias)
	if name := kimi.OfficialConversationModelName(alias); name != "" {
		return name
	}
	for _, model := range models {
		matches := model.Alias == alias || (model.PreviousAlias != "" && model.PreviousAlias == alias)
		if !matches {
			continue
		}
		if name := strings.TrimSpace(model.DisplayName); name != "" {
			return name
		}
	}
	name := strings.TrimSpace(trayBYOKModelID(alias))
	if protocol.IsKimiCompatibleModelID(name) || (name == "" && protocol.IsKimiCompatibleModelID(alias)) {
		return trayUnlistedGatewayModel
	}
	if name != "" {
		return name
	}
	return "默认模型"
}

// trayBYOKModelID returns the model id a BYOK alias addresses, without the
// provider prefix the official menu adds.
func trayBYOKModelID(alias string) string {
	for _, prefix := range trayBYOKPrefixes {
		if strings.HasPrefix(alias, prefix) {
			return strings.TrimPrefix(alias, prefix)
		}
	}
	if match := trayBYOKUUIDPrefix.FindString(alias); match != "" {
		return alias[len(match):]
	}
	return ""
}

func trayModelDisplayName(model kimi.ConversationModel, models []kimi.ConversationModel) string {
	if name := strings.TrimSpace(model.DisplayName); name != "" {
		return name
	}
	return trayReadableModelName(model.Alias, models)
}

func trayMatchEffort(level string, efforts []string) (string, bool) {
	level = strings.TrimSpace(level)
	if level == "" {
		return "", false
	}
	for _, effort := range efforts {
		effort = strings.TrimSpace(effort)
		if strings.EqualFold(effort, level) {
			return effort, true
		}
	}
	return "", false
}

func trayTargetThinkingLevel(model kimi.ConversationModel) string {
	if model.SourceKind == "official" {
		return ""
	}
	if level, ok := trayMatchEffort(model.DefaultEffort, model.SupportEfforts); ok {
		return level
	}
	for _, effort := range model.SupportEfforts {
		if effort = strings.TrimSpace(effort); effort != "" && !strings.EqualFold(effort, "off") {
			return effort
		}
	}
	if len(model.SupportEfforts) > 0 {
		return strings.TrimSpace(model.SupportEfforts[0])
	}
	return ""
}

func trayRebindFailureMessage(err error) string {
	reason := "换模请求未能完成。"
	if err != nil {
		switch err.Error() {
		case "account_service_unavailable":
			reason = "本机后台暂不可用。"
		case "account_action_rejected":
			reason = "本机后台拒绝了这次换模请求。"
		case "operation_busy":
			reason = "本机后台正在处理另一项操作，请稍后再试。"
		case "conversation_kimi_state_unknown":
			reason = "无法确认 Kimi 运行状态，请稍后再试。"
		case "conversation_kimi_restart_required":
			reason = "Kimi 正在运行，需要关闭并重开后才能换模。"
		case "conversation_kimi_stop_failed":
			reason = "未能关闭 Kimi，换模没有执行。"
		case "conversation_rollback_failed":
			reason = "换模未完成，数据回滚也需要人工确认。"
		case "invalid_conversation_thinking_level":
			reason = "目标模型不支持当前会话的思考档位。"
		case "invalid_conversation_target_model":
			reason = "目标模型已不在可切换列表中。"
		case "conversation_rebind_cancelled":
			reason = "换模操作已取消。"
		}
	}
	return reason + "请在面板的会话页面查看当前模型与 Kimi 运行状态后重试。"
}

func trayRebindNotice(result kimi.ConversationRebindResult, restart bool) string {
	restartFailed := restart && (!result.KimiRestarted || result.RestartReason != "")
	warning := "该模型使用 Anthropic 原生协议，历史对话可能无法完整保留，建议在新会话中继续。"
	switch {
	case restartFailed && result.DirectAnthropicHistoryWarning:
		return "会话模型已切换，但 Kimi Work 未能自动重开，请手动打开 Kimi Work。\n\n" + warning
	case restartFailed:
		return "会话模型已切换，但 Kimi Work 未能自动重开，请手动打开 Kimi Work。"
	case result.DirectAnthropicHistoryWarning:
		return "会话模型已切换。" + warning
	}
	return ""
}

func trayCancelButtonFirst(goos string) bool {
	return goos == "darwin"
}

type trayUpdateView struct {
	item *application.MenuItem
}

func (v *trayUpdateView) refresh(state trayUpdateState, err error) {
	label := ""
	if err == nil {
		label = trayUpdateLabel(state)
	}
	if label == "" {
		v.item.SetHidden(true)
		v.item.SetLabel(trayUpdatePlaceholder)
		return
	}
	v.item.SetLabel(label)
	v.item.SetEnabled(true)
	v.item.SetHidden(false)
}

type trayRebindTarget struct {
	key     string
	title   string
	alias   string
	model   string
	effort  string
	restart bool
}

type trayConversationsView struct {
	app             *application.App
	menu            *application.Menu
	client          accountTrayClient
	open            func()
	nav             *windowNavController
	rebind          atomic.Bool
	submenus        []*application.Menu
	retiredSubmenus []*application.Menu
}

func (v *trayConversationsView) refresh(list kimi.ConversationList, err error) {
	for _, submenu := range v.retiredSubmenus {
		submenu.Destroy()
	}
	v.retiredSubmenus = v.submenus
	v.submenus = nil
	v.menu.Clear()
	switch {
	case err != nil || list.Reason != "":
		v.menu.Add("会话列表暂不可读").SetEnabled(false)
	case list.Application == kimi.RunningStateUnknown:
		v.menu.Add("无法确认 Kimi 运行状态，请稍后重试").SetEnabled(false)
	case v.rebind.Load():
		v.menu.Add("正在切换会话模型…").SetEnabled(false)
	case len(list.Conversations) == 0:
		v.menu.Add("暂无历史会话").SetEnabled(false)
	default:
		for index, conversation := range list.Conversations {
			if index >= trayRecentConversations {
				break
			}
			v.addConversation(conversation, list.Models, list.KimiRunning)
		}
	}
	v.menu.AddSeparator()
	v.menu.Add("在面板查看全部会话…").OnClick(func(*application.Context) {
		if v.open != nil {
			v.open()
		}
		if v.nav != nil {
			application.InvokeAsync(func() { v.nav.RequestOpenConversations() })
		}
	})
}

func (v *trayConversationsView) addConversation(conversation kimi.WorkConversation, models []kimi.ConversationModel, kimiRunning bool) {
	title := trayConversationTitle(conversation)
	alias := conversation.ModelAlias
	if conversation.MigrationTargetAlias != "" {
		alias = conversation.MigrationTargetAlias
	}
	submenu := v.menu.AddSubmenu(title + "（" + trayReadableModelName(alias, models) + "）")
	v.submenus = append(v.submenus, submenu)
	if len(models) == 0 {
		submenu.Add("暂无可切换的模型").SetEnabled(false)
		return
	}
	for _, model := range models {
		name := trayModelDisplayName(model, models)
		current := conversation.ModelAlias == model.Alias
		item := submenu.AddCheckbox(name, current)
		if current {
			item.SetEnabled(false)
			continue
		}
		item.OnClick(func(*application.Context) {
			v.confirmRebind(conversation, model, name, kimiRunning)
		})
	}
}

func trayRebindTargetFor(conversation kimi.WorkConversation, model kimi.ConversationModel, modelName string, kimiRunning bool) trayRebindTarget {
	return trayRebindTarget{
		key:     conversation.ConversationKey,
		title:   trayConversationTitle(conversation),
		alias:   model.Alias,
		model:   modelName,
		effort:  trayTargetThinkingLevel(model),
		restart: kimiRunning,
	}
}

func (v *trayConversationsView) confirmRebind(conversation kimi.WorkConversation, model kimi.ConversationModel, modelName string, kimiRunning bool) {
	if v.rebind.Load() || conversation.ModelAlias == model.Alias {
		return
	}
	target := trayRebindTargetFor(conversation, model, modelName, kimiRunning)
	application.InvokeAsync(func() {
		if !target.restart {
			go v.runRebind(target)
			return
		}
		message := "切换会话《" + target.title + "》到 " + target.model + " 需要自动关闭并重开 Kimi Work（约 2–3 秒），请确认当前没有正在回复的任务。是否立即切换？"
		dialog := v.app.Dialog.Question().SetTitle("切换最近会话模型").SetMessage(message)
		confirm := func() { go v.runRebind(target) }
		if trayCancelButtonFirst(runtime.GOOS) {
			dialog.AddButton("取消").SetAsCancel()
			dialog.AddButton("立即切换").SetAsDefault().OnClick(confirm)
		} else {
			dialog.AddButton("立即切换").SetAsDefault().OnClick(confirm)
			dialog.AddButton("取消").SetAsCancel()
		}
		dialog.Show()
	})
}

func (v *trayConversationsView) runRebind(target trayRebindTarget) {
	if !v.rebind.CompareAndSwap(false, true) {
		return
	}
	defer v.rebind.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), trayRebindTimeout)
	defer cancel()
	request := kimi.ConversationRebindRequest{
		ConversationKeys: []string{target.key},
		TargetModelAlias: target.alias,
		ThinkingLevel:    target.effort,
		RestartKimi:      target.restart,
	}
	var result kimi.ConversationRebindResult
	if err := v.client.request(ctx, http.MethodPost, "/api/work/conversations/rebind", request, &result); err != nil {
		application.InvokeAsync(func() {
			v.app.Dialog.Error().SetTitle("会话换模未完成").SetMessage(trayRebindFailureMessage(err)).Show()
		})
		return
	}
	if message := trayRebindNotice(result, target.restart); message != "" {
		application.InvokeAsync(func() {
			v.app.Dialog.Info().SetTitle("会话模型已切换").SetMessage(message).Show()
		})
	}
}

type traySnapshot struct {
	accounts         trayAccounts
	accountsErr      error
	updates          trayUpdateState
	updatesErr       error
	conversations    kimi.ConversationList
	conversationsErr error
}

func fetchTraySnapshot(ctx context.Context, client accountTrayClient) traySnapshot {
	var snapshot traySnapshot
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		var response struct {
			Accounts trayAccounts `json:"accounts"`
		}
		if err := client.request(ctx, http.MethodGet, "/api/accounts", nil, &response); err != nil {
			snapshot.accountsErr = err
			return
		}
		snapshot.accounts = response.Accounts
	}()
	go func() {
		defer wg.Done()
		var response trayUpdateState
		if err := client.request(ctx, http.MethodGet, "/api/updates", nil, &response); err != nil {
			snapshot.updatesErr = err
			return
		}
		snapshot.updates = response
	}()
	go func() {
		defer wg.Done()
		var response kimi.ConversationList
		if err := client.request(ctx, http.MethodGet, "/api/work/conversations", nil, &response); err != nil {
			snapshot.conversationsErr = err
			return
		}
		snapshot.conversations = response
	}()
	wg.Wait()
	return snapshot
}

type trayController struct {
	tray          *application.SystemTray
	menu          *application.Menu
	client        accountTrayClient
	loading       atomic.Bool
	updates       *trayUpdateView
	accounts      *trayAccountsView
	conversations *trayConversationsView
}

func (t *trayController) open() {
	if !t.loading.CompareAndSwap(false, true) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), trayFetchTimeout)
		defer cancel()
		snapshot := fetchTraySnapshot(ctx, t.client)
		application.InvokeAsync(func() {
			defer t.loading.Store(false)
			t.updates.refresh(snapshot.updates, snapshot.updatesErr)
			t.accounts.refresh(snapshot.accounts, snapshot.accountsErr)
			t.conversations.refresh(snapshot.conversations, snapshot.conversationsErr)
			t.menu.Update()
			t.tray.OpenMenu()
		})
	}()
}

func (c accountTrayClient) openWork(ctx context.Context) error {
	return c.request(ctx, http.MethodPost, "/api/actions", map[string]any{"action": "open-work"}, nil)
}

func trayOpenWorkFailureMessage(err error) string {
	switch err.Error() {
	case "unmanaged_kimi_running", "native_bridge_waiting_work":
		return "请从官方 Kimi 窗口进入 Work。"
	case "not_installed", "kimi_not_installed":
		return "请先安装官方 Kimi 客户端。"
	case "operation_busy":
		return "上一次操作仍在进行，请稍后重试。"
	default:
		return "请检查 Kimi 是否已安装、后台服务是否运行，然后重试。"
	}
}

func installTray(app *application.App, tray *application.SystemTray, dir string, diagnostics *windowDiagnostics, openWindow func(), nav *windowNavController) {
	client := accountTrayClient{dir: dir, client: &http.Client{Timeout: trayHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	menu := app.NewMenu()
	updateItem := menu.Add(trayUpdatePlaceholder)
	updateItem.SetHidden(true)
	updateItem.OnClick(func(*application.Context) {
		openWindow()
		application.InvokeAsync(func() { nav.RequestOpenUpdates() })
	})
	menu.Add("打开 KimiWork +").OnClick(func(*application.Context) { openWindow() })
	menu.Add("打开 Kimi Work").OnClick(func(*application.Context) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), trayOpenWorkTimeout)
			defer cancel()
			if err := client.openWork(ctx); err != nil {
				application.InvokeAsync(func() {
					app.Dialog.Error().SetTitle("无法打开 Kimi Work").SetMessage(trayOpenWorkFailureMessage(err)).Show()
				})
			}
		}()
	})
	accountsMenu := menu.AddSubmenu("切换 Kimi 账号")
	accountsMenu.Add("正在读取账号…").SetEnabled(false)
	conversationsMenu := menu.AddSubmenu("切换最近会话模型")
	conversationsMenu.Add("正在读取最近会话…").SetEnabled(false)
	diagnosticsMenu := menu.AddSubmenu("诊断与恢复")
	diagnosticsMenu.Add("窗口恢复与诊断").OnClick(func(*application.Context) {
		message := windowDiagnosticMessage(runtime.GOOS, diagnostics.Status())
		app.Dialog.Info().SetTitle("KimiWork + 窗口诊断").SetMessage(message).Show()
	})
	diagnosticsMenu.Add("导出窗口诊断").OnClick(func(*application.Context) {
		path, err := diagnostics.Export(dir)
		if err != nil {
			app.Dialog.Error().SetTitle("无法导出窗口诊断").SetMessage("请检查应用状态目录是否可写，然后重试。").Show()
			return
		}
		app.Dialog.Info().SetTitle("窗口诊断已导出").SetMessage("诊断不含面板令牌、密钥或页面内容。文件位置：\n" + path).Show()
	})
	menu.AddSeparator()
	menu.Add("退出窗口（网关继续运行）").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)

	controller := &trayController{
		tray:   tray,
		menu:   menu,
		client: client,
		updates: &trayUpdateView{
			item: updateItem,
		},
		accounts: &trayAccountsView{
			app:    app,
			menu:   accountsMenu,
			client: client,
			show: func() {
				openWindow()
				application.InvokeAsync(func() { nav.RequestOpenAccounts() })
			},
		},
		conversations: &trayConversationsView{
			app:    app,
			menu:   conversationsMenu,
			client: client,
			open:   openWindow,
			nav:    nav,
		},
	}
	tray.OnClick(controller.open)
	tray.OnRightClick(controller.open)
}

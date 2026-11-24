package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wssz527/kimiwork-plus/internal/bridge"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func trayTestStateDir(t *testing.T, url, key string) string {
	t.Helper()
	dir := t.TempDir()
	if err := core.AtomicJSON(filepath.Join(dir, "desktop-endpoint.json"), bridge.Endpoint{URL: url, Key: key, BuildID: "test"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTrayConversationTitleCollapsesAndTruncates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		title       string
		firstUser   string
		want        string
		description string
	}{
		{"title", "  发布\n\n计划\t复核 ", "", "发布 计划 复核", "collapse whitespace"},
		{"fallback", "", "  从第一句开始\n继续  ", "从第一句开始 继续", "use first user text"},
		{"untitled", "  ", " \n ", "未命名会话", "fall back to a stable title"},
		{"exact", strings.Repeat("文", trayConversationTitleLen), "", strings.Repeat("文", trayConversationTitleLen), "keep 28 runes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := trayConversationTitle(kimi.WorkConversation{Title: tc.title, FirstUserText: tc.firstUser})
			if got != tc.want {
				t.Fatalf("%s: got %q, want %q", tc.description, got, tc.want)
			}
		})
	}

	long := strings.Repeat("会", trayConversationTitleLen+5)
	got := trayConversationTitle(kimi.WorkConversation{Title: long})
	if got != strings.Repeat("会", trayConversationTitleLen)+"…" {
		t.Fatalf("long unicode title was not truncated safely: %q", got)
	}
	if runes := []rune(got); len(runes) != trayConversationTitleLen+1 {
		t.Fatalf("truncated title has %d runes", len(runes))
	}
}

func TestTrayReadableModelNames(t *testing.T) {
	models := []kimi.ConversationModel{
		{Alias: "byok-kth-kimi/kimi-k2", DisplayName: "Kimi K2"},
		{Alias: "byok-kth-resp/group/glm-5-3-flash"},
		{Alias: "byok-kth-kimi/new-alias", PreviousAlias: "byok-kth-kimi/old-alias", DisplayName: "迁移模型"},
	}
	for _, tc := range []struct{ alias, want string }{
		{"k3-agent", "官方 · K3"},
		{"k3d1-agent", "官方 · K3.1 Pro"},
		{"k2d8-preview", "官方 · K2.8 Preview"},
		{"byok-kth-kimi/kimi-k2", "Kimi K2"},
		{"byok-kth-kimi/old-alias", "迁移模型"},
		{"byok-kth-resp/group/glm-5-3-flash", "group/glm-5-3-flash"},
		{"byok-123e4567-e89b-12d3-a456-426614174000/provider/model", "provider/model"},
		{"unknown-model", "默认模型"},
		{"", "默认模型"},
	} {
		if got := trayReadableModelName(tc.alias, models); got != tc.want {
			t.Fatalf("trayReadableModelName(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
	if got := trayModelDisplayName(kimi.ConversationModel{Alias: "byok-kth-kimi/kimi-k2"}, models); got != "Kimi K2" {
		t.Fatalf("display name fallback = %q", got)
	}
	if got := trayModelDisplayName(kimi.ConversationModel{Alias: "byok-kth-resp/group/glm-5-3-flash"}, models); got != "group/glm-5-3-flash" {
		t.Fatalf("readable display name fallback = %q", got)
	}
}

func TestTrayTargetThinkingLevel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		current  string
		defaults string
		efforts  []string
		want     string
	}{
		{"keep supported", "HIGH", "", []string{"off", "low", "high"}, "high"},
		{"default effort", "xhigh", "medium", []string{"off", "low", "medium", "high"}, "medium"},
		{"first non-off", "", "", []string{"off", "low", "high"}, "low"},
		{"only off", "", "", []string{"off"}, "off"},
		{"unsupported default", "", "ultra", []string{"off", "low"}, "low"},
		{"no efforts", "", "", nil, ""},
		{"invalid default without support", "", "high", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := trayTargetThinkingLevel(
				kimi.WorkConversation{ThinkingLevel: tc.current},
				kimi.ConversationModel{DefaultEffort: tc.defaults, SupportEfforts: tc.efforts},
			)
			if got != tc.want {
				t.Fatalf("thinking level = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTrayUpdateLabels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state trayUpdateState
		want  string
	}{
		{"available with v", trayUpdateState{State: "available", Available: &trayUpdateRelease{Version: "v1.1.11"}}, "发现新版本 v1.1.11 · 前往更新…"},
		{"available without v", trayUpdateState{State: "available", Available: &trayUpdateRelease{Version: "1.1.11"}}, "发现新版本 v1.1.11 · 前往更新…"},
		{"double prefix", trayUpdateState{State: "downloaded", Available: &trayUpdateRelease{Version: "VV1.1.11"}}, "发现新版本 v1.1.11 · 前往更新…"},
		{"missing version", trayUpdateState{State: "downloaded", Available: &trayUpdateRelease{}}, trayUpdatePlaceholder},
		{"missing release", trayUpdateState{State: "available"}, trayUpdatePlaceholder},
		{"not available", trayUpdateState{State: "idle", Available: &trayUpdateRelease{Version: "v9"}}, ""},
	} {
		if got := trayUpdateLabel(tc.state); got != tc.want {
			t.Fatalf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
	if trayFetchTimeout != 3*time.Second || trayHTTPTimeout <= trayRebindTimeout || trayResponseLimit != 2<<20 {
		t.Fatalf("unexpected tray timeouts or response limit: %v %v %v", trayFetchTimeout, trayHTTPTimeout, trayResponseLimit)
	}
}

func TestTrayUpdateViewHidesAndShows(t *testing.T) {
	menu := application.NewMenu()
	item := menu.Add(trayUpdatePlaceholder)
	view := &trayUpdateView{item: item}
	view.refresh(trayUpdateState{State: "idle"}, nil)
	if !item.Hidden() {
		t.Fatal("idle updates must keep the menu item hidden")
	}
	view.refresh(trayUpdateState{State: "available", Available: &trayUpdateRelease{Version: "1.2.3"}}, nil)
	if item.Hidden() || !item.Enabled() || item.Label() != "发现新版本 v1.2.3 · 前往更新…" {
		t.Fatalf("available update did not show: hidden=%v enabled=%v label=%q", item.Hidden(), item.Enabled(), item.Label())
	}
	view.refresh(trayUpdateState{}, errors.New("panel unavailable"))
	if !item.Hidden() {
		t.Fatal("failed update reads must hide the menu item")
	}
}

func TestTrayAccountsMenuStates(t *testing.T) {
	menu := application.NewMenu()
	view := &trayAccountsView{menu: menu}
	view.refresh(trayAccounts{State: "ready", CurrentID: "a", Accounts: []bridge.AccountSummary{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}}, nil)
	if menu.ItemAt(0) == nil || !menu.ItemAt(0).Checked() || menu.ItemAt(0).Enabled() {
		t.Fatalf("current account must be checked and disabled: %+v", menu.ItemAt(0))
	}
	if second := menu.ItemAt(1); second == nil || second.Checked() || !second.Enabled() {
		t.Fatalf("other account must stay enabled: %+v", second)
	}
	if last := menu.ItemAt(3); last == nil || last.Label() != "打开账号管理" {
		t.Fatalf("missing open-accounts entry: %+v", last)
	}

	failing := application.NewMenu()
	(&trayAccountsView{menu: failing}).refresh(trayAccounts{}, errors.New("panel_unavailable"))
	if failing.ItemAt(0) == nil || failing.ItemAt(0).Enabled() || failing.ItemAt(0).Label() != "账号状态暂不可读" {
		t.Fatalf("failed account read must degrade to a disabled row: %+v", failing.ItemAt(0))
	}

	empty := application.NewMenu()
	(&trayAccountsView{menu: empty}).refresh(trayAccounts{State: "ready"}, nil)
	if empty.ItemAt(0) == nil || empty.ItemAt(0).Enabled() || empty.ItemAt(0).Label() != "尚未保存账号" {
		t.Fatalf("empty accounts must degrade to a disabled row: %+v", empty.ItemAt(0))
	}
}

func TestTrayConversationsMenu(t *testing.T) {
	models := []kimi.ConversationModel{
		{Alias: "byok-kth-kimi/a", DisplayName: "模型 A"},
		{Alias: "byok-kth-resp/b", DisplayName: "模型 B"},
	}
	list := kimi.ConversationList{
		KimiRunning: true,
		Models:      models,
		Conversations: []kimi.WorkConversation{
			{ConversationKey: "k1", Title: "第一会话", ModelAlias: "byok-kth-kimi/a"},
			{ConversationKey: "k2", Title: "第二会话", ModelAlias: "官方未映射模型"},
			{ConversationKey: "k3", Title: strings.Repeat("长", trayConversationTitleLen+4), ModelAlias: ""},
			{ConversationKey: "k4", Title: "第四会话", ModelAlias: ""},
		},
	}
	menu := application.NewMenu()
	view := &trayConversationsView{menu: menu}
	view.refresh(list, nil)

	if first := menu.ItemAt(0); first == nil || !first.IsSubmenu() || first.Label() != "第一会话（模型 A）" {
		t.Fatalf("first conversation label = %+v", first)
	}
	if menu.ItemAt(3) == nil || !menu.ItemAt(3).IsSeparator() {
		t.Fatalf("expected a separator after three conversations: %+v", menu.ItemAt(3))
	}
	if bottom := menu.ItemAt(4); bottom == nil || bottom.Label() != "在面板查看全部会话…" {
		t.Fatalf("missing view-all entry: %+v", bottom)
	}
	if menu.ItemAt(5) != nil {
		t.Fatal("only three recent conversations may be listed")
	}

	sub := menu.ItemAt(0).GetSubmenu()
	current := sub.ItemAt(0)
	if current == nil || !current.Checked() || current.Enabled() || current.Label() != "模型 A" {
		t.Fatalf("current model must be checked and disabled: %+v", current)
	}
	other := sub.ItemAt(1)
	if other == nil || other.Checked() || !other.Enabled() || other.Label() != "模型 B" {
		t.Fatalf("target model must stay enabled: %+v", other)
	}
	if second := menu.ItemAt(1); !strings.HasSuffix(second.Label(), "（默认模型）") {
		t.Fatalf("unknown model alias must use the default label: %q", second.Label())
	}
	if third := menu.ItemAt(2); !strings.HasPrefix(third.Label(), strings.Repeat("长", trayConversationTitleLen)+"…（") {
		t.Fatalf("long titles must be truncated inside the submenu label: %q", third.Label())
	}

	failing := application.NewMenu()
	(&trayConversationsView{menu: failing}).refresh(kimi.ConversationList{}, errors.New("conversation_database_unavailable"))
	if failing.ItemAt(0) == nil || failing.ItemAt(0).Enabled() || failing.ItemAt(0).Label() != "会话列表暂不可读" {
		t.Fatalf("failed conversation read must degrade safely: %+v", failing.ItemAt(0))
	}

	reported := application.NewMenu()
	(&trayConversationsView{menu: reported}).refresh(kimi.ConversationList{Reason: "conversation_database_unavailable"}, nil)
	if reported.ItemAt(0) == nil || reported.ItemAt(0).Enabled() || reported.ItemAt(0).Label() != "会话列表暂不可读" {
		t.Fatalf("reported conversation read failure must degrade safely: %+v", reported.ItemAt(0))
	}

	empty := application.NewMenu()
	(&trayConversationsView{menu: empty}).refresh(kimi.ConversationList{}, nil)
	if empty.ItemAt(0) == nil || empty.ItemAt(0).Enabled() || empty.ItemAt(0).Label() != "暂无历史会话" {
		t.Fatalf("empty conversation list must degrade safely: %+v", empty.ItemAt(0))
	}

	noModels := application.NewMenu()
	(&trayConversationsView{menu: noModels}).refresh(kimi.ConversationList{Conversations: []kimi.WorkConversation{{ConversationKey: "k", Title: "会话"}}}, nil)
	if placeholder := noModels.ItemAt(0).GetSubmenu().ItemAt(0); placeholder == nil || placeholder.Enabled() || placeholder.Label() != "暂无已导入的第三方模型" {
		t.Fatalf("missing models must show a disabled placeholder: %+v", placeholder)
	}
}

func TestTraySnapshotFetchAllSources(t *testing.T) {
	key := strings.Repeat("a", 64)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("missing panel authentication")
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/accounts":
			w.Write([]byte(`{"accounts":{"state":"ready","currentId":"a","accounts":[{"id":"a","name":"A"}]}}`))
		case "/api/updates":
			w.Write([]byte(`{"state":"available","available":{"version":"v1.1.11"}}`))
		case "/api/work/conversations":
			w.Write([]byte(`{"conversations":[{"conversationKey":"k","title":"T","modelAlias":"byok-kth-kimi/a"}],"models":[{"alias":"byok-kth-kimi/a","displayName":"A"}],"kimiRunning":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}
	snapshot := fetchTraySnapshot(context.Background(), client)
	if snapshot.accountsErr != nil || snapshot.updatesErr != nil || snapshot.conversationsErr != nil {
		t.Fatalf("unexpected snapshot errors: %v %v %v", snapshot.accountsErr, snapshot.updatesErr, snapshot.conversationsErr)
	}
	if snapshot.accounts.State != "ready" || snapshot.updates.State != "available" || len(snapshot.conversations.Conversations) != 1 || !snapshot.conversations.KimiRunning {
		t.Fatalf("snapshot not parsed: %+v", snapshot)
	}
	if requests.Load() != 3 {
		t.Fatalf("expected three independent reads, got %d", requests.Load())
	}
}

func TestTraySnapshotDegradesSlowSource(t *testing.T) {
	key := strings.Repeat("b", 64)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			w.Write([]byte(`{"accounts":{"state":"ready","currentId":"a","accounts":[{"id":"a","name":"A"}]}}`))
		case "/api/updates":
			w.Write([]byte(`{"state":"idle"}`))
		case "/api/work/conversations":
			select {
			case <-r.Context().Done():
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	snapshot := fetchTraySnapshot(ctx, client)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a slow conversation read blocked the tray for %v", elapsed)
	}
	if snapshot.accountsErr != nil || snapshot.updatesErr != nil {
		t.Fatalf("fast sources were dragged down: %v %v", snapshot.accountsErr, snapshot.updatesErr)
	}
	if snapshot.conversationsErr == nil {
		t.Fatal("slow conversation source should fail independently")
	}
	if snapshot.accounts.State != "ready" || snapshot.updates.State != "idle" {
		t.Fatalf("fast sources were not parsed: %+v", snapshot)
	}
}

func TestTraySnapshotHandlesRejectedSources(t *testing.T) {
	key := strings.Repeat("c", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer server.Close()
	client := accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}
	snapshot := fetchTraySnapshot(context.Background(), client)
	if snapshot.accountsErr == nil || snapshot.updatesErr == nil || snapshot.conversationsErr == nil {
		t.Fatalf("all sources should report their own failure: %+v", snapshot)
	}
}

func TestAccountTrayAllowsLargeConversationPayload(t *testing.T) {
	key := strings.Repeat("d", 64)
	large := strings.Repeat("x", 100<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"conversations":[{"conversationKey":"k","title":"` + large + `"}],"models":[],"kimiRunning":false}`))
	}))
	defer server.Close()
	client := accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}
	var list kimi.ConversationList
	if err := client.request(context.Background(), http.MethodGet, "/api/work/conversations", nil, &list); err != nil {
		t.Fatalf("a 100 KiB conversation payload must fit the tray limit: %v", err)
	}
	if len(list.Conversations) != 1 || len(list.Conversations[0].Title) != len(large) {
		t.Fatalf("large conversation payload was truncated: %+v", list)
	}
}

func TestAccountTrayRejectsOversizedPayload(t *testing.T) {
	key := strings.Repeat("e", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat(" ", trayResponseLimit+1)))
	}))
	defer server.Close()
	client := accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}
	err := client.request(context.Background(), http.MethodGet, "/api/accounts", nil, nil)
	if err == nil || err.Error() != "account_service_invalid_response" {
		t.Fatalf("oversized payload was accepted: %v", err)
	}
}

func TestTrayRebindSendsSelection(t *testing.T) {
	key := strings.Repeat("f", 64)
	var got kimi.ConversationRebindRequest
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/work/conversations/rebind" {
			t.Errorf("unexpected rebind request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing same-origin mutation headers")
		}
		calls.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"conversationKeys":["k"],"kimiRestarted":true}`))
	}))
	defer server.Close()

	view := &trayConversationsView{client: accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}}
	view.runRebind(trayRebindTarget{key: "k", alias: "byok-kth-kimi/a", effort: "high", restart: true})
	if got.ConversationKeys == nil || len(got.ConversationKeys) != 1 || got.ConversationKeys[0] != "k" ||
		got.TargetModelAlias != "byok-kth-kimi/a" || got.ThinkingLevel != "high" || !got.RestartKimi {
		t.Fatalf("unexpected rebind request: %+v", got)
	}

	view.rebind.Store(true)
	view.runRebind(trayRebindTarget{key: "k2", alias: "byok-kth-kimi/b"})
	if calls.Load() != 1 {
		t.Fatalf("concurrent rebind guard did not hold: %d calls", calls.Load())
	}
}

func TestTrayRebindWithoutRunningKimi(t *testing.T) {
	key := strings.Repeat("g", 64)
	var got kimi.ConversationRebindRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"conversationKeys":["k"]}`))
	}))
	defer server.Close()
	view := &trayConversationsView{client: accountTrayClient{dir: trayTestStateDir(t, server.URL, key), client: server.Client()}}
	view.runRebind(trayRebindTarget{key: "k", alias: "byok-kth-kimi/a", restart: false})
	if got.RestartKimi {
		t.Fatalf("rebind must not restart a stopped Kimi: %+v", got)
	}
}

func TestTrayRebindFailureMessages(t *testing.T) {
	for _, code := range []string{"conversation_kimi_restart_required", "conversation_kimi_stop_failed", "conversation_rollback_failed", "invalid_conversation_thinking_level", "invalid_conversation_target_model", "account_service_unavailable"} {
		message := trayRebindFailureMessage(errors.New(code))
		if !strings.Contains(message, "会话页面") {
			t.Fatalf("%s: failure message must point at the conversations page: %q", code, message)
		}
	}
	if message := trayRebindFailureMessage(nil); !strings.Contains(message, "会话页面") {
		t.Fatalf("nil failure message = %q", message)
	}
}

package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/frontend"
	"github.com/wssz527/kimiwork-plus/internal/appupdate"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/quota"
	"github.com/wssz527/kimiwork-plus/internal/webtools"
)

type Endpoint struct {
	URL     string `json:"url"`
	Key     string `json:"key"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	BuildID string `json:"buildId"`
}
type KeyStore interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type Service struct {
	Store       *core.Store
	Keys        KeyStore
	Migration   *MigrationCoordinator
	Updates     *appupdate.Manager
	StateDir    string
	Endpoint    Endpoint
	Gateway     http.Handler
	Extra       func(context.Context, string, core.Document) (any, error)
	NativeState func() core.Document
	OnConfig    func(core.Document) error
	mutation    sync.Mutex
	mu          sync.RWMutex
	discoveries map[string]core.Document
	http        *http.Server
	stop        context.CancelFunc
	stopOnce    sync.Once
}

func (s *Service) SetStop(stop context.CancelFunc) { s.stop = stop }

func (s *Service) stopDaemon() {
	s.stopOnce.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
}

func RandomKey() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func (s *Service) Start(port string, setup ...func() error) error {
	previous, previousErr := ReadEndpoint(s.StateDir)
	reusePreviousPort := port == "" && previousErr == nil
	if reusePreviousPort {
		port = previous.URL[len("http://127.0.0.1:"):]
	}
	if port == "" {
		port = "0"
	}
	l, e := net.Listen("tcp4", "127.0.0.1:"+port)
	if e != nil && reusePreviousPort {
		l, e = net.Listen("tcp4", "127.0.0.1:0")
	}
	if e != nil {
		return e
	}
	key := RandomKey()
	if previousErr == nil {
		key = previous.Key
	}
	s.Endpoint = Endpoint{URL: "http://" + l.Addr().String(), Key: key, PID: os.Getpid(), Version: Version, BuildID: BuildID}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	for _, initialize := range setup {
		if e = initialize(); e != nil {
			l.Close()
			return e
		}
	}
	if e = core.AtomicJSON(filepath.Join(s.StateDir, "desktop-endpoint.json"), s.Endpoint); e != nil {
		l.Close()
		return e
	}
	go s.http.Serve(l)
	return nil
}
func (s *Service) Close(ctx context.Context) error {
	if s.Updates != nil {
		s.Updates.Close()
	}
	return s.http.Shutdown(ctx)
}
func (s *Service) SetGateway(h http.Handler) { s.mu.Lock(); s.Gateway = h; s.mu.Unlock() }
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, e error) {
	code := core.ErrorCode(e)
	status := 503
	if strings.HasPrefix(code, "invalid_") || code == "protocol_conflict" || code == "context_window_exceeds_limit" {
		status = 400
	}
	switch code {
	case "configuration_conflict", "translation_settings_conflict", "connection_in_use", "operation_busy", "quota_source_not_removable", "quota_credential_connection_unavailable", "account_work_busy", "account_login_pending", "account_login_not_pending", "account_login_unchanged", "account_current_in_use", "update_not_available", "rollback_unavailable", "direct_duplicate_endpoint", "direct_mode_unsupported", "direct_foreign_source", "native_import_direct_mode", "native_import_gateway_mode":
		status = 409
	case "direct_preview_refused":
		status = 503
	case "direct_catalog_too_large":
		status = 413
	case "direct_import_failed":
		status = 502
	case "direct_no_eligible_models", "direct_missing_api_key":
		status = 400
	case "unauthorized":
		status = 401
	case "origin_refused":
		status = 403
	case "not_found", "account_not_found":
		status = 404
	}
	var quotaError *quota.PanelActionError
	if errors.As(e, &quotaError) {
		status = quotaError.Status
		code = quotaError.Code
	}
	var codeUsageError *quota.CodeUsageError
	if errors.As(e, &codeUsageError) {
		status, code = codeUsageError.Status, codeUsageError.Code
	}
	var magpieError *quota.MagpieError
	if errors.As(e, &magpieError) {
		status, code = magpieError.Status, magpieError.Code
	}
	var webError *webtools.Error
	if errors.As(e, &webError) {
		status = webError.Status
		code = webError.Code
	}
	reply(w, status, core.Document{"error": core.Document{"code": code, "message": message(code)}})
}
func message(code string) string {
	switch code {
	case "configuration_conflict":
		return "配置已在其他窗口修改，请刷新后再保存。"
	case "native_preview_unavailable":
		return "此项原生能力当前不可用，请检查后台服务状态后重试。"
	case "connection_in_use":
		return "仍有模型使用此连接，请先修改模型。"
	case "credentials_unavailable":
		return "系统凭据存储不可用，密钥未保存。"
	case "account_work_busy":
		return "Work 仍有任务运行，请等待任务结束后再切换账号。"
	case "account_work_state_unavailable":
		return "无法确认 Work 是否空闲。请打开官方 Work 并刷新状态后重试。"
	case "account_login_pending":
		return "请先完成官方登录，或取消并恢复原账号。"
	case "account_current_in_use":
		return "当前账号或待恢复的账号不能移除，请先完成切换。"
	case "account_identity_unverified", "account_identity_mismatch":
		return "尚未确认官方页面与 Work 的账号一致，请检查官方登录状态后重试。"
	case "account_credentials_rejected":
		return "该账号的登录凭据已失效，请仅为该账号重新登录。"
	case "account_switch_restore_failed":
		return "切换失败且原账号尚未确认恢复，请在账号管理中恢复原账号。"
	case "credential_keychain_locked":
		return "登录钥匙串已锁定，请在 macOS 中解锁后从本窗口重新保存；密钥未写入。"
	case "credential_keychain_unavailable":
		return "当前登录会话无法使用系统钥匙串，请检查应用签名和会话权限；密钥未写入。"
	case "credential_session_unavailable":
		return "当前后台会话不能显示授权窗口，请从应用窗口重新保存。"
	case "credential_authorization_required":
		return "此条目需要系统授权，请从应用窗口重新保存并响应系统提示。"
	case "credential_authorization_cancelled":
		return "系统授权已取消，密钥未保存；原配置仍保留。"
	case "credential_authorization_failed":
		return "系统未接受授权，密钥未保存；原配置仍保留。"
	case "credential_store_corrupt":
		return "凭据条目内容无法识别，请从应用窗口重新输入并保存。"
	case "credential_interaction_not_restored":
		return "系统凭据交互状态未能恢复，请重启后台服务后重试。"
	case "credential_store_failed":
		return "系统凭据操作未完成，请查看诊断状态；密钥未保存。"
	case "updates_unavailable":
		return "当前进程未启用自动更新管理。"
	case "update_not_available":
		return "当前没有可安装的新版本，请先点击「检查更新」。"
	case "rollback_unavailable":
		return "当前没有可回退的上一版本备份。"
	case "invalid_update_settings":
		return "更新设置参数无效，请确认渠道为稳定版或预览版。"
	case "invalid_integration_mode":
		return "接入模式无效，请选择网关或直连。"
	case "direct_no_eligible_models":
		return "当前没有可直连导入的模型，请确认连接地址、上下文长度和工具调用支持。"
	case "direct_duplicate_endpoint":
		return "多个连接使用同一地址和协议，直连无法区分，请修改其中一个连接的地址。"
	case "direct_missing_api_key":
		return "缺少导入所需的服务商密钥，请在连接页重新保存密钥后重试。"
	case "direct_mode_unsupported":
		return "当前 Kimi Work 版本不支持直连导入，请升级官方 Work 或改用网关模式。"
	case "direct_preview_refused":
		return "Kimi 未接受本次直连导入，现有配置未被预先断开；请检查连接与模型设置后重试。"
	case "direct_catalog_too_large":
		return "单次直连目录过大，请减少一次同步的模型数量或拆分连接后重试。"
	case "direct_import_failed":
		return "Kimi 没有完成这次直连写入；请重新同步，仍失败时查看诊断信息。"
	case "direct_foreign_source":
		return "官方 Kimi 中已有不是本产品写入的模型源，未覆盖；请先核对现有来源。"
	case "native_import_direct_mode":
		return "当前是直连模式，网关模型同步不会执行；请先切换到网关模式。"
	case "native_import_gateway_mode":
		return "当前是网关模式，直连导入不会执行；请先切换到直连模式。"
	}
	return "操作未完成，请检查输入或诊断信息。"
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Host != strings.TrimPrefix(s.Endpoint.URL, "http://") {
		fail(w, errors.New("origin_refused"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		s.mu.RLock()
		g := s.Gateway
		s.mu.RUnlock()
		if g == nil {
			fail(w, errors.New("gateway_unavailable"))
		} else {
			g.ServeHTTP(w, r)
		}
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.Endpoint.URL {
		fail(w, errors.New("origin_refused"))
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" && site != "same-origin" {
		fail(w, errors.New("origin_refused"))
		return
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !strings.Contains(name, "/") && (strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".js")) {
			if b, e := frontend.Assets.ReadFile(name); e == nil {
				w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(name)))
				w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
				if r.Method != "HEAD" {
					w.Write(b)
				}
				return
			}
		}
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.Endpoint.Key)) != 1 {
		fail(w, errors.New("unauthorized"))
		return
	}
	if r.URL.Path == "/api/health" && r.Method == "GET" {
		reply(w, 200, core.Document{"version": Version, "buildId": BuildID, "pid": os.Getpid(), "state": "running", "isolated": true})
		return
	}
	var input core.Document
	if r.Method == "POST" {
		if r.Header.Get("Origin") != s.Endpoint.URL {
			fail(w, errors.New("origin_refused"))
			return
		}
		media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != "application/json" {
			fail(w, errors.New("invalid_content_type"))
			return
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, 32769))
		if e != nil || len(b) > 32768 || json.Unmarshal(b, &input) != nil || input == nil {
			fail(w, errors.New("invalid_json"))
			return
		}
	} else if r.Method != "GET" {
		fail(w, errors.New("not_found"))
		return
	}
	out, e := s.action(r.Context(), r.Method, r.URL.Path, input)
	if e != nil {
		fail(w, e)
		return
	}
	status := http.StatusOK
	if (r.URL.Path == "/api/actions" || r.URL.Path == "/api/daemon/stop" || r.URL.Path == "/api/service" || r.URL.Path == "/api/updates/download" || r.URL.Path == "/api/updates/rollback") && r.Method == "POST" {
		status = http.StatusAccepted
	}
	reply(w, status, out)
}
func (s *Service) action(ctx context.Context, method, path string, in core.Document) (any, error) {
	if method == "GET" {
		switch path {
		case "/api/state":
			return s.State()
		case "/api/providers":
			return core.Providers(), nil
		case "/api/service":
			return s.lifecycleAction(ctx, core.Document{"action": "status-service"})
		case "/api/updates":
			if s.Updates == nil {
				return core.Document{"currentVersion": Version, "buildId": BuildID, "state": "local"}, nil
			}
			return s.Updates.State(), nil
		}
	}
	if method == "POST" {
		switch path {
		case "/api/updates/check":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			st, err := s.Updates.CheckNow(ctx)
			return core.Document{"ok": err == nil, "updates": st}, nil
		case "/api/updates/download":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			st, err := s.Updates.StartDownloadAndApply()
			if err != nil {
				return nil, err
			}
			return core.Document{"ok": true, "updates": st}, nil
		case "/api/updates/cancel":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			return core.Document{"ok": true, "updates": s.Updates.Cancel()}, nil
		case "/api/updates/rollback":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			st, err := s.Updates.StartRollback()
			if err != nil {
				return nil, err
			}
			return core.Document{"ok": true, "updates": st}, nil
		case "/api/updates/settings":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			var autoCheck *bool
			if raw, ok := in["autoCheck"]; ok {
				b, valid := raw.(bool)
				if !valid {
					return nil, errors.New("invalid_update_settings")
				}
				autoCheck = &b
			}
			channel := ""
			if raw, ok := in["channel"]; ok {
				c, valid := raw.(string)
				if !valid || (c != "stable" && c != "preview") {
					return nil, errors.New("invalid_update_settings")
				}
				channel = c
			}
			st, err := s.Updates.UpdateSettings(autoCheck, channel)
			if err != nil {
				return nil, err
			}
			return core.Document{"ok": true, "updates": st}, nil
		case "/api/workspace", "/api/models", "/api/connections", "/api/subagents", "/api/translation", "/api/budget/save", "/api/integration-mode":
			return s.configure(path, in)
		case "/api/actions", "/api/context", "/api/tools", "/api/web-tools", "/api/quota/code", "/api/quota/magpie":
			if s.Extra != nil {
				return s.Extra(ctx, path, in)
			}
		case "/api/translation/preview", "/api/translation/from-connection":
			return s.translationAction(ctx, path, in)
		case "/api/discovery":
			result, e := s.discover(ctx, in)
			if e != nil {
				return nil, e
			}
			return core.Document{"discovery": result}, nil
		case "/api/model-search":
			return s.search(in)
		case "/api/service":
			if core.String(in["action"]) == "restart-daemon" {
				if s.stop == nil {
					return nil, errors.New("daemon_restart_unavailable")
				}
				if err := ReplaceDaemon(s.StateDir); err != nil {
					return nil, err
				}
				go s.stopDaemon()
				return core.Document{"operation": core.Document{"action": "restart-daemon", "state": "stopping", "endpoint": s.Endpoint.URL}}, nil
			}
			return s.lifecycleAction(ctx, in)
		case "/api/migration/resume":
			if s.Migration == nil {
				return core.Document{"ok": true, "migration": core.Document{"status": "not_needed"}}, nil
			}
			if _, err := s.Migration.ResumeInteractive(); err != nil {
				return nil, err
			}
			if s.OnConfig != nil {
				if d, _, readErr := s.Store.Read(); readErr == nil {
					_ = s.OnConfig(d)
				}
			}
			state, err := s.State()
			return core.Document{"ok": true, "migration": s.Migration.Status(), "state": state}, err
		case "/api/daemon/stop":
			if s.stop == nil {
				return nil, errors.New("daemon_stop_unavailable")
			}
			go s.stopDaemon()
			return core.Document{"operation": core.Document{"action": "stop-daemon", "state": "stopping"}}, nil
		}
	}
	if method == "GET" && (path == "/api/quota/code" || path == "/api/quota/magpie" || path == "/api/accounts" || path == "/api/work/swarm") && s.Extra != nil {
		return s.Extra(ctx, path, nil)
	}
	expected := map[string]string{"/api/usage/query": "POST", "/api/usage/export": "GET", "/api/usage/clear": "POST", "/api/quota": "GET", "/api/quota/refresh": "POST", "/api/quota/sources": "POST", "/api/budget": "GET"}
	if want, ok := expected[path]; ok {
		if method != want {
			return nil, errors.New("not_found")
		}
		if s.Extra != nil {
			return s.Extra(ctx, path, in)
		}
	}
	return nil, errors.New("native_preview_unavailable")
}
func (s *Service) configure(path string, in core.Document) (any, error) {
	if path == "/api/translation" {
		return s.saveTranslation(in)
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	// Credentials staged by this save are discarded when it fails before the
	// configuration is durable. A failure after the commit leaves them active,
	// because the committed document already refers to them.
	var pending []string
	d, rev, e := s.Store.Prepare(core.String(in["revision"]), func(d core.Document) error {
		switch path {
		case "/api/workspace":
			if _, ok := in["models"].([]any); !ok {
				return errors.New("invalid_models")
			}
			if _, ok := in["connections"].([]any); !ok {
				return errors.New("invalid_connections")
			}
			if e := core.MergeModels(d, core.Rows(in["models"])); e != nil {
				return e
			}
			changes, e := core.MergeConnections(d, core.Rows(in["connections"]))
			if e != nil {
				return e
			}
			if e = core.Validate(d); e != nil {
				return e
			}
			var staged []string
			if staged, e = s.stageCredentials(d, changes); e == nil {
				pending = staged
			}
			return e
		case "/api/models":
			if _, ok := in["models"].([]any); !ok {
				return errors.New("invalid_models")
			}
			return core.MergeModels(d, core.Rows(in["models"]))
		case "/api/connections":
			if _, ok := in["connections"].([]any); !ok {
				return errors.New("invalid_connections")
			}
			changes, e := core.MergeConnections(d, core.Rows(in["connections"]))
			if e != nil {
				return e
			}
			if e = core.Validate(d); e != nil {
				return e
			}
			var staged []string
			if staged, e = s.stageCredentials(d, changes); e == nil {
				pending = staged
			}
			return e
		case "/api/budget/save":
			if core.String(in["revision"]) == "" {
				return core.ErrConflict
			}
			return core.MergeBudget(d, in["budget"])
		case "/api/subagents":
			policy, err := core.NormalizeSubagents(in, core.Rows(d["models"]))
			if err != nil {
				return err
			}
			d["subagents"] = map[string]any(policy)
		case "/api/integration-mode":
			if core.String(in["revision"]) == "" {
				return core.ErrConflict
			}
			return core.SetIntegrationMode(d, core.String(in["mode"]))
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	d, _, e = s.Store.Commit(d, rev)
	if e != nil {
		// The commit may have failed after writing the recovery journal that the
		// next start applies, so only unreferenced keys are removed here.
		s.discardStagedCredentials(pending)
		return nil, e
	}
	if s.OnConfig != nil {
		if e = s.OnConfig(d); e != nil {
			return nil, e
		}
	}
	state, e := s.State()
	return core.Document{"ok": true, "state": state}, e
}
func (s *Service) State() (core.Document, error) {
	d, rev, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	connections := connectionRows(d, s.Keys)
	trans := core.Document{"enabled": false, "provider": "bing", "targetLanguage": "zh", "view": "translated", "customEndpoint": "", "customModel": "", "apiKeyEnv": "KIMI_WORK_TRANSLATION_API_KEY"}
	updatesDoc := core.Document{"currentVersion": Version, "buildId": BuildID, "state": "local"}
	if s.Updates != nil {
		updatesDoc = s.Updates.State()
	}
	state := core.Document{
		"schemaVersion": 1, "revision": rev, "capabilities": core.Document{"modelEditor": core.Document{"upstreamProtocol": true}},
		"configuration": core.Document{"state": "ready", "file": "gateway.json"}, "service": s.serviceStatus(), "updates": updatesDoc,
		"gateway": core.Document{"state": "unavailable", "modelCount": 0, "activeRequests": 0}, "models": d["models"],
		"connections": core.Document{"active": connections, "saved": connections, "restartRequired": false, "apply": core.Document{"state": "applied"}},
		"connection":  core.Document{"active": core.Document{}, "saved": core.Document{}, "environment": core.Document{"apiKeySet": false, "tokenSet": true}},
		"subagents":   core.Document{"configured": core.Document{"mode": "inherit"}, "policy": core.Document{"mode": "inherit"}, "runtime": core.Document{"state": "inherit", "mode": "inherit"}},
		"translation": core.Document{"configured": trans, "revision": rev, "environment": core.Document{"keySet": false}, "runtime": core.Document{"state": "unavailable", "reason": "native_preview_unavailable"}},
		"integration": core.Document{"state": "unchecked", "imported": false, "reason": "native_preview_unavailable", "desktop": core.Document{"state": "unavailable", "reason": "native_preview_unavailable"}},
		"kimiProcess": core.Document{"running": nil, "state": "unknown"}, "context": core.Document{"policy": core.Document{"mode": "inherit"}, "status": "unavailable"},
		"tools": core.Document{"available": false, "reason": "native_preview_unavailable", "servers": []any{}}, "search": core.Document{"status": "unavailable"},
		"metrics": core.Document{"requests": 0, "errors": 0}, "diagnostics": core.Document{}, "setup": core.Document{"revealed": false, "intranet": core.Document{"passed": false}}, "operation": nil,
		"integrationMode": core.IntegrationMode(d), "directMode": directModeBase(d),
	}
	s.mu.RLock()
	g := s.Gateway
	s.mu.RUnlock()
	if source, ok := g.(interface{ State() core.Document }); ok {
		for k, v := range source.State() {
			state[k] = v
		}
	}
	settings, e := core.TranslationSettings(d["translation"])
	if e != nil {
		return nil, e
	}
	runtimeState := "inactive"
	reason := "no_managed_child"
	if !settings.Enabled {
		runtimeState = "disabled"
		reason = "translation_disabled"
	}
	state["translation"] = core.Document{"configured": translationPublicConfig(settings), "revision": translationRevision(settings), "environment": core.Document{"keySet": s.translationKey(d, settings) != ""}, "runtime": core.Document{"state": runtimeState, "reason": reason}}
	core.Object(state["gateway"])["url"] = s.Endpoint.URL + "/v1"
	if s.Migration != nil {
		state["migration"] = s.Migration.Status()
	}
	if s.NativeState != nil {
		for k, v := range s.NativeState() {
			state[k] = v
		}
	}
	return state, nil
}

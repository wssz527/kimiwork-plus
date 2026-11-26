package bridge

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/frontend"
	"github.com/wssz527/kimiwork-plus/internal/appupdate"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/platform"
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
	OpenURL     func(string) error
	ExportDir   string
	StateDir    string
	Endpoint    Endpoint
	Gateway     http.Handler
	Extra       func(context.Context, string, core.Document) (any, error)
	NativeState func() core.Document
	OnConfig    func(core.Document) error
	// ScheduleAutoSync is set when the native layer is attached. It is told the
	// document before and after every save that can change the Kimi catalog.
	ScheduleAutoSync func(path string, prev, next core.Document)
	autoSync         *autoSyncCoordinator
	mutation         sync.Mutex
	mu               sync.RWMutex
	discoveries      map[string]core.Document
	http             *http.Server
	stop             context.CancelFunc
	stopOnce         sync.Once
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
	if s.autoSync != nil {
		s.autoSync.stop()
	}
	if s.http == nil {
		return nil
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
	if strings.HasPrefix(code, "invalid_") || code == "protocol_conflict" || code == "context_window_exceeds_limit" || code == "kimi_k3_model_id_conflict" || code == "connection_protocol_required" {
		status = 400
	}
	switch code {
	case "configuration_conflict", "translation_settings_conflict", "connection_in_use", "connection_unavailable", "subagent_model_in_use", "operation_busy", "account_session_unavailable", "account_work_home_unavailable", "account_page_sync_pending", "quota_source_not_removable", "quota_credential_connection_unavailable", "account_work_busy", "account_login_pending", "account_login_not_pending", "account_login_unchanged", "account_current_in_use", "update_not_available", "rollback_unavailable", "direct_duplicate_endpoint", "direct_mode_unsupported", "direct_foreign_source", "native_import_direct_mode", "native_import_gateway_mode":
		status = 409
	case "conversation_kimi_restart_required", "conversation_database_busy":
		status = 409
	case "operation_wait_cancelled":
		status = http.StatusRequestTimeout
	case "conversation_not_found":
		status = 404
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
	case "not_found", "account_not_found", "release_url_unavailable":
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
	body := core.Document{"code": code, "message": message(code)}
	var occupied *core.ModelUseError
	if errors.As(e, &occupied) && len(occupied.Models) > 0 {
		body["models"] = occupied.Models
		body["message"] = message(code) + " 涉及模型：" + strings.Join(occupied.Models[:min(len(occupied.Models), 3)], "、") + "。"
	}
	reply(w, status, core.Document{"error": body})
}
func message(code string) string {
	switch code {
	case "configuration_conflict":
		return "配置已在其他窗口修改，请刷新后再保存。"
	case "native_preview_unavailable":
		return "此项原生能力当前不可用，请检查后台服务状态后重试。"
	case "invalid_base_url":
		return "请输入有效的上游 API 地址。"
	case "connection_unavailable":
		return "所选连接已不可用，请刷新列表并重新选择。"
	case "subagent_model_in_use":
		return "子代理仍在使用这些模型，请先修改子代理设置。"
	case "account_session_unavailable":
		return "请先打开并登录 Kimi Work，再保存当前账号。"
	case "account_work_home_unavailable":
		return "找不到 Kimi Work 的配置。请先安装并打开 Kimi Work，登录后再切换账号。"
	case "account_page_sync_pending":
		return "正在等待 Kimi Work 同步账号登录状态，请稍后重试。"
	case "operation_wait_cancelled":
		return "等待后台同步结束超时，请稍后重试。"
	case "invalid_probe_request", "invalid_probe_protocol":
		return "请检查填写的探测参数与接口协议。"
	case "probe_missing_key":
		return "请先填写 API Key。"
	case "probe_unauthorized":
		return "上游拒绝了认证，请检查 API Key 和模型权限。"
	case "probe_model_required":
		return "上游未提供可用的模型目录，请填写一个模型 ID 后重试。"
	case "probe_timeout":
		return "连接测试超时，请检查网络后重试。"
	case "probe_unreachable":
		return "无法连接上游，请检查 API 地址和网络。"
	case "probe_inference_failed":
		return "上游未接受测试请求，请检查模型 ID 和接口协议。"
	case "connection_in_use":
		return "仍有模型使用此连接，请先修改模型。"
	case "connection_protocol_required":
		return "请为该连接选择接口协议（Chat 或 Responses）后再保存。"
	case "invalid_connection_protocols":
		return "一个连接只能选择一种接口协议，且必须是 Chat 或 Responses。"
	case "invalid_connection_protocol_in_use":
		return "这条连接仍有已保存的模型使用 Responses。改成 Chat Completions 会中断已有会话，请保留现有协议或先为这些模型更换连接。"
	case "operation_busy":
		return "上一次操作仍在进行，请稍后重试。"
	case "credentials_unavailable":
		return "加密凭据存储不可用，密钥未保存。"
	case "account_work_busy":
		return "Work 仍有任务运行，请等待任务结束后再切换账号。"
	case "invalid_conversation_rebind":
		return "请选择至少一个会话和一个目标模型。"
	case "invalid_conversation_target_model":
		return "目标模型当前不可用，请刷新列表并重新选择。"
	case "invalid_conversation_migration_target":
		return "找不到此模型的旧别名，请先同步模型再刷新会话列表。"
	case "invalid_conversation_thinking_level":
		return "目标模型不支持所选思考档位，请重新选择。"
	case "conversation_rebind_cancelled":
		return "改绑已取消，未写入会话。请刷新后重试。"
	case "conversation_kimi_restart_required":
		return "Kimi 正在运行。请刷新状态并确认重启后再改绑。"
	case "conversation_kimi_state_unknown":
		return "无法确认 Kimi 的运行状态，请刷新后重试。"
	case "conversation_profile_unsupported":
		return "诊断隔离模式不可改绑会话，请使用默认方式打开 Kimi。"
	case "conversation_kimi_stop_failed":
		return "未能确认 Kimi 已安全退出，请手动退出 Kimi 后重试。"
	case "conversation_not_found":
		return "部分所选会话已不可用，请刷新列表。"
	case "conversation_database_busy":
		return "Kimi 会话库正在使用，请退出 Kimi 后重试。"
	case "conversation_database_missing", "conversation_database_unavailable", "conversation_database_schema_unsupported", "conversation_node_unavailable":
		return "当前无法访问 Kimi 会话库，请刷新状态后重试。"
	case "conversation_bindings_invalid", "conversation_preferences_invalid":
		return "会话绑定数据格式异常，改绑已中止。"
	case "conversation_bindings_read_only":
		return "会话绑定文件是只读的，改绑已中止。请取消文件的只读属性后重试。"
	case "conversation_rollback_failed":
		return "会话绑定恢复尚未完成。请完全退出 Kimi，在「旧会话改绑模型」中刷新。"
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
	case "credential_master_key_invalid":
		return "本机凭据主密钥缺失或无效，原文件未被覆盖。请保留应用数据并从备份恢复；不要删除后重建，否则已有凭据无法恢复。"
	case "credential_readback_mismatch", "credentials_readback_failed":
		return "新凭据写入后未通过读回验证，未验证的值已清理。请重试；若仍失败，请重启后台服务。"
	case "credential_interaction_not_restored":
		return "系统凭据交互状态未能恢复，请重启后台服务后重试。"
	case "credential_store_failed":
		return "系统凭据操作未完成，请查看诊断状态；密钥未保存。"
	case "invalid_subagent_roles":
		return "角色模型分配无效，请确认所选模型均在候选池中且支持工具调用。"
	case "updates_unavailable":
		return "当前进程未启用自动更新管理。"
	case "release_url_unavailable":
		return "当前没有可打开的发布页地址，请先检查更新。"
	case "invalid_external_url":
		return "外部链接地址无效，仅支持 HTTPS 链接。"
	case "export_write_failed":
		return "无法写入导出文件，请检查下载目录权限后重试。"
	case "open_url_failed":
		return "无法调起系统默认浏览器，请手动在浏览器中打开链接。"
	case "update_not_available":
		return "当前没有可安装的新版本，请先点击「检查更新」。"
	case "rollback_unavailable":
		return "当前没有可回退的上一版本备份。"
	case "invalid_update_settings":
		return "更新设置参数无效，请确认渠道为稳定版或预览版。"
	case "invalid_integration_mode":
		return "接入模式无效，请选择网关或直连。"
	case "invalid_kimi_k3_compatibility":
		return "请在本机网关模式下调整 K3 原会话切换设置。"
	case "kimi_k3_model_id_conflict":
		return "有模型 ID 与 K3 兼容别名冲突，请修改对应模型 ID。"
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
		bodyLimit := 32 << 10
		if r.URL.Path == "/api/work/conversations/rebind" {
			bodyLimit = 2 << 20
		}
		b, e := io.ReadAll(io.LimitReader(r.Body, int64(bodyLimit)+1))
		if e != nil || len(b) > bodyLimit || json.Unmarshal(b, &input) != nil || input == nil {
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
		case "/api/diagnostics/export":
			if s.Extra == nil {
				return nil, errors.New("native_preview_unavailable")
			}
			return s.Extra(ctx, path, nil)
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
		case "/api/connections/probe":
			return s.probeConnection(ctx, in)
		case "/api/open-url":
			rawURL, ok := in["url"].(string)
			if !ok {
				return nil, errors.New("invalid_external_url")
			}
			safeURL, err := platform.ValidateExternalHTTPURL(rawURL)
			if err != nil || !strings.HasPrefix(safeURL, "https://") {
				return nil, errors.New("invalid_external_url")
			}
			opener := s.OpenURL
			if opener == nil {
				opener = platform.OpenExternalURL
			}
			if err := opener(safeURL); err != nil {
				return nil, errors.New("open_url_failed")
			}
			return core.Document{"ok": true, "url": safeURL}, nil
		case "/api/diagnostics/export":
			if s.Extra == nil {
				return nil, errors.New("native_preview_unavailable")
			}
			report, err := s.Extra(ctx, path, nil)
			if err != nil {
				return nil, err
			}
			savedPath, err := s.writeExportJSON("kimi-work-plus-diagnostics", report)
			if err != nil {
				return nil, err
			}
			return core.Document{"ok": true, "path": savedPath, "report": report}, nil
		case "/api/usage/export":
			if s.Extra == nil {
				return nil, errors.New("native_preview_unavailable")
			}
			exported, err := s.Extra(ctx, path, in)
			if err != nil {
				return nil, err
			}
			savedPath, err := s.writeExportJSON("kimi-work-plus-usage", exported)
			if err != nil {
				return nil, err
			}
			return core.Document{"ok": true, "path": savedPath}, nil
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
		case "/api/updates/open-release":
			if s.Updates == nil {
				return nil, errors.New("updates_unavailable")
			}
			st := s.Updates.State()
			rawURL := ""
			switch avail := st["available"].(type) {
			case *appupdate.AvailableRelease:
				if avail != nil {
					rawURL = avail.ReleaseURL
				}
			case core.Document:
				rawURL = core.String(avail["releaseUrl"])
			case map[string]any:
				rawURL = core.String(avail["releaseUrl"])
			}
			if rawURL == "" {
				rawURL = core.String(st["releaseUrl"])
			}
			if rawURL == "" {
				return nil, errors.New("release_url_unavailable")
			}
			safeURL, err := platform.ValidateExternalHTTPURL(rawURL)
			if err != nil {
				return nil, errors.New("release_url_unavailable")
			}
			opener := s.OpenURL
			if opener == nil {
				opener = platform.OpenExternalURL
			}
			if err := opener(safeURL); err != nil {
				return nil, errors.New("open_url_failed")
			}
			return core.Document{"ok": true, "url": safeURL}, nil
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
		case "/api/workspace", "/api/models", "/api/connections", "/api/connections/delete", "/api/subagents", "/api/translation", "/api/budget/save", "/api/integration-mode", "/api/kimi-k3-compatibility":
			return s.configure(path, in)
		case "/api/actions", "/api/context", "/api/tools", "/api/web-tools", "/api/quota/code", "/api/quota/magpie", "/api/work/conversations/rebind":
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
	if method == "GET" && (path == "/api/quota/code" || path == "/api/quota/magpie" || path == "/api/accounts" || path == "/api/work/swarm" || path == "/api/work/conversations") && s.Extra != nil {
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
	var previous core.Document
	var removed core.ConnectionRemoval
	d, rev, e := s.Store.Prepare(core.String(in["revision"]), func(d core.Document) error {
		previous = cloneDocument(d)
		switch path {
		case "/api/workspace":
			if _, ok := in["models"].([]any); !ok {
				return errors.New("invalid_models")
			}
			if _, ok := in["connections"].([]any); !ok {
				return errors.New("invalid_connections")
			}
			mergeModels := core.MergeModels
			if raw, exists := in["pruneSubagents"]; exists {
				prune, ok := raw.(bool)
				if !ok {
					return errors.New("invalid_models")
				}
				if prune {
					mergeModels = core.MergeModelsPruningReferences
				}
			}
			if e := mergeModels(d, core.Rows(in["models"])); e != nil {
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
		case "/api/connections/delete":
			receipt, err := deleteConnection(d, in)
			if err != nil {
				return err
			}
			removed = receipt
			return core.Validate(d)
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
		case "/api/kimi-k3-compatibility":
			if core.String(in["revision"]) == "" {
				return core.ErrConflict
			}
			enabled, ok := in["enabled"].(bool)
			if !ok || core.IntegrationMode(d) != core.IntegrationModeGateway {
				return errors.New("invalid_kimi_k3_compatibility")
			}
			d[core.KimiK3CompatibilityKey] = enabled
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
	if removed.APIKeyRef != "" {
		defer s.discardUnreferencedCredentials([]string{removed.APIKeyRef}, "")
	}
	if s.OnConfig != nil {
		if e = s.OnConfig(d); e != nil {
			return nil, e
		}
	}
	if s.ScheduleAutoSync != nil {
		// A save that only touches a gateway-side field leaves the Kimi catalog
		// alone; the coordinator compares the two documents itself.
		s.ScheduleAutoSync(path, previous, d)
	}
	state, e := s.State()
	result := core.Document{"ok": true, "state": state}
	if removed.ConnectionID != "" {
		result["removed"] = removed
	}
	return result, e
}

// cloneDocument is the document as it was before a save: the merge helpers only
// replace the rows they own, but a caller must never see a mutated history.
func cloneDocument(d core.Document) core.Document {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	var copy core.Document
	if json.Unmarshal(raw, &copy) != nil {
		return nil
	}
	return copy
}

func (s *Service) writeExportJSON(prefix string, value any) (string, error) {
	data, err := marshalExportJSON(value)
	if err != nil {
		return "", errors.New("export_write_failed")
	}
	base := prefix + "-" + time.Now().Format("20060102-150405")

	primaryDir := strings.TrimSpace(s.ExportDir)
	if primaryDir == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil && strings.TrimSpace(home) != "" {
			primaryDir = filepath.Join(home, "Downloads")
		}
	}
	if primaryDir != "" {
		if info, statErr := os.Stat(primaryDir); statErr == nil && info.IsDir() {
			if saved, writeErr := writePrivateExportFile(primaryDir, base, data); writeErr == nil {
				return saved, nil
			}
		}
	}
	if strings.TrimSpace(s.StateDir) != "" {
		fallbackDir := filepath.Join(s.StateDir, "exports")
		if mkErr := os.MkdirAll(fallbackDir, 0o700); mkErr == nil {
			if saved, writeErr := writePrivateExportFile(fallbackDir, base, data); writeErr == nil {
				return saved, nil
			}
		}
	}
	return "", errors.New("export_write_failed")
}

func marshalExportJSON(value any) ([]byte, error) {
	var raw []byte
	switch typed := value.(type) {
	case json.RawMessage:
		raw = typed
	case []byte:
		raw = typed
	}
	if len(raw) > 0 {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err == nil {
			buf.WriteByte('\n')
			return buf.Bytes(), nil
		}
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func writePrivateExportFile(dir, base string, data []byte) (string, error) {
	const maxAttempts = 1000
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		name := base + ".json"
		if attempt > 1 {
			name = base + "-" + strconv.Itoa(attempt) + ".json"
		}
		target := filepath.Join(dir, name)
		// O_EXCL is what makes the suffix a guarantee rather than a guess: two
		// exports in the same second each get their own file, and the loser
		// moves on instead of truncating the winner.
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		// The body is written, flushed and closed before the name is reported, so
		// a reader never sees a file this call is still filling. A failure removes
		// the partial file instead of leaving a truncated export behind.
		_, writeErr := file.Write(data)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		if closeErr := file.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			_ = os.Remove(target)
			return "", writeErr
		}
		return target, nil
	}
	return "", os.ErrExist
}

func (s *Service) State() (core.Document, error) {
	d, rev, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	s.mu.RLock()
	g := s.Gateway
	s.mu.RUnlock()
	var gatewayState core.Document
	if source, ok := g.(interface{ State() core.Document }); ok {
		gatewayState = source.State()
	}
	var connections []any
	if _, hasConnections := gatewayState["connections"]; !hasConnections {
		connections = connectionRows(d, s.Keys)
	}
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
		"integration": core.Document{"state": "unchecked", "imported": false, "reason": "native_preview_unavailable", "desktop": core.Document{"state": "unavailable", "reason": "native_preview_unavailable"}, "autoSync": core.Document{"state": autoSyncIdle, "reason": "", "updatedAt": int64(0), "pending": false}},
		"kimiProcess": core.Document{"running": nil, "state": "unknown"}, "context": core.Document{"policy": core.Document{"mode": "inherit"}, "status": "unavailable"},
		"tools": core.Document{"available": false, "reason": "native_preview_unavailable", "servers": []any{}}, "search": core.Document{"status": "unavailable"},
		"metrics": core.Document{"requests": 0, "errors": 0}, "diagnostics": core.Document{"operationLog": core.Document{"available": false, "retained": 0, "limit": diagnosticsEventLimit, "lastAt": int64(0), "reason": "diagnostics_unconfigured"}}, "setup": core.Document{"revealed": false, "intranet": core.Document{"passed": false}}, "operation": nil,
		"integrationMode": core.IntegrationMode(d), "directMode": directModeBase(d),
		"kimiK3Compatibility": core.KimiK3Compatibility(d),
	}
	for k, v := range gatewayState {
		state[k] = v
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
	state["translation"] = s.translationState(d, settings, core.Document{"state": runtimeState, "reason": reason})
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

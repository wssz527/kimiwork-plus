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

	"kimiworkplus.local/migration/frontend"
	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/quota"
	"kimiworkplus.local/migration/internal/webtools"
)

const Version = "0.13.0-preview.1"

type Endpoint struct {
	URL     string `json:"url"`
	Key     string `json:"key"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}
type KeyStore interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type Service struct {
	Store       *core.Store
	Keys        KeyStore
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
}

func RandomKey() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func (s *Service) Start(port string, setup ...func() error) error {
	l, e := net.Listen("tcp4", "127.0.0.1:"+port)
	if e != nil {
		return e
	}
	s.Endpoint = Endpoint{URL: "http://" + l.Addr().String(), Key: RandomKey(), PID: os.Getpid(), Version: Version}
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
func (s *Service) Close(ctx context.Context) error { return s.http.Shutdown(ctx) }
func (s *Service) SetGateway(h http.Handler)       { s.mu.Lock(); s.Gateway = h; s.mu.Unlock() }
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
	case "configuration_conflict", "translation_settings_conflict", "connection_in_use", "operation_busy", "quota_source_not_removable", "quota_credential_connection_unavailable":
		status = 409
	case "unauthorized":
		status = 401
	case "origin_refused":
		status = 403
	case "not_found":
		status = 404
	}
	var quotaError *quota.PanelActionError
	if errors.As(e, &quotaError) {
		status = quotaError.Status
		code = quotaError.Code
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
		return "此项原生能力尚未通过迁移验收，请继续使用稳定版 0.12.1。"
	case "connection_in_use":
		return "仍有模型使用此连接，请先修改模型。"
	case "credentials_unavailable":
		return "系统凭据存储不可用，密钥未保存。"
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
		reply(w, 200, core.Document{"version": Version, "pid": os.Getpid(), "state": "running", "isolated": true})
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
	if r.URL.Path == "/api/actions" && r.Method == "POST" {
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
		}
	}
	if method == "POST" {
		switch path {
		case "/api/models", "/api/connections", "/api/subagents", "/api/translation":
			return s.configure(path, in)
		case "/api/actions", "/api/context", "/api/tools", "/api/web-tools":
			if s.Extra != nil {
				return s.Extra(ctx, path, in)
			}
		case "/api/translation/preview", "/api/translation/from-connection":
			return s.translationAction(ctx, path, in)
		case "/api/discovery":
			return s.discover(ctx, in)
		case "/api/model-search":
			return s.search(in)
		}
	}
	expected := map[string]string{"/api/usage/query": "POST", "/api/usage/export": "GET", "/api/usage/clear": "POST", "/api/quota": "GET", "/api/quota/refresh": "POST", "/api/quota/sources": "POST"}
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
	d, _, e := s.Store.Update(core.String(in["revision"]), func(d core.Document) error {
		switch path {
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
			return s.stageCredentials(d, changes)
		case "/api/subagents":
			policy, err := core.NormalizeSubagents(in, core.Rows(d["models"]))
			if err != nil {
				return err
			}
			d["subagents"] = map[string]any(policy)
		case "/api/translation":
			return errors.New("native_preview_unavailable")
		}
		return nil
	})
	if e != nil {
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
	state := core.Document{
		"schemaVersion": 1, "revision": rev, "capabilities": core.Document{"modelEditor": core.Document{"upstreamProtocol": true}},
		"configuration": core.Document{"state": "ready", "file": "gateway.json"}, "service": core.Document{"mode": "preview", "state": "running"}, "updates": core.Document{"currentVersion": Version, "state": "not-checked"},
		"gateway": core.Document{"state": "unavailable", "modelCount": 0, "activeRequests": 0}, "models": d["models"],
		"connections": core.Document{"active": connections, "saved": connections, "restartRequired": false, "apply": core.Document{"state": "applied"}},
		"connection":  core.Document{"active": core.Document{}, "saved": core.Document{}, "environment": core.Document{"apiKeySet": false, "tokenSet": true}},
		"subagents":   core.Document{"configured": core.Document{"mode": "inherit"}, "policy": core.Document{"mode": "inherit"}, "runtime": core.Document{"state": "inherit", "mode": "inherit"}},
		"translation": core.Document{"configured": trans, "revision": rev, "environment": core.Document{"keySet": false}, "runtime": core.Document{"state": "unavailable", "reason": "native_preview_unavailable"}},
		"integration": core.Document{"state": "unchecked", "imported": false, "reason": "native_preview_unavailable", "desktop": core.Document{"state": "unavailable", "reason": "native_preview_unavailable"}},
		"kimiProcess": core.Document{"running": nil, "state": "unknown"}, "context": core.Document{"policy": core.Document{"mode": "inherit"}, "status": "unavailable"},
		"tools": core.Document{"available": false, "reason": "native_preview_unavailable", "servers": []any{}}, "search": core.Document{"status": "unavailable"},
		"metrics": core.Document{"requests": 0, "errors": 0}, "diagnostics": core.Document{}, "setup": core.Document{"revealed": false, "intranet": core.Document{"passed": false}}, "operation": nil,
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
	state["translation"] = core.Document{"configured": settings, "revision": translationRevision(settings), "environment": core.Document{"keySet": s.translationKey(d, settings) != ""}, "runtime": core.Document{"state": runtimeState, "reason": reason}}
	core.Object(state["gateway"])["url"] = s.Endpoint.URL + "/v1"
	if s.NativeState != nil {
		for k, v := range s.NativeState() {
			state[k] = v
		}
	}
	return state, nil
}

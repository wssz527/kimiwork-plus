package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
	"kimiworkplus.local/migration/internal/platform"
)

type Native struct {
	mu         sync.Mutex
	service    *Service
	controller *kimi.Controller
	process    core.Document
	contract   *kimi.ContractResult
	operation  core.Document
	jobs       chan struct{}
	policies   *nativePolicies
	web        *webService
	mcp        *kimi.MCPManager
}

func AttachNative(ctx context.Context, s *Service, gateway *Runtime) (*Native, error) {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	installs := kimi.Detect(runtime.GOOS, env)
	install := kimi.Installation{}
	if len(installs) > 0 {
		install = installs[0]
	}
	controller, e := kimi.NewController(kimi.ControllerSettings{Installation: install, StateDir: s.StateDir, UserDataDir: filepath.Join(s.StateDir, "kimi-profile"), GatewayURL: s.Endpoint.URL + "/v1", GatewayToken: gateway.Token()})
	if e != nil {
		return nil, e
	}
	policies, e := newNativePolicies(s.StateDir)
	if e != nil {
		return nil, e
	}
	n := &Native{service: s, controller: controller, policies: policies, process: core.Document{"running": nil, "state": "unknown"}, jobs: make(chan struct{}, 2)}
	controller.SetTranslationHandler(n.translate)
	d, _, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	if e = n.Configure(d); e != nil {
		return nil, e
	}
	s.OnConfig = func(d core.Document) error {
		if e := gateway.Apply(d); e != nil {
			return e
		}
		return n.Configure(d)
	}
	s.NativeState = n.State
	s.Extra = func(ctx context.Context, path string, in core.Document) (any, error) {
		switch path {
		case "/api/actions":
			return n.Action(ctx, in)
		case "/api/context":
			return s.saveNativeContext(n.policies, in)
		case "/api/tools":
			return n.toolsAction(ctx, in)
		case "/api/web-tools":
			return n.webAction(in)
		}
		return gateway.Action(ctx, path, in)
	}
	go n.watch(ctx)
	return n, nil
}
func (n *Native) Configure(d core.Document) error {
	raw, _ := json.Marshal(d["models"])
	var models []kimi.Model
	if e := json.Unmarshal(raw, &models); e != nil {
		return e
	}
	translation, e := core.TranslationSettings(d["translation"])
	if e != nil {
		return e
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	settings := n.controller.Settings()
	settings.Models = models
	settings.Translation = kimi.TranslationSettings{Enabled: translation.Enabled, Provider: translation.Provider, TargetLanguage: translation.TargetLanguage, View: translation.View}
	if e := n.controller.Configure(settings); e != nil {
		return e
	}
	if n.web == nil {
		if e := n.initEnhancements(); e != nil {
			return e
		}
	}
	return n.policies.Configure(d)
}
func (n *Native) watch(ctx context.Context) {
	defer n.web.Close()
	defer n.mcp.Close(context.Background())
	native := n.controller.NativeState(ctx)
	n.mu.Lock()
	n.contract = native.Contract
	n.process = processView(native.Application)
	n.mu.Unlock()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var configObserved os.FileInfo
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.mu.Lock()
			settings := n.controller.Settings()
			if settings.Installation.Application != "" {
				pids, known, reason := kimi.WithIdentityTimeout(settings.Installation.Application)
				if !known {
					n.process = core.Document{"running": nil, "state": "unknown", "reason": reason}
				} else {
					n.process = core.Document{"running": len(pids) > 0, "state": "checked"}
				}
			}
			n.controller.Tick(ctx)
			n.controller.ReconcileWithReadback(ctx)
			n.mu.Unlock()
			configObserved = n.reconcile(configObserved)
		}
	}
}
func processView(state kimi.RunningState) core.Document {
	if state == kimi.Running {
		return core.Document{"running": true, "state": "checked"}
	}
	if state == kimi.NotRunning {
		return core.Document{"running": false, "state": "checked"}
	}
	return core.Document{"running": nil, "state": "unknown"}
}
func (n *Native) State() core.Document {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := n.controller.Status(context.Background())
	process := core.Document{}
	for k, v := range n.process {
		process[k] = v
	}
	desktop := core.Document{"state": string(status.State), "reason": status.Reason, "managed": status.ManagedPID > 0}
	integration := core.Document{"state": "unchecked", "imported": false, "desktop": desktop, "reason": "native_import_not_verified"}
	if status.Integration != nil {
		raw, _ := json.Marshal(status.Integration)
		_ = json.Unmarshal(raw, &integration)
		integration["desktop"] = desktop
	}
	var operation core.Document
	if n.operation != nil {
		operation = core.Document{}
		for k, v := range n.operation {
			operation[k] = v
		}
	}
	out := core.Document{"kimiProcess": process, "integration": integration, "profile": n.controller.ProfileStatus(), "nativeTransport": kimi.ManagedTransportInfo(), "operation": operation, "diagnostics": core.Document{"contract": n.contract, "nativeComponents": platform.Components()}}
	if d, _, e := n.service.Store.Read(); e == nil {
		for k, v := range n.policies.State(d) {
			out[k] = v
		}
	}
	out["tools"] = n.toolsState(context.Background())
	out["webTools"] = n.web.State(core.Object(out["search"]))
	if status.Translation != nil {
		d, _, e := n.service.Store.Read()
		if e == nil {
			c, e := core.TranslationSettings(d["translation"])
			if e == nil {
				out["translation"] = core.Document{"configured": c, "revision": translationRevision(c), "environment": core.Document{"keySet": n.service.translationKey(d, c) != ""}, "runtime": status.Translation}
			}
		}
	}
	return out
}
func (n *Native) Action(ctx context.Context, in core.Document) (any, error) {
	action := core.String(in["action"])
	switch action {
	case "launch-kimi", "connect-kimi", "prepare-native", "check-translation", "apply-connections":
	default:
		return nil, errors.New("native_preview_unavailable")
	}
	n.mu.Lock()
	if n.operation != nil && n.operation["state"] == "running" {
		n.mu.Unlock()
		return nil, errors.New("operation_busy")
	}
	id := RandomKey()[:16]
	n.operation = core.Document{"id": id, "action": action, "state": "running", "startedAt": time.Now().UnixMilli()}
	initial := core.Document{"id": id, "action": action, "state": "running"}
	n.mu.Unlock()
	go func() {
		deadline, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		var err error
		state := "done"
		switch action {
		case "launch-kimi":
			err = n.controller.Start(deadline)
			state = "starting"
		case "connect-kimi", "prepare-native":
			var result kimi.ImportResult
			result, err = n.controller.ImportModelsAuthorized(deadline)
			if err == nil {
				err = importResultError(result)
			}
			state = result.State
			if state == "" || state == "accepted" || state == "applied" {
				state = "prepared"
			}
			n.operation["result"] = result
		case "apply-connections":
			n.service.mutation.Lock()
			d, _, e := n.service.Store.Read()
			err = e
			if e == nil {
				err = n.service.OnConfig(d)
			}
			n.service.mutation.Unlock()
		case "check-translation":
			d, _, e := n.service.Store.Read()
			err = e
			if e == nil {
				_, err = n.service.translationAction(deadline, "/api/translation/preview", core.Document{"settings": d["translation"]})
			}
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		n.operation["finishedAt"] = time.Now().UnixMilli()
		if errors.Is(err, kimi.ErrUnmanagedKimiRunning) {
			state = "waiting-kimi-exit"
		} else if err != nil {
			state = "failed"
		}
		n.operation["state"] = state
		if err != nil {
			n.operation["reason"] = core.ErrorCode(err)
		}
	}()
	return core.Document{"operation": initial}, nil
}
func (n *Native) translate(ctx context.Context, reply kimi.TranslationReply) (any, error) {
	fail := func(code string) (any, error) { return core.Document{"error": code}, nil }
	input := core.Object(reply.Value)
	text, ok := input["text"].(string)
	if !ok || text == "" || len(text) > 524288 {
		return fail("translation_input_invalid")
	}
	protected, ok := input["protectedTexts"].([]any)
	if !ok || len(protected) > 1024 {
		return fail("translation_input_invalid")
	}
	pieces := []string{}
	total := 0
	for _, v := range protected {
		s, ok := v.(string)
		if !ok || len(s) > 524288 {
			return fail("translation_input_invalid")
		}
		total += len(s)
		if total > 1048576 {
			return fail("translation_input_invalid")
		}
		pieces = append(pieces, s)
	}
	select {
	case n.jobs <- struct{}{}:
		defer func() { <-n.jobs }()
	default:
		return fail("translation_busy")
	}
	d, _, e := n.service.Store.Read()
	if e != nil {
		return fail("translation_invalid_config")
	}
	c, e := core.TranslationSettings(d["translation"])
	if e != nil {
		return fail("translation_invalid_config")
	}
	if !c.Enabled {
		return fail("translation_disabled")
	}
	translated, e := core.NewTranslator().Translate(ctx, text, c, n.service.translationKey(d, c), pieces)
	if e != nil {
		return fail(core.ErrorCode(e))
	}
	return core.Document{"text": translated}, nil
}

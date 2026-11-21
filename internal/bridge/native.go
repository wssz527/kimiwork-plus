package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

type Native struct {
	mu                  sync.Mutex
	compatMu            sync.Mutex
	profileMu           sync.Mutex
	profileReason       string
	service             *Service
	controller          *kimi.Controller
	process             core.Document
	contract            *kimi.ContractResult
	compatibility       kimi.CompatibilityReport
	compatFingerprint   kimi.OfficialFingerprint
	compatLiveProbedPID int
	operation           core.Document
	jobs                chan struct{}
	policies            *nativePolicies
	web                 *webService
	webKeys             *webKey
	mcp                 *kimi.MCPManager
	dns                 byokDNS
	probe               *byokdns.Probe
	admin               byokdns.Administrator
	dnsReason           string
	resolverPath        string
	// hostsPath overrides the static hosts file a Direct Mode prepare writes. It
	// exists so a test never writes the machine's real /etc/hosts.
	hostsPath         string
	workCapabilities  kimi.WorkCapabilities
	quota             *nativeQuota
	accounts          *accountManager
	accountActivation core.Document
	accountReason     string
	accountSyncID     string
	accountSyncAt     time.Time
	swarm             kimi.SwarmSnapshot
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
	profile, e := kimi.OfficialProfileDirectory()
	if e != nil {
		return nil, e
	}
	controller, e := kimi.NewController(kimi.ControllerSettings{Installation: install, StateDir: s.StateDir, UserDataDir: profile, LoginDataDir: profile, ProfileMode: kimi.ProfileModeOfficial, GatewayURL: s.Endpoint.URL + "/v1", GatewayToken: gateway.Token()})
	if e != nil {
		return nil, e
	}
	home, e := controller.WorkHome()
	if e != nil {
		return nil, e
	}
	policies, e := newNativePolicies(s.StateDir, home)
	if e != nil {
		return nil, e
	}
	capabilities := kimi.ProbeWorkCapabilities(install)
	policies.capabilities = capabilities.Subagents
	n := &Native{service: s, controller: controller, policies: policies, process: core.Document{"running": nil, "state": "unknown"}, compatibility: initialCompatibilityReport(), jobs: make(chan struct{}, 2), workCapabilities: capabilities}
	n.quota = newNativeQuota(s, controller.KimiCodeKey, kimi.LocalKimiCodeKey)
	if err := n.initAccounts(gateway); err != nil {
		n.accountReason = core.ErrorCode(err)
	}
	n.initBYOK()
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
		case "/api/quota/code", "/api/quota/magpie":
			return n.quota.Action(ctx, path, in)
		case "/api/accounts":
			return core.Document{"accounts": n.accountsState()}, nil
		case "/api/work/swarm":
			return n.readSwarm(ctx)
		}
		return gateway.Action(ctx, path, in)
	}
	go n.watch(ctx)
	return n, nil
}
func (n *Native) Configure(d core.Document) error {
	if e := n.syncWorkHome(); e != nil {
		return e
	}
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
	mode := core.IntegrationMode(d)
	settings.IntegrationMode = mode
	if mode == core.IntegrationModeDirect {
		settings.DirectSpec = buildDirectSpec(d, n.credentialAvailability(d))
	} else {
		settings.DirectSpec = nil
	}
	settings.Translation = kimi.TranslationSettings{Enabled: translation.Enabled, Provider: translation.Provider, TargetLanguage: translation.TargetLanguage, View: translation.View}
	if e := n.controller.Configure(settings); e != nil {
		return e
	}
	if runtime.GOOS == "darwin" && mode == core.IntegrationModeDirect && n.dns != nil && n.directDNSCanStop() {
		_ = n.dns.Close()
	}
	if n.web == nil {
		if e := n.initEnhancements(); e != nil {
			return e
		}
	}
	return n.policies.Configure(d)
}
func (n *Native) watch(ctx context.Context) {
	defer n.closeBYOK()
	defer n.web.Close()
	defer func() {
		n.profileMu.Lock()
		defer n.profileMu.Unlock()
		_ = n.mcp.Close(context.Background())
	}()
	native := n.controller.NativeState(ctx)
	n.mu.Lock()
	n.contract = native.Contract
	n.process = processView(native.Application)
	n.mu.Unlock()
	n.runCompatibilityCheck(ctx, native.Contract, false)
	if !n.modelSyncWriteBlocked() {
		// An installation upgraded from the Node prototype carries dead
		// `byok-kwp-*` providers that no store owns, so nothing else will ever
		// remove them.
		n.cleanupOrphanedLegacy()
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var configObserved os.FileInfo
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.maybeRecheckCompatibility(ctx)
			modelBlocked := n.modelSyncWriteBlocked()
			accountBlocked := n.accountSwitchWriteBlocked()
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
			var integration kimi.IntegrationResult
			if !modelBlocked {
				integration = n.controller.ReconcileWithReadback(ctx)
			}
			n.mu.Unlock()
			if !modelBlocked {
				n.controller.RepairIntegration(ctx, integration)
			}
			if err := n.syncWorkHome(); err == nil {
				configObserved = n.reconcile(configObserved)
			}
			if !accountBlocked {
				n.observeAccounts(ctx)
			}
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
func (n *Native) UpdateIdleCheck(gateway *Runtime) (bool, string) {
	if gateway != nil && gateway.ActiveRequests() > 0 {
		return false, "网关仍有正在传输的模型请求，等待完成后再更新。"
	}
	if n == nil {
		return true, ""
	}
	n.mu.Lock()
	operating := n.operation != nil && n.operation["state"] == "running"
	status := n.controller.Status(context.Background())
	n.mu.Unlock()
	if operating {
		return false, "后台正在执行 Kimi 接入或维护操作，等待完成后再更新。"
	}
	if status.ManagedPID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		busy, err := n.controller.AccountWorkBusy(ctx)
		cancel()
		if err == nil && busy {
			return false, "Kimi Work 仍有正在进行的任务，等待任务结束后再替换。"
		}
	}
	return true, ""
}

func refreshKimiInstallation(settings kimi.ControllerSettings, discover func() []kimi.Installation) kimi.ControllerSettings {
	if settings.Installation.Application != "" {
		return settings
	}
	if installs := discover(); len(installs) > 0 {
		settings.Installation = installs[0]
	}
	return settings
}

func nativeDesktopState(status kimi.ControllerStatus) core.Document {
	desktop := core.Document{"state": string(status.State), "reason": status.Reason, "managed": status.ManagedPID > 0}
	if status.Native != nil && status.Reason == "" {
		desktop["reason"] = status.Native.Reason
	}
	return desktop
}

func settledNativeImportReceipt(current *kimi.IntegrationResult, receipt kimi.ImportResult) kimi.ImportResult {
	if current == nil {
		return receipt
	}
	if current.Applied && !current.RouteCleanupPending {
		receipt.RouteCleanupPending = false
		if receipt.Reason == "route_cleanup_pending" {
			receipt.Reason = ""
		}
	}
	if !current.Adopted {
		return receipt
	}
	receipt.RequiresRestart = false
	receipt.RestartDeferred = false
	receipt.Applied = current.Applied
	receipt.Adopted = true
	if receipt.State == kimi.ImportStateRequiresRestart {
		receipt.State = kimi.ImportStateApplied
		if receipt.Reason == kimi.NativeRequiresRestart {
			receipt.Reason = ""
		}
	}
	if receipt.RouteCleanupPending {
		receipt.State = kimi.ImportStateAccepted
		receipt.Reason = "route_cleanup_pending"
	}
	return receipt
}

func (n *Native) integrationForState(current *kimi.IntegrationResult) *kimi.IntegrationResult {
	if current == nil || current.Mode != kimi.IntegrationModeDirect || !current.Applied {
		return current
	}
	home, err := n.controller.WorkHome()
	if err != nil {
		return current
	}
	updated := *current
	updated.RouteCleanupPending = kimi.DirectGatewayRemains(home, kimi.AccountScope(home), n.controller.Settings().GatewayURL)
	return &updated
}

func (n *Native) State() core.Document {
	n.mu.Lock()
	status := n.controller.Status(context.Background())
	currentIntegration := n.integrationForState(status.Integration)
	process := core.Document{}
	for k, v := range n.process {
		process[k] = v
	}
	desktop := nativeDesktopState(status)
	integration := core.Document{"state": "unchecked", "imported": false, "desktop": desktop, "reason": "native_import_not_verified"}
	if currentIntegration != nil {
		raw, _ := json.Marshal(currentIntegration)
		var verified core.Document
		if json.Unmarshal(raw, &verified) == nil && verified != nil {
			integration = verified
			integration["desktop"] = desktop
		}
	}
	var operation core.Document
	if n.operation != nil {
		operation = core.Document{}
		for k, v := range n.operation {
			operation[k] = v
		}
		var receipt kimi.ImportResult
		switch value := operation["result"].(type) {
		case kimi.ImportResult:
			receipt = value
		case *kimi.ImportResult:
			if value != nil {
				receipt = *value
			}
		}
		if receipt.State != "" || receipt.Reason != "" || receipt.OK {
			raw, _ := json.Marshal(settledNativeImportReceipt(currentIntegration, receipt))
			var published core.Document
			if json.Unmarshal(raw, &published) == nil {
				integration["nativeImport"] = published
			}
		}
	}
	out := core.Document{"kimiProcess": process, "integration": integration, "profile": n.controller.ProfileStatus(), "nativeTransport": kimi.ManagedTransportInfo(), "operation": operation, "compatibility": n.compatibilitySnapshot(), "diagnostics": core.Document{"contract": n.contract, "nativeComponents": platform.Components(), "profileReason": n.profileReason}}
	out["workCapabilities"] = n.workCapabilities
	if n.swarm.State == "" {
		out["swarm"] = kimi.SwarmSnapshot{State: kimi.SwarmStateUnknown, Reason: "work_swarm_not_checked"}
	} else {
		out["swarm"] = n.swarm
	}
	n.mu.Unlock()
	if n.quota != nil {
		for key, value := range n.quota.State() {
			out[key] = value
		}
	}
	out["accounts"] = n.accountsState()
	out["setup"] = n.byokState()
	if d, _, e := n.service.Store.Read(); e == nil {
		policies := n.policies.State(d)
		for k, v := range policies {
			out[k] = v
		}
		out["directMode"] = n.directModeState(d, integration, core.Object(policies["search"]))
	}
	out["tools"] = n.toolsState(context.Background())
	out["webTools"] = n.web.State(core.Object(out["search"]))
	if status.Translation != nil {
		d, _, e := n.service.Store.Read()
		if e == nil {
			c, e := core.TranslationSettings(d["translation"])
			if e == nil {
				out["translation"] = core.Document{"configured": translationPublicConfig(c), "revision": translationRevision(c), "environment": core.Document{"keySet": n.service.translationKey(d, c) != ""}, "runtime": status.Translation}
			}
		}
	}
	return out
}
func (n *Native) Action(ctx context.Context, in core.Document) (any, error) {
	action := core.String(in["action"])
	profileMode := ""
	if value, present := in["profileMode"]; present {
		mode, valid := value.(string)
		if !valid || action != "launch-kimi" || (mode != kimi.ProfileModeOfficial && mode != kimi.ProfileModeExistingAuthorized) {
			return nil, errors.New("invalid_profile_mode")
		}
		profileMode = mode
	}
	var swarmKey string
	var swarmEnabled bool
	if action == "set-swarm" {
		var ok bool
		if swarmEnabled, ok = in["enabled"].(bool); !ok {
			return nil, errors.New("invalid_swarm_mode")
		}
		swarmKey, _ = in["conversationKey"].(string)
		if !kimi.ValidSwarmConversationKey(swarmKey) {
			return nil, errors.New("invalid_swarm_conversation")
		}
	}
	if action == "check-upstream" {
		connectionID := core.String(in["connectionId"])
		if connectionID == "" {
			return nil, errors.New("connection_unavailable")
		}
		checked, err := n.service.discover(ctx, core.Document{"connectionId": connectionID})
		if err != nil {
			return nil, err
		}
		result := checked.(core.Document)
		operation := core.Document{"id": core.String(in["id"]), "action": action, "connectionId": connectionID, "state": result["state"]}
		for _, key := range []string{"reason", "httpStatus", "modelCount"} {
			if value, ok := result[key]; ok {
				operation[key] = value
			}
		}
		return core.Document{"operation": operation}, nil
	}
	switch action {
	case "launch-kimi", "open-work", "restart-kimi", "connect-kimi", "prepare-native", "remove-dns", "check-translation", "check-compatibility", "apply-connections", "account-capture", "account-begin-add", "account-complete-add", "account-cancel-add", "account-switch", "account-remove", "set-swarm":
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
		timeout := 45 * time.Second
		if action == "prepare-native" || action == "remove-dns" {
			timeout = 180 * time.Second
		} else if action == "connect-kimi" && n.isDirectMode() {
			// A direct import settles the static hosts entry first, and that can
			// need one administrator prompt; the gateway path never does.
			timeout = 180 * time.Second
		} else if strings.HasPrefix(action, "account-") {
			timeout = 90 * time.Second
		}
		deadline, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var err error
		var result any
		state := "done"
		switch action {
		case "check-compatibility":
			report := n.runCompatibilityCheck(deadline, nil, true)
			result = core.Document{"compatibility": report}
		case "set-swarm":
			result, err = n.setSwarm(deadline, swarmKey, swarmEnabled)
		case "account-capture", "account-begin-add", "account-complete-add", "account-cancel-add", "account-switch", "account-remove":
			if action != "account-remove" && n.accountSwitchWriteBlocked() {
				err = errors.New(kimi.CompatErrAccountSwitchBlocked)
				break
			}
			if n.accounts == nil {
				err = errors.New("account_switch_unavailable")
				break
			}
			result, err = n.accounts.Action(deadline, in)
			if err == nil {
				if action == "account-remove" || action == "account-capture" || action == "account-complete-add" {
					// A removal or a fresh capture resolves whatever the previous
					// activation reported. Keeping it would leave the overview page
					// showing a failure or a rollback that no longer describes anything.
					n.clearStaleAccountActivation()
				}
				n.refreshAccountQuota(deadline)
			}
		case "launch-kimi":
			settings := n.controller.Settings()
			refreshed := refreshKimiInstallation(settings, func() []kimi.Installation { return kimi.Detect(runtime.GOOS, currentEnvMap()) })
			if refreshed.Installation != settings.Installation {
				err = n.controller.Configure(refreshed)
			}
			if err == nil {
				if profileMode == kimi.ProfileModeExistingAuthorized {
					err = n.controller.StartWithExistingLogin(deadline)
				} else {
					err = n.controller.StartOfficial(deadline)
				}
			}
			if err == nil {
				err = n.syncWorkHome()
			}
		case "open-work":
			err = n.controller.OpenWork(deadline)
		case "restart-kimi":
			err = n.controller.RestartOfficial(deadline)
			if err == nil {
				err = n.syncWorkHome()
			}
		case "connect-kimi":
			if n.modelSyncWriteBlocked() {
				err = errors.New(kimi.CompatErrModelSyncBlocked)
				break
			}
			n.cleanupOrphanedLegacy()
			var imported kimi.ImportResult
			if n.isDirectMode() {
				// The import needs the intranet name to resolve without this
				// product's listener, so the static entry is settled first. A
				// machine that already has it costs no administrator prompt.
				if err = n.prepareBYOK(deadline); err == nil {
					imported, err = n.importDirect(deadline)
				}
			} else {
				imported, err = n.controller.ImportModelsAuthorized(deadline)
			}
			if err == nil {
				err = importResultError(imported)
			}
			state = imported.State
			if state == "" || state == "accepted" || state == "applied" {
				state = "prepared"
			}
			result = imported
		case "prepare-native":
			if n.modelSyncWriteBlocked() {
				err = errors.New(kimi.CompatErrModelSyncBlocked)
				break
			}
			n.cleanupOrphanedLegacy()
			err = n.prepareBYOK(deadline)
			var imported *kimi.ImportResult
			if err == nil {
				var value kimi.ImportResult
				var importErr error
				if n.isDirectMode() {
					value, importErr = n.importDirect(deadline)
				} else {
					value, importErr = n.controller.ImportModelsAuthorized(deadline)
				}
				imported = &value
				err = importErr
			}
			if imported != nil {
				if err == nil {
					err = importResultError(*imported)
				}
				state = imported.State
				if state == "" || state == "accepted" || state == "applied" {
					state = "prepared"
				}
				result = *imported
			}
		case "remove-dns":
			err = n.removeBYOK(deadline)
			state = "removed"
		case "apply-connections":
			n.service.mutation.Lock()
			d, _, e := n.service.Store.Read()
			err = e
			if err == nil {
				err = n.service.authorizeConnectionCredentials(d)
			}
			if err == nil {
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
		n.finishOperation(id, state, result, err)
	}()
	return core.Document{"operation": initial}, nil
}

// importDirect runs one Direct Mode import. The request holds the provider keys,
// so it stays local to this call: it is never stored on the operation, in a
// state file or in the settings.
func (n *Native) importDirect(ctx context.Context) (kimi.ImportResult, error) {
	request, err := n.directImportFor()
	if err != nil {
		return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: core.ErrorCode(err)}, err
	}
	result, err := n.controller.ImportDirectAuthorized(ctx, request)
	request = kimi.DirectImportRequest{}
	return result, err
}

func (n *Native) finishOperation(id, state string, result any, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.operation == nil || n.operation["id"] != id {
		return
	}
	n.operation["finishedAt"] = time.Now().UnixMilli()
	if errors.Is(err, kimi.ErrUnmanagedKimiRunning) {
		state = "waiting-kimi-exit"
	} else if err != nil {
		state = "failed"
	}
	n.operation["state"] = state
	if result != nil {
		n.operation["result"] = result
	}
	if err != nil {
		n.operation["reason"] = core.ErrorCode(err)
	}
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

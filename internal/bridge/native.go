package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/platform"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
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
	receipt             *nativeImportReceipt
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
	hostsPath          string
	workCapabilities   kimi.WorkCapabilities
	quota              *nativeQuota
	accounts           *accountManager
	accountActivation  core.Document
	accountReason      string
	accountSyncID      string
	accountSyncAt      time.Time
	swarm              kimi.SwarmSnapshot
	workUsage          *workUsageCollector
	diagnostics        *diagnosticsLog
	autoLaunch         func(context.Context) error
	autoOpenWork       func(context.Context) error
	importModels       func(context.Context) (kimi.ImportResult, error)
	importDirectModels func(context.Context, kimi.DirectImportRequest) (kimi.ImportResult, error)
	disconnectModels   func(context.Context) (kimi.DisconnectResult, error)
	// The probes below exist so a test can place the process and the readback
	// without starting or touching a real Kimi.
	managedProbe      func() bool
	runningProbe      func() bool
	staticHostsProbe  func() bool
	autoSyncReconcile func(context.Context) kimi.IntegrationResult
	autoSync          *autoSyncCoordinator
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
	n.autoLaunch = n.startManagedOfficial
	n.autoOpenWork = controller.OpenWork
	n.diagnostics = newDiagnosticsLog(s.StateDir)
	n.workUsage = newWorkUsageCollector(s.StateDir, home, gateway.usage, s.Store, gateway.budget)
	n.workUsage.Sync()
	n.quota = newNativeQuota(s, controller.KimiCodeKey, kimi.LocalKimiCodeKey)
	if err := n.initAccounts(gateway); err != nil {
		n.accountReason = core.ErrorCode(err)
	}
	n.initBYOK()
	n.autoSync = newAutoSyncCoordinator(ctx, n)
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
	s.ScheduleAutoSync = n.ScheduleAutoSync
	s.autoSync = n.autoSync
	s.Extra = func(ctx context.Context, path string, in core.Document) (any, error) {
		switch path {
		case "/api/actions":
			return n.Action(ctx, in)
		case "/api/diagnostics/export":
			return n.diagnosticsExport(), nil
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
		case "/api/work/conversations":
			return n.readWorkConversations(ctx)
		case "/api/work/conversations/rebind":
			return n.rebindWorkConversations(ctx, in)
		case "/api/usage/query", "/api/usage/export":
			if n.workUsage != nil {
				n.workUsage.Sync()
			}
		case "/api/usage/clear":
			res, err := gateway.Action(ctx, path, in)
			if err == nil && n.workUsage != nil {
				if cr, ok := res.(ledger.ClearResult); ok && cr.OK {
					n.workUsage.MarkCleared(in["before"], core.String(in["timeZone"]))
				}
			}
			return res, err
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
	if core.KimiK3Compatibility(d) {
		for i := range models {
			models[i].CatalogID = protocol.KimiCompatibleModelID(models[i].ID)
		}
	}
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
	defer n.autoSync.stop()
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
			configObserved = n.watchStep(ctx, configObserved)
		}
	}
}

func (n *Native) watchStep(ctx context.Context, configObserved os.FileInfo) os.FileInfo {
	n.maybeRecheckCompatibility(ctx)
	modelBlocked := n.modelSyncWriteBlocked()
	accountBlocked := n.accountSwitchWriteBlocked()
	n.mu.Lock()
	settings := n.controller.Settings()
	n.mu.Unlock()

	var checkedProcess core.Document
	if settings.Installation.Application != "" {
		pids, known, reason := kimi.WithIdentityTimeout(settings.Installation.Application)
		if !known {
			checkedProcess = core.Document{"running": nil, "state": "unknown", "reason": reason}
		} else {
			checkedProcess = core.Document{"running": len(pids) > 0, "state": "checked"}
		}
	}
	if checkedProcess != nil {
		n.mu.Lock()
		n.process = checkedProcess
		n.mu.Unlock()
	}

	n.controller.Tick(ctx)
	if !modelBlocked {
		integration := n.controller.ReconcileWithReadback(ctx)
		n.controller.RepairIntegration(ctx, integration)
		if d, _, err := n.service.Store.Read(); err == nil {
			n.autoSync.observe(&integration, d)
		}
		n.autoSync.kickIfReady()
	}
	if err := n.syncWorkHome(); err == nil {
		configObserved = n.reconcile(configObserved)
	}
	if n.workUsage != nil {
		n.workUsage.Sync()
	}
	if !accountBlocked {
		n.observeAccounts(ctx)
	}
	return configObserved
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

// settledImportReason reports whether a reason only says the import is still
// waiting for its evidence on disk. A readback that holds and has adopted the
// import resolves exactly these.
func settledImportReason(reason string) bool {
	switch reason {
	case "", "route_cleanup_pending", "native_import_not_verified", kimi.NativeRequiresRestart:
		return true
	}
	return false
}

// settledNativeImportReceipt reads a receipt against what the profile now holds.
// The receipt is the newer evidence only for the parts a readback cannot answer:
// a refused import is never promoted, and an accepted one is only settled when
// the readback both holds it and the running application loaded it.
func settledNativeImportReceipt(current *kimi.IntegrationResult, receipt kimi.ImportResult) kimi.ImportResult {
	if current == nil {
		return receipt
	}
	// Whether the previous source is still connected is a readback fact, so it is
	// settled for every receipt. It never says the import itself succeeded.
	if current.Applied && !current.RouteCleanupPending {
		receipt.RouteCleanupPending = false
		if receipt.Reason == "route_cleanup_pending" {
			receipt.Reason = ""
		}
	}
	if !receipt.OK || (receipt.State != kimi.ImportStateAccepted && receipt.State != kimi.ImportStateRequiresRestart && receipt.State != kimi.ImportStateApplied) {
		// A refused or account-mismatched import says nothing about what is on
		// disk. An adopted readback must not report it as an applied import.
		return receipt
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
	if receipt.State == kimi.ImportStateAccepted && current.Applied && !current.RouteCleanupPending {
		// The page accepted the import and the readback has held it since. The
		// receipt describes a settled import, not one still waiting on disk.
		receipt.State = kimi.ImportStateApplied
		if settledImportReason(receipt.Reason) {
			receipt.Reason = ""
		}
	}
	if receipt.RouteCleanupPending {
		receipt.State = kimi.ImportStateAccepted
		receipt.Reason = "route_cleanup_pending"
	}
	return receipt
}

// nativeImportReceipt is the last import answer, kept next to the operation that
// produced it. Native.operation only holds the newest operation of any kind, so
// a compatibility check or an open-work click would otherwise drop a restart or
// a route cleanup the import still needs.
type nativeImportReceipt struct {
	action  string
	mode    string
	account string
	result  kimi.ImportResult
}

// nativeImportAction reports whether action runs a model import. Only those
// operations produce a receipt.
func nativeImportAction(action string) bool {
	return action == "connect-kimi" || action == "prepare-native" || action == "auto-sync"
}

func importResultValue(value any) (kimi.ImportResult, bool) {
	switch typed := value.(type) {
	case kimi.ImportResult:
		return typed, true
	case *kimi.ImportResult:
		if typed != nil {
			return *typed, true
		}
	}
	return kimi.ImportResult{}, false
}

func importReceiptPresent(receipt kimi.ImportResult) bool {
	return receipt.OK || receipt.State != "" || receipt.Reason != ""
}

func receiptIntegrationMode(current *kimi.IntegrationResult) string {
	if current != nil && current.Mode == kimi.IntegrationModeDirect {
		return kimi.IntegrationModeDirect
	}
	return kimi.IntegrationModeGateway
}

// rememberImportReceipt keeps the answer to the import that just finished. It
// takes Native.mu itself; finishOperation uses the locked variant so the receipt
// is recorded together with the operation that produced it.
func (n *Native) rememberImportReceipt(action string, result any) {
	n.mu.Lock()
	n.rememberImportReceiptLocked(action, result)
	n.mu.Unlock()
}

func (n *Native) rememberImportReceiptLocked(action string, result any) {
	if !nativeImportAction(action) {
		return
	}
	receipt, ok := importResultValue(result)
	if !ok || !importReceiptPresent(receipt) {
		return
	}
	record := &nativeImportReceipt{action: action, mode: kimi.IntegrationModeGateway, result: receipt}
	if n.isDirectMode() {
		record.mode = kimi.IntegrationModeDirect
	}
	if receipt.Readback != nil {
		record.account = receipt.Readback.AccountDigest
	}
	n.receipt = record
}

// publishedNativeImport is the receipt State publishes. It is settled against
// the current readback every time it is read, so a deferred restart turns into
// `applied` once adoption is observed, and so does an accepted import whose
// files are now read back. A receipt recorded for another account, or for the
// other integration mode, never describes the current profile; a route the mode
// switch left to clean up is the one piece of that receipt that still applies.
//
// It reads Native.operation and Native.receipt, so callers hold Native.mu.
func (n *Native) publishedNativeImport(current *kimi.IntegrationResult) (kimi.ImportResult, bool) {
	record := n.receipt
	if record == nil {
		// No import ran yet: publish the current operation's own answer if it is
		// one, which is everything an earlier build published.
		if n.operation == nil || !nativeImportAction(core.String(n.operation["action"])) {
			return kimi.ImportResult{}, false
		}
		receipt, ok := importResultValue(n.operation["result"])
		if !ok || !importReceiptPresent(receipt) {
			return kimi.ImportResult{}, false
		}
		return settledNativeImportReceipt(current, receipt), true
	}
	if current == nil {
		// Nothing is known about the profile yet, so there is nothing to settle
		// the receipt against.
		return settledNativeImportReceipt(nil, record.result), true
	}
	if record.account != "" && !sameIntegrationAccount(record.account, current) {
		// Another account is signed in: the old profile's import is not evidence
		// about this one, including unfinished cleanup.
		return kimi.ImportResult{}, false
	}
	if record.mode != receiptIntegrationMode(current) {
		if record.result.OK && (record.result.RouteCleanupPending || record.result.Reason == "route_cleanup_pending") {
			// Switching modes leaves the previous source to disconnect, and the
			// selected mode's readback cannot settle that. The restart request
			// does not carry over.
			return kimi.ImportResult{OK: record.result.OK, State: kimi.ImportStateAccepted,
				Reason: "route_cleanup_pending", RouteCleanupPending: true}, true
		}
		return kimi.ImportResult{}, false
	}
	return settledNativeImportReceipt(current, record.result), true
}

func sameIntegrationAccount(receiptAccount string, current *kimi.IntegrationResult) bool {
	if current == nil || receiptAccount == "" {
		return true
	}
	if current.Readback != nil && current.Readback.AccountDigest != "" {
		return receiptAccount == current.Readback.AccountDigest
	}
	if current.AccountDigest == "" {
		return true
	}
	if receiptAccount == current.AccountDigest {
		return true
	}
	sum := sha256.Sum256([]byte(receiptAccount))
	return hex.EncodeToString(sum[:]) == current.AccountDigest
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
	// The log summary may read history from disk on first use; it must happen
	// before Native.mu is held.
	operationLog := n.diagnostics.summary()
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
	}
	integration["autoSync"] = n.autoSync.status()
	if receipt, ok := n.publishedNativeImport(currentIntegration); ok {
		raw, _ := json.Marshal(receipt)
		var published core.Document
		if json.Unmarshal(raw, &published) == nil {
			integration["nativeImport"] = published
		}
	}
	out := core.Document{"kimiProcess": process, "integration": integration, "profile": n.controller.ProfileStatus(), "nativeTransport": kimi.ManagedTransportInfo(), "operation": operation, "compatibility": n.compatibilitySnapshot(), "diagnostics": core.Document{"contract": n.contract, "nativeComponents": platform.Components(), "profileReason": n.profileReason, "operationLog": operationLog}}
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
		out["kimiK3State"] = kimiK3State(d, currentIntegration, process["running"] == true)
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
			n.logOperationRejected(action, "invalid_profile_mode")
			return nil, errors.New("invalid_profile_mode")
		}
		profileMode = mode
	}
	var swarmKey string
	var swarmEnabled bool
	if action == "set-swarm" {
		var ok bool
		if swarmEnabled, ok = in["enabled"].(bool); !ok {
			n.logOperationRejected(action, "invalid_swarm_mode")
			return nil, errors.New("invalid_swarm_mode")
		}
		swarmKey, _ = in["conversationKey"].(string)
		if !kimi.ValidSwarmConversationKey(swarmKey) {
			n.logOperationRejected(action, "invalid_swarm_conversation")
			return nil, errors.New("invalid_swarm_conversation")
		}
	}
	if action == "check-upstream" {
		started := time.Now()
		connectionID := core.String(in["connectionId"])
		if connectionID == "" {
			n.logOperationRejected(action, "connection_unavailable")
			return nil, errors.New("connection_unavailable")
		}
		n.logOperationStarted("", action)
		checked, err := n.service.discover(ctx, core.Document{"connectionId": connectionID})
		if err != nil {
			n.logOperationFinished("", action, "", "failed", core.ErrorCode(err), time.Since(started).Milliseconds(), nil)
			return nil, err
		}
		result := checked.(core.Document)
		operation := core.Document{"id": core.String(in["id"]), "action": action, "connectionId": connectionID, "state": result["state"]}
		for _, key := range []string{"reason", "httpStatus", "modelCount"} {
			if value, ok := result[key]; ok {
				operation[key] = value
			}
		}
		n.logOperationFinished("", action, "", core.String(operation["state"]), core.String(operation["reason"]), time.Since(started).Milliseconds(), nil)
		return core.Document{"operation": operation}, nil
	}
	switch action {
	case "launch-kimi", "open-work", "restart-kimi", "connect-kimi", "disconnect-models", "prepare-native", "remove-dns", "check-translation", "check-compatibility", "apply-connections", "account-capture", "account-begin-add", "account-complete-add", "account-cancel-add", "account-switch", "account-remove", "set-swarm":
	default:
		n.logOperationRejected(action, "native_preview_unavailable")
		return nil, errors.New("native_preview_unavailable")
	}
	n.mu.Lock()
	if n.operation != nil && n.operation["state"] == "running" {
		n.mu.Unlock()
		n.logOperationRejected(action, "operation_busy")
		return nil, errors.New("operation_busy")
	}
	id := RandomKey()[:16]
	n.operation = core.Document{"id": id, "action": action, "state": "running", "startedAt": time.Now().UnixMilli()}
	initial := core.Document{"id": id, "action": action, "state": "running"}
	n.mu.Unlock()
	n.logOperationStarted(id, action)
	go func() {
		timeout := 45 * time.Second
		if action == "prepare-native" || action == "remove-dns" {
			timeout = 180 * time.Second
		} else if (action == "connect-kimi" && !n.isDirectMode()) || action == "disconnect-models" {
			// Auto-launching Kimi plus the 60 second bridge budget (and one
			// OpenWork retry if Kimi was hidden in the tray) must finish cleanly
			// before the outer deadline expires.
			timeout = 120 * time.Second
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
				if action == "account-switch" {
					if d, _, readErr := n.service.Store.Read(); readErr == nil {
						_ = n.policies.Configure(d)
					}
				}
			}
		case "launch-kimi":
			if profileMode == kimi.ProfileModeExistingAuthorized {
				if err = n.refreshControllerInstallation(); err == nil {
					err = n.controller.StartWithExistingLogin(deadline)
				}
				if err == nil {
					err = n.syncWorkHome()
				}
			} else {
				err = n.startManagedOfficial(deadline)
			}
		case "open-work":
			err = n.controller.OpenWork(deadline)
		case "restart-kimi":
			err = n.controller.RestartOfficial(deadline)
			if err == nil {
				err = n.syncWorkHome()
			}
		case "disconnect-models":
			n.setOperationStage(id, diagnosticStageDisconnectModels)
			var removed kimi.DisconnectResult
			removed, err = n.disconnectModelsWithAutoStart(deadline)
			if err == nil && !removed.OK {
				reason := removed.Reason
				if reason == "" {
					reason = "model_disconnect_failed"
				}
				err = errors.New(reason)
			}
			if err == nil {
				state = "removed"
				if n.policies != nil && n.service != nil {
					if d, _, readErr := n.service.Store.Read(); readErr == nil {
						_ = n.policies.Configure(d)
					}
				}
			}
			if removed.OK || removed.State != "" || removed.Reason != "" {
				result = removed
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
				n.setOperationStage(id, diagnosticStagePrepare)
				if err = n.prepareBYOK(deadline); err == nil {
					n.setOperationStage(id, diagnosticStageImportDirect)
					imported, err = n.importDirect(deadline)
				}
			} else {
				n.setOperationStage(id, diagnosticStageImportModels)
				imported, err = n.importModelsWithAutoStart(deadline)
			}
			if err == nil {
				err = importResultError(imported)
			}
			if err == nil && n.policies != nil && n.service != nil {
				if d, _, readErr := n.service.Store.Read(); readErr == nil {
					_ = n.policies.Configure(d)
				}
			}
			state = imported.State
			if state == "" || state == "accepted" || state == "applied" {
				state = "prepared"
			}
			if imported.OK || imported.State != "" || imported.Reason != "" {
				result = imported
			}
		case "prepare-native":
			if n.modelSyncWriteBlocked() {
				err = errors.New(kimi.CompatErrModelSyncBlocked)
				break
			}
			n.cleanupOrphanedLegacy()
			n.setOperationStage(id, diagnosticStagePrepare)
			err = n.prepareBYOK(deadline)
			var imported *kimi.ImportResult
			if err == nil {
				var value kimi.ImportResult
				var importErr error
				if n.isDirectMode() {
					n.setOperationStage(id, diagnosticStageImportDirect)
					value, importErr = n.importDirect(deadline)
				} else {
					n.setOperationStage(id, diagnosticStageImportModels)
					value, importErr = n.importModelsWithAutoStart(deadline)
				}
				if value.OK || value.State != "" || value.Reason != "" {
					imported = &value
				}
				err = importErr
			}
			if imported != nil {
				if err == nil {
					err = importResultError(*imported)
				}
				if err == nil && n.policies != nil && n.service != nil {
					if d, _, readErr := n.service.Store.Read(); readErr == nil {
						_ = n.policies.Configure(d)
					}
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

func (n *Native) refreshControllerInstallation() error {
	settings := n.controller.Settings()
	refreshed := refreshKimiInstallation(settings, func() []kimi.Installation { return kimi.Detect(runtime.GOOS, currentEnvMap()) })
	if refreshed.Installation != settings.Installation {
		return n.controller.Configure(refreshed)
	}
	return nil
}

func (n *Native) startManagedOfficial(ctx context.Context) error {
	if err := n.refreshControllerInstallation(); err != nil {
		return err
	}
	if err := n.controller.StartOfficial(ctx); err != nil {
		return err
	}
	return n.syncWorkHome()
}

func (n *Native) ensureManagedKimi(ctx context.Context) error {
	if n.controller == nil || n.controller.HasManagedBridge() || n.autoLaunch == nil {
		return nil
	}
	return n.autoLaunch(ctx)
}

func runWithManagedKimi[T any](
	ctx context.Context,
	ensure func(context.Context) error,
	openWork func(context.Context) error,
	run func(context.Context) (T, error),
	waitingWork func(T) bool,
) (T, error) {
	var zero T
	if err := ensure(ctx); err != nil {
		return zero, err
	}
	result, err := run(ctx)
	if err == nil && waitingWork(result) && openWork != nil {
		if openErr := openWork(ctx); openErr == nil {
			result, err = run(ctx)
		}
	}
	return result, err
}

func importWaitingWork(result kimi.ImportResult) bool {
	return !result.OK && result.State == kimi.ImportStateWaitingWork
}

func disconnectWaitingWork(result kimi.DisconnectResult) bool {
	return !result.OK && result.State == kimi.ImportStateWaitingWork
}

func (n *Native) importModelsWithAutoStart(ctx context.Context) (kimi.ImportResult, error) {
	run := n.importModels
	if run == nil {
		run = n.controller.ImportModelsAuthorized
	}
	return runWithManagedKimi(ctx, n.ensureManagedKimi, n.autoOpenWork, run, importWaitingWork)
}

func (n *Native) disconnectModelsWithAutoStart(ctx context.Context) (kimi.DisconnectResult, error) {
	run := n.disconnectModels
	if run == nil {
		run = n.controller.DisconnectModelsAuthorized
	}
	return runWithManagedKimi(ctx, n.ensureManagedKimi, n.autoOpenWork, run, disconnectWaitingWork)
}

// importDirect runs one Direct Mode import. The request holds the provider keys,
// so it stays local to this call: it is never stored on the operation, in a
// state file or in the settings.
func (n *Native) importDirect(ctx context.Context) (kimi.ImportResult, error) {
	return n.importDirectWithEnsure(ctx, n.ensureManagedKimi)
}

// importDirectWithEnsure is importDirect with the launch step left to the
// caller. A background write passes a no-op: it must never start Kimi.
func (n *Native) importDirectWithEnsure(ctx context.Context, ensure func(context.Context) error) (kimi.ImportResult, error) {
	request, err := n.directImportFor()
	if err != nil {
		return kimi.ImportResult{State: kimi.ImportStateFailed, Reason: core.ErrorCode(err)}, err
	}
	defer func() {
		request = kimi.DirectImportRequest{}
	}()
	run := n.importDirectModels
	if run == nil {
		run = n.controller.ImportDirectAuthorized
	}
	return runWithManagedKimi(ctx, ensure, n.autoOpenWork,
		func(ctx context.Context) (kimi.ImportResult, error) {
			return run(ctx, request)
		}, importWaitingWork)
}

// releaseOperation frees a claimed operation slot without publishing an outcome.
// It is the shutdown path: nothing ran, so nothing is recorded.
func (n *Native) releaseOperation(id string) {
	n.mu.Lock()
	if n.operation != nil && n.operation["id"] == id {
		n.operation = nil
	}
	n.mu.Unlock()
}

func (n *Native) finishOperation(id, state string, result any, err error) {
	finishedAt := time.Now()
	n.mu.Lock()
	if n.operation == nil || n.operation["id"] != id {
		n.mu.Unlock()
		return
	}
	n.operation["finishedAt"] = finishedAt.UnixMilli()
	if errors.Is(err, kimi.ErrUnmanagedKimiRunning) {
		state = "waiting-kimi-exit"
	} else if err != nil {
		state = "failed"
	}
	n.operation["state"] = state
	if result != nil {
		n.operation["result"] = result
	}
	reason := ""
	if err != nil {
		reason = core.ErrorCode(err)
		n.operation["reason"] = reason
	}
	action := core.String(n.operation["action"])
	if action == "disconnect-models" && err == nil {
		n.receipt = nil
	}
	stage := core.String(n.operation["stage"])
	duration := finishedAt.UnixMilli() - documentInt64(n.operation["startedAt"])
	if duration < 0 {
		duration = 0
	}
	// The receipt outlives the operation: a later open-work or compatibility check
	// replaces Native.operation but must not take a pending restart with it. It is
	// recorded under the same lock that published this operation, so a newer
	// operation cannot interleave with it.
	n.rememberImportReceiptLocked(action, result)
	n.mu.Unlock()
	// The diagnostic write happens with Native.mu released: a stalled disk must
	// not block readers of the operation state, and a failed write is ignored.
	if reason == "" {
		if imported := diagnosticImportOf(result); imported != nil {
			reason = imported.Reason
		}
	}
	n.logOperationFinished(id, action, stage, state, reason, duration, result)
	n.autoSync.operationFinished(action, result, err)
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

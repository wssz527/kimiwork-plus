package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kimi 3.2.28 switches models in place only while the BYOK provider id of the
// gateway route is `byok-kth-kimi`. The official allocator looks for that id on
// a matching provider, then in the source's route binding for the gateway URL,
// and only then allocates a fresh one - and a fresh allocation is canonical only
// while no earlier `byok-kth-kimi/` alias is blocked and no provider of that
// name exists. A user who upgraded from an older build, switched Direct Mode
// off, or restarted on another loopback port therefore keeps a random
// `byok-<uuid>` provider, and the model picker opens a new conversation instead
// of switching in place.
//
// The repair below acts on the signed-in account only. A live byok-kth source
// must point at loopback. Direct store edits compare-and-swap against the bytes
// that were read and preserve secrets, owned resources and other accounts.

const (
	kthSourceID   = "byok-kth"
	kthSourceKind = "kth"
	// kthProviderID is the id the official importer allocates for the gateway's
	// kimi route, and the only one that permits an in-place model switch.
	kthProviderID  = "byok-kth-kimi"
	kthAliasPrefix = "byok-kth-kimi/"
	// kthRepairPasses bounds how often the store is re-read when a concurrent
	// writer keeps winning the compare-and-swap.
	kthRepairPasses = 3
)

var byokUUIDProvider = regexp.MustCompile(`^byok-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// kthStoreScan is one read of the store's signed-in account, seen from what a
// loopback import needs.
//
// `present` is a live byok-kth source. A quarantined or disconnected row is not
// one: the official disconnect may leave it behind, and an import that follows
// must still see the account as free.
type kthStoreScan struct {
	valid         bool
	account       map[string]any
	source        map[string]any
	routeKey      string
	present       bool
	owned         bool
	canonical     bool
	stuck         string
	bound         bool
	modeled       bool
	orphanPending bool
	blocked       []string
}

// needsDisconnect reports a byok-kth source this product owns whose kimi
// provider is on a non-canonical id with no canonical provider beside it.
func (s kthStoreScan) needsDisconnect() bool {
	return s.owned && !s.canonical && s.stuck != ""
}

// needsRebind reports a source that already carries the canonical provider
// while its gateway route still points at an older id. The official preview
// only reuses the canonical id through that binding after a port change.
func (s kthStoreScan) needsRebind() bool {
	return s.owned && s.canonical && !s.bound
}

// needsBlockedCleanup reports blocked aliases that keep the official allocator
// from minting `byok-kth-kimi` again after the source is gone.
func (s kthStoreScan) needsBlockedCleanup() bool {
	return !s.present && !s.modeled && len(s.blocked) > 0
}

func (s kthStoreScan) needsPendingSettle() bool {
	return s.orphanPending
}

func scanKthStore(store map[string]any, accountKey, baseURL string) kthStoreScan {
	scan := kthStoreScan{routeKey: kimiRouteKey(baseURL)}
	accounts, ok := store["accounts"].(map[string]any)
	if !ok || accountKey == "" {
		return scan
	}
	account, ok := accounts[accountKey].(map[string]any)
	if !ok {
		return scan
	}
	scan.valid = true
	scan.account = account
	if source, found := kthAccountSource(account); found {
		scan.source = source
		scan.present = !inactiveSource(source)
		scan.owned = scan.present && kthSourceOwned(source, account)
		scan.bound = stringField(mapField(source, "routeBindings"), scan.routeKey) == kthProviderID
	}
	providers, kimi := kthProviderIDs(account, scan.source)
	scan.canonical = providers[kthProviderID]
	if scan.owned && !scan.canonical {
		scan.stuck = kimi
	}
	active, pending := mapField(account, "active"), mapField(account, "pending")
	activeModeled, pendingModeled := kthSnapshotModeled(active), kthSnapshotModeled(pending)
	scan.modeled = activeModeled || pendingModeled
	scan.orphanPending = !scan.present && pending != nil &&
		(activeModeled || kthSnapshotHasProvider(active)) && !pendingModeled && !kthSnapshotHasProvider(pending)
	blocked := mapField(account, "blockedAliases")
	for alias := range blocked {
		if strings.HasPrefix(alias, kthAliasPrefix) {
			scan.blocked = append(scan.blocked, alias)
		}
	}
	sort.Strings(scan.blocked)
	return scan
}

// kthStoreState answers the same question from disk. An unreadable store, a
// missing account or a legacy store all answer "nothing to repair", because
// without the v3 shape there is no safe way to decide what is ours.
func kthStoreState(kimiDir, accountKey, baseURL string) kthStoreScan {
	raw, ok := readStoreRaw(filepath.Join(kimiDir, NativeWorkStore))
	if !ok {
		return kthStoreScan{}
	}
	var store map[string]any
	if json.Unmarshal(raw, &store) != nil || store == nil {
		return kthStoreScan{}
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		return kthStoreScan{}
	}
	return scanKthStore(store, accountKey, baseURL)
}

func kthAccountSource(account map[string]any) (map[string]any, bool) {
	rows, _ := account["sources"].([]any)
	for _, row := range rows {
		source, ok := row.(map[string]any)
		if ok && stringField(source, "id") == kthSourceID {
			return source, true
		}
	}
	return nil, false
}

func inactiveSource(source map[string]any) bool {
	return source["quarantined"] == true || stringField(source, "status") == "disconnected"
}

// kthSourceOwned reports whether a source is the gateway this product wrote:
// the official kth kind, and every address it declares on loopback. A remote or
// foreign source is never repaired.
func kthSourceOwned(source, account map[string]any) bool {
	if stringField(source, "kind") != kthSourceKind {
		return false
	}
	proved := false
	if registry := stringField(source, "registryUrl"); registry != "" {
		if !isLoopbackGateway(kthGatewayBase(registry)) {
			return false
		}
		proved = true
	}
	for _, provider := range kthProviderRows(account, source) {
		base := kthGatewayBase(providerBaseURL(provider))
		if base == "" {
			continue
		}
		if !isLoopbackGateway(base) {
			return false
		}
		proved = true
	}
	return proved
}

// kthGatewayBase reduces a registry or provider address to its /v1 root so the
// import's catalog URL and its gateway URL compare the same way.
func kthGatewayBase(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	value = strings.TrimSuffix(value, "/models/api.json")
	return strings.TrimRight(value, "/")
}

// kthProviderRows is every provider the byok-kth source declares: the ones it
// carries itself and the applied or pending ones the account names it on.
func kthProviderRows(account, source map[string]any) []map[string]any {
	var out []map[string]any
	for _, group := range []map[string]any{source, mapField(source, "desired")} {
		rows, _ := group["providers"].([]any)
		for _, row := range rows {
			if provider, ok := row.(map[string]any); ok {
				out = append(out, provider)
			}
		}
	}
	for _, group := range []string{"active", "pending"} {
		rows, _ := mapField(account, group)["providers"].([]any)
		for _, row := range rows {
			provider, ok := row.(map[string]any)
			if ok && stringField(provider, "sourceId") == kthSourceID {
				out = append(out, provider)
			}
		}
	}
	return out
}

// kthProviderIDs collects the provider ids the source declares and the id of
// its kimi route, which is the one the model picker switches on.
func kthProviderIDs(account, source map[string]any) (map[string]bool, string) {
	ids := map[string]bool{}
	kimi := ""
	for _, provider := range kthProviderRows(account, source) {
		id := stringField(provider, "id")
		if id == "" {
			continue
		}
		ids[id] = true
		if kimi == "" && id != kthProviderID && kthProviderProtocol(provider) == "kimi" {
			kimi = id
		}
	}
	return ids, kimi
}

func kthProviderProtocol(provider map[string]any) string {
	return firstNonEmpty(stringField(provider, "protocol"), stringField(provider, "type"))
}

func kthSnapshotModeled(snapshot map[string]any) bool {
	for _, row := range modelRows(snapshot) {
		if strings.HasPrefix(stringField(row, "alias"), kthAliasPrefix) {
			return true
		}
	}
	return false
}

func kthSnapshotHasProvider(snapshot map[string]any) bool {
	rows, _ := snapshot["providers"].([]any)
	for _, row := range rows {
		provider, _ := row.(map[string]any)
		if stringField(provider, "id") == kthProviderID {
			return true
		}
	}
	return false
}

// kimiRouteKey is the exact compact key the official store uses for the kimi
// route: the source family, the route family, the upstream group and the
// gateway URL without a trailing slash.
func kimiRouteKey(baseURL string) string {
	encoded, err := json.Marshal([]any{"kimi", "kimi", nil, strings.TrimRight(baseURL, "/")})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// readStoreRaw reads the official store only when it is a regular file the v3
// schema allows. Anything else answers "not readable" rather than a guess.
func readStoreRaw(file string) ([]byte, bool) {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maxStoreBytes {
		return nil, false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return raw, true
}

var errKthStoreConflict = errors.New("concurrent_modification")

// updateKthStore parses the store, lets update change it, and replaces the file
// only while the bytes on disk are still the ones parsed. It reports whether
// anything was written.
func updateKthStore(kimiDir string, update func(map[string]any) bool) (bool, error) {
	file := filepath.Join(kimiDir, NativeWorkStore)
	raw, ok := readStoreRaw(file)
	if !ok {
		return false, nil
	}
	var store map[string]any
	if json.Unmarshal(raw, &store) != nil || store == nil {
		return false, nil
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		return false, nil
	}
	if !update(store) {
		return false, nil
	}
	current, err := os.ReadFile(file)
	if err != nil {
		return false, errKthStoreConflict
	}
	if !bytes.Equal(current, raw) {
		return false, errKthStoreConflict
	}
	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return false, errors.New("invalid_store")
	}
	if err := replaceFile(file, append(encoded, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// rebindKthRouteBinding points the gateway route at the canonical provider so
// the official preview reuses it on the new loopback port.
func rebindKthRouteBinding(kimiDir, accountKey, baseURL string) (bool, error) {
	return updateKthStore(kimiDir, func(store map[string]any) bool {
		scan := scanKthStore(store, accountKey, baseURL)
		if !scan.needsRebind() {
			return false
		}
		bindings, ok := scan.source["routeBindings"].(map[string]any)
		if !ok {
			if raw, present := scan.source["routeBindings"]; present && raw != nil {
				// A shape this package does not understand is not rewritten.
				return false
			}
			bindings = map[string]any{}
			scan.source["routeBindings"] = bindings
		}
		bindings[scan.routeKey] = kthProviderID
		return true
	})
}

// clearKthBlockedAliases drops only the aliases that block a canonical
// allocation. Every other blocked entry, including a Direct Mode source's, is
// left alone.
func clearKthBlockedAliases(kimiDir, accountKey, baseURL string) (bool, error) {
	return updateKthStore(kimiDir, func(store map[string]any) bool {
		scan := scanKthStore(store, accountKey, baseURL)
		if !scan.needsBlockedCleanup() {
			return false
		}
		blocked, _ := scan.account["blockedAliases"].(map[string]any)
		for _, alias := range scan.blocked {
			delete(blocked, alias)
		}
		return true
	})
}

func mapField(object map[string]any, name string) map[string]any {
	value, _ := object[name].(map[string]any)
	return value
}

// kthDisconnectExpression disconnects the gateway source through the official
// API so the import that follows allocates the canonical id again. It re-checks
// on the page that the source is the local kth source before asking, and leaves
// every other source alone. `skipped` says the build has no source API at all,
// which is not a failure: such a build imports through its own entry point.
const kthDisconnectExpression = `(async()=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
if(!sources||typeof sources.list!=="function"||typeof sources.disconnect!=="function")return {ok:true,skipped:true};
const loopback=value=>/^http:\/\/(?:127\.0\.0\.1|\[::1\]):\d+\/v1(?:\/models\/api\.json)?\/?$/i.test(String(value??""));
const listed=await sources.list();
if(listed&&typeof listed==="object"&&listed.ok===false){
  return {ok:false,code:(typeof listed.reasonCode==="string"&&listed.reasonCode)?listed.reasonCode:"failed"};
}
const snapshot=listed?.ok===true?listed.value:null;
if(!snapshot||!Array.isArray(snapshot.sources)||typeof snapshot.scopeToken!=="string"||
  typeof snapshot.accountRevision!=="string"||!snapshot.accountRevision)return {ok:false,code:"contract-changed"};
const existing=snapshot.sources.find(source=>source?.id==="byok-kth");
if(!existing||existing.quarantined===true||existing.status==="disconnected")return {ok:false,code:"source-missing"};
if(existing.kind!=="kth"||!loopback(existing.registryUrl||""))return {ok:false,code:"foreign-source"};
const hadOtherPending=snapshot.sources.some(source=>{
  if(source?.id===existing.id)return false;
  if(Array.isArray(source?.models))return source.models.some(model=>model?.status==="pending");
  return snapshot.applyState!==undefined&&!["applied","idle","ready"].includes(snapshot.applyState);
});
const disconnected=await sources.disconnect({sourceId:existing.id,accountRevision:snapshot.accountRevision,
  sourceRevision:existing.revision??null});
if(disconnected?.ok!==true)return {ok:false,code:disconnected?.reasonCode||"disconnect-failed"};
let status=typeof disconnected.value?.status==="string"?disconnected.value.status:"";
if(status==="pending"&&!hadOtherPending&&typeof sources.applyPending==="function"&&
  typeof disconnected.value?.accountRevision==="string"&&disconnected.value.accountRevision){
  const listedLatest=await sources.list();
  if(listedLatest&&typeof listedLatest==="object"&&listedLatest.ok===false){
    return {ok:false,code:(typeof listedLatest.reasonCode==="string"&&listedLatest.reasonCode)?listedLatest.reasonCode:"failed"};
  }
  const latest=listedLatest?.ok===true?listedLatest.value:null;
  if(!latest||latest.scopeToken!==snapshot.scopeToken||latest.accountRevision!==disconnected.value.accountRevision){
    return {ok:false,code:"byok-source-stale"};
  }
  const applied=await sources.applyPending(disconnected.value.accountRevision);
  if(applied?.ok!==true)return {ok:false,code:applied?.reasonCode||"apply-failed"};
  status=typeof applied.value?.status==="string"?applied.value.status:"";
}
if(status==="failed"||status==="apply-failed")return {ok:false,code:"apply-failed"};
let applyScopeToken="",applyRevision="";
if(status==="deferred"&&!hadOtherPending&&typeof sources.applyPending==="function"&&
  typeof disconnected.value?.accountRevision==="string"&&disconnected.value.accountRevision){
  applyScopeToken=snapshot.scopeToken;applyRevision=disconnected.value.accountRevision;
}
return {ok:true,status:status,applyScopeToken:applyScopeToken,applyRevision:applyRevision};
})()`

func kthSettlePendingExpression(directSourceID, expectedRevision string) string {
	return `(async(directSourceID,expectedRevision)=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
if(typeof sources?.list!=="function"||typeof sources.applyPending!=="function")return {ok:false,code:"contract-changed"};
const reply=await sources.list();
if(reply&&typeof reply==="object"&&reply.ok===false){
  return {ok:false,code:(typeof reply.reasonCode==="string"&&reply.reasonCode)?reply.reasonCode:"failed"};
}
const current=reply?.ok===true?reply.value:null;
if(!current||!Array.isArray(current.sources)||typeof current.scopeToken!=="string"||
  typeof current.accountRevision!=="string"||!current.accountRevision)return {ok:false,code:"contract-changed"};
if(!expectedRevision||current.accountRevision!==expectedRevision||current.sources.some(source=>
  source?.id==="byok-kth"&&source.quarantined!==true&&source.status!=="disconnected"))return {ok:true,status:"pending"};
const direct=directSourceID&&(current.sources.find(source=>source?.id===directSourceID)||
  current.sources.find(source=>source?.kind==="custom"&&source.displayName==="` + DirectDisplayName + `"));
const otherPending=current.sources.some(source=>{
  if(source?.id==="byok-kth"||(source===direct&&source?.kind==="custom"))return false;
  if(Array.isArray(source?.models))return source.models.some(model=>model?.status==="pending");
  return current.applyState!==undefined&&!["applied","idle","ready"].includes(current.applyState);
});
if(otherPending)return {ok:true,status:"pending"};
const applied=await sources.applyPending(current.accountRevision);
if(applied?.ok!==true)return {ok:false,code:applied?.reasonCode||"apply-failed"};
const status=typeof applied.value?.status==="string"?applied.value.status:"";
if(status==="failed"||status==="apply-failed")return {ok:false,code:"apply-failed"};
return {ok:true,status:status,applyScopeToken:current.scopeToken,applyRevision:current.accountRevision};
})(` + quoteJS(directSourceID) + `,` + quoteJS(expectedRevision) + `)`
}

// kthResetAnswer reports a skipped legacy disconnect or a result the import
// must stop for. An empty answer means the reset applied.
type kthResetAnswer struct {
	skipped bool
	stop    *ImportResult
}

// repairKthProvider puts the store in the shape the official allocator needs
// before an import runs. It answers a non-nil result when the import must stop,
// and nil when the import may proceed.
//
// A source that is stuck on a non-canonical id is disconnected through the
// official API first; the wait after it lets Electron's own post-restart model
// refresh finish before the store is scanned. A deferred disconnect is retried
// against `applyPending` while the official window settles; only when it stays
// deferred does the import ask for the restart that applies it. The blocked
// aliases the disconnect leaves behind are cleared in the same call, so the
// import that follows allocates `byok-kth-kimi` again. A gateway already removed
// from sources can still need its pending removal applied before this cleanup.
func (a *cdpActions) repairKthProvider(ctx context.Context, settings actionTarget, account, baseURL, disconnectDirectSourceID string) (*ImportResult, error) {
	disconnected, settledPending := false, false
	for pass := 0; pass < kthRepairPasses; {
		scan := kthStoreState(a.kimiHome, account, baseURL)
		if !scan.valid {
			return nil, nil
		}
		switch {
		case scan.needsDisconnect() && !disconnected:
			reset, err := a.kthProviderReset(ctx, settings, kthDisconnectExpression)
			if err != nil || reset.stop != nil {
				return reset.stop, err
			}
			if reset.skipped {
				// A build without byokSources allocates through its own importer:
				// there is nothing here to repair and the import is unchanged.
				return nil, nil
			}
			// Electron stops and restarts Daimon for an applied disconnect and
			// fires its own `onDaimonReady` model refresh asynchronously. That
			// refresh writes the store back from the bytes it read before the
			// disconnect, restoring `blockedAliases` seconds later. One step lets
			// it land before the scan below decides what still needs clearing.
			if err := a.waitStep(ctx); err != nil {
				return nil, err
			}
			disconnected = true
		case scan.needsDisconnect():
			// The official API reported an applied disconnect but the store still
			// holds the source. Importing now would mint another random id.
			return &ImportResult{State: ImportStateFailed, Reason: "native_import_store_changed"}, nil
		case scan.needsPendingSettle() && !settledPending:
			expression := kthSettlePendingExpression(disconnectDirectSourceID, stringField(scan.account, "revision"))
			reset, err := a.kthProviderReset(ctx, settings, expression)
			if err != nil || reset.stop != nil {
				return reset.stop, err
			}
			if err := a.waitStep(ctx); err != nil {
				return nil, err
			}
			settledPending = true
			continue
		case scan.needsPendingSettle():
			return kthProviderResetPending(false), nil
		case scan.needsRebind():
			if _, err := rebindKthRouteBinding(a.kimiHome, account, baseURL); err != nil {
				if err := a.waitStep(ctx); err != nil {
					return nil, err
				}
			}
		case scan.needsBlockedCleanup():
			if _, err := clearKthBlockedAliases(a.kimiHome, account, baseURL); err != nil {
				if err := a.waitStep(ctx); err != nil {
					return nil, err
				}
			}
		default:
			return nil, nil
		}
		pass++
	}
	// The store kept changing under every pass. Proceeding would let the
	// official allocator mint a random id, so the import stops and the next sync
	// retries from fresh bytes.
	return &ImportResult{State: ImportStateFailed, Reason: "native_import_store_changed"}, nil
}

func (a *cdpActions) kthProviderReset(ctx context.Context, settings actionTarget, expression string) (kthResetAnswer, error) {
	value, err := a.evaluate(ctx, settings.sessionID, expression)
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			a.dropSession(settings.targetID)
		}
		state, reason, surfaced := classifyActionError(err)
		return kthResetAnswer{stop: &ImportResult{State: state, Reason: reason}}, surfaced
	}
	answer, _ := value.(map[string]any)
	if answer == nil {
		return kthResetAnswer{stop: &ImportResult{State: ImportStateContractChanged, Reason: NativeContractChanged}}, nil
	}
	if skipped, _ := answer["skipped"].(bool); skipped {
		return kthResetAnswer{skipped: true}, nil
	}
	if ok, _ := answer["ok"].(bool); !ok {
		code, _ := answer["code"].(string)
		state := ImportStateFailed
		reason := kthResetFailureReason(code)
		switch reason {
		case NativeContractChanged:
			state = ImportStateContractChanged
		case NativeAccountChanged:
			state = ImportStateAccountChanged
		}
		return kthResetAnswer{stop: &ImportResult{State: state, Reason: reason}}, nil
	}
	status, _ := answer["status"].(string)
	if status == "deferred" {
		scopeToken, _ := answer["applyScopeToken"].(string)
		revision, _ := answer["applyRevision"].(string)
		if scopeToken != "" && revision != "" {
			// The disconnect was written but the post-ready window answered
			// `busy`. Applying it a moment later repairs the provider without
			// restarting Kimi, so the import may continue in this call.
			outcome, err := a.retryDeferredApply(ctx, settings, scopeToken, revision)
			if err != nil {
				state, reason, surfaced := classifyActionError(err)
				return kthResetAnswer{stop: &ImportResult{State: state, Reason: reason}}, surfaced
			}
			switch outcome {
			case "applied":
				return kthResetAnswer{}, nil
			case "account-changed":
				return kthResetAnswer{stop: &ImportResult{State: ImportStateAccountChanged,
					Reason: NativeAccountChanged}}, nil
			case "apply-failed":
				return kthResetAnswer{stop: &ImportResult{State: ImportStateFailed,
					Reason: "native_import_provider_reset_failed"}}, nil
			}
		}
	}
	if status != "applied" {
		return kthResetAnswer{stop: kthProviderResetPending(status == "deferred")}, nil
	}
	return kthResetAnswer{}, nil
}

func kthProviderResetPending(deferred bool) *ImportResult {
	return &ImportResult{State: ImportStateRequiresRestart, Reason: "model_provider_reset_pending",
		RequiresRestart: true, RestartDeferred: deferred}
}

// kthResetFailureReason maps a page code onto the vocabulary the panel renders.
func kthResetFailureReason(code string) string {
	switch strings.ReplaceAll(safeActionCode(code), "-", "_") {
	case "contract_changed":
		return NativeContractChanged
	case "account_changed":
		return NativeAccountChanged
	case "foreign_source":
		return "native_import_foreign_source"
	case "feature_disabled":
		return "native_import_feature_disabled"
	case "no_account":
		return "native_import_no_account"
	case "byok_not_ready":
		return "native_import_byok_not_ready"
	}
	return "native_import_provider_reset_failed"
}

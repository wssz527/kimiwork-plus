package kimi

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Direct Mode imports a user's own provider accounts straight into Kimi 3.2.28
// through the official `byokSources` surface, with no local gateway in between.
//
// The split of knowledge here is deliberate:
//
//	the gateway stays the default   the value that ran production until now
//	direct mode is an explicit mode  only the user's own provider keys are used
//
// A direct import carries provider keys over the private loopback bridge into
// one page call. Nothing about a key is ever written to the intent, the state
// directory or a log: the intent records the source id, the model aliases and
// the account digest, which is what reconciliation needs and nothing more.

const (
	// IntegrationModeGateway routes models through this product's local gateway.
	IntegrationModeGateway = "gateway"
	// IntegrationModeDirect imports the user's providers into Kimi directly.
	IntegrationModeDirect = "direct"
	// DirectDisplayName is the managed custom source's name. It is the fallback
	// identity a source is found by when its id has changed across a sync.
	DirectDisplayName = "KimiWork + 直连"
)

// DirectProviderSpec is a provider declaration safe to keep in settings and
// readback: it names the endpoint, never the key.
type DirectProviderSpec struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Protocol    string `json:"protocol"`
	BaseURL     string `json:"baseUrl"`
}

// DirectProvider is the import-only shape. APIKey travels over the private
// loopback bridge to the page; it is never written to disk, an intent, a log or
// a state file.
type DirectProvider struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Protocol    string `json:"protocol"`
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"`
}

// DirectModel is one model imported from a direct provider. ID is the logical
// model id KimiWork + knows the model by; Model is the upstream id the provider
// serves.
type DirectModel struct {
	ID             string   `json:"id"`
	ProviderID     string   `json:"providerId"`
	Model          string   `json:"model"`
	DisplayName    string   `json:"displayName"`
	MaxContextSize int64    `json:"maxContextSize"`
	MaxOutputSize  int64    `json:"maxOutputSize,omitempty"`
	Capabilities   []string `json:"capabilities"`
	SupportEfforts []string `json:"supportEfforts,omitempty"`
	DefaultEffort  string   `json:"defaultEffort,omitempty"`
}

// DirectSpec is the non-secret declaration kept on ControllerSettings: what
// reconciliation verifies and what the panel renders.
type DirectSpec struct {
	DisplayName string               `json:"displayName,omitempty"`
	Providers   []DirectProviderSpec `json:"providers"`
	Models      []DirectModel        `json:"models"`
}

// DirectImportRequest is one direct import. It exists only for the duration of
// the call: the bridge forwards it to the page and the raw request is dropped.
type DirectImportRequest struct {
	DisplayName          string           `json:"displayName,omitempty"`
	Providers            []DirectProvider `json:"providers"`
	Models               []DirectModel    `json:"models"`
	ExpectedAccountKey   string           `json:"expectedAccountKey,omitempty"`
	OpenSettings         bool             `json:"openSettings,omitempty"`
	ActivateWork         bool             `json:"activateWork,omitempty"`
	ExistingSourceID     string           `json:"existingSourceId,omitempty"`
	DisconnectGateway    bool             `json:"disconnectGateway,omitempty"`
	DisconnectGatewayURL string           `json:"disconnectGatewayUrl,omitempty"`
}

// integrationMode reads the configured mode; empty means the gateway, because a
// caller that never heard of Direct Mode must keep the released behaviour.
func (s ControllerSettings) integrationMode() string {
	if strings.TrimSpace(s.IntegrationMode) == IntegrationModeDirect {
		return IntegrationModeDirect
	}
	return IntegrationModeGateway
}

var directProtocols = map[string]bool{
	"openai": true, "openai_responses": true, "anthropic": true,
	"google-genai": true, "kimi": true,
}

func isValidDirectProtocol(protocol string) bool { return directProtocols[protocol] }

// isValidDirectBaseURL accepts an https endpoint or a loopback http one, with
// no userinfo, query or fragment, and without a path that already names an
// operation the provider client appends itself.
func isValidDirectBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		host := parsed.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return false
		}
	default:
		return false
	}
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		if strings.HasSuffix(path, suffix) {
			return false
		}
	}
	return true
}

// validDirectKey refuses an empty key and anything a JSON string or an HTTP
// path could split on.
func validDirectKey(value string) bool {
	if value == "" || len(value) > 8192 {
		return false
	}
	return !strings.ContainsAny(value, "\r\n\x00")
}

// validateDirectImportRequest answers "" when the request is importable and a
// reason code otherwise. Nothing here is allowed to be "repaired" on the page:
// a bad endpoint or a model without tool use is a caller mistake.
func validateDirectImportRequest(request DirectImportRequest) string {
	encoded, _ := json.Marshal(request)
	if len(encoded) > maxDirectCatalog {
		return "direct_catalog_too_large"
	}
	invalid := "native_bridge_invalid_request"
	if len(request.DisplayName) > 256 {
		return invalid
	}
	if len(request.Providers) == 0 || len(request.Providers) > 100 {
		return invalid
	}
	providerIDs := map[string]bool{}
	pairs := map[string]bool{}
	for _, provider := range request.Providers {
		if provider.ID == "" || len(provider.ID) > 256 || providerIDs[provider.ID] {
			return invalid
		}
		providerIDs[provider.ID] = true
		if len(provider.DisplayName) > 256 || !isValidDirectProtocol(provider.Protocol) ||
			!isValidDirectBaseURL(provider.BaseURL) || !validDirectKey(provider.APIKey) {
			return invalid
		}
		pair := provider.Protocol + "\x00" + normalizeDirectURL(provider.BaseURL, provider.Protocol)
		if pairs[pair] {
			// Two providers on one endpoint would collapse into the same existing
			// provider on the next sync, so the draft is refused instead.
			return invalid
		}
		pairs[pair] = true
	}
	if len(request.Models) == 0 || len(request.Models) > 1000 {
		return invalid
	}
	modelIDs := map[string]bool{}
	for _, model := range request.Models {
		if model.ID == "" || modelIDs[model.ID] {
			return invalid
		}
		modelIDs[model.ID] = true
		if !providerIDs[model.ProviderID] || strings.TrimSpace(model.Model) == "" ||
			len(model.DisplayName) > 256 || model.MaxContextSize <= 0 || model.MaxOutputSize < 0 {
			return invalid
		}
		capabilities, ok := directCapabilities(model.Capabilities)
		if !ok || !containsString(capabilities, "tool_use") {
			return invalid
		}
		if !validEfforts(model.SupportEfforts, model.DefaultEffort) {
			return invalid
		}
	}
	if request.ExistingSourceID != "" && len(request.ExistingSourceID) > 256 {
		return invalid
	}
	if request.DisconnectGatewayURL != "" && !isLoopbackGateway(request.DisconnectGatewayURL) {
		return invalid
	}
	return ""
}

var directCapabilitySet = map[string]bool{
	"tool_use": true, "thinking": true, "image_in": true, "video_in": true,
}

func directCapabilities(values []string) ([]string, bool) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !directCapabilitySet[value] || seen[value] {
			return nil, false
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, true
}

func validEfforts(support []string, defaultEffort string) bool {
	seen := map[string]bool{}
	for _, effort := range support {
		if effort == "" || len(effort) > 64 || seen[effort] {
			return false
		}
		seen[effort] = true
	}
	if defaultEffort != "" && !seen[defaultEffort] {
		return false
	}
	return true
}

// normalizeDirectURL is the comparison Kimi itself makes: trailing slashes are
// dropped, and an anthropic endpoint's trailing /v1 is dropped too.
func normalizeDirectURL(value, protocol string) string {
	parsed, err := parseURL(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	if protocol == "anthropic" {
		path = strings.TrimSuffix(path, "/v1")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + path
}

// directImportExpression is the page-side import. It runs one `byokSources`
// conversation: find the managed source, preview, save, apply, verify, then
// optionally disconnect the old gateway. A cleanup replay is pinned to the
// original page scope. The draft carries exactly the fields Kimi 3.2.28 allows.
func directImportExpression(request DirectImportRequest, cleanupScopeToken string) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return `(async(raw,cleanupScopeToken)=>{
const request=JSON.parse(raw);
const sources=globalThis.clawBridgeAPI?.byokSources;
if(!sources||typeof sources.list!=="function"||typeof sources.preview!=="function"||typeof sources.save!=="function"||typeof sources.disconnect!=="function"){
  return {ok:false,code:"direct-mode-unsupported"};
}
const unwrap=result=>result?.ok===true?result.value:null;` + settleDisconnectJS + `
const normalizeURL=(value,protocol)=>{
  let text=String(value??"").trim().replace(/\/+$/,"");
  if(protocol==="anthropic")text=text.replace(/\/v1$/,"");
  return text.toLowerCase();
};
const isLoopback=value=>/^http:\/\/(?:127\.0\.0\.1|localhost|\[::1\]):\d+\//i.test(String(value??""));
let listCode="";
const readSnapshot=async()=>{
  const reply=await sources.list();
  if(reply&&typeof reply==="object"&&reply.ok===false){
    listCode=(typeof reply.reasonCode==="string"&&reply.reasonCode)?reply.reasonCode:"failed";
    return null;
  }
  const snapshot=reply?.ok===true?reply.value:null;
  if(!snapshot||!Array.isArray(snapshot.sources)||typeof snapshot.scopeToken!=="string"){
    listCode="contract-changed";
    return null;
  }
  listCode="";
  return snapshot;
};
let snapshot=await readSnapshot();
if(!snapshot)return {ok:false,code:listCode||"contract-changed"};
if(cleanupScopeToken&&snapshot.scopeToken!==cleanupScopeToken)return {ok:false,code:"account-changed"};
let gatewayDisconnected=false;
const ownGateway=source=>{
  if(!source||expectedGateway===""||source.quarantined===true||source.status==="disconnected")return false;
  const providers=Array.isArray(source.providers)?source.providers:[];
  const addresses=[];
  for(const provider of providers){if(typeof provider?.baseUrl==="string"&&provider.baseUrl)addresses.push(provider.baseUrl);}
  const registry=typeof source.registryUrl==="string"?source.registryUrl:"";
  if(registry){
    // Port drift leaves older loopback providers inside our managed registry.
    return gatewayAddress(registry)===expectedGateway&&addresses.every(isLoopback);
  }
  return addresses.length>0&&addresses.every(value=>
    isLoopback(value)&&gatewayAddress(value)===expectedGateway);
};
const gatewayAddress=value=>String(value??"").trim().replace(/\/models\/api\.json\/?$/i,"").replace(/\/+$/,"").toLowerCase();
const expectedGateway=request.disconnectGateway?gatewayAddress(request.disconnectGatewayUrl):"";
const hadManagedGateway=ownGateway(snapshot.sources.find(source=>source.id==="byok-kth"||source.kind==="kth"));
const wantedProviders=Array.isArray(request.providers)?request.providers:[];
const wantedModels=Array.isArray(request.models)?request.models:[];
let existing=null;
if(typeof request.existingSourceId==="string"&&request.existingSourceId){
  existing=snapshot.sources.find(source=>source.id===request.existingSourceId)||null;
}
if(!existing&&typeof request.displayName==="string"&&request.displayName){
  existing=snapshot.sources.find(source=>source.kind==="custom"&&source.displayName===request.displayName)||null;
}
if(existing&&existing.kind!=="custom")return {ok:false,code:"foreign-source"};
const existingProviders=Array.isArray(existing?.providers)?existing.providers:[];
const usedExisting=new Set();
const providerIDs={};
const draftProviders=wantedProviders.map(provider=>{
  const match=existingProviders.find(candidate=>!usedExisting.has(candidate.id)&&candidate.protocol===provider.protocol&&normalizeURL(candidate.baseUrl,provider.protocol)===normalizeURL(provider.baseUrl,provider.protocol));
  const id=match&&typeof match.id==="string"&&match.id?match.id:provider.id;
  if(match)usedExisting.add(id);
  providerIDs[provider.id]=id;
  return {id:id,displayName:provider.displayName||provider.id,protocol:provider.protocol,
    baseUrl:provider.baseUrl,key:{mode:"set",value:provider.apiKey},headers:{mode:"clear"}};
});
const draftModels=wantedModels.map(model=>{
  const entry={providerId:providerIDs[model.providerId]||model.providerId,model:model.model,
    displayName:model.displayName||model.model,maxContextSize:model.maxContextSize,
    declaredContextSize:model.maxContextSize,
    capabilities:Array.isArray(model.capabilities)?model.capabilities:["tool_use"],provenance:"manual"};
  if(typeof model.maxOutputSize==="number"&&model.maxOutputSize>0)entry.maxOutputSize=model.maxOutputSize;
  if(Array.isArray(model.supportEfforts)&&model.supportEfforts.length){
    entry.supportEfforts=model.supportEfforts;
    if(typeof model.defaultEffort==="string"&&model.supportEfforts.indexOf(model.defaultEffort)>=0)entry.defaultEffort=model.defaultEffort;
  }
  return entry;
});
const draft={kind:"custom",displayName:request.displayName||"` + DirectDisplayName + `",providers:draftProviders,models:draftModels};
const hasOtherPending=(current,sourceID)=>current.sources.some(source=>{
  if(source.id===sourceID)return false;
  if(Array.isArray(source.models))return source.models.some(model=>model.status==="pending");
  return current.applyState!==undefined&&!["applied","idle","ready"].includes(current.applyState);
});
let allowApply=!hasOtherPending(snapshot,existing?.id);
const importDraft=async(current,existing,allowApply)=>{
  const previewRequest={accountRevision:current.accountRevision,sourceRevision:existing?.revision??null,draft:draft};
  if(existing)previewRequest.sourceId=existing.id;
  const previewReply=await sources.preview(previewRequest);
  const preview=unwrap(previewReply);
  if(!preview||preview.canSave!==true||typeof preview.previewId!=="string"||preview.scopeToken!==current.scopeToken||preview.accountRevision!==current.accountRevision){
    if(typeof preview?.previewId==="string"&&typeof sources.discard==="function")await sources.discard(preview.previewId);
    return {error:previewReply?.reasonCode||"direct-preview-refused",previewFailed:true};
  }
  const saved=await sources.save({previewId:preview.previewId,accountRevision:preview.accountRevision,sourceRevision:preview.sourceRevision});
  if(!saved?.ok||typeof saved.value?.sourceId!=="string"||!saved.value.sourceId)return {error:saved?.reasonCode||"import-failed"};
  let status=saved.value.status;
  if(status==="pending"&&allowApply&&typeof sources.applyPending==="function"){
    const latest=await readSnapshot();
    if(!latest||latest.scopeToken!==current.scopeToken||latest.accountRevision!==saved.value.accountRevision)return {error:listCode||"byok-source-stale"};
    const applied=await sources.applyPending(saved.value.accountRevision);
    if(!applied?.ok)return {error:applied?.reasonCode||"import-failed"};
    status=applied.value?.status;
  }
  return {saved:saved.value,status:status};
};
let imported=await importDraft(snapshot,existing,allowApply);
if(imported.previewFailed&&imported.error==="byok-invalid-source"&&existing){
  // Kimi 3.2.28 can retain an old defaultEffort when a draft omits it.
  // A fresh provider identity avoids that merge within the same source save.
  const replacements=new Map();
  for(const provider of wantedProviders){
    const previousID=providerIDs[provider.id];
    if(usedExisting.has(previousID)&&!existingProviders.some(candidate=>candidate.id===provider.id)&&
       wantedModels.some(model=>model.providerId===provider.id&&!model.defaultEffort)){
      replacements.set(previousID,provider.id);
    }
  }
  if(replacements.size){
    for(const provider of draft.providers)provider.id=replacements.get(provider.id)||provider.id;
    for(const model of draft.models)model.providerId=replacements.get(model.providerId)||model.providerId;
    imported=await importDraft(snapshot,existing,allowApply);
  }
}
if(imported.error)return {ok:false,code:imported.error};
let saved=imported.saved,status=imported.status;
if(["applied","pending","deferred"].indexOf(status)<0)return {ok:false,code:"import-failed"};
const officialProviderIDs=source=>{
  if(!Array.isArray(source?.providers))return null;
  const ids=new Map();
  const used=new Set();
  for(const wanted of draft.providers){
    const matches=source.providers.filter(provider=>provider.protocol===wanted.protocol&&
      normalizeURL(provider.baseUrl,provider.protocol)===normalizeURL(wanted.baseUrl,wanted.protocol));
    const id=matches[0]?.id;
    if(matches.length!==1||typeof id!=="string"||!id||used.has(id))return null;
    ids.set(wanted.id,id);
    used.add(id);
  }
  return ids;
};
const cleanupGateway=async()=>{
  if(!request.disconnectGateway||status!=="applied")return {settled:!hadManagedGateway};
  const current=await readSnapshot();
  if(!current||current.scopeToken!==snapshot.scopeToken)return {error:listCode||"contract-changed"};
  const direct=current.sources.find(source=>source.id===saved.sourceId&&source.kind==="custom");
  const directModels=Array.isArray(direct?.models)?direct.models:[];
  const ids=officialProviderIDs(direct);
  const ready=!direct?.quarantined&&ids&&draftModels.every(wanted=>directModels.some(model=>
    model.providerId===ids.get(wanted.providerId)&&model.model===wanted.model&&model.status==="available"));
  if(!ready)return {settled:!hadManagedGateway};
  const gateway=current.sources.find(source=>source.id==="byok-kth"||source.kind==="kth");
  // Nothing of this installation's gateway is left to remove: an inactive or
  // foreign one is not this cleanup's business.
  if(!ownGateway(gateway))return {settled:true};
  const cleanupOtherPending=hasOtherPending(current,gateway.id);
  const reply=await sources.disconnect({sourceId:gateway.id,accountRevision:current.accountRevision,sourceRevision:gateway.revision??null});
  if(reply?.ok===true)gatewayDisconnected=true;
  return await settleDisconnect(reply,current,cleanupOtherPending);
};
let cleanup=await cleanupGateway();
if(cleanup?.error)return {ok:false,code:cleanup.error};
let routeCleanupPending=cleanup?.settled!==true||(!!cleanupScopeToken&&status!=="applied");
if(request.disconnectGateway&&routeCleanupPending&&status==="applied"){
  const current=await readSnapshot();
  if(!current||current.scopeToken!==snapshot.scopeToken)return {ok:false,code:listCode||"contract-changed"};
  const direct=current.sources.find(source=>source.id===saved.sourceId&&source.kind==="custom");
  const directModels=Array.isArray(direct?.models)?direct.models:[];
  const directPending=directModels.some(model=>model.status==="pending");
  const foreignPending=hasOtherPending(current,saved.sourceId);
  // A background catalog refresh can race the first disconnect. Re-save only
  // our exact manual draft when no other source has a pending confirmation.
  const ids=officialProviderIDs(direct);
  if(ids&&directPending&&!foreignPending){
    allowApply=true;
    for(const provider of draft.providers)provider.id=ids.get(provider.id);
    for(const model of draft.models)model.providerId=ids.get(model.providerId);
    imported=await importDraft(current,direct,allowApply);
    if(imported.error)return {ok:false,code:imported.error};
    saved=imported.saved;status=imported.status;
    if(["applied","pending","deferred"].indexOf(status)<0)return {ok:false,code:"import-failed"};
    cleanup=await cleanupGateway();
    if(cleanup?.error)return {ok:false,code:cleanup.error};
    routeCleanupPending=cleanup?.settled!==true;
  }
}
let applyScopeToken="",applyRevision="",cleanupApplyScopeToken="",cleanupApplyRevision="";
if(status==="deferred"&&allowApply&&typeof sources.applyPending==="function"&&
  typeof saved.accountRevision==="string"&&saved.accountRevision){
  applyScopeToken=snapshot.scopeToken;applyRevision=saved.accountRevision;
}
if(routeCleanupPending&&cleanup?.settled!==true){
  cleanupApplyScopeToken=typeof cleanup?.applyScopeToken==="string"?cleanup.applyScopeToken:"";
  cleanupApplyRevision=typeof cleanup?.applyRevision==="string"?cleanup.applyRevision:"";
}
return {ok:true,sourceId:saved.sourceId,gatewayDisconnected:gatewayDisconnected,routeCleanupPending:routeCleanupPending,
  cleanupApplyScopeToken:cleanupApplyScopeToken,cleanupApplyRevision:cleanupApplyRevision,
  models:draftModels.length,requiresRestart:status!=="applied",restartDeferred:status==="deferred",
  applyScopeToken:applyScopeToken,applyRevision:applyRevision};
})(` + quoteJS(string(encoded)) + `,` + quoteJS(cleanupScopeToken) + `)`, nil
}

// ---------------------------------------------------------------------------
// store side

// directAccount is one 3.2.28 account with every source it declares. Direct
// Mode needs all accounts, not only the ones holding a kth source: the managed
// custom source may be the only source a direct installation has.
type directAccount struct {
	key     string
	account map[string]any
	sources []map[string]any
}

func directAccountCandidates(store map[string]any) []directAccount {
	accounts, ok := store["accounts"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(accounts))
	for key := range accounts {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]directAccount, 0, len(keys))
	for _, key := range keys {
		account, ok := accounts[key].(map[string]any)
		if !ok {
			continue
		}
		var sources []map[string]any
		rows, _ := account["sources"].([]any)
		for _, row := range rows {
			if source, ok := row.(map[string]any); ok {
				sources = append(sources, source)
			}
		}
		out = append(out, directAccount{key: key, account: account, sources: sources})
	}
	return out
}

// DirectGatewayRemains reports this installation's loopback gateway in the
// current direct account. A matching current registry proves ownership even
// when its providers still name older loopback ports; without a registry every
// provider must match. Remote providers always refuse cleanup.
func DirectGatewayRemains(kimiDir, accountScope, gatewayURL string) bool {
	wanted := directGatewayBase(gatewayURL)
	if !isLoopbackGateway(wanted) {
		return false
	}
	store, kind := readJSONFile(filepath.Join(kimiDir, NativeWorkStore), maxStoreBytes)
	if kind != "" {
		return false
	}
	scopeDigest := ""
	if accountScope != "" {
		scopeDigest = sha256Hex(accountScope)
	}
	account, ambiguous := selectDirectAccount(directAccountCandidates(store), scopeDigest, nil)
	if ambiguous || account.account == nil {
		return false
	}
	for _, source := range account.sources {
		if stringField(source, "kind") != "kth" && stringField(source, "id") != "byok-kth" ||
			source["quarantined"] == true || stringField(source, "status") == "disconnected" {
			continue
		}
		registry := stringField(source, "registryUrl")
		var addresses []string
		for _, group := range []string{"providers", "desired"} {
			rows, _ := source[group].([]any)
			if group == "desired" {
				desired, _ := source[group].(map[string]any)
				rows, _ = desired["providers"].([]any)
			}
			for _, value := range rows {
				provider, _ := value.(map[string]any)
				if address := stringField(provider, "baseUrl"); address != "" {
					addresses = append(addresses, address)
				}
			}
		}
		if registry == "" && len(addresses) == 0 {
			continue
		}
		owned := registry == "" || directGatewayBase(registry) == wanted
		for _, address := range addresses {
			base := directGatewayBase(address)
			if !isLoopbackGateway(base) || registry == "" && base != wanted {
				owned = false
				break
			}
		}
		if owned {
			return true
		}
	}
	return false
}

func directGatewayBase(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	value = strings.TrimSuffix(value, "/models/api.json")
	return normalizeDirectURL(value, "")
}

func selectDirectAccount(candidates []directAccount, scopeDigest string, intent *nativeIntent) (directAccount, bool) {
	if len(candidates) == 0 {
		return directAccount{}, false
	}
	if scopeDigest != "" {
		for _, candidate := range candidates {
			if sha256Hex(candidate.key) == scopeDigest {
				return candidate, false
			}
		}
		return directAccount{}, false
	}
	if intent != nil && intent.ProfileKeyDigest != "" {
		for _, candidate := range candidates {
			if sha256Hex(candidate.key) == intent.ProfileKeyDigest {
				return candidate, false
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0], false
	}
	return directAccount{}, true
}

// directSourceOf finds the managed custom source. The id is trusted first
// because it is what the intent recorded; the display name is the fallback for
// a source the page replaced during a sync.
func directSourceOf(sources []map[string]any, sourceID string) (map[string]any, bool) {
	if sourceID != "" {
		for _, source := range sources {
			if stringField(source, "id") == sourceID && stringField(source, "kind") == "custom" {
				return source, true
			}
		}
	}
	for _, source := range sources {
		if stringField(source, "kind") == "custom" && stringField(source, "displayName") == DirectDisplayName {
			return source, true
		}
	}
	return nil, false
}

// DirectSourceRemains finds this tool's connected custom source in the current
// account. The id recorded by an earlier intent is preferred; the managed display
// name recovers cleanup after a gateway reconcile has replaced that intent.
func DirectSourceRemains(kimiDir, accountScope, preferredSourceID string) string {
	store, kind := readJSONFile(filepath.Join(kimiDir, NativeWorkStore), maxStoreBytes)
	if kind != "" {
		return ""
	}
	scopeDigest := ""
	if accountScope != "" {
		scopeDigest = sha256Hex(accountScope)
	}
	account, ambiguous := selectDirectAccount(directAccountCandidates(store), scopeDigest, nil)
	if ambiguous || account.account == nil {
		return ""
	}
	return directSourceRemainsInAccount(account.account, preferredSourceID)
}

func directSourceRemainsInAccount(account map[string]any, preferredSourceID string) string {
	rows, _ := account["sources"].([]any)
	sources := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		source, ok := row.(map[string]any)
		if !ok || source["quarantined"] == true || stringField(source, "status") == "disconnected" {
			continue
		}
		sources = append(sources, source)
	}
	source, found := directSourceOf(sources, preferredSourceID)
	if !found {
		return ""
	}
	return stringField(source, "id")
}

func intentDirectSource(intent *nativeIntent) string {
	if intent == nil || intent.Mode != IntegrationModeDirect {
		return ""
	}
	return intent.DirectSourceID
}

// directActiveRows is the applied model set of one source minus the aliases the
// account blocks. The provider facts are read from `active` only: a fact that
// exists merely in `pending` has not been applied.
func directActiveRows(account map[string]any, sourceID string) (map[string]nativeProviderV3, []map[string]any) {
	if sourceID == "" {
		return nil, nil
	}
	active, _ := account["active"].(map[string]any)
	facts := map[string]nativeProviderV3{}
	providerRows, _ := active["providers"].([]any)
	for _, value := range providerRows {
		provider, ok := value.(map[string]any)
		if !ok || stringField(provider, "sourceId") != sourceID {
			continue
		}
		id := stringField(provider, "id")
		if id == "" {
			continue
		}
		facts[id] = nativeProviderV3{protocol: stringField(provider, "protocol"),
			baseURL: stringField(provider, "baseUrl"), credential: stringField(provider, "credentialRef")}
	}
	blocked, _ := account["blockedAliases"].(map[string]any)
	var rows []map[string]any
	seen := map[string]bool{}
	for _, row := range modelRows(active) {
		if stringField(row, "sourceId") != sourceID {
			continue
		}
		alias := stringField(row, "alias")
		if alias == "" || seen[alias] {
			continue
		}
		if status := stringField(blocked, alias); status != "" && status != "available" {
			continue
		}
		if _, ok := facts[stringField(row, "providerId")]; !ok {
			continue
		}
		seen[alias] = true
		rows = append(rows, row)
	}
	return facts, rows
}

func directPendingBlocks(account map[string]any, sourceID string) bool {
	active, _ := account["active"].(map[string]any)
	pending, _ := account["pending"].(map[string]any)
	applied := map[string]bool{}
	for _, row := range modelRows(active) {
		if rowSource := stringField(row, "sourceId"); sourceID == "" || rowSource != sourceID {
			continue
		}
		if alias := stringField(row, "alias"); alias != "" {
			applied[alias] = true
		}
	}
	for _, row := range modelRows(pending) {
		if rowSource := stringField(row, "sourceId"); sourceID == "" || rowSource != sourceID {
			continue
		}
		// Kimi discovers the provider catalog after a manual save. Those
		// unconfirmed discoveries are not part of our requested direct import.
		if stringField(row, "provenance") == "catalog" {
			continue
		}
		if alias := stringField(row, "alias"); alias != "" && !applied[alias] {
			return true
		}
	}
	return false
}

// findDirectProvider matches a spec provider to an applied provider fact by the
// pair Kimi itself keeps stable: protocol and normalised endpoint.
func findDirectProvider(facts map[string]nativeProviderV3, spec DirectProviderSpec) (string, nativeProviderV3, bool) {
	ids := make([]string, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	wanted := normalizeDirectURL(spec.BaseURL, spec.Protocol)
	for _, id := range ids {
		fact := facts[id]
		if fact.protocol != spec.Protocol {
			continue
		}
		if normalizeDirectURL(fact.baseURL, fact.protocol) == wanted {
			return id, fact, true
		}
	}
	return "", nativeProviderV3{}, false
}

func findDirectModelRow(rows []map[string]any, providerID, model string) (map[string]any, bool) {
	for _, row := range rows {
		if stringField(row, "providerId") == providerID && stringField(row, "model") == model {
			return row, true
		}
	}
	return nil, false
}

// directModelTerms is the declaration a generated config model carries, which
// is what a runtime page's model row is compared against.
func directModelTerms(declared map[string]any) map[string]any {
	entry := map[string]any{
		"provider":       stringField(declared, "provider"),
		"model":          stringField(declared, "model"),
		"maxContextSize": declared["maxContextSize"],
	}
	for _, name := range []string{"maxOutputSize", "capabilities", "supportEfforts", "defaultEffort"} {
		if value, ok := declared[name]; ok {
			entry[name] = value
		}
	}
	return entry
}

func directWantedModelTerms(model DirectModel, providerID string) map[string]any {
	terms := map[string]any{
		"provider": providerID, "model": model.Model,
		"maxContextSize": model.MaxContextSize, "capabilities": model.Capabilities,
	}
	if model.MaxOutputSize > 0 {
		terms["maxOutputSize"] = model.MaxOutputSize
	}
	if len(model.SupportEfforts) > 0 {
		terms["supportEfforts"] = model.SupportEfforts
	}
	if model.DefaultEffort != "" {
		terms["defaultEffort"] = model.DefaultEffort
	}
	return terms
}

// verifyDirectSource checks the whole spec against one applied custom source
// and the config Kimi generated from it. It answers a state and reason for the
// first hard failure, or the logical-id to alias mapping when everything holds.
func verifyDirectSource(account directAccount, source map[string]any, spec *DirectSpec,
	config map[string]any) (map[string]string, []accountCatalogEntry, string, []string, string, string) {
	sourceID := stringField(source, "id")
	facts, rows := directActiveRows(account.account, sourceID)
	modelSection, _ := config["model"].(map[string]any)
	providers, _ := modelSection["providers"].(map[string]any)
	models, _ := modelSection["models"].(map[string]any)
	credentials, _ := config["credentials"].(map[string]any)

	byID := map[string]DirectProviderSpec{}
	for _, provider := range spec.Providers {
		byID[provider.ID] = provider
	}
	aliases := map[string]string{}
	entries := make([]accountCatalogEntry, 0, len(spec.Models))
	usedAliases := map[string]string{}
	missing := make([]string, 0)
	credentialRef := ""
	for _, model := range spec.Models {
		providerSpec, ok := byID[model.ProviderID]
		if !ok {
			missing = append(missing, model.ID)
			continue
		}
		providerID, fact, found := findDirectProvider(facts, providerSpec)
		if !found {
			missing = append(missing, model.ID)
			continue
		}
		row, found := findDirectModelRow(rows, providerID, model.Model)
		if !found {
			missing = append(missing, model.ID)
			continue
		}
		wantedModel := directWantedModelTerms(model, providerID)
		activeModel := make(map[string]any, len(wantedModel))
		for name, value := range wantedModel {
			activeModel[name] = value
		}
		delete(activeModel, "provider")
		if !modelMatches(row, activeModel) {
			missing = append(missing, model.ID)
			continue
		}
		alias := stringField(row, "alias")
		if alias == "" {
			missing = append(missing, model.ID)
			continue
		}
		if existing, duplicate := usedAliases[alias]; duplicate && existing != model.ID {
			return nil, nil, "", nil, IntegrationConflict, "profile_invalid"
		}
		declaredProvider, ok := providers[providerID].(map[string]any)
		if !ok {
			return nil, nil, "", nil, IntegrationWaitingConfig, "config_not_generated"
		}
		if stringField(declaredProvider, "type") != fact.protocol ||
			normalizeDirectURL(stringField(declaredProvider, "baseUrl"), fact.protocol) !=
				normalizeDirectURL(fact.baseURL, fact.protocol) {
			return nil, nil, "", nil, IntegrationConflict, "foreign_provider"
		}
		credential := stringField(declaredProvider, "credential")
		if credential == "" {
			return nil, nil, "", nil, IntegrationWaitingConfig, "credential_not_generated"
		}
		if entry, ok := credentials[credential].(map[string]any); !ok || stringField(entry, "apiKey") == "" {
			return nil, nil, "", nil, IntegrationWaitingConfig, "credential_not_generated"
		}
		declaredModel, ok := models[alias].(map[string]any)
		if !ok {
			return nil, nil, "", nil, IntegrationWaitingConfig, "config_not_generated"
		}
		if !modelMatches(declaredModel, wantedModel) {
			return nil, nil, "", nil, IntegrationConflict, "foreign_model"
		}
		usedAliases[alias] = model.ID
		aliases[model.ID] = alias
		if credentialRef == "" {
			credentialRef = credential
		}
		entries = append(entries, accountCatalogEntry{
			alias: alias,
			provider: map[string]any{
				"type":    fact.protocol,
				"baseUrl": normalizeNativeURL(fact.baseURL),
			},
			model: directModelTerms(declaredModel),
		})
	}
	if len(missing) > 0 {
		return nil, nil, "", missing, IntegrationNeedsImport, "models_missing"
	}
	for _, row := range rows {
		if usedAliases[stringField(row, "alias")] == "" {
			return nil, nil, "", nil, IntegrationNeedsImport, "models_removed"
		}
	}
	return aliases, entries, credentialRef, nil, IntegrationReady, ""
}

// verifyDirectAccount is Reconcile for Direct Mode: it verifies the official
// store and the generated config, and never writes either. The only file it
// writes is this tool's own intent.
func (n *NativeIntegration) verifyDirectAccount(options ReconcileOptions) IntegrationResult {
	mode := IntegrationModeDirect
	if !options.Enabled {
		// Direct Mode is disabled by the caller only when it has no spec. The
		// official store still holds the source, so nothing is written: clearing
		// the intent here would lose the record of a working import.
		return IntegrationResult{State: IntegrationDisabled, Reason: "direct_spec_missing", Mode: mode}
	}
	if options.DirectSpec == nil || len(options.DirectSpec.Models) == 0 {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_request", Mode: mode}
	}
	if ok, reason := n.acquireLock(); !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: reason, Mode: mode}
	}
	defer n.releaseLock()
	intent, intentErr := n.readIntent()
	if intentErr == errUnsafeFile {
		return IntegrationResult{State: IntegrationConflict, Reason: "intent_unavailable", Mode: mode}
	}
	store, storeErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr == "absent" {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_store_missing", Mode: mode}
	}
	if storeErr != "" {
		return IntegrationResult{State: IntegrationConflict, Reason: "profile_store_" + storeErr, Mode: mode}
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		// A legacy v1/v2 store has no per-account shape to verify a custom source
		// against; it is reported, never guessed at.
		return IntegrationResult{State: IntegrationConflict, Reason: "direct_mode_unsupported", Mode: mode}
	}
	scopeDigest := ""
	if options.AccountScope != "" {
		scopeDigest = sha256Hex(options.AccountScope)
	}
	chosen, ambiguous := selectDirectAccount(directAccountCandidates(store), scopeDigest, intent)
	if ambiguous {
		return IntegrationResult{State: IntegrationAccountUnknown, Reason: "account_ambiguous", Mode: mode}
	}
	if chosen.account == nil {
		if intent != nil && intent.AccountDigest != "" && scopeDigest != "" && intent.AccountDigest != scopeDigest && !options.AllowAccountRebind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed", Mode: mode}
		}
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_missing", Mode: mode}
	}
	rebind := false
	if intent != nil && intent.AccountDigest != "" && scopeDigest != "" && intent.AccountDigest != scopeDigest {
		rebind = options.AllowAccountRebind && sha256Hex(chosen.key) == scopeDigest
		if !rebind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed", Mode: mode}
		}
	}
	if intent != nil && intent.ProfileKeyDigest != "" && !rebind && sha256Hex(chosen.key) != intent.ProfileKeyDigest {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed", Mode: mode}
	}
	directSourceID := options.DirectSourceID
	if directSourceID == "" && !rebind {
		directSourceID = intentDirectSource(intent)
	}
	source, found := directSourceOf(chosen.sources, directSourceID)
	if !found {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_missing", Mode: mode}
	}
	sourceID := stringField(source, "id")
	result := IntegrationResult{State: IntegrationReady, Mode: mode, DirectSourceID: sourceID,
		AccountDigest: scopeDigest, ProfileKeyDigest: sha256If(chosen.key)}
	if source["quarantined"] == true || stringField(source, "status") == "disconnected" {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_delisted", Mode: mode,
			DirectSourceID: sourceID}
	}
	if directPendingBlocks(chosen.account, sourceID) {
		return IntegrationResult{State: IntegrationPending, Reason: "native_profile_pending", Mode: mode,
			DirectSourceID: sourceID}
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "absent" {
		return IntegrationResult{State: IntegrationWaitingConfig, Reason: "config_missing", Mode: mode,
			DirectSourceID: sourceID}
	}
	if configErr != "" {
		return IntegrationResult{State: IntegrationConflict, Reason: "config_" + configErr, Mode: mode,
			DirectSourceID: sourceID}
	}
	aliases, _, credentialRef, missing, state, reason := verifyDirectSource(chosen, source, options.DirectSpec, config)
	if state != IntegrationReady {
		return IntegrationResult{State: state, Reason: reason, MissingModels: missing, Mode: mode,
			DirectSourceID: sourceID}
	}
	result.Aliases = aliases
	result.CredentialRef = credentialRef
	result.Imported = true
	result.Applied = true
	sameAccount := scopeDigest != "" && intent != nil && intent.AccountDigest == scopeDigest
	next := &nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: n.sourceID, OwnerID: n.ownerID,
		KimiHome: n.kimiDir, Mode: IntegrationModeDirect, DirectSourceID: sourceID,
		AccountDigest:    firstNonEmpty(scopeDigest, intentAccount(intent)),
		ProfileKeyDigest: firstNonEmpty(sha256If(chosen.key), sameAccountDigest(sameAccount, intent)),
		CredentialRef:    credentialRef, Aliases: aliases,
		Active:     true,
		Generation: intentGeneration(intent) + 1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if intentSignature(intent) != intentSignature(next) {
		if err := n.writeIntent(next); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error(), Mode: mode,
				DirectSourceID: sourceID}
		}
	}
	return result
}

// directCatalog is accountCatalog for Direct Mode. It returns the terms a
// running page has to report before the runtime can be called adopted.
func (n *NativeIntegration) directCatalog(settings ControllerSettings, scope string) ([]accountCatalogEntry, string, bool) {
	if settings.DirectSpec == nil || len(settings.DirectSpec.Models) == 0 {
		return nil, "invalid_request", false
	}
	store, storeErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr != "" {
		return nil, "profile_store_" + storeErr, storeErr == "absent"
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		return nil, "direct_mode_unsupported", true
	}
	intent, _ := n.readIntent()
	chosen, ambiguous := selectDirectAccount(directAccountCandidates(store), sha256If(scope), intent)
	if ambiguous {
		return nil, "account_ambiguous", false
	}
	if chosen.account == nil {
		return nil, "profile_missing", true
	}
	source, found := directSourceOf(chosen.sources, intentDirectSource(intent))
	if !found {
		return nil, "profile_missing", true
	}
	sourceID := stringField(source, "id")
	if source["quarantined"] == true || stringField(source, "status") == "disconnected" {
		return nil, "profile_delisted", true
	}
	if directPendingBlocks(chosen.account, sourceID) {
		return nil, "native_profile_pending", false
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr != "" {
		return nil, "config_" + configErr, false
	}
	_, entries, _, _, state, reason := verifyDirectSource(chosen, source, settings.DirectSpec, config)
	if state != IntegrationReady {
		return nil, reason, state == IntegrationNeedsImport
	}
	return entries, "", false
}

// gradeDirectImport turns a direct import's page answer into a state backed by
// a Direct Mode reconcile. As with the gateway, `ok` from the page is not
// "applied": the store and the generated config decide that.
func (c *Controller) gradeDirectImport(result ImportResult, expectedAccount string, settings ControllerSettings, child *Child) ImportResult {
	if settings.integrationMode() != IntegrationModeDirect {
		return c.gradeImport(result, expectedAccount, child)
	}
	if result.State == "" {
		result.State = importState(result)
	}
	if !result.OK {
		return result
	}
	c.recordImportReload(child, result.RequiresRestart)
	integration := c.reconcileAfterImport(expectedAccount)
	result.Readback = integration.Readback
	result.Adopted = integration.Adopted
	if integration.Reason == "account_changed" {
		result.OK = false
		result.State, result.Reason = ImportStateAccountChanged, NativeAccountChanged
		result.Applied, result.Adopted = false, false
		return result
	}
	if result.RequiresRestart {
		result.State = ImportStateRequiresRestart
		result.Reason = firstNonEmpty(result.Reason, NativeRequiresRestart)
		result.Applied = integration.Applied
		return result
	}
	if result.RouteCleanupPending && !result.RequiresRestart {
		result.State = ImportStateAccepted
		result.Reason = "route_cleanup_pending"
		result.Applied = integration.Applied
		return result
	}
	if integration.Applied && integration.Mode == IntegrationModeDirect {
		result.State = ImportStateApplied
		result.Applied = true
		return result
	}
	result.State = ImportStateAccepted
	result.Applied = false
	result.Reason = firstNonEmpty(integration.Reason, result.Reason, "native_import_not_verified")
	return result
}

package kimi

import (
	"context"
	"errors"
)

// accountModelConfig reads the models the running application has loaded.
//
// Two official surfaces answer this, and the released build moved from the first
// to the second:
//
//   - `kimiAgentAPI.getByokProfile()` on the Work shell page, which is what the
//     build whose store held `profiles` exposed.
//   - `byokSources.list()` on the 3.2.28 Work shell or desktop page.
//
// The legacy call is asked first so a build that still answers it keeps being
// read from the place it always was; the second is a read-only call either way.
func (a *cdpActions) accountModelConfig(ctx context.Context, page accountPage) (accountModelConfig, error) {
	if page.credentials || page.sessionID == "" {
		return accountModelConfig{}, errors.New(AccountErrUnsupported)
	}
	if config, err := a.legacyAccountModelConfig(ctx, page); err == nil {
		return config, nil
	}
	return a.accountModelConfigV3(ctx, page)
}

func (a *cdpActions) legacyAccountModelConfig(ctx context.Context, page accountPage) (accountModelConfig, error) {
	value, err := a.evaluate(ctx, page.sessionID, accountModelConfigExpression)
	if err != nil {
		return accountModelConfig{}, errors.New(AccountErrPageTimeout)
	}
	raw, _ := value.(map[string]any)
	if ok, _ := raw["ok"].(bool); !ok {
		return accountModelConfig{}, errors.New(AccountErrUnavailable)
	}
	profile, _ := raw["profile"].(map[string]any)
	return accountProfileConfig(profile)
}

// The Work shell exposes the read-only model source list in 3.2.28. The desktop
// target is a fallback for builds that expose the same list on clawBridgeAPI.
func (a *cdpActions) accountModelConfigV3(ctx context.Context, page accountPage) (accountModelConfig, error) {
	mode, directSourceID := a.integrationRoute()
	for _, session := range a.modelSessions(ctx, page) {
		value, err := a.evaluate(ctx, session, accountModelStoreV3Expression)
		if err != nil {
			continue
		}
		raw, _ := value.(map[string]any)
		if ok, _ := raw["ok"].(bool); !ok {
			continue
		}
		if config, err := accountStoreV3ConfigFor(raw, mode, directSourceID); err == nil {
			return config, nil
		}
	}
	return accountModelConfig{}, errors.New(AccountErrUnavailable)
}

// modelSessions is the page the caller named and, after it, the claw page. Only
// allowlisted desktop targets are ever attached to.
func (a *cdpActions) modelSessions(ctx context.Context, page accountPage) []string {
	out := []string{page.sessionID}
	targets, err := a.targets(ctx)
	if err != nil {
		return out
	}
	for _, target := range targets {
		if target.path != "/desktopclaw.html" || target.sessionID == "" || target.sessionID == page.sessionID {
			continue
		}
		out = append(out, target.sessionID)
	}
	return out
}

func accountProfileConfig(profile map[string]any) (accountModelConfig, error) {
	rows := profileRows(profile)
	models := map[string]any{}
	providers := map[string]any{}
	delisted := uniqueSorted(profile["delisted"])
	for _, row := range rows {
		alias, providerID := stringField(row, "alias"), stringField(row, "providerId")
		if containsString(delisted, alias) {
			continue
		}
		if _, duplicate := models[alias]; duplicate {
			return accountModelConfig{}, errors.New(AccountErrUnsupported)
		}
		models[alias] = modelEntry(row)
		providers[providerID] = map[string]any{"type": providerType(row), "baseUrl": profile["baseUrl"], "credential": profile["credentialRef"]}
	}
	return accountModelConfig{providers: providers, models: models, credentialPublished: true}, nil
}

// accountStoreV3Config turns the claw surface's read-only answer into the same
// two sections the legacy profile produced.
//
// Two differences are deliberate. That answer carries no credential reference —
// the page reports whether a key is configured, not which reference holds it —
// so the provider it describes is compared on the terms it does publish. And a
// model counts only when the surface reports it `available`, which is the page's
// own word for applied and not blocked; `pending`, `unconfirmed`, `delisted` and
// `disconnected` are not.
func accountStoreV3Config(raw map[string]any) (accountModelConfig, error) {
	return accountStoreV3ConfigFor(raw, IntegrationModeGateway, "")
}

// accountStoreV3ConfigFor reads either the gateway's kth source or, in Direct
// Mode, the managed custom source. In Direct Mode the runtime check must look
// at that source: reading byok-kth there would report the models as missing even
// when the direct import is loaded.
func accountStoreV3ConfigFor(raw map[string]any, mode, directSourceID string) (accountModelConfig, error) {
	sources, _ := raw["sources"].([]any)
	source := kthSourceRow(sources)
	if mode == IntegrationModeDirect {
		source = directSourceRow(sources, directSourceID)
	}
	if source == nil {
		return accountModelConfig{}, errors.New(AccountErrUnavailable)
	}
	if quarantined, _ := source["quarantined"].(bool); quarantined {
		return accountModelConfig{}, errors.New(AccountErrUnavailable)
	}
	providers := map[string]any{}
	rows, _ := source["providers"].([]any)
	for _, row := range rows {
		provider, ok := row.(map[string]any)
		if !ok {
			continue
		}
		id, protocol := stringField(provider, "id"), stringField(provider, "protocol")
		baseURL := normalizeNativeURL(stringField(provider, "baseUrl"))
		if id == "" || protocol == "" || baseURL == "" {
			continue
		}
		providers[id] = map[string]any{"type": protocol, "baseUrl": baseURL}
	}
	if len(providers) == 0 {
		return accountModelConfig{}, errors.New(AccountErrUnavailable)
	}
	models := map[string]any{}
	modelRows, _ := source["models"].([]any)
	for _, row := range modelRows {
		entry, ok := row.(map[string]any)
		if !ok {
			continue
		}
		alias, providerID := stringField(entry, "alias"), stringField(entry, "providerId")
		if alias == "" || providers[providerID] == nil {
			continue
		}
		if stringField(entry, "status") != "available" {
			continue
		}
		if _, duplicate := models[alias]; duplicate {
			return accountModelConfig{}, errors.New(AccountErrUnsupported)
		}
		models[alias] = modelEntry(entry)
	}
	return accountModelConfig{providers: providers, models: models}, nil
}

func kthSourceRow(sources []any) map[string]any {
	var fallback map[string]any
	for _, row := range sources {
		source, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if stringField(source, "id") == "byok-kth" {
			return source
		}
		if stringField(source, "kind") == "kth" && fallback == nil {
			fallback = source
		}
	}
	return fallback
}

// directSourceRow finds the managed direct custom source in a page snapshot. The
// id may have changed across a sync, so the display name is the fallback.
func directSourceRow(sources []any, sourceID string) map[string]any {
	if sourceID != "" {
		for _, row := range sources {
			source, ok := row.(map[string]any)
			if ok && stringField(source, "id") == sourceID && stringField(source, "kind") == "custom" {
				return source
			}
		}
	}
	for _, row := range sources {
		source, ok := row.(map[string]any)
		if ok && stringField(source, "kind") == "custom" && stringField(source, "displayName") == DirectDisplayName {
			return source
		}
	}
	return nil
}

// accountModelConfigExpression reads the released build's own profile call.
// Only declaration terms leave the page: the payload also carries a token, a
// token mask and an endpoint, and none of them are picked.
const accountModelConfigExpression = `(async function(){
try{
var api=globalThis.kimiAgentAPI;
if(!api||typeof api.getStatus!=="function"||typeof api.getByokProfile!=="function")return {ok:false};
var status=await api.getStatus();
if(!status||status.state!=="ready")return {ok:false};
var payload=await api.getByokProfile();
function record(value){return value&&typeof value==="object"&&!Array.isArray(value);}
if(!record(payload)||!record(payload.profile))return {ok:false};
var source=payload.profile;
if(source.id!=="byok-kth"||source.kind!=="kth"||typeof source.baseUrl!=="string"||typeof source.credentialRef!=="string")return {ok:false};
if(!Array.isArray(source.models)||source.models.length>256)return {ok:false};
if(source.pending!==undefined&&(!Array.isArray(source.pending)||source.pending.length!==0))return {ok:false};
function pick(value,keys){
  if(!record(value))throw new Error();
  var result={};
  for(var key of keys){
    if(!Object.prototype.hasOwnProperty.call(value,key))continue;
    var field=value[key];
    if(Array.isArray(field)){
      if(field.length>256||field.some(function(item){return typeof item!=="string"||item.length>1024;}))throw new Error();
      result[key]=field.slice();
    }else if(field===null||typeof field==="number"||typeof field==="boolean"||typeof field==="string"&&field.length<=4096){
      result[key]=field;
    }else throw new Error();
  }
  return result;
}
var profile=pick(source,["baseUrl","credentialRef","delisted"]);
profile.models=source.models.map(function(value){
  var row=pick(value,["alias","providerId","model","family","maxContextSize","maxOutputSize","defaultEffort","capabilities","supportEfforts"]);
  if(typeof row.alias!=="string"||!row.alias.startsWith("byok-kth-")||typeof row.providerId!=="string"||!row.providerId.startsWith("byok-kth-")||typeof row.model!=="string"||typeof row.maxContextSize!=="number"||!Number.isFinite(row.maxContextSize)||row.maxContextSize<=0)throw new Error();
  return row;
});
status=await api.getStatus();
return status&&status.state==="ready"?{ok:true,profile:profile}:{ok:false};
}catch(error){return {ok:false};}
})()`

// accountModelStoreV3Expression reads the source list without returning credentials.
const accountModelStoreV3Expression = `(async function(){
try{
var sources=globalThis.kimiAgentAPI?.byokSources??globalThis.clawBridgeAPI?.byokSources;
if(!sources||typeof sources.list!=="function")return {ok:false};
var reply=await sources.list();
var snapshot=reply?.ok===true?reply.value:null;
function record(value){return value&&typeof value==="object"&&!Array.isArray(value);}
if(!record(snapshot)||!Array.isArray(snapshot.sources)||snapshot.sources.length>64)return {ok:false};
function pick(value,keys){
  if(!record(value))throw new Error();
  var result={};
  for(var key of keys){
    if(!Object.prototype.hasOwnProperty.call(value,key))continue;
    var field=value[key];
    if(Array.isArray(field)){
      if(field.length>256||field.some(function(item){return typeof item!=="string"||item.length>1024;}))throw new Error();
      result[key]=field.slice();
    }else if(field===null||typeof field==="number"||typeof field==="boolean"||typeof field==="string"&&field.length<=4096){
      result[key]=field;
    }else throw new Error();
  }
  return result;
}
var rows=snapshot.sources.map(function(source){
  var head=pick(source,["id","kind","quarantined","displayName","revision"]);
  if(typeof head.id!=="string"||typeof head.kind!=="string")throw new Error();
  head.providers=(Array.isArray(source.providers)?source.providers:[]).map(function(provider){
    var row=pick(provider,["id","protocol","baseUrl","keyConfigured"]);
    if(typeof row.id!=="string"||typeof row.protocol!=="string"||typeof row.baseUrl!=="string")throw new Error();
    return row;
  });
  head.models=(Array.isArray(source.models)?source.models:[]).slice(0,256).map(function(model){
    var row=pick(model,["alias","providerId","model","family","maxContextSize","maxOutputSize","defaultEffort","capabilities","supportEfforts","status"]);
    if(typeof row.alias!=="string"||typeof row.providerId!=="string"||typeof row.model!=="string")throw new Error();
    return row;
  });
  return head;
});
return {ok:true,applyState:typeof snapshot.applyState==="string"?snapshot.applyState:null,sources:rows};
}catch(error){return {ok:false};}
})()`

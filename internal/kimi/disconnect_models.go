package kimi

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

func (a *cdpActions) DisconnectModels(ctx context.Context, request DisconnectRequest) (DisconnectResult, error) {
	if a.kimiHome == "" || !filepath.IsAbs(a.kimiHome) {
		return DisconnectResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	current := AccountScope(a.kimiHome)
	if current == "" {
		return DisconnectResult{State: ImportStateWaitingLogin, Reason: NativeWaitingLogin}, nil
	}
	if request.ExpectedAccountKey != "" && request.ExpectedAccountKey != current {
		return DisconnectResult{State: ImportStateAccountChanged, Reason: NativeAccountChanged}, nil
	}
	settings, opened, failure, err := a.resolveImportTarget(ctx, true, true)
	if err != nil {
		return DisconnectResult{}, err
	}
	if failure != nil {
		return DisconnectResult{State: failure.State, Reason: failure.Reason}, nil
	}
	if err := a.waitForDaimonNotBooting(ctx, nil); err != nil {
		state, reason, surfaced := classifyActionError(err)
		result := DisconnectResult{State: state, Reason: reason, Opened: opened}
		a.closeDisconnectIfOpened(ctx, opened, &result)
		return result, surfaced
	}
	value, err := a.evaluate(ctx, settings.sessionID, disconnectModelsExpression(request.DirectSourceID))
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			a.dropSession(settings.targetID)
		}
		state, reason, surfaced := classifyActionError(err)
		result := DisconnectResult{State: state, Reason: reason, Opened: opened}
		a.closeDisconnectIfOpened(ctx, opened, &result)
		return result, surfaced
	}
	result := decodeDisconnectAnswer(value, opened)
	a.closeDisconnectIfOpened(ctx, opened, &result)
	return result, nil
}

func decodeDisconnectAnswer(value any, opened bool) DisconnectResult {
	answer, _ := value.(map[string]any)
	result := DisconnectResult{Opened: opened}
	if count, ok := answer["sources"].(float64); ok && count > 0 {
		result.Sources = int(count)
	}
	if ok, _ := answer["ok"].(bool); ok {
		result.OK = true
		result.State = "disconnected"
		return result
	}
	code, _ := answer["code"].(string)
	normalized := strings.ReplaceAll(safeActionCode(code), "-", "_")
	switch normalized {
	case "account_changed":
		result.State = ImportStateAccountChanged
		result.Reason = NativeAccountChanged
	case "deferred":
		result.State = ImportStateRequiresRestart
		result.Reason = "model_disconnect_deferred"
	default:
		result.State = ImportStateFailed
		result.Reason = disconnectFailureReason(code)
	}
	return result
}

func disconnectFailureReason(code string) string {
	normalized := strings.ReplaceAll(safeActionCode(code), "-", "_")
	normalized = strings.TrimPrefix(normalized, "model_disconnect_")
	if normalized == "" {
		normalized = "failed"
	}
	reason := "model_disconnect_" + normalized
	if len(reason) > 100 {
		reason = reason[:100]
	}
	return reason
}

func (a *cdpActions) closeDisconnectIfOpened(ctx context.Context, opened bool, result *DisconnectResult) {
	if !opened || result == nil {
		return
	}
	closeCtx, cancel := closeContext(ctx)
	defer cancel()
	closed, err := a.CloseWorkSettings(closeCtx)
	value := err == nil && closed.OK
	result.Closed = &value
}

func disconnectModelsExpression(directSourceID string) string {
	return `(async(directSourceID)=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
if(!sources||typeof sources.list!=="function"||typeof sources.disconnect!=="function"){
  return {ok:false,code:"unsupported"};
}
const unwrap=result=>result?.ok===true?result.value:null;
const inactive=source=>source?.quarantined===true||source?.status==="disconnected";
const settledApplyState=state=>state==null||state==="idle"||state==="applied"||state==="ready";
const loopback=value=>/^http:\/\/(?:127\.0\.0\.1|\[::1\]):\d+\/v1(?:\/models\/api\.json)?\/?$/i.test(String(value??""));
const managedGateway=source=>{
  if(!source||(source.id!=="byok-kth"&&source.kind!=="kth"))return false;
  const registry=typeof source.registryUrl==="string"?source.registryUrl:"";
  const providers=Array.isArray(source.providers)?source.providers:[];
  if(registry)return loopback(registry)&&providers.every(provider=>loopback(provider?.baseUrl));
  return providers.length>0&&providers.every(provider=>loopback(provider?.baseUrl));
};
const managedDirect=source=>source?.kind==="custom"&&
  ((directSourceID&&source.id===directSourceID)||source.displayName==="KimiWork + 直连");
const managed=source=>!inactive(source)&&(managedGateway(source)||managedDirect(source));
const otherForeignPending=snapshot=>snapshot.sources.some(source=>!managed(source)&&
  Array.isArray(source.models)&&source.models.some(model=>model?.status==="pending"));
const read=async()=>{
  const snapshot=unwrap(await sources.list());
  if(!snapshot||!Array.isArray(snapshot.sources)||typeof snapshot.scopeToken!=="string"||
    (snapshot.accountRevision!==null&&typeof snapshot.accountRevision!=="string"))return null;
  return snapshot;
};
const initial=await read();
if(!initial)return {ok:false,code:"contract-changed"};
const initialScope=initial.scopeToken;
if(directSourceID&&initial.sources.some(source=>source.id===directSourceID&&source.kind!=="custom"&&!inactive(source))){
  return {ok:false,code:"source-conflict"};
}
const ids=initial.sources.filter(managed).map(source=>source.id).filter(id=>typeof id==="string"&&id);
let disconnected=0;
let lastStatus="";
for(const id of ids){
  const current=await read();
  if(!current)return {ok:false,code:"contract-changed",sources:disconnected};
  if(current.scopeToken!==initialScope)return {ok:false,code:"account-changed",sources:disconnected};
  const source=current.sources.find(candidate=>candidate.id===id);
  if(!source||!managed(source))continue;
  if(typeof current.accountRevision!=="string"||!current.accountRevision||
    typeof source.revision!=="string"||!source.revision){
    return {ok:false,code:"contract-changed",sources:disconnected};
  }
  const reply=await sources.disconnect({sourceId:source.id,accountRevision:current.accountRevision,
    sourceRevision:source.revision});
  if(reply?.ok!==true)return {ok:false,code:reply?.reasonCode||"failed",sources:disconnected};
  disconnected++;
  if(reply.value&&reply.value.status!==undefined&&reply.value.status!==null&&typeof reply.value.status!=="string"){
    return {ok:false,code:"contract-changed",sources:disconnected};
  }
  let status=typeof reply.value?.status==="string"?reply.value.status:"";
  if(status==="pending"&&!otherForeignPending(current)&&typeof sources.applyPending==="function"&&
    typeof reply.value?.accountRevision==="string"&&reply.value.accountRevision){
    const latest=await read();
    if(!latest)return {ok:false,code:"contract-changed",sources:disconnected};
    if(latest.scopeToken!==initialScope)return {ok:false,code:"account-changed",sources:disconnected};
    if(latest.accountRevision!==reply.value.accountRevision){
      return {ok:false,code:"byok-source-stale",sources:disconnected};
    }
    const applied=await sources.applyPending(reply.value.accountRevision);
    if(applied?.ok!==true)return {ok:false,code:applied?.reasonCode||"apply-failed",sources:disconnected};
    if(applied.value&&applied.value.status!==undefined&&applied.value.status!==null&&typeof applied.value.status!=="string"){
      return {ok:false,code:"contract-changed",sources:disconnected};
    }
    status=typeof applied.value?.status==="string"?applied.value.status:"";
  }
  if(status==="failed"||status==="apply-failed"){
    return {ok:false,code:"apply-failed",sources:disconnected};
  }
  if(status!==""&&status!=="applied"&&status!=="deferred"&&status!=="pending"){
    return {ok:false,code:"contract-changed",sources:disconnected};
  }
  lastStatus=status;
}
let verified=await read();
if(!verified)return {ok:false,code:"contract-changed",sources:disconnected};
if(verified.scopeToken!==initialScope)return {ok:false,code:"account-changed",sources:disconnected};
if(verified.sources.some(managed))return {ok:false,code:"not-verified",sources:disconnected};
if(lastStatus===""&&!settledApplyState(verified.applyState)&&!otherForeignPending(verified)&&
  typeof sources.applyPending==="function"&&typeof verified.accountRevision==="string"&&verified.accountRevision){
  const applied=await sources.applyPending(verified.accountRevision);
  if(applied?.ok!==true)return {ok:false,code:applied?.reasonCode||"apply-failed",sources:disconnected};
  if(applied.value&&applied.value.status!==undefined&&applied.value.status!==null&&typeof applied.value.status!=="string"){
    return {ok:false,code:"contract-changed",sources:disconnected};
  }
  const status=typeof applied.value?.status==="string"?applied.value.status:"";
  if(status==="failed"||status==="apply-failed"){
    return {ok:false,code:"apply-failed",sources:disconnected};
  }
  if(status!==""&&status!=="applied"&&status!=="deferred"&&status!=="pending"){
    return {ok:false,code:"contract-changed",sources:disconnected};
  }
  lastStatus=status;
  verified=await read();
  if(!verified)return {ok:false,code:"contract-changed",sources:disconnected};
  if(verified.scopeToken!==initialScope)return {ok:false,code:"account-changed",sources:disconnected};
  if(verified.sources.some(managed))return {ok:false,code:"not-verified",sources:disconnected};
}
if(verified.applyState==="apply-failed"||verified.applyState==="failed"){
  return {ok:false,code:"apply-failed",sources:disconnected};
}
if(lastStatus==="deferred"||lastStatus==="pending"||
  (!settledApplyState(verified.applyState)&&(disconnected>0||!otherForeignPending(verified)))){
  return {ok:false,code:"deferred",sources:disconnected};
}
return {ok:true,sources:disconnected};
})(` + quoteJS(directSourceID) + `)`
}

package kimi

import (
	"context"
	"errors"
	"time"
)

// Electron restarts Daimon right after an applied `byokSources.disconnect` and
// marks every running turn `#unknown` until its own status and blueprint RPCs
// finish. A `save` or `disconnect` sent inside that roughly 130-500ms window
// writes its pending bytes cleanly and then answers `status: "deferred"`, with
// the pending revision still sitting on `accountRevision`. Polling
// `applyPending` after the window applies it in place, without restarting Kimi.
//
// The page keeps every expression single-shot: goja has no timers, and the
// release tests assert exact call sequences. The bounded wait lives here.
var deferredApplyWindow = 3 * time.Second

// applyPendingExpression asks the page to apply the pending revision this
// import just wrote, but only while the account and the revision are the ones
// the import observed. Every answer is a code the caller can act on.
func applyPendingExpression(scopeToken, expectedRevision string) string {
	return `(async(scopeToken, expectedRevision)=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
if(!sources||typeof sources.list!=="function"||typeof sources.applyPending!=="function"){
  return {ok:false,code:"unsupported"};
}
const unwrap=result=>result?.ok===true?result.value:null;
const current=unwrap(await sources.list());
if(!current||typeof current.scopeToken!=="string")return {ok:false,code:"contract-changed"};
if(current.scopeToken!==scopeToken)return {ok:false,code:"account-changed"};
if(current.accountRevision!==expectedRevision)return {ok:false,code:"stale"};
const applied=await sources.applyPending(expectedRevision);
if(!applied?.ok)return {ok:false,code:applied?.reasonCode||"apply-failed"};
return {ok:true,status:typeof applied.value?.status==="string"?applied.value.status:""};
})(` + quoteJS(scopeToken) + `,` + quoteJS(expectedRevision) + `)`
}

// retryDeferredApply waits out the post-`ready` reconciliation window and
// applies the revision the deferred save left behind. The first wait comes
// before the first poll because the save just saw `busy`: polling immediately
// would only read the same window again.
//
// It answers one of "applied", "deferred", "account-changed", "stale",
// "unsupported" or "apply-failed". Only "applied" allows the caller to clear
// the restart prompt; every other outcome preserves it, so a real running turn
// is never interrupted.
func (a *cdpActions) retryDeferredApply(ctx context.Context, settings actionTarget, scopeToken, expectedRevision string) (string, error) {
	deadline := time.Now().Add(deferredApplyWindow)
	for {
		if err := a.waitStep(ctx); err != nil {
			return "", err
		}
		value, err := a.evaluate(ctx, settings.sessionID, applyPendingExpression(scopeToken, expectedRevision))
		if err != nil {
			if errors.Is(err, ErrSessionInvalid) {
				a.dropSession(settings.targetID)
			}
			return "", err
		}
		answer, _ := value.(map[string]any)
		ok, _ := answer["ok"].(bool)
		if ok {
			status, _ := answer["status"].(string)
			switch status {
			case "applied":
				return "applied", nil
			case "apply-failed", "failed":
				return "apply-failed", nil
			}
			if time.Now().Before(deadline) {
				continue
			}
			return "deferred", nil
		}
		code, _ := answer["code"].(string)
		switch code {
		case "account-changed", "stale", "unsupported":
			return code, nil
		case "apply-failed":
			return "apply-failed", nil
		}
		return "deferred", nil
	}
}

// settleDisconnectJS is the one settlement every page-side
// `byokSources.disconnect` call shares. Electron answers `status: "pending"`
// while a restarted Daimon prepares the write and `"deferred"` inside its
// post-ready window; the revision is then applied in place, or handed to Go so
// the bounded retry can apply it without restarting Kimi. The caller's `current`
// snapshot is the account evidence, and `otherPending` says another source's
// confirmation is in flight and must not be applied on its behalf.
const settleDisconnectJS = `
const settleDisconnect=async(reply,current,otherPending)=>{
if(reply?.ok!==true)return {ok:false,settled:false};
const statusOf=value=>typeof value?.status==="string"?value.status:"";
let status=statusOf(reply.value);
const revision=typeof reply.value?.accountRevision==="string"?reply.value.accountRevision:"";
if(status==="pending"&&!otherPending&&typeof sources.applyPending==="function"&&revision){
  const latest=unwrap(await sources.list());
  if(latest&&latest.scopeToken===current.scopeToken&&latest.accountRevision===revision){
    const applied=await sources.applyPending(revision);
    if(applied?.ok!==true)return {ok:false,settled:false};
    status=statusOf(applied.value);
  }
}
if(status===""||status==="applied")return {ok:true,settled:true};
if(status==="deferred"&&typeof sources.applyPending==="function"&&revision){
  return {ok:true,settled:false,applyScopeToken:current.scopeToken,applyRevision:revision};
}
return {ok:true,settled:false};
};`

// disconnectDirectSourceExpression disconnects one managed custom source by the
// id the import recorded, for the cleanup a deferred apply delayed. It repeats
// the ownership rules the import itself uses: a source of another kind, or one
// already inactive, is never touched.
func disconnectDirectSourceExpression(sourceID string) string {
	return `(async(sourceID)=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
if(!sourceID)return {ok:true,settled:true};
if(!sources||typeof sources.list!=="function"||typeof sources.disconnect!=="function"){
  return {ok:false,settled:false};
}
const unwrap=result=>result?.ok===true?result.value:null;` + settleDisconnectJS + `
const current=unwrap(await sources.list());
if(!current||!Array.isArray(current.sources)||typeof current.scopeToken!=="string"){
  return {ok:false,settled:false};
}
let direct=current.sources.find(source=>source.id===sourceID);
if(!direct)direct=current.sources.find(source=>source.kind==="custom"&&source.displayName==="` + DirectDisplayName + `");
if(!direct)return {ok:true,settled:true};
if(direct.kind!=="custom")return {ok:false,settled:false};
if(direct.quarantined===true||direct.status==="disconnected")return {ok:true,settled:true};
const otherPending=current.sources.some(source=>source.id!==direct.id&&
  (Array.isArray(source.models)?source.models:[]).some(model=>model.status==="pending"));
const reply=await sources.disconnect({sourceId:direct.id,accountRevision:current.accountRevision,
  sourceRevision:direct.revision??null});
return await settleDisconnect(reply,current,otherPending);
})(` + quoteJS(sourceID) + `)`
}

// disconnectDirectSourceOnce runs one cleanup attempt. A transport failure is
// not an import failure: the replacement is already applied, and the next sync
// retries the cleanup from the store. `deferred` says the disconnect itself
// wrote pending bytes, so removing the old source still waits for a restart.
func (a *cdpActions) disconnectDirectSourceOnce(ctx context.Context, settings actionTarget, sourceID string) (settled, deferred bool) {
	value, err := a.evaluate(ctx, settings.sessionID, disconnectDirectSourceExpression(sourceID))
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			a.dropSession(settings.targetID)
		}
		return false, false
	}
	return a.settleCleanupAnswer(ctx, settings, value)
}

// settleCleanupAnswer finishes one cleanup answer. An answer without `settled`
// is the older build's plain `ok`, which already meant the cleanup landed; a
// deferred disconnect carries the revision the bounded retry applies.
func (a *cdpActions) settleCleanupAnswer(ctx context.Context, settings actionTarget, value any) (settled, deferred bool) {
	answer, _ := value.(map[string]any)
	if ok, _ := answer["ok"].(bool); !ok {
		return false, false
	}
	settledNow, hasSettled := answer["settled"].(bool)
	if settledNow || !hasSettled {
		return true, false
	}
	scopeToken, _ := answer["applyScopeToken"].(string)
	revision, _ := answer["applyRevision"].(string)
	if scopeToken == "" || revision == "" {
		return false, false
	}
	outcome, err := a.retryDeferredApply(ctx, settings, scopeToken, revision)
	if err != nil {
		return false, false
	}
	return outcome == "applied", outcome == "deferred"
}

// disconnectGatewayExpression disconnects this installation's loopback gateway
// after a deferred Direct Mode apply, using the same ownership proof as the
// import: the registry names the expected gateway and every provider address is
// loopback. A remote or foreign source answers `foreign-source` and is left
// alone.
func disconnectGatewayExpression(gatewayURL string) string {
	return `(async(expected)=>{
const sources=globalThis.clawBridgeAPI?.byokSources;
const expectedGateway=String(expected??"").trim().replace(/\/models\/api\.json\/?$/i,"").replace(/\/+$/,"").toLowerCase();
if(!expectedGateway)return {ok:false,settled:false};
if(!sources||typeof sources.list!=="function"||typeof sources.disconnect!=="function"){
  return {ok:false,settled:false};
}
const unwrap=result=>result?.ok===true?result.value:null;` + settleDisconnectJS + `
const gatewayAddress=value=>String(value??"").trim().replace(/\/models\/api\.json\/?$/i,"").replace(/\/+$/,"").toLowerCase();
const isLoopback=value=>/^http:\/\/(?:127\.0\.0\.1|localhost|\[::1\]):\d+\//i.test(String(value??""));
const current=unwrap(await sources.list());
if(!current||!Array.isArray(current.sources)||typeof current.scopeToken!=="string"||
  typeof current.accountRevision!=="string"||!current.accountRevision)return {ok:false,settled:false};
const gateway=current.sources.find(source=>source.id==="byok-kth"||source.kind==="kth");
if(!gateway||gateway.quarantined===true||gateway.status==="disconnected")return {ok:true,settled:true};
const providers=Array.isArray(gateway.providers)?gateway.providers:[];
const addresses=[];
for(const provider of providers){if(typeof provider?.baseUrl==="string"&&provider.baseUrl)addresses.push(provider.baseUrl);}
const registry=typeof gateway.registryUrl==="string"?gateway.registryUrl:"";
const owned=registry?gatewayAddress(registry)===expectedGateway&&addresses.every(isLoopback):
  addresses.length>0&&addresses.every(value=>isLoopback(value)&&gatewayAddress(value)===expectedGateway);
if(!owned)return {ok:false,settled:false};
const otherPending=current.sources.some(source=>source.id!==gateway.id&&
  (Array.isArray(source.models)?source.models:[]).some(model=>model.status==="pending"));
const reply=await sources.disconnect({sourceId:gateway.id,accountRevision:current.accountRevision,
  sourceRevision:gateway.revision??null});
return await settleDisconnect(reply,current,otherPending);
})(` + quoteJS(gatewayURL) + `)`
}

// disconnectGatewayOnce runs one gateway cleanup attempt for the same reason a
// direct cleanup is bounded: the replacement is applied and the next sync can
// finish this from the store.
func (a *cdpActions) disconnectGatewayOnce(ctx context.Context, settings actionTarget, gatewayURL string) (settled, deferred bool) {
	value, err := a.evaluate(ctx, settings.sessionID, disconnectGatewayExpression(gatewayURL))
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			a.dropSession(settings.targetID)
		}
		return false, false
	}
	return a.settleCleanupAnswer(ctx, settings, value)
}

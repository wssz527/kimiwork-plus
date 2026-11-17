package kimi

import (
	"context"
	"errors"
)

// The official coordinator publishes ready only after committing its runtime.
// Watching before credential writes ties a non-ready -> ready transition to this
// switch. Status objects carry transport credentials, so only state is retained.
const accountRuntimeProbeGlobal = "__KIMI_WORKPLUS_RUNTIME_PROBE__"

// accountWatchRuntime installs the probe and clears whatever a previous watch
// left. The caller runs it before any credential write: a ready seen after the
// switch is only attributable to the new account if the non-ready window was
// observed from the start.
func (a *cdpActions) accountWatchRuntime(ctx context.Context, page accountPage) error {
	if page.credentials {
		return errors.New(AccountErrUnsupported)
	}
	if page.sessionID == "" {
		return errors.New(AccountErrUnavailable)
	}
	value, err := a.evaluate(ctx, page.sessionID, accountRuntimeWatchExpression)
	if err != nil {
		return errors.New(AccountErrPageTimeout)
	}
	raw, _ := value.(map[string]any)
	if raw == nil {
		return errors.New(AccountErrPageTimeout)
	}
	if ok, _ := raw["ok"].(bool); !ok {
		return errors.New(AccountErrUnsupported)
	}
	return nil
}

// Hot switching requires an observed transition before querying the runtime.
// After a desktop restart, current ready plus a successful query is sufficient.
func (a *cdpActions) accountRuntimeReady(ctx context.Context, page accountPage, requireTransition bool) (bool, error) {
	if page.credentials {
		return false, errors.New(AccountErrUnsupported)
	}
	if page.sessionID == "" {
		return false, errors.New(AccountErrUnavailable)
	}
	raw, err := a.accountRuntimeRead(ctx, page, requireTransition)
	if err != nil {
		return false, err
	}
	if installed, _ := raw["installed"].(bool); !installed {
		if reason, _ := raw["reason"].(string); reason == "unsupported" {
			return false, errors.New(AccountErrUnsupported)
		}
		// The page was reloaded and took the observation with it. A transition
		// that is already lost cannot be recreated, so a caller that needs one is
		// told the wait expired instead of being handed a current `ready`.
		if requireTransition {
			return false, errors.New(AccountErrPageTimeout)
		}
		if err := a.accountWatchRuntime(ctx, page); err != nil {
			return false, err
		}
		if raw, err = a.accountRuntimeRead(ctx, page, false); err != nil {
			return false, err
		}
		if installed, _ := raw["installed"].(bool); !installed {
			return false, errors.New(AccountErrPageTimeout)
		}
	}
	if state, _ := raw["state"].(string); state != "ready" {
		return false, nil
	}
	if current, _ := raw["current"].(string); current != "ready" {
		return false, nil
	}
	if transition, _ := raw["readyAfterNotReady"].(bool); requireTransition && !transition {
		return false, nil
	}
	switch rpc, _ := raw["rpc"].(string); rpc {
	case "ok":
		return true, nil
	case "unsupported":
		// The page hosts kimiAgentAPI but has no rpc entry point, so the runtime
		// cannot be queried on this build at all.
		return false, errors.New(AccountErrUnsupported)
	}
	return false, nil
}

// accountRuntimeRead asks the page for its observation and, once the runtime is
// ready, for the models query that proves the kernel answers.
func (a *cdpActions) accountRuntimeRead(ctx context.Context, page accountPage, requireTransition bool) (map[string]any, error) {
	value, err := a.evaluate(ctx, page.sessionID, accountRuntimeReadyExpression(requireTransition))
	if err != nil {
		return nil, errors.New(AccountErrPageTimeout)
	}
	raw, _ := value.(map[string]any)
	if raw == nil {
		return nil, errors.New(AccountErrPageTimeout)
	}
	return raw, nil
}

// accountStopRuntimeWatch removes the listener and the observation. It is safe to
// call on a page that no longer exists: the caller defers it, and a cleanup that
// failed must not be mistaken for a verified switch.
func (a *cdpActions) accountStopRuntimeWatch(ctx context.Context, page accountPage) error {
	if page.credentials {
		return nil
	}
	if page.sessionID == "" {
		return errors.New(AccountErrUnavailable)
	}
	value, err := a.evaluate(ctx, page.sessionID, accountRuntimeStopExpression)
	if err != nil {
		return errors.New(AccountErrPageTimeout)
	}
	if ok, _ := value.(bool); !ok {
		return errors.New(AccountErrUnsupported)
	}
	return nil
}

// accountRuntimeWatchExpression installs the listener and starts a fresh
// observation. ok:false means the page lacks the official entry points, which is
// a build this probe cannot run on rather than a transient failure.
const accountRuntimeWatchExpression = `(function(){
try{
var api=globalThis.kimiAgentAPI;
if(!api||typeof api.onEvent!=="function"||typeof api.getStatus!=="function")return {ok:false,reason:"unsupported"};
var previous=globalThis.` + accountRuntimeProbeGlobal + `;
if(previous&&typeof previous.dispose==="function"){try{previous.dispose();}catch(error){}}
globalThis.` + accountRuntimeProbeGlobal + `=null;
var probe={state:"",readyAfterNotReady:false,verified:false,record:null,dispose:null};
probe.record=function(status){
  if(!status||typeof status!=="object")return;
  var state=typeof status.state==="string"?status.state.slice(0,64):"";
  if(!state)return;
  var previous=probe.state;
  probe.state=state;
  if(state!=="ready"){probe.readyAfterNotReady=false;probe.verified=false;}
  else if(previous!=="ready"&&previous!==""){probe.readyAfterNotReady=true;}
};
var off=api.onEvent(function(payload){
  try{
    if(!payload||typeof payload!=="object"||payload.t!=="status")return;
    probe.record(payload.status);
  }catch(error){}
});
probe.dispose=typeof off==="function"?off:null;
globalThis.` + accountRuntimeProbeGlobal + `=probe;
return {ok:true};
}catch(error){return {ok:false,reason:"unsupported"};}
})()`

// Polls also record state transitions so a missed push does not lose evidence.
// RPC results stay in the renderer; only their validated shape leaves the page.
const accountRuntimeReadyBody = `async function(requireTransition){
var api=globalThis.kimiAgentAPI;
if(!api||typeof api.getStatus!=="function"||typeof api.onEvent!=="function")return {installed:false,reason:"unsupported"};
var probe=globalThis.` + accountRuntimeProbeGlobal + `;
if(!probe||typeof probe!=="object")return {installed:false,reason:"missing"};
var current=null;
try{current=await api.getStatus();}catch(error){return {installed:true,state:typeof probe.state==="string"?probe.state:"",readyAfterNotReady:probe.readyAfterNotReady===true,rpc:"unreadable"};}
if(typeof probe.record==="function")probe.record(current);
var out={installed:true,state:typeof probe.state==="string"?probe.state:"",readyAfterNotReady:probe.readyAfterNotReady===true};
if(out.state!=="ready")return out;
out.current=current&&typeof current.state==="string"?current.state.slice(0,64):"";
if(out.current!=="ready")return out;
if(requireTransition&&out.readyAfterNotReady!==true)return out;
if(probe.verified===true){out.rpc="ok";return out;}
if(typeof api.rpc!=="function"){out.rpc="unsupported";return out;}
var call=null;
try{call=api.rpc("conversations.getKimiModelList");}catch(error){out.rpc="error";return out;}
var outcome=await new Promise(function(resolve){
  var settled=false;
  var timer=null;
  if(typeof setTimeout==="function"){timer=setTimeout(function(){if(settled)return;settled=true;resolve({timeout:true});},5000);}
  function done(result){
    if(settled)return;
    settled=true;
    if(timer!==null&&typeof clearTimeout==="function")clearTimeout(timer);
    resolve(result);
  }
  Promise.resolve(call).then(function(value){done({value:value});},function(){done({error:true});});
});
if(outcome.timeout){out.rpc="timeout";return out;}
if(outcome.error){out.rpc="error";return out;}
var payload=outcome.value;
if(payload&&typeof payload==="object"&&payload.ok===false){out.rpc="error";return out;}
if(payload&&typeof payload==="object"&&payload.ok===true&&payload.result!==undefined)payload=payload.result;
var models=payload&&typeof payload==="object"?payload.models:null;
if(!Array.isArray(models)){out.rpc="shape";return out;}
probe.verified=true;
out.rpc="ok";
return out;
}`

func accountRuntimeReadyExpression(requireTransition bool) string {
	if requireTransition {
		return "(" + accountRuntimeReadyBody + ")(true)"
	}
	return "(" + accountRuntimeReadyBody + ")(false)"
}

// accountRuntimeStopExpression removes the listener and the global. It answers
// true when nothing is left, including when nothing was installed.
const accountRuntimeStopExpression = `(function(){
try{
var probe=globalThis.` + accountRuntimeProbeGlobal + `;
if(probe&&typeof probe.dispose==="function"){try{probe.dispose();}catch(error){}}
globalThis.` + accountRuntimeProbeGlobal + `=null;
return true;
}catch(error){return false;}
})()`

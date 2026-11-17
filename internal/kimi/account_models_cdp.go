package kimi

import (
	"context"
	"errors"
)

func (a *cdpActions) accountModelConfig(ctx context.Context, page accountPage) (map[string]any, error) {
	if page.credentials || page.sessionID == "" {
		return nil, errors.New(AccountErrUnsupported)
	}
	value, err := a.evaluate(ctx, page.sessionID, accountModelConfigExpression)
	if err != nil {
		return nil, errors.New(AccountErrPageTimeout)
	}
	raw, _ := value.(map[string]any)
	if ok, _ := raw["ok"].(bool); !ok {
		return nil, errors.New(AccountErrUnavailable)
	}
	catalog, ok := raw["catalog"].(map[string]any)
	if !ok {
		return nil, errors.New(AccountErrUnsupported)
	}
	return catalog, nil
}

// config.get reads the daemon's config file, not an in-memory model registry.
// The caller separately verifies the account's runtime transition before using it.
const accountModelConfigExpression = `(async function(){
try{
var api=globalThis.kimiAgentAPI;
if(!api||typeof api.getStatus!=="function"||typeof api.rpc!=="function")return {ok:false};
var status=await api.getStatus();
if(!status||status.state!=="ready")return {ok:false};
var payload=await api.rpc("config.get",{key:"model"});
if(payload&&payload.ok===false)return {ok:false};
if(payload&&payload.ok===true&&payload.result!==undefined)payload=payload.result;
function record(value){return value&&typeof value==="object"&&!Array.isArray(value);}
if(!payload||payload.exists!==true||!record(payload.value))return {ok:false};
var source=payload.value;
if(!record(source.models)||!record(source.providers))return {ok:false};
function pick(value,keys){
  if(!record(value))throw new Error();
  var result={};
  for(var key of keys){
    if(!Object.prototype.hasOwnProperty.call(value,key))continue;
    var field=value[key];
    if(Array.isArray(field)){
      if(field.length>64||field.some(function(item){return typeof item!=="string"||item.length>1024;}))throw new Error();
      result[key]=field.slice();
    }else if(field===null||typeof field==="number"||typeof field==="boolean"||typeof field==="string"&&field.length<=4096){
      result[key]=field;
    }else throw new Error();
  }
  return result;
}
var aliases=Object.keys(source.models).filter(function(alias){return alias.startsWith("byok-kth-");});
if(aliases.length>256)return {ok:false};
var catalog={models:{},providers:{}};
for(var alias of aliases){
  if(alias.length>1024)return {ok:false};
  var model=pick(source.models[alias],["provider","model","maxContextSize","maxOutputSize","defaultEffort","capabilities","supportEfforts"]);
  if(typeof model.provider!=="string"||!model.provider.startsWith("byok-kth-"))return {ok:false};
  catalog.models[alias]=model;
  catalog.providers[model.provider]=pick(source.providers[model.provider],["type","baseUrl","credential"]);
}
status=await api.getStatus();
return status&&status.state==="ready"?{ok:true,catalog:catalog}:{ok:false};
}catch(error){return {ok:false};}
})()`

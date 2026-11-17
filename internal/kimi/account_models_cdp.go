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
	profile, _ := raw["profile"].(map[string]any)
	return accountProfileConfig(profile)
}

func accountProfileConfig(profile map[string]any) (map[string]any, error) {
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
			return nil, errors.New(AccountErrUnsupported)
		}
		models[alias] = modelEntry(row)
		providers[providerID] = map[string]any{"type": providerType(row), "baseUrl": profile["baseUrl"], "credential": profile["credentialRef"]}
	}
	return map[string]any{"models": models, "providers": providers}, nil
}

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

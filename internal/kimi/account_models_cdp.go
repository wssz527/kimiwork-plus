package kimi

import (
	"context"
	"errors"
)

func (a *cdpActions) accountModelAliases(ctx context.Context, page accountPage) ([]string, error) {
	if page.credentials || page.sessionID == "" {
		return nil, errors.New(AccountErrUnsupported)
	}
	value, err := a.evaluate(ctx, page.sessionID, accountModelAliasesExpression)
	if err != nil {
		return nil, errors.New(AccountErrPageTimeout)
	}
	raw, _ := value.(map[string]any)
	if ok, _ := raw["ok"].(bool); !ok {
		return nil, errors.New(AccountErrUnavailable)
	}
	rows, ok := raw["aliases"].([]any)
	if !ok {
		return nil, errors.New(AccountErrUnsupported)
	}
	aliases := make([]string, 0, len(rows))
	for _, row := range rows {
		alias, ok := row.(string)
		if !ok || alias == "" || len(alias) > 1024 {
			return nil, errors.New(AccountErrUnsupported)
		}
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

const accountModelAliasesExpression = `(async function(){
try{
var api=globalThis.kimiAgentAPI;
if(!api||typeof api.getStatus!=="function"||typeof api.rpc!=="function")return {ok:false};
var status=await api.getStatus();
if(!status||status.state!=="ready")return {ok:false};
var payload=await api.rpc("conversations.getKimiModelList");
if(payload&&payload.ok===false)return {ok:false};
if(payload&&payload.ok===true&&payload.result!==undefined)payload=payload.result;
if(!payload||!Array.isArray(payload.models))return {ok:false};
var aliases=[];
for(var row of payload.models){
  if(!row||typeof row.model!=="string"||!row.model||row.model.length>1024)return {ok:false};
  aliases.push(row.model);
}
status=await api.getStatus();
return status&&status.state==="ready"?{ok:true,aliases:aliases}:{ok:false};
}catch(error){return {ok:false};}
})()`

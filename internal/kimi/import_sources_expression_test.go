package kimi

import (
	"testing"

	"github.com/dop251/goja"
)

func TestSourceImportUsesPreviewRevisionAndAppliesOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		calls string
		ok    bool
	}{
		{"same account and revisions", "", "list,preview,save,list,apply", true},
		{"account changes before save", `preview.scopeToken="another"`, "list,preview,discard", false},
		{"account changes before apply", `updated.scopeToken="another"`, "list,preview,save,list", false},
		{"foreign registry", `initial.sources[0].registryUrl="http://127.0.0.1:8100/v1/models/api.json"`, "list", false},
		{"pending changes from another source", `initial.applyState="prepared";initial.sources.push({id:"other",kind:"registry"})`, "list,preview,save", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(`
const calls=[];
const initial={scopeToken:"account", accountRevision:"before", sources:[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"}]};
const updated={...initial,accountRevision:"after"};
const preview={scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",previewId:"preview",canSave:true,candidates:[{modelId:"model"}]};
let saved=false;
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(saved?updated:initial);},
  preview(request){
    calls.push("preview");
    if(request.accountRevision!=="before"||request.sourceRevision!=="source"||request.sourceId!=="byok-kth"||request.draft.registryUrl!==initial.sources[0].registryUrl||request.draft.registryKey.value!=="fixture")throw new Error("invalid preview request");
    return ok(preview);
  },
  discard(id){if(id!=="preview")throw new Error("wrong preview");calls.push("discard");},
  save(request){
    calls.push("save");
    if(request.previewId!=="preview"||request.accountRevision!=="before"||request.sourceRevision!=="source")throw new Error("wrong revisions");
    saved=true;return ok({sourceId:"byok-kth",accountRevision:"after",status:"pending"});
  },
  applyPending(revision){
    calls.push("apply");
    if(revision!=="after")throw new Error("wrong applied revision");
    return ok({status:"applied"});
  },
}};
` + tc.setup); err != nil {
				t.Fatal(err)
			}
			if value, err := vm.RunString(importProbeExpression); err != nil || !value.ToBoolean() {
				t.Fatalf("new source API was not detected: %v, %v", value, err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1/") + `).then(result=>globalThis.importResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("importResult")
			if result == nil || result.ToObject(vm).Get("ok").ToBoolean() != tc.ok {
				t.Fatalf("unexpected import result: %v", result)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.calls {
				t.Fatalf("got calls %v, want %s, err=%v", calls, tc.calls, err)
			}
		})
	}
}

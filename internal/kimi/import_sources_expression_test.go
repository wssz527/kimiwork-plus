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
		{"own previous loopback registry", `initial.sources[0].registryUrl="http://127.0.0.1:8100/v1/models/api.json"`, "list,preview,save,list,apply", true},
		{"foreign registry", `initial.sources[0].registryUrl="https://other.example/v1/models/api.json"`, "list", false},
		{"pending changes from another source without model evidence", `initial.applyState="prepared";initial.sources.push({id:"other",kind:"registry"})`, "list,preview,save", true},
		{"idle state with another settled source", `initial.applyState="idle";initial.sources.push({id:"other",kind:"registry",models:[{status:"available"}]})`, "list,preview,save,list,apply", true},
		{"prepared state with another settled source", `initial.applyState="prepared";initial.sources.push({id:"other",kind:"registry",models:[{status:"available"}]})`, "list,preview,save,list,apply", true},
		{"pending model from another source", `initial.applyState="prepared";initial.sources.push({id:"other",kind:"registry",models:[{status:"pending"}]})`, "list,preview,save", true},
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
    if(request.accountRevision!=="before"||request.sourceRevision!=="source"||request.sourceId!=="byok-kth"||request.draft.registryUrl!=="http://127.0.0.1:9100/v1/models/api.json"||request.draft.registryKey.value!=="fixture")throw new Error("invalid preview request");
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
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1/", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
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

func TestSourceImportUpgradesDefaultDisplayNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
		want     string
	}{
		{"empty displayName becomes KimiWork +", "", "KimiWork +"},
		{"legacy KTH displayName upgrades to KimiWork +", "KTH", "KimiWork +"},
		{"v1.0.0 default displayName upgrades to KimiWork +", "Kimi Work+", "KimiWork +"},
		{"custom displayName is preserved", "My Gateway", "My Gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(`
let seenName = "";
const initial={scopeToken:"account", accountRevision:"r1", sources:[{id:"byok-kth",kind:"kth",displayName:` + `"` + tc.existing + `"` + `,revision:"s1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"}]};
const preview={scopeToken:"account",accountRevision:"r1",sourceRevision:"s1",sourceId:"byok-kth",previewId:"p1",canSave:true,candidates:[{modelId:"m1"}]};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){return ok(initial);},
  preview(req){seenName=req.draft.displayName;return ok(preview);},
  discard(){},
  save(){return ok({sourceId:"byok-kth",accountRevision:"r2",status:"pending"});},
  applyPending(){return ok({status:"applied"});},
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1/", "") + `).then(r=>globalThis.res=r);`); err != nil {
				t.Fatal(err)
			}
			got := vm.Get("seenName").String()
			if got != tc.want {
				t.Fatalf("displayName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSourceImportResolvesRouteSelectionRequired(t *testing.T) {
	t.Run("retries preview with deduplicated selectedCandidates", func(t *testing.T) {
		vm := goja.New()
		if _, err := vm.RunString(`
const calls=[];
let selectedSeen=null;
const initial={scopeToken:"account",accountRevision:"before",sources:[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:49958/v1/models/api.json"}]};
const updated={...initial,accountRevision:"after"};
let saved=false;
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(saved?updated:initial);},
  preview(request){
    calls.push("preview");
    if(!request.draft.selectedCandidates){
      return ok({
        scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",
        previewId:"preview-1",canSave:true,
        source:{notices:[{groupId:"kimi",modelId:"m1",protocol:"kimi",reason:"route-selection-required"}]},
        candidates:[
          {id:"cand-1",modelId:"m1"},
          {id:"cand-2",modelId:"m2"},
          {id:"cand-2-alt",modelId:"m2"},
        ],
      });
    }
    selectedSeen=request.draft.selectedCandidates.join(",");
    return ok({
      scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",
      previewId:"preview-2",canSave:true,
      source:{notices:[]},
      candidates:[{id:"cand-1",modelId:"m1"},{id:"cand-2",modelId:"m2"},{id:"cand-2-alt",modelId:"m2"}],
    });
  },
  discard(id){calls.push("discard:"+id);},
  save(request){
    calls.push("save:"+request.previewId);
    saved=true;
    return ok({sourceId:"byok-kth",accountRevision:"after",status:"pending"});
  },
  applyPending(revision){
    calls.push("apply:"+revision);
    return ok({status:"applied"});
  },
}};
`); err != nil {
			t.Fatal(err)
		}
		if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:63501/v1", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
			t.Fatal(err)
		}
		result := vm.Get("importResult").ToObject(vm)
		if !result.Get("ok").ToBoolean() || result.Get("models").ToInteger() != 2 {
			t.Fatalf("unexpected import result: ok=%v models=%v code=%v", result.Get("ok"), result.Get("models"), result.Get("code"))
		}
		if got := vm.Get("selectedSeen").String(); got != "cand-1,cand-2" {
			t.Fatalf("selectedCandidates = %q, want cand-1,cand-2", got)
		}
		calls, err := vm.RunString(`calls.join(",")`)
		if err != nil || calls.String() != "list,preview,discard:preview-1,preview,save:preview-2,list,apply:after" {
			t.Fatalf("calls = %v, err=%v", calls, err)
		}
	})

	t.Run("fails closed without saving when route selection cannot be resolved", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			candidate string
			wantCalls string
		}{
			{"missing candidate ids", `[{modelId:"m1"}]`, "list,preview,discard:preview-1"},
			{"still blocked after selection", `[{id:"cand-1",modelId:"m1"}]`, "list,preview,discard:preview-1,preview,discard:preview-2"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				vm := goja.New()
				if _, err := vm.RunString(`
const calls=[];
let count=0;
const initial={scopeToken:"account",accountRevision:"before",sources:[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:49958/v1/models/api.json"}]};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(initial);},
  preview(){
    calls.push("preview");
    count++;
    return ok({
      scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",
      previewId:"preview-"+count,canSave:true,
      source:{notices:[{groupId:"kimi",modelId:"m1",protocol:"kimi",reason:"route-selection-required"}]},
      candidates:` + tc.candidate + `,
    });
  },
  discard(id){calls.push("discard:"+id);},
  save(){calls.push("save");return ok({sourceId:"byok-kth",accountRevision:"after",status:"applied"});},
}};
`); err != nil {
					t.Fatal(err)
				}
				if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:63501/v1", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
					t.Fatal(err)
				}
				result := vm.Get("importResult").ToObject(vm)
				if result.Get("ok").ToBoolean() || result.Get("code").String() != "route-selection-required" {
					t.Fatalf("unexpected result: ok=%v code=%v", result.Get("ok"), result.Get("code"))
				}
				calls, err := vm.RunString(`calls.join(",")`)
				if err != nil || calls.String() != tc.wantCalls {
					t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
				}
			})
		}
	})
}

func TestGatewayImportDisconnectsDirectOnlyAfterReplacement(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failPreview    bool
		failDisconnect bool
		deferred       bool
		wantCalls      string
		wantOK         bool
		wantPending    bool
	}{
		{name: "applied before cleanup", wantCalls: "list,preview,save,list,apply,list,disconnect", wantOK: true},
		{name: "preview failure preserves direct", failPreview: true, wantCalls: "list,preview"},
		{name: "cleanup failure is retryable", failDisconnect: true,
			wantCalls: "list,preview,save,list,apply,list,disconnect", wantOK: true, wantPending: true},
		{name: "deferred replacement preserves direct until restart", deferred: true,
			wantCalls: "list,preview,save,list,apply", wantOK: true, wantPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			applyStatus := `"applied"`
			if tc.deferred {
				applyStatus = `"deferred"`
			}
			if _, err := vm.RunString(`
const calls=[];
let directConnected=true;
let imported=false;
const direct={id:"direct-source",kind:"custom",displayName:"KimiWork + 直连",revision:"d1"};
const gateway={id:"byok-kth",kind:"kth",revision:"k1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok({scopeToken:"scope",accountRevision:imported?"r2":"r1",sources:(directConnected?[direct]:[]).concat([gateway])});},
  preview(){calls.push("preview");if(` + boolString(tc.failPreview) + `)return {ok:false,reasonCode:"preview-refused"};
    return ok({scopeToken:"scope",accountRevision:"r1",sourceRevision:"k1",sourceId:"byok-kth",previewId:"p1",canSave:true,candidates:[{id:"m1"}]});},
  save(){calls.push("save");imported=true;return ok({sourceId:"byok-kth",accountRevision:"r2",status:"pending"});},
  applyPending(){calls.push("apply");return ok({status:` + applyStatus + `});},
  disconnect(request){calls.push("disconnect");if(` + boolString(tc.failDisconnect) + `)return {ok:false,reasonCode:"disconnect-refused"};
    if(request.sourceId!=="direct-source")throw new Error("wrong source");directConnected=false;return ok({});},
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1", "direct-source") + `).then(result=>globalThis.result=result,error=>globalThis.failure=String(error&&error.stack||error));`); err != nil {
				t.Fatal(err)
			}
			if vm.Get("result") == nil {
				t.Fatalf("page expression rejected: %v", vm.Get("failure"))
			}
			result := vm.Get("result").ToObject(vm)
			pending := result.Get("routeCleanupPending") != nil && result.Get("routeCleanupPending").ToBoolean()
			if result.Get("ok").ToBoolean() != tc.wantOK || pending != tc.wantPending {
				t.Fatalf("result = %v, failure = %v", result, vm.Get("failure"))
			}
			calls, _ := vm.RunString(`calls.join(",")`)
			if calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s", calls, tc.wantCalls)
			}
			connected, _ := vm.RunString(`directConnected`)
			if tc.failPreview && !connected.ToBoolean() {
				t.Fatal("a failed gateway preview disconnected the working direct source")
			}
		})
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

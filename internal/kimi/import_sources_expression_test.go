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

// A preview that would mint a random provider id is the one way an import can
// pass every revision check and still lose in-place switching. The page drops
// such a preview instead of saving it.
func TestSourceImportNeverSavesANonCanonicalProviderPreview(t *testing.T) {
	for _, tc := range []struct {
		name       string
		providers  string
		disconnect string
		wantOK     bool
		wantCode   string
		wantCalls  string
	}{
		{
			name:       "a random provider id is discarded before save",
			providers:  `source:{providers:[{id:"byok-0b6e0d5f",protocol:"kimi",baseUrl:"http://127.0.0.1:9100/v1"}]},`,
			disconnect: `disconnect(request){calls.push("disconnect");return ok({});},`,
			wantCode:   "non-canonical-provider",
			wantCalls:  "list,preview,discard:preview",
		},
		{
			name:       "the canonical provider id is saved",
			providers:  `source:{providers:[{id:"byok-kth-kimi",protocol:"kimi",baseUrl:"http://127.0.0.1:9100/v1"}]},`,
			disconnect: `disconnect(request){calls.push("disconnect");return ok({});},`,
			wantOK:     true,
			wantCalls:  "list,preview,save,list,apply:after",
		},
		{
			name:       "a preview without source providers keeps the released behaviour",
			providers:  `source:{notices:[]},`,
			disconnect: `disconnect(request){calls.push("disconnect");return ok({});},`,
			wantOK:     true,
			wantCalls:  "list,preview,save,list,apply:after",
		},
		{
			name:       "a preview with only other protocols is saved",
			providers:  `source:{providers:[{id:"byok-kth-resp",protocol:"openai_responses"}]},`,
			disconnect: `disconnect(request){calls.push("disconnect");return ok({});},`,
			wantOK:     true,
			wantCalls:  "list,preview,save,list,apply:after",
		},
		{
			name:       "a build without the source disconnect keeps the released behaviour",
			providers:  `source:{providers:[{id:"byok-0b6e0d5f",protocol:"kimi"}]},`,
			disconnect: ``,
			wantOK:     true,
			wantCalls:  "list,preview,save,list,apply:after",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(`
const calls=[];
let saved=false;
const initial={scopeToken:"account",accountRevision:"before",sources:[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"}]};
const updated={...initial,accountRevision:"after"};
const preview={scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",
  previewId:"preview",canSave:true,candidates:[{id:"m1",modelId:"m1"}],` + tc.providers + `};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(saved?updated:initial);},
  preview(){calls.push("preview");return ok(preview);},
  discard(id){calls.push("discard:"+id);},
  save(){calls.push("save");saved=true;return ok({sourceId:"byok-kth",accountRevision:"after",status:"pending"});},
  applyPending(revision){calls.push("apply:"+revision);return ok({status:"applied"});},
  ` + tc.disconnect + `
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("importResult").ToObject(vm)
			if result.Get("ok").ToBoolean() != tc.wantOK {
				t.Fatalf("result: %v", result.Export())
			}
			code := ""
			if value := result.Get("code"); value != nil {
				code = value.String()
			}
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

// `deferred` is what Electron answers inside its post-ready window. The page
// hands the caller the revision to apply later, but only for a save this import
// owns: another source's pending work must keep its own confirmation.
func TestSourceImportEmitsApplyCredentialsOnlyForAFreshDeferredSave(t *testing.T) {
	for _, tc := range []struct {
		name         string
		saveStatus   string
		otherPending bool
		wantScope    string
		wantRevision string
		wantCalls    string
	}{
		{name: "the apply itself comes back deferred", saveStatus: "pending",
			wantScope: "account", wantRevision: "after", wantCalls: "list,preview,save,list,apply:after"},
		{name: "a fresh deferred save", saveStatus: "deferred",
			wantScope: "account", wantRevision: "after", wantCalls: "list,preview,save"},
		{name: "another source is pending", saveStatus: "deferred", otherPending: true,
			wantCalls: "list,preview,save"},
		{name: "an applied save asks for nothing", saveStatus: "applied",
			wantCalls: "list,preview,save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			sources := `[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"}]`
			if tc.otherPending {
				sources = `[{id:"byok-kth",kind:"kth",revision:"source",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"},{id:"other",kind:"registry",models:[{status:"pending"}]}]`
			}
			if _, err := vm.RunString(`
const calls=[];
let saved=false;
const initial={scopeToken:"account",accountRevision:"before",sources:` + sources + `};
const updated={...initial,accountRevision:"after"};
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok(saved?updated:initial);},
  preview(){calls.push("preview");return ok({scopeToken:"account",accountRevision:"before",sourceRevision:"source",sourceId:"byok-kth",previewId:"preview",canSave:true,candidates:[{id:"m1"}]});},
  discard(){calls.push("discard");},
  save(){calls.push("save");saved=true;return ok({sourceId:"byok-kth",accountRevision:"after",status:"` + tc.saveStatus + `"});},
  applyPending(revision){calls.push("apply:"+revision);return ok({status:"deferred"});},
  disconnect(){return ok({});},
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1", "") + `).then(result=>globalThis.importResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("importResult").ToObject(vm)
			scope, revision := "", ""
			if value := result.Get("applyScopeToken"); value != nil {
				scope = value.String()
			}
			if value := result.Get("applyRevision"); value != nil {
				revision = value.String()
			}
			if scope != tc.wantScope || revision != tc.wantRevision {
				t.Fatalf("apply scope=%q revision=%q, want %q/%q", scope, revision, tc.wantScope, tc.wantRevision)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

// The Go retry loop trusts this expression's own guards: it must never apply a
// revision that belongs to another account or to newer bytes than the save saw.
func TestApplyPendingExpressionGuardsTheAccountAndRevision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      string
		wantOK     bool
		wantStatus string
		wantCode   string
		wantCalls  string
	}{
		{name: "matching revision applies", wantOK: true, wantStatus: "applied", wantCalls: "list,apply:revision"},
		{name: "another account aborts", setup: `globalThis.clawBridgeAPI.byokSources.list=()=>{calls.push("list");return ok({scopeToken:"another",accountRevision:"revision"});};`,
			wantCode: "account-changed", wantCalls: "list"},
		{name: "a changed revision is stale", setup: `globalThis.clawBridgeAPI.byokSources.list=()=>{calls.push("list");return ok({scopeToken:"account",accountRevision:"newer"});};`,
			wantCode: "stale", wantCalls: "list"},
		{name: "a build without applyPending is unsupported",
			setup:    `delete globalThis.clawBridgeAPI.byokSources.applyPending;`,
			wantCode: "unsupported", wantCalls: ""},
		{name: "an apply failure is reported",
			setup:    `globalThis.clawBridgeAPI.byokSources.applyPending=()=>{calls.push("apply:revision");return {ok:false,reasonCode:"byok-busy"};};`,
			wantCode: "byok-busy", wantCalls: "list,apply:revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(`
const calls=[];
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok({scopeToken:"account",accountRevision:"revision",sources:[]});},
  applyPending(revision){calls.push("apply:"+revision);return ok({status:"applied"});},
}};
` + tc.setup); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + applyPendingExpression("account", "revision") + `).then(result=>globalThis.applyResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("applyResult").ToObject(vm)
			if result.Get("ok").ToBoolean() != tc.wantOK {
				t.Fatalf("result: %v", result.Export())
			}
			status, code := "", ""
			if value := result.Get("status"); value != nil {
				status = value.String()
			}
			if value := result.Get("code"); value != nil {
				code = value.String()
			}
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("status=%q code=%q, want %q/%q", status, code, tc.wantStatus, tc.wantCode)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

// A gateway import that switches away from Direct Mode disconnects the old
// source after the replacement is applied. Electron answers `pending` while a
// restarted Daimon prepares that write and `deferred` inside its post-ready
// window; neither may leave the old source connected on disk.
func TestGatewayImportSettlesAPendingOrDeferredDirectDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name         string
		otherPending bool
		saveStatus   string
		disconnect   string
		wantCalls    string
		wantPending  bool
		wantScope    string
		wantRevision string
	}{
		{
			name: "a pending disconnect is applied in place", saveStatus: "pending",
			disconnect: `{status:"pending",accountRevision:"r3"}`,
			wantCalls:  "list,preview,save,list,apply:r2,list,disconnect,list,apply:r3",
		},
		{
			name: "a deferred disconnect hands its revision to the retry", saveStatus: "pending",
			disconnect:  `{status:"deferred",accountRevision:"r3"}`,
			wantCalls:   "list,preview,save,list,apply:r2,list,disconnect",
			wantPending: true, wantScope: "account", wantRevision: "r3",
		},
		{
			name: "an applied disconnect stays one call", saveStatus: "applied",
			disconnect: `{status:"applied",accountRevision:"r3"}`,
			wantCalls:  "list,preview,save,list,disconnect",
		},
		{
			name:         "another source's pending confirmation is never applied",
			otherPending: true, saveStatus: "applied",
			disconnect:  `{status:"pending",accountRevision:"r3"}`,
			wantCalls:   "list,preview,save,list,disconnect",
			wantPending: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			sources := `[{id:"direct-source",kind:"custom",displayName:"KimiWork + 直连",revision:"d1"},{id:"byok-kth",kind:"kth",revision:"k1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"}]`
			if tc.otherPending {
				sources = `[{id:"direct-source",kind:"custom",displayName:"KimiWork + 直连",revision:"d1"},{id:"byok-kth",kind:"kth",revision:"k1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"},{id:"other",kind:"registry",models:[{status:"pending"}]}]`
			}
			if _, err := vm.RunString(`
const calls=[];
let revision="r1";
const sources=(function(){return ` + sources + `;})();
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok({scopeToken:"account",accountRevision:revision,sources:sources});},
  preview(){calls.push("preview");return ok({scopeToken:"account",accountRevision:"r1",sourceRevision:"k1",sourceId:"byok-kth",previewId:"preview",canSave:true,candidates:[{id:"m1"}]});},
  save(){calls.push("save");revision="r2";return ok({sourceId:"byok-kth",accountRevision:"r2",status:"` + tc.saveStatus + `"});},
  applyPending(applied){calls.push("apply:"+applied);return ok({status:"applied"});},
  disconnect(request){calls.push("disconnect");revision="r3";return ok(` + tc.disconnect + `);},
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + importExpression("fixture", "http://127.0.0.1:9100/v1", "direct-source") + `).then(result=>globalThis.importResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("importResult").ToObject(vm)
			pending := result.Get("routeCleanupPending") != nil && result.Get("routeCleanupPending").ToBoolean()
			if result.Get("ok").ToBoolean() != true || pending != tc.wantPending {
				t.Fatalf("result: %v", result.Export())
			}
			scope, appliedRevision := "", ""
			if value := result.Get("cleanupApplyScopeToken"); value != nil {
				scope = value.String()
			}
			if value := result.Get("cleanupApplyRevision"); value != nil {
				appliedRevision = value.String()
			}
			if scope != tc.wantScope || appliedRevision != tc.wantRevision {
				t.Fatalf("cleanup scope=%q revision=%q, want %q/%q", scope, appliedRevision, tc.wantScope, tc.wantRevision)
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

// The standalone cleanup expressions are the retry path a deferred apply uses.
// They must parse and settle their own disconnect replies exactly like the
// imports do: pending is applied in place, deferred carries the revision Go
// retries, and a reply without a status stays one call.
func TestStandaloneDisconnectExpressionsSettleTheirOwnReplies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		expression   func() string
		reply        string
		wantCalls    string
		wantSettled  bool
		wantScope    string
		wantRevision string
	}{
		{
			name:       "a pending direct disconnect is applied in place",
			expression: func() string { return disconnectDirectSourceExpression("direct-source") },
			reply:      `{status:"pending",accountRevision:"r3"}`,
			wantCalls:  "list,disconnect,list,apply:r3", wantSettled: true,
		},
		{
			name:       "a deferred direct disconnect carries its revision",
			expression: func() string { return disconnectDirectSourceExpression("direct-source") },
			reply:      `{status:"deferred",accountRevision:"r3"}`,
			wantCalls:  "list,disconnect",
			wantScope:  "scope", wantRevision: "r3",
		},
		{
			name:       "an applied gateway disconnect is one call",
			expression: func() string { return disconnectGatewayExpression("http://127.0.0.1:9100/v1") },
			reply:      `{status:"applied",accountRevision:"r3"}`,
			wantCalls:  "list,disconnect", wantSettled: true,
		},
		{
			name:       "a pending gateway disconnect is applied in place",
			expression: func() string { return disconnectGatewayExpression("http://127.0.0.1:9100/v1") },
			reply:      `{status:"pending",accountRevision:"r3"}`,
			wantCalls:  "list,disconnect,list,apply:r3", wantSettled: true,
		},
		{
			name:       "a deferred gateway disconnect carries its revision",
			expression: func() string { return disconnectGatewayExpression("http://127.0.0.1:9100/v1") },
			reply:      `{status:"deferred",accountRevision:"r3"}`,
			wantCalls:  "list,disconnect",
			wantScope:  "scope", wantRevision: "r3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(`
const calls=[];
let revision="r2";
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");return ok({scopeToken:"scope",accountRevision:revision,sources:[
    {id:"direct-source",kind:"custom",displayName:"KimiWork + 直连",revision:"d1"},
    {id:"byok-kth",kind:"kth",revision:"k1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json",providers:[]}]});},
  applyPending(applied){calls.push("apply:"+applied);return ok({status:"applied"});},
  disconnect(){calls.push("disconnect");revision="r3";return ok(` + tc.reply + `);},
}};
`); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + tc.expression() + `).then(result=>globalThis.cleanupResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("cleanupResult").ToObject(vm)
			settled := result.Get("settled") != nil && result.Get("settled").ToBoolean()
			scope, revision := "", ""
			if value := result.Get("applyScopeToken"); value != nil {
				scope = value.String()
			}
			if value := result.Get("applyRevision"); value != nil {
				revision = value.String()
			}
			if result.Get("ok").ToBoolean() != true || settled != tc.wantSettled ||
				scope != tc.wantScope || revision != tc.wantRevision {
				t.Fatalf("result: %v", result.Export())
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || calls.String() != tc.wantCalls {
				t.Fatalf("calls = %v, want %s, err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

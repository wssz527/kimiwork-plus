package kimi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// The fixtures below use the released 3.2.28 field names: a v3 `accounts` map,
// per-source `routeBindings`, providers split across `active` and `pending`, and
// the account's `blockedAliases`. A later rename in the official store fails
// here rather than on a user's machine.

const kthTestCredential = "cred-kth-1"

func kthTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	config, err := json.Marshal(map[string]any{"model": map[string]any{},
		"credentials": map[string]any{"kimiWeb": map[string]any{"userId": "user-kth"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, NativeWorkConfig), config, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// kthTestStore wraps one account in a store with the resources the import owns.
func kthTestStore(accountKey string, account map[string]any) map[string]any {
	return map[string]any{
		"v":        3,
		"accounts": map[string]any{accountKey: account},
		"ownedResources": map[string]any{
			"providerIds":    []any{kthProviderID},
			"modelAliases":   []any{"byok-kth-kimi/kimi-a"},
			"credentialRefs": []any{kthTestCredential},
		},
		"secrets": map[string]any{kthTestCredential: map[string]any{"encryption": "dpapi", "data": "sealed"}},
	}
}

// kthTestAccount is one account whose gateway source already carries the
// canonical provider on `baseURL`, with `routeBindings` left as given.
func kthTestAccount(baseURL string, bindings map[string]any) map[string]any {
	if bindings == nil {
		bindings = map[string]any{}
	}
	return map[string]any{
		"revision": "account-rev-1",
		"sources": []any{map[string]any{
			"id": "byok-kth", "kind": "kth", "displayName": "KimiWork +",
			"revision": "source-rev-1", "discoveryMode": "manual",
			"registryUrl": baseURL + "/models/api.json",
			"desired": map[string]any{"providers": []any{map[string]any{
				"id": kthProviderID, "protocol": "kimi", "baseUrl": baseURL,
			}}, "models": []any{}},
			"routeBindings": bindings,
		}},
		"active": map[string]any{
			"rev": "active-rev-1",
			"providers": []any{map[string]any{"id": kthProviderID, "sourceId": "byok-kth",
				"protocol": "kimi", "baseUrl": baseURL, "credentialRef": kthTestCredential}},
			"models": []any{map[string]any{"alias": "byok-kth-kimi/kimi-a", "providerId": kthProviderID,
				"model": "kimi-a", "family": "kimi", "sourceId": "byok-kth"}},
		},
		"pending":        map[string]any{"rev": "pending-rev-1", "providers": []any{}, "models": []any{}},
		"blockedAliases": map[string]any{},
	}
}

// kthTestStuckAccount is the same account with its kimi provider on a random
// id, which is what an earlier upgrade or a Direct Mode round trip leaves.
func kthTestStuckAccount(baseURL, providerID string) map[string]any {
	account := kthTestAccount(baseURL, nil)
	source := account["sources"].([]any)[0].(map[string]any)
	source["desired"] = map[string]any{"providers": []any{map[string]any{
		"id": providerID, "protocol": "kimi", "baseUrl": baseURL,
	}}, "models": []any{}}
	active := account["active"].(map[string]any)
	active["providers"] = []any{map[string]any{"id": providerID, "sourceId": "byok-kth",
		"protocol": "kimi", "baseUrl": baseURL, "credentialRef": kthTestCredential}}
	active["models"] = []any{map[string]any{"alias": providerID + "/kimi-a", "providerId": providerID,
		"model": "kimi-a", "family": "kimi", "sourceId": "byok-kth"}}
	return account
}

func kthTestWrite(t *testing.T, home string, store map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, NativeWorkStore), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return encoded
}

func kthTestRead(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	var store map[string]any
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	return store
}

func kthTestAccountOf(t *testing.T, home string) map[string]any {
	t.Helper()
	account, ok := kthTestRead(t, home)["accounts"].(map[string]any)[AccountScope(home)].(map[string]any)
	if !ok {
		t.Fatal("the account is missing from the store")
	}
	return account
}

func kthTestSourceOf(t *testing.T, account map[string]any) map[string]any {
	t.Helper()
	source, ok := kthAccountSource(account)
	if !ok {
		t.Fatal("the byok-kth source is missing from the account")
	}
	return source
}

// ---------------------------------------------------------------------------
// the scan

func TestKthRouteKeyMatchesTheOfficialStoreString(t *testing.T) {
	key := kimiRouteKey("http://127.0.0.1:63501/v1/")
	if key != `["kimi","kimi",null,"http://127.0.0.1:63501/v1"]` {
		t.Fatalf("route key = %s", key)
	}
}

func TestKthScanRecognizesAPortDriftOnACanonicalProvider(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	oldURL := "http://127.0.0.1:9100/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestAccount(oldURL,
		map[string]any{"gpt": "byok-kth-resp", kimiRouteKey(oldURL): kthProviderID})))

	scan := kthStoreState(home, account, "http://127.0.0.1:9200/v1")
	if !scan.valid || !scan.owned || !scan.canonical {
		t.Fatalf("scan: %+v", scan)
	}
	if scan.needsDisconnect() || scan.needsBlockedCleanup() {
		t.Fatalf("a canonical source must only need a rebind: %+v", scan)
	}
	if !scan.needsRebind() {
		t.Fatalf("the new port must ask for a rebind: %+v", scan)
	}
	// The old port's own key stays bound; the scan answers about this gateway.
	if scan.bound {
		t.Fatal("the new route key was reported as bound")
	}
	if bound := kthStoreState(home, account, oldURL); !bound.bound || bound.needsRebind() {
		t.Fatalf("the old port must stay bound: %+v", bound)
	}
}

func TestKthRepairRebindsTheGatewayRouteAndKeepsEverythingElse(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	oldURL, newURL := "http://127.0.0.1:9100/v1", "http://127.0.0.1:9200/v1"
	kthTestWrite(t, home, kthTestStore(account, kthTestAccount(oldURL,
		map[string]any{"gpt": "byok-kth-resp", kimiRouteKey(oldURL): kthProviderID})))

	changed, err := rebindKthRouteBinding(home, account, newURL)
	if err != nil || !changed {
		t.Fatalf("rebind changed=%v err=%v", changed, err)
	}
	source := kthTestSourceOf(t, kthTestAccountOf(t, home))
	bindings := source["routeBindings"].(map[string]any)
	if bindings[kimiRouteKey(newURL)] != kthProviderID {
		t.Fatalf("new route = %v", bindings)
	}
	// The older binding and every non-kimi route survive.
	for _, key := range []string{kimiRouteKey(oldURL), "gpt"} {
		if bindings[key] == nil {
			t.Fatalf("binding %s was dropped: %v", key, bindings)
		}
	}
	store := kthTestRead(t, home)
	for _, name := range []string{"ownedResources", "secrets"} {
		if _, ok := store[name].(map[string]any); !ok {
			t.Fatalf("%s was dropped: %v", name, store)
		}
	}
	info, err := os.Stat(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("store mode = %o", mode)
	}
	if scan := kthStoreState(home, account, newURL); scan.needsRebind() {
		t.Fatalf("the repair did not settle: %+v", scan)
	}
	// A second pass writes nothing.
	changed, err = rebindKthRouteBinding(home, account, newURL)
	if err != nil || changed {
		t.Fatalf("rebind was not idempotent: changed=%v err=%v", changed, err)
	}
}

func TestKthRebindInitializesMissingRouteBindings(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	store := kthTestStore(account, kthTestAccount(baseURL, nil))
	source := store["accounts"].(map[string]any)[account].(map[string]any)["sources"].([]any)[0].(map[string]any)
	delete(source, "routeBindings")
	kthTestWrite(t, home, store)

	changed, err := rebindKthRouteBinding(home, account, baseURL)
	if err != nil || !changed {
		t.Fatalf("rebind changed=%v err=%v", changed, err)
	}
	bindings, ok := kthTestSourceOf(t, kthTestAccountOf(t, home))["routeBindings"].(map[string]any)
	if !ok || bindings[kimiRouteKey(baseURL)] != kthProviderID {
		t.Fatalf("routeBindings = %v", bindings)
	}
}

func TestKthRebindRefusesAnUnknownRouteBindingsShape(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	store := kthTestStore(account, kthTestAccount(baseURL, nil))
	source := store["accounts"].(map[string]any)[account].(map[string]any)["sources"].([]any)[0].(map[string]any)
	source["routeBindings"] = []any{"kimi"}
	before := kthTestWrite(t, home, store)

	changed, err := rebindKthRouteBinding(home, account, baseURL)
	if err != nil || changed {
		t.Fatalf("an unknown shape was rewritten: changed=%v err=%v", changed, err)
	}
	after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the store was rewritten around an unknown routeBindings shape")
	}
}

func TestKthRepairNeverBindsAForeignSource(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"remote registry": func(account map[string]any) {
			source := account["sources"].([]any)[0].(map[string]any)
			source["registryUrl"] = "https://api.example.com/v1/models/api.json"
		},
		"remote provider": func(account map[string]any) {
			active := account["active"].(map[string]any)
			active["providers"] = []any{map[string]any{"id": kthProviderID, "sourceId": "byok-kth",
				"protocol": "kimi", "baseUrl": "https://api.example.com/v1"}}
		},
		"another kind": func(account map[string]any) {
			account["sources"].([]any)[0].(map[string]any)["kind"] = "custom"
		},
		"quarantined": func(account map[string]any) {
			account["sources"].([]any)[0].(map[string]any)["quarantined"] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := kthTestHome(t)
			account := AccountScope(home)
			baseURL := "http://127.0.0.1:9100/v1"
			document := kthTestAccount(baseURL, nil)
			mutate(document)
			before := kthTestWrite(t, home, kthTestStore(account, document))

			scan := kthStoreState(home, account, "http://127.0.0.1:9200/v1")
			if scan.owned || scan.needsRebind() || scan.needsDisconnect() {
				t.Fatalf("a foreign source was claimed: %+v", scan)
			}
			changed, err := rebindKthRouteBinding(home, account, "http://127.0.0.1:9200/v1")
			if err != nil || changed {
				t.Fatalf("a foreign source was written: changed=%v err=%v", changed, err)
			}
			after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("the store was rewritten for a foreign source")
			}
		})
	}
}

func TestKthScanFindsAProviderStuckOnARandomID(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	stuckID := "byok-9f2c1d-kimi"
	kthTestWrite(t, home, kthTestStore(account, kthTestStuckAccount("http://127.0.0.1:9100/v1", stuckID)))

	scan := kthStoreState(home, account, "http://127.0.0.1:9100/v1")
	if !scan.valid || !scan.owned || scan.canonical {
		t.Fatalf("scan: %+v", scan)
	}
	if !scan.needsDisconnect() || scan.stuck != stuckID {
		t.Fatalf("a random provider id was not detected: %+v", scan)
	}
	if scan.needsRebind() || scan.needsBlockedCleanup() {
		t.Fatalf("a stuck source asks for one repair: %+v", scan)
	}
}

func TestKthScanIgnoresAStuckProviderOnAForeignSource(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"remote registry": func(account map[string]any) {
			account["sources"].([]any)[0].(map[string]any)["registryUrl"] = "https://api.example.com/v1/models/api.json"
		},
		"remote provider": func(account map[string]any) {
			active := account["active"].(map[string]any)
			provider := active["providers"].([]any)[0].(map[string]any)
			provider["baseUrl"] = "https://api.example.com/v1"
		},
		"not the kth kind": func(account map[string]any) {
			account["sources"].([]any)[0].(map[string]any)["kind"] = "custom"
		},
		"disconnected": func(account map[string]any) {
			account["sources"].([]any)[0].(map[string]any)["status"] = "disconnected"
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := kthTestHome(t)
			account := AccountScope(home)
			document := kthTestStuckAccount("http://127.0.0.1:9100/v1", "byok-9f2c1d-kimi")
			mutate(document)
			kthTestWrite(t, home, kthTestStore(account, document))
			scan := kthStoreState(home, account, "http://127.0.0.1:9100/v1")
			if scan.needsDisconnect() {
				t.Fatalf("a foreign source would be disconnected: %+v", scan)
			}
		})
	}
}

func TestKthScanLeavesLegacyAndUnknownStoresAlone(t *testing.T) {
	home := kthTestHome(t)
	legacy, _ := json.Marshal(map[string]any{"v": 1, "profiles": map[string]any{}})
	if err := os.WriteFile(filepath.Join(home, NativeWorkStore), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if scan := kthStoreState(home, AccountScope(home), "http://127.0.0.1:9100/v1"); scan.valid {
		t.Fatalf("a legacy store was treated as v3: %+v", scan)
	}
	kthTestWrite(t, home, kthTestStore("another-account",
		kthTestStuckAccount("http://127.0.0.1:9100/v1", "byok-9f2c1d-kimi")))
	scan := kthStoreState(home, AccountScope(home), "http://127.0.0.1:9100/v1")
	if scan.valid || scan.needsDisconnect() {
		t.Fatalf("another account's source was claimed: %+v", scan)
	}
}

func TestKthScanClearsOnlyTheBlockedAliasesThatPrefixTheProvider(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	document := kthTestAccount("http://127.0.0.1:9100/v1", nil)
	document["sources"] = []any{}
	document["active"] = map[string]any{"providers": []any{}, "models": []any{
		map[string]any{"alias": "byok-uuid/model-a", "providerId": "byok-uuid"}}}
	document["blockedAliases"] = map[string]any{
		"byok-kth-kimi/kimi-a":  "delisted",
		"byok-kth-kimi/kimi-b":  "disconnected",
		"byok-uuid/model-a":     "delisted",
		"byok-kth-resp/gpt-a":   "delisted",
		"byok-kth-kimi/kimi-c*": "quarantined",
	}
	kthTestWrite(t, home, kthTestStore(account, document))

	scan := kthStoreState(home, account, "http://127.0.0.1:9100/v1")
	if !scan.needsBlockedCleanup() || len(scan.blocked) != 3 {
		t.Fatalf("scan: %+v", scan)
	}
	changed, err := clearKthBlockedAliases(home, account, "http://127.0.0.1:9100/v1")
	if err != nil || !changed {
		t.Fatalf("cleanup changed=%v err=%v", changed, err)
	}
	blocked := kthTestAccountOf(t, home)["blockedAliases"].(map[string]any)
	if _, present := blocked["byok-kth-kimi/kimi-a"]; present {
		t.Fatalf("a blocking alias survived: %v", blocked)
	}
	for _, alias := range []string{"byok-uuid/model-a", "byok-kth-resp/gpt-a"} {
		if _, present := blocked[alias]; !present {
			t.Fatalf("%s was removed: %v", alias, blocked)
		}
	}
	changed, err = clearKthBlockedAliases(home, account, "http://127.0.0.1:9100/v1")
	if err != nil || changed {
		t.Fatalf("cleanup was not idempotent: changed=%v err=%v", changed, err)
	}
}

func TestKthBlockedCleanupRefusesWhileAModelUsesThePrefixOrASourceExists(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"an active model uses the alias": func(account map[string]any) {
			account["sources"] = []any{}
			account["active"] = map[string]any{"providers": []any{}, "models": []any{
				map[string]any{"alias": "byok-kth-kimi/kimi-a", "providerId": kthProviderID}}}
		},
		"a pending model uses the alias": func(account map[string]any) {
			account["sources"] = []any{}
			account["pending"] = map[string]any{"providers": []any{}, "models": []any{
				map[string]any{"alias": "byok-kth-kimi/kimi-b", "providerId": kthProviderID}}}
		},
		"a live source exists": func(account map[string]any) {},
	} {
		t.Run(name, func(t *testing.T) {
			home := kthTestHome(t)
			account := AccountScope(home)
			document := kthTestAccount("http://127.0.0.1:9100/v1", nil)
			mutate(document)
			document["blockedAliases"] = map[string]any{"byok-kth-kimi/kimi-z": "delisted"}
			before := kthTestWrite(t, home, kthTestStore(account, document))

			scan := kthStoreState(home, account, "http://127.0.0.1:9100/v1")
			if scan.needsBlockedCleanup() {
				t.Fatalf("scan asked for a wrong cleanup: %+v", scan)
			}
			changed, err := clearKthBlockedAliases(home, account, "http://127.0.0.1:9100/v1")
			if err != nil || changed {
				t.Fatalf("cleanup changed=%v err=%v", changed, err)
			}
			after, err := os.ReadFile(filepath.Join(home, NativeWorkStore))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("the store was rewritten while a model still uses the alias")
			}
		})
	}
}

func TestKthRepairLeavesOtherAccountsAlone(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	baseURL := "http://127.0.0.1:9100/v1"
	store := kthTestStore(account, kthTestAccount(baseURL, nil))
	theirs := kthTestAccount(baseURL, map[string]any{"gpt": "byok-kth-resp"})
	store["accounts"].(map[string]any)["another-account"] = theirs
	kthTestWrite(t, home, store)

	changed, err := rebindKthRouteBinding(home, account, baseURL)
	if err != nil || !changed {
		t.Fatalf("rebind changed=%v err=%v", changed, err)
	}
	if bindings := kthTestSourceOf(t, kthTestAccountOf(t, home))["routeBindings"].(map[string]any); bindings[kimiRouteKey(baseURL)] != kthProviderID {
		t.Fatalf("the signed-in account was not rebound: %v", bindings)
	}
	other := kthTestRead(t, home)["accounts"].(map[string]any)["another-account"].(map[string]any)
	if bindings := kthTestSourceOf(t, other)["routeBindings"].(map[string]any); len(bindings) != 1 || bindings["gpt"] == nil {
		t.Fatalf("another account was written: %v", bindings)
	}
}

func TestKthStoreWriteLosingTheRaceLeavesTheFileAlone(t *testing.T) {
	home := kthTestHome(t)
	account := AccountScope(home)
	kthTestWrite(t, home, kthTestStore(account, kthTestAccount("http://127.0.0.1:9100/v1", nil)))
	file := filepath.Join(home, NativeWorkStore)
	concurrent := []byte(`{"v":3,"accounts":{},"writtenBy":"kimi"}`)

	changed, err := updateKthStore(home, func(map[string]any) bool {
		// The official application commits the store between this call's read and
		// its write.
		if writeErr := os.WriteFile(file, concurrent, 0o600); writeErr != nil {
			t.Error(writeErr)
		}
		return true
	})
	if !errors.Is(err, errKthStoreConflict) || changed {
		t.Fatalf("lost race changed=%v err=%v", changed, err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(concurrent) {
		t.Fatal("the concurrent write was overwritten")
	}
}

func TestKthStoreWriteRefusesUnsafeFiles(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		home := t.TempDir()
		outside := filepath.Join(t.TempDir(), "store.json")
		if err := os.WriteFile(outside, []byte(`{"v":3,"accounts":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, NativeWorkStore)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if changed, err := rebindKthRouteBinding(home, "k", "http://127.0.0.1:9100/v1"); changed || err != nil {
			t.Fatalf("a symlink was written through: changed=%v err=%v", changed, err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		home := t.TempDir()
		file := filepath.Join(home, NativeWorkStore)
		handle, err := os.Create(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.Truncate(maxStoreBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		if changed, err := clearKthBlockedAliases(home, "k", "http://127.0.0.1:9100/v1"); changed || err != nil {
			t.Fatalf("an oversized store was written: changed=%v err=%v", changed, err)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		home := t.TempDir()
		file := filepath.Join(home, NativeWorkStore)
		if err := os.WriteFile(file, []byte(`{"accounts":`), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := rebindKthRouteBinding(home, "k", "http://127.0.0.1:9100/v1"); changed || err != nil {
			t.Fatalf("a corrupt store was written: changed=%v err=%v", changed, err)
		}
	})
}

// ---------------------------------------------------------------------------
// the official disconnect expression

// runKthDisconnectExpression executes the real expression against a fake page
// whose byokSources store is set up by the test.
func runKthDisconnectExpression(t *testing.T, setup string) (*goja.Runtime, goja.Value) {
	t.Helper()
	vm := goja.New()
	if _, err := vm.RunString(kthDisconnectMock + setup); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + kthDisconnectExpression + `).then(result=>globalThis.kthResult=result);`); err != nil {
		t.Fatal(err)
	}
	return vm, vm.Get("kthResult")
}

const kthDisconnectMock = `
const calls=[];
let connected=true;
let status="applied";
let revision="a1";
const kthDefaultSource={id:"byok-kth",kind:"kth",revision:"k1",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"};
const kthCurrentSource=()=>globalThis.kthSource===undefined?kthDefaultSource:globalThis.kthSource;
const ok=value=>({ok:true,value});
globalThis.clawBridgeAPI={byokSources:{
  list(){calls.push("list");
    const rows=(connected?[kthCurrentSource()]:[]).filter(Boolean);
    const otherSources=globalThis.kthOtherSources||[];
    return ok({scopeToken:"s1",accountRevision:revision,applyState:"applied",sources:rows.concat(otherSources)});},
  disconnect(request){calls.push("disconnect:"+request.sourceId);
    if(globalThis.kthRefuse)return {ok:false,reasonCode:"byok-busy"};
    connected=false;revision="a2";
    return ok({status:status,accountRevision:revision});},
  applyPending(value){calls.push("apply:"+value);status="applied";revision="a3";return ok({status:"applied"});},
}};
`

func TestKthDisconnectExpressionAppliesTheOfficialDisconnect(t *testing.T) {
	vm, value := runKthDisconnectExpression(t, "")
	result := value.ToObject(vm)
	if !result.Get("ok").ToBoolean() || result.Get("status").String() != "applied" {
		t.Fatalf("answer: %v", result.Export())
	}
}

func TestKthDisconnectExpressionAppliesAPendingDisconnect(t *testing.T) {
	vm, _ := runKthDisconnectExpression(t, `
globalThis.clawBridgeAPI.byokSources.disconnect=function(request){
  calls.push("disconnect:"+request.sourceId);connected=false;revision="a2";
  return {ok:true,value:{status:"pending",accountRevision:"a2"}};};`)
	result := vm.Get("kthResult").ToObject(vm)
	if !result.Get("ok").ToBoolean() || result.Get("status").String() != "applied" {
		t.Fatalf("answer: %v", result.Export())
	}
	calls, err := vm.RunString(`calls.join(",")`)
	if err != nil || calls.String() != "list,disconnect:byok-kth,list,apply:a2" {
		t.Fatalf("calls = %v err=%v", calls, err)
	}
}

func TestKthDisconnectExpressionRefusesAForeignSource(t *testing.T) {
	for name, setup := range map[string]string{
		"remote registry": `globalThis.kthSource={id:"byok-kth",kind:"kth",registryUrl:"https://api.example.com/v1/models/api.json"};`,
		"another kind":    `globalThis.kthSource={id:"byok-kth",kind:"custom",registryUrl:"http://127.0.0.1:9100/v1/models/api.json"};`,
		"quarantined":     `globalThis.kthSource={id:"byok-kth",kind:"kth",quarantined:true,registryUrl:"http://127.0.0.1:9100/v1/models/api.json"};`,
		"absent":          `globalThis.kthSource=null;`,
	} {
		t.Run(name, func(t *testing.T) {
			vm := goja.New()
			if _, err := vm.RunString(kthDisconnectMock + setup); err != nil {
				t.Fatal(err)
			}
			if _, err := vm.RunString(`(` + kthDisconnectExpression + `).then(result=>globalThis.kthResult=result);`); err != nil {
				t.Fatal(err)
			}
			result := vm.Get("kthResult").ToObject(vm)
			if result.Get("ok").ToBoolean() {
				t.Fatalf("a foreign source was disconnected: %v", result.Export())
			}
			calls, err := vm.RunString(`calls.join(",")`)
			if err != nil || strings.Contains(calls.String(), "disconnect") {
				t.Fatalf("calls = %v err=%v", calls, err)
			}
		})
	}
}

func TestKthDisconnectExpressionSkipsALegacyBuildAndReportsARefusal(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(`globalThis.clawBridgeAPI={};`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + kthDisconnectExpression + `).then(result=>globalThis.kthResult=result);`); err != nil {
		t.Fatal(err)
	}
	skipped := vm.Get("kthResult").ToObject(vm)
	if !skipped.Get("ok").ToBoolean() || !skipped.Get("skipped").ToBoolean() {
		t.Fatalf("legacy build: %v", skipped.Export())
	}

	vm = goja.New()
	if _, err := vm.RunString(`globalThis.kthRefuse=true;` + kthDisconnectMock); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`(` + kthDisconnectExpression + `).then(result=>globalThis.kthResult=result);`); err != nil {
		t.Fatal(err)
	}
	refused := vm.Get("kthResult").ToObject(vm)
	if refused.Get("ok").ToBoolean() || refused.Get("code").String() != "byok-busy" {
		t.Fatalf("refusal: %v", refused.Export())
	}
}

// The page cannot apply a deferred reset itself: the Go side owns the bounded
// wait. It only hands over the revision to apply, and only when no other source
// has pending work of its own.
func TestKthDisconnectEmitsApplyCredentialsOnlyForAFreshDeferredReset(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        string
		wantScope    string
		wantRevision string
	}{
		{name: "a deferred disconnect", setup: `status="deferred";`, wantScope: "s1", wantRevision: "a2"},
		{name: "an applied disconnect", setup: ``},
		{name: "another source is pending",
			setup: `status="deferred";globalThis.kthOtherSources=[{id:"other",kind:"registry",models:[{status:"pending"}]}];`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm, _ := runKthDisconnectExpression(t, tc.setup)
			result := vm.Get("kthResult").ToObject(vm)
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
		})
	}
}

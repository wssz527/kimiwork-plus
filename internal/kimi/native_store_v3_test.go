package kimi

import (
	"os"
	"path/filepath"
	"testing"
)

// The 3.2.28 store is a different shape from the one this package grew up with:
// `accounts` instead of `profiles`, no account-wide base URL or credential, and
// an `active`/`pending` split. These fixtures use the released field names so a
// later rename in the official store fails here rather than on a user's machine.

// v3Account is one account as 3.2.28 writes it.
func v3Account(baseURL, credentialRef string) map[string]any {
	return map[string]any{
		"revision": "account-rev-1",
		"sources": []any{map[string]any{
			"id": "byok-kth", "kind": "kth", "displayName": "Kimi Work+",
			"revision": "source-rev-1", "discoveryMode": "manual",
			"desired": map[string]any{"providers": []any{}, "models": []any{}},
			"catalogBase": map[string]any{
				"providers": []any{}, "models": []any{}, "format": "openaiList",
			},
			"routeBindings": map[string]any{"kimi": "byok-kth-kimi", "gpt": "byok-kth-resp"},
			"lastCheckedAt": "2026-11-18T00:00:00.000Z",
		}},
		"active": map[string]any{
			"rev": "active-rev-1",
			"providers": []any{
				map[string]any{"id": "byok-kth-kimi", "sourceId": "byok-kth", "protocol": "kimi",
					"baseUrl": baseURL, "credentialRef": credentialRef, "credentialVersion": "cv-1",
					"publicHeaders": map[string]any{}},
				map[string]any{"id": "byok-kth-resp", "sourceId": "byok-kth", "protocol": "openai_responses",
					"baseUrl": baseURL, "credentialRef": credentialRef, "credentialVersion": "cv-1",
					"publicHeaders": map[string]any{}},
			},
			"models": []any{
				map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
					"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072,
					"maxOutputSize": 8192, "capabilities": []any{"tool_use"}, "sourceId": "byok-kth"},
				map[string]any{"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
					"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144,
					"sourceId": "byok-kth"},
			},
		},
		"blockedAliases": map[string]any{},
	}
}

// seedV3Store writes the account store and the generated config 3.2.28 leaves
// after it has applied that account.
func (f *nativeFixture) seedV3Store(t *testing.T, key, baseURL, credentialRef string) {
	t.Helper()
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkStore), map[string]any{
		"v": 3, "accounts": map[string]any{key: v3Account(baseURL, credentialRef)},
		"ownedResources": map[string]any{
			"providerIds":    []any{"byok-kth-kimi", "byok-kth-resp"},
			"modelAliases":   []any{"byok-kth-kimi/kimi-probe-model", "byok-kth-resp/gpt-probe-model"},
			"credentialRefs": []any{credentialRef},
		},
		"secrets": map[string]any{credentialRef: map[string]any{"encryption": "dpapi", "data": "sealed"}},
	})
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkConfig), map[string]any{
		"model": map[string]any{
			"providers": map[string]any{
				"byok-kth-kimi": map[string]any{"type": "kimi", "baseUrl": baseURL, "credential": credentialRef},
				"byok-kth-resp": map[string]any{"type": "openai_responses", "baseUrl": baseURL, "credential": credentialRef},
			},
			"models": map[string]any{
				"byok-kth-kimi/kimi-probe-model": map[string]any{"provider": "byok-kth-kimi",
					"model": "kimi-probe-model", "maxContextSize": float64(131072),
					"maxOutputSize": float64(8192), "capabilities": []any{"tool_use"}},
				"byok-kth-resp/gpt-probe-model": map[string]any{"provider": "byok-kth-resp",
					"model": "gpt-probe-model", "maxContextSize": float64(262144)},
			},
		},
		"credentials": map[string]any{credentialRef: map[string]any{"apiKey": "synthetic-not-a-real-key"}},
	})
}

// A v3 store used to answer `profile_missing`, because the reader only knew the
// legacy `profiles` root. The account is present, applied and generated, so the
// honest answer is `ready`.
func TestNativeAccountStoreIsReadNotReportedMissing(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: url, Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationReady {
		t.Fatalf("v3 account: %s/%s", result.State, result.Reason)
	}
	if result.Changed {
		t.Fatal("a v3 store was written to")
	}
	if result.CredentialRef != "cred-1" || result.ProfileKeyDigest != sha256Hex(key) {
		t.Fatalf("v3 answer: %+v", result)
	}
}

// The account key is the same level in both store generations, so an account
// switch must not be reported just because the store moved to `accounts`.
func TestNativeAccountStoreKeepsTheSameScopeLevel(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	oldKey, newKey := sha256Hex("account-old"), sha256Hex("account-new")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), map[string]any{
		"v": 3, "accounts": map[string]any{
			oldKey: v3Account(url, "cred-old"),
			newKey: v3Account(url, "cred-1"),
		},
		"ownedResources": map[string]any{"providerIds": []any{}, "modelAliases": []any{}, "credentialRefs": []any{}},
		"secrets":        map[string]any{},
	})
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), map[string]any{
		"model": map[string]any{
			"providers": map[string]any{
				"byok-kth-kimi": map[string]any{"type": "kimi", "baseUrl": url, "credential": "cred-1"},
				"byok-kth-resp": map[string]any{"type": "openai_responses", "baseUrl": url, "credential": "cred-1"},
			},
			"models": map[string]any{
				"byok-kth-kimi/kimi-probe-model": map[string]any{"provider": "byok-kth-kimi",
					"model": "kimi-probe-model", "maxContextSize": float64(131072),
					"maxOutputSize": float64(8192), "capabilities": []any{"tool_use"}},
				"byok-kth-resp/gpt-probe-model": map[string]any{"provider": "byok-kth-resp",
					"model": "gpt-probe-model", "maxContextSize": float64(262144)},
			},
		},
		"credentials": map[string]any{"cred-1": map[string]any{"apiKey": "synthetic-not-a-real-key"}},
	})
	fixture.seedOldIntent(t, switchedAccountsFor("account-old", "account-new", "cred-1"), url)

	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	intent, err := fixture.integration(t).readIntent()
	if err != nil {
		t.Fatal(err)
	}
	chosen, ambiguous := selectProfile(kthProfiles(store), hashOf(newKey), intent)
	if ambiguous || chosen.key != newKey {
		t.Fatalf("v3 selection: ambiguous=%v key=%q", ambiguous, chosen.key)
	}
	if result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: newKey, AllowAccountRebind: true}); result.State != IntegrationReady {
		t.Fatalf("v3 target account: %s/%s", result.State, result.Reason)
	}
}

func hashOf(value string) string { return sha256Hex(value) }

// A blocked alias stays blocked even while an entry for it is still applied: the
// status map is the account's own word about it.
func TestNativeAccountStoreHonoursBlockedAliases(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")
	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	account := store["accounts"].(map[string]any)[key].(map[string]any)
	account["blockedAliases"] = map[string]any{"byok-kth-resp/gpt-probe-model": "delisted"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
		t.Fatalf("blocked alias: %s/%s", result.State, result.Reason)
	}
}

// `pending` survives an apply until the application confirms it, so only a model
// that is not applied yet means a transaction is in flight.
func TestNativeAccountStoreOnlyWaitsForUnappliedPending(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")
	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	account := store["accounts"].(map[string]any)[key].(map[string]any)
	active := account["active"].(map[string]any)
	account["pending"] = map[string]any{"rev": "active-rev-1", "providers": active["providers"],
		"models": active["models"], "applyState": "ready"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)

	if result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: key}); result.State != IntegrationReady {
		t.Fatalf("applied pending: %s/%s", result.State, result.Reason)
	}

	account["pending"] = map[string]any{"rev": "next-rev", "providers": active["providers"],
		"models": append(active["models"].([]any), map[string]any{"alias": "byok-kth-kimi/kimi-next-model",
			"providerId": "byok-kth-kimi", "model": "kimi-next-model", "family": "kimi",
			"maxContextSize": float64(131072)}), "applyState": "prepared"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationPending || result.Reason != "native_profile_pending" {
		t.Fatalf("unapplied pending: %s/%s", result.State, result.Reason)
	}
}

// The official application generates `config.json` from this store, so a
// generated entry that belongs to someone else is a conflict, not something to
// overwrite.
func TestNativeAccountStoreNeverWritesOverForeignConfig(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	config["model"].(map[string]any)["providers"].(map[string]any)["byok-kth-kimi"].(map[string]any)["baseUrl"] = "https://other.example/v1"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)
	before := fixture.rawConfig(t)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationConflict || result.Reason != "foreign_provider" {
		t.Fatalf("foreign provider: %s/%s", result.State, result.Reason)
	}
	if after := fixture.rawConfig(t); after != before {
		t.Fatal("a v3 store's generated config was rewritten")
	}
}

// A generated config that has not caught up yet is reported as generating, not
// as a foreign entry.
func TestNativeAccountStoreWaitsForGeneratedConfig(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	delete(config["model"].(map[string]any)["models"].(map[string]any), "byok-kth-resp/gpt-probe-model")
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationWaitingConfig || result.Reason != "config_not_generated" {
		t.Fatalf("generating config: %s/%s", result.State, result.Reason)
	}
}

// A v3 account whose providers point somewhere else is somebody else's
// configuration, even when the account key matches.
func TestNativeAccountStoreRefusesForeignProviderURL(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, "https://other.example/v1", "cred-1")

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "foreign_url" {
		t.Fatalf("foreign url: %s/%s", result.State, result.Reason)
	}
}

// The readback is what the panel shows, and it has to answer for a v3 store too.
func TestNativeReadbackReadsTheAccountStore(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")

	readback := fixture.integration(t).Readback(key)
	if !readback.StorePresent || readback.Models != 2 || readback.CredentialRef != "cred-1" {
		t.Fatalf("v3 readback: %+v", readback)
	}
	if !readback.Holds(url) {
		t.Fatal("v3 readback did not hold at the configured URL")
	}
}

// The catalog check has to accept the v3 account too, or every switch starts an
// import that is not needed.
func TestAccountCatalogReadsTheAccountStore(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, url, "cred-1")
	settings := ControllerSettings{GatewayURL: url, GatewayToken: "synthetic-not-a-real-key", Models: testCatalog()}

	entries, reason, needsImport := fixture.integration(t).accountCatalog(settings, key)
	if reason != "" || needsImport {
		t.Fatalf("v3 catalog: reason=%s needsImport=%v", reason, needsImport)
	}
	if len(entries) != 2 {
		t.Fatalf("v3 catalog entries: %d", len(entries))
	}
	for _, entry := range entries {
		if entry.provider["credential"] != "cred-1" {
			t.Fatalf("v3 provider terms: %v", entry.provider)
		}
	}
}

func TestAccountCatalogRecognizesOwnPreviousLoopbackURL(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, "http://127.0.0.1:9998/v1", "cred-1")
	settings := ControllerSettings{GatewayURL: "http://127.0.0.1:9999/v1", GatewayToken: "synthetic-not-a-real-key", Models: testCatalog()}

	_, reason, needsImport := fixture.integration(t).accountCatalog(settings, key)
	if reason != "base_url_changed" || !needsImport {
		t.Fatalf("own previous loopback URL: reason=%s needsImport=%v", reason, needsImport)
	}
}

func TestAccountCatalogStillRefusesForeignURL(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("account-new")
	fixture.seedV3Store(t, key, "https://other.example/v1", "cred-1")
	settings := ControllerSettings{GatewayURL: "http://127.0.0.1:9999/v1", GatewayToken: "synthetic-not-a-real-key", Models: testCatalog()}

	_, reason, needsImport := fixture.integration(t).accountCatalog(settings, key)
	if reason != "foreign_url" || needsImport {
		t.Fatalf("foreign URL: reason=%s needsImport=%v", reason, needsImport)
	}
}

func (f *nativeFixture) rawConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, NativeWorkConfig))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// An installation upgraded in place can hold a store with both roots. The legacy
// root is the one this package has always owned, so it keeps winning, and the
// v3 root does not make the answer ambiguous.
func TestNativeStorePrefersTheLegacyRootWhenBothArePresent(t *testing.T) {
	fixture := newNativeFixture(t)
	const url = "http://127.0.0.1:9999/v1"
	key := fixture.seedProfile(t, url, "cred-1")
	store := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkStore))
	store["v"] = 3
	store["accounts"] = map[string]any{
		key:                   v3Account(url, "cred-1"),
		sha256Hex("other-id"): v3Account(url, "cred-2"),
	}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), store)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true, URL: url,
		Models: testCatalog()})
	if result.State != IntegrationRepaired || result.Reason != "" {
		t.Fatalf("dual-root store: %s/%s", result.State, result.Reason)
	}
	if len(kthProfiles(store)) != 1 {
		t.Fatalf("dual-root store produced %d candidates", len(kthProfiles(store)))
	}
}

// An account with no applied model is not a profile: reporting one as `ready`
// would let the panel claim models that are not there.
func TestNativeAccountStoreWithoutModelsIsNotAProfile(t *testing.T) {
	fixture := newNativeFixture(t)
	key := sha256Hex("account-new")
	account := v3Account("http://127.0.0.1:9999/v1", "cred-1")
	account["active"].(map[string]any)["models"] = []any{}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), map[string]any{
		"v": 3, "accounts": map[string]any{key: account},
	})

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog(), AccountScope: key})
	if result.State != IntegrationNeedsImport || result.Reason != "profile_missing" {
		t.Fatalf("empty account: %s/%s", result.State, result.Reason)
	}
}

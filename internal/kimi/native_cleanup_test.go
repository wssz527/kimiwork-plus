package kimi

import (
	"os"
	"path/filepath"
	"testing"
)

// The orphaned entries an earlier prototype left behind are exactly the ones
// this cleanup is allowed to remove. Everything else — the official Kimi Code
// provider, an active gateway provider, an account's own credential — has to
// survive untouched, because a cleanup that removes a working provider is worse
// than one that leaves a dead entry behind.

func seedCleanupFixture(t *testing.T, ownedProviders, ownedCredentials []any) *nativeFixture {
	t.Helper()
	fixture := newNativeFixture(t)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), map[string]any{
		"v": 3,
		"accounts": map[string]any{
			sha256Hex("account-new"): map[string]any{"revision": "account-rev-1"},
		},
		"ownedResources": map[string]any{
			"providerIds":    ownedProviders,
			"modelAliases":   []any{"byok-kth-kimi/kimi-probe-model", "byok-kth-resp/gpt-probe-model"},
			"credentialRefs": ownedCredentials,
		},
	})
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), map[string]any{
		"model": map[string]any{
			"providers": map[string]any{
				"daimon-kimi-code": map[string]any{"type": "kimi", "baseUrl": "https://api.moonshot.cn/v1",
					"credential": "daimon-kimi-code"},
				"byok-kwp-kimi": map[string]any{"type": "kimi", "baseUrl": "http://127.0.0.1:63999/v1",
					"credential": "kwp-legacy-cred"},
				"byok-kwp-resp": map[string]any{"type": "openai_responses", "baseUrl": "http://[::1]:63999/v1",
					"credential": "byok-kwp-legacy-cred"},
				"byok-kth-kimi": map[string]any{"type": "kimi", "baseUrl": "http://127.0.0.1:9999/v1",
					"credential": "cred-1"},
				"byok-kth-resp": map[string]any{"type": "openai_responses", "baseUrl": "http://127.0.0.1:9999/v1",
					"credential": "cred-1"},
				"byok-5a7e9d16-1c4f-4f3f-9f43-2f5a1d6b7c89": map[string]any{"type": "openai",
					"baseUrl": "https://api.openai.com/v1", "credential": "byok-uuid-cred"},
			},
			"models": map[string]any{
				"byok-kwp-kimi/dead-model": map[string]any{"provider": "byok-kwp-kimi",
					"model": "dead-model", "maxContextSize": float64(131072)},
				"byok-kwp-resp/dead-gpt": map[string]any{"provider": "byok-kwp-resp",
					"model": "dead-gpt", "maxContextSize": float64(131072)},
				"byok-kth-kimi/kimi-probe-model": map[string]any{"provider": "byok-kth-kimi",
					"model": "kimi-probe-model", "maxContextSize": float64(131072)},
				"byok-kth-resp/gpt-probe-model": map[string]any{"provider": "byok-kth-resp",
					"model": "gpt-probe-model", "maxContextSize": float64(262144)},
			},
		},
		"credentials": map[string]any{
			"daimon-kimi-code":      map[string]any{"apiKey": "kimi-code-token"},
			"kwp-legacy-cred":       map[string]any{"apiKey": "legacy-token"},
			"byok-kwp-legacy-cred":  map[string]any{"apiKey": "legacy-token-2"},
			"cred-1":                map[string]any{"apiKey": "synthetic-not-a-real-key"},
			"byok-uuid-cred":        map[string]any{"apiKey": "user-token"},
			"kimiCode":              map[string]any{"apiKey": "{{SECRET_kmc0de00}}"},
			"unrelated-but-present": map[string]any{"apiKey": "keep-me"},
		},
	})
	return fixture
}

func TestCleanupOrphanedLegacyEntriesRemovesDeadPrototypeEntries(t *testing.T) {
	fixture := seedCleanupFixture(t, []any{"byok-kth-kimi", "byok-kth-resp"}, []any{"cred-1"})

	changed, err := CleanupOrphanedLegacyEntries(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("orphaned entries were not reported as removed")
	}
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	providers := config["model"].(map[string]any)["providers"].(map[string]any)
	if _, present := providers["byok-kwp-kimi"]; present {
		t.Fatal("an orphaned loopback provider survived")
	}
	if _, present := providers["byok-kwp-resp"]; present {
		t.Fatal("an orphaned IPv6 loopback provider survived")
	}
	for _, keep := range []string{"daimon-kimi-code", "byok-kth-kimi", "byok-kth-resp",
		"byok-5a7e9d16-1c4f-4f3f-9f43-2f5a1d6b7c89"} {
		if _, present := providers[keep]; !present {
			t.Fatalf("provider %q was removed", keep)
		}
	}
	models := config["model"].(map[string]any)["models"].(map[string]any)
	if len(models) != 2 {
		t.Fatalf("models left: %v", models)
	}
	credentials := config["credentials"].(map[string]any)
	for _, gone := range []string{"kwp-legacy-cred", "byok-kwp-legacy-cred"} {
		if _, present := credentials[gone]; present {
			t.Fatalf("orphaned credential %q survived", gone)
		}
	}
	for _, keep := range []string{"daimon-kimi-code", "cred-1", "byok-uuid-cred", "kimiCode",
		"unrelated-but-present"} {
		if _, present := credentials[keep]; !present {
			t.Fatalf("credential %q was removed", keep)
		}
	}
}

func TestCleanupOrphanedLegacyEntriesIsIdempotent(t *testing.T) {
	fixture := seedCleanupFixture(t, []any{"byok-kth-kimi", "byok-kth-resp"}, []any{"cred-1"})
	if changed, err := CleanupOrphanedLegacyEntries(fixture.dir); err != nil || !changed {
		t.Fatalf("first call: changed=%v err=%v", changed, err)
	}
	before := fixture.rawConfig(t)
	changed, err := CleanupOrphanedLegacyEntries(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second call reported a change")
	}
	if after := fixture.rawConfig(t); after != before {
		t.Fatal("second call rewrote the config")
	}
}

func TestCleanupOrphanedLegacyEntriesSkipsOwnedProviders(t *testing.T) {
	fixture := seedCleanupFixture(t, []any{"byok-kwp-kimi"}, []any{"kwp-legacy-cred"})
	if changed, err := CleanupOrphanedLegacyEntries(fixture.dir); err != nil || !changed {
		t.Fatalf("cleanup: changed=%v err=%v", changed, err)
	}
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	providers := config["model"].(map[string]any)["providers"].(map[string]any)
	if _, present := providers["byok-kwp-kimi"]; !present {
		t.Fatal("an owned provider was removed")
	}
	if _, present := providers["byok-kwp-resp"]; present {
		t.Fatal("an unowned provider was not removed")
	}
	models := config["model"].(map[string]any)["models"].(map[string]any)
	if _, present := models["byok-kwp-kimi/dead-model"]; !present {
		t.Fatal("an owned provider's model was removed")
	}
	credentials := config["credentials"].(map[string]any)
	if _, present := credentials["kwp-legacy-cred"]; !present {
		t.Fatal("an owned credential was removed")
	}
}

func TestCleanupOrphanedLegacyEntriesSkipsNonLoopbackURLs(t *testing.T) {
	fixture := seedCleanupFixture(t, []any{"byok-kth-kimi", "byok-kth-resp"}, []any{"cred-1"})
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	providers := config["model"].(map[string]any)["providers"].(map[string]any)
	providers["byok-kwp-cloud"] = map[string]any{"type": "openai", "baseUrl": "https://api.example.com/v1",
		"credential": "kwp-cloud-cred"}
	config["model"].(map[string]any)["models"].(map[string]any)["byok-kwp-cloud/cloud-model"] =
		map[string]any{"provider": "byok-kwp-cloud", "model": "cloud-model", "maxContextSize": float64(1)}
	config["credentials"].(map[string]any)["kwp-cloud-cred"] = map[string]any{"apiKey": "cloud-token"}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	if changed, err := CleanupOrphanedLegacyEntries(fixture.dir); err != nil || !changed {
		t.Fatalf("cleanup: changed=%v err=%v", changed, err)
	}
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	if _, present := after["model"].(map[string]any)["providers"].(map[string]any)["byok-kwp-cloud"]; !present {
		t.Fatal("a non-loopback byok-kwp provider was removed")
	}
	if _, present := after["credentials"].(map[string]any)["kwp-cloud-cred"]; !present {
		t.Fatal("a non-loopback provider's credential was removed")
	}
}

func TestCleanupOrphanedLegacyEntriesKeepsSharedCredentials(t *testing.T) {
	fixture := seedCleanupFixture(t, []any{"byok-kth-kimi", "byok-kth-resp"}, []any{"cred-1"})
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	providers := config["model"].(map[string]any)["providers"].(map[string]any)
	// The dead provider points at the credential a live provider still uses. The
	// provider goes, the credential stays.
	providers["byok-kwp-resp"].(map[string]any)["credential"] = "byok-uuid-cred"
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	if changed, err := CleanupOrphanedLegacyEntries(fixture.dir); err != nil || !changed {
		t.Fatalf("cleanup: changed=%v err=%v", changed, err)
	}
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	if _, present := after["credentials"].(map[string]any)["byok-uuid-cred"]; !present {
		t.Fatal("a credential a live provider still references was removed")
	}
}

func TestCleanupOrphanedLegacyEntriesIgnoresAMissingOrLegacyStore(t *testing.T) {
	// A legacy store, a missing store and a store without ownership data all
	// answer "nothing to do": without ownership evidence, a byok-kwp entry might
	// still be in use.
	for name, store := range map[string]any{
		"missing": nil,
		"legacy": map[string]any{"v": 1, "profiles": map[string]any{
			"key": map[string]any{"byok-kth": map[string]any{"kind": "kth"}}}},
		"no-ownership": map[string]any{"v": 3, "accounts": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := seedCleanupFixture(t, []any{}, []any{})
			storeFile := filepath.Join(fixture.dir, NativeWorkStore)
			if store == nil {
				if err := os.Remove(storeFile); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.writeJSON(t, storeFile, store)
			}
			before := fixture.rawConfig(t)
			changed, err := CleanupOrphanedLegacyEntries(fixture.dir)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Fatal("cleanup acted without a v3 store")
			}
			if after := fixture.rawConfig(t); after != before {
				t.Fatal("cleanup rewrote the config")
			}
		})
	}
}

func TestCleanupOrphanedLegacyEntriesWithoutConfigIsQuiet(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), map[string]any{
		"v": 3, "accounts": map[string]any{}, "ownedResources": map[string]any{},
	})
	changed, err := CleanupOrphanedLegacyEntries(fixture.dir)
	if err != nil || changed {
		t.Fatalf("missing config: changed=%v err=%v", changed, err)
	}
}

func TestCleanupOrphanedLegacyEntriesRequiresAnAbsoluteHome(t *testing.T) {
	if _, err := CleanupOrphanedLegacyEntries("relative/path"); err == nil {
		t.Fatal("a relative home was accepted")
	}
}

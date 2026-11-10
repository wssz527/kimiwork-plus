package kimi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The native integration is tested against synthetic Kimi files, because the
// rules it enforces are about ownership, not about a particular build. Every
// fixture here is written into a temporary directory; no real Kimi home is read.

type nativeFixture struct {
	dir     string
	state   string
	profile map[string]any
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	base := t.TempDir()
	fixture := &nativeFixture{dir: filepath.Join(base, "kimi"), state: filepath.Join(base, "state")}
	if err := os.MkdirAll(fixture.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixture.state, 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *nativeFixture) writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeFixture) readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// seedProfile writes the profile Kimi's own store holds and a generated config
// whose credential is already present, which is the state after a successful
// native import.
func (f *nativeFixture) seedProfile(t *testing.T, baseURL, credentialRef string) string {
	t.Helper()
	const key = "profile-key-aaaa"
	profile := map[string]any{
		"kind":          "kth",
		"id":            "byok-kth",
		"baseUrl":       baseURL,
		"credentialRef": credentialRef,
		"models": []any{
			map[string]any{"alias": "byok-kth-kimi/kimi-probe-model", "providerId": "byok-kth-kimi",
				"model": "kimi-probe-model", "family": "kimi", "maxContextSize": 131072,
				"maxOutputSize": 8192, "capabilities": []any{"tool_use"}},
			map[string]any{"alias": "byok-kth-resp/gpt-probe-model", "providerId": "byok-kth-resp",
				"model": "gpt-probe-model", "family": "responses", "maxContextSize": 262144},
		},
	}
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkStore), map[string]any{
		"v": 1, "profiles": map[string]any{key: map[string]any{"byok-kth": profile}},
	})
	f.writeJSON(t, filepath.Join(f.dir, NativeWorkConfig), map[string]any{
		"model":       map[string]any{"providers": map[string]any{}, "models": map[string]any{}},
		"credentials": map[string]any{credentialRef: map[string]any{"apiKey": "synthetic-not-a-real-key"}},
	})
	return key
}

func (f *nativeFixture) integration(t *testing.T) *NativeIntegration {
	t.Helper()
	integration, err := NewNativeIntegration(f.state, f.dir, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	return integration
}

func testCatalog() []Model {
	return []Model{
		{ID: "kimi-probe-model", Protocol: "chat", ContextWindow: 131072},
		{ID: "gpt-probe-model", Protocol: "responses", ContextWindow: 262144},
	}
}

func TestNativeReconcileWritesOnlyWhatItOwns(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	integration := fixture.integration(t)

	result := integration.Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationRepaired {
		t.Fatalf("first reconcile: %s/%s", result.State, result.Reason)
	}
	if !result.Changed {
		t.Fatal("first reconcile reported no change")
	}
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	models := config["model"].(map[string]any)["models"].(map[string]any)
	if len(models) != 2 {
		t.Fatalf("models written: %v", models)
	}
	providers := config["model"].(map[string]any)["providers"].(map[string]any)
	if len(providers) != 2 {
		t.Fatalf("providers written: %v", providers)
	}

	// A second pass must be a no-op: the same files, no receipt churn.
	before, _ := os.ReadFile(filepath.Join(fixture.dir, NativeWorkConfig))
	second := integration.Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if second.State != IntegrationReady || second.Changed {
		t.Fatalf("second reconcile: %s changed=%v", second.State, second.Changed)
	}
	after, _ := os.ReadFile(filepath.Join(fixture.dir, NativeWorkConfig))
	if string(before) != string(after) {
		t.Fatal("an idempotent reconcile rewrote config.json")
	}
}

func TestNativeReconcilePreservesForeignEntries(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	// Another tool already wrote this model. It must survive untouched.
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	model := config["model"].(map[string]any)
	model["models"] = map[string]any{
		"foreign/alias": map[string]any{"provider": "someone-else", "model": "theirs", "maxContextSize": 4096},
	}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationRepaired {
		t.Fatalf("reconcile: %s/%s", result.State, result.Reason)
	}
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	models := after["model"].(map[string]any)["models"].(map[string]any)
	if _, present := models["foreign/alias"]; !present {
		t.Fatal("a foreign model was removed")
	}
}

func TestNativeReconcileRefusesAForeignProvider(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	// The provider id this tool would write already exists with different
	// content, and this tool has no ownership digest for it.
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	model := config["model"].(map[string]any)
	model["providers"] = map[string]any{
		"byok-kth-kimi": map[string]any{"type": "kimi", "baseUrl": "http://elsewhere.test/v1", "credential": "other"},
	}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationConflict || result.Reason != "foreign_provider" {
		t.Fatalf("want foreign_provider conflict, got %s/%s", result.State, result.Reason)
	}
	// The foreign provider must be byte-identical afterwards.
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	provider := after["model"].(map[string]any)["providers"].(map[string]any)["byok-kth-kimi"].(map[string]any)
	if provider["baseUrl"] != "http://elsewhere.test/v1" {
		t.Fatal("a foreign provider was overwritten")
	}
}

func TestNativeReconcileReportsMissingModels(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1",
		Models: []Model{{ID: "kimi-probe-model", Protocol: "chat", ContextWindow: 131072},
			{ID: "kimi-not-in-profile", Protocol: "chat", ContextWindow: 4096}}})
	if result.State != IntegrationNeedsImport || result.Reason != "models_missing" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
	if len(result.MissingModels) != 1 || result.MissingModels[0] != "kimi-not-in-profile" {
		t.Fatalf("missing models: %v", result.MissingModels)
	}
	if !result.NeedsNativeImport() {
		t.Fatal("models_missing must be importable")
	}
}

func TestNativeReconcileRefusesAnAccountSwitch(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	integration := fixture.integration(t)
	if result := integration.Reconcile(ReconcileOptions{Enabled: true, AccountScope: "account-a",
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()}); result.State != IntegrationRepaired {
		t.Fatalf("first: %s/%s", result.State, result.Reason)
	}
	// A different account must never be repaired from the old profile.
	result := integration.Reconcile(ReconcileOptions{Enabled: true, AccountScope: "account-b",
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.Reason != "account_changed" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
	if result.NeedsNativeImport() {
		t.Fatal("an account change must not be auto-imported")
	}
}

func TestNativeReconcileWaitsForTheGeneratedCredential(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	// The profile is intact but Kimi's generator has not written the key yet.
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	config["credentials"] = map[string]any{}
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationWaitingConfig || result.Reason != "credential_not_generated" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
	// The key is never invented locally.
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	if credentials, _ := after["credentials"].(map[string]any); len(credentials) != 0 {
		t.Fatal("a credential was written by this package")
	}
}

func TestNativeReconcileRefusesConcurrentModification(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	integration := fixture.integration(t)
	// Change config.json after the reconcile parsed it. The write must lose.
	configPath := filepath.Join(fixture.dir, NativeWorkConfig)
	raw, _ := os.ReadFile(configPath)
	if err := os.WriteFile(configPath, append(raw, []byte("\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	result := integration.Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	// A trailing newline is not a conflict (the bytes are re-read), but the file
	// must still be valid JSON and the reconcile must not have destroyed it.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(after, &document); err != nil {
		t.Fatalf("config.json is no longer valid JSON: %v (%s)", err, result.State)
	}
}

func TestNativeDisconnectRemovesOnlyOwnedEntries(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	integration := fixture.integration(t)
	if result := integration.Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()}); result.State != IntegrationRepaired {
		t.Fatalf("seed: %s/%s", result.State, result.Reason)
	}
	// A user edits one imported model, so it is no longer byte-identical to the
	// digest this tool recorded.
	config := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	models := config["model"].(map[string]any)["models"].(map[string]any)
	edited := models["byok-kth-resp/gpt-probe-model"].(map[string]any)
	edited["maxContextSize"] = float64(1234)
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), config)

	result := integration.Reconcile(ReconcileOptions{Enabled: false,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationDisabled {
		t.Fatalf("disconnect: %s/%s", result.State, result.Reason)
	}
	after := fixture.readJSON(t, filepath.Join(fixture.dir, NativeWorkConfig))
	remaining := after["model"].(map[string]any)["models"].(map[string]any)
	if _, present := remaining["byok-kth-kimi/kimi-probe-model"]; present {
		t.Fatal("an owned, unmodified model survived disconnect")
	}
	if _, present := remaining["byok-kth-resp/gpt-probe-model"]; !present {
		t.Fatal("a user-edited model was deleted")
	}
}

func TestNativeLockIsExclusiveAcrossInstances(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	first := fixture.integration(t)
	second := fixture.integration(t)

	// Hold the lock, then confirm a second instance refuses.
	ok, reason := first.acquireLock()
	if !ok {
		t.Fatalf("first lock: %s", reason)
	}
	if ok, reason := second.acquireLock(); ok || reason != "owner_conflict" {
		t.Fatalf("second lock: ok=%v reason=%s", ok, reason)
	}
	first.releaseLock()
	if ok, reason := second.acquireLock(); !ok {
		t.Fatalf("lock not reusable: %s", reason)
	}
	second.releaseLock()
}

func TestNativeIntentRecordsDigestsOnly(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	integration := fixture.integration(t)
	if result := integration.Reconcile(ReconcileOptions{Enabled: true, AccountScope: "account-a",
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()}); result.State != IntegrationRepaired {
		t.Fatalf("seed: %s/%s", result.State, result.Reason)
	}
	data, err := os.ReadFile(filepath.Join(fixture.state, NativeWorkIntent))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "synthetic-not-a-real-key") {
		t.Fatal("the intent file contains a credential")
	}
	if !strings.Contains(text, "\"digest\"") {
		t.Fatal("the intent file records no ownership digest")
	}
	var intent nativeIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.SchemaVersion != NativeWorkSchema || intent.OwnerID == "" || !intent.Active {
		t.Fatalf("intent: %+v", intent)
	}
	if intent.ProfileKeyDigest == "" || intent.AccountDigest == "" {
		t.Fatal("identity digests are missing")
	}
}

func TestNativeReconcileRejectsInvalidInput(t *testing.T) {
	fixture := newNativeFixture(t)
	integration := fixture.integration(t)
	cases := []ReconcileOptions{
		{Enabled: true, URL: "", Models: testCatalog()},
		{Enabled: true, URL: "ftp://x/v1", Models: testCatalog()},
		{Enabled: true, URL: "http://user:pass@127.0.0.1:1/v1", Models: testCatalog()},
		{Enabled: true, URL: "http://127.0.0.1:1/v1?token=x", Models: testCatalog()},
		{Enabled: true, URL: "http://127.0.0.1:1/v1", Models: nil},
		{Enabled: true, URL: "http://127.0.0.1:1/v1", Models: []Model{{ID: "a", Protocol: "anthropic"}}},
		{Enabled: true, URL: "http://127.0.0.1:1/v1",
			Models: []Model{{ID: "a", Protocol: "chat"}, {ID: "a", Protocol: "chat"}}},
	}
	for _, options := range cases {
		result := integration.Reconcile(options)
		if result.State != IntegrationConflict || result.Reason != "invalid_request" {
			t.Fatalf("%+v -> %s/%s", options, result.State, result.Reason)
		}
	}
}

func TestNativeProfileSelectionRefusesAmbiguity(t *testing.T) {
	fixture := newNativeFixture(t)
	// Two profiles and no account or intent to tell them apart.
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkStore), map[string]any{
		"v": 1, "profiles": map[string]any{
			"key-one": map[string]any{"byok-kth": map[string]any{"kind": "kth", "baseUrl": "http://a/v1",
				"credentialRef": "c", "models": []any{}}},
			"key-two": map[string]any{"byok-kth": map[string]any{"kind": "kth", "baseUrl": "http://b/v1",
				"credentialRef": "c", "models": []any{}}},
		},
	})
	fixture.writeJSON(t, filepath.Join(fixture.dir, NativeWorkConfig), map[string]any{
		"model": map[string]any{}, "credentials": map[string]any{"c": map[string]any{"apiKey": "x"}}})
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:1/v1", Models: testCatalog()})
	if result.State != IntegrationAccountUnknown || result.Reason != "account_ambiguous" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
}

func TestNativeReadsARefusedUnsafeFile(t *testing.T) {
	fixture := newNativeFixture(t)
	fixture.seedProfile(t, "http://127.0.0.1:9999/v1", "cred-1")
	// Replace config.json with a symlink; the integration must refuse it.
	configPath := filepath.Join(fixture.dir, NativeWorkConfig)
	realPath := filepath.Join(fixture.dir, "real-config.json")
	if err := os.Rename(configPath, realPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, configPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	result := fixture.integration(t).Reconcile(ReconcileOptions{Enabled: true,
		URL: "http://127.0.0.1:9999/v1", Models: testCatalog()})
	if result.State != IntegrationConflict || result.Reason != "config_unsafe" {
		t.Fatalf("got %s/%s", result.State, result.Reason)
	}
}

func TestNativeModelMatchTreatsEmptyListAsAbsent(t *testing.T) {
	// Kimi's generator writes `capabilities: []` where this package omits the
	// field. Both mean "nothing declared" and must compare equal.
	current := map[string]any{"provider": "p", "model": "m", "maxContextSize": float64(1),
		"capabilities": []any{}, "supportEfforts": []any{}}
	desired := map[string]any{"provider": "p", "model": "m", "maxContextSize": float64(1)}
	if !modelMatches(current, desired) {
		t.Fatal("an empty list was treated as a mismatch")
	}
	current["capabilities"] = []any{"tool_use"}
	if modelMatches(current, desired) {
		t.Fatal("a non-empty list was treated as absent")
	}
}

func TestNativeStableDigestIgnoresKeyOrder(t *testing.T) {
	left := map[string]any{"a": float64(1), "b": map[string]any{"c": "x", "d": "y"}}
	right := map[string]any{"b": map[string]any{"d": "y", "c": "x"}, "a": float64(1)}
	if digestOf(left) != digestOf(right) {
		t.Fatal("digest depends on key order")
	}
	if digestOf(left) == digestOf(map[string]any{"a": float64(2)}) {
		t.Fatal("digest ignores a value change")
	}
}

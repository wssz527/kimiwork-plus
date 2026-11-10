package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/core"
)

type stageFailureKeys struct {
	testKeys
	sets       int
	failDelete bool
}

func (k *stageFailureKeys) Set(ref, value string) error {
	k.sets++
	if k.sets == 2 {
		return errors.New("denied")
	}
	return k.testKeys.Set(ref, value)
}
func (k *stageFailureKeys) Delete(ref string) error {
	if k.failDelete {
		return errors.New("denied")
	}
	return k.testKeys.Delete(ref)
}
func TestFailedStageCleanup(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		keys := &stageFailureKeys{testKeys: testKeys{"old": "existing"}, failDelete: failDelete}
		s := Service{Keys: keys}
		d := core.Document{"connections": []any{core.Document{"apiKeyEnv": "A"}, core.Document{"apiKeyEnv": "B"}}}
		value := "synthetic"
		err := s.stageCredentials(d, map[string]*string{"A": &value, "B": &value})
		if err == nil || keys.testKeys["old"] != "existing" {
			t.Fatal(err)
		}
		if !failDelete && len(keys.testKeys) != 1 {
			t.Fatal("orphaned stage")
		}
		if failDelete && err.Error() != "credentials_cleanup_failed" {
			t.Fatal(err)
		}
	}
}

func TestQuotaUIAndTranslationSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	keys := CacheKeys(testKeys{})
	runtime, e := NewRuntime(dir, store, keys)
	if e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, Keys: keys, StateDir: dir, Gateway: runtime, Extra: runtime.Action, OnConfig: runtime.Apply}
	_, rev, _ := store.Read()
	_, e = s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic"}}})
	if e != nil {
		t.Fatal(e)
	}
	input := core.Document{"action": "upsert", "source": map[string]any{"adapter": "deepseek", "sourceId": "deepseek-local", "sourceName": "Local", "auth": map[string]any{"connectionId": "local"}}}
	result, e := runtime.Action(context.Background(), "/api/quota/sources", input)
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "synthetic") || !strings.Contains(string(encoded), "credentialSet") {
		t.Fatal("quota panel response invalid")
	}
	if _, e = runtime.Action(context.Background(), "/api/quota/sources", core.Document{"action": "remove", "sourceId": "deepseek-local"}); e == nil {
		t.Fatal("removed preset")
	}
	result, e = runtime.Action(context.Background(), "/api/quota/refresh", core.Document{"sourceIds": []any{}})
	if e != nil {
		t.Fatal(e)
	}
	if core.Object(result)["lastRefresh"] == nil {
		t.Fatal("refresh report absent")
	}
	_, rev, _ = store.Read()
	settings, _ := core.TranslationSettings(nil)
	_, e = s.saveTranslation(core.Document{"revision": rev, "translationRevision": translationRevision(settings), "enabled": true, "provider": "google", "targetLanguage": "ja", "view": "original"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.saveTranslation(core.Document{"translationRevision": translationRevision(settings), "enabled": false}); e == nil {
		t.Fatal("stale translation overwrite")
	}
	state, e := s.State()
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "apiKeyRef") || strings.Contains(string(raw), "synthetic") {
		t.Fatal("private credential metadata exposed")
	}
	d, _, _ := store.Read()
	saved, _ := core.TranslationSettings(d["translation"])
	if saved.Provider != "google" || saved.TargetLanguage != "ja" || !saved.Enabled {
		t.Fatal(saved)
	}
}
func TestDiscoveryDoesNotSendSavedKeyToAnotherEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer temporary" {
			t.Error("saved key escaped")
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "state")
	store, _ := core.Open(dir)
	keys := CacheKeys(testKeys{})
	s := &Service{Store: store, Keys: keys}
	_, rev, _ := store.Read()
	_, e := s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "saved-secret"}}})
	if e != nil {
		t.Fatal(e)
	}
	in := core.Document{"connectionId": "local", "baseURL": server.URL}
	if _, e = s.discover(context.Background(), in); e == nil || calls != 0 {
		t.Fatal("unsafe discovery", e, calls)
	}
	in["apiKey"] = "temporary"
	result, e := s.discover(context.Background(), in)
	if e != nil || core.Object(result)["state"] != "passed" || calls != 1 {
		t.Fatal(result, e, calls)
	}
}
func TestCredentialStageDoesNotReplaceCurrentOnConflict(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, _ := core.Open(dir)
	keys := CacheKeys(testKeys{})
	s := &Service{Store: store, Keys: keys}
	_, rev, _ := store.Read()
	_, e := s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "old"}}})
	if e != nil {
		t.Fatal(e)
	}
	before, rev, _ := store.Read()
	_, _, e = store.Update(rev, func(d core.Document) error {
		if e := s.stageCredentials(d, map[string]*string{"KIMI_WORK_LOCAL_KEY": optional("new")}); e != nil {
			return e
		}
		external := core.Document{}
		for k, v := range before {
			external[k] = v
		}
		external["externalEdit"] = true
		return core.AtomicJSON(filepath.Join(dir, "gateway.json"), external)
	})
	if !errors.Is(e, core.ErrConflict) {
		t.Fatal(e)
	}
	current, _, _ := store.Read()
	key, e := credentialFor(core.Object(core.Rows(current["connections"])[0]), keys)
	if e != nil || key != "old" {
		t.Fatal("active key changed on conflict", e)
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func TestCustomTranslationCredentialSaveAndReadback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{})
	service := &Service{Store: store, Keys: keys, StateDir: dir}
	_, revision, _ := store.Read()
	current, _ := core.TranslationSettings(nil)
	_, err = service.saveTranslation(core.Document{
		"revision": revision, "translationRevision": translationRevision(current),
		"enabled": false, "provider": "custom", "targetLanguage": "zh", "view": "translated",
		"customEndpoint": "http://127.0.0.1:9080/v1/chat/completions", "customModel": "fixture-model",
		"apiKeyEnv": "FIXTURE_TRANSLATION_KEY", "apiKey": "synthetic-translation",
	})
	if err != nil {
		t.Fatal(err)
	}
	document, revision, _ := store.Read()
	saved, err := core.TranslationSettings(document["translation"])
	if err != nil {
		t.Fatal(err)
	}
	if saved.APIKeyEnv != "FIXTURE_TRANSLATION_KEY" || !strings.HasPrefix(saved.APIKeyRef, "translation-") || service.translationKey(document, saved) != "synthetic-translation" {
		t.Fatalf("saved translation identity is not readable: %+v", saved)
	}
	state, err := service.State()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), saved.APIKeyRef) || strings.Contains(string(encoded), "synthetic-translation") {
		t.Fatal("the credential reference or secret escaped into the public state")
	}
	_, err = service.saveTranslation(core.Document{
		"revision": revision, "translationRevision": translationRevision(saved),
		"enabled": false, "provider": "custom", "targetLanguage": "zh", "view": "translated",
		"customEndpoint": "http://127.0.0.1:9080/v1/chat/completions", "customModel": "fixture-model",
		"apiKeyEnv": "FIXTURE_TRANSLATION_KEY",
	})
	if err != nil || service.translationKey(document, saved) != "synthetic-translation" {
		t.Fatalf("a settings-only save lost the key: %v", err)
	}

	legacy := core.Document{"enabled": false, "provider": "bing", "targetLanguage": "zh", "view": "translated", "apiKeyEnv": saved.APIKeyRef}
	restored, err := core.TranslationSettings(legacy)
	if err != nil || restored.APIKeyRef != saved.APIKeyRef || restored.APIKeyEnv != "KIMI_WORK_TRANSLATION_API_KEY" {
		t.Fatalf("a preview reference could not be read after upgrade: %+v %v", restored, err)
	}
}

func TestTranslationSaveWithoutAPIKeyEnvKeepsManagedRef(t *testing.T) {
	s := newConfigPanelTestService(t, testKeys{})
	_, rev, _ := s.Store.Read()
	draft := core.Document{"revision": rev, "apiKeyEnv": "TEST_TRANSLATOR_KEY", "apiKey": "initial-test-key"}
	callConfigPanel(t, s, http.MethodPost, "/api/translation", draft, http.StatusOK)
	saved, _ := readTranslation(t, s)
	if saved.APIKeyRef == "" {
		t.Fatal("managed key was not stored")
	}
	for i, env := range []any{nil, "", nil} {
		state := callConfigPanel(t, s, http.MethodGet, "/api/state", nil, http.StatusOK)
		configured := assertTranslationKeyState(t, state, "managed")
		configured["revision"] = state["revision"]
		configured["enabled"] = !configured["enabled"].(bool)
		configured["apiKeyMasked"] = "masked"
		delete(configured, "apiKeyEnv")
		if i != 0 {
			configured["apiKeyEnv"] = env
		}
		callConfigPanel(t, s, http.MethodPost, "/api/translation", configured, http.StatusOK)
		current, _ := readTranslation(t, s)
		if current.APIKeyEnv != saved.APIKeyEnv || current.APIKeyRef != saved.APIKeyRef {
			t.Fatal("settings-only save lost the managed credential")
		}
	}
	_, rev = readTranslation(t, s)
	callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{
		"revision": rev, "apiKey": "rotated-test-key",
	}, http.StatusOK)
	current, _ := readTranslation(t, s)
	if key, _ := s.Keys.Get(current.APIKeyRef); current.APIKeyRef == saved.APIKeyRef || key != "rotated-test-key" {
		t.Fatal("rotated credential was not stored under a new ref")
	}
	if old, _ := s.Keys.Get(saved.APIKeyRef); old != "" {
		t.Fatal("unreferenced credential survived rotation")
	}
}

func TestTranslationRejectsInvalidKeyInputs(t *testing.T) {
	s := newConfigPanelTestService(t, testKeys{})
	_, rev, _ := s.Store.Read()
	for _, input := range []core.Document{
		{"apiKeyEnv": 1}, {"apiKeyEnv": []any{}}, {"apiKey": false}, {"apiKey": []any{}},
		{"apiKey": "test\rkey"}, {"apiKey": "test\nkey"}, {"apiKey": "test\x00key"},
	} {
		input["revision"] = rev
		result := callConfigPanel(t, s, http.MethodPost, "/api/translation", input, http.StatusBadRequest)
		if core.Object(result["error"])["code"] != "invalid_translation_config" {
			t.Fatal(result)
		}
		if _, unchanged, err := s.Store.Read(); err != nil || unchanged != rev {
			t.Fatalf("an invalid credential input changed the configuration: %v", err)
		}
	}
}

func TestTranslationClearAPIKey(t *testing.T) {
	const env = "TEST_TRANSLATION_KEY"
	const defaultEnv = "KIMI_WORK_TRANSLATION_API_KEY"
	s := newConfigPanelTestService(t, testKeys{env: "legacy-test-key", defaultEnv: "default-test-key"})
	_, rev, _ := s.Store.Read()
	callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{"revision": rev, "apiKeyEnv": env}, http.StatusOK)
	state := callConfigPanel(t, s, http.MethodGet, "/api/state", nil, http.StatusOK)
	assertTranslationKeyState(t, state, "environment")
	callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{
		"revision": state["revision"], "apiKey": "managed-test-key",
	}, http.StatusOK)
	managed, rev := readTranslation(t, s)
	conflict := callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{
		"revision": rev, "apiKey": "another-test-key", "clearApiKey": true,
	}, http.StatusBadRequest)
	if core.Object(conflict["error"])["code"] != "invalid_translation_config" {
		t.Fatal(conflict)
	}
	if current, currentRev := readTranslation(t, s); currentRev != rev || current.APIKeyRef != managed.APIKeyRef {
		t.Fatal("rejected request changed configuration")
	}
	result := callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{
		"revision": rev, "clearApiKey": true,
	}, http.StatusOK)
	assertTranslationKeyState(t, core.Object(result["state"]), "")
	if cleared, _ := readTranslation(t, s); cleared.APIKeyRef != "" || cleared.APIKeyEnv != defaultEnv || !cleared.APIKeyEnvDisabled {
		t.Fatal("clear did not reset the private reference and environment binding")
	}
	stored, _, err := s.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.TranslationSettings(stored["translation"]); err != nil || core.Object(stored["translation"])[translationKeyEnvDisabled] != nil {
		t.Fatalf("clear must preserve the stored translation settings schema: %v", err)
	}
	for _, ref := range []string{env, defaultEnv, managed.APIKeyRef} {
		if key, _ := s.Keys.Get(ref); key != "" {
			t.Fatal("clear left an unreferenced credential behind")
		}
	}
}

func TestTranslationClearAPIKeyPreservesConnectionCredentials(t *testing.T) {
	const env = "TEST_SHARED_KEY"
	const defaultEnv = "KIMI_WORK_TRANSLATION_API_KEY"
	s := newConfigPanelTestService(t, testKeys{env: "connection-test-key", defaultEnv: "other-connection-test-key"})
	_, rev, _ := s.Store.Read()
	callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{
		"revision": rev, "apiKeyEnv": env, "apiKey": "shared-test-key",
	}, http.StatusOK)
	managed, rev := readTranslation(t, s)
	_, rev, err := s.Store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{"id": "legacy", "name": "Legacy", "baseURL": "https://legacy.example/v1", "apiKeyEnv": env},
			core.Document{"id": "default-key", "name": "Default key", "baseURL": "https://default.example/v1", "apiKeyEnv": defaultEnv},
			core.Document{"id": "managed", "name": "Managed", "baseURL": "https://managed.example/v1", "apiKeyRef": managed.APIKeyRef},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := core.Document{"revision": rev, "clearApiKey": true, "apiKeyEnv": "",
		"provider": "custom", "customEndpoint": "https://new.example/v1/chat/completions"}
	result := callConfigPanel(t, s, http.MethodPost, "/api/translation", draft, http.StatusOK)
	configured := assertTranslationKeyState(t, core.Object(result["state"]), "")
	if configured["apiKeyEnv"] != defaultEnv || configured["apiKeyEnvDisabled"] != nil {
		t.Fatalf("unexpected public cleared settings: %v", configured)
	}
	if key, _ := s.Keys.Get(managed.APIKeyRef); key != "shared-test-key" {
		t.Fatal("clear deleted a credential shared with another connection")
	}
	if key, _ := s.Keys.Get(env); key != "connection-test-key" {
		t.Fatal("clear deleted the legacy connection credential")
	}
	if key, _ := s.Keys.Get(defaultEnv); key != "other-connection-test-key" {
		t.Fatal("clear deleted another connection's default-named credential")
	}
	_, rev = readTranslation(t, s)
	draft["revision"], draft["apiKeyEnv"] = rev, env
	result = callConfigPanel(t, s, http.MethodPost, "/api/translation", draft, http.StatusOK)
	assertTranslationKeyState(t, core.Object(result["state"]), "connection")
}

func TestTranslationCopyConnectionRestoresClearedDefaultBinding(t *testing.T) {
	const env = "KIMI_WORK_TRANSLATION_API_KEY"
	s := newConfigPanelTestService(t, testKeys{env: "test-connection-key"})
	_, rev, _ := s.Store.Read()
	_, _, err := s.Store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{core.Document{"id": "a", "name": "A", "baseURL": "https://a.example/v1", "protocols": []any{"chat"}, "apiKeyEnv": env}}
		d["models"] = []any{core.Document{"id": "kimi-a", "connectionId": "a", "protocol": "chat", "contextWindow": 32000}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	copyAndSave := func() core.Document {
		t.Helper()
		copy := callConfigPanel(t, s, http.MethodPost, "/api/translation/from-connection", core.Document{"modelId": "kimi-a"}, http.StatusOK)
		draft := core.Object(copy["settings"])
		_, latest := readTranslation(t, s)
		draft["revision"] = latest
		out := callConfigPanel(t, s, http.MethodPost, "/api/translation", draft, http.StatusOK)
		return core.Object(out["state"])
	}
	assertTranslationKeyState(t, copyAndSave(), "connection")
	_, rev = readTranslation(t, s)
	out := callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{"revision": rev, "clearApiKey": true}, http.StatusOK)
	assertTranslationKeyState(t, core.Object(out["state"]), "")
	cleared, _ := readTranslation(t, s)
	if cleared.APIKeyEnv != env || !cleared.APIKeyEnvDisabled {
		t.Fatal("clear did not disable the default environment binding")
	}
	assertTranslationKeyState(t, copyAndSave(), "connection")
	if restored, _ := readTranslation(t, s); restored.APIKeyEnvDisabled {
		t.Fatal("copy did not reactivate the environment binding")
	}

	_, rev = readTranslation(t, s)
	callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{"revision": rev, "clearApiKey": true}, http.StatusOK)
	_, rev = readTranslation(t, s)
	out = callConfigPanel(t, s, http.MethodPost, "/api/translation", core.Document{"revision": rev, "apiKey": "test-rotated-key"}, http.StatusOK)
	assertTranslationKeyState(t, core.Object(out["state"]), "managed")
	if rotated, _ := readTranslation(t, s); rotated.APIKeyEnvDisabled {
		t.Fatal("new key did not reset the cleared binding marker")
	}
}

func readTranslation(t *testing.T, s *Service) (core.TranslationConfig, string) {
	t.Helper()
	d, rev, err := s.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	c, err := savedTranslationSettings(d)
	if err != nil {
		t.Fatal(err)
	}
	return c, rev
}

func assertTranslationKeyState(t *testing.T, state core.Document, source string) core.Document {
	t.Helper()
	tr := core.Object(state["translation"])
	configured := core.Object(tr["configured"])
	if configured["apiKeySource"] != source || configured["hasApiKey"] != (source != "") ||
		core.Object(tr["environment"])["keySet"] != configured["hasApiKey"] || configured["apiKeyRef"] != nil {
		t.Fatalf("translation key state does not match source %q: %v", source, tr)
	}
	return configured
}

func TestTranslationPreviewWithoutAPIKeyEnvUsesOnlyTheSavedService(t *testing.T) {
	const env = "TEST_PREVIEW_KEY"
	const defaultEnv = "KIMI_WORK_TRANSLATION_API_KEY"
	const key = "test-preview-credential"
	for _, source := range []string{"connection", "managed"} {
		t.Run(source, func(t *testing.T) {
			savedEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+key {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var input struct {
					Messages []struct{ Content string }
				}
				if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Messages) == 0 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				content := input.Messages[len(input.Messages)-1].Content
				_ = json.NewEncoder(w).Encode(core.Document{"choices": []any{core.Document{"message": core.Document{"content": content}}}})
			}))
			defer savedEndpoint.Close()
			otherEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("changed URL received a stored credential")
				}
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer otherEndpoint.Close()

			s := newConfigPanelTestService(t, testKeys{env: key, defaultEnv: "test-default-credential"})
			_, rev, _ := s.Store.Read()
			draft := core.Document{"provider": "custom", "customModel": "kimi-preview-model",
				"customEndpoint": savedEndpoint.URL + "/v1/chat/completions", "apiKeyEnv": env}
			if source == "connection" {
				_, updated, err := s.Store.Update(rev, func(d core.Document) error {
					d["connections"] = []any{core.Document{"id": "api", "name": "API", "baseURL": savedEndpoint.URL + "/v1", "apiKeyEnv": env, "protocols": []any{"chat"}}}
					d["models"] = []any{core.Document{"id": "kimi-preview-model", "connectionId": "api", "protocol": "chat", "contextWindow": 32000}}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				rev = updated
				copied := callConfigPanel(t, s, http.MethodPost, "/api/translation/from-connection", core.Document{"modelId": "kimi-preview-model"}, http.StatusOK)
				draft = core.Object(copied["settings"])
			} else {
				draft["apiKey"] = key
			}
			draft["revision"] = rev
			saved := callConfigPanel(t, s, http.MethodPost, "/api/translation", draft, http.StatusOK)
			configured := assertTranslationKeyState(t, core.Object(saved["state"]), source)
			delete(configured, "apiKeyEnv")
			for _, body := range []core.Document{{}, {"settings": nil}, {"settings": configured}} {
				callConfigPanel(t, s, http.MethodPost, "/api/translation/preview", body, http.StatusOK)
			}
			configured["apiKeyEnv"] = ""
			callConfigPanel(t, s, http.MethodPost, "/api/translation/preview", core.Document{"settings": configured}, http.StatusOK)
			delete(configured, "apiKeyEnv")
			configured["customEndpoint"] = otherEndpoint.URL + "/v1/chat/completions"
			_, err := s.translationAction(context.Background(), "/api/translation/preview", core.Document{"settings": configured})
			if core.ErrorCode(err) != "translation_http_error" {
				t.Fatalf("unexpected changed-endpoint preview result: %v", err)
			}
		})
	}
}

func TestFixedTranslationPreviewUsesSavedPrivateCredential(t *testing.T) {
	const secret = "synthetic-independent-translation"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var input struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.Messages) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content := input.Messages[len(input.Messages)-1].Content
		var texts []string
		if json.Unmarshal([]byte(content), &texts) == nil {
			for index := range texts {
				texts[index] = "译文"
			}
			encoded, _ := json.Marshal(texts)
			content = string(encoded)
		} else {
			content = "译文"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(core.Document{"choices": []any{core.Document{"message": core.Document{"content": content}}}})
	}))
	defer upstream.Close()

	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Keys: CacheKeys(testKeys{}), StateDir: dir}
	if err := service.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	call := func(path string, body core.Document) (int, core.Document) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		method := http.MethodPost
		if path == "/api/state" {
			method = http.MethodGet
		}
		request, err := http.NewRequest(method, service.Endpoint.URL+path, strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+service.Endpoint.Key)
		request.Header.Set("Origin", service.Endpoint.URL)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var result core.Document
		if json.Unmarshal(raw, &result) != nil {
			t.Fatalf("invalid response: %s", raw)
		}
		return response.StatusCode, result
	}
	_, revision, _ := store.Read()
	current, _ := core.TranslationSettings(nil)
	status, result := call("/api/translation", core.Document{
		"revision": revision, "translationRevision": translationRevision(current),
		"enabled": true, "provider": "custom", "targetLanguage": "zh", "view": "translated",
		"customEndpoint": upstream.URL + "/v1/chat/completions", "customModel": "fixture-model",
		"apiKeyEnv": "FIXTURE_TRANSLATION_KEY", "apiKey": secret,
	})
	if status != http.StatusOK {
		t.Fatalf("save: %d %+v", status, result)
	}
	status, result = call("/api/state", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d %+v", status, result)
	}
	state := result
	public := core.Object(core.Object(state["translation"])["configured"])
	if public == nil || public["customEndpoint"] != upstream.URL+"/v1/chat/completions" {
		t.Fatalf("public translation state was not read back: %+v", public)
	}
	encoded, _ := json.Marshal(public)
	if strings.Contains(string(encoded), "apiKeyRef") || strings.Contains(string(encoded), secret) {
		t.Fatal("the public preview draft contains private credential data")
	}
	status, result = call("/api/translation/preview", core.Document{"settings": public})
	if status != http.StatusOK || core.String(core.Object(result["preview"])["translated"]) == "" {
		t.Fatalf("preview: %d %+v", status, result)
	}
	public["customEndpoint"] = upstream.URL + "/changed/chat/completions"
	saved, _ := readTranslation(t, service)
	for _, env := range []string{core.String(public["apiKeyEnv"]), saved.APIKeyRef} {
		public["apiKeyEnv"] = env
		status, result = call("/api/translation/preview", core.Document{"settings": public})
		if status == http.StatusOK || core.String(core.Object(result["error"])["code"]) != "translation_http_error" {
			t.Fatalf("changed endpoint borrowed the old key: %d %+v", status, result)
		}
	}
}

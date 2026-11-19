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
	status, result = call("/api/translation/preview", core.Document{"settings": public})
	if status == http.StatusOK || core.String(core.Object(result["error"])["code"]) != "translation_http_error" {
		t.Fatalf("changed endpoint borrowed the old key: %d %+v", status, result)
	}
}

package bridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/core"
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

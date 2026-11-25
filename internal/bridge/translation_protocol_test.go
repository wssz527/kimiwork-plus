package bridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// The translation preview can only be offered from a connection that really
// answers Chat. A connection naming Responses must never be reported as
// Chat-capable, and a connection saved before declarations existed keeps the
// model-route fallback it always had.
func TestTranslationFromConnectionFollowsTheDeclaredProtocol(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{
		"KIMI_WORK_CHATONLY_KEY": "synthetic-chat",
		"KIMI_WORK_RESPONLY_KEY": "synthetic-responses",
		"KIMI_WORK_LEGACY_KEY":   "synthetic-legacy",
	})
	service := &Service{Store: store, Keys: keys, StateDir: dir}
	_, revision, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(revision, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{"id": "chatonly", "name": "Chat only", "baseURL": "https://chat.example/v1", "protocols": []any{"chat"}},
			core.Document{"id": "responly", "name": "Responses only", "baseURL": "https://responses.example/v1", "protocols": []any{"responses"}},
			core.Document{"id": "legacy", "name": "Legacy", "baseURL": "https://legacy.example/v1"},
		}
		d["models"] = []any{
			core.Document{"id": "kimi-chat", "protocol": "chat", "contextWindow": float64(32000), "connectionId": "chatonly"},
			core.Document{"id": "vendor/gpt", "protocol": "responses", "contextWindow": float64(32000), "connectionId": "responly"},
			core.Document{"id": "legacy/model", "protocol": "responses", "contextWindow": float64(32000), "connectionId": "legacy"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(modelID string) (core.Document, error) {
		result, err := service.translationAction(context.Background(), "/api/translation/from-connection",
			core.Document{"modelId": modelID})
		if err != nil {
			return nil, err
		}
		return core.Object(result), nil
	}
	result, err := apply("kimi-chat")
	if err != nil {
		t.Fatalf("a Chat connection was refused: %v", err)
	}
	source := core.Object(result["source"])
	if core.String(source["chatSupport"]) != "declared" || core.String(source["baseURL"]) != "https://chat.example/v1" {
		t.Fatalf("source = %+v", source)
	}
	if _, err := apply("vendor/gpt"); err == nil || err.Error() != "translation_connection_responses_only" {
		t.Fatalf("a Responses connection claimed Chat support: %v", err)
	}
	if _, err := apply("legacy/model"); err == nil || err.Error() != "translation_connection_chat_undeclared" {
		t.Fatalf("an undeclared connection did not fall back to the model route: %v", err)
	}
}

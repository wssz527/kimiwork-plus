package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func TestWorkConversationRoutes(t *testing.T) {
	const origin = "http://127.0.0.1:22101"
	const alias = "byok-test/target"
	service := &Service{Endpoint: Endpoint{URL: origin, Key: "test"}}
	service.Extra = func(_ context.Context, path string, in core.Document) (any, error) {
		switch path {
		case "/api/work/conversations":
			if in != nil {
				t.Fatal("query was not read-only")
			}
			return kimi.ConversationList{Application: kimi.Running, KimiRunning: true}, nil
		case "/api/work/conversations/rebind":
			request, err := parseConversationRebind(in)
			if err != nil {
				return nil, err
			}
			if !request.RestartKimi {
				return nil, errors.New("conversation_kimi_restart_required")
			}
			if !slices.Equal(request.ConversationKeys, []string{"one", "two"}) || request.TargetModelAlias != alias || request.ThinkingLevel != "high" {
				t.Fatalf("request = %+v", request)
			}
			return kimi.ConversationRebindResult{ConversationKeys: request.ConversationKeys, KimiRestarted: true}, nil
		default:
			t.Fatalf("unexpected route %s", path)
			return nil, nil
		}
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test")
		if method == http.MethodPost {
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		service.ServeHTTP(w, r)
		return w
	}
	get := do(http.MethodGet, "/api/work/conversations", "")
	var list kimi.ConversationList
	if get.Code != 200 || json.Unmarshal(get.Body.Bytes(), &list) != nil || !list.KimiRunning {
		t.Fatalf("GET %d: %s", get.Code, get.Body)
	}
	for _, tc := range []struct {
		name, request, expected string
		status                  int
	}{
		{"success", `{"conversationKeys":["one","two"],"targetModelAlias":"` + alias + `","thinkingLevel":"high","restartKimi":true}`, `"kimiRestarted":true`, 200},
		{"restart required", `{"conversationKeys":["one"],"targetModelAlias":"` + alias + `"}`, `"code":"conversation_kimi_restart_required"`, 409},
		{"invalid keys", `{"conversationKeys":"one","targetModelAlias":"` + alias + `"}`, `"code":"invalid_conversation_rebind"`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := do(http.MethodPost, "/api/work/conversations/rebind", tc.request)
			if got.Code != tc.status || !strings.Contains(got.Body.String(), tc.expected) {
				t.Fatalf("POST %d: %s", got.Code, got.Body)
			}
		})
	}
}

func TestConversationRebindUsesNativeOperationSlot(t *testing.T) {
	native := &Native{operation: core.Document{"state": "running", "action": "connect-kimi"}}
	in := core.Document{"conversationKeys": []any{"one"}, "targetModelAlias": "byok-test/target"}
	if _, err := native.rebindWorkConversations(context.Background(), in); err == nil || err.Error() != "operation_busy" {
		t.Fatalf("concurrent write was not refused: %v", err)
	}
}

func TestConversationRebindRouteAcceptsMigrationBatch(t *testing.T) {
	const origin = "http://127.0.0.1:22101"
	keys := make([]string, 500)
	for i := range keys {
		keys[i] = strings.Repeat("workspace-long-conversation-key/", 4) + string(rune('a'+i))
	}
	service := &Service{Endpoint: Endpoint{URL: origin, Key: "test"}}
	service.Extra = func(_ context.Context, path string, in core.Document) (any, error) {
		parsed, err := parseConversationRebind(in)
		if err != nil || path != "/api/work/conversations/rebind" || !slices.Equal(parsed.ConversationKeys, keys) {
			t.Fatalf("batch route = %s, read %d keys, error = %v", path, len(parsed.ConversationKeys), err)
		}
		return kimi.ConversationRebindResult{ConversationKeys: parsed.ConversationKeys}, nil
	}
	body, err := json.Marshal(kimi.ConversationRebindRequest{ConversationKeys: keys, TargetModelAlias: "byok-test/target"})
	if err != nil || len(body) <= 32<<10 {
		t.Fatalf("batch body = %d bytes, %v", len(body), err)
	}
	req := httptest.NewRequest(http.MethodPost, origin+"/api/work/conversations/rebind", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	service.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("batch status = %d, %s", w.Code, w.Body)
	}
}

func TestConversationAliasMigrationRejectsMixedSelection(t *testing.T) {
	in := core.Document{"targetModelAlias": "byok-kth-kimi/kimi-example", "migratePreviousAlias": true}
	parsed, err := parseConversationRebind(in)
	if err != nil || !parsed.MigratePreviousAlias || len(parsed.ConversationKeys) != 0 {
		t.Fatalf("migration request = %+v, %v", parsed, err)
	}
	in["conversationKeys"] = []any{"one"}
	if _, err := parseConversationRebind(in); err == nil {
		t.Fatal("accepted a migration request with conversation keys")
	}
}

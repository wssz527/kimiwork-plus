package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

func TestNativeSwarmRequiresAnExplicitBoolean(t *testing.T) {
	for _, value := range []any{nil, "true", 1} {
		native := &Native{}
		if _, err := native.Action(context.Background(), core.Document{"action": "set-swarm", "enabled": value}); err == nil || err.Error() != "invalid_swarm_mode" {
			t.Fatal("implicit toggle was accepted", err)
		}
		if native.operation != nil {
			t.Fatal("invalid input started an operation")
		}
	}
}

func TestNativeSwarmRequiresTheDisplayedConversation(t *testing.T) {
	for _, key := range []any{nil, 123, "", " conv-1 "} {
		native := &Native{operation: core.Document{"state": "running"}}
		_, err := native.Action(context.Background(), core.Document{"action": "set-swarm", "enabled": true, "conversationKey": key})
		if err == nil || err.Error() != "invalid_swarm_conversation" {
			t.Fatalf("invalid conversation accepted: %v", err)
		}
	}
}

func TestSwarmReadRouteUsesTheReadOnlyNativeSurface(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	service := &Service{Store: store, StateDir: dir, Extra: func(_ context.Context, path string, in core.Document) (any, error) {
		if path != "/api/work/swarm" || in != nil {
			t.Error("read route was not passed as read-only")
		}
		reads++
		return core.Document{"swarm": kimi.SwarmSnapshot{State: kimi.SwarmStateUnknown, Reason: kimi.SwarmErrConversationUnavailable}}, nil
	}}
	if err := service.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	request, _ := http.NewRequest("GET", service.Endpoint.URL+"/api/work/swarm", nil)
	request.Header.Set("Authorization", "Bearer "+service.Endpoint.Key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Swarm kimi.SwarmSnapshot `json:"swarm"`
	}
	if json.NewDecoder(response.Body).Decode(&body) != nil || response.StatusCode != 200 || reads != 1 || body.Swarm.State != kimi.SwarmStateUnknown || body.Swarm.Enabled || body.Swarm.Confirmed {
		t.Fatal("missing conversation was presented as an enabled mode")
	}
}

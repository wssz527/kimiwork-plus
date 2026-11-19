package bridge

import (
	"net/http"
	"testing"

	"github.com/wssz527/kimi-work-plus/internal/core"
)

func TestBudgetHTTPUsesConfigurationTransaction(t *testing.T) {
	fixture := newBudgetFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	status, result := fixture.call(t, "GET", "/api/budget", nil, fixture.service.Endpoint.Key, true)
	if status != 200 {
		t.Fatal(status, result)
	}
	budget := core.Object(result["budget"])
	revision := core.String(budget["revision"])
	settings := core.Document{"enabled": true, "currency": "CNY", "timeZone": "Asia/Shanghai", "daily": core.Document{"limit": 5, "action": "block"}}
	status, result = fixture.call(t, "POST", "/api/budget/save", core.Document{"revision": revision, "budget": settings}, fixture.service.Endpoint.Key, true)
	if status != 200 || core.Object(core.Object(result["state"])["budget"])["enabled"] != true {
		t.Fatal(status, result)
	}
	stored, nextRevision, err := fixture.store.Read()
	if err != nil || stored["budget"] == nil || nextRevision == revision {
		t.Fatal("budget was not committed", err)
	}
	status, result = fixture.call(t, "POST", "/api/budget/save", core.Document{"revision": revision, "budget": nil}, fixture.service.Endpoint.Key, true)
	if status != 409 {
		t.Fatal("stale budget write was not rejected", status, result)
	}
	status, result = fixture.call(t, "POST", "/api/budget/save", core.Document{"budget": nil}, fixture.service.Endpoint.Key, true)
	if status != 409 {
		t.Fatal("budget write without revision was accepted", status, result)
	}
	status, result = fixture.call(t, "POST", "/api/budget/save", core.Document{"revision": nextRevision, "budget": core.Document{"enabled": true, "currency": "wrong"}}, fixture.service.Endpoint.Key, true)
	if status != 400 {
		t.Fatal("invalid budget accepted", status, result)
	}
	_, unchanged, _ := fixture.store.Read()
	if unchanged != nextRevision {
		t.Fatal("failed save changed configuration")
	}
	status, result = fixture.call(t, "POST", "/api/budget/save", core.Document{"revision": nextRevision, "budget": nil}, fixture.service.Endpoint.Key, true)
	if status != 200 || core.Object(core.Object(result["state"])["budget"])["enabled"] != false {
		t.Fatal("budget could not be removed", status, result)
	}
}

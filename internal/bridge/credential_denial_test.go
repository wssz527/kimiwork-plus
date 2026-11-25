package bridge

import (
	"errors"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"testing"
)

type deniedKeys struct{ testKeys }

func (deniedKeys) Get(string) (string, error) { return "", errors.New("credential_access_denied") }

func TestCredentialDenialKeepsPanelAvailable(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Keys: CacheKeys(testKeys{})}
	_, rev, _ := store.Read()
	_, err = s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "fixture", "name": "Fixture", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic", "protocols": []any{"chat"}}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(dir, store, deniedKeys{})
	if err != nil {
		t.Fatal(err)
	}
	state := r.State()
	connections := core.Object(state["connections"])
	if core.Object(connections["apply"])["reason"] != "credentials_unavailable" {
		t.Fatal(state)
	}
	if core.Object(core.Rows(connections["active"])[0])["apiKeySet"] != false {
		t.Fatal(state)
	}
}

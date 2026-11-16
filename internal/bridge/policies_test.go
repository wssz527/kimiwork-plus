package bridge

import (
	"context"
	"encoding/json"
	"kimiworkplus.local/migration/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newIsolatedTestPolicies(dir string) (*nativePolicies, error) {
	home := filepath.Join(dir, "kimi-profile", "daimon-share", "daimon")
	return newNativePolicies(dir, home)
}

func TestNativeSettingsStayIsolated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	p, e := newIsolatedTestPolicies(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(p.subagents.HomeDir(), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p.subagents.ConfigPath(), []byte("# fixture\n"), 0600); e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, StateDir: dir, Keys: CacheKeys(testKeys{})}
	n := &Native{service: s, policies: p}
	if e = n.initEnhancements(); e != nil {
		t.Fatal(e)
	}
	defer n.web.Close()
	d, _, _ := store.Read()
	if e = p.Configure(d); e != nil {
		t.Fatal(e)
	}
	result, e := s.saveNativeContext(p, core.Document{"policy": core.Document{"mode": "custom", "triggerRatio": 0.85, "reservedTokens": float64(0)}})
	if e != nil {
		t.Fatal(e)
	}
	if core.Object(core.Object(result)["context"])["status"] != "file-applied" {
		t.Fatal(result)
	}
	_, e = s.saveNativeContext(p, core.Document{"services": core.Document{"search": core.Document{"enabled": true, "baseURL": "https://example.test/search", "apiKey": "synthetic-web"}}})
	if e != nil {
		t.Fatal(e)
	}
	d, _, _ = store.Read()
	raw, _ := json.Marshal(p.State(d))
	if strings.Contains(string(raw), "synthetic-web") {
		t.Fatal("key leaked")
	}
	result, e = n.toolsAction(context.Background(), core.Document{"action": "add", "server": core.Document{"name": "fixture", "transport": "stdio", "command": "/usr/bin/false"}})
	if e != nil {
		t.Fatal(e)
	}
	if core.Object(core.Object(result)["tools"])["available"] != true {
		t.Fatal(result)
	}
	if _, e = n.toolsAction(context.Background(), core.Document{"action": "test", "name": "fixture"}); e == nil {
		t.Fatal("claimed runtime probe")
	}
	result, e = n.webAction(core.Document{"action": "save", "provider": "official"})
	if e != nil {
		t.Fatal(e)
	}
	if result == nil {
		t.Fatal("missing web state")
	}
}

func TestConnectionWithoutKeyAndSubagentPolicy(t *testing.T) {
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
	s := &Service{Store: store, Keys: keys, OnConfig: runtime.Apply, Gateway: runtime}
	_, rev, _ := store.Read()
	if _, e = s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "empty", "name": "Empty", "baseURL": "http://127.0.0.1:9999/v1"}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = NewRuntime(dir, store, keys); e != nil {
		t.Fatal(e)
	}
	_, rev, _ = store.Read()
	_, e = s.configure("/api/models", core.Document{"revision": rev, "models": []any{core.Document{"id": "kimi-alpha", "connectionId": "empty", "protocol": "chat", "contextWindow": float64(32000), "tools": true}}})
	if e != nil {
		t.Fatal(e)
	}
	_, rev, _ = store.Read()
	if _, e = s.configure("/api/subagents", core.Document{"revision": rev, "mode": "force", "model": "kimi-alpha"}); e != nil {
		t.Fatal(e)
	}
	d, _, _ := store.Read()
	if core.Object(d["subagents"])["modelAlias"] != "byok-kth-kimi/kimi-alpha" {
		t.Fatal(d)
	}
}

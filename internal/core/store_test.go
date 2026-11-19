package core

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRevisionMergeAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, rev, _ := s.Read()
	d, rev2, e := s.Update(rev, func(d Document) error {
		d["connections"] = []any{map[string]any{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1"}}
		d["models"] = []any{map[string]any{"id": "alpha", "protocol": "responses", "contextWindow": float64(32000), "connectionId": "local", "tools": false, "maxOutputTokens": float64(4096), "futureField": "keep"}}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = s.Update(rev, func(d Document) error { return nil })
	if !errors.Is(e, ErrConflict) {
		t.Fatal("stale revision accepted", e)
	}
	_, rev3, e := s.Update(rev2, func(d Document) error {
		return MergeModels(d, []any{map[string]any{"id": "alpha", "name": "renamed", "injected": "bad"}})
	})
	if e != nil {
		t.Fatal(e)
	}
	d, _, _ = s.Read()
	m := Object(Rows(d["models"])[0])
	if m["futureField"] != "keep" || m["maxOutputTokens"] != float64(4096) || m["tools"] != false || m["injected"] != nil {
		t.Fatal(m)
	}
	_, _, e = s.Update(rev3, func(d Document) error {
		return MergeModels(d, []any{map[string]any{"id": "alpha", "tools": nil, "maxOutputTokens": nil}})
	})
	if e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	d, _, _ = s.Read()
	m = Object(Rows(d["models"])[0])
	if _, ok := m["tools"]; ok {
		t.Fatal(m)
	}
	if _, ok := m["maxOutputTokens"]; ok {
		t.Fatal(m)
	}
	st, err := os.Stat(filepath.Join(dir, "gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !st.Mode().IsRegular() || st.Mode().Perm()&0o600 != 0o600 {
			t.Fatal(st.Mode())
		}
	} else if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestStorePreservesChineseModelNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, rev, _ := s.Read()
	wantNames := []string{"GLM-5.3 Flash · 路由", "深度求索 V4.1 · 代理"}
	_, _, e = s.Update(rev, func(d Document) error {
		d["connections"] = []any{map[string]any{"id": "magpie", "name": "喜鹊网关", "baseURL": "http://127.0.0.1:9999/v1"}}
		d["models"] = []any{
			map[string]any{"id": "magpie/kimi-group%2Fglm-5-3-flash", "name": wantNames[0], "protocol": "chat", "contextWindow": float64(358400), "connectionId": "magpie", "upstreamModelId": "group/glm-5-3-flash"},
			map[string]any{"id": "magpie/kimi-group%2Fdeepseek-v4-1-flash", "name": wantNames[1], "protocol": "chat", "contextWindow": float64(300000), "connectionId": "magpie", "upstreamModelId": "group/deepseek-v4-1-flash"},
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	doc, _, _ := reopened.Read()
	rows := Rows(doc["models"])
	if len(rows) != 2 {
		t.Fatalf("models count = %d", len(rows))
	}
	for i, want := range wantNames {
		if got := String(Object(rows[i])["name"]); got != want {
			t.Fatalf("model[%d].name = %q, want %q", i, got, want)
		}
	}
}
func TestCrashRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, _ := Open(dir)
	d, rev, _ := s.Read()
	d["future"] = "preserved"
	if e := AtomicJSON(s.path+".journal", journal{Before: rev, After: d}); e != nil {
		t.Fatal(e)
	}
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	actual, _, _ := s.Read()
	if actual["future"] != "preserved" {
		t.Fatal(actual)
	}
}
func TestConnectionReference(t *testing.T) {
	d := Document{"models": []any{map[string]any{"id": "a", "connectionId": "local"}}}
	if _, e := MergeConnections(d, []any{}); e == nil {
		t.Fatal("removed referenced connection")
	}
}

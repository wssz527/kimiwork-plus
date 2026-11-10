package core

import (
	"errors"
	"os"
	"path/filepath"
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
	st, _ := os.Stat(filepath.Join(dir, "gateway.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
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

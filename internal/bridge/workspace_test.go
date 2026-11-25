package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func TestWorkspaceSavesConnectionsAndModelsAtomically(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, StateDir: dir}
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	call := func(body core.Document) (int, core.Document) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", s.Endpoint.URL+"/api/workspace", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+s.Endpoint.Key)
		req.Header.Set("Origin", s.Endpoint.URL)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := core.Document{}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	_, revision, _ := store.Read()
	body := core.Document{"revision": revision, "connections": []any{core.Document{"id": "first", "name": "First", "baseURL": "http://127.0.0.1:9999/v1", "protocols": []any{"responses"}}}, "models": []any{core.Document{"id": "first/model", "connectionId": "first", "protocol": "responses", "contextWindow": 32000}}}
	if status, result := call(body); status != 200 {
		t.Fatal(status, result)
	}
	before, revision, _ := store.Read()
	if len(core.Rows(before["models"])) != 1 || len(core.Rows(before["connections"])) != 1 {
		t.Fatal(before)
	}
	body["revision"] = revision
	first := core.Object(core.Rows(body["connections"])[0])
	first["protocols"] = []any{"chat"}
	if status, result := call(body); status != http.StatusBadRequest || core.Object(result["error"])["code"] != "invalid_connection_protocol_in_use" {
		t.Fatal("protocol change would break the existing Responses route", status, result)
	}
	if after, current, err := store.Read(); err != nil || current != revision || core.Revision(before) != core.Revision(after) {
		t.Fatal("rejected protocol change modified the workspace", current, err)
	}
	body["connections"] = []any{core.Document{"id": "second", "name": "Second", "baseURL": "http://127.0.0.1:9998/v1", "protocols": []any{"responses"}}}
	body["models"] = []any{core.Document{"id": "second/model", "connectionId": "missing", "protocol": "responses", "contextWindow": 32000}}
	if status, result := call(body); status < 400 {
		t.Fatal("invalid model accepted", status, result)
	}
	after, unchanged, _ := store.Read()
	if unchanged != revision || core.Revision(before) != core.Revision(after) {
		t.Fatal("partial workspace save", before, after)
	}
	body["models"] = []any{core.Document{"id": "second/model", "connectionId": "second", "protocol": "responses", "contextWindow": 32000}}
	if status, result := call(body); status != 200 {
		t.Fatal("atomic connection replacement failed", status, result)
	}
	if status, _ := call(body); status != 409 {
		t.Fatal("stale workspace save accepted", status)
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/core"
)

func TestNativeQuotaRoutesPersistChoiceAndRefreshMagpie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/magpie/quotas" || r.Header.Get("Authorization") != "Bearer magpie-private-key" {
			t.Error("quota request did not use configured connection")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"provider":"demo","name":"Demo","kind":"balance","balance":"$12.50"}]`))
	}))
	defer upstream.Close()
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, _ := store.Read()
	_, _, err = store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{core.Document{"id": "magpie", "name": "Magpie", "baseURL": upstream.URL + "/v1", "apiKeyRef": "magpie-secret"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := &countedKeys{values: map[string]string{"magpie-secret": "magpie-private-key"}}
	s := &Service{Store: store, StateDir: dir, Keys: keys}
	q := newNativeQuota(s, func() string { return "work-private-key" }, func() string { return "local-private-key" })
	s.Extra = q.Action
	s.NativeState = q.State
	if err := s.Start("0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	call := func(method, path string, body core.Document) core.Document {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, s.Endpoint.URL+path, strings.NewReader(string(raw)))
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
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode, out)
		}
		encoded, _ := json.Marshal(out)
		if strings.Contains(string(encoded), "private-key") {
			t.Fatal("credential exposed by quota route")
		}
		return out
	}
	code := core.Object(call("GET", "/api/quota/code", nil)["state"])
	for _, raw := range core.Rows(code["sources"]) {
		row := core.Object(raw)
		if row["id"] == "local" && (row["availability"] != "unadopted" || row["adopted"] != false) {
			t.Fatal("local sign-in was adopted without explicit selection")
		}
	}
	call("POST", "/api/quota/code", core.Document{"action": "select", "source": "local"})
	call("POST", "/api/quota/magpie", core.Document{"action": "address", "connectionId": "magpie"})
	magpie := core.Object(call("POST", "/api/quota/magpie", core.Document{"action": "refresh"})["state"])
	if magpie["status"] != "ok" || magpie["connectionId"] != "magpie" || len(core.Rows(core.Object(magpie["snapshot"])["entries"])) != 1 {
		t.Fatal(magpie)
	}
	restored := newNativeQuota(s, func() string { return "work-private-key" }, func() string { return "local-private-key" })
	if restored.code.State().SelectedSource == nil || *restored.code.State().SelectedSource != "local" || restored.settings.ConnectionID != "magpie" {
		t.Fatal("quota selection did not survive restart")
	}
	raw, err := os.ReadFile(q.settingsPath())
	if err != nil || strings.Contains(string(raw), "private-key") {
		t.Fatal("credential persisted outside keychain", err)
	}
}

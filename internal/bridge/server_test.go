package bridge

import (
	"context"
	"encoding/json"
	"io"
	"github.com/wssz527/kimi-work-plus/internal/core"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPConfigurationAndIsolation(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, e := core.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	s := &Service{Store: store, StateDir: dir}
	if e = s.Start("0"); e != nil {
		t.Fatal(e)
	}
	defer s.Close(context.Background())
	call := func(method, path, body, token, origin string) (int, core.Document) {
		t.Helper()
		r, _ := http.NewRequest(method, s.Endpoint.URL+path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", "application/json")
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		d := core.Document{}
		_ = json.Unmarshal(b, &d)
		return res.StatusCode, d
	}
	if code, _ := call("GET", "/api/state", "", "", ""); code != 401 {
		t.Fatal(code)
	}
	if code, _ := call("GET", "/api/state", "", s.Endpoint.Key, "https://evil.invalid"); code != 403 {
		t.Fatal(code)
	}
	code, state := call("GET", "/api/state", "", s.Endpoint.Key, "")
	if code != 200 {
		t.Fatal(code, state)
	}
	rev := core.String(state["revision"])
	body := `{"revision":"` + rev + `","connections":[{"id":"local","name":"Local","baseURL":"http://127.0.0.1:9999/v1"}]}`
	if code, _ = call("POST", "/api/connections", body, s.Endpoint.Key, ""); code != 403 {
		t.Fatal(code)
	}
	code, state = call("POST", "/api/connections", body, s.Endpoint.Key, s.Endpoint.URL)
	if code != 200 {
		t.Fatal(code, state)
	}
	if code, _ = call("POST", "/api/connections", body, s.Endpoint.Key, s.Endpoint.URL); code != 409 {
		t.Fatal("stale save", code)
	}
	rev = core.String(core.Object(state["state"])["revision"])
	body = `{"revision":"` + rev + `","models":[{"id":"alpha","protocol":"responses","connectionId":"local","contextWindow":32000,"reasoning":true,"tools":false}]}`
	code, state = call("POST", "/api/models", body, s.Endpoint.Key, s.Endpoint.URL)
	if code != 200 {
		t.Fatal(code, state)
	}
	stored, _, _ := store.Read()
	m := core.Object(core.Rows(stored["models"])[0])
	if m["reasoning"] != true {
		t.Fatal(m)
	}
	if !Healthy(s.Endpoint) {
		t.Fatal("health identity failed")
	}
	if code, _ := call("POST", "/api/actions", `{"action":"launch-kimi"}`, s.Endpoint.Key, s.Endpoint.URL); code != 503 {
		t.Fatal("unimplemented native operation was accepted", code)
	}
}

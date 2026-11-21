package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// directHTTPFixture is a real HTTP service over a temporary store, which is what
// makes the assertions about status codes meaningful.
type directHTTPFixture struct {
	service *Service
	store   *core.Store
}

func newDirectHTTPFixture(t *testing.T) *directHTTPFixture {
	t.Helper()
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, StateDir: dir}
	if err := service.Start("0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return &directHTTPFixture{service: service, store: store}
}

func (f *directHTTPFixture) call(t *testing.T, method, path string, body core.Document) (int, core.Document) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequest(method, f.service.Endpoint.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.service.Endpoint.Key)
	request.Header.Set("Content-Type", "application/json")
	if method == "POST" {
		request.Header.Set("Origin", f.service.Endpoint.URL)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	decoded := core.Document{}
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded
}

func (f *directHTTPFixture) revision(t *testing.T) string {
	t.Helper()
	_, revision, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestIntegrationModeHTTPCommit(t *testing.T) {
	fixture := newDirectHTTPFixture(t)
	status, state := fixture.call(t, "GET", "/api/state", nil)
	if status != 200 {
		t.Fatal(status, state)
	}
	if state["integrationMode"] != core.IntegrationModeGateway {
		t.Fatalf("a fresh document did not report the gateway: %+v", state["integrationMode"])
	}
	if direct, ok := state["directMode"].(map[string]any); !ok || direct["mode"] != core.IntegrationModeGateway {
		t.Fatalf("directMode = %#v", state["directMode"])
	}

	status, state = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": fixture.revision(t), "mode": core.IntegrationModeDirect})
	if status != 200 || state["ok"] != true {
		t.Fatal(status, state)
	}
	if core.Object(core.Object(state["state"])["directMode"])["mode"] != core.IntegrationModeDirect {
		t.Fatalf("state = %+v", state["state"])
	}
	stored, _, err := fixture.store.Read()
	if err != nil || stored["integrationMode"] != core.IntegrationModeDirect {
		t.Fatalf("stored = %+v err = %v", stored["integrationMode"], err)
	}

	// The mode round-trips back, so a user can leave Direct Mode again.
	status, _ = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": fixture.revision(t), "mode": core.IntegrationModeGateway})
	if status != 200 {
		t.Fatal(status)
	}
	stored, _, _ = fixture.store.Read()
	if stored["integrationMode"] != core.IntegrationModeGateway {
		t.Fatalf("stored = %+v", stored["integrationMode"])
	}
}

func TestIntegrationModeHTTPRejectsBadInput(t *testing.T) {
	fixture := newDirectHTTPFixture(t)
	before := fixture.revision(t)
	status, result := fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": before, "mode": "profiles"})
	if status != 400 || core.Object(result["error"])["code"] != "invalid_integration_mode" {
		t.Fatal(status, result)
	}
	status, result = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": before})
	if status != 400 || core.Object(result["error"])["code"] != "invalid_integration_mode" {
		t.Fatal(status, result)
	}
	if fixture.revision(t) != before {
		t.Fatal("a rejected mode write changed the configuration")
	}

	status, result = fixture.call(t, "POST", "/api/integration-mode", core.Document{"mode": core.IntegrationModeDirect})
	if status != 409 || core.Object(result["error"])["code"] != "configuration_conflict" {
		t.Fatal(status, result)
	}
	status, result = fixture.call(t, "POST", "/api/integration-mode", core.Document{"revision": "stale", "mode": core.IntegrationModeDirect})
	if status != 409 || core.Object(result["error"])["code"] != "configuration_conflict" {
		t.Fatal(status, result)
	}
	if fixture.revision(t) != before {
		t.Fatal("a conflicting mode write changed the configuration")
	}
}

func TestDirectErrorCodesMapToStatusAndMessages(t *testing.T) {
	for code, want := range map[string]int{
		"invalid_integration_mode":   400,
		"direct_no_eligible_models":  400,
		"direct_missing_api_key":     400,
		"direct_duplicate_endpoint":  409,
		"direct_mode_unsupported":    409,
		"direct_foreign_source":      409,
		"direct_preview_refused":     503,
		"direct_catalog_too_large":   413,
		"direct_import_failed":       502,
		"native_import_direct_mode":  409,
		"native_import_gateway_mode": 409,
	} {
		recorder := &statusRecorder{header: http.Header{}}
		fail(recorder, errorCode(code))
		if recorder.status != want {
			t.Fatalf("%s answered %d, want %d", code, recorder.status, want)
		}
		if message(code) == "" || strings.Contains(message(code), "预览") {
			t.Fatalf("%s message = %q", code, message(code))
		}
	}
}

// errorCode is a typed error so core.ErrorCode keeps the exact string instead of
// collapsing it to operation_failed.
type errorCode string

func (e errorCode) Error() string { return string(e) }

type statusRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (r *statusRecorder) Header() http.Header { return r.header }
func (r *statusRecorder) Write(body []byte) (int, error) {
	r.body = append(r.body, body...)
	return len(body), nil
}
func (r *statusRecorder) WriteHeader(status int) { r.status = status }

func TestCredentialMessagesNoLongerSayPreview(t *testing.T) {
	for _, code := range []string{
		"credential_keychain_unavailable",
		"credential_session_unavailable",
		"credential_authorization_required",
		"credential_store_corrupt",
		"credential_interaction_not_restored",
	} {
		text := message(code)
		if strings.Contains(text, "预览") {
			t.Fatalf("%s still mentions the preview build: %q", code, text)
		}
	}
	// The update channel's own wording names the preview channel, not the build.
	if !strings.Contains(message("invalid_update_settings"), "预览版") {
		t.Fatalf("update channel message = %q", message("invalid_update_settings"))
	}
}

package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// A connection save the panel must change is a client error, not a broken
// server. Each refusal names the reason so the user can resolve it.
func TestConnectionProtocolErrorsAreClientErrors(t *testing.T) {
	generic := message("operation_failed")
	for _, code := range []string{"connection_protocol_required", "invalid_connection_protocols", "invalid_connection_protocol_in_use"} {
		recorder := httptest.NewRecorder()
		fail(recorder, errors.New(code))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", code, recorder.Code)
		}
		var body core.Document
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		payload := core.Object(body["error"])
		if core.String(payload["code"]) != code {
			t.Fatalf("%s: %#v", code, payload)
		}
		if text := core.String(payload["message"]); text == "" || text == generic {
			t.Fatalf("%s: message = %q", code, text)
		}
	}
}

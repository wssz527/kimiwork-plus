package protocol_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// A connection names the one wire its supplier speaks. Every request served
// from it follows that declaration, whichever Kimi route the model id maps to;
// a connection saved before declarations existed keeps the per-model rule.

func TestChatRequestOnAResponsesConnectionIsBridged(t *testing.T) {
	upstream := newUpstream(t, responsesJSONReply(`{"id":"resp_1","object":"response","model":"kimi-vendor",
		"status":"completed","output":[{"type":"message","role":"assistant",
		"content":[{"type":"output_text","text":"bridged"}]}]}`))
	connection := "vendor"
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: connection, BaseURL: upstream.server.URL, APIKey: "vendor-key", Protocols: []string{protocol.ProtocolResponses}},
	}, []protocol.Model{chatModel("kimi-vendor", func(m *protocol.Model) { m.ConnectionID = &connection })}, nil)

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    "kimi-vendor",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if reply.StatusCode != http.StatusOK || !strings.Contains(body, "bridged") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	captured := upstream.captured()
	if len(captured) != 1 || captured[0].URL != "/responses" {
		t.Fatalf("upstream captured = %+v", captured)
	}
	// The bridge converts the Chat body, so the vendor sees `input`, not messages.
	if strings.Contains(captured[0].Body, `"messages"`) {
		t.Fatalf("the Chat body reached a Responses upstream: %s", captured[0].Body)
	}
}

func TestChatRequestOnAChatConnectionStaysChatForAResponsesRow(t *testing.T) {
	upstream := newUpstream(t, jsonReply("chat-answered"))
	connection := "vendor"
	models := []protocol.Model{responsesRow("vendor/gpt", func(m *protocol.Model) { m.ConnectionID = &connection })}
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: connection, BaseURL: upstream.server.URL, APIKey: "vendor-key", Protocols: []string{protocol.ProtocolChat}},
	}, models, func(c *protocol.Config) { c.KimiK3Compatibility = true })

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    protocol.KimiCompatibleModelID("vendor/gpt"),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if reply.StatusCode != http.StatusOK || !strings.Contains(body, "chat-answered") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	captured := upstream.captured()
	if len(captured) != 1 || captured[0].URL != "/chat/completions" {
		t.Fatalf("a Chat connection was asked for /responses: %+v", captured)
	}
	if !strings.Contains(captured[0].Body, `"messages"`) {
		t.Fatalf("the Chat body was rewritten: %s", captured[0].Body)
	}
}

func TestResponsesRouteOnAChatConnectionIsRefused(t *testing.T) {
	upstream := newUpstream(t, jsonReply("never"))
	connection := "vendor"
	models := []protocol.Model{responsesRow("vendor/gpt", func(m *protocol.Model) { m.ConnectionID = &connection })}
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: connection, BaseURL: upstream.server.URL, APIKey: "vendor-key", Protocols: []string{protocol.ProtocolChat}},
	}, models, nil)

	reply, body := post(t, url, "/responses", map[string]any{"model": "vendor/gpt", "input": "hello"}, nil)
	if reply.StatusCode != http.StatusBadRequest || errorCode(t, body) != "invalid_protocol" {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if captured := upstream.captured(); len(captured) != 0 {
		t.Fatalf("a refused request reached the vendor: %+v", captured)
	}
}

func TestLegacyConnectionKeepsTheResponsesCompatibilityBridge(t *testing.T) {
	upstream := newUpstream(t, responsesJSONReply(`{"id":"resp_1","object":"response","model":"vendor/gpt",
		"status":"completed","output":[{"type":"message","role":"assistant",
		"content":[{"type":"output_text","text":"legacy-bridged"}]}]}`))
	connection := "vendor"
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: connection, BaseURL: upstream.server.URL, APIKey: "vendor-key"},
	}, []protocol.Model{responsesRow("vendor/gpt", func(m *protocol.Model) { m.ConnectionID = &connection })},
		func(c *protocol.Config) { c.KimiK3Compatibility = true })

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    protocol.KimiCompatibleModelID("vendor/gpt"),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if reply.StatusCode != http.StatusOK || !strings.Contains(body, "legacy-bridged") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if captured := upstream.captured(); len(captured) != 1 || captured[0].URL != "/responses" {
		t.Fatalf("the legacy bridge was lost: %+v", captured)
	}
}

// A standby's `upstreamProtocol` describes the row it stands in for; the
// connection it lands on still names the only wire that supplier speaks.
func TestADeclaredProtocolWinsOverAFailoverWireOverride(t *testing.T) {
	models, err := protocol.ValidateModels([]json.RawMessage{[]byte(
		`{"id":"kimi-main","protocol":"chat","contextWindow":8192,"connectionId":"primary",` +
			`"failover":[{"connectionId":"standby","upstreamProtocol":"chat"}]}`)})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	standby := newUpstream(t, responsesJSONReply(`{"id":"resp_2","object":"response","model":"kimi-main",
		"status":"completed","output":[{"type":"message","role":"assistant",
		"content":[{"type":"output_text","text":"from-standby"}]}]}`))
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: "primary", BaseURL: primary.server.URL, APIKey: "synthetic-key"},
		{ID: "standby", BaseURL: standby.server.URL, APIKey: "vendor-key", Protocols: []string{protocol.ProtocolResponses}},
	}, models, nil)

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    "kimi-main",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if reply.StatusCode != http.StatusOK || !strings.Contains(body, "from-standby") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if captured := standby.captured(); len(captured) != 1 || captured[0].URL != "/responses" {
		t.Fatalf("standby captured = %+v", captured)
	}
}

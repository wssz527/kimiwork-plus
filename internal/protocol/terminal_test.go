package protocol_test

import (
	"net/http"
	"testing"

	"kimiworkplus.local/migration/internal/protocol"
)

func TestMissingOrIncompleteTerminalIsNotSuccess(t *testing.T) {
	cases := []struct {
		name, contentType, route, body string
		stream                         bool
	}{
		{"chat_stream_eof", "text/event-stream", "/chat/completions", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n", true},
		{"responses_json_incomplete", "application/json", "/responses", `{"id":"r_test","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write([]byte(test.body))
			})
			model := chatModel("review-model")
			payload := map[string]any{"model": "review-model", "messages": []any{}, "stream": test.stream}
			if test.route == "/responses" {
				model = responsesModel("review-model")
				payload = map[string]any{"model": "review-model", "input": "hi", "stream": false}
			}
			gateway, url := startGateway(t, fake.server.URL, []protocol.Model{model}, nil)
			response, _ := post(t, url, test.route, payload, nil)
			recent := gateway.Metrics().Recent
			if len(recent) != 1 {
				t.Fatalf("expected one request, got %d", len(recent))
			}
			record := recent[0]
			if response.StatusCode != http.StatusOK || record.TransportStatus == nil || *record.TransportStatus != http.StatusOK {
				t.Fatalf("transport %+v", record)
			}
			if record.LedgerStatus() != "incomplete" {
				t.Fatalf("ledger status %q, outcome %q", record.LedgerStatus(), record.Outcome)
			}
			if test.route == "/responses" && (record.Usage == nil || *record.Usage.TotalTokens != 10) {
				t.Fatalf("usage %+v", record.Usage)
			}
		})
	}
}

func TestNonStreamingCompletionHasExplicitOutcome(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r_test","object":"response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}`))
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{responsesModel("review-model")}, nil)
	response, _ := post(t, url, "/responses", map[string]any{"model": "review-model", "input": "hi", "stream": false}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	record := gateway.Metrics().Recent[0]
	if record.LedgerStatus() != "completed" || record.Outcome != "completed" {
		t.Fatalf("record %+v", record)
	}
}

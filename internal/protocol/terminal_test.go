package protocol_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
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

func TestUpstreamStreamInterruptionIsRecordedAsFailed(t *testing.T) {
	for _, route := range []string{"/chat/completions", "/responses"} {
		t.Run(route, func(t *testing.T) {
			fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			})
			model := chatModel("review-model")
			payload := map[string]any{"model": "review-model", "messages": []any{}, "stream": true}
			if route == "/responses" {
				model = responsesModel("review-model")
				payload = map[string]any{"model": "review-model", "input": "hi", "stream": true}
			}
			entries := make(chan protocol.UsageEntry, 2)
			gateway, url := startGateway(t, fake.server.URL, []protocol.Model{model}, func(cfg *protocol.Config) {
				cfg.UsageSink = protocol.UsageSinkFunc(func(entry protocol.UsageEntry) error {
					entries <- entry
					return nil
				})
			})
			response, body := post(t, url, route, payload, nil)
			if response.StatusCode != http.StatusOK || body != "data: {\"partial\":true}\n\n" {
				t.Fatalf("forwarded response %d %q", response.StatusCode, body)
			}
			var entry protocol.UsageEntry
			for entry.Status == "" || entry.Status == "started" {
				select {
				case entry = <-entries:
				case <-time.After(5 * time.Second):
					t.Fatal("terminal usage entry was not written")
				}
			}
			record := gateway.Metrics().Recent[0]
			if record.Status != http.StatusBadGateway || record.Error != "response_stream_interrupted" || record.Side != protocol.SideConnection || record.LedgerStatus() != "failed" {
				t.Fatalf("interrupted record %+v, ledger %q", record, record.LedgerStatus())
			}
			if record.TransportStatus == nil || *record.TransportStatus != http.StatusOK || record.Outcome != "failed" {
				t.Fatalf("transport/outcome %+v", record)
			}
			if record.Failure == nil || record.Failure.Reason != "response_stream_interrupted" || record.Failure.Side != protocol.SideConnection {
				t.Fatalf("failure %+v", record.Failure)
			}
			if entry.Status != "failed" || entry.ErrorCode != "response_stream_interrupted" || entry.HTTPStatus == nil || *entry.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("usage entry %+v", entry)
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

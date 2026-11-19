package protocol_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// failoverGateway wires one gateway whose model names a primary and standby
// connection, each pointing at its own fake vendor.
func failoverGateway(t *testing.T, model protocol.Model, servers map[string]*httptest.Server,
	config func(*protocol.Config)) (*protocol.Gateway, string) {
	t.Helper()
	endpoints := make([]protocol.Endpoint, 0, len(servers))
	for id, server := range servers {
		endpoints = append(endpoints, protocol.Endpoint{ID: id, BaseURL: server.URL, APIKey: "synthetic-" + id})
	}
	backend, err := protocol.NewBackend(endpoints, []protocol.Model{model})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	cfg := protocol.Config{Backend: backend, Key: gatewayKey, RecordRequests: true}
	if config != nil {
		config(&cfg)
	}
	gateway, err := protocol.NewGateway(cfg)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	server, url, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return gateway, url
}

func failoverModel(primary, standby string) protocol.Model {
	model := chatModel("kimi-failover")
	model.ConnectionID = &primary
	model.Failover = []protocol.ModelFailover{{ConnectionID: standby}}
	return model
}

func jsonReply(content string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + content +
			`"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}
}

// TestFailoverReachesTheStandbyAndAttributesTheStandby is the point of the
// feature: the primary cannot serve the request, the standby answers, and the
// record names the connection that really answered rather than the primary.
func TestFailoverReachesTheStandbyAndAttributesTheStandby(t *testing.T) {
	down := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	up := newUpstream(t, jsonReply("from standby"))
	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"primary": down.server, "standby": up.server}, nil)

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	if reply.StatusCode != 200 || !strings.Contains(body, "from standby") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if len(down.captured()) != 1 || len(up.captured()) != 1 {
		t.Fatalf("attempts primary=%d standby=%d", len(down.captured()), len(up.captured()))
	}
	record := gateway.Metrics().Recent[0]
	if record.Attempts != 2 {
		t.Fatalf("attempts %d", record.Attempts)
	}
	if record.ConnectionID == nil || *record.ConnectionID != "standby" {
		t.Fatalf("recorded connection %v", record.ConnectionID)
	}
	if record.FailoverFrom == nil || *record.FailoverFrom != "primary" {
		t.Fatalf("failoverFrom %v", record.FailoverFrom)
	}
	if record.Status != 200 || record.Usage == nil || *record.Usage.InputTokens != 10 {
		t.Fatalf("record %+v", record)
	}
	// Each attempt carries its own connection's credential, never the other's.
	if got := up.captured()[0].Headers.Get("Authorization"); got != "Bearer synthetic-standby" {
		t.Fatalf("standby authorization %q", got)
	}
	if got := down.captured()[0].Headers.Get("Authorization"); got != "Bearer synthetic-primary" {
		t.Fatalf("primary authorization %q", got)
	}
}

// TestFailoverRetriesOnlyRetryableStatuses makes the boundary explicit: a
// rejected request is answered as it is, because sending it to a second vendor
// would fail again and could be charged twice.
func TestFailoverRetriesOnlyRetryableStatuses(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		retried bool
	}{
		{"rate limited", http.StatusTooManyRequests, true},
		{"internal error", http.StatusInternalServerError, true},
		{"bad gateway", http.StatusBadGateway, true},
		{"gateway timeout", http.StatusGatewayTimeout, true},
		{"bad request", http.StatusBadRequest, false},
		{"unauthorized", http.StatusUnauthorized, false},
		{"forbidden", http.StatusForbidden, false},
		{"not found", http.StatusNotFound, false},
		{"unprocessable", http.StatusUnprocessableEntity, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"error":{"type":"synthetic"}}`))
			})
			standby := newUpstream(t, jsonReply("should not be used"))
			gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
				map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)
			reply, _ := post(t, url, "/chat/completions",
				map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
			// A retried request is answered by the standby, so the client sees the
			// standby's status. A non-retryable one is answered by the primary.
			want := test.status
			if test.retried {
				want = http.StatusOK
			}
			if reply.StatusCode != want {
				t.Fatalf("status %d want %d", reply.StatusCode, want)
			}
			record := gateway.Metrics().Recent[0]
			if test.retried {
				if len(standby.captured()) != 1 || record.Attempts != 2 {
					t.Fatalf("retryable status was not retried: attempts %d", record.Attempts)
				}
				if record.ConnectionID == nil || *record.ConnectionID != "standby" {
					t.Fatalf("recorded connection %v", record.ConnectionID)
				}
				return
			}
			if len(standby.captured()) != 0 {
				t.Fatalf("non-retryable status %d reached the standby", test.status)
			}
			if record.Attempts != 1 || record.ConnectionID == nil || *record.ConnectionID != "primary" {
				t.Fatalf("attempts %d connection %v", record.Attempts, record.ConnectionID)
			}
		})
	}
}

// TestFailoverIsNotRetriedAfterTheStreamStarted pins the rule that matters most
// for a user: once bytes have been forwarded, a broken stream is reported rather
// than silently continued somewhere else.
func TestFailoverIsNotRetriedAfterTheStreamStarted(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	})
	standby := newUpstream(t, jsonReply("should not be used"))
	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "stream": true, "messages": []any{}}, nil)
	if !strings.Contains(body, "partial") {
		t.Fatalf("first chunk did not reach the client: %q", body)
	}
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	if len(standby.captured()) != 0 {
		t.Fatalf("a broken stream was silently continued on the standby")
	}
	record := gateway.Metrics().Recent[0]
	if record.Attempts != 1 || record.ConnectionID == nil || *record.ConnectionID != "primary" {
		t.Fatalf("attempts %d connection %v", record.Attempts, record.ConnectionID)
	}
}

// TestFailoverStopsAtTheFirstWorkingCandidate proves the ordered list is a list
// and not a broadcast: a later candidate is never contacted.
func TestFailoverStopsAtTheFirstWorkingCandidate(t *testing.T) {
	down := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	good := newUpstream(t, jsonReply("good"))
	never := newUpstream(t, jsonReply("never"))
	model := failoverModel("primary", "second")
	model.Failover = append(model.Failover, protocol.ModelFailover{ConnectionID: "third"})
	gateway, url := failoverGateway(t, model, map[string]*httptest.Server{
		"primary": down.server, "second": good.server, "third": never.server}, nil)
	if reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	if len(never.captured()) != 0 {
		t.Fatalf("a later candidate was contacted after one answered")
	}
	if record := gateway.Metrics().Recent[0]; record.ConnectionID == nil || *record.ConnectionID != "second" {
		t.Fatalf("recorded connection %v", gateway.Metrics().Recent[0].ConnectionID)
	}
}

// TestFailoverUsesTheStandbysOwnUpstreamModelID covers one local model served
// under a different vendor name, which is what a multi-provider setup looks like
// in practice.
func TestFailoverUsesTheStandbysOwnUpstreamModelID(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadGateway)
	})
	standby := newUpstream(t, jsonReply("renamed"))
	local, vendor := "kimi-renamed", "vendor/renamed"
	primaryID := "primary"
	model := chatModel(local)
	model.ConnectionID = &primaryID
	model.Failover = []protocol.ModelFailover{{ConnectionID: "standby", UpstreamModelID: &vendor}}
	_, url := failoverGateway(t, model,
		map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)

	if reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": local, "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	captured := standby.captured()
	if len(captured) != 1 {
		t.Fatalf("standby attempts %d", len(captured))
	}
	var seen struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(captured[0].Body), &seen) != nil || seen.Model != vendor {
		t.Fatalf("standby saw model %q", seen.Model)
	}
	first := primary.captured()
	if len(first) != 1 || !strings.Contains(first[0].Body, `"model":"`+local+`"`) {
		t.Fatalf("primary body %v", first)
	}
}

// TestAllCandidatesFailingIsReportedAsSuch keeps the failure honest: the client
// learns the failover itself failed, not just the last candidate's error.
func TestAllCandidatesFailingIsReportedAsSuch(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	standby := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadGateway)
	})
	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	// The last candidate's own status is what the client sees, because that is
	// the answer it has to act on.
	if reply.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	record := gateway.Metrics().Recent[0]
	if record.Attempts != 2 || record.ConnectionID == nil || *record.ConnectionID != "standby" {
		t.Fatalf("attempts %d connection %v", record.Attempts, record.ConnectionID)
	}
	if record.Error != "all_connections_failed" {
		t.Fatalf("error %q", record.Error)
	}
	if record.Failure == nil || record.Failure.Side != protocol.SideUpstream {
		t.Fatalf("failure %+v", record.Failure)
	}
}

// TestFailoverNeverReachesAnUnconfiguredConnection: a standby this process does
// not hold is skipped, and a model whose every connection is missing is a local
// configuration answer rather than a transport failure.
func TestFailoverNeverReachesAnUnconfiguredConnection(t *testing.T) {
	up := newUpstream(t, jsonReply("ok"))
	_, url := failoverGateway(t, failoverModel("primary", "missing"),
		map[string]*httptest.Server{"primary": up.server}, nil)
	if reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if len(up.captured()) != 1 {
		t.Fatalf("primary attempts %d", len(up.captured()))
	}

	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"other": up.server}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "connection_unavailable" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if record := gateway.Metrics().Recent[0]; record.Failure == nil || record.Failure.Side != protocol.SideLocal {
		t.Fatalf("failure %+v", record.Failure)
	}
}

// TestFailoverDoesNotMaskAMissingPrimary: a primary the process cannot reach is
// a broken configuration, and quietly spending at a standby instead would hide
// it. This is the same answer every release before failover gave.
func TestFailoverDoesNotMaskAMissingPrimary(t *testing.T) {
	standby := newUpstream(t, jsonReply("should not be used"))
	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"standby": standby.server}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "connection_unavailable" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if len(standby.captured()) != 0 {
		t.Fatal("a missing primary silently moved the request to the standby")
	}
	if record := gateway.Metrics().Recent[0]; record.Attempts != 0 || record.Failure == nil ||
		record.Failure.Side != protocol.SideLocal {
		t.Fatalf("record %+v", record)
	}
}

// TestFailoverRespectsCancellation: a cancelled request is not retried, because
// the client stopped waiting and a standby attempt would spend money for nobody.
func TestFailoverRespectsCancellation(t *testing.T) {
	blocked := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		<-r.Context().Done()
	})
	standby := newUpstream(t, jsonReply("should not be used"))
	_, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"primary": blocked.server, "standby": standby.server}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/chat/completions",
		strings.NewReader(`{"model":"kimi-failover","messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		reply, err := http.DefaultClient.Do(request)
		if err == nil {
			_, _ = reply.Body.Read(make([]byte, 8))
			_ = reply.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(blocked.captured()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(standby.captured()) != 0 {
		t.Fatalf("a cancelled request was retried on the standby")
	}
}

// TestFailoverDoesNotRetryARequestTheUpstreamAlreadyAnswered is the money-safety
// rule: once the vendor has taken the request, a failure to read or convert its
// reply must be reported, never re-sent somewhere else that would bill again.
// A 200 whose body cannot be converted is exactly that case.
func TestFailoverDoesNotRetryARequestTheUpstreamAlreadyAnswered(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not a message`))
	})
	standby := newUpstream(t, jsonReply("should not be used"))
	anthropic := protocol.ProtocolAnthropic
	model := failoverModel("primary", "standby")
	model.UpstreamProtocol = &anthropic
	gateway, url := failoverGateway(t, model,
		map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "upstream_invalid_response" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if len(standby.captured()) != 0 {
		t.Fatalf("a reply the upstream already answered was re-sent: %d standby attempts", len(standby.captured()))
	}
	if record := gateway.Metrics().Recent[0]; record.Attempts != 1 {
		t.Fatalf("attempts %d", record.Attempts)
	}
}

func TestFailoverDoesNotRetryAfterUpstreamConsumesRequestThenDisconnects(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	})
	standby := newUpstream(t, jsonReply("must not be billed again"))
	gateway, url := failoverGateway(t, failoverModel("primary", "standby"),
		map[string]*httptest.Server{"primary": primary.server, "standby": standby.server}, nil)
	reply, _ := post(t, url, "/chat/completions", map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	if len(primary.captured()) != 1 || len(standby.captured()) != 0 || reply.StatusCode == 200 {
		t.Fatal("a consumed request was retried after an ambiguous disconnect")
	}
	if record := gateway.Metrics().Recent[0]; record.Attempts != 1 {
		t.Fatal("ambiguous exchange tried multiple connections")
	}
}

func TestFailoverRetriesAConnectionThatNeverOpened(t *testing.T) {
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonReply("never reaches here")(w, r, nil)
	}))
	dead := unreachable.URL
	unreachable.Close()
	standby := newUpstream(t, jsonReply("from standby"))
	endpoints := []protocol.Endpoint{
		{ID: "primary", BaseURL: dead, APIKey: "synthetic-primary"},
		{ID: "standby", BaseURL: standby.server.URL, APIKey: "synthetic-standby"},
	}
	model := failoverModel("primary", "standby")
	backend, err := protocol.NewBackend(endpoints, []protocol.Model{model})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	gateway, err := protocol.NewGateway(protocol.Config{Backend: backend, Key: gatewayKey, RecordRequests: true})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	server, url, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-failover", "messages": []any{}}, nil)
	if reply.StatusCode != 200 || !strings.Contains(body, "from standby") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if record := gateway.Metrics().Recent[0]; record.Attempts != 2 ||
		record.FailoverFrom == nil || *record.FailoverFrom != "primary" {
		t.Fatalf("attempts %d failoverFrom %v", record.Attempts, record.FailoverFrom)
	}
}

// TestCandidatesWithoutFailoverAreExactlyTheOldSingleUpstream keeps the
// compatibility promise: a row that never mentions failover gets one candidate.
func TestCandidatesWithoutFailoverAreExactlyTheOldSingleUpstream(t *testing.T) {
	id := "work"
	upstreamID := "vendor/model"
	model := chatModel("kimi-plain")
	model.ConnectionID = &id
	model.UpstreamModelID = &upstreamID
	candidates := model.Candidates()
	if len(candidates) != 1 {
		t.Fatalf("candidates %d", len(candidates))
	}
	if candidates[0].ConnectionID == nil || *candidates[0].ConnectionID != id ||
		candidates[0].UpstreamID != upstreamID || candidates[0].Standby {
		t.Fatalf("candidate %+v", candidates[0])
	}

	legacy := chatModel("kimi-legacy")
	legacyCandidates := legacy.Candidates()
	if len(legacyCandidates) != 1 || legacyCandidates[0].ConnectionID != nil ||
		legacyCandidates[0].UpstreamID != "kimi-legacy" {
		t.Fatalf("legacy candidate %+v", legacyCandidates[0])
	}
}

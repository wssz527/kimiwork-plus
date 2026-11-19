package bridge

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func spendPtr[T any](value T) *T { return &value }

func spendLedger(t *testing.T, entries ...ledger.Entry) *ledger.Ledger {
	t.Helper()
	instance, err := ledger.New(ledger.Options{File: filepath.Join(t.TempDir(), "ledger.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if result := instance.Record(entry); !result.OK {
			t.Fatalf("ledger entry %s: %s", entry.OperationID, result.Reason)
		}
	}
	return instance
}

func spendRefusalStatus(code string) int64 {
	switch code {
	case "request_too_large":
		return 413
	case "model_not_enabled_for_kimi", "model_protocol_mismatch":
		return 409
	case "budget_limit_reached":
		return 429
	case "connection_unavailable", "catalog_unavailable", "instructions_unavailable":
		return 503
	}
	return 400
}

func spendRefused(id string, at time.Time, code string) ledger.Entry {
	return ledger.Entry{OperationID: id, At: at.UnixMilli(), Status: "failed",
		HTTPStatus: spendPtr(spendRefusalStatus(code)), ErrorCode: spendPtr(code)}
}

func spendUnknown(id string, at time.Time, status, code string, httpStatus int) ledger.Entry {
	entry := ledger.Entry{OperationID: id, At: at.UnixMilli(), Status: status,
		HTTPStatus: spendPtr(int64(httpStatus))}
	if code != "" {
		entry.ErrorCode = spendPtr(code)
	}
	return entry
}

func spendPriced(id string, at time.Time, cost float64) ledger.Entry {
	return ledger.Entry{OperationID: id, At: at.UnixMilli(), Status: "completed",
		EstimatedCost: spendPtr(cost), PriceSource: spendPtr("budget_table:USD")}
}

func spendZone(t *testing.T, name string) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

func assertSpend(t *testing.T, name string, got, want protocol.Spend) {
	t.Helper()
	if got.Cost != want.Cost || got.Priced != want.Priced || got.Unpriced != want.Unpriced ||
		got.Inflight != want.Inflight || got.Partial != want.Partial {
		t.Fatalf("%s spend %+v, want %+v", name, got, want)
	}
}

func TestSpendExcludesRefusalsThatNeverReachedAnUpstream(t *testing.T) {
	location := spendZone(t, "Asia/Shanghai")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, location)
	yesterday := now.AddDate(0, 0, -1)
	entries := []ledger.Entry{
		spendPriced("priced-today", now, 0.25),
		spendPriced("priced-yesterday", yesterday, 0.10),
		ledger.Entry{OperationID: "inflight", At: now.UnixMilli(), Status: "started"},
		spendRefused("local-yesterday", yesterday, "invalid_json"),
	}
	for _, code := range []string{
		"request_too_large", "invalid_json", "invalid_model", "model_not_enabled_for_kimi",
		"model_protocol_mismatch", "budget_limit_reached", "connection_unavailable",
		"catalog_unavailable", "instructions_unavailable", "invalid_messages", "invalid_input",
	} {
		entries = append(entries, spendRefused("local-"+code, now, code))
	}
	gate := newBudgetGate(spendLedger(t, entries...), func() time.Time { return now })
	day, month := gate.spend(now, location, "budget_table:USD")
	assertSpend(t, "day", day, protocol.Spend{Cost: 0.25, Priced: 1, Unpriced: 0, Inflight: 1})
	assertSpend(t, "month", month, protocol.Spend{Cost: 0.35, Priced: 2, Unpriced: 0, Inflight: 1})
}

func TestSpendKeepsUnknownCostsAfterForwarding(t *testing.T) {
	location := spendZone(t, "Asia/Shanghai")
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, location)
	gate := newBudgetGate(spendLedger(t,
		spendPriced("priced", now, 0.125),
		ledger.Entry{OperationID: "inflight", At: now.UnixMilli(), Status: "started"},
		spendUnknown("upstream-500", now, "failed", "", 500),
		spendUnknown("upstream-429", now, "failed", "", 429),
		spendUnknown("upstream-401", now, "failed", "", 401),
		spendUnknown("cancelled", now, "cancelled", "request_cancelled", 499),
		spendUnknown("stream-interrupted", now, "failed", "response_stream_interrupted", 502),
		spendUnknown("stream-failed", now, "failed", "upstream_stream_failed", 200),
		spendUnknown("connection-failed", now, "failed", "upstream_connection_failed", 503),
		spendUnknown("invalid-response", now, "failed", "upstream_invalid_response", 503),
		spendUnknown("all-failed", now, "failed", "all_connections_failed", 502),
		spendUnknown("incomplete", now, "incomplete", "", 200),
		spendUnknown("unpriced-success", now, "completed", "", 200),
		spendUnknown("unknown-failure", now, "failed", "local_error", 500),
		spendUnknown("invalid-request", now, "failed", "invalid_request", 400),
		spendUnknown("invalid-protocol", now, "failed", "invalid_protocol", 400),
		spendUnknown("thinking-budget", now, "failed", "invalid_thinking_budget", 400),
		spendUnknown("thinking-roundtrip", now, "failed",
			"anthropic_thinking_tool_roundtrip_unsupported", 400),
	), func() time.Time { return now })
	day, month := gate.spend(now, location, "budget_table:USD")
	want := protocol.Spend{Cost: 0.125, Priced: 1, Unpriced: 16, Inflight: 1, Partial: true}
	assertSpend(t, "day", day, want)
	assertSpend(t, "month", month, want)
}

func TestBudgetDoesNotMarkSpendPartialForLocalRefusals(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai",
		"daily":    core.Document{"limit": costPerRequest, "action": "block"},
		"monthly":  core.Document{"limit": 100, "action": "remind"}})
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != http.StatusOK {
		t.Fatalf("priced request: %d %s", reply.StatusCode, body)
	}
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != http.StatusTooManyRequests ||
		errorCodeOf(t, body) != "budget_limit_reached" {
		t.Fatalf("budget refusal: %d %s", reply.StatusCode, body)
	}
	if code, body := rawChatCall(t, f, "{"); code != http.StatusBadRequest || errorCodeOf(t, body) != "invalid_json" {
		t.Fatalf("invalid body: %d %s", code, body)
	}
	if reply, body := chatCall(t, f, "kimi-missing"); reply.StatusCode != http.StatusConflict ||
		errorCodeOf(t, body) != "model_not_enabled_for_kimi" {
		t.Fatalf("disabled model: %d %s", reply.StatusCode, body)
	}
	if f.requests.Load() != 1 {
		t.Fatalf("local refusals reached the vendor: %d attempts", f.requests.Load())
	}
	code, result := f.budget(t, nil)
	if code != http.StatusOK {
		t.Fatalf("budget status %d", code)
	}
	status := core.Object(core.Object(result["budget"])["status"])
	for _, window := range []string{"daily", "monthly"} {
		row := core.Object(core.Object(status["spend"])[window])
		if row["spent"] != costPerRequest || row["pricedRequests"] != float64(1) ||
			row["unpricedRequests"] != float64(0) || row["partial"] != false {
			t.Fatalf("%s spend counts local refusals: %v", window, row)
		}
		if row["inflightRequests"] != float64(0) {
			t.Fatalf("%s inflight %v", window, row["inflightRequests"])
		}
	}
}

func rawChatCall(t *testing.T, f *budgetFixture, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest("POST", f.service.Endpoint.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.runtime.Token())
	request.Header.Set("Content-Type", "application/json")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	raw := make([]byte, 4096)
	n, _ := reply.Body.Read(raw)
	return reply.StatusCode, string(raw[:n])
}

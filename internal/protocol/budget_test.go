package protocol_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func TestParseBudgetAcceptsACompleteDocument(t *testing.T) {
	policy, err := protocol.ParseBudget(map[string]any{
		"enabled": true, "currency": "CNY", "timeZone": "Asia/Shanghai", "serialize": true,
		"daily":   map[string]any{"limit": 12.5, "action": "block"},
		"monthly": map[string]any{"limit": 200, "action": "remind"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !policy.Enabled || policy.Currency != "CNY" || policy.TimeZone != "Asia/Shanghai" || !policy.Serialize {
		t.Fatalf("policy %+v", policy)
	}
	if policy.Daily == nil || policy.Daily.Limit != 12.5 || policy.Daily.Action != protocol.BudgetActionBlock {
		t.Fatalf("daily %+v", policy.Daily)
	}
	if policy.Monthly == nil || policy.Monthly.Action != protocol.BudgetActionRemind {
		t.Fatalf("monthly %+v", policy.Monthly)
	}
	if !policy.Enforcing() {
		t.Fatalf("policy should enforce")
	}
	// The document round-trips: what is saved is what is read back.
	again, err := protocol.ParseBudget(policy.Document())
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if again.Daily == nil || again.Daily.Limit != policy.Daily.Limit || again.TimeZone != policy.TimeZone {
		t.Fatalf("round trip %+v", again)
	}
}

func TestParseBudgetRefusesWhatCannotBeEnforced(t *testing.T) {
	for name, document := range map[string]any{
		"missing currency": map[string]any{"enabled": true, "daily": map[string]any{"limit": 1}},
		"lowercase currency": map[string]any{"enabled": true, "currency": "usd",
			"daily": map[string]any{"limit": 1}},
		"zero limit": map[string]any{"enabled": true, "currency": "USD", "daily": map[string]any{"limit": 0}},
		"negative limit": map[string]any{"enabled": true, "currency": "USD",
			"daily": map[string]any{"limit": -1}},
		"unknown action": map[string]any{"enabled": true, "currency": "USD",
			"daily": map[string]any{"limit": 1, "action": "drop"}},
		"unknown window": map[string]any{"enabled": true, "currency": "USD", "weekly": map[string]any{"limit": 1}},
		"unknown field":  map[string]any{"enabled": true, "currency": "USD", "dailyy": map[string]any{"limit": 1}},
		"unknown zone":   map[string]any{"enabled": true, "currency": "USD", "timeZone": "Mars/Base"},
		"not an object":  []string{"enabled"},
		"half a limit": map[string]any{"enabled": true, "currency": "USD",
			"daily": map[string]any{"limit": 1}, "monthly": map[string]any{"action": "block"}},
	} {
		if _, err := protocol.ParseBudget(document); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestParseBudgetEmptyDocumentIsNoPolicy(t *testing.T) {
	policy, err := protocol.ParseBudget(nil)
	if err != nil || !policy.IsZero() {
		t.Fatalf("nil: %+v %v", policy, err)
	}
	policy, err = protocol.ParseBudget(map[string]any{})
	if err != nil || policy.Document() != nil {
		t.Fatalf("empty: %+v %v", policy, err)
	}
	// A disabled policy with no window is still a document: it carries the unit a
	// price table needs, so it must round-trip rather than vanish.
	policy, err = protocol.ParseBudget(map[string]any{"enabled": false, "currency": "USD", "timeZone": "UTC"})
	if err != nil || policy.Currency != "USD" {
		t.Fatalf("disabled: %+v %v", policy, err)
	}
	if policy.Enforcing() {
		t.Fatalf("a disabled policy must not enforce")
	}
}

func TestModelPriceRequiresBothSides(t *testing.T) {
	if _, err := protocol.ParseModelPrice(map[string]any{"inputPerMillion": 1}); err == nil {
		t.Fatal("a price without an output rate was accepted")
	}
	if _, err := protocol.ParseModelPrice(map[string]any{"inputPerMillion": 1, "outputPerMillion": 1, "perRequest": 0.1}); err == nil {
		t.Fatal("an unknown price field was accepted")
	}
	if _, err := protocol.ParseModelPrice(map[string]any{"inputPerMillion": -1, "outputPerMillion": 1}); err == nil {
		t.Fatal("a negative rate was accepted")
	}
	price, err := protocol.ParseModelPrice(map[string]any{"inputPerMillion": 2.5, "outputPerMillion": 10,
		"cachedInputPerMillion": 0.25})
	if err != nil || price.InputPerMillion == nil || *price.OutputPerMillion != 10 {
		t.Fatalf("price %+v %v", price, err)
	}
}

func TestPriceTableCostsWhatTheUpstreamReported(t *testing.T) {
	input, output := 1.0, 2.0
	table := protocol.NewPriceTable("USD", map[string]protocol.ModelPrice{
		"priced": {InputPerMillion: &input, OutputPerMillion: &output},
	})
	if table == nil || table.Source() != protocol.BudgetSourcePrefix+"USD" || !table.Has("priced") {
		t.Fatalf("table %+v", table)
	}
	million := func(value int64) *int64 { return &value }
	// 1M input and 1M output at 1.0 and 2.0 is 3.0.
	cost := table.Cost("priced", &protocol.Usage{InputTokens: million(1_000_000), OutputTokens: million(1_000_000)})
	if cost == nil || *cost != 3 {
		t.Fatalf("cost %v", cost)
	}
	// Cached input is a subset of input and is charged at its own rate once.
	cached := 0.5
	cachedTable := protocol.NewPriceTable("USD", map[string]protocol.ModelPrice{
		"priced": {InputPerMillion: &input, OutputPerMillion: &output, CachedInputPerMillion: &cached},
	})
	cost = cachedTable.Cost("priced", &protocol.Usage{InputTokens: million(1_000_000), OutputTokens: million(0),
		CachedInputTokens: million(1_000_000)})
	if cost == nil || *cost != 0.5 {
		t.Fatalf("cached cost %v", cost)
	}
	// Unknown models and missing evidence produce no cost at all.
	if cost := table.Cost("unpriced", &protocol.Usage{InputTokens: million(1), OutputTokens: million(1)}); cost != nil {
		t.Fatalf("an unpriced model was charged %v", *cost)
	}
	if cost := table.Cost("priced", &protocol.Usage{InputTokens: million(10)}); cost != nil {
		t.Fatalf("a half-reported usage was charged %v", *cost)
	}
	if cost := table.Cost("priced", nil); cost != nil {
		t.Fatalf("a usage-less request was charged %v", *cost)
	}
	// A vendor that reports more cached tokens than input is clamped, not
	// allowed to produce a negative charge.
	cost = cachedTable.Cost("priced", &protocol.Usage{InputTokens: million(10), OutputTokens: million(0),
		CachedInputTokens: million(20)})
	if cost == nil || *cost < 0 {
		t.Fatalf("clamped cost %v", cost)
	}
}

// fakeGate is a budget gate under the test's own control, so the gateway wiring
// can be checked separately from the daemon's ledger arithmetic.
type fakeGate struct {
	mu       sync.Mutex
	decision protocol.BudgetDecision
	requests []protocol.BudgetRequest
	settled  []protocol.BudgetSettlement
	releases int
	cost     *float64
}

func (f *fakeGate) Admit(_ context.Context, request protocol.BudgetRequest) (protocol.BudgetDecision, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	decision := f.decision
	if decision.Refusal == nil {
		return decision, func() { f.mu.Lock(); f.releases++; f.mu.Unlock() }
	}
	return decision, nil
}

func (f *fakeGate) Settle(settlement protocol.BudgetSettlement) (*float64, *string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, settlement)
	if f.cost == nil || settlement.Prices == nil {
		return nil, nil
	}
	source := settlement.Prices.Source()
	cost := *f.cost
	return &cost, &source
}

func TestGatewayAsksTheGateOnceAndSettlesOnce(t *testing.T) {
	up := newUpstream(t, jsonReply("ok"))
	prices := protocol.NewPriceTable("USD", map[string]protocol.ModelPrice{"kimi-cost": {
		InputPerMillion: one(), OutputPerMillion: one()}})
	gate := &fakeGate{decision: protocol.BudgetDecision{Windows: []string{"daily"}, Prices: prices}}
	cost := 0.25
	gate.cost = &cost
	gateway, url := startGateway(t, up.server.URL, []protocol.Model{chatModel("kimi-cost")},
		func(c *protocol.Config) { c.Budget = gate })

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-cost", "messages": []any{}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if reply.Header.Get(protocol.BudgetWarningHeader) != "daily" {
		t.Fatalf("warning %q", reply.Header.Get(protocol.BudgetWarningHeader))
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.requests) != 1 || gate.requests[0].Model != "kimi-cost" {
		t.Fatalf("admissions %+v", gate.requests)
	}
	if len(gate.settled) != 1 || gate.settled[0].Model != "kimi-cost" ||
		gate.settled[0].Usage == nil || *gate.settled[0].Usage.InputTokens != 10 {
		t.Fatalf("settlements %+v", gate.settled)
	}
	// The settlement carries the table the admission was made under, which is
	// what keeps an in-flight request in its own currency.
	if gate.settled[0].Prices != prices {
		t.Fatalf("settlement priced under %v", gate.settled[0].Prices)
	}
	if gate.releases != 1 {
		t.Fatalf("releases %d", gate.releases)
	}
	record := gateway.Metrics().Recent[0]
	if record.EstimatedCost == nil || *record.EstimatedCost != 0.25 ||
		record.PriceSource == nil || *record.PriceSource != "budget_table:USD" {
		t.Fatalf("record cost %v source %v", record.EstimatedCost, record.PriceSource)
	}
}

func one() *float64 {
	value := 1.0
	return &value
}

func TestGatewayRefusalCarriesWindowAndNeverReachesTheUpstream(t *testing.T) {
	up := newUpstream(t, jsonReply("ok"))
	// Both windows are reached; the month is the one a retry has to wait for.
	gate := &fakeGate{decision: protocol.BudgetDecision{Refusal: &protocol.Refusal{Status: 429,
		Code: "budget_limit_reached"}, Windows: []string{"daily", "monthly"},
		RefuseWindow: "monthly", RetryAfter: 90}}
	gateway, url := startGateway(t, up.server.URL, []protocol.Model{chatModel("kimi-cost")},
		func(c *protocol.Config) { c.Budget = gate })
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-cost", "messages": []any{}}, nil)
	if reply.StatusCode != 429 || errorCode(t, body) != "budget_limit_reached" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if reply.Header.Get(protocol.BudgetWindowHeader) != "monthly" {
		t.Fatalf("window %q", reply.Header.Get(protocol.BudgetWindowHeader))
	}
	if reply.Header.Get("Retry-After") != "90" {
		t.Fatalf("retry-after %q", reply.Header.Get("Retry-After"))
	}
	if len(up.captured()) != 0 {
		t.Fatal("a refused request reached the upstream")
	}
	record := gateway.Metrics().Recent[0]
	if record.Failure == nil || record.Failure.Side != protocol.SideLocal ||
		record.Failure.Reason != "budget_limit_reached" {
		t.Fatalf("failure %+v", record.Failure)
	}
	if record.EstimatedCost != nil {
		t.Fatal("a refused request was charged")
	}
}

func TestGatewayWithoutAGateWritesNoCost(t *testing.T) {
	up := newUpstream(t, jsonReply("ok"))
	gateway, url := startGateway(t, up.server.URL, []protocol.Model{chatModel("kimi-cost")}, nil)
	if reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-cost", "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	record := gateway.Metrics().Recent[0]
	if record.EstimatedCost != nil || record.PriceSource != nil {
		t.Fatalf("record %+v", record)
	}
}

func TestBudgetWindowBoundariesFollowTheConfiguredZone(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("zone data unavailable")
	}
	// 2026-03-01 00:30 in Shanghai is still February in UTC.
	now := time.Date(2026, 3, 1, 0, 30, 0, 0, zone)
	if start := protocol.MonthStart(now, zone); start.Month() != time.March || start.Day() != 1 || start.Hour() != 0 {
		t.Fatalf("month start %v", start)
	}
	if start := protocol.DayStart(now, zone); start.Day() != 1 || start.Hour() != 0 {
		t.Fatalf("day start %v", start)
	}
	if next := protocol.NextDay(now, zone); next.Day() != 2 {
		t.Fatalf("next day %v", next)
	}
	if next := protocol.NextMonth(now, zone); next.Month() != time.April {
		t.Fatalf("next month %v", next)
	}
	// The UTC instant of the local midnight is 16:00 the day before.
	if start := protocol.DayStart(now, zone).UTC(); start.Hour() != 16 || start.Day() != 28 {
		t.Fatalf("utc day start %v", start)
	}
}

func TestRetryableClassification(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504, 529} {
		if !protocol.RetryableStatus(status) {
			t.Fatalf("%d should be retryable", status)
		}
	}
	for _, status := range []int{200, 400, 401, 402, 403, 404, 409, 422} {
		if protocol.RetryableStatus(status) {
			t.Fatalf("%d should not be retryable", status)
		}
	}
}

func TestFailoverDocumentRoundTrips(t *testing.T) {
	rows := []json.RawMessage{
		json.RawMessage(`{"id":"kimi-cost","protocol":"chat","contextWindow":1000,
			"connectionId":"primary","failover":[{"connectionId":"backup"},
			{"connectionId":"primary"},{"connectionId":"backup"},{"connectionId":"third","upstreamModelId":"v/third"}],
			"pricing":{"inputPerMillion":1,"outputPerMillion":2}}`),
	}
	models, err := protocol.ValidateModels(rows)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	model := models[0]
	// The primary connection is dropped from the standby list and duplicates are
	// collapsed: one connection is tried once.
	if len(model.Failover) != 2 {
		t.Fatalf("failover %+v", model.Failover)
	}
	if model.Failover[0].ConnectionID != "backup" || model.Failover[1].ConnectionID != "third" {
		t.Fatalf("failover %+v", model.Failover)
	}
	if model.Pricing == nil || *model.Pricing.InputPerMillion != 1 {
		t.Fatalf("pricing %v", model.Pricing)
	}
	candidates := model.Candidates()
	if len(candidates) != 3 {
		t.Fatalf("candidates %d", len(candidates))
	}
	if candidates[0].ConnectionID == nil || *candidates[0].ConnectionID != "primary" || candidates[0].Standby {
		t.Fatalf("primary %+v", candidates[0])
	}
	if !candidates[1].Standby || candidates[1].UpstreamID != "kimi-cost" {
		t.Fatalf("backup %+v", candidates[1])
	}
	if candidates[2].UpstreamID != "v/third" {
		t.Fatalf("third %+v", candidates[2])
	}
}

func TestFailoverDocumentIsValidated(t *testing.T) {
	for name, row := range map[string]string{
		"bad connection id": `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"failover":[{"connectionId":"bad id"}]}`,
		"unknown field":     `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"failover":[{"connectionId":"a","baseURL":"x"}]}`,
		"bad protocol":      `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"failover":[{"connectionId":"a","upstreamProtocol":"bogus"}]}`,
		"not an array":      `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"failover":{"connectionId":"a"}}`,
		"half price":        `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"pricing":{"inputPerMillion":1}}`,
		"negative price":    `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"pricing":{"inputPerMillion":-1,"outputPerMillion":1}}`,
		"too many standbys": `{"id":"kimi-cost","protocol":"chat","contextWindow":1000,"failover":[{"connectionId":"a"},{"connectionId":"b"},{"connectionId":"c"},{"connectionId":"d"},{"connectionId":"e"},{"connectionId":"f"},{"connectionId":"g"},{"connectionId":"h"},{"connectionId":"i"}]}`,
	} {
		if _, err := protocol.ValidateModels([]json.RawMessage{json.RawMessage(row)}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

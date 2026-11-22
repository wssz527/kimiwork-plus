package bridge

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// budgetGate is the daemon's answer to the protocol layer's two questions: may
// this request go out, and what did it cost. Spend is never held in a second
// store — it is read back out of the same ledger the panel shows and the export
// writes, so a restart, a clear or a retention cut moves the budget total in
// exactly the way it moves the usage page.
type budgetGate struct {
	ledger *ledger.Ledger
	now    func() time.Time

	mu     sync.RWMutex
	policy protocol.BudgetPolicy
	prices *protocol.PriceTable
	// slot is the serialising gate. A nil channel means requests run in
	// parallel, which is the default and the behaviour of every earlier release.
	slot chan struct{}
}

func newBudgetGate(instance *ledger.Ledger, now func() time.Time) *budgetGate {
	if now == nil {
		now = time.Now
	}
	return &budgetGate{ledger: instance, now: now}
}

// configure swaps the policy in atomically, so a live request never sees a
// half-applied policy.
func (g *budgetGate) configure(policy protocol.BudgetPolicy, prices *protocol.PriceTable) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = policy
	g.prices = prices
	if policy.Enabled && policy.Serialize && policy.Location != nil {
		if g.slot == nil {
			g.slot = make(chan struct{}, 1)
		}
		return
	}
	g.slot = nil
}

// Policy returns the policy in force.
func (g *budgetGate) Policy() protocol.BudgetPolicy {
	policy, _ := g.snapshot()
	return policy
}

// Prices returns the price table in force, or nil.
func (g *budgetGate) Prices() *protocol.PriceTable {
	_, prices := g.snapshot()
	return prices
}

// snapshot reads the policy and the table under one lock. configure swaps both
// together, so reading them separately could pair an old table with a new
// policy and price a request in a currency its limits are not stated in.
func (g *budgetGate) snapshot() (protocol.BudgetPolicy, *protocol.PriceTable) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.policy, g.prices
}

// Admit implements protocol.BudgetGate.
func (g *budgetGate) Admit(ctx context.Context, request protocol.BudgetRequest) (protocol.BudgetDecision, func()) {
	// The table is bound before any limit is checked, so a request admitted by a
	// policy that only tracks spend is still priced in the unit it started in.
	policy, prices := g.snapshot()
	decision := protocol.BudgetDecision{Prices: prices}
	if !policy.Enabled || policy.Location == nil {
		return decision, nil
	}
	release := func() {}
	if slot := g.serialSlot(); slot != nil {
		select {
		case slot <- struct{}{}:
			release = func() { <-slot }
		case <-ctx.Done():
			// Waiting for a slot was itself the request being cancelled; nothing
			// was admitted, so the caller must not treat this as a spend.
			return protocol.BudgetDecision{Refusal: &protocol.Refusal{Status: 499, Code: "request_cancelled"}}, nil
		}
	}
	now := g.now()
	day, month := g.spend(now, policy.Location, protocol.BudgetSourcePrefix+policy.Currency)
	var blocking []struct {
		name  string
		reset time.Time
	}
	for _, window := range []struct {
		name  string
		limit *protocol.BudgetLimit
		spent protocol.Spend
		reset time.Time
	}{
		{name: "daily", limit: policy.Daily, spent: day, reset: protocol.NextDay(now, policy.Location)},
		{name: "monthly", limit: policy.Monthly, spent: month, reset: protocol.NextMonth(now, policy.Location)},
	} {
		if window.limit == nil || window.spent.Cost < window.limit.Limit {
			continue
		}
		decision.Windows = append(decision.Windows, window.name)
		if window.limit.Action == protocol.BudgetActionBlock {
			blocking = append(blocking, struct {
				name  string
				reset time.Time
			}{window.name, window.reset})
		}
	}
	if len(blocking) > 0 {
		// A retry only succeeds once every blocking window has lifted, so both the
		// wait and the named window are the last one to reset. Naming the sooner
		// window would invite a retry that is refused again.
		last := blocking[0]
		for _, window := range blocking[1:] {
			if window.reset.After(last.reset) {
				last = window
			}
		}
		decision.Refusal = &protocol.Refusal{Status: 429, Code: "budget_limit_reached"}
		decision.RefuseWindow = last.name
		decision.RetryAfter = int(last.reset.Sub(now).Seconds()) + 1
		release()
		return decision, nil
	}
	return decision, release
}

func (g *budgetGate) serialSlot() chan struct{} {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.slot
}

// Settle implements protocol.BudgetGate. It prices against the table the request
// was admitted under, so a currency or rate change while it was in flight does
// not move it into a different unit. A request whose model has no price stays
// unpriced: no cost and no price source, never a zero.
func (g *budgetGate) Settle(settlement protocol.BudgetSettlement) (*float64, *string) {
	prices := settlement.Prices
	if prices == nil || settlement.Model == "" {
		return nil, nil
	}
	cost := prices.Cost(settlement.Model, settlement.Usage)
	if cost == nil {
		return nil, nil
	}
	source := prices.Source()
	return cost, &source
}

// Historical costs keep their currency even when the last model price is removed.
// Only costs in the selected budget currency can be added together.
func (g *budgetGate) spend(now time.Time, location *time.Location, source string) (protocol.Spend, protocol.Spend) {
	dayStart := protocol.DayStart(now, location).UnixMilli()
	monthStart := protocol.MonthStart(now, location).UnixMilli()
	day := protocol.Spend{From: time.UnixMilli(dayStart).UTC().Format(time.RFC3339),
		To: now.UTC().Format(time.RFC3339)}
	month := protocol.Spend{From: time.UnixMilli(monthStart).UTC().Format(time.RFC3339),
		To: now.UTC().Format(time.RFC3339)}
	if g.ledger == nil {
		day.Partial, month.Partial = true, true
		return day, month
	}
	for _, record := range g.ledger.Records() {
		if record.Source == workUsageSource {
			continue
		}
		at := parseLedgerInstant(record.At)
		if at < monthStart {
			continue
		}
		// A request that has not finished has no cost yet. It is reported as
		// in flight rather than as an unpriced one, because its usage has not
		// been reported yet and very often will be.
		started := record.Status == "started"
		priced := !started && record.EstimatedCost != nil && record.PriceSource != nil &&
			*record.PriceSource == source
		refused := refusedBeforeForwarding(record)
		for _, window := range []*protocol.Spend{&month, &day} {
			if at < dayStart && window == &day {
				continue
			}
			switch {
			case started:
				window.Inflight++
			case priced:
				window.Cost += *record.EstimatedCost
				window.Priced++
			default:
				if !refused {
					window.Unpriced++
				}
			}
		}
	}
	for _, window := range []*protocol.Spend{&day, &month} {
		window.Partial = window.Unpriced > 0
		window.Cost = math.Round(window.Cost*1e8) / 1e8
	}
	return day, month
}

// These codes are exclusive to refusals before any upstream attempt. Failures
// from conversion, transport, cancellation or upstream responses remain unknown.
func refusedBeforeForwarding(record *ledger.Record) bool {
	if record.Status != "failed" || record.ErrorCode == nil || record.HTTPStatus == nil ||
		*record.HTTPStatus < http.StatusBadRequest || *record.HTTPStatus >= 600 {
		return false
	}
	switch *record.ErrorCode {
	case "request_too_large", "invalid_json", "invalid_model", "model_not_enabled_for_kimi",
		"model_protocol_mismatch", "budget_limit_reached", "connection_unavailable",
		"catalog_unavailable", "instructions_unavailable", "invalid_messages", "invalid_input":
		return true
	default:
		return false
	}
}

func parseLedgerInstant(value string) int64 {
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// budgetStatus renders the panel's view: what is enforced, what it has cost so
// far, and which configured models a limit cannot see.
func (g *budgetGate) budgetStatus(models []any) core.Document {
	policy, prices := g.snapshot()
	timeZone := policy.TimeZone
	if policy.Location != nil {
		timeZone = policy.Location.String()
	}
	status := core.Document{
		"enabled": policy.Enabled, "currency": policy.Currency, "timeZone": timeZone,
		"serialize": policy.Serialize, "enforcement": "estimate_from_ledger",
		"daily": nil, "monthly": nil, "spend": core.Document{"daily": nil, "monthly": nil},
	}
	if policy.Location != nil {
		now := g.now()
		day, month := g.spend(now, policy.Location, protocol.BudgetSourcePrefix+policy.Currency)
		for _, window := range []struct {
			name  string
			limit *protocol.BudgetLimit
			spent protocol.Spend
			reset time.Time
		}{
			{name: "daily", limit: policy.Daily, spent: day, reset: protocol.NextDay(now, policy.Location)},
			{name: "monthly", limit: policy.Monthly, spent: month, reset: protocol.NextMonth(now, policy.Location)},
		} {
			row := core.Document{"spent": window.spent.Cost, "pricedRequests": window.spent.Priced,
				"unpricedRequests": window.spent.Unpriced, "inflightRequests": window.spent.Inflight,
				"partial": window.spent.Partial, "resetsAt": window.reset.UTC().Format(time.RFC3339)}
			if window.limit != nil {
				row["limit"] = window.limit.Limit
				row["action"] = window.limit.Action
				row["reached"] = window.spent.Cost >= window.limit.Limit
				row["remaining"] = math.Max(window.limit.Limit-window.spent.Cost, 0)
				status[window.name] = core.Document{"limit": window.limit.Limit, "action": window.limit.Action}
			}
			core.Object(status["spend"])[window.name] = row
		}
	}
	priced := []any{}
	unpriced := []any{}
	for _, value := range models {
		id := core.String(core.Object(value)["id"])
		if id == "" {
			continue
		}
		if prices != nil && prices.Has(id) {
			priced = append(priced, id)
			continue
		}
		unpriced = append(unpriced, id)
	}
	status["pricedModels"] = priced
	status["unpricedModels"] = unpriced
	if policy.Enforcing() && len(unpriced) > 0 {
		// A limit cannot see a model it cannot price. Saying so is the difference
		// between an honest guard and a false sense of one.
		status["warning"] = "unpriced_models_not_limited"
	}
	return status
}

// budgetView is what /api/budget returns: the stored policy, the effective one,
// today's and this month's spend, and the models no limit can see.
func (r *Runtime) budgetView() (core.Document, error) {
	d, rev, err := r.store.Read()
	if err != nil {
		return nil, err
	}
	policy := r.budget.Policy()
	settings := core.Document{"configured": d["budget"], "effective": policy.Document(),
		"revision": rev, "status": r.budget.budgetStatus(core.Rows(d["models"]))}
	if prices := r.budget.Prices(); prices != nil {
		settings["priceSource"] = prices.Source()
		settings["currency"] = prices.Currency()
	}
	return core.Document{"budget": settings}, nil
}

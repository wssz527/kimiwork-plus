package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
)

// Budget enforcement is an estimate over the ledger this process wrote, not a
// vendor-side hard cap. A request whose usage the upstream never reported has no
// cost, so a window total is a floor until every request in it is priced, and a
// model without a complete price cannot contribute an estimated cost. Both are
// reported rather than shown as zero. Existing spend can still block new requests.
//
// Requests that overlap are admitted before the earlier one settles, so a
// blocking limit can be exceeded by the cost already in flight. `serialize`
// closes that gap at the price of admitting one request at a time.
const (
	BudgetActionRemind = "remind"
	BudgetActionBlock  = "block"

	// BudgetSourcePrefix names the price table a stored cost was computed from,
	// so a total never silently mixes two currencies.
	BudgetSourcePrefix = "budget_table:"

	// BudgetWarningHeader carries the reached reminder windows on the reply.
	BudgetWarningHeader = "X-Kimi-Work-Budget-Warning"
	// BudgetWindowHeader names the window that refused a request.
	BudgetWindowHeader = "X-Kimi-Work-Budget-Window"

	maxBudgetAmount    = 1e9
	maxPricePerMillion = 1e6
)

// Budget errors.
var (
	ErrInvalidBudget = errors.New("invalid_budget")
	ErrInvalidPrice  = errors.New("invalid_price")
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// BudgetLimit is one window's ceiling and what reaching it does.
type BudgetLimit struct {
	Limit  float64 `json:"limit"`
	Action string  `json:"action"`
}

// ModelPrice is the user's own price for one model in the budget currency, per
// one million tokens. Input and output are both required: a price that covers
// only one side cannot price a request, and a half estimate is worse than none.
type ModelPrice struct {
	InputPerMillion       *float64 `json:"inputPerMillion"`
	OutputPerMillion      *float64 `json:"outputPerMillion"`
	CachedInputPerMillion *float64 `json:"cachedInputPerMillion,omitempty"`
	CacheWritePerMillion  *float64 `json:"cacheWritePerMillion,omitempty"`
}

// BudgetPolicy is one validated budget document. Location is resolved once so a
// request never loads a zone.
type BudgetPolicy struct {
	Enabled   bool
	Currency  string
	TimeZone  string
	Location  *time.Location
	Daily     *BudgetLimit
	Monthly   *BudgetLimit
	Serialize bool
}

// IsZero reports whether the document says nothing at all, which is what keeps
// an installation that never opened budget settings byte-identical to the
// previous release.
func (p BudgetPolicy) IsZero() bool {
	return !p.Enabled && p.Daily == nil && p.Monthly == nil && !p.Serialize && p.Currency == "" && p.TimeZone == ""
}

// Enforcing reports whether a request can be warned about or refused.
func (p BudgetPolicy) Enforcing() bool {
	return p.Enabled && (p.Daily != nil || p.Monthly != nil)
}

// ParseBudget validates one stored budget document. An unknown field is refused
// rather than ignored: a typo in `monthly` must not silently leave the user
// unguarded.
func ParseBudget(raw any) (BudgetPolicy, error) {
	if raw == nil {
		return BudgetPolicy{}, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return BudgetPolicy{}, ErrInvalidBudget
	}
	policy := BudgetPolicy{}
	for key, value := range object {
		switch key {
		case "enabled":
			flag, ok := value.(bool)
			if !ok {
				return BudgetPolicy{}, ErrInvalidBudget
			}
			policy.Enabled = flag
		case "currency":
			text, ok := value.(string)
			if !ok || !currencyPattern.MatchString(text) {
				return BudgetPolicy{}, ErrInvalidBudget
			}
			policy.Currency = text
		case "timeZone":
			text, ok := value.(string)
			if !ok || len(text) > 64 {
				return BudgetPolicy{}, ErrInvalidBudget
			}
			policy.TimeZone = text
		case "daily", "monthly":
			if value == nil {
				continue
			}
			limit, err := parseBudgetLimit(value)
			if err != nil {
				return BudgetPolicy{}, err
			}
			if key == "daily" {
				policy.Daily = limit
			} else {
				policy.Monthly = limit
			}
		case "serialize":
			flag, ok := value.(bool)
			if !ok {
				return BudgetPolicy{}, ErrInvalidBudget
			}
			policy.Serialize = flag
		default:
			return BudgetPolicy{}, ErrInvalidBudget
		}
	}
	if policy.IsZero() {
		return BudgetPolicy{}, nil
	}
	// A budget document always names its money: a limit without a unit cannot be
	// enforced, and a price table without one cannot be totalled.
	if !currencyPattern.MatchString(policy.Currency) {
		return BudgetPolicy{}, ErrInvalidBudget
	}
	location := time.Local
	if policy.TimeZone != "" {
		resolved, err := time.LoadLocation(policy.TimeZone)
		if err != nil {
			return BudgetPolicy{}, ErrInvalidBudget
		}
		location = resolved
	}
	policy.TimeZone = location.String()
	policy.Location = location
	return policy, nil
}

func parseBudgetLimit(raw any) (*BudgetLimit, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidBudget
	}
	limit := &BudgetLimit{Action: BudgetActionBlock}
	for key, value := range object {
		switch key {
		case "limit":
			amount, ok := numberOf(value)
			if !ok || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 || amount > maxBudgetAmount {
				return nil, ErrInvalidBudget
			}
			limit.Limit = math.Round(amount*1e6) / 1e6
		case "action":
			action, ok := value.(string)
			if !ok || (action != BudgetActionRemind && action != BudgetActionBlock) {
				return nil, ErrInvalidBudget
			}
			limit.Action = action
		default:
			return nil, ErrInvalidBudget
		}
	}
	if limit.Limit <= 0 {
		return nil, ErrInvalidBudget
	}
	return limit, nil
}

func numberOf(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, true
	case float32:
		return float64(item), true
	case int:
		return float64(item), true
	case int64:
		return float64(item), true
	case json.Number:
		parsed, err := item.Float64()
		return parsed, err == nil
	}
	return 0, false
}

// ParseModelPrice validates one model row's `pricing` object.
func ParseModelPrice(raw any) (*ModelPrice, error) {
	if raw == nil {
		return nil, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, ErrInvalidPrice
	}
	price := &ModelPrice{}
	for key, value := range object {
		amount, ok := numberOf(value)
		if !ok || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount > maxPricePerMillion {
			return nil, ErrInvalidPrice
		}
		rounded := math.Round(amount*1e6) / 1e6
		switch key {
		case "inputPerMillion":
			price.InputPerMillion = &rounded
		case "outputPerMillion":
			price.OutputPerMillion = &rounded
		case "cachedInputPerMillion":
			price.CachedInputPerMillion = &rounded
		case "cacheWritePerMillion":
			price.CacheWritePerMillion = &rounded
		default:
			return nil, ErrInvalidPrice
		}
	}
	if price.InputPerMillion == nil || price.OutputPerMillion == nil {
		return nil, ErrInvalidPrice
	}
	return price, nil
}

// Document renders the model row's stored price, or nil when it has none.
func (p *ModelPrice) Document() map[string]any {
	if p == nil {
		return nil
	}
	out := map[string]any{}
	if p.InputPerMillion != nil {
		out["inputPerMillion"] = *p.InputPerMillion
	}
	if p.OutputPerMillion != nil {
		out["outputPerMillion"] = *p.OutputPerMillion
	}
	if p.CachedInputPerMillion != nil {
		out["cachedInputPerMillion"] = *p.CachedInputPerMillion
	}
	if p.CacheWritePerMillion != nil {
		out["cacheWritePerMillion"] = *p.CacheWritePerMillion
	}
	return out
}

// Document renders the canonical stored shape. Only set fields are written, so
// what is saved is what is read back.
func (p BudgetPolicy) Document() map[string]any {
	if p.IsZero() {
		return nil
	}
	out := map[string]any{"enabled": p.Enabled}
	if p.Currency != "" {
		out["currency"] = p.Currency
	}
	if p.TimeZone != "" {
		out["timeZone"] = p.TimeZone
	}
	if p.Serialize {
		out["serialize"] = true
	}
	if p.Daily != nil {
		out["daily"] = map[string]any{"limit": p.Daily.Limit, "action": p.Daily.Action}
	}
	if p.Monthly != nil {
		out["monthly"] = map[string]any{"limit": p.Monthly.Limit, "action": p.Monthly.Action}
	}
	return out
}

// ---------------------------------------------------------------------------

// PriceTable is the cost model distilled from the model rows and the budget
// currency. A nil table prices nothing, which is the pre-budget behaviour.
type PriceTable struct {
	currency string
	models   map[string]ModelPrice
}

// NewPriceTable builds the table. Models without a complete price keep no entry.
func NewPriceTable(currency string, prices map[string]ModelPrice) *PriceTable {
	if len(prices) == 0 || currency == "" {
		return nil
	}
	copied := make(map[string]ModelPrice, len(prices))
	for model, price := range prices {
		if model == "" || price.InputPerMillion == nil || price.OutputPerMillion == nil {
			continue
		}
		copied[model] = price
	}
	if len(copied) == 0 {
		return nil
	}
	return &PriceTable{currency: currency, models: copied}
}

// Currency is the unit every cost in this table is expressed in.
func (t *PriceTable) Currency() string {
	if t == nil {
		return ""
	}
	return t.currency
}

// Source is the ledger's price-source identifier for this table. It names the
// unit, so two tables with the same unit are the same money even if a rate was
// corrected later; a retotal over the ledger then still adds up.
func (t *PriceTable) Source() string {
	if t == nil {
		return ""
	}
	return BudgetSourcePrefix + t.currency
}

// Has reports whether this model has a complete price.
func (t *PriceTable) Has(model string) bool {
	if t == nil {
		return false
	}
	_, ok := t.models[model]
	return ok
}

// Cost prices one finished request, or returns nil when the request cannot be
// priced. A nil answer is missing evidence, never a zero charge.
func (t *PriceTable) Cost(model string, usage *Usage) *float64 {
	if t == nil || usage == nil {
		return nil
	}
	price, ok := t.models[model]
	if !ok || price.InputPerMillion == nil || price.OutputPerMillion == nil {
		return nil
	}
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		return nil
	}
	input := float64(*usage.InputTokens)
	output := float64(*usage.OutputTokens)
	cached := 0.0
	if usage.CachedInputTokens != nil {
		cached = float64(*usage.CachedInputTokens)
	}
	written := 0.0
	if usage.CacheWriteTokens != nil {
		written = float64(*usage.CacheWriteTokens)
	}
	// A cached token is reported inside the input count, so it is moved to its
	// own rate instead of being charged twice. A vendor that reports the two
	// parts inconsistently is clamped rather than allowed to produce a negative
	// full-rate remainder.
	if cached+written > input {
		over := cached + written - input
		if cached >= over {
			cached -= over
		} else {
			written -= over - cached
			cached = 0
		}
	}
	full := input - cached - written
	if full < 0 {
		full = 0
	}
	inputRate := *price.InputPerMillion
	cachedRate := inputRate
	if price.CachedInputPerMillion != nil {
		cachedRate = *price.CachedInputPerMillion
	}
	writeRate := inputRate
	if price.CacheWritePerMillion != nil {
		writeRate = *price.CacheWritePerMillion
	}
	cost := (full*inputRate + cached*cachedRate + written*writeRate + output*(*price.OutputPerMillion)) / 1e6
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return nil
	}
	rounded := math.Round(cost*1e8) / 1e8
	return &rounded
}

// ---------------------------------------------------------------------------

// BudgetRequest is what a gate knows before a request is forwarded.
type BudgetRequest struct {
	Route string
	Model string
	At    int64
}

// BudgetSettlement is one finished request. Usage is nil when the upstream never
// reported any, which is a request that cannot be priced. Prices is the table the
// request was admitted under: a currency change while it was in flight must not
// move it into the new unit.
type BudgetSettlement struct {
	Route  string
	Model  string
	At     int64
	Usage  *Usage
	Prices *PriceTable
}

// BudgetDecision is a gate's answer. A nil Refusal admits the request; Windows
// names every limit already reached ("daily", "monthly") and RefuseWindow names
// the blocking limit a retry has to wait for. Prices is the table in force at
// admission and is what the settlement is priced against.
type BudgetDecision struct {
	Refusal      *Refusal
	Windows      []string
	RefuseWindow string
	RetryAfter   int
	Prices       *PriceTable
}

// BudgetGate decides admission and prices finished work. The daemon implements
// it, because it owns the ledger the spend is read from. A nil gate keeps the
// pre-budget behaviour exactly.
type BudgetGate interface {
	// Admit may wait for a slot when the policy serialises requests. The
	// returned release must be called exactly once after the response is done
	// and is nil when the request was refused.
	Admit(ctx context.Context, request BudgetRequest) (BudgetDecision, func())
	// Settle prices one finished request against the table its own admission
	// carried, and returns nils when it cannot be priced.
	Settle(settlement BudgetSettlement) (*float64, *string)
}

// Spend is what the ledger holds for one window. Partial is true when some
// requests in it were not priced, which makes Cost a floor rather than a total.
type Spend struct {
	Cost     float64 `json:"cost"`
	Priced   int     `json:"pricedRequests"`
	Unpriced int     `json:"unpricedRequests"`
	Inflight int     `json:"inflightRequests"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Partial  bool    `json:"partial"`
}

// budgetWarning renders the reached reminder windows as one header value.
func budgetWarning(decision BudgetDecision) string {
	kept := make([]string, 0, len(decision.Windows))
	seen := map[string]bool{}
	for _, window := range decision.Windows {
		if (window == "daily" || window == "monthly") && !seen[window] {
			seen[window] = true
			kept = append(kept, window)
		}
	}
	return strings.Join(kept, ",")
}

// budgetRefusalWindow names the limit that refused a request, so the client can
// say which window is exhausted instead of showing a bare status. It is the
// window that unblocks last, matching the wait Retry-After states: a window that
// lifts tomorrow cannot make a retry succeed while the month is still spent.
func budgetRefusalWindow(decision BudgetDecision) string {
	if decision.RefuseWindow != "" {
		return decision.RefuseWindow
	}
	if len(decision.Windows) == 0 {
		return ""
	}
	return decision.Windows[0]
}

// optionalString keeps "" as absent, matching how every other record field
// distinguishes unknown from a value.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	copied := value
	return &copied
}

// DayStart is the local calendar boundary a daily window reads, which is what
// makes "today" mean the user's today and not UTC's.
func DayStart(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// MonthStart is the first instant of the local month.
func MonthStart(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
}

// NextDay is the instant a reached daily limit resets at.
func NextDay(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
}

// NextMonth is the first instant of the next local month.
func NextMonth(now time.Time, location *time.Location) time.Time {
	local := now.In(location)
	return time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, location)
}

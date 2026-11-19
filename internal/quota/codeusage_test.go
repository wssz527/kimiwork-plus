package quota_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/quota"
)

// The fixtures in this file are the two shapes the official clients parse, plus
// the aliases the vendor's own web build ships. They were transcribed from the
// readers the vendor ships, not invented:
//
//	Kimi Code.app  out/src-*.cjs   parseQuotaUsages / parseBoosterWallet
//	~/.kimi-code/bin/kimi          packages/oauth/src/managed-usage.ts
//
// The numbers are made up; the field names are not.

const codeUsageReply = `{
  "usages": {
    "limit_5h": {"used_ratio": 0.42, "reset_time": "2026-11-16T19:01:15.24Z"},
    "limit_7d": {"used_ratio": "0.2", "reset_time": "2026-11-23T05:01:15.24Z"},
    "limit_month_total": {"used_ratio": 0.31},
    "limit_month_code": {"used_ratio": 0.05}
  },
  "boosterWallet": {
    "balance": {"type": "BOOSTER", "amount": 500000000, "amountLeft": 350000000},
    "monthlyChargeLimitEnabled": true,
    "monthlyChargeLimit": {"priceInCents": 5000, "currency": "usd"},
    "monthlyUsed": {"priceInCents": 1200, "currency": "usd"}
  }
}`

type memoryKeys struct {
	values     map[string]string
	failWrite  bool
	failRead   bool
	failDelete bool
	// mangleNext models a store that accepted one write but did not persist it:
	// the next Set stores a transformed value, and later writes are clean.
	mangleNext func(string) string
}

func newMemoryKeys() *memoryKeys { return &memoryKeys{values: map[string]string{}} }

func (m *memoryKeys) Get(key string) (string, error) {
	if m.failRead {
		return "", errors.New("credential_store_failed")
	}
	// The caller's own store answers ("", nil) when nothing is saved, which is
	// "missing", not a failure.
	return m.values[key], nil
}

func (m *memoryKeys) Set(key, value string) error {
	if m.failWrite {
		return errors.New("credential_write_denied")
	}
	if m.mangleNext != nil {
		value, m.mangleNext = m.mangleNext(value), nil
	}
	m.values[key] = value
	return nil
}

func (m *memoryKeys) Delete(key string) error {
	if m.failDelete {
		return errors.New("credential_delete_failed")
	}
	delete(m.values, key)
	return nil
}

var (
	codeManualKey = fakeKey("kc", "manual", "test", "d4e5f6a7")
	codeWorkKey   = fakeKey("kc", "work", "test", "b1c2d3e4")
	codeLocalKey  = fakeKey("kc", "local", "test", "f7a8b9c0")
)

func codeClock(clock *int64) func() int64 {
	return func() int64 { return *clock }
}

// hasNote reports whether a note list carries one code. The handlers answer with
// several notes at once, so a test asserts membership rather than position.
func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if note == want {
			return true
		}
	}
	return false
}

// gatedTransport holds the first request until release is closed, then answers.
// It is how a test changes the selection while a read is genuinely in flight.
type gatedTransport struct {
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	status    int
	payload   string
	callsSeen int
}

func newGatedTransport(status int, payload string) *gatedTransport {
	return &gatedTransport{entered: make(chan struct{}), release: make(chan struct{}),
		status: status, payload: payload}
}

func (g *gatedTransport) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		g.callsSeen++
		g.once.Do(func() { close(g.entered) })
		<-g.release
		return &http.Response{StatusCode: g.status, Status: http.StatusText(g.status),
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(g.payload)), Request: request}, nil
	})}
}

func newCodeUsage(t *testing.T, options quota.CodeUsageOptions) *quota.CodeUsage {
	t.Helper()
	if options.Now == nil {
		options.Now = func() int64 { return 1794196800000 }
	}
	// The throttle is off unless a test asks for it, so two reads can be driven
	// in a row.
	if options.ThrottleMS == 0 {
		options.ThrottleMS = -1
	}
	return quota.NewCodeUsage(options)
}

// --- parsing ---------------------------------------------------------------

func TestKimiCodeReadingUsesTheKeyActuallySentWhenResolverRotates(t *testing.T) {
	calls := 0
	var sent string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		sent = strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(codeUsageReply))}, nil
	})}
	service := newCodeUsage(t, quota.CodeUsageOptions{HTTPClient: client, WorkKey: func() string {
		calls++
		if calls < 3 {
			return codeWorkKey
		}
		return codeLocalKey
	}})
	if result := service.Refresh(context.Background(), true); result.Status != "ok" {
		t.Fatal(result.Status)
	}
	state := service.State()
	if sent != codeLocalKey || state.AccountMasked == nil || *state.AccountMasked != quota.AccountTag(sent) {
		t.Fatal("reading identity differs from the credential actually sent")
	}
}

func TestKimiCodeReplyIsReadAsDocumentedWindowsAndAWallet(t *testing.T) {
	var document any
	if err := json.Unmarshal([]byte(codeUsageReply), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	payload, code := quota.ParseCodeUsageReply(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if len(payload.Windows) != 4 {
		t.Fatalf("windows %+v", payload.Windows)
	}
	byKey := map[string]quota.CodeUsageWindow{}
	for _, window := range payload.Windows {
		byKey[window.Key] = window
	}
	// used_ratio may be a string; the ratio is the vendor's own fraction and the
	// percentages are derived from it, never a second reading.
	if byKey["limit_5h"].UsedRatio != 0.42 || byKey["limit_5h"].UsedPercent != 42 ||
		byKey["limit_5h"].RemainingPercent != 58 {
		t.Fatalf("limit_5h %+v", byKey["limit_5h"])
	}
	if byKey["limit_7d"].UsedRatio != 0.2 || byKey["limit_7d"].RemainingPercent != 80 {
		t.Fatalf("limit_7d %+v", byKey["limit_7d"])
	}
	if byKey["limit_5h"].ResetAt == nil || *byKey["limit_5h"].ResetAt != "2026-11-16T19:01:15.240Z" {
		t.Fatalf("reset %v", byKey["limit_5h"].ResetAt)
	}
	// The month windows are two different statements: `code` is a subset of
	// `total`, and neither is ever added to another window.
	if _, present := byKey["limit_month_code"]; !present {
		t.Fatalf("the month code window was dropped")
	}
	wallet := payload.Wallet
	if wallet == nil {
		t.Fatalf("no wallet")
	}
	// The wallet's figures are in millionths of a cent, the official reader's own
	// `FIXED_POINT_CENTS`: 500000000 -> 500 cents.
	if wallet.TotalCents == nil || *wallet.TotalCents != 500 {
		t.Fatalf("total %v", wallet.TotalCents)
	}
	if wallet.BalanceCents == nil || *wallet.BalanceCents != 350 {
		t.Fatalf("balance %v", wallet.BalanceCents)
	}
	if wallet.Currency == nil || *wallet.Currency != "USD" {
		t.Fatalf("currency %v", wallet.Currency)
	}
	if wallet.MonthlyUsedCents == nil || *wallet.MonthlyUsedCents != 1200 {
		t.Fatalf("monthly used %v", wallet.MonthlyUsedCents)
	}
	if wallet.MonthlyLimitEnabled == nil || !*wallet.MonthlyLimitEnabled {
		t.Fatalf("monthly limit flag %v", wallet.MonthlyLimitEnabled)
	}
}

func TestKimiCodeReplyAcceptsTheCamelCaseAliases(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`{"usages":{"limit5h":{"usedRatio":0.5},
		"limit7d":{"usedRatio":0.1,"resetAt":"2026-11-23T05:00:00Z"}}}`), &document)
	payload, code := quota.ParseCodeUsageReply(document)
	if code != "" || len(payload.Windows) != 2 {
		t.Fatalf("payload %+v code %q", payload, code)
	}
	if payload.Windows[0].UsedPercent != 50 {
		t.Fatalf("window %+v", payload.Windows[0])
	}
}

func TestAnUnreadableKimiCodeReplyIsAFailureAndNeverZero(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"usages":{}}`,
		`{"usages":{"limit_5h":{}}}`,
		`{"usages":{"limit_5h":{"used_ratio":"not a number"}}}`,
		`{"usages":{"limit_5h":{"used_ratio":1.4}}}`,
		`[]`,
		`null`,
		`"text"`,
	} {
		var document any
		_ = json.Unmarshal([]byte(body), &document)
		payload, code := quota.ParseCodeUsageReply(document)
		if code == "" {
			t.Fatalf("%s: an unreadable reply became %+v", body, payload)
		}
		if len(payload.Windows) != 0 {
			t.Fatalf("%s: a failed parse carried windows", body)
		}
	}
}

func TestAWindowThatCannotBeReadIsNamedAndTheRestSurvive(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`{"usages":{"limit_5h":{"used_ratio":0.3},
		"limit_7d":{"used_ratio":"n/a"}}}`), &document)
	payload, code := quota.ParseCodeUsageReply(document)
	if code != "" {
		t.Fatalf("a partly readable reply was refused: %q", code)
	}
	if len(payload.Windows) != 1 || payload.Windows[0].Key != "limit_5h" {
		t.Fatalf("windows %+v", payload.Windows)
	}
	if len(payload.Notes) != 1 || payload.Notes[0] != "window_unreadable:limit_7d" {
		t.Fatalf("notes %+v", payload.Notes)
	}
}

func TestABoosterWalletWithNoAmountIsNotAZeroBalance(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`{"usages":{"limit_5h":{"used_ratio":0.3}},
		"boosterWallet":{"balance":{"type":"BOOSTER","amount":0,"amountLeft":0}}}`), &document)
	payload, code := quota.ParseCodeUsageReply(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if payload.Wallet != nil {
		t.Fatalf("an empty wallet became %+v", payload.Wallet)
	}
}

// --- sources ---------------------------------------------------------------

func TestTheThreeSourcesReportTheirOwnAvailability(t *testing.T) {
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{
		Keys:     keys,
		WorkKey:  func() string { return codeWorkKey },
		LocalKey: func() string { return codeLocalKey },
	})
	state := service.State()
	if len(state.Sources) != 3 {
		t.Fatalf("sources %+v", state.Sources)
	}
	byID := map[string]quota.CodeSourceView{}
	for _, source := range state.Sources {
		byID[source.ID] = source
	}
	if byID["manual"].Availability != quota.CodeAvailable || byID["work"].Availability != quota.CodeAvailable {
		t.Fatalf("sources %+v", state.Sources)
	}
	// Detecting the local sign-in is not adopting it.
	if byID["local"].Availability != quota.CodeUnadopted || byID["local"].Adopted {
		t.Fatalf("local %+v", byID["local"])
	}
	if byID["local"].AccountMasked == nil || !strings.HasPrefix(*byID["local"].AccountMasked, "acct-") {
		t.Fatalf("local account %v", byID["local"].AccountMasked)
	}
	for _, source := range state.Sources {
		if source.AccountMasked != nil && *source.AccountMasked == codeManualKey {
			t.Fatalf("a credential reached the state")
		}
	}
}

func TestAMissingSourceIsMissingAndNotUnavailable(t *testing.T) {
	service := newCodeUsage(t, quota.CodeUsageOptions{
		Keys:     newMemoryKeys(),
		WorkKey:  func() string { return "" },
		LocalKey: func() string { return "" },
	})
	state := service.State()
	for _, source := range state.Sources {
		if source.Availability != quota.CodeMissing {
			t.Fatalf("%s availability %q", source.ID, source.Availability)
		}
		if source.AccountMasked != nil {
			t.Fatalf("%s carries an account without a key", source.ID)
		}
	}
	if state.Status != "not_configured" {
		t.Fatalf("status %q", state.Status)
	}
}

// The Work account can change while this product runs. The key must be resolved
// again on every refresh, not once at construction.
func TestTheWorkAccountIsResolvedAgainOnEveryRefresh(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	current := codeWorkKey
	service := newCodeUsage(t, quota.CodeUsageOptions{
		Keys:       newMemoryKeys(),
		WorkKey:    func() string { return current },
		HTTPClient: recorder.client(),
	})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	if recorder.calls[0].Headers["Authorization"] != "Bearer "+codeWorkKey {
		t.Fatalf("the first read did not use the Work key")
	}
	firstAccount := service.State().AccountMasked
	current = fakeKey("kc", "work2", "test", "00001111")
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if recorder.calls[1].Headers["Authorization"] != "Bearer "+current {
		t.Fatalf("the account switch was not followed")
	}
	if service.State().AccountMasked == nil || *service.State().AccountMasked == *firstAccount {
		t.Fatalf("the card still names the old account")
	}
}

func TestTheLocalKeyIsNeverUsedWithoutAdoption(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	service := newCodeUsage(t, quota.CodeUsageOptions{
		Keys:       newMemoryKeys(),
		WorkKey:    func() string { return "" },
		LocalKey:   func() string { return codeLocalKey },
		HTTPClient: recorder.client(),
	})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("an unadopted local key was used")
	}
	if _, err := service.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "local"}); err != nil {
		t.Fatal(err)
	}
	state := service.State()
	if state.SelectedSource == nil || *state.SelectedSource != "local" {
		t.Fatalf("selected %v", state.SelectedSource)
	}
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.calls) != 1 || recorder.calls[0].Headers["Authorization"] != "Bearer "+codeLocalKey {
		t.Fatalf("the adopted local key was not used")
	}
}

// --- reads -----------------------------------------------------------------

func TestARefreshAsksTheDocumentedEndpointWithABearerAndNoQuery(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client()})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	call := recorder.calls[0]
	if call.URL != "https://api.kimi.com/coding/v1/usages" || call.Method != "GET" {
		t.Fatalf("call %+v", call)
	}
	if call.Headers["Authorization"] != "Bearer "+codeManualKey {
		t.Fatalf("authorization %q", call.Headers["Authorization"])
	}
	if call.Headers["Accept"] != "application/json" {
		t.Fatalf("accept %q", call.Headers["Accept"])
	}
	state := service.State()
	if state.Status != "ok" || len(state.Windows) != 4 || state.Wallet == nil {
		t.Fatalf("state %+v", state)
	}
	if state.ObservedAt == nil || state.ReadingSource == nil || *state.ReadingSource != "manual" {
		t.Fatalf("state %+v", state)
	}
}

func TestARefusalIsReportedWithItsReasonAndNotAsZero(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":"bad"}`, "auth_rejected"},
		{403, `{}`, "auth_rejected"},
		{404, `{}`, "usage_endpoint_unavailable"},
		{429, `{}`, "rate_limited"},
		{500, `{}`, "upstream_error"},
		{200, `{"usages":{}}`, "invalid_response"},
		{200, `not json`, "invalid_response"},
	}
	for _, entry := range cases {
		recorder := &recorder{reply: func(call call) (int, string) { return entry.status, entry.body }}
		keys := newMemoryKeys()
		keys.values["kimi-code:manual"] = codeManualKey
		service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client()})
		answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
		if err != nil {
			t.Fatalf("%d: %v", entry.status, err)
		}
		report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh)
		if report.Status != "error" || report.ErrorReason == nil || *report.ErrorReason != entry.want {
			t.Fatalf("%d %s: report %+v", entry.status, entry.body, report)
		}
		state := service.State()
		if state.Status != "error" || state.ErrorReason == nil || *state.ErrorReason != entry.want {
			t.Fatalf("%d: state %+v", entry.status, state)
		}
		// The failure is never a reading: no window, no zero.
		if len(state.Windows) != 0 || state.Wallet != nil {
			t.Fatalf("%d: a failed read produced numbers %+v", entry.status, state)
		}
	}
}

func TestAFailedRefreshKeepsTheLastGoodReadingAndMarksItStale(t *testing.T) {
	clock := int64(1794196800000)
	step := 0
	recorder := &recorder{reply: func(call call) (int, string) {
		step++
		if step == 1 {
			return 200, codeUsageReply
		}
		return 401, `{"error":"expired"}`
	}}
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client(),
		Now: codeClock(&clock), FreshnessMS: 15 * 60 * 1000})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	clock += 60 * 1000
	answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh)
	if report.Status != "stale" || *report.ErrorReason != "auth_rejected" {
		t.Fatalf("report %+v", report)
	}
	state := service.State()
	if state.Status != "stale" || !state.Stale || *state.ErrorReason != "auth_rejected" {
		t.Fatalf("state %+v", state)
	}
	// The numbers shown are the ones that were actually read.
	if len(state.Windows) != 4 || state.Windows[0].UsedPercent != 42 {
		t.Fatalf("windows %+v", state.Windows)
	}
}

func TestNoCredentialMeansNoRequestAtAll(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: newMemoryKeys(),
		WorkKey: func() string { return "" }, HTTPClient: recorder.client()})
	answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh)
	if report.Status != "not_configured" || *report.ErrorReason != "credential_missing" {
		t.Fatalf("report %+v", report)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("a request was sent without a credential")
	}
}

func TestAnOldReadingIsStaleWithoutAnyFailure(t *testing.T) {
	clock := int64(1794196800000)
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client(),
		Now: codeClock(&clock), FreshnessMS: 60 * 1000})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	clock += 2 * 60 * 1000
	state := service.State()
	if state.Status != "stale" || !state.Stale || *state.ErrorReason != "age" {
		t.Fatalf("state %+v", state)
	}
	if len(state.Windows) != 4 {
		t.Fatalf("stale numbers were dropped: %+v", state.Windows)
	}
}

func TestASecondReadInsideTheThrottleIsAnsweredRatherThanSent(t *testing.T) {
	clock := int64(1794196800000)
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client(),
		Now: codeClock(&clock), ThrottleMS: 5000})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh); report.Status != "throttled" {
		t.Fatalf("report %+v", report)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("a throttled read was sent")
	}
	// `force` is the person saying "now".
	answer, err = service.Action(context.Background(), quota.CodeActionRefresh,
		map[string]any{"force": true})
	if err != nil {
		t.Fatal(err)
	}
	if report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh); report.Status != "ok" {
		t.Fatalf("forced report %+v", report)
	}
	if len(recorder.calls) != 2 {
		t.Fatalf("a forced read was not sent")
	}
}

// --- identity --------------------------------------------------------------
//
// A reading belongs to the source and account that produced it. Nothing else is
// allowed to borrow its numbers, in either direction.

func TestAReadingFromAnotherSourceIsNotShownAsOK(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client(),
		WorkKey: func() string { return codeWorkKey }})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	if state := service.State(); state.Status != "ok" {
		t.Fatalf("the first read was not shown: %+v", state)
	}
	if _, err := service.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "work"}); err != nil {
		t.Fatal(err)
	}
	state := service.State()
	if state.Status != "not_read" {
		t.Fatalf("the manual reading answered for the Work source: %+v", state)
	}
	if len(state.Windows) != 0 || state.Wallet != nil {
		t.Fatalf("another source's numbers were shown: %+v", state)
	}
	if !hasNote(state.Notes, "reading_superseded") {
		t.Fatalf("the superseded reading was not declared: %+v", state.Notes)
	}
	if state.ReadingSource != nil || state.AccountMasked != nil {
		t.Fatalf("a superseded reading still names its origin: %+v", state)
	}
}

// The same source under another account is another reading. A failure of the new
// account must not be softened into "stale" on the old account's numbers: those
// numbers belong to somebody else.
func TestAnotherWorkAccountsNumbersAreNotShownAsThisAccountsStale(t *testing.T) {
	current := codeWorkKey
	// The recorder answers for the key that was accepted, so changing `current`
	// genuinely switches which credential the read carries.
	accepted := codeWorkKey
	recorder := &recorder{reply: func(call call) (int, string) {
		if call.Headers["Authorization"] == "Bearer "+accepted {
			return 200, codeUsageReply
		}
		return 401, `{"error":"expired"}`
	}}
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: newMemoryKeys(),
		WorkKey: func() string { return current }, HTTPClient: recorder.client()})
	if _, err := service.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	first := service.State()
	if first.Status != "ok" || len(first.Windows) != 4 {
		t.Fatalf("the first account did not read: %+v", first)
	}
	firstAccount := first.AccountMasked

	current = fakeKey("kc", "work2", "test", "00001111")
	answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := answer["lastRefresh"].(quota.CodeUsageRefresh)
	if report.Status != "error" || report.ErrorReason == nil || *report.ErrorReason != "auth_rejected" {
		t.Fatalf("the new account's failure was reported as %+v", report)
	}
	state := service.State()
	if state.Status != "error" || state.Stale {
		t.Fatalf("the previous account's numbers were kept as stale: %+v", state)
	}
	if len(state.Windows) != 0 || state.Wallet != nil {
		t.Fatalf("the previous account's numbers were shown: %+v", state)
	}
	if state.AccountMasked != nil && firstAccount != nil && *state.AccountMasked == *firstAccount {
		t.Fatalf("the card still names the previous account")
	}
	if !hasNote(state.Notes, "reading_superseded") {
		t.Fatalf("the superseded reading was not declared: %+v", state.Notes)
	}
}

// A selection can change while a request is on the wire. The reply is still
// recorded against the identity that asked for it, but it is not the answer to
// what is selected now, so neither the numbers nor an "ok" may be reported.
func TestASelectionChangedDuringAReadIsReportedAsSuperseded(t *testing.T) {
	for _, name := range []string{"a successful reply", "a refused reply"} {
		status, payload := 200, codeUsageReply
		if name == "a refused reply" {
			status, payload = 401, `{"error":"expired"}`
		}
		t.Run(name, func(t *testing.T) {
			gate := newGatedTransport(status, payload)
			keys := newMemoryKeys()
			keys.values["kimi-code:manual"] = codeManualKey
			service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys,
				WorkKey: func() string { return codeWorkKey }, HTTPClient: gate.client()})

			results := make(chan quota.CodeUsageRefresh, 1)
			go func() { results <- service.Refresh(context.Background(), false) }()
			<-gate.entered

			if _, err := service.Action(context.Background(), quota.CodeActionSelect,
				map[string]any{"source": "work"}); err != nil {
				t.Fatal(err)
			}
			close(gate.release)

			report := <-results
			if report.Status != "superseded" || report.ErrorReason == nil ||
				*report.ErrorReason != "selection_changed" {
				t.Fatalf("an in-flight read of another source was reported as %+v", report)
			}
			state := service.State()
			if state.Status == "ok" {
				t.Fatalf("the superseded read answered as ok: %+v", state)
			}
			if len(state.Windows) != 0 || state.Wallet != nil {
				t.Fatalf("the superseded read's numbers were shown: %+v", state)
			}
			// Only a reply that produced numbers leaves a reading to declare
			// superseded; a refused one never became a reading at all.
			if status == 200 && !hasNote(state.Notes, "reading_superseded") {
				t.Fatalf("the superseded reading was not declared: %+v", state.Notes)
			}
		})
	}
}

// --- the hand-typed key ----------------------------------------------------

func TestAHandTypedKeyGoesOnlyToTheStoreAndIsVerifiedByReadBack(t *testing.T) {
	keys := newMemoryKeys()
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys})
	if _, err := service.Action(context.Background(), quota.CodeActionSaveManual,
		map[string]any{"key": codeManualKey}); err != nil {
		t.Fatal(err)
	}
	if keys.values["kimi-code:manual"] != codeManualKey {
		t.Fatalf("the key was not stored under its own reference")
	}
	if state := service.State(); state.SelectedSource == nil || *state.SelectedSource != "manual" {
		t.Fatalf("saving did not select the manual source")
	}
}

// A store that accepts a write but answers with something else would fail every
// later request on the new reference. The replacement is refused and the key that
// was already working is put back; it must never be deleted by a failed save.
func TestAFailedReplacementPutsTheWorkingKeyBack(t *testing.T) {
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys})
	keys.mangleNext = func(value string) string { return value + "-truncated" }
	_, err := service.Action(context.Background(), quota.CodeActionSaveManual,
		map[string]any{"key": fakeKey("kc", "second", "test", "99999999")})
	if err == nil {
		t.Fatalf("a mangled write was accepted")
	}
	var refusal *quota.CodeUsageError
	if !errors.As(err, &refusal) || refusal.Code != "credential_readback_failed" {
		t.Fatalf("error %v", err)
	}
	if got := keys.values["kimi-code:manual"]; got != codeManualKey {
		t.Fatalf("the previous key was not restored: %q", got)
	}
	// The card must still name the account that key belongs to.
	state := service.State()
	if state.Sources[0].Availability != quota.CodeAvailable {
		t.Fatalf("the restored key is not reported available: %+v", state.Sources[0])
	}
	if state.Sources[0].AccountMasked == nil ||
		*state.Sources[0].AccountMasked != quota.AccountTag(codeManualKey) {
		t.Fatalf("the account tag is %v", state.Sources[0].AccountMasked)
	}
}

// A failed save must not delete a key that was never there either; that is the
// same rollback path with nothing to restore.
func TestAFailedFirstSaveLeavesNoValueBehind(t *testing.T) {
	keys := newMemoryKeys()
	keys.mangleNext = func(value string) string { return value + "-truncated" }
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys})
	if _, err := service.Action(context.Background(), quota.CodeActionSaveManual,
		map[string]any{"key": codeManualKey}); err == nil {
		t.Fatalf("a mangled write was accepted")
	}
	if _, present := keys.values["kimi-code:manual"]; present {
		t.Fatalf("a failed first save left a value in the store")
	}
	if service.State().SelectedSource != nil {
		t.Fatalf("a failed save changed the selection")
	}
}

func TestAnUnusableManualKeyIsRefusedBeforeItReachesTheStore(t *testing.T) {
	keys := newMemoryKeys()
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys})
	for _, body := range []map[string]any{
		{"key": ""},
		{"key": "   "},
		{"key": "with space"},
		{"key": strings.Repeat("a", 5000)},
		{"key": "line\nbreak"},
		{},
	} {
		if _, err := service.Action(context.Background(), quota.CodeActionSaveManual, body); err == nil {
			t.Fatalf("%v was accepted", body["key"])
		}
	}
	if len(keys.values) != 0 {
		t.Fatalf("a refused key reached the store")
	}
}

func TestClearingTheManualKeyForgetsItAndFallsBack(t *testing.T) {
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys,
		WorkKey: func() string { return codeWorkKey }})
	if _, err := service.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Action(context.Background(), quota.CodeActionClearManual, nil); err != nil {
		t.Fatal(err)
	}
	if _, present := keys.values["kimi-code:manual"]; present {
		t.Fatalf("the key survived the clear")
	}
	state := service.State()
	if state.SelectedSource == nil || *state.SelectedSource != "work" {
		t.Fatalf("the fallback source is %v", state.SelectedSource)
	}
}

func TestAnUnknownSourceIsRefused(t *testing.T) {
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: newMemoryKeys()})
	if _, err := service.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "somewhere-else"}); err == nil {
		t.Fatalf("an unknown source was accepted")
	}
	if _, err := service.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "local"}); err == nil {
		t.Fatalf("an absent local key was adopted")
	}
}

// --- the selection, and only the selection, is persisted --------------------

func TestTheChoiceSurvivesARestartAndNoSecretOrNumberDoes(t *testing.T) {
	dir := t.TempDir()
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	recorder := &recorder{reply: func(call call) (int, string) { return 200, codeUsageReply }}
	first := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, StateDir: dir,
		HTTPClient: recorder.client()})
	if _, err := first.Action(context.Background(), quota.CodeActionSelect,
		map[string]any{"source": "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Action(context.Background(), quota.CodeActionRefresh, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "code-usage.json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	for _, secret := range []string{codeManualKey, "usedRatio", "usedPercent", "42", "1250"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the state file carries %q: %s", secret, raw)
		}
	}
	second := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, StateDir: dir})
	state := second.State()
	if state.SelectedSource == nil || *state.SelectedSource != "manual" {
		t.Fatalf("the choice did not survive: %+v", state.SelectedSource)
	}
	// A number read before the restart is not shown as if it had just been read.
	if state.Status != "not_read" || len(state.Windows) != 0 {
		t.Fatalf("a cached number was replayed: %+v", state)
	}
}

func TestAnUnreadableStateFileIsDroppedRatherThanTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code-usage.json")
	for _, body := range []string{`{"schemaVersion":99,"selected":"manual"}`,
		`{"schemaVersion":1,"selected":"somewhere-else"}`,
		`not json`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		service := newCodeUsage(t, quota.CodeUsageOptions{Keys: newMemoryKeys(), StateDir: dir})
		state := service.State()
		if state.SelectedSource != nil && *state.SelectedSource == "manual" {
			t.Fatalf("%s: a hand-edited choice was trusted", body)
		}
		// An unrecognised value is refused; a merely unknown schema is what makes
		// the store state itself unusable. Both leave the selection empty.
		if strings.Contains(body, `"schemaVersion":99`) && state.Cache.Error == nil {
			t.Fatalf("%s: an unknown schema was not reported", body)
		}
	}
}

// --- masking ---------------------------------------------------------------

func TestTheCredentialNeverAppearsInAnyAnswer(t *testing.T) {
	keys := newMemoryKeys()
	keys.values["kimi-code:manual"] = codeManualKey
	recorder := &recorder{reply: func(call call) (int, string) { return 401, `{"error":"bad key"}` }}
	service := newCodeUsage(t, quota.CodeUsageOptions{Keys: keys, HTTPClient: recorder.client(),
		WorkKey:  func() string { return codeWorkKey },
		LocalKey: func() string { return codeLocalKey }})
	answer, err := service.Action(context.Background(), quota.CodeActionRefresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{codeManualKey, codeWorkKey, codeLocalKey} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("a credential reached the answer: %s", encoded)
		}
	}
}

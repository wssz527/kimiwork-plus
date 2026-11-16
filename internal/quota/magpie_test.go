package quota_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kimiworkplus.local/migration/internal/quota"
)

// The fixtures below are trimmed copies of real replies from a running Magpie
// 0.1.3960 on this machine, with account names replaced. They are kept because
// the shape has four variants that a single invented sample would miss:
//
//	windows with only used/remaining      the majority of subscription entries
//	windows with amount/limit/unit        a vendor's absolute credits
//	windows with only a display string    a pre-rendered "Unknown" or "500 / 500"
//	kind "balance" with a formatted string such as "$19.18"
//
// Regenerate with: magpie quota --json
func loadMagpieFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "magpie-quotas.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func TestMagpieQuotasAreReadFromTheDocumentedReply(t *testing.T) {
	var document any
	if err := json.Unmarshal([]byte(loadMagpieFixture(t)), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if len(snapshot.Entries) != 4 {
		t.Fatalf("entries %d", len(snapshot.Entries))
	}
	byProvider := map[string]quota.MagpieEntry{}
	for _, entry := range snapshot.Entries {
		byProvider[entry.Provider] = entry
	}

	// A plain subscription window carries percentages, and the basis says so.
	kimi := byProvider["kimi-for-coding"]
	if kimi.Kind != "subscription" || len(kimi.Windows) != 2 {
		t.Fatalf("kimi entry %+v", kimi)
	}
	weekly := kimi.Windows[0]
	if weekly.Name != "Weekly" || weekly.Basis != quota.MagpieBasisPercent {
		t.Fatalf("weekly %+v", weekly)
	}
	if weekly.Used == nil || *weekly.Used != 20 || weekly.Remaining == nil || *weekly.Remaining != 80 {
		t.Fatalf("weekly figures %+v", weekly)
	}
	if weekly.ResetAt == nil || *weekly.ResetAt != "2026-11-23T05:01:15.240Z" {
		t.Fatalf("weekly reset %v", weekly.ResetAt)
	}

	// A window with absolute figures uses them, and a percentage is not money.
	codearts := byProvider["codearts"]
	if len(codearts.Windows) != 3 {
		t.Fatalf("codearts windows %+v", codearts.Windows)
	}
	total := codearts.Windows[0]
	if total.Basis != quota.MagpieBasisAmount {
		t.Fatalf("total basis %q", total.Basis)
	}
	if total.Amount == nil || *total.Amount != 5500 || total.Limit == nil || *total.Limit != 5600 {
		t.Fatalf("total figures %+v", total)
	}
	if total.Unit == nil || *total.Unit != "credits" {
		t.Fatalf("total unit %v", total.Unit)
	}
	if total.Used != nil {
		t.Fatalf("an absolute window also carried a percentage: %+v", total)
	}
	if total.Display == nil || *total.Display != "5500 / 5600 · 100 即将过期" {
		t.Fatalf("display %v", total.Display)
	}
	// The reset time falls back to runsOutAt when the entry has no resetsAt.
	if total.ResetAt == nil {
		t.Fatalf("a reset was dropped")
	}

	// A balance entry keeps the vendor's own string, because the currency may be
	// one this package does not know.
	bye := byProvider["bye-gpt"]
	if bye.Kind != "balance" || bye.Balance == nil || *bye.Balance != "$19.18" {
		t.Fatalf("balance entry %+v", bye)
	}
	if bye.BalanceCurrency == nil || *bye.BalanceCurrency != "USD" ||
		bye.BalanceAmount == nil || *bye.BalanceAmount != 19.18 {
		t.Fatalf("balance parse %+v %+v", bye.BalanceCurrency, bye.BalanceAmount)
	}
}

func TestAWindowWithOnlyADisplayStringIsKeptAsTextAndNotAsZero(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`[{"provider":"codex","name":"Codex","kind":"subscription",
		"windows":[{"name":"Image generation","used":0,"remaining":100,"display":"Unknown",
		"text":true,"lane":"images"}]}]`), &document)
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	window := snapshot.Entries[0].Windows[0]
	if window.Basis != quota.MagpieBasisPercent {
		t.Fatalf("a window with percentages is not text: %+v", window)
	}
	if window.Display == nil || *window.Display != "Unknown" {
		t.Fatalf("display %v", window.Display)
	}
}

func TestATextOnlyWindowReportsNoNumberAtAll(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`[{"provider":"x","name":"X","kind":"subscription",
		"windows":[{"name":"Credits","display":"500 / 500"}]}]`), &document)
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	window := snapshot.Entries[0].Windows[0]
	if window.Basis != quota.MagpieBasisText {
		t.Fatalf("basis %q", window.Basis)
	}
	if window.Used != nil || window.Remaining != nil || window.Amount != nil || window.Limit != nil {
		t.Fatalf("a text-only window invented a number: %+v", window)
	}
}

func TestAnUnreadableMagpieReplyIsFailureAndAnEmptyListIsNot(t *testing.T) {
	for _, body := range []string{`null`, `"text"`, `{}`, `{"data":{}}`, `{"data":[1,2,3]}`} {
		var document any
		_ = json.Unmarshal([]byte(body), &document)
		if snapshot, code := quota.ParseMagpieQuotas(document); code == "" {
			t.Fatalf("%s: an unreadable reply became %+v", body, snapshot)
		}
	}
	// Nothing configured is a real answer, distinct from a reply that could not
	// be read.
	var document any
	_ = json.Unmarshal([]byte(`[]`), &document)
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" || len(snapshot.Entries) != 0 {
		t.Fatalf("empty list %+v code %q", snapshot, code)
	}
	var wrapped any
	_ = json.Unmarshal([]byte(`{"data":[]}`), &wrapped)
	if _, code := quota.ParseMagpieQuotas(wrapped); code != "" {
		t.Fatalf("wrapped empty list code %q", code)
	}
}

func TestTheHTTPEnvelopeIsAcceptedBecauseTheGatewayWrapsTheArray(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`{"data":[{"provider":"kimi-for-coding","name":"Kimi Code",
		"kind":"subscription","windows":[{"name":"Weekly","used":20,"remaining":80}]}]}`), &document)
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" || len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot %+v code %q", snapshot, code)
	}
}

func TestABalanceStringThisPackageDoesNotParseKeepsTheTextAndSaysSo(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(`[{"provider":"odd","name":"Odd","kind":"balance","balance":"1234 tokens"}]`), &document)
	snapshot, code := quota.ParseMagpieQuotas(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	entry := snapshot.Entries[0]
	if entry.Balance == nil || *entry.Balance != "1234 tokens" {
		t.Fatalf("balance %v", entry.Balance)
	}
	if entry.BalanceAmount != nil {
		t.Fatalf("an unparsed balance became a number: %v", entry.BalanceAmount)
	}
	if len(snapshot.Notes) != 1 || snapshot.Notes[0] != "balance_unparsed:odd" {
		t.Fatalf("notes %+v", snapshot.Notes)
	}
}

// --- reading ---------------------------------------------------------------

func magpieTransport(t *testing.T, fixture string) *recorder {
	t.Helper()
	return &recorder{reply: func(call call) (int, string) { return 200, fixture }}
}

func TestARefreshAsksTheDocumentedPathOnTheConfiguredGateway(t *testing.T) {
	recorder := magpieTransport(t, loadMagpieFixture(t))
	reader, err := quota.NewMagpie(quota.MagpieOptions{BaseURL: "http://127.0.0.1:3430",
		HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	call := recorder.calls[0]
	if call.URL != "http://127.0.0.1:3430/v1/magpie/quotas" || call.Method != "GET" {
		t.Fatalf("call %+v", call)
	}
	// No key configured means no key header: a local Magpie answers without one.
	if call.Headers["Authorization"] != "" {
		t.Fatalf("an unconfigured key was sent")
	}
	if snapshot.FetchedAt == nil || len(snapshot.Entries) != 4 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	state := reader.State()
	if state.Status != "ok" || state.Snapshot == nil {
		t.Fatalf("state %+v", state)
	}
}

func TestASharedGatewayKeyIsSentBothWaysMagpieAccepts(t *testing.T) {
	recorder := magpieTransport(t, loadMagpieFixture(t))
	shared := fakeKey("gw", "magpie", "test", "aabbccdd")
	reader, err := quota.NewMagpie(quota.MagpieOptions{APIKey: shared, HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.calls[0].Headers["Authorization"] != "Bearer "+shared {
		t.Fatalf("authorization %q", recorder.calls[0].Headers["Authorization"])
	}
	// The recorder keys headers by their canonical form; HTTP header names are
	// case-insensitive, so the server sees the same field either way.
	if recorder.calls[0].Headers["X-Api-Key"] != shared {
		t.Fatalf("x-api-key %q", recorder.calls[0].Headers["X-Api-Key"])
	}
	// The key is never in an answer.
	encoded, _ := json.Marshal(reader.State())
	if strings.Contains(string(encoded), shared) {
		t.Fatalf("the shared key reached the state: %s", encoded)
	}
}

func TestAMagpieThatIsNotRunningIsReportedAsUnreachable(t *testing.T) {
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: deadTransport{}.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err == nil {
		t.Fatalf("a dead gateway answered")
	}
	state := reader.State()
	if state.Status != "error" || state.ErrorReason == nil || *state.ErrorReason != "magpie_unreachable" {
		t.Fatalf("state %+v", state)
	}
	if state.Snapshot != nil {
		t.Fatalf("an unreachable gateway produced numbers")
	}
}

func TestARejectedKeyIsItsOwnReason(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 401, `{"error":"denied"}` }}
	reader, err := quota.NewMagpie(quota.MagpieOptions{APIKey: "bad", HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err == nil {
		t.Fatalf("a rejected key answered")
	}
	state := reader.State()
	if state.ErrorReason == nil || *state.ErrorReason != "magpie_key_rejected" {
		t.Fatalf("state %+v", state)
	}
}

func TestAFailedReadAfterAGoodOneKeepsTheNumbersAndMarksThemStale(t *testing.T) {
	step := 0
	recorder := &recorder{reply: func(call call) (int, string) {
		step++
		if step == 1 {
			return 200, loadMagpieFixture(t)
		}
		return 500, `{}`
	}}
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err == nil {
		t.Fatalf("the failed read answered")
	}
	state := reader.State()
	if state.Status != "stale" || !state.Stale || *state.ErrorReason != "magpie_error" {
		t.Fatalf("state %+v", state)
	}
	if state.Snapshot == nil || len(state.Snapshot.Entries) != 4 {
		t.Fatalf("the last good reading was dropped")
	}
}

func TestAnAddressWithACredentialInItIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, base := range []string{
		"http://user:pass@127.0.0.1:3425",
		"https://magpie.example.com/v1/magpie",
		"http://example.com:3425",
		"file:///tmp/magpie",
		"not a url",
		"http://127.0.0.1:3425/?x=1",
	} {
		if _, err := quota.NewMagpie(quota.MagpieOptions{BaseURL: base}); err == nil {
			t.Fatalf("%s was accepted", base)
		}
	}
	// Loopback over plain HTTP is the documented local form, and a LAN https
	// gateway is the documented shared form.
	for _, base := range []string{"http://127.0.0.1:3425", "http://localhost:3425",
		"https://magpie.example.com"} {
		if _, err := quota.NewMagpie(quota.MagpieOptions{BaseURL: base}); err != nil {
			t.Fatalf("%s was refused: %v", base, err)
		}
	}
}

func TestTheAddressCanBeChangedAndIsValidatedFirst(t *testing.T) {
	recorder := magpieTransport(t, loadMagpieFixture(t))
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := reader.Action(context.Background(), quota.MagpieActionAddress,
		map[string]any{"baseUrl": "http://127.0.0.1:3430"})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := answer["state"].(quota.MagpieState)
	if state.BaseURL != "http://127.0.0.1:3430" {
		t.Fatalf("base url %q", state.BaseURL)
	}
	if _, err := reader.Action(context.Background(), quota.MagpieActionAddress,
		map[string]any{"baseUrl": "http://example.com:3425"}); err == nil {
		t.Fatalf("a remote plain-http gateway was accepted")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("changing the address made a request")
	}
}

func TestAnUnreachableMagpieAnswersWithItsReasonRatherThanAnHTTPError(t *testing.T) {
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: deadTransport{}.client()})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := reader.Action(context.Background(), quota.MagpieActionRefresh, nil)
	if err != nil {
		t.Fatalf("a read failure was raised as an action error: %v", err)
	}
	last, _ := answer["lastRefresh"].(map[string]any)
	if last["errorReason"] != "magpie_unreachable" {
		t.Fatalf("lastRefresh %+v", last)
	}
	state, _ := answer["state"].(quota.MagpieState)
	if state.Status != "error" {
		t.Fatalf("state %+v", state)
	}
}

func TestTheStateMakesNoRequest(t *testing.T) {
	recorder := magpieTransport(t, loadMagpieFixture(t))
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		reader.State()
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("State made a request")
	}
	if reader.State().Status != "not_read" {
		t.Fatalf("status before a read is %q", reader.State().Status)
	}
}

// A repointed reader must not answer with the previous gateway's balances: those
// numbers describe a different Magpie, and a card that says "ok" while showing
// them is wrong in the way that matters.
func TestRepointingTheGatewayDropsTheReadingItNoLongerDescribes(t *testing.T) {
	recorder := magpieTransport(t, loadMagpieFixture(t))
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := reader.State(); state.Status != "ok" || state.Snapshot == nil {
		t.Fatalf("the first read was not shown: %+v", state)
	}
	if _, err := reader.Action(context.Background(), quota.MagpieActionAddress,
		map[string]any{"baseUrl": "http://127.0.0.1:3430"}); err != nil {
		t.Fatal(err)
	}
	state := reader.State()
	if state.Status == "ok" || state.Stale {
		t.Fatalf("the previous gateway's balances still answer for this one: %+v", state)
	}
	if state.Snapshot != nil {
		t.Fatalf("the previous gateway's snapshot was kept")
	}
	if state.Status != "not_read" {
		t.Fatalf("status after a repoint is %q", state.Status)
	}
	if !hasNote(state.Notes, "reading_from_another_address") {
		t.Fatalf("the stale-by-address reading was not declared: %+v", state.Notes)
	}
	// The new address is readable on its own terms.
	if _, err := reader.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := recorder.calls[len(recorder.calls)-1].URL,
		"http://127.0.0.1:3430/v1/magpie/quotas"; got != want {
		t.Fatalf("the read went to %q", got)
	}
	if state := reader.State(); state.Status != "ok" || state.Snapshot == nil {
		t.Fatalf("the new gateway's read was not shown: %+v", state)
	}
}

// A failure recorded against the old address must not be reported as a failure of
// the new one either: "not read yet" is the truth.
func TestAFailureFromThePreviousGatewayIsNotChargedToTheNewOne(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 500, `{}` }}
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Refresh(context.Background()); err == nil {
		t.Fatalf("the failing read answered")
	}
	if state := reader.State(); state.Status != "error" {
		t.Fatalf("state %+v", state)
	}
	if err := reader.SetBaseURL("http://127.0.0.1:3430"); err != nil {
		t.Fatal(err)
	}
	state := reader.State()
	if state.Status != "not_read" || state.ErrorReason != nil {
		t.Fatalf("the old gateway's failure is charged to the new one: %+v", state)
	}
	if !hasNote(state.Notes, "reading_from_another_address") {
		t.Fatalf("the discarded failure was not declared: %+v", state.Notes)
	}
}

// Two reads in flight must not land out of order: the refresh lock is what makes
// the older reply unable to overwrite the newer one.
func TestConcurrentReadsCannotOverwriteANewerReplyWithAnOlderOne(t *testing.T) {
	var mu sync.Mutex
	first := true
	recorder := &recorder{reply: func(call call) (int, string) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			// The read that started first answers last, and reports one balance.
			time.Sleep(30 * time.Millisecond)
			return 200, `[{"provider":"older","kind":"balance","balance":"$1.00"}]`
		}
		return 200, `[{"provider":"newer","kind":"balance","balance":"$2.00"}]`
	}}
	reader, err := quota.NewMagpie(quota.MagpieOptions{HTTPClient: recorder.client()})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); _, _ = reader.Refresh(context.Background()) }()
	go func() { defer wait.Done(); _, _ = reader.Refresh(context.Background()) }()
	wait.Wait()
	state := reader.State()
	if state.Snapshot == nil || len(state.Snapshot.Entries) != 1 {
		t.Fatalf("state %+v", state)
	}
	// Whichever ran second is the installed reply; the point is that exactly one
	// reply is installed and the address tag matches the current address.
	if state.Snapshot.Entries[0].Provider != "newer" {
		t.Fatalf("the older reply overwrote the newer one: %+v", state.Snapshot.Entries[0])
	}
	if state.Status != "ok" {
		t.Fatalf("state %+v", state)
	}
}

// deadTransport is a gateway that is not listening.
type deadTransport struct{}

func (d deadTransport) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, os.ErrDeadlineExceeded
	})}
}

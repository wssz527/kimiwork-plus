package quota_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wssz527/kimi-work-plus/internal/quota"
)

// Every fixture below is the documented response shape, trimmed to the fields
// the adapter reads. No request leaves this file: the transport is a recorder,
// and the only credentials are fakes that must never appear in any output.
func fakeKey(parts ...string) string { return strings.Join(parts, "-") }

var (
	deepSeekKey = fakeKey("sk", "deepseek", "test", "a1b2c3d4")
	moonshotKey = fakeKey("sk", "moonshot", "test", "5e6f7a8b")
	customKey   = fakeKey("gw", "relay", "test", "9c0d1e2f")
)

type call struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

type recorder struct {
	calls []call
	reply func(call call) (int, string)
}

func (r *recorder) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		headers := map[string]string{}
		for name := range request.Header {
			headers[name] = request.Header.Get(name)
		}
		body := []byte{}
		if request.Body != nil {
			body, _ = io.ReadAll(request.Body)
		}
		entry := call{URL: request.URL.String(), Method: request.Method, Headers: headers, Body: string(body)}
		r.calls = append(r.calls, entry)
		status, payload := 200, "{}"
		if r.reply != nil {
			status, payload = r.reply(entry)
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status),
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func deepSeekBalance() string {
	return `{"is_available":true,"balance_infos":[
		{"currency":"USD","total_balance":"0.00","granted_balance":"0.00","topped_up_balance":"0.00"},
		{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`
}

func moonshotBalance() string {
	return `{"code":0,"data":{"available_balance":49.58894,"voucher_balance":46.58893,"cash_balance":3.00001},"scode":"0x0","status":true}`
}

func manager(t *testing.T, options quota.Options) *quota.Accounts {
	t.Helper()
	if options.Now == nil {
		options.Now = func() int64 { return 1794196800000 }
	}
	if options.FreshnessMS == 0 {
		options.FreshnessMS = 15 * 60 * 1000
	}
	// The throttle is off by default so a test can drive two reads in a row; a
	// test that wants it sets a positive value.
	if options.ThrottleMS == 0 {
		options.ThrottleMS = -1
	}
	return quota.New(options)
}

// --- amounts ---------------------------------------------------------------

func TestAmountsAreReadFromStringsAndMissingIsNull(t *testing.T) {
	cases := []struct {
		in   any
		want *float64
	}{
		{"110.00", ptr(110)},
		{float64(110), ptr(110)},
		{"  3.00001 ", ptr(3.00001)},
		{"abc", nil},
		{"", nil},
		{nil, nil},
		{"1e9", nil},
		{map[string]any{"total": 1}, nil},
	}
	for _, test := range cases {
		got := quota.ToAmount(test.in)
		if test.want == nil {
			if got != nil {
				t.Fatalf("%v: got %v want nil", test.in, *got)
			}
			continue
		}
		if got == nil || *got != *test.want {
			t.Fatalf("%v: got %v want %v", test.in, got, *test.want)
		}
	}
}

func ptr(value float64) *float64 { return &value }

func TestResetTimesAreEpochOrISOInstants(t *testing.T) {
	if got := quota.ToTimestamp(float64(1767225600)); got == nil || *got != 1767225600000 {
		t.Fatalf("seconds: %v", got)
	}
	if got := quota.ToTimestamp(float64(1767225600000)); got == nil || *got != 1767225600000 {
		t.Fatalf("milliseconds: %v", got)
	}
	if got := quota.ToTimestamp("2026-11-10T00:00:00Z"); got == nil || *got != 1794268800000 {
		t.Fatalf("iso: %v", got)
	}
	for _, value := range []any{"2026-11-10", "3pm", ""} {
		if got := quota.ToTimestamp(value); got != nil {
			t.Fatalf("%v: a guessed instant %v", value, *got)
		}
	}
}

// --- vendor parsers --------------------------------------------------------

func TestDeepSeekReplyBecomesOneRecordPerCurrency(t *testing.T) {
	var document any
	if err := json.Unmarshal([]byte(deepSeekBalance()), &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parsed, code := quota.ParseDeepSeekBalance(document)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if parsed.Available == nil || !*parsed.Available {
		t.Fatalf("available %v", parsed.Available)
	}
	// The currency with money left comes first, so a card does not reorder
	// itself between two reads.
	if len(parsed.Records) != 2 || *parsed.Records[0].Unit != "CNY" || *parsed.Records[1].Unit != "USD" {
		t.Fatalf("records %+v", parsed.Records)
	}
	if *parsed.Records[0].Remaining != 110 || *parsed.Records[1].Remaining != 0 {
		t.Fatalf("remaining %v %v", *parsed.Records[0].Remaining, *parsed.Records[1].Remaining)
	}
	// A zero balance is a real 0 and stays 0.
	if len(parsed.Parts) != 2 || parsed.Parts[0].Label != "赠金" || parsed.Parts[0].Amount != 10 ||
		parsed.Parts[1].Label != "充值" || parsed.Parts[1].Amount != 100 {
		t.Fatalf("parts %+v", parsed.Parts)
	}
}

func TestDeepSeekWithoutAReadableAmountIsAFailure(t *testing.T) {
	for _, body := range []string{
		`{"is_available":true,"balance_infos":[]}`,
		`{"is_available":true}`,
		`{"balance_infos":[{"currency":"CNY","total_balance":"n/a"}]}`,
		`null`,
	} {
		var document any
		_ = json.Unmarshal([]byte(body), &document)
		if _, code := quota.ParseDeepSeekBalance(document); code == "" {
			t.Fatalf("%s: an unreadable reply became a balance", body)
		}
	}
	var document any
	_ = json.Unmarshal([]byte(`{"balance_infos":[{"currency":"CNY","total_balance":"5.00"}]}`), &document)
	parsed, code := quota.ParseDeepSeekBalance(document)
	if code != "" || *parsed.Records[0].Remaining != 5 {
		t.Fatalf("single currency %+v", parsed)
	}
}

func TestMoonshotReplyReadsTheAvailableBalance(t *testing.T) {
	var document any
	_ = json.Unmarshal([]byte(moonshotBalance()), &document)
	parsed, code := quota.ParseMoonshotBalance(document)
	if code != "" || *parsed.Records[0].Remaining != 49.58894 {
		t.Fatalf("parsed %+v code %q", parsed, code)
	}
	if len(parsed.Parts) != 2 || parsed.Parts[0].Label != "代金券" || parsed.Parts[1].Label != "现金" {
		t.Fatalf("parts %+v", parsed.Parts)
	}
	// `code` other than 0 is the platform's own refusal, and its words are not
	// read.
	for _, body := range []string{`{"code":401,"status":false}`, `{"code":0,"data":{}}`} {
		var value any
		_ = json.Unmarshal([]byte(body), &value)
		if _, code := quota.ParseMoonshotBalance(value); code == "" {
			t.Fatalf("%s: a refusal became a balance", body)
		}
	}
}

// --- refresh ---------------------------------------------------------------

func TestRefreshAsksTheDocumentedEndpointWithTheResolvedKey(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(ref string) string {
		if ref == "deepseek:default" {
			return deepSeekKey
		}
		return ""
	}, HTTPClient: recorder.client()})
	if _, err := accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	before := accounts.State()
	if before.Cards[0].Status != "not_read" {
		t.Fatalf("status before a read %q", before.Cards[0].Status)
	}
	if before.Cards[0].Records[0].Remaining != nil {
		t.Fatalf("an unread card carries a number: %+v", before.Cards[0].Records[0])
	}

	report := accounts.Refresh(context.Background(), nil, false)
	if len(report.Results) != 1 || report.Results[0].Status != "ok" {
		t.Fatalf("results %+v", report.Results)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	if recorder.calls[0].URL != quota.DeepSeekEndpoint || recorder.calls[0].Method != "GET" {
		t.Fatalf("call %+v", recorder.calls[0])
	}
	if recorder.calls[0].Headers["Authorization"] != "Bearer "+deepSeekKey {
		t.Fatalf("auth %q", recorder.calls[0].Headers["Authorization"])
	}

	state := accounts.State()
	card := state.Cards[0]
	if card.Status != "ok" || card.Stale {
		t.Fatalf("card %+v", card)
	}
	if len(card.Records) != 2 || *card.Records[0].Remaining != 110 || card.Records[0].Kind != "api_balance" {
		t.Fatalf("records %+v", card.Records)
	}
	if card.ObservedAt == nil || !strings.HasPrefix(*card.ObservedAt, "20") {
		t.Fatalf("observed at %v", card.ObservedAt)
	}
	// Different currencies are separate records with their own unit, and there
	// is no combined figure anywhere in the payload.
	if state.Combining != "never" || len(state.Records) != 2 {
		t.Fatalf("state %+v", state)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), deepSeekKey) || strings.Contains(string(encoded), "Bearer") {
		t.Fatalf("state leaked a credential: %s", encoded)
	}
}

func TestCustomWindowIsItsOwnRecordWithLimitUsedAndReset(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) {
		return 200, `{"data":{"quota":{"limit":500000,"used":125000,"resetAt":1767225600}}}`
	}}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client()})
	if _, err := accounts.AddSource(map[string]any{
		"adapter": "custom", "sourceId": "gateway-plan", "providerId": "relay",
		"sourceName": "自建网关 · Coding Plan", "kind": "coding_plan", "unit": "tokens",
		"endpoint": "https://relay.example.test/api/quota", "windowLabel": "5h",
		"fields": map[string]any{"limit": "data.quota.limit", "used": "data.quota.used",
			"resetAt": "data.quota.resetAt"},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	accounts.Refresh(context.Background(), nil, false)
	rows := accounts.State().Records
	if len(rows) != 1 {
		t.Fatalf("records %+v", rows)
	}
	row := rows[0]
	if row.Kind != "coding_plan" || row.WindowLabel == nil || *row.WindowLabel != "5h" ||
		row.Unit == nil || *row.Unit != "tokens" {
		t.Fatalf("row %+v", row)
	}
	if *row.Limit != 500000 || *row.Used != 125000 || *row.Remaining != 375000 {
		t.Fatalf("amounts %+v", row)
	}
	if row.ResetAt == nil || *row.ResetAt != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("reset at %v", row.ResetAt)
	}
	if row.Status != "ok" || row.Stale {
		t.Fatalf("status %+v", row)
	}
}

func TestAWindowWithOnlyUsedReportsNoRemaining(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, `{"data":{"used":42}}` }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client()})
	if _, err := accounts.AddSource(map[string]any{"adapter": "custom", "sourceId": "spent-only",
		"kind": "gateway_limit", "unit": "requests", "endpoint": "https://relay.example.test/usage",
		"fields": map[string]any{"used": "data.used"}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	accounts.Refresh(context.Background(), nil, false)
	row := accounts.State().Records[0]
	if row.Used == nil || *row.Used != 42 {
		t.Fatalf("used %+v", row)
	}
	if row.Remaining != nil || row.Limit != nil {
		t.Fatalf("a figure was invented: %+v", row)
	}
}

func TestFailedRefreshKeepsTheLastGoodReadingAndMarksItStale(t *testing.T) {
	failing := false
	recorder := &recorder{reply: func(call call) (int, string) {
		if failing {
			return 500, `{"error":{"message":"internal"}}`
		}
		return 200, deepSeekBalance()
	}}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.Refresh(context.Background(), nil, false)
	good := accounts.State().Cards[0]
	if good.Status != "ok" {
		t.Fatalf("first read %q", good.Status)
	}
	observedAt := *good.ObservedAt

	failing = true
	report := accounts.Refresh(context.Background(), nil, false)
	if report.Results[0].Status != "stale" || *report.Results[0].ErrorReason != "upstream_error" {
		t.Fatalf("second read %+v", report.Results[0])
	}
	card := accounts.State().Cards[0]
	if card.Status != "stale" || !card.Stale || card.StaleReason == nil || *card.StaleReason != "refresh_failed" {
		t.Fatalf("card %+v", card)
	}
	// The numbers are the ones that were read, with their own timestamp.
	if *card.Records[0].Remaining != 110 || *card.ObservedAt != observedAt ||
		*card.LastSuccessfulAt != observedAt {
		t.Fatalf("card %+v", card)
	}
	if card.ErrorReason == nil || *card.ErrorReason != "upstream_error" {
		t.Fatalf("error reason %v", card.ErrorReason)
	}
	if len(recorder.calls) != 2 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
}

func TestFirstFailureWithNoEarlierReadingIsAnError(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 403, "forbidden" }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	report := accounts.Refresh(context.Background(), nil, false)
	result := report.Results[0]
	if result.Status != "error" || *result.ErrorReason != "auth_rejected" || *result.HTTPStatus != 403 {
		t.Fatalf("result %+v", result)
	}
	card := accounts.State().Cards[0]
	if card.Status != "error" || *card.ErrorReason != "auth_rejected" {
		t.Fatalf("card %+v", card)
	}
	if len(card.Records) != 1 || card.Records[0].Remaining != nil || card.Records[0].ObservedAt != nil {
		t.Fatalf("records %+v", card.Records)
	}
	if card.LastSuccessfulAt != nil {
		t.Fatalf("a failed first read has a success time")
	}
}

func TestAReplyThatNoLongerMatchesKeepsTheOldNumbers(t *testing.T) {
	body := `{"data":{"remaining":12}}`
	recorder := &recorder{reply: func(call call) (int, string) { return 200, body }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "custom", "sourceId": "relay", "kind": "api_balance",
		"unit": "USD", "endpoint": "https://relay.example.test/balance",
		"fields": map[string]any{"remaining": "data.remaining"}})
	accounts.Refresh(context.Background(), nil, false)
	if *accounts.State().Records[0].Remaining != 12 {
		t.Fatalf("first read %+v", accounts.State().Records[0])
	}
	body = `{"data":{"remaining":"not a number"}}`
	report := accounts.Refresh(context.Background(), nil, false)
	if report.Results[0].Status != "stale" || *report.Results[0].ErrorReason != "value_missing" {
		t.Fatalf("second read %+v", report.Results[0])
	}
	if *accounts.State().Records[0].Remaining != 12 {
		t.Fatalf("the old number was lost")
	}
}

func TestNoCredentialMeansNoRequestAndANotConfiguredCard(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return "" },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	if accounts.State().Cards[0].Status != "not_configured" {
		t.Fatalf("status %q", accounts.State().Cards[0].Status)
	}
	report := accounts.Refresh(context.Background(), nil, false)
	if report.Results[0].Status != "not_configured" || *report.Results[0].ErrorReason != "credential_missing" {
		t.Fatalf("result %+v", report.Results[0])
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("a request was sent without a credential")
	}
}

func TestKimiCodeSubscriptionIsReportedUnsupported(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, "{}" }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return moonshotKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "kimi-code", "sourceId": "kimi-code",
		"accountLabel": "主力账号"})
	report := accounts.Refresh(context.Background(), nil, false)
	if report.Results[0].Status != "unsupported" || *report.Results[0].ErrorReason != "unsupported_source" {
		t.Fatalf("result %+v", report.Results[0])
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("an unsupported source sent a request")
	}
	card := accounts.State().Cards[0]
	if card.Supported || card.Status != "unsupported" {
		t.Fatalf("card %+v", card)
	}
	if card.OfficialPage == nil || *card.OfficialPage != "https://www.kimi.com/code/docs/kimi-code/membership.html" {
		t.Fatalf("official page %v", card.OfficialPage)
	}
	if card.ConsoleURL == nil || *card.ConsoleURL != "https://www.kimi.com/code/console" {
		t.Fatalf("console url %v", card.ConsoleURL)
	}
	if card.Records[0].Remaining != nil {
		t.Fatalf("an unsupported card carries a number")
	}
}

func TestEveryPresetStatesWhetherItIsSupported(t *testing.T) {
	byAdapter := map[string]quota.Preset{}
	for _, preset := range quota.Presets() {
		byAdapter[preset.Adapter] = preset
	}
	if !byAdapter["deepseek"].Supported || !byAdapter["moonshot-cn"].Supported ||
		!byAdapter["moonshot-global"].Supported || byAdapter["kimi-code"].Supported {
		t.Fatalf("presets %+v", byAdapter)
	}
	if *byAdapter["deepseek"].OfficialPage != "https://api-docs.deepseek.com/api/get-user-balance" ||
		*byAdapter["moonshot-cn"].OfficialPage != "https://platform.moonshot.cn/docs/api/balance" ||
		*byAdapter["moonshot-global"].OfficialPage != "https://platform.kimi.ai/docs/api/balance" {
		t.Fatalf("official pages %+v", byAdapter)
	}
	for _, preset := range quota.Presets() {
		// A supported vendor preset must name the document it was read from; the
		// custom adapter has no vendor behind it, so it names none.
		if preset.Supported && preset.Adapter != "custom" && preset.OfficialPage == nil {
			t.Fatalf("%s is supported without a document", preset.Adapter)
		}
	}
}

func TestMoonshotAdapterAsksItsOwnRegionalEndpoint(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, moonshotBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(ref string) string {
		if ref == "moonshot:default" {
			return moonshotKey
		}
		return ""
	}, HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "moonshot-cn"})
	accounts.Refresh(context.Background(), nil, false)
	if recorder.calls[0].URL != quota.MoonshotCNEndpoint {
		t.Fatalf("url %q", recorder.calls[0].URL)
	}
	row := accounts.State().Records[0]
	if *row.Remaining != 49.58894 || row.Unit == nil || *row.Unit != "CNY" {
		t.Fatalf("row %+v", row)
	}
}

func TestTheCredentialNeverAppearsInStateOrOnDisk(t *testing.T) {
	stateDir := t.TempDir()
	recorder := &recorder{reply: func(call call) (int, string) { return 401, `{"error":"bad key"}` }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek",
		"accountLabel": "工作账号"})
	report := accounts.Refresh(context.Background(), nil, false)
	if *report.Results[0].ErrorReason != "auth_rejected" {
		t.Fatalf("result %+v", report.Results[0])
	}
	encoded, _ := json.Marshal(accounts.State())
	if strings.Contains(string(encoded), deepSeekKey) || strings.Contains(string(encoded), "Bearer") {
		t.Fatalf("state leaked a credential: %s", encoded)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("nothing was persisted")
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if strings.Contains(string(raw), deepSeekKey) || strings.Contains(string(raw), "Bearer") {
			t.Fatalf("%s leaked a credential", entry.Name())
		}
	}
	// The masked account reference is the label the person typed, not the key.
	card := accounts.State().Cards[0]
	if card.AccountRefMasked != "工作账号" {
		t.Fatalf("masked ref %q", card.AccountRefMasked)
	}
	if tag := quota.AccountTag(deepSeekKey); !strings.HasPrefix(tag, "acct-") || len(tag) != 13 {
		t.Fatalf("tag %q", tag)
	}
	if quota.AccountTag(deepSeekKey) == quota.AccountTag(moonshotKey) {
		t.Fatalf("two keys share a tag")
	}
}

func TestALabelThatLooksLikeACredentialIsRefused(t *testing.T) {
	if got, err := quota.SanitizeLabel("工作账号"); err != nil || got != "工作账号" {
		t.Fatalf("label %q err %v", got, err)
	}
	if got, err := quota.SanitizeLabel(""); err != nil || got != "" {
		t.Fatalf("empty label %q err %v", got, err)
	}
	if _, err := quota.SanitizeLabel("sk-" + strings.Repeat("b", 24)); err == nil {
		t.Fatalf("a credential-shaped label was accepted")
	}
	if _, err := quota.SanitizeLabel(strings.Repeat("a", 41)); err == nil {
		t.Fatalf("an over-long label was accepted")
	}
}

// --- endpoint safety -------------------------------------------------------

func TestCustomEndpointsAreRefusedUnlessPublicHTTPSOrLoopback(t *testing.T) {
	for _, url := range []string{
		"https://relay.example.com/api/balance",
		"http://127.0.0.1:3425/v1/magpie/quotas",
		"http://localhost:3000/usage",
	} {
		if check := quota.CheckEndpoint(url); !check.OK {
			t.Fatalf("%s refused: %s", url, check.Code)
		}
	}
	// Plain HTTP to a remote host would put the key on the wire in clear.
	if check := quota.CheckEndpoint("http://relay.example.com/balance"); check.Code != "insecure_endpoint" {
		t.Fatalf("remote http code %q", check.Code)
	}
	// Cloud metadata, LAN, and other non-public destinations.
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data",
		"https://10.0.0.5/balance",
		"https://192.168.1.1/balance",
		"https://172.16.0.1/balance",
		"https://100.64.0.1/balance",
		"https://[fd00::1]/balance",
		"https://[fe80::1]/balance",
		"https://metadata/balance",
		"https://nas.local/balance",
		"https://box.internal/balance",
	} {
		if check := quota.CheckEndpoint(url); check.OK {
			t.Fatalf("%s was accepted", url)
		}
	}
	// A credential or a query in the URL would end up somewhere it can be logged.
	for _, url := range []string{
		"https://user:" + "hunter2" + "@relay.example.com/balance",
		"https://relay.example.com/balance?key=abc",
		"https://relay.example.com/balance#frag",
		"file:///etc/passwd",
		"not a url",
	} {
		if check := quota.CheckEndpoint(url); check.OK {
			t.Fatalf("%s was accepted", url)
		}
	}
}

func TestAStoredEndpointRepointedAtAPrivateHostIsNeverRequested(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, `{"remaining":5}` }}
	stateDir := t.TempDir()
	accounts := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	accounts.AddSource(map[string]any{"adapter": "custom", "sourceId": "relay", "kind": "api_balance",
		"unit": "USD", "endpoint": "https://relay.example.test/balance",
		"fields": map[string]any{"remaining": "remaining"}})
	// Simulate a hand-edited config file that repoints the source at metadata.
	path := filepath.Join(stateDir, "quota-sources.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	edited := strings.Replace(string(raw), "https://relay.example.test/balance",
		"https://169.254.169.254/balance", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	reopened := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	// The entry is validated again on load, so a repointed endpoint is refused
	// before any request can carry the credential to it.
	if sources := reopened.ListSources(); len(sources) != 0 {
		t.Fatalf("a blocked endpoint survived the load: %+v", sources)
	}
	reopened.Refresh(context.Background(), nil, false)
	if len(recorder.calls) != 0 {
		t.Fatalf("a blocked endpoint was requested")
	}
	// The same gate refuses it directly, which is what a caller storing a new
	// source goes through.
	if check := quota.CheckEndpoint("https://169.254.169.254/balance"); check.Code != "blocked_target" {
		t.Fatalf("code %q", check.Code)
	}
}

func TestARedirectIsRefusedSoTheHeaderIsNeverReplayed(t *testing.T) {
	// A redirect is refused by the transport, which surfaces as a network
	// failure rather than a followed request.
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, http.ErrUseLastResponse
	})}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: client})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	report := accounts.Refresh(context.Background(), nil, false)
	if *report.Results[0].ErrorReason != "network_error" {
		t.Fatalf("result %+v", report.Results[0])
	}
	if accounts.State().Cards[0].Status != "error" {
		t.Fatalf("status %q", accounts.State().Cards[0].Status)
	}
}

func TestACustomSourceReadsOnlyTheOneDeclaredPath(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) {
		return 200, `{"data":{"remaining":12,"account":{"email":"someone@example.test","token":"leak"}},"debug":{"upstream":"internal"}}`
	}}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "custom", "sourceId": "relay", "kind": "api_balance",
		"unit": "USD", "endpoint": "https://relay.example.test/balance",
		"fields": map[string]any{"remaining": "data.remaining"}})
	accounts.Refresh(context.Background(), nil, false)
	encoded, _ := json.Marshal(accounts.State())
	for _, secret := range []string{"someone@example.test", "internal", customKey} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("state leaked %q: %s", secret, encoded)
		}
	}
}

func TestAPOSTBodyMayNotCarryACredential(t *testing.T) {
	for _, body := range []map[string]any{{"api_key": "x"}, {"pass" + "word": "x"}} {
		if _, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "api_balance",
			"unit": "USD", "endpoint": "https://relay.example.test/b", "method": "POST",
			"body": body, "fields": map[string]any{"remaining": "a"}}); err == nil {
			t.Fatalf("%v was accepted", body)
		}
	}
	// A body that only says what to ask is allowed, and only GET/POST are.
	source, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "gateway_limit",
		"unit": "credits", "endpoint": "https://relay.example.test/b", "method": "POST",
		"body": map[string]any{"scope": "monthly"}, "fields": map[string]any{"remaining": "a"}})
	if err != nil || source.Method != "POST" || source.Body["scope"] != "monthly" {
		t.Fatalf("source %+v err %v", source, err)
	}
	if _, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "api_balance",
		"unit": "USD", "endpoint": "https://relay.example.test/b", "method": "DELETE",
		"fields": map[string]any{"remaining": "a"}}); err == nil {
		t.Fatalf("DELETE was accepted")
	}
}

func TestAValuePathIsValidatedBeforeItIsUsed(t *testing.T) {
	if value, ok := quota.ReadPath(map[string]any{"a": map[string]any{"b": []any{
		map[string]any{"c": float64(7)}}}}, "a.b.0.c"); !ok || value != float64(7) {
		t.Fatalf("read %v %v", value, ok)
	}
	for _, path := range []string{"a.b", "b", "5", "__proto__.polluted", "constructor"} {
		document := any(map[string]any{"a": float64(1)})
		if path == "5" {
			document = []any{float64(1), float64(2)}
		}
		if _, ok := quota.ReadPath(document, path); ok {
			t.Fatalf("%q resolved", path)
		}
	}
	for _, path := range []string{"a..b", "a; rm -rf /"} {
		if _, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "api_balance",
			"unit": "USD", "endpoint": "https://relay.example.test/b",
			"fields": map[string]any{"remaining": path}}); err == nil {
			t.Fatalf("%q was accepted", path)
		}
	}
}

func TestACustomSourceNeedsAFigureAndAKnownUnit(t *testing.T) {
	base := map[string]any{"adapter": "custom", "kind": "api_balance", "unit": "USD",
		"endpoint": "https://relay.example.test/b"}
	cases := []map[string]any{
		{"fields": map[string]any{}},
		{"unit": "bananas", "fields": map[string]any{"remaining": "a"}},
		{"kind": "subscription_window", "fields": map[string]any{"remaining": "a"}},
		{"fields": map[string]any{"remaining": "a"}, "auth": map[string]any{"mode": "header", "headerName": "cookie"}},
	}
	for _, patch := range cases {
		input := map[string]any{}
		for key, value := range base {
			input[key] = value
		}
		for key, value := range patch {
			input[key] = value
		}
		if _, err := quota.NormalizeSource(input); err == nil {
			t.Fatalf("%v was accepted", patch)
		}
	}
	source, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "api_balance",
		"unit": "USD", "endpoint": "https://relay.example.test/b",
		"fields": map[string]any{"remaining": "a"}, "auth": map[string]any{"mode": "header", "headerName": "x-api-key"}})
	if err != nil || source.Auth["headerName"] != "x-api-key" {
		t.Fatalf("auth %+v err %v", source.Auth, err)
	}
}

// --- persistence -----------------------------------------------------------

func TestSourcesAndReadingsSurviveARestart(t *testing.T) {
	stateDir := t.TempDir()
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	first := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	first.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek", "accountLabel": "工作账号"})
	first.AddSource(map[string]any{"adapter": "kimi-code", "sourceId": "kimi-code"})
	first.Refresh(context.Background(), []string{"deepseek"}, false)
	if *first.State().Cards[0].Records[0].Remaining != 110 {
		t.Fatalf("first read %+v", first.State().Cards[0])
	}

	second := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	sources := second.ListSources()
	if len(sources) != 2 || sources[0].SourceID != "deepseek" || sources[1].SourceID != "kimi-code" {
		t.Fatalf("sources %+v", sources)
	}
	card := second.State().Cards[0]
	if *card.Records[0].Remaining != 110 || card.AccountRefMasked != "工作账号" {
		t.Fatalf("card %+v", card)
	}
	if second.State().Cards[1].Status != "unsupported" {
		t.Fatalf("second card %+v", second.State().Cards[1])
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, "quota-sources.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), deepSeekKey) {
		t.Fatalf("the source file holds a credential")
	}
	cache, err := os.ReadFile(filepath.Join(stateDir, "quota-cache.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(cache), deepSeekKey) {
		t.Fatalf("the cache holds a credential")
	}
	stat, err := os.Stat(filepath.Join(stateDir, "quota-cache.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		if !stat.Mode().IsRegular() || stat.Mode().Perm()&0o600 != 0o600 {
			t.Fatalf("mode %o", stat.Mode().Perm())
		}
	} else if stat.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", stat.Mode().Perm())
	}
}

func TestACacheEntryThatDoesNotCarryThisShapeIsDropped(t *testing.T) {
	stateDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(stateDir, "quota-sources.json"),
		[]byte(`{"schemaVersion":1,"sources":[{"adapter":"deepseek","sourceId":"deepseek"}]}`), 0o600)
	_ = os.WriteFile(filepath.Join(stateDir, "quota-cache.json"),
		[]byte(`{"schemaVersion":1,"refreshedAt":1,"sources":{"deepseek":{
			"lastAttemptAt":1,"lastAttemptOk":true,"lastSuccessAt":1,"records":[
			{"kind":"api_balance","unit":"CNY","remaining":"not a number"},
			{"kind":"not_a_kind","unit":"CNY","remaining":5},
			{"kind":"api_balance","unit":"CNY","remaining":3}]}}}`), 0o600)
	accounts := quota.New(quota.Options{StateDir: stateDir, Credentials: func(string) string { return deepSeekKey },
		Now: func() int64 { return 1794196800000 }, FreshnessMS: 15 * 60 * 1000})
	rows := accounts.State().Records
	// Only the well-formed record survives; the other two are not turned into 0.
	if len(rows) != 1 || *rows[0].Remaining != 3 {
		t.Fatalf("records %+v", rows)
	}
}

func TestAddingAndRemovingIsTheOnlyWrite(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	added, err := accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	if err != nil || !added.Supported {
		t.Fatalf("add %+v err %v", added, err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("adding a source made a request")
	}
	if _, err := accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"}); err == nil {
		t.Fatalf("a duplicate id was accepted")
	}
	if !accounts.RemoveSource("deepseek") {
		t.Fatalf("remove failed")
	}
	if accounts.RemoveSource("deepseek") {
		t.Fatalf("removing twice succeeded")
	}
	if len(accounts.State().Cards) != 0 {
		t.Fatalf("a removed source still has a card")
	}
}

func TestRefreshingOneSourceLeavesTheOthersAlone(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(ref string) string {
		if ref == "deepseek:default" {
			return deepSeekKey
		}
		return ""
	}, HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "moonshot-cn"})
	accounts.Refresh(context.Background(), []string{"deepseek"}, false)
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	if accounts.State().Cards[0].Status != "ok" || accounts.State().Cards[1].Status != "not_configured" {
		t.Fatalf("cards %+v", accounts.State().Cards)
	}
	// A single unknown id is reported, not silently ignored.
	report := accounts.Refresh(context.Background(), []string{"nope"}, false)
	if len(report.Results) != 1 || report.Results[0].Status != "unknown" ||
		*report.Results[0].ErrorReason != "unknown_source" {
		t.Fatalf("report %+v", report.Results)
	}
}

func TestASourceIsThrottledRatherThanAskedTwice(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), ThrottleMS: 60000})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.Refresh(context.Background(), nil, false)
	second := accounts.Refresh(context.Background(), nil, false)
	if second.Results[0].Status != "throttled" || second.Results[0].Skipped == nil {
		t.Fatalf("second read %+v", second.Results[0])
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	// An explicit "refresh now" still goes out.
	accounts.Refresh(context.Background(), []string{"deepseek"}, true)
	if len(recorder.calls) != 2 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
}

func TestStateMakesNoRequestAndNoWrite(t *testing.T) {
	stateDir := t.TempDir()
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	before, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	accounts.State()
	accounts.State()
	if len(recorder.calls) != 0 {
		t.Fatalf("state made a request")
	}
	after, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("state wrote a file")
	}
}

func TestAReadingOlderThanTheFreshnessWindowIsStaleWithoutAFailure(t *testing.T) {
	clock := int64(1800000000000)
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), Now: func() int64 { return clock }, FreshnessMS: 60000})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.Refresh(context.Background(), nil, false)
	if accounts.State().Cards[0].Status != "ok" {
		t.Fatalf("status %q", accounts.State().Cards[0].Status)
	}
	clock += 120000
	card := accounts.State().Cards[0]
	if card.Status != "stale" || card.StaleReason == nil || *card.StaleReason != "age" {
		t.Fatalf("card %+v", card)
	}
	if *card.Records[0].Remaining != 110 {
		t.Fatalf("the number was lost")
	}
}

func TestTwoAdaptersNeverCombineIntoOneFigure(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) {
		if strings.Contains(call.URL, "deepseek") {
			return 200, deepSeekBalance()
		}
		return 200, moonshotBalance()
	}}
	accounts := manager(t, quota.Options{Credentials: func(ref string) string {
		switch ref {
		case "deepseek:default":
			return deepSeekKey
		case "moonshot:default":
			return moonshotKey
		}
		return ""
	}, HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "moonshot-cn"})
	accounts.Refresh(context.Background(), nil, false)
	state := accounts.State()
	if len(state.Records) != 3 {
		t.Fatalf("records %d", len(state.Records))
	}
	units := []string{}
	sourceIDs := []string{}
	for _, row := range state.Records {
		if row.Unit != nil {
			units = append(units, *row.Unit)
		}
		sourceIDs = append(sourceIDs, row.SourceID)
	}
	if strings.Join(units, ",") != "CNY,USD,CNY" {
		t.Fatalf("units %v", units)
	}
	if strings.Join(sourceIDs, ",") != "deepseek,deepseek,moonshot-cn" {
		t.Fatalf("sources %v", sourceIDs)
	}
	if state.Combining != "never" {
		t.Fatalf("combining %q", state.Combining)
	}
	for _, card := range state.Cards {
		encoded, _ := json.Marshal(card)
		if strings.Contains(string(encoded), `"total"`) {
			t.Fatalf("a card carries a total: %s", encoded)
		}
	}
}

func TestACustomSourceRoundTripsThroughTheStore(t *testing.T) {
	stateDir := t.TempDir()
	recorder := &recorder{reply: func(call call) (int, string) { return 200, `{"result":{"left":250}}` }}
	first := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	if _, err := first.AddSource(map[string]any{"adapter": "custom", "sourceId": "relay-post",
		"providerId": "relay", "kind": "gateway_limit", "unit": "credits",
		"endpoint": "https://relay.example.test/api/quota", "method": "POST",
		"body":        map[string]any{"scope": "monthly"},
		"auth":        map[string]any{"mode": "header", "headerName": "x-api-key"},
		"windowLabel": "month", "fields": map[string]any{"remaining": "result.left"}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// A fresh instance reads the stored file back; a field lost on the round
	// trip would change the request that goes out.
	second := manager(t, quota.Options{Credentials: func(string) string { return customKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	second.Refresh(context.Background(), nil, false)
	if len(recorder.calls) != 1 {
		t.Fatalf("calls %d", len(recorder.calls))
	}
	sent := recorder.calls[0]
	if sent.Method != "POST" || sent.Body != `{"scope":"monthly"}` {
		t.Fatalf("call %+v", sent)
	}
	if sent.Headers["X-Api-Key"] != customKey {
		t.Fatalf("headers %v", sent.Headers)
	}
	if sent.Headers["Authorization"] != "" {
		t.Fatalf("a Bearer header was also sent: %v", sent.Headers)
	}
	row := second.State().Records[0]
	if *row.Remaining != 250 || row.WindowLabel == nil || *row.WindowLabel != "month" ||
		row.Kind != "gateway_limit" {
		t.Fatalf("row %+v", row)
	}
}

func TestRenamingKeepsTheReadingAndRefusesACredentialShapedLabel(t *testing.T) {
	stateDir := t.TempDir()
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.Refresh(context.Background(), nil, false)
	renamed, err := accounts.RelabelSource("deepseek", "主力")
	if err != nil || renamed.AccountLabel == nil || *renamed.AccountLabel != "主力" {
		t.Fatalf("rename %+v err %v", renamed, err)
	}
	// The numbers were read before the rename and stay attached to the source.
	if *accounts.State().Records[0].Remaining != 110 || accounts.State().Cards[0].AccountRefMasked != "主力" {
		t.Fatalf("reading lost on rename")
	}
	if _, err := accounts.RelabelSource("deepseek", "sk-"+strings.Repeat("a", 24)); err == nil {
		t.Fatalf("a credential-shaped label was accepted")
	}
	if _, err := accounts.RelabelSource("missing", "主力"); err == nil {
		t.Fatalf("renaming an unknown source succeeded")
	}
	// The new label survives a restart.
	reopened := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), StateDir: stateDir})
	label := reopened.ListSources()[0].AccountLabel
	if label == nil || *label != "主力" {
		t.Fatalf("label after restart %v", label)
	}
}

func TestCustomSourcesRejectInferenceEndpointsAndFields(t *testing.T) {
	if code := quota.CheckEndpoint("https://relay.example.test/v1/responses").Code; code != "inference_endpoint" {
		t.Fatalf("code %q", code)
	}
	if code := quota.CheckEndpoint("http://127.0.0.1:3000/v1/chat/completions").Code; code != "inference_endpoint" {
		t.Fatalf("code %q", code)
	}
	for _, body := range []map[string]any{{"model": "probe"}, {"scope": map[string]any{"messages": []any{}}},
		{"input": "long-context"}} {
		if _, err := quota.NormalizeSource(map[string]any{"adapter": "custom", "kind": "api_balance",
			"unit": "USD", "endpoint": "https://relay.example.test/balance", "method": "POST",
			"body": body, "fields": map[string]any{"remaining": "data.remaining"}}); err == nil {
			t.Fatalf("%v was accepted", body)
		}
	}
}

func TestSeedSourcesAreUsedOnlyWhenNothingIsStored(t *testing.T) {
	stateDir := t.TempDir()
	seed := []map[string]any{{"adapter": "kimi-code", "sourceId": "kimi-code"},
		{"adapter": "deepseek", "sourceId": "deepseek-seeded"}}
	first := manager(t, quota.Options{StateDir: stateDir, Seed: seed})
	if len(first.ListSources()) != 2 {
		t.Fatalf("seed %+v", first.ListSources())
	}
	first.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "moonshot-cn"})
	second := manager(t, quota.Options{StateDir: stateDir, Seed: seed})
	sources := second.ListSources()
	if len(sources) != 3 {
		t.Fatalf("a seed came back after a stored list existed: %+v", sources)
	}
}

func TestConcurrentRefreshesDoNotCrossAnswers(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "a"})
	accounts.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "b"})
	done := make(chan quota.RefreshReport, 2)
	go func() { done <- accounts.Refresh(context.Background(), []string{"a"}, false) }()
	go func() { done <- accounts.Refresh(context.Background(), []string{"b"}, false) }()
	first, second := <-done, <-done
	for _, report := range []quota.RefreshReport{first, second} {
		if len(report.Results) != 1 {
			t.Fatalf("a caller was answered for another source: %+v", report.Results)
		}
	}
}

// --- panel action ----------------------------------------------------------

func panelConnections() []quota.ConnectionView {
	preset := "deepseek"
	return []quota.ConnectionView{
		{ID: "conn-a", Name: "Provider A", ProviderPreset: &preset, HasCredential: true},
		{ID: "conn-b", Name: "Provider B", HasCredential: false},
	}
}

func TestPanelActionStateIsTheSameAsPanelState(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	connections := panelConnections()
	direct := accounts.PanelState(connections)
	throughAction, err := accounts.PanelAction(context.Background(), quota.PanelPathState, nil, connections)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	directJSON, _ := json.Marshal(direct)
	actionJSON, _ := json.Marshal(throughAction)
	if string(directJSON) != string(actionJSON) {
		t.Fatalf("the GET path differs from PanelState:\n%s\n%s", directJSON, actionJSON)
	}
}

func TestPanelActionRefreshSemantics(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.AddSource(map[string]any{"adapter": "moonshot-cn", "sourceId": "moonshot-cn"})

	// An omitted list refreshes everything.
	result, err := accounts.PanelAction(context.Background(), quota.PanelPathRefresh,
		map[string]any{}, panelConnections())
	if err != nil {
		t.Fatalf("refresh all: %v", err)
	}
	report, ok := result["lastRefresh"].(quota.RefreshReport)
	if !ok {
		t.Fatalf("lastRefresh %T", result["lastRefresh"])
	}
	if len(report.Results) != 2 {
		t.Fatalf("an omitted list refreshed %d sources", len(report.Results))
	}

	// An explicit empty list refreshes nothing, which is not the same request.
	recorder.calls = nil
	empty, err := accounts.PanelAction(context.Background(), quota.PanelPathRefresh,
		map[string]any{"sourceIds": []any{}}, panelConnections())
	if err != nil {
		t.Fatalf("refresh none: %v", err)
	}
	emptyReport, _ := empty["lastRefresh"].(quota.RefreshReport)
	if len(emptyReport.Results) != 0 || len(recorder.calls) != 0 {
		t.Fatalf("an empty list did work: %d results, %d calls",
			len(emptyReport.Results), len(recorder.calls))
	}
}

func TestPanelActionRefreshDefaultsForceToFalse(t *testing.T) {
	clock := int64(1794196800000)
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := quota.New(quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client(), Now: func() int64 { return clock },
		FreshnessMS: 15 * 60 * 1000, ThrottleMS: 60000})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})
	accounts.PanelAction(context.Background(), quota.PanelPathRefresh, map[string]any{}, nil)
	// A second click inside the window is throttled because force defaults off.
	second, err := accounts.PanelAction(context.Background(), quota.PanelPathRefresh,
		map[string]any{}, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	report, _ := second["lastRefresh"].(quota.RefreshReport)
	if len(report.Results) != 1 || report.Results[0].Status != "throttled" {
		t.Fatalf("force defaulted on: %+v", report.Results)
	}
	// An explicit force still goes out.
	forced, err := accounts.PanelAction(context.Background(), quota.PanelPathRefresh,
		map[string]any{"force": true}, nil)
	if err != nil {
		t.Fatalf("forced: %v", err)
	}
	forcedReport, _ := forced["lastRefresh"].(quota.RefreshReport)
	if forcedReport.Results[0].Status != "ok" || len(recorder.calls) != 2 {
		t.Fatalf("force was ignored: %+v (%d calls)", forcedReport.Results, len(recorder.calls))
	}
}

func TestPanelActionRefusals(t *testing.T) {
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: (&recorder{}).client()})
	cases := []struct {
		name   string
		path   string
		input  map[string]any
		status int
		code   string
	}{
		{"refresh-list-type", quota.PanelPathRefresh, map[string]any{"sourceIds": "deepseek"}, 400, "invalid_quota_refresh"},
		{"refresh-force-type", quota.PanelPathRefresh, map[string]any{"force": "yes"}, 400, "invalid_quota_refresh"},
		{"refresh-nil-body", quota.PanelPathRefresh, nil, 400, "invalid_quota_refresh"},
		{"sources-nil-body", quota.PanelPathSources, nil, 400, "invalid_quota_source"},
		{"sources-bad-action", quota.PanelPathSources, map[string]any{"action": "rename"}, 400, "invalid_quota_source"},
		{"sources-remove-no-id", quota.PanelPathSources, map[string]any{"action": "remove"}, 400, "invalid_quota_source"},
		{"unknown-path", "/api/nope", nil, 404, "not_found"},
	}
	for _, test := range cases {
		_, err := accounts.PanelAction(context.Background(), test.path, test.input, nil)
		refusal, ok := err.(*quota.PanelActionError)
		if !ok {
			t.Fatalf("%s: error %v is not a PanelActionError", test.name, err)
		}
		if refusal.Status != test.status || refusal.Code != test.code {
			t.Fatalf("%s: %d %s want %d %s", test.name, refusal.Status, refusal.Code,
				test.status, test.code)
		}
	}
}

func TestPanelActionFromJSONAcceptsABodyOrNone(t *testing.T) {
	recorder := &recorder{reply: func(call call) (int, string) { return 200, deepSeekBalance() }}
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey },
		HTTPClient: recorder.client()})
	accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"})

	if _, err := accounts.PanelActionFromJSON(context.Background(), quota.PanelPathState, nil,
		panelConnections()); err != nil {
		t.Fatalf("empty GET body: %v", err)
	}
	if _, err := accounts.PanelActionFromJSON(context.Background(), quota.PanelPathRefresh,
		[]byte(`{"sourceIds":["deepseek"]}`), panelConnections()); err != nil {
		t.Fatalf("refresh body: %v", err)
	}
	// A body that is not JSON is a refusal with the path's own code, not a panic.
	_, err := accounts.PanelActionFromJSON(context.Background(), quota.PanelPathRefresh,
		[]byte(`{broken`), panelConnections())
	refusal, ok := err.(*quota.PanelActionError)
	if !ok || refusal.Status != 400 || refusal.Code != "invalid_quota_refresh" {
		t.Fatalf("malformed body: %v", err)
	}
}

func TestPanelRemovesOfficialAdapterBoundToUserConnection(t *testing.T) {
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey }})
	connections := panelConnections()
	state, err := accounts.PanelAction(context.Background(), quota.PanelPathSources, map[string]any{
		"action": "upsert", "source": map[string]any{"adapter": "deepseek", "sourceId": "deepseek",
			"sourceName": "DeepSeek 余额", "auth": map[string]any{"connectionId": "conn-a"}}}, connections)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := state["sources"].([]map[string]any)
	if len(rows) != 1 || rows[0]["removable"] != true {
		t.Fatalf("sources = %#v", rows)
	}
	state, err = accounts.PanelAction(context.Background(), quota.PanelPathSources,
		map[string]any{"action": "remove", "sourceId": "deepseek"}, connections)
	if err != nil || len(accounts.ListSources()) != 0 {
		t.Fatalf("remove state=%#v err=%v", state, err)
	}
}

func TestPanelKeepsDefaultAdapterWithoutUserConnection(t *testing.T) {
	accounts := manager(t, quota.Options{Credentials: func(string) string { return deepSeekKey }})
	if _, err := accounts.AddSource(map[string]any{"adapter": "deepseek", "sourceId": "deepseek"}); err != nil {
		t.Fatal(err)
	}
	state := accounts.PanelState(panelConnections())
	rows, _ := state["sources"].([]map[string]any)
	if len(rows) != 1 || rows[0]["removable"] != false {
		t.Fatalf("sources = %#v", rows)
	}
	_, err := accounts.PanelAction(context.Background(), quota.PanelPathSources,
		map[string]any{"action": "remove", "sourceId": "deepseek"}, panelConnections())
	if err == nil || err.Error() != "quota_source_not_removable" {
		t.Fatalf("remove error = %v", err)
	}
}

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

// budgetFixture is one daemon-shaped runtime over a real ledger, with one fake
// vendor that answers a fixed usage envelope.
type budgetFixture struct {
	dir      string
	store    *core.Store
	keys     KeyStore
	runtime  *Runtime
	service  *Service
	upstream *httptest.Server
	requests atomic.Int32
}

func newBudgetFixture(t *testing.T, handler http.HandlerFunc) *budgetFixture {
	t.Helper()
	fixture := &budgetFixture{dir: filepath.Join(t.TempDir(), "state")}
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(fixture.upstream.Close)
	store, err := core.Open(fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store, fixture.keys = store, CacheKeys(testKeys{})
	fixture.start(t)
	return fixture
}

func (f *budgetFixture) start(t *testing.T) {
	t.Helper()
	runtime, err := NewRuntime(f.dir, f.store, f.keys)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime = runtime
	f.service = &Service{Store: f.store, Keys: f.keys, StateDir: f.dir, Gateway: runtime,
		Extra: runtime.Action, OnConfig: runtime.Apply}
	if err = f.service.Start("0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.service.Close(context.Background()) })
}

// restart closes the HTTP service and builds a new runtime over the same state
// directory, which is the process restart the budget has to survive.
func (f *budgetFixture) restart(t *testing.T) {
	t.Helper()
	if err := f.service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.start(t)
}

func (f *budgetFixture) call(t *testing.T, method, path string, body any, key string, panel bool) (int, core.Document) {
	t.Helper()
	raw := []byte("{}")
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = encoded
	}
	request, err := http.NewRequest(method, f.service.Endpoint.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	if panel {
		request.Header.Set("Origin", f.service.Endpoint.URL)
	}
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var result core.Document
	decoder := json.NewDecoder(reply.Body)
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("%s %s: invalid JSON", method, path)
	}
	return reply.StatusCode, result
}

// budget drives the panel's two budget routes over the real HTTP boundary: the
// read, and the write through the same configuration transaction every other
// settings save uses.
func (f *budgetFixture) budget(t *testing.T, in core.Document) (int, core.Document) {
	t.Helper()
	if in == nil {
		return f.call(t, "GET", "/api/budget", nil, f.service.Endpoint.Key, true)
	}
	return f.call(t, "POST", "/api/budget/save", in, f.service.Endpoint.Key, true)
}

// configure saves connections, models and a budget document through the same
// routes the panel uses, so the test exercises the real merge and validation.
func (f *budgetFixture) configure(t *testing.T, models []any, budget core.Document) {
	t.Helper()
	_, rev, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	code, result := f.call(t, "POST", "/api/connections", core.Document{"revision": rev,
		"connections": []any{core.Document{"id": "work", "name": "Local fixture",
			"baseURL": f.upstream.URL, "apiKey": "synthetic-key", "protocols": []any{"chat"}}}}, f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("connections: %d %v", code, result)
	}
	_, rev, _ = f.store.Read()
	code, result = f.call(t, "POST", "/api/models", core.Document{"revision": rev, "models": models},
		f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("models: %d %v", code, result)
	}
	if budget == nil {
		return
	}
	_, rev, _ = f.store.Read()
	code, result = f.budget(t, core.Document{"revision": rev, "budget": budget})
	if code != 200 {
		t.Fatalf("budget: %d %v", code, result)
	}
}

func chatCall(t *testing.T, f *budgetFixture, model string) (*http.Response, string) {
	t.Helper()
	payload, _ := json.Marshal(core.Document{"model": model, "messages": []any{
		core.Document{"role": "user", "content": "synthetic"}}})
	request, _ := http.NewRequest("POST", f.service.Endpoint.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer "+f.runtime.Token())
	request.Header.Set("Content-Type", "application/json")
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	body, _ := io.ReadAll(reply.Body)
	return reply, string(body)
}

// usage replies with a fixed, known envelope: 1000 input and 500 output tokens.
// A price of 1.0 per million on both sides therefore costs 0.0015 per request.
func usage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
}

const costPerRequest = 0.0015

func pricedModel() []any {
	return []any{core.Document{"id": "kimi-cost", "protocol": "chat", "contextWindow": 32000,
		"connectionId": "work", "upstreamModelId": "vendor-cost",
		"pricing": core.Document{"inputPerMillion": 1.0, "outputPerMillion": 1.0}}}
}

// TestBudgetBlocksRequestsOverTheDailyLimit is the headline behaviour: once the
// ledger says today's estimated spend reached the limit, the next request is
// refused before it reaches the vendor and carries the window that refused it.
func TestBudgetBlocksRequestsOverTheDailyLimit(t *testing.T) {
	f := newBudgetFixture(t, usage)
	// Two requests at 0.0015 each reach 0.003, which is the limit; the third is
	// refused.
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": 0.003, "action": "block"}})
	for i := 0; i < 2; i++ {
		if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != 200 {
			t.Fatalf("request %d: status %d body %s", i, reply.StatusCode, body)
		}
	}
	before := f.requests.Load()
	reply, body := chatCall(t, f, "kimi-cost")
	if reply.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if code := errorCodeOf(t, body); code != "budget_limit_reached" {
		t.Fatalf("code %q", code)
	}
	if reply.Header.Get("X-Kimi-Work-Budget-Window") != "daily" {
		t.Fatalf("window %q", reply.Header.Get("X-Kimi-Work-Budget-Window"))
	}
	if reply.Header.Get("Retry-After") == "" {
		t.Fatal("a refused request must say when the window resets")
	}
	if f.requests.Load() != before {
		t.Fatal("a refused request still reached the vendor")
	}
	// The refusal is in the ledger as a failed local request, not as an upstream
	// answer and not as a cost.
	records := f.runtime.usage.Records()
	last := records[len(records)-1]
	if last.Status != "failed" || last.EstimatedCost != nil {
		t.Fatalf("refusal record %+v", last)
	}
	if last.ErrorCode == nil || *last.ErrorCode != "budget_limit_reached" {
		t.Fatalf("refusal error %v", last.ErrorCode)
	}
}

// TestBudgetRemindWarnsWithoutRefusing keeps the softer mode honest: the request
// goes through and the reply says the window is reached.
func TestBudgetRemindWarnsWithoutRefusing(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest, "action": "remind"}})
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != 200 {
		t.Fatalf("first request: %d %s", reply.StatusCode, body)
	}
	reply, body := chatCall(t, f, "kimi-cost")
	if reply.StatusCode != 200 {
		t.Fatalf("remind must not refuse: %d %s", reply.StatusCode, body)
	}
	if reply.Header.Get("X-Kimi-Work-Budget-Warning") != "daily" {
		t.Fatalf("warning %q", reply.Header.Get("X-Kimi-Work-Budget-Warning"))
	}
}

// TestBudgetSurvivesARestart proves spend is read from the ledger and not from
// memory: a fresh process sees the same total and refuses on the same rule.
func TestBudgetSurvivesARestart(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest * 2, "action": "block"}})
	for i := 0; i < 2; i++ {
		if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, reply.StatusCode, body)
		}
	}
	f.restart(t)
	code, result := f.budget(t, nil)
	if code != 200 {
		t.Fatalf("budget status %d", code)
	}
	status := core.Object(core.Object(result["budget"])["status"])
	spend := core.Object(core.Object(status["spend"])["daily"])
	if spend["spent"] != costPerRequest*2 {
		t.Fatalf("spent after restart %v", spend["spent"])
	}
	reply, body := chatCall(t, f, "kimi-cost")
	if reply.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
}

func TestBudgetKeepsHistoricalSpendAfterRemovingTheLastPrice(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest, "action": "block"}})
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != 200 {
		t.Fatalf("seed request: %d %s", reply.StatusCode, body)
	}
	models := pricedModel()
	core.Object(models[0])["pricing"] = nil
	f.configure(t, models, nil)
	code, result := f.budget(t, nil)
	if code != 200 {
		t.Fatalf("budget status: %d", code)
	}
	status := core.Object(core.Object(result["budget"])["status"])
	for _, window := range []string{"daily", "monthly"} {
		spend := core.Object(core.Object(status["spend"])[window])
		if spend["spent"] != costPerRequest || spend["pricedRequests"] != float64(1) {
			t.Fatalf("%s historical spend disappeared: %v", window, spend)
		}
	}
	before := f.requests.Load()
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("reached budget was bypassed: %d %s", reply.StatusCode, body)
	}
	if f.requests.Load() != before {
		t.Fatal("refused request reached upstream")
	}
	f.configure(t, models, core.Document{"enabled": true, "currency": "CNY",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest, "action": "block"}})
	_, result = f.budget(t, nil)
	status = core.Object(core.Object(result["budget"])["status"])
	if spend := core.Object(core.Object(status["spend"])["daily"]); spend["spent"] != float64(0) {
		t.Fatalf("different currencies were combined: %v", spend)
	}
	if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != http.StatusOK {
		t.Fatalf("another currency's spend blocked the request: %d %s", reply.StatusCode, body)
	}
}

// A model with no price must never be charged a fabricated zero cost.
func TestBudgetDoesNotPriceAnUnknownModelAsZero(t *testing.T) {
	f := newBudgetFixture(t, usage)
	models := []any{
		core.Document{"id": "kimi-cost", "protocol": "chat", "contextWindow": 32000,
			"connectionId": "work", "upstreamModelId": "vendor-cost",
			"pricing": core.Document{"inputPerMillion": 1.0, "outputPerMillion": 1.0}},
		core.Document{"id": "kimi-unpriced", "protocol": "chat", "contextWindow": 32000,
			"connectionId": "work", "upstreamModelId": "vendor-unpriced"},
	}
	f.configure(t, models, core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest, "action": "block"}})
	for i := 0; i < 3; i++ {
		if reply, body := chatCall(t, f, "kimi-unpriced"); reply.StatusCode != 200 {
			t.Fatalf("unpriced request %d: %d %s", i, reply.StatusCode, body)
		}
	}
	for _, record := range f.runtime.usage.Records() {
		if record.ModelAlias != nil && *record.ModelAlias == "kimi-unpriced" && record.EstimatedCost != nil {
			t.Fatalf("an unpriced model was charged: %+v", record)
		}
	}
	_, result := f.budget(t, nil)
	status := core.Object(core.Object(result["budget"])["status"])
	if status["warning"] != "unpriced_models_not_limited" {
		t.Fatalf("warning %v", status["warning"])
	}
	if !containsValue(core.Rows(status["unpricedModels"]), "kimi-unpriced") {
		t.Fatalf("unpriced models %v", status["unpricedModels"])
	}
	if containsValue(core.Rows(status["unpricedModels"]), "kimi-cost") {
		t.Fatalf("a priced model was reported unpriced")
	}
}

// TestSerializedBudgetAdmitsOneRequestAtATime checks the documented answer to
// concurrent overspend: with `serialize` on, the second request cannot be
// admitted until the first one's cost is in the ledger.
func TestSerializedBudgetAdmitsOneRequestAtATime(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		usage(w, nil)
	}
	f := newBudgetFixture(t, handler)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "serialize": true,
		"daily": core.Document{"limit": costPerRequest * 2, "action": "block"}})

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			reply, _ := chatCall(t, f, "kimi-cost")
			statuses[index] = reply.StatusCode
		}(i)
	}
	// Only one request may be inside the vendor at a time.
	<-entered
	select {
	case <-entered:
		t.Fatal("two requests were admitted at once in serialized mode")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	for index, status := range statuses {
		if status != 200 && status != http.StatusTooManyRequests {
			t.Fatalf("request %d: %d", index, status)
		}
	}
	// The third request is refused because the first two exhausted the limit,
	// which only holds when each admission saw the previous settlement.
	refused := 0
	for _, status := range statuses {
		if status == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("serialized budget admitted every request: %v", statuses)
	}
	if f.requests.Load() > 2 {
		t.Fatalf("vendor saw %d requests", f.requests.Load())
	}
}

// TestParallelBudgetCanOvershoot documents the real limitation instead of
// hiding it: without `serialize`, requests already in flight are admitted before
// the earlier one's cost lands, so the total can pass the limit.
func TestParallelBudgetCanOvershoot(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		usage(w, nil)
	}
	f := newBudgetFixture(t, handler)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": costPerRequest, "action": "block"}})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = chatCall(t, f, "kimi-cost")
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not reach the vendor")
		}
	}
	close(release)
	wg.Wait()
	if f.requests.Load() != 2 {
		t.Fatalf("both overlapping requests should have been admitted, vendor saw %d", f.requests.Load())
	}
	_, result := f.budget(t, nil)
	status := core.Object(core.Object(result["budget"])["status"])
	spend := core.Object(core.Object(status["spend"])["daily"])
	if spend["spent"] != costPerRequest*2 {
		t.Fatalf("spent %v", spend["spent"])
	}
	if spend["reached"] != true {
		t.Fatalf("window should be reported reached: %v", spend)
	}
}

// TestBudgetIsInertWithoutADocument is the compatibility promise: an
// installation that never configured a budget behaves exactly as before.
func TestBudgetIsInertWithoutADocument(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), nil)
	for i := 0; i < 3; i++ {
		if reply, body := chatCall(t, f, "kimi-cost"); reply.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, reply.StatusCode, body)
		}
	}
	for _, record := range f.runtime.usage.Records() {
		if record.PriceSource != nil && strings.HasPrefix(*record.PriceSource, "budget_table:") {
			t.Fatalf("a request was priced without a budget document")
		}
		if record.EstimatedCost != nil {
			t.Fatalf("a request was charged without a currency")
		}
	}
	_, result := f.budget(t, nil)
	settings := core.Object(result["budget"])
	if settings["configured"] != nil {
		t.Fatalf("configured budget %v", settings["configured"])
	}
	status := core.Object(settings["status"])
	if status["enabled"] != false {
		t.Fatalf("status %v", status)
	}
}

// TestBudgetReadNeedsNoRevision: reading the status must never be able to change
// the policy, which is why reading and writing are separate routes.
func TestBudgetReadNeedsNoRevision(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), core.Document{"enabled": true, "currency": "USD",
		"timeZone": "Asia/Shanghai", "daily": core.Document{"limit": 5, "action": "block"}})
	code, result := f.budget(t, nil)
	if code != 200 {
		t.Fatalf("read %d", code)
	}
	if result["budget"] == nil || core.Object(core.Object(result["budget"])["status"])["enabled"] != true {
		t.Fatalf("read returned %v", result)
	}
	// A save without a revision is refused rather than treated as a clear.
	if code, result := f.call(t, "POST", "/api/budget/save", core.Document{"budget": nil},
		f.service.Endpoint.Key, true); code != 409 {
		t.Fatalf("a save without a revision was accepted: %d %v", code, result)
	}
	d, _, _ := f.store.Read()
	if d["budget"] == nil {
		t.Fatal("the policy was cleared by a revision-less save")
	}
}

// TestBudgetRejectsAnInvalidDocument keeps a typo from silently disabling a
// limit.
func TestBudgetRejectsAnInvalidDocument(t *testing.T) {
	f := newBudgetFixture(t, usage)
	f.configure(t, pricedModel(), nil)
	for _, budget := range []core.Document{
		{"enabled": true, "currency": "usd", "daily": core.Document{"limit": 1}},
		{"enabled": true, "currency": "USD", "daily": core.Document{"limit": 0}},
		{"enabled": true, "currency": "USD", "daily": core.Document{"limit": 1, "action": "ignore"}},
		{"enabled": true, "currency": "USD", "timeZone": "Mars/Base"},
		{"enabled": true, "currency": "USD", "monthly": core.Document{"limit": 1}, "weekly": core.Document{"limit": 1}},
	} {
		_, rev, _ := f.store.Read()
		code, result := f.budget(t, core.Document{"revision": rev, "budget": budget})
		if code == 200 {
			t.Fatalf("accepted %v: %v", budget, result)
		}
	}
	// A price that covers only one side of the bill is refused too.
	_, rev, _ := f.store.Read()
	code, _ := f.call(t, "POST", "/api/models", core.Document{"revision": rev, "models": []any{
		core.Document{"id": "kimi-half", "protocol": "chat", "contextWindow": 32000,
			"connectionId": "work", "pricing": core.Document{"inputPerMillion": 1.0}}}},
		f.service.Endpoint.Key, true)
	if code == 200 {
		t.Fatal("accepted a half price")
	}
}

// TestModelSaveKeepsFailoverAndPricing is the UI-facing round trip: saving a
// model through the panel route must not drop the new fields.
func TestModelSaveKeepsFailoverAndPricing(t *testing.T) {
	f := newBudgetFixture(t, usage)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { usage(w, nil) }))
	defer second.Close()
	_, rev, _ := f.store.Read()
	code, result := f.call(t, "POST", "/api/connections", core.Document{"revision": rev, "connections": []any{
		core.Document{"id": "work", "name": "Primary", "baseURL": f.upstream.URL, "apiKey": "synthetic-key", "protocols": []any{"chat"}},
		core.Document{"id": "backup", "name": "Backup", "baseURL": second.URL, "apiKey": "vendor-key", "protocols": []any{"chat"}}}},
		f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("connections %d %v", code, result)
	}
	_, rev, _ = f.store.Read()
	model := core.Document{"id": "kimi-cost", "protocol": "chat", "contextWindow": 32000,
		"connectionId": "work", "upstreamModelId": "vendor-cost",
		"pricing":  core.Document{"inputPerMillion": 1.0, "outputPerMillion": 2.0},
		"failover": []any{core.Document{"connectionId": "backup", "upstreamModelId": "backup-cost"}}}
	code, result = f.call(t, "POST", "/api/models", core.Document{"revision": rev, "models": []any{model}},
		f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("models %d %v", code, result)
	}
	// Saving the same model again, as the panel does with an unchanged form,
	// must keep both fields.
	_, rev, _ = f.store.Read()
	code, result = f.call(t, "POST", "/api/models", core.Document{"revision": rev, "models": []any{core.Document{
		"id": "kimi-cost", "name": "Renamed", "protocol": "chat", "contextWindow": 32000,
		"connectionId": "work", "upstreamModelId": "vendor-cost"}}}, f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("models again %d %v", code, result)
	}
	d, _, _ := f.store.Read()
	row := core.Object(core.Rows(d["models"])[0])
	if row["name"] != "Renamed" {
		t.Fatalf("name %v", row["name"])
	}
	if row["pricing"] == nil {
		t.Fatal("pricing was dropped by the merge whitelist")
	}
	standbys := core.Rows(row["failover"])
	if len(standbys) != 1 || core.String(core.Object(standbys[0])["connectionId"]) != "backup" ||
		core.String(core.Object(standbys[0])["upstreamModelId"]) != "backup-cost" {
		t.Fatalf("failover %v", row["failover"])
	}
	// The connections route must keep refusing to delete a connection a standby
	// still uses.
	_, rev, _ = f.store.Read()
	code, _ = f.call(t, "POST", "/api/connections", core.Document{"revision": rev, "connections": []any{
		core.Document{"id": "work", "name": "Primary", "baseURL": f.upstream.URL, "apiKey": nil}}},
		f.service.Endpoint.Key, true)
	if code == 200 {
		t.Fatal("deleted a connection a failover candidate still names")
	}
}

func errorCodeOf(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return payload.Error.Code
}

func containsValue(rows []any, want any) bool {
	for _, row := range rows {
		if row == want {
			return true
		}
	}
	return false
}

// TestFailoverIsRecordedUnderTheConnectionThatAnswered is the ledger-side
// promise: a request served by a standby is written under the standby, so the
// usage page and the budget both see where the money actually went.
func TestFailoverIsRecordedUnderTheConnectionThatAnswered(t *testing.T) {
	f := newBudgetFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { usage(w, nil) }))
	defer backup.Close()
	_, rev, _ := f.store.Read()
	code, result := f.call(t, "POST", "/api/connections", core.Document{"revision": rev, "connections": []any{
		core.Document{"id": "work", "name": "Primary", "baseURL": f.upstream.URL, "apiKey": "synthetic-key", "protocols": []any{"chat"}},
		core.Document{"id": "backup", "name": "Backup", "baseURL": backup.URL, "apiKey": "vendor-key", "protocols": []any{"chat"}}}},
		f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("connections %d %v", code, result)
	}
	_, rev, _ = f.store.Read()
	model := core.Document{"id": "kimi-cost", "protocol": "chat", "contextWindow": 32000,
		"connectionId": "work", "upstreamModelId": "vendor-cost",
		"pricing":  core.Document{"inputPerMillion": 1.0, "outputPerMillion": 1.0},
		"failover": []any{core.Document{"connectionId": "backup"}}}
	code, result = f.call(t, "POST", "/api/models", core.Document{"revision": rev, "models": []any{model}},
		f.service.Endpoint.Key, true)
	if code != 200 {
		t.Fatalf("models %d %v", code, result)
	}
	// Tracking spend without a limit is a real configuration: the currency alone
	// makes the price table usable, and nothing is refused.
	_, rev, _ = f.store.Read()
	if code, result = f.budget(t, core.Document{"revision": rev,
		"budget": core.Document{"enabled": false, "currency": "USD", "timeZone": "Asia/Shanghai"}}); code != 200 {
		t.Fatalf("budget %d %v", code, result)
	}
	reply, body := chatCall(t, f, "kimi-cost")
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	records := f.runtime.usage.Records()
	if len(records) != 1 {
		t.Fatalf("records %d", len(records))
	}
	record := records[0]
	if record.Status != "completed" {
		t.Fatalf("status %q", record.Status)
	}
	if record.ConnectionID == nil || *record.ConnectionID != "backup" {
		t.Fatalf("the ledger attributed the request to %v", record.ConnectionID)
	}
	if record.EstimatedCost == nil || *record.EstimatedCost != costPerRequest {
		t.Fatalf("cost %v", record.EstimatedCost)
	}
	// The failed primary attempt is not written as a successful request: there is
	// exactly one record, and it is the one that answered.
	if f.requests.Load() != 1 {
		t.Fatalf("primary attempts %d", f.requests.Load())
	}
}

package quota_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/quota"
)

// This file is the quota dual run. The same scenarios are driven through the
// legacy 0.12.1 manager (testdata/legacy-run.mjs) and through this package, and
// the published State payloads are compared field by field.
//
// Regenerate the legacy side with:
//
//	bun internal/quota/testdata/legacy-run.mjs \
//	    internal/quota/testdata/quota-cases.json \
//	    internal/quota/testdata/legacy-observations.json

const (
	quotaCasesFile    = "testdata/quota-cases.json"
	quotaLegacyGolden = "testdata/legacy-observations.json"
	// The panel comparison needs sources that borrow a connection, which would
	// change the plain State payload; two fixture files keep both honest.
	panelCasesFile    = "testdata/panel-cases.json"
	panelLegacyGolden = "testdata/panel-observations.json"
)

var dualKeys = map[string]string{
	"deepseek:default": fakeKey("sk", "deepseek", "test", "a1b2c3d4"),
	"moonshot:default": fakeKey("sk", "moonshot", "test", "5e6f7a8b"),
	"custom":           fakeKey("gw", "relay", "test", "9c0d1e2f"),
}

type quotaFixture struct {
	Scenarios []quotaScenario `json:"scenarios"`
	Checks    []quotaCheck    `json:"checks"`
}

type quotaScenario struct {
	Name         string              `json:"name"`
	Source       map[string]any      `json:"source"`
	ExtraSources []map[string]any    `json:"extraSources"`
	Connections  []fixtureConnection `json:"connections"`
	FreshnessMS  int64               `json:"freshnessMs"`
	Steps        []quotaStep         `json:"steps"`
}

type fixtureConnection struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ProviderPreset string `json:"providerPresetId"`
	HasCredential  bool   `json:"hasCredential"`
}

type quotaStep struct {
	Kind      string         `json:"kind"`
	AdvanceMS int64          `json:"advanceMs"`
	Reply     map[string]any `json:"reply"`
}

type quotaCheck struct {
	Name   string `json:"name"`
	Inputs []any  `json:"inputs"`
}

type legacyQuota struct {
	Scenarios []legacyQuotaScenario `json:"scenarios"`
	Checks    map[string]any        `json:"checks"`
}

type legacyQuotaScenario struct {
	Name  string            `json:"name"`
	Error string            `json:"error"`
	Steps []legacyQuotaStep `json:"steps"`
}

type legacyQuotaStep struct {
	Kind  string         `json:"kind"`
	State map[string]any `json:"state"`
}

func loadQuotaFixtures(t *testing.T) quotaFixture {
	t.Helper()
	return loadQuotaFixtureFile(t, quotaCasesFile)
}

func loadQuotaFixtureFile(t *testing.T, file string) quotaFixture {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixture quotaFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fixture
}

func loadLegacyQuota(t *testing.T) legacyQuota {
	t.Helper()
	raw, err := os.ReadFile(quotaLegacyGolden)
	if err != nil {
		t.Fatalf("read legacy observations: %v (run the bun command in this file's comment)", err)
	}
	var document legacyQuota
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode legacy observations: %v", err)
	}
	return document
}

// scriptedTransport answers with whatever the current step says, which is how
// one scenario succeeds and then fails without a second manager.
type scriptedTransport struct {
	reply map[string]any
}

func (s *scriptedTransport) client() *http.Client {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := 200
		if value, ok := s.reply["status"].(float64); ok {
			status = int(value)
		}
		body := s.reply["body"]
		encoded, err := json.Marshal(body)
		if err != nil {
			encoded = []byte("{}")
		}
		if text, ok := body.(string); ok {
			encoded = []byte(text)
		}
		reply := &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
			Request:    request,
		}
		return reply, nil
	})
	return &http.Client{Transport: transport}
}

func runQuotaScenario(t *testing.T, scenario quotaScenario) []map[string]any {
	t.Helper()
	clock := int64(1794196800000)
	transport := &scriptedTransport{reply: map[string]any{}}
	freshness := scenario.FreshnessMS
	if freshness == 0 {
		freshness = 15 * 60 * 1000
	}
	accounts := quota.New(quota.Options{
		// A source borrowing a connection resolves that connection's credential,
		// which is how the real caller wires it.
		Credentials: func(ref string) string {
			if strings.HasPrefix(ref, "connection:") {
				id := strings.TrimPrefix(ref, "connection:")
				for _, connection := range scenario.Connections {
					if connection.ID == id {
						if connection.HasCredential {
							return dualKeys["custom"]
						}
						return ""
					}
				}
				return ""
			}
			return dualKeys[ref]
		},
		HTTPClient:  transport.client(),
		Now:         func() int64 { return clock },
		FreshnessMS: freshness,
		ThrottleMS:  -1,
	})
	if _, err := accounts.AddSource(scenario.Source); err != nil {
		t.Fatalf("add %v: %v", scenario.Source["sourceId"], err)
	}
	for _, extra := range scenario.ExtraSources {
		if _, err := accounts.AddSource(extra); err != nil {
			t.Fatalf("add extra %v: %v", extra["sourceId"], err)
		}
	}
	out := []map[string]any{}
	for _, step := range scenario.Steps {
		clock += step.AdvanceMS
		if step.Kind == "refresh" {
			transport.reply = step.Reply
			accounts.Refresh(context.Background(), nil, false)
		}
		encoded, err := json.Marshal(accounts.State())
		if err != nil {
			t.Fatalf("encode state: %v", err)
		}
		var state map[string]any
		if err := json.Unmarshal(encoded, &state); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		out = append(out, projectState(state))
	}
	return out
}

// projectState keeps exactly the fields the legacy observation records, so the
// comparison is about behaviour rather than about which extra fields this
// package publishes.
func projectState(state map[string]any) map[string]any {
	project := func(cards []any) []any {
		out := []any{}
		for _, raw := range cards {
			card, _ := raw.(map[string]any)
			out = append(out, map[string]any{
				"sourceId": card["sourceId"], "providerId": card["providerId"],
				"sourceName": card["sourceName"], "kind": card["kind"],
				"supported": card["supported"], "status": card["status"], "stale": card["stale"],
				"staleReason": card["staleReason"], "accountRefMasked": card["accountRefMasked"],
				"billingIdentity": card["billingIdentity"], "observedAt": card["observedAt"],
				"lastSuccessfulAt": card["lastSuccessfulAt"], "lastAttemptAt": card["lastAttemptAt"],
				"errorReason": card["errorReason"], "available": card["available"], "parts": card["parts"],
				"credentialRef": card["credentialRef"], "officialPage": card["officialPage"],
				"consoleUrl": card["consoleUrl"],
				"records":    projectRecords(card["records"]),
			})
		}
		return out
	}
	return map[string]any{
		"combining":   state["combining"],
		"refreshedAt": state["refreshedAt"],
		"freshnessMs": state["freshnessMs"],
		"cards":       project(asSlice(state["cards"])),
		"records":     projectRecords(state["records"]),
		"sources":     projectSources(asSlice(state["sources"])),
	}
}

func projectRecords(raw any) []any {
	out := []any{}
	for _, entry := range asSlice(raw) {
		record, _ := entry.(map[string]any)
		out = append(out, map[string]any{
			"sourceId": record["sourceId"], "kind": record["kind"],
			"windowLabel": record["windowLabel"], "unit": record["unit"],
			"limit": record["limit"], "used": record["used"], "remaining": record["remaining"],
			"periodStart": record["periodStart"], "resetAt": record["resetAt"],
			"observedAt": record["observedAt"], "lastSuccessfulAt": record["lastSuccessfulAt"],
			"stale": record["stale"], "status": record["status"], "errorReason": record["errorReason"],
		})
	}
	return out
}

func projectSources(raw []any) []any {
	out := []any{}
	for _, entry := range raw {
		source, _ := entry.(map[string]any)
		projected := map[string]any{
			"sourceId": source["sourceId"], "adapter": source["adapter"],
			"providerId": source["providerId"], "sourceName": source["sourceName"],
			"kind": source["kind"], "unit": source["unit"], "supported": source["supported"],
			"credentialRef": source["credentialRef"], "accountLabel": source["accountLabel"],
			"officialPage": source["officialPage"], "consoleUrl": source["consoleUrl"],
		}
		if source["adapter"] == "custom" {
			projected["method"] = source["method"]
			projected["endpoint"] = source["endpoint"]
			projected["host"] = source["host"]
			projected["fields"] = source["fields"]
			projected["auth"] = source["auth"]
			projected["windowLabel"] = source["windowLabel"]
		}
		out = append(out, projected)
	}
	return out
}

func asSlice(value any) []any {
	rows, _ := value.([]any)
	return rows
}

func TestDualRunMatchesLegacyQuota(t *testing.T) {
	if _, err := os.Stat(quotaLegacyGolden); err != nil {
		t.Skip("legacy observations not generated; run the bun command in this file's comment")
	}
	fixture := loadQuotaFixtures(t)
	legacy := loadLegacyQuota(t)
	if len(fixture.Scenarios) != len(legacy.Scenarios) {
		t.Fatalf("scenario count %d want %d", len(fixture.Scenarios), len(legacy.Scenarios))
	}
	for index, scenario := range fixture.Scenarios {
		scenario := scenario
		want := legacy.Scenarios[index]
		t.Run(scenario.Name, func(t *testing.T) {
			if want.Name != scenario.Name {
				t.Fatalf("legacy observation order changed: %q at %d", want.Name, index)
			}
			if want.Error != "" {
				t.Fatalf("the legacy run failed this scenario: %s", want.Error)
			}
			got := runQuotaScenario(t, scenario)
			if len(got) != len(want.Steps) {
				t.Fatalf("step count %d want %d", len(got), len(want.Steps))
			}
			for stepIndex := range got {
				if !reflect.DeepEqual(got[stepIndex], want.Steps[stepIndex].State) {
					t.Fatalf("step %d (%s) state differs\n got %s\nwant %s", stepIndex,
						want.Steps[stepIndex].Kind, prettyQuota(got[stepIndex]),
						prettyQuota(want.Steps[stepIndex].State))
				}
			}
		})
	}
}

func prettyQuota(value any) string {
	encoded, err := json.MarshalIndent(value, " ", " ")
	if err != nil {
		return "<?>"
	}
	return string(encoded)
}

// TestDualRunMatchesLegacyQuotaChecks compares the pure helpers: amount and
// timestamp parsing, the endpoint gate, and the account tag.
func TestDualRunMatchesLegacyQuotaChecks(t *testing.T) {
	if _, err := os.Stat(quotaLegacyGolden); err != nil {
		t.Skip("legacy observations not generated")
	}
	fixture := loadQuotaFixtures(t)
	legacy := loadLegacyQuota(t)

	for _, check := range fixture.Checks {
		switch check.Name {
		case "amount-parsing":
			want, _ := legacy.Checks["amount-parsing"].([]any)
			for index, input := range check.Inputs {
				got := quota.ToAmount(input)
				if !sameAmount(got, want[index]) {
					t.Fatalf("amount %v: got %v want %v", input, got, want[index])
				}
			}
		case "timestamp-parsing":
			want, _ := legacy.Checks["timestamp-parsing"].([]any)
			for index, input := range check.Inputs {
				got := quota.ToTimestamp(input)
				if !sameInstant(got, want[index]) {
					t.Fatalf("timestamp %v: got %v want %v", input, got, want[index])
				}
			}
		case "endpoint-gate":
			want, _ := legacy.Checks["endpoint-gate"].([]any)
			for index, input := range check.Inputs {
				text, _ := input.(string)
				got := quota.CheckEndpoint(text)
				entry, _ := want[index].(map[string]any)
				wantOK, _ := entry["ok"].(bool)
				wantCode := ""
				if value, ok := entry["code"].(string); ok {
					wantCode = value
				}
				gotCode := ""
				if !got.OK {
					gotCode = got.Code
				}
				if got.OK != wantOK || gotCode != wantCode {
					t.Fatalf("endpoint %q: got ok=%v code=%q want ok=%v code=%q",
						text, got.OK, gotCode, wantOK, wantCode)
				}
			}
		}
	}
	wantTag, _ := legacy.Checks["accountTag"].(string)
	if got := quota.AccountTag(dualKeys["deepseek:default"]); got != wantTag {
		t.Fatalf("account tag %q want %q", got, wantTag)
	}
}

func sameInstant(got *int64, want any) bool {
	if want == nil {
		return got == nil
	}
	number, ok := want.(float64)
	if !ok {
		return false
	}
	return got != nil && *got == int64(number)
}

func sameAmount(got *float64, want any) bool {
	if want == nil {
		return got == nil
	}
	number, ok := want.(float64)
	if !ok {
		return false
	}
	return got != nil && *got == number
}

func TestLegacyQuotaObservationsAreReproducible(t *testing.T) {
	if os.Getenv("DUAL_RUN_REGEN") != "1" {
		t.Skip("set DUAL_RUN_REGEN=1 to re-run the legacy manager")
	}
	bun := findQuotaBun()
	if bun == "" {
		t.Skip("no bun executable found")
	}
	output := filepath.Join(t.TempDir(), "legacy.json")
	command := exec.Command(bun, "run", "testdata/legacy-run.mjs", quotaCasesFile, output)
	command.Dir = "."
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("legacy run failed: %v\n%s", err, combined)
	}
	fresh, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	committed, err := os.ReadFile(quotaLegacyGolden)
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}
	if string(fresh) != string(committed) {
		t.Fatalf("the committed observations no longer match a fresh run")
	}
}

func findQuotaBun() string {
	for _, candidate := range []string{
		os.Getenv("BUN"),
		filepath.Join("..", "..", "..", "..", ".bun", "bun-1.4.4"),
		"bun",
	} {
		if candidate == "" {
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved
		}
	}
	return ""
}

// The panel golden is the decoration the legacy panel controller applied on top
// of the same State payload. It is a separate file because it exercises a
// different layer: the four facts a source row shows that this package cannot
// know on its own.

type legacyPanel struct {
	Scenarios []struct {
		Name  string           `json:"name"`
		Steps []map[string]any `json:"steps"`
	} `json:"scenarios"`
}

// TestDualRunMatchesLegacyPanelDecoration compares PanelState with the legacy
// controller's own decoration, which is what the existing UI actually reads.
func TestDualRunMatchesLegacyPanelDecoration(t *testing.T) {
	if _, err := os.Stat(panelLegacyGolden); err != nil {
		t.Skip("legacy panel observations not generated")
	}
	raw, err := os.ReadFile(panelLegacyGolden)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var legacy legacyPanel
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fixture := loadQuotaFixtureFile(t, panelCasesFile)
	if len(fixture.Scenarios) != len(legacy.Scenarios) {
		t.Fatalf("scenario count %d want %d", len(fixture.Scenarios), len(legacy.Scenarios))
	}
	for index, scenario := range fixture.Scenarios {
		scenario := scenario
		want := legacy.Scenarios[index]
		t.Run("panel/"+scenario.Name, func(t *testing.T) {
			if want.Name != scenario.Name {
				t.Fatalf("legacy observation order changed: %q at %d", want.Name, index)
			}
			steps := runPanelScenario(t, scenario)
			if len(steps) != len(want.Steps) {
				t.Fatalf("step count %d want %d", len(steps), len(want.Steps))
			}
			for stepIndex := range steps {
				if message, ok := want.Steps[stepIndex]["error"]; ok {
					t.Fatalf("the legacy run failed step %d: %v", stepIndex, message)
				}
				for _, field := range []string{"records", "sources", "connections", "combining"} {
					if !reflect.DeepEqual(steps[stepIndex][field], want.Steps[stepIndex][field]) {
						t.Fatalf("step %d %s differs\n got %s\nwant %s", stepIndex, field,
							prettyQuota(steps[stepIndex][field]), prettyQuota(want.Steps[stepIndex][field]))
					}
				}
			}
		})
	}
}

func runPanelScenario(t *testing.T, scenario quotaScenario) []map[string]any {
	t.Helper()
	clock := int64(1794196800000)
	transport := &scriptedTransport{reply: map[string]any{}}
	freshness := scenario.FreshnessMS
	if freshness == 0 {
		freshness = 15 * 60 * 1000
	}
	accounts := quota.New(quota.Options{
		Credentials: func(ref string) string {
			if strings.HasPrefix(ref, "connection:") {
				id := strings.TrimPrefix(ref, "connection:")
				for _, connection := range scenario.Connections {
					if connection.ID == id {
						if connection.HasCredential {
							return dualKeys["custom"]
						}
						return ""
					}
				}
				return ""
			}
			return dualKeys[ref]
		},
		HTTPClient:  transport.client(),
		Now:         func() int64 { return clock },
		FreshnessMS: freshness,
		ThrottleMS:  -1,
	})
	if _, err := accounts.AddSource(scenario.Source); err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, extra := range scenario.ExtraSources {
		if _, err := accounts.AddSource(extra); err != nil {
			t.Fatalf("add extra: %v", err)
		}
	}
	connections := make([]quota.ConnectionView, 0, len(scenario.Connections))
	for _, connection := range scenario.Connections {
		view := quota.ConnectionView{ID: connection.ID, Name: connection.Name,
			HasCredential: connection.HasCredential}
		if connection.ProviderPreset != "" {
			preset := connection.ProviderPreset
			view.ProviderPreset = &preset
		}
		connections = append(connections, view)
	}
	out := []map[string]any{}
	for _, step := range scenario.Steps {
		clock += step.AdvanceMS
		if step.Kind == "refresh" {
			transport.reply = step.Reply
			accounts.Refresh(context.Background(), nil, false)
		}
		panel := accounts.PanelState(connections)
		encoded, err := json.Marshal(panel)
		if err != nil {
			t.Fatalf("encode panel state: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode panel state: %v", err)
		}
		projected := map[string]any{
			"records":     projectPanelRecords(decoded["records"]),
			"sources":     projectPanelSources(decoded["sources"]),
			"connections": decoded["connections"],
			"combining":   decoded["combining"],
		}
		out = append(out, projected)
	}
	return out
}

// projectPanelRecords and projectPanelSources keep only the fields the legacy
// panel decoration produced, so the comparison is about behaviour.
func projectPanelRecords(raw any) []any {
	out := []any{}
	for _, entry := range asSlice(raw) {
		record, _ := entry.(map[string]any)
		out = append(out, map[string]any{
			"sourceId": record["sourceId"], "providerId": record["providerId"],
			"accountRefMasked": record["accountRefMasked"],
			"billingIdentity":  record["billingIdentity"], "kind": record["kind"],
			"windowLabel": record["windowLabel"], "unit": record["unit"],
			"limit": record["limit"], "used": record["used"], "remaining": record["remaining"],
			"periodStart": record["periodStart"], "resetAt": record["resetAt"],
			"observedAt": record["observedAt"], "lastSuccessfulAt": record["lastSuccessfulAt"],
			"stale": record["stale"], "status": record["status"], "errorReason": record["errorReason"],
			"sourceName": record["sourceName"], "parts": record["parts"],
			"officialUrl": record["officialUrl"],
		})
	}
	return out
}

func projectPanelSources(raw any) []any {
	out := []any{}
	for _, entry := range asSlice(raw) {
		source, _ := entry.(map[string]any)
		projected := map[string]any{
			"sourceId": source["sourceId"], "adapter": source["adapter"],
			"providerId": source["providerId"], "sourceName": source["sourceName"],
			"kind": source["kind"], "unit": source["unit"], "supported": source["supported"],
			"credentialRef": source["credentialRef"], "accountLabel": source["accountLabel"],
			"officialPage": source["officialPage"], "consoleUrl": source["consoleUrl"],
			"status": source["status"], "credentialSet": source["credentialSet"],
			"errorReason": source["errorReason"], "removable": source["removable"],
			"officialUrl": source["officialUrl"], "notes": source["notes"],
		}
		if source["adapter"] == "custom" {
			projected["method"] = source["method"]
			projected["endpoint"] = source["endpoint"]
			projected["host"] = source["host"]
			projected["fields"] = source["fields"]
			projected["auth"] = source["auth"]
			projected["windowLabel"] = source["windowLabel"]
		}
		out = append(out, projected)
	}
	return out
}

// The panel-action golden is the legacy controller's own answer to the body the
// existing UI sends. It is a separate fixture from the State comparison because
// it exercises the input translation, not the reading.
const (
	panelActionCasesFile    = "testdata/panel-action-cases.json"
	panelActionLegacyGolden = "testdata/panel-action-observations.json"
)

type panelActionCase struct {
	Label       string              `json:"label"`
	Path        string              `json:"path"`
	Input       map[string]any      `json:"input"`
	Connections []fixtureConnection `json:"connections"`
	Reply       map[string]any      `json:"reply"`
	Setup       struct {
		Sources []map[string]any `json:"sources"`
	} `json:"setup"`
}

type legacyPanelAction struct {
	Cases []struct {
		Label  string         `json:"label"`
		Path   string         `json:"path"`
		Status *int           `json:"status"`
		Code   string         `json:"code"`
		OK     map[string]any `json:"ok"`
	} `json:"cases"`
}

// TestDualRunMatchesLegacyPanelAction compares PanelAction with the legacy
// controller's refreshQuotas / configureQuotaSource, including the status and
// error code each refusal produced.
func TestDualRunMatchesLegacyPanelAction(t *testing.T) {
	if _, err := os.Stat(panelActionLegacyGolden); err != nil {
		t.Skip("legacy panel-action observations not generated")
	}
	raw, err := os.ReadFile(panelActionCasesFile)
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}
	var fixtures struct {
		Cases []panelActionCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode cases: %v", err)
	}
	rawGolden, err := os.ReadFile(panelActionLegacyGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var legacy legacyPanelAction
	if err := json.Unmarshal(rawGolden, &legacy); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if len(fixtures.Cases) != len(legacy.Cases) {
		t.Fatalf("case count %d want %d", len(fixtures.Cases), len(legacy.Cases))
	}
	for index, testCase := range fixtures.Cases {
		testCase := testCase
		want := legacy.Cases[index]
		t.Run(testCase.Label, func(t *testing.T) {
			if want.Label != testCase.Label {
				t.Fatalf("golden order changed: %q at %d", want.Label, index)
			}
			got, err := runPanelAction(t, testCase)
			if want.Status != nil {
				// The legacy side refused; this package must refuse with the same
				// status and the same code.
				if err == nil {
					t.Fatalf("accepted; the legacy side answered %d %s", *want.Status, want.Code)
				}
				refusal, ok := err.(*quota.PanelActionError)
				if !ok {
					t.Fatalf("error %v is not a PanelActionError", err)
				}
				if refusal.Status != *want.Status || refusal.Code != want.Code {
					t.Fatalf("refusal %d %s want %d %s", refusal.Status, refusal.Code,
						*want.Status, want.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused with %v; the legacy side accepted", err)
			}
			if !reflect.DeepEqual(got, want.OK) {
				t.Fatalf("outcome differs\n got %s\nwant %s", prettyQuota(got), prettyQuota(want.OK))
			}
		})
	}
}

// runPanelAction drives one case through PanelAction and projects the outcome to
// the fields the legacy observation recorded.
func runPanelAction(t *testing.T, testCase panelActionCase) (map[string]any, error) {
	t.Helper()
	clock := int64(1794196800000)
	transport := &scriptedTransport{reply: testCase.Reply}
	accounts := quota.New(quota.Options{
		Credentials: func(ref string) string {
			if strings.HasPrefix(ref, "connection:") {
				id := strings.TrimPrefix(ref, "connection:")
				for _, connection := range testCase.Connections {
					if connection.ID == id {
						if connection.HasCredential {
							return dualKeys["custom"]
						}
						return ""
					}
				}
				return ""
			}
			return dualKeys[ref]
		},
		HTTPClient:  transport.client(),
		Now:         func() int64 { return clock },
		FreshnessMS: 15 * 60 * 1000,
		// The legacy panel used the documented throttle; the fixture's scripted
		// transport answers instantly, so a second read inside the window would
		// be throttled on both sides equally.
		ThrottleMS: -1,
	})
	for _, source := range testCase.Setup.Sources {
		if _, err := accounts.AddSource(source); err != nil {
			t.Fatalf("setup source %v: %v", source["sourceId"], err)
		}
	}
	connections := make([]quota.ConnectionView, 0, len(testCase.Connections))
	for _, connection := range testCase.Connections {
		view := quota.ConnectionView{ID: connection.ID, Name: connection.Name,
			HasCredential: connection.HasCredential}
		if connection.ProviderPreset != "" {
			preset := connection.ProviderPreset
			view.ProviderPreset = &preset
		}
		connections = append(connections, view)
	}
	result, err := accounts.PanelAction(context.Background(), testCase.Path, testCase.Input, connections)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]any{
		"sources":     projectActionSources(decoded["sources"]),
		"records":     projectActionRecords(decoded["records"]),
		"combining":   decoded["combining"],
		"lastRefresh": projectLastRefresh(decoded["lastRefresh"]),
	}
	return out, nil
}

func projectActionSources(raw any) []any {
	out := []any{}
	for _, entry := range asSlice(raw) {
		source, _ := entry.(map[string]any)
		out = append(out, map[string]any{
			"sourceId": source["sourceId"], "adapter": source["adapter"], "kind": source["kind"],
			"unit": source["unit"], "credentialRef": source["credentialRef"],
			"status": source["status"], "credentialSet": source["credentialSet"],
			"removable": source["removable"], "officialUrl": source["officialUrl"],
			"endpoint": nilOr(source["endpoint"]), "method": nilOr(source["method"]),
			"windowLabel": nilOr(source["windowLabel"]),
			"fields":      nilOr(source["fields"]), "auth": nilOr(source["auth"]),
		})
	}
	return out
}

func projectActionRecords(raw any) []any {
	out := []any{}
	for _, entry := range asSlice(raw) {
		record, _ := entry.(map[string]any)
		out = append(out, map[string]any{
			"sourceId": record["sourceId"], "remaining": record["remaining"],
			"limit": record["limit"], "used": record["used"], "kind": record["kind"],
			"unit": record["unit"],
		})
	}
	return out
}

func projectLastRefresh(raw any) any {
	if raw == nil {
		return nil
	}
	report, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	results := []any{}
	for _, entry := range asSlice(report["results"]) {
		row, _ := entry.(map[string]any)
		results = append(results, map[string]any{
			"sourceId": row["sourceId"], "status": row["status"],
			"errorReason": nilOr(row["errorReason"]),
		})
	}
	return map[string]any{"results": results}
}

// nilOr keeps a missing field absent rather than turning it into a typed nil.
func nilOr(value any) any {
	if value == nil {
		return nil
	}
	return value
}

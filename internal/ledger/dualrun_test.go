package ledger_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/ledger"
)

// This file is the usage-ledger dual run. The same fixture entries are written
// through the legacy 0.12.1 ledger (testdata/legacy-run.mjs) and through this
// package, and the published query payloads are compared field by field.
//
// Regenerate the legacy side with:
//
//	bun internal/ledger/testdata/legacy-run.mjs \
//	    internal/ledger/testdata/ledger-cases.json \
//	    internal/ledger/testdata/legacy-observations.json

const (
	ledgerCasesFile    = "testdata/ledger-cases.json"
	ledgerLegacyGolden = "testdata/legacy-observations.json"
)

type ledgerFixture struct {
	SchemaVersion int              `json:"schemaVersion"`
	Entries       []map[string]any `json:"entries"`
	Queries       []queryCase      `json:"queries"`
	Errors        []queryCase      `json:"errors"`
}

type queryCase struct {
	Label string         `json:"label"`
	Query map[string]any `json:"query"`
}

type legacyLedger struct {
	Digest  string         `json:"digest"`
	Status  legacyStatus   `json:"status"`
	Queries []legacyQuery  `json:"queries"`
	Errors  []legacyQuery  `json:"errors"`
	Export  map[string]any `json:"export"`
}

type legacyStatus struct {
	State         string `json:"state"`
	Records       int    `json:"records"`
	SkippedLines  int    `json:"skippedLines"`
	SchemaVersion int    `json:"schemaVersion"`
}

type legacyQuery struct {
	Label  string         `json:"label"`
	Result map[string]any `json:"result"`
	Error  string         `json:"error"`
}

func loadLedgerFixtures(t *testing.T) ledgerFixture {
	t.Helper()
	raw, err := os.ReadFile(ledgerCasesFile)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixture ledgerFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fixture
}

func loadLegacyLedger(t *testing.T) legacyLedger {
	t.Helper()
	raw, err := os.ReadFile(ledgerLegacyGolden)
	if err != nil {
		t.Fatalf("read legacy observations: %v (run the bun command in this file's comment)", err)
	}
	var document legacyLedger
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode legacy observations: %v", err)
	}
	return document
}

// writeFixtureEntries feeds the shared entries through this package's ledger.
func writeFixtureEntries(t *testing.T, fixture ledgerFixture) *ledger.Ledger {
	t.Helper()
	file := filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	for _, raw := range fixture.Entries {
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("encode entry: %v", err)
		}
		var entry ledger.Entry
		if err := json.Unmarshal(encoded, &entry); err != nil {
			t.Fatalf("decode entry: %v", err)
		}
		if result := instance.Record(entry); !result.OK {
			t.Fatalf("record %v refused: %s", raw["operationId"], result.Reason)
		}
	}
	return instance
}

// toGoQuery converts a fixture query into this package's typed query.
func toGoQuery(t *testing.T, raw map[string]any) ledger.Query {
	t.Helper()
	query := ledger.Query{}
	if value, ok := raw["preset"].(string); ok {
		query.Preset = value
	}
	if value, ok := raw["timeZone"].(string); ok {
		query.TimeZone = value
	}
	query.From = raw["from"]
	query.To = raw["to"]
	if value, ok := raw["now"].(float64); ok {
		now := int64(value)
		query.Now = &now
	}
	if value, ok := raw["limit"].(float64); ok {
		query.Limit = int(value)
	}
	if filters, ok := raw["filters"].(map[string]any); ok {
		query.Filters = map[string]string{}
		for key, entry := range filters {
			if text, ok := entry.(string); ok {
				query.Filters[key] = text
			}
		}
	}
	return query
}

func TestDualRunMatchesLegacyLedger(t *testing.T) {
	if _, err := os.Stat(ledgerLegacyGolden); err != nil {
		t.Skip("legacy observations not generated; run the bun command in this file's comment")
	}
	fixture := loadLedgerFixtures(t)
	legacy := loadLegacyLedger(t)
	instance := writeFixtureEntries(t, fixture)

	if got := instance.State(); got != legacy.Status.State {
		t.Fatalf("state %q want %q", got, legacy.Status.State)
	}
	if got := len(instance.Records()); got != legacy.Status.Records {
		t.Fatalf("record count %d want %d", got, legacy.Status.Records)
	}
	if got := instance.Status(); got.SkippedLines != legacy.Status.SkippedLines ||
		got.SchemaVersion != legacy.Status.SchemaVersion {
		t.Fatalf("status %+v want %+v", got, legacy.Status)
	}
	// The account digest is the same on both sides, which is what makes a
	// filtered query comparable.
	if got := ledger.RedactIdentifier("synthetic-account-label"); got != legacy.Digest {
		t.Fatalf("digest %q want %q", got, legacy.Digest)
	}

	for index, testCase := range fixture.Queries {
		testCase := testCase
		t.Run(testCase.Label, func(t *testing.T) {
			want := legacy.Queries[index]
			if want.Label != testCase.Label {
				t.Fatalf("legacy observation order changed: %q at %d", want.Label, index)
			}
			query := toGoQuery(t, resolveDigest(testCase.Query, legacy.Digest))
			result, err := instance.Query(query)
			if err != nil {
				t.Fatalf("query failed: %v", err)
			}
			compareQuery(t, toMap(t, result), want.Result)
		})
	}

	for index, testCase := range fixture.Errors {
		testCase := testCase
		t.Run("error/"+testCase.Label, func(t *testing.T) {
			want := legacy.Errors[index]
			if want.Error == "" {
				t.Fatalf("the legacy run accepted %q", testCase.Label)
			}
			_, err := instance.Query(toGoQuery(t, resolveDigest(testCase.Query, legacy.Digest)))
			if err == nil {
				t.Fatalf("%q was accepted; the legacy side reported %q", testCase.Label, want.Error)
			}
			if err.Error() != want.Error {
				t.Fatalf("%q reported %q want %q", testCase.Label, err.Error(), want.Error)
			}
		})
	}
}

func resolveDigest(raw map[string]any, digest string) map[string]any {
	out := map[string]any{}
	for key, value := range raw {
		if key == "filters" {
			filters := map[string]any{}
			for name, entry := range value.(map[string]any) {
				if text, ok := entry.(string); ok && text == "__REDACTED_ACCOUNT__" {
					filters[name] = digest
					continue
				}
				filters[name] = entry
			}
			out[key] = filters
			continue
		}
		out[key] = value
	}
	return out
}

func toMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// compareQuery checks the fields a UI reads. The bucket list is compared whole
// because a bucket boundary that moved is a visible regression.
func compareQuery(t *testing.T, got, want map[string]any) {
	t.Helper()
	for _, field := range []string{"schemaVersion", "range", "summary", "recordsTotal",
		"recordsTruncated", "filters", "coverage", "buckets", "records"} {
		if !reflect.DeepEqual(got[field], want[field]) {
			t.Fatalf("%s differs\n got %s\nwant %s", field, prettyJSON(got[field]), prettyJSON(want[field]))
		}
	}
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(value, " ", " ")
	if err != nil {
		return "<?>"
	}
	return string(encoded)
}

// TestLegacyLedgerObservationsAreReproducible re-runs the legacy ledger when bun
// is available, so the committed observation file cannot drift.
func TestLegacyLedgerObservationsAreReproducible(t *testing.T) {
	if os.Getenv("DUAL_RUN_REGEN") != "1" {
		t.Skip("set DUAL_RUN_REGEN=1 to re-run the legacy ledger")
	}
	bun := findLedgerBun()
	if bun == "" {
		t.Skip("no bun executable found")
	}
	output := filepath.Join(t.TempDir(), "legacy.json")
	command := exec.Command(bun, "run", "testdata/legacy-run.mjs", ledgerCasesFile, output)
	command.Dir = "."
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("legacy run failed: %v\n%s", err, combined)
	}
	fresh, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	committed, err := os.ReadFile(ledgerLegacyGolden)
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}
	if string(fresh) != string(committed) {
		t.Fatalf("the committed observations no longer match a fresh run")
	}
}

func findLedgerBun() string {
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

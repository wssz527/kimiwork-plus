package ledger_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/ledger"
)

const shanghai = "Asia/Shanghai"

func workspace(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
}

func str(value string) *string { return &value }
func num(value int64) *int64   { return &value }

func open(t *testing.T, file string) *ledger.Ledger {
	t.Helper()
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	return instance
}

func mustRecord(t *testing.T, instance *ledger.Ledger, entry ledger.Entry) ledger.Result {
	t.Helper()
	result := instance.Record(entry)
	if !result.OK {
		t.Fatalf("record refused: %+v", result)
	}
	return result
}

// --- normalisation ---------------------------------------------------------

func TestAnthropicInputIsOrdinaryPlusCacheReadPlusCacheCreation(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	result := mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-06T02:00:00.000Z",
		Status: "completed", UsageProtocol: "anthropic", Usage: map[string]any{
			"input_tokens": float64(10), "output_tokens": float64(5), "cache_read_input_tokens": float64(90),
			"cache_creation_input_tokens": float64(20)}})
	if result.Record["inputTokens"] != int64(120) || result.Record["outputTokens"] != int64(5) ||
		result.Record["totalTokens"] != int64(125) || result.Record["cacheReadTokens"] != int64(90) ||
		result.Record["cacheWriteTokens"] != int64(20) {
		t.Fatalf("flat usage %v", result.Record)
	}
	// Newer replies nest the breakdown per cache lifetime; only the parts that
	// are really there are summed, and a missing breakdown stays unknown.
	nested := mustRecord(t, instance, ledger.Entry{OperationID: "op-b", At: "2026-11-06T03:00:00.000Z",
		UsageProtocol: "anthropic", Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5),
			"cache_creation": map[string]any{"ephemeral_5m_input_tokens": float64(7), "ephemeral_1h_input_tokens": float64(3)}}})
	if nested.Record["inputTokens"] != int64(20) || nested.Record["cacheWriteTokens"] != int64(10) {
		t.Fatalf("nested usage %v", nested.Record)
	}
	if nested.Record["cacheReadTokens"] != nil {
		t.Fatalf("an absent cache read became a value: %v", nested.Record["cacheReadTokens"])
	}
	bare := mustRecord(t, instance, ledger.Entry{OperationID: "op-c", At: "2026-11-06T04:00:00.000Z",
		UsageProtocol: "anthropic", Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)}})
	if bare.Record["cacheWriteTokens"] != nil {
		t.Fatalf("absent cache creation became a value")
	}
}

func TestOpenAICachedPromptIsASubsetNotAnExtraCharge(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	result := mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-06T02:00:00.000Z",
		Usage: map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(20),
			"total_tokens": float64(120), "prompt_tokens_details": map[string]any{"cached_tokens": float64(60)}}})
	if result.Record["inputTokens"] != int64(100) || result.Record["totalTokens"] != int64(120) ||
		result.Record["cacheReadTokens"] != int64(60) {
		t.Fatalf("usage %v", result.Record)
	}
	// No reported total and no output count: the total is not invented.
	partial := mustRecord(t, instance, ledger.Entry{OperationID: "op-b", At: "2026-11-06T03:00:00.000Z",
		Usage: map[string]any{"prompt_tokens": float64(100)}})
	if partial.Record["totalTokens"] != nil || partial.Record["outputTokens"] != nil {
		t.Fatalf("a total was invented: %v", partial.Record)
	}
}

// --- the ledger ------------------------------------------------------------

func TestThreeRequestsAggregateOnlyWhatWasObserved(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", Source: "gateway", At: "2026-11-06T02:00:00.000Z",
		Status: "completed", ModelID: str("claude-sonnet"), ModelAlias: str("byok-claude"),
		ProviderID: str("anthropic"), ConnectionID: str("work"), UsageProtocol: "anthropic",
		Usage:     map[string]any{"input_tokens": float64(1000), "output_tokens": float64(200), "cache_read_input_tokens": float64(500)},
		LatencyMS: num(900), HTTPStatus: num(200)})
	mustRecord(t, instance, ledger.Entry{OperationID: "op-b", Source: "gateway", At: "2026-11-07T03:00:00.000Z",
		Status: "completed", ModelID: str("gpt-sol"), ProviderID: str("openai"), ConnectionID: str("home"),
		Usage: map[string]any{"prompt_tokens": float64(40), "completion_tokens": float64(10),
			"total_tokens": float64(50), "prompt_tokens_details": map[string]any{"cached_tokens": float64(30)}},
		LatencyMS: num(300), HTTPStatus: num(200)})
	// A request whose usage was never reported is a counted request, not a
	// zero-token one: its token fields are null and the summary stays null.
	bare := mustRecord(t, instance, ledger.Entry{OperationID: "op-c", Source: "gateway", At: "2026-11-07T04:00:00.000Z",
		Status: "failed", ModelID: str("gpt-sol"), ProviderID: str("openai"), ConnectionID: str("home"),
		ErrorCode: str("upstream_rate_limited"), HTTPStatus: num(429), LatencyMS: num(40)})
	if bare.Record["dataSource"] != "request_only" || bare.Record["inputTokens"] != nil ||
		bare.Record["totalTokens"] != nil {
		t.Fatalf("bare record %v", bare.Record)
	}

	now := int64(1794196800000) // 2026-11-09T04:00:00Z
	result, err := instance.Query(ledger.Query{Preset: "30d", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.SchemaVersion != ledger.SchemaVersion || result.Range.Unit != "day" ||
		result.Range.TimeZone != shanghai {
		t.Fatalf("range %+v", result.Range)
	}
	// Requests, requests with evidence and errors are three separate counts.
	if result.Summary.Requests != 3 || result.Summary.RequestsWithUsage != 2 || result.Summary.Errors != 1 {
		t.Fatalf("summary counts %+v", result.Summary)
	}
	if *result.Summary.InputTokens != 1000+500+40 || *result.Summary.OutputTokens != 210 ||
		*result.Summary.TotalTokens != 1500+200+50 || *result.Summary.CacheReadTokens != 530 {
		t.Fatalf("summary tokens %+v", result.Summary)
	}
	if result.Summary.CacheWriteTokens != nil || result.Summary.EstimatedCost != nil {
		t.Fatalf("an unknown became a value: %+v", result.Summary)
	}
	if result.Summary.DataSources["upstream_usage"] != 2 || result.Summary.DataSources["request_only"] != 1 {
		t.Fatalf("data sources %v", result.Summary.DataSources)
	}
	if result.Summary.ByStatus["completed"] != 2 || result.Summary.ByStatus["failed"] != 1 {
		t.Fatalf("by status %v", result.Summary.ByStatus)
	}
	// Detail rows are newest first and carry no conversation content.
	order := []string{}
	for _, row := range result.Records {
		order = append(order, row["operationId"].(string))
	}
	if strings.Join(order, ",") != "op-c,op-b,op-a" {
		t.Fatalf("order %v", order)
	}
	// Every published field is always present, so a consumer never has to tell
	// a missing field from an unknown value.
	if len(result.Records[0]) != len(result.Records[2]) {
		t.Fatalf("record shapes differ: %v vs %v", result.Records[0], result.Records[2])
	}
	if result.Records[2]["cacheWriteTokens"] != nil || result.Records[2]["account"] != nil ||
		result.Records[2]["conversationId"] != nil || result.Records[2]["priceSource"] != nil {
		t.Fatalf("unexpected values %v", result.Records[2])
	}
	// Every bucket of the range exists, including the empty ones.
	if len(result.Buckets) != 30 {
		t.Fatalf("buckets %d", len(result.Buckets))
	}
	populated := 0
	for _, bucket := range result.Buckets {
		if bucket.Requests > 0 {
			populated++
			continue
		}
		if bucket.InputTokens != nil || bucket.OutputTokens != nil {
			t.Fatalf("an empty bucket has token evidence: %+v", bucket)
		}
	}
	if populated != 2 {
		t.Fatalf("populated buckets %d", populated)
	}
	total := 0
	for _, bucket := range result.Buckets {
		total += bucket.Requests
	}
	if total != result.Summary.Requests {
		t.Fatalf("buckets and summary disagree: %d vs %d", total, result.Summary.Requests)
	}
	if result.Coverage.Pricing != "not_provided" || result.Coverage.RecordsInLedger != 3 ||
		result.Coverage.RecordsInRange != 3 || *result.Coverage.RecordingSince != "2026-11-06T02:00:00.000Z" {
		t.Fatalf("coverage %+v", result.Coverage)
	}
}

func TestPeriodicCompactionSkipsCleanHistory(t *testing.T) {
	file := workspace(t)
	first := open(t, file)
	mustRecord(t, first, ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z", Status: "completed"})
	instance, err := ledger.New(ledger.Options{File: file, CompactEvery: 1})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	mustRecord(t, instance, ledger.Entry{OperationID: "op-b", At: "2026-11-08T03:00:00.000Z", Status: "completed"})
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("clean history was rewritten")
	}
}

func TestPeriodicCompactionRemovesMergedHistory(t *testing.T) {
	file := workspace(t)
	first := open(t, file)
	mustRecord(t, first, ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z", Status: "started"})
	instance, err := ledger.New(ledger.Options{File: file, CompactEvery: 1})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z", Status: "completed"})
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("merged history was not compacted")
	}
	if records := open(t, file).Records(); len(records) != 1 || records[0].Status != "completed" {
		t.Fatalf("compacted history %+v", records)
	}
}

func TestRepeatedTerminalReceiptIsMergedNotCountedTwice(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	entry := ledger.Entry{OperationID: "op-1", At: "2026-11-08T02:00:00.000Z", ModelID: str("claude-sonnet"),
		LatencyMS: num(120), HTTPStatus: num(200),
		Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)}}
	started := entry
	started.Status = "started"
	mustRecord(t, instance, started)
	done := entry
	done.Status = "completed"
	first := mustRecord(t, instance, done)
	if !first.Duplicate || !first.Changed {
		t.Fatalf("terminal merge %+v", first)
	}
	// The same final event arriving again changes nothing.
	again := mustRecord(t, instance, done)
	if !again.Duplicate || again.Changed {
		t.Fatalf("repeat merge %+v", again)
	}
	if len(instance.Records()) != 1 {
		t.Fatalf("records %d", len(instance.Records()))
	}
	stored := instance.Records()[0]
	if stored.Status != "completed" || *stored.InputTokens != 10 || *stored.TotalTokens != 15 {
		t.Fatalf("stored %+v", stored)
	}
	// A different source is a different operation, even with the same id.
	other := entry
	other.Source = "panel"
	other.Status = "completed"
	mustRecord(t, instance, other)
	if len(instance.Records()) != 2 {
		t.Fatalf("a different source merged into the same record")
	}
}

func TestRestartReadsTheSameHistoryAndADamagedLineDoesNotLoseTheRest(t *testing.T) {
	file := workspace(t)
	first := open(t, file)
	mustRecord(t, first, ledger.Entry{OperationID: "op-a", At: "2026-11-07T02:00:00.000Z", Status: "completed",
		ModelID: str("claude-sonnet"), Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)}})
	mustRecord(t, first, ledger.Entry{OperationID: "op-b", At: "2026-11-08T02:00:00.000Z", Status: "completed",
		ModelID: str("gpt-sol"), Usage: map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(1)}})
	now := int64(1794196800000)
	before, err := first.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	rebuilt := open(t, file)
	if rebuilt.State() != "ok" {
		t.Fatalf("state %q", rebuilt.State())
	}
	after, err := rebuilt.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	beforeJSON, _ := json.Marshal(before.Summary)
	afterJSON, _ := json.Marshal(after.Summary)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("summary changed across a restart:\n%s\n%s", beforeJSON, afterJSON)
	}
	if len(after.Records) != len(before.Records) {
		t.Fatalf("records changed across a restart")
	}
	// The file is private to the user.
	stat, err := os.Stat(file)
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

	// A truncated tail is skipped, everything before it still loads, and the
	// damage stays visible in the status instead of silently shrinking history.
	handle, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = handle.WriteString(`{"operationId":"op-c","at":"2026-11-08T0`)
	_ = handle.Close()
	damaged := open(t, file)
	if len(damaged.Records()) != 2 || damaged.State() != "corrupt" {
		t.Fatalf("damaged state %q records %d", damaged.State(), len(damaged.Records()))
	}
	if status := damaged.Status(); status.SkippedLines != 1 || status.Records != 2 {
		t.Fatalf("status %+v", status)
	}
	// And the next append starts on a clean line.
	mustRecord(t, damaged, ledger.Entry{OperationID: "op-d", At: "2026-11-08T05:00:00.000Z",
		Usage: map[string]any{"input_tokens": float64(1)}})
	order := []string{}
	for _, record := range open(t, file).Records() {
		order = append(order, record.OperationID)
	}
	if strings.Join(order, ",") != "op-a,op-b,op-d" {
		t.Fatalf("order after repair %v", order)
	}
}

func TestUnwritableLocationKeepsTheInMemoryHistory(t *testing.T) {
	file := workspace(t)
	// The ledger path is a directory: nothing can be written there.
	if err := os.MkdirAll(file, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	instance := open(t, file)
	result := instance.Record(ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z",
		Status: "completed", Usage: map[string]any{"input_tokens": float64(7)}})
	if !result.OK {
		t.Fatalf("a write failure cost the caller its answer: %+v", result)
	}
	if len(instance.Records()) != 1 {
		t.Fatalf("history %d", len(instance.Records()))
	}
	if instance.State() != "unsafe_file" {
		t.Fatalf("state %q", instance.State())
	}
}

func TestRetentionAndClearBoundTheHistory(t *testing.T) {
	file := workspace(t)
	now := int64(1794196800000)
	instance, err := ledger.New(ledger.Options{File: file, MaxRecords: 2, MaxAgeDays: 30,
		Now: func() int64 { return now }})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	mustRecord(t, instance, ledger.Entry{OperationID: "op-old", At: "2026-09-01T00:00:00.000Z",
		Usage: map[string]any{"input_tokens": float64(1)}})
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-07T02:00:00.000Z",
		Usage: map[string]any{"input_tokens": float64(10)}})
	mustRecord(t, instance, ledger.Entry{OperationID: "op-b", At: "2026-11-08T02:00:00.000Z",
		Usage: map[string]any{"input_tokens": float64(20)}})
	order := []string{}
	for _, record := range instance.Records() {
		order = append(order, record.OperationID)
	}
	if strings.Join(order, ",") != "op-a,op-b" {
		t.Fatalf("retention %v", order)
	}
	reopened, err := ledger.New(ledger.Options{File: file, MaxRecords: 2, MaxAgeDays: 30,
		Now: func() int64 { return now }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(reopened.Records()) != 2 {
		t.Fatalf("a pruned record was written back")
	}
	cleared := instance.Clear("2026-11-08", shanghai)
	if !cleared.OK || cleared.Removed != 1 {
		t.Fatalf("clear %+v", cleared)
	}
	if len(instance.Records()) != 1 || instance.Records()[0].OperationID != "op-b" {
		t.Fatalf("after clear %v", instance.Records())
	}
	if again := instance.Clear(nil, ""); !again.OK || again.Removed != 1 {
		t.Fatalf("clear all %+v", again)
	}
	if len(open(t, file).Records()) != 0 {
		t.Fatalf("a cleared record came back")
	}
}

func TestExportIsTheSameRedactedSet(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z",
		Status: "completed", ModelID: str("claude-sonnet"),
		Account: str("synthetic-account-label"), APIKeyID: str("synthetic-key-label"),
		ConversationID: str("conv-77"), Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)}})
	now := int64(1794196800000)
	raw, err := instance.ExportJSON(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var exported map[string]any
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rows, _ := exported["records"].([]any)
	if len(rows) != 1 {
		t.Fatalf("records %v", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["account"] != ledger.RedactIdentifier("synthetic-account-label") || row["conversationId"] != "conv-77" {
		t.Fatalf("row %v", row)
	}
	for _, secret := range []string{"synthetic-account-label", "synthetic-key-label"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("export leaked %q", secret)
		}
	}
	// The raw file is redacted too, not only the export.
	stored, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(stored), "synthetic-account-label") {
		t.Fatalf("the file holds the raw account label")
	}
}

func TestFiltersReduceEverythingTogetherAndListTheWholeRange(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-08T02:00:00.000Z",
		ModelID: str("claude-sonnet"), ModelAlias: str("byok-claude"), ProviderID: str("anthropic"),
		ConnectionID: str("work"), ConversationID: str("conv-1"),
		Usage: map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)}})
	mustRecord(t, instance, ledger.Entry{OperationID: "op-b", At: "2026-11-08T03:00:00.000Z",
		ModelID: str("gpt-sol"), ProviderID: str("openai"), ConnectionID: str("home"),
		Usage: map[string]any{"prompt_tokens": float64(20), "completion_tokens": float64(1)}})
	now := int64(1794196800000)
	all, err := instance.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all.Filters.Connections) != 2 || all.Filters.Connections[0].Value != "home" {
		t.Fatalf("connections %v", all.Filters.Connections)
	}
	if len(all.Filters.Providers) != 2 || all.Filters.Models[0].Value != "claude-sonnet" {
		t.Fatalf("providers %v models %v", all.Filters.Providers, all.Filters.Models)
	}
	filtered, err := instance.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now,
		Filters: map[string]string{"modelId": "claude-sonnet"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if filtered.Summary.Requests != 1 || *filtered.Summary.InputTokens != 10 {
		t.Fatalf("filtered summary %+v", filtered.Summary)
	}
	total := 0
	for _, bucket := range filtered.Buckets {
		total += bucket.Requests
	}
	if total != 1 {
		t.Fatalf("buckets ignored the filter: %d", total)
	}
	// The available options are listed for the whole range, not the current
	// filter: otherwise the filter could never be widened again.
	if len(filtered.Filters.Connections) != 2 {
		t.Fatalf("filter options narrowed to the filter: %v", filtered.Filters.Connections)
	}
	// The alias selects the same request as the upstream model id.
	byAlias, err := instance.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now,
		Filters: map[string]string{"modelId": "byok-claude"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if byAlias.Summary.Requests != 1 {
		t.Fatalf("alias filter %+v", byAlias.Summary)
	}
	if _, err := instance.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now,
		Filters: map[string]string{"at": "x"}}); err != ledger.ErrInvalidFilter {
		t.Fatalf("an unknown filter field was accepted: %v", err)
	}
}

func TestBucketBoundariesFollowTheLocalCalendar(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	// Spring forward: the local day has 23 hours.
	springNow := int64(1773028800000) // 2026-03-09T04:00:00Z
	spring, err := instance.Query(ledger.Query{Preset: "today", TimeZone: "America/New_York", Now: &springNow})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(spring.Buckets) != 24 && len(spring.Buckets) != 23 {
		t.Fatalf("spring buckets %d", len(spring.Buckets))
	}
	// Fall back: 25 hours, and the repeated hour is told apart by its offset.
	fall, err := instance.Query(ledger.Query{Preset: "custom", From: "2026-11-01", To: "2026-11-01",
		TimeZone: "America/New_York", Now: &springNow})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(fall.Buckets) != 25 {
		t.Fatalf("fall buckets %d", len(fall.Buckets))
	}
	repeated := []int{}
	for _, bucket := range fall.Buckets {
		if bucket.LocalStart == "2026-11-01T01:00" {
			repeated = append(repeated, bucket.UTCOffsetMinutes)
		}
	}
	if len(repeated) != 2 || repeated[0] != -240 || repeated[1] != -300 {
		t.Fatalf("repeated hour %v", repeated)
	}
	// The unit of a custom range follows its length.
	now := int64(1794196800000)
	cases := [][3]string{
		{"2026-11-01", "2026-11-01", "hour"},
		{"2026-10-01", "2026-11-05", "day"},
		{"2026-06-01", "2026-08-01", "day"},
		{"2026-01-01", "2026-11-01", "week"},
	}
	for _, test := range cases {
		result, err := instance.Query(ledger.Query{Preset: "custom", From: test[0], To: test[1],
			TimeZone: shanghai, Now: &now})
		if err != nil {
			t.Fatalf("%v: %v", test, err)
		}
		if result.Range.Unit != test[2] {
			t.Fatalf("%v: unit %q", test, result.Range.Unit)
		}
	}
	if _, err := instance.Query(ledger.Query{Preset: "custom", From: "2026-11-05", To: "2026-11-01"}); err != ledger.ErrInvalidRange {
		t.Fatalf("a reversed range was accepted: %v", err)
	}
	if _, err := instance.Query(ledger.Query{Preset: "today", TimeZone: "Not/AZone"}); err != ledger.ErrInvalidTimeZone {
		t.Fatalf("an unknown zone was accepted: %v", err)
	}
}

func TestAnEventStoredInUTCLandsInItsLocalDay(t *testing.T) {
	file := workspace(t)
	instance := open(t, file)
	// 16:30Z is 00:30 the next day in Shanghai.
	mustRecord(t, instance, ledger.Entry{OperationID: "op-a", At: "2026-11-08T16:30:00.000Z",
		Usage: map[string]any{"input_tokens": float64(4)}})
	now := int64(1794196800000)
	today, err := instance.Query(ledger.Query{Preset: "today", TimeZone: shanghai, Now: &now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	populated := []string{}
	for _, bucket := range today.Buckets {
		if bucket.Requests > 0 {
			populated = append(populated, bucket.LocalStart)
		}
	}
	if len(populated) != 1 || populated[0] != "2026-11-09T00:00" {
		t.Fatalf("buckets %v", populated)
	}
	// The raw event time is kept as the UTC instant it happened at.
	if today.Records[0]["at"] != "2026-11-08T16:30:00.000Z" {
		t.Fatalf("record at %v", today.Records[0]["at"])
	}
	utcNow := int64(1794168000000) // 2026-11-08T20:00:00Z
	utc, err := instance.Query(ledger.Query{Preset: "today", TimeZone: "UTC", Now: &utcNow})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, bucket := range utc.Buckets {
		if bucket.Requests > 0 && bucket.LocalStart != "2026-11-08T16:00" {
			t.Fatalf("utc bucket %q", bucket.LocalStart)
		}
	}
}

func TestRedactionIsStableAndNotReversible(t *testing.T) {
	first := ledger.RedactIdentifier("synthetic-account-label")
	if !strings.HasPrefix(first, "id_") || len(first) != 15 {
		t.Fatalf("digest %q", first)
	}
	if ledger.RedactIdentifier("synthetic-account-label") != first {
		t.Fatalf("digest is not stable")
	}
	if ledger.RedactIdentifier("another-label") == first {
		t.Fatalf("two labels share a digest")
	}
	if ledger.RedactIdentifier("") != "" || ledger.RedactIdentifier(strings.Repeat("x", 5000)) != "" {
		t.Fatalf("an unusable value produced a digest")
	}
}

func TestNormalizationRedactsBeforeIdentifierValidation(t *testing.T) {
	instance := open(t, workspace(t))
	account := strings.Repeat("account with spaces ", 20)
	key := "sk-\n" + strings.Repeat("special:key ", 20)
	record, err := instance.Normalize(ledger.Entry{OperationID: "op-redaction", Account: &account, APIKeyID: &key})
	if err != nil {
		t.Fatal(err)
	}
	if record["account"] != ledger.RedactIdentifier(account) || record["apiKeyId"] != ledger.RedactIdentifier(key) {
		t.Fatalf("redacted identifiers = %#v", record)
	}
	if strings.Contains(fmt.Sprint(record), account) || strings.Contains(fmt.Sprint(record), key) {
		t.Fatal("raw identifiers survived normalization")
	}
}

func TestLedgerConcurrentQueryRecordAndClear(t *testing.T) {
	file := workspace(t)
	now := int64(1794196800000)
	instance, err := ledger.New(ledger.Options{
		File:       file,
		MaxRecords: 200,
		Now:        func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := instance.Query(ledger.Query{Preset: "7d", TimeZone: shanghai, Now: &now}); err != nil {
						t.Errorf("query: %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 120; i++ {
			id := fmt.Sprintf("op-%d", i%30)
			status := "started"
			if i%2 == 1 {
				status = "completed"
			}
			_ = instance.Record(ledger.Entry{
				OperationID: id,
				At:          now - int64(i)*1000,
				Status:      status,
				ModelID:     str("model-a"),
				PriceSource: str("table"),
				Usage:       map[string]any{"input_tokens": float64(10 + i), "output_tokens": float64(5)},
			})
			if i%25 == 0 {
				_ = instance.Clear(nil, "")
			}
		}
		close(stop)
	}()
	wg.Wait()
}

func TestLedgerSkipsOversizedCorruptLineWithoutLosingLaterRecords(t *testing.T) {
	file := workspace(t)
	now := int64(1794196800000)
	first, err := ledger.New(ledger.Options{
		File:         file,
		MaxFileBytes: 4096,
		Now:          func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	mustRecord(t, first, ledger.Entry{OperationID: "op-before", At: "2026-11-08T01:00:00.000Z", Status: "completed"})
	mustRecord(t, first, ledger.Entry{OperationID: "op-after", At: "2026-11-08T02:00:00.000Z", Status: "completed"})
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	hugeCorrupt := `{"operationId":"bad","junk":"` + strings.Repeat("a", 5000) + `"}`
	combined := lines[0] + "\n" + hugeCorrupt + "\n" + lines[1] + "\n"
	if err := os.WriteFile(file, []byte(combined), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := ledger.New(ledger.Options{
		File:         file,
		MaxFileBytes: 4096,
		Now:          func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State() != "corrupt" || reloaded.Status().SkippedLines != 1 {
		t.Fatalf("unexpected status: %+v", reloaded.Status())
	}
	recs := reloaded.Records()
	if len(recs) != 2 || recs[0].OperationID != "op-before" || recs[1].OperationID != "op-after" {
		t.Fatalf("expected both valid records around oversized corrupt line, got %+v", recs)
	}
}

func TestLedgerQuarantinesUnreadableFileBeforeWriting(t *testing.T) {
	file := workspace(t)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	original := strings.Repeat("x", 5000)
	if err := os.WriteFile(file, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	now := int64(1794196855555)
	instance, err := ledger.New(ledger.Options{
		File:         file,
		MaxFileBytes: 1000,
		Now:          func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.State() != "unavailable" {
		t.Fatalf("expected unavailable state, got %q", instance.State())
	}
	backupPath := file + ".corrupt.1794196855555"
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt backup should not exist before write")
	}
	mustRecord(t, instance, ledger.Entry{OperationID: "op-new", At: "2026-11-08T03:00:00.000Z", Status: "completed"})
	backupRaw, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("expected quarantined backup %s: %v", backupPath, err)
	}
	if string(backupRaw) != original {
		t.Fatalf("quarantined backup content mismatch: len=%d want=%d", len(backupRaw), len(original))
	}
	if instance.State() != "ok" {
		t.Fatalf("expected ok state after quarantine and write, got %q", instance.State())
	}
	reopened, err := ledger.New(ledger.Options{
		File:         file,
		MaxFileBytes: 1000,
		Now:          func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if recs := reopened.Records(); len(recs) != 1 || recs[0].OperationID != "op-new" {
		t.Fatalf("unexpected records in fresh file: %+v", recs)
	}
}

func TestLedgerCompactTrimsOldestRecordsBelowMaxFileBytes(t *testing.T) {
	file := workspace(t)
	now := int64(1794196800000)
	const limit = int64(3600)
	instance, err := ledger.New(ledger.Options{
		File:         file,
		MaxRecords:   1000,
		MaxFileBytes: limit,
		CompactEvery: 1000,
		Now:          func() int64 { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	var prevStat os.FileInfo
	compactedAt := -1
	for i := 0; i < 20; i++ {
		mustRecord(t, instance, ledger.Entry{
			OperationID: fmt.Sprintf("op-%02d", i),
			At:          now - int64(30-i)*1000,
			Status:      "completed",
			ModelID:     str("model-compact-test"),
		})
		curStat, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if curStat.Size() >= limit {
			t.Fatalf("after op-%02d file size %d still >= maxFileBytes %d", i, curStat.Size(), limit)
		}
		if prevStat != nil && !os.SameFile(prevStat, curStat) {
			compactedAt = i
			prevStat = curStat
			break
		}
		prevStat = curStat
	}
	if compactedAt < 0 {
		t.Fatal("expected compaction to trigger once file reached maxFileBytes")
	}
	if prevStat.Size() > limit*3/4 {
		t.Fatalf("post-compact size %d should be <= 3/4 of limit %d", prevStat.Size(), limit)
	}
	recs := instance.Records()
	if len(recs) == 0 || len(recs) > compactedAt {
		t.Fatalf("expected oldest records trimmed on compaction, got %d records at step %d", len(recs), compactedAt)
	}
	wantNewest := fmt.Sprintf("op-%02d", compactedAt)
	if recs[len(recs)-1].OperationID != wantNewest {
		t.Fatalf("newest record = %q, want %q", recs[len(recs)-1].OperationID, wantNewest)
	}
	mustRecord(t, instance, ledger.Entry{
		OperationID: "op-next",
		At:          now - 1000,
		Status:      "completed",
		ModelID:     str("model-compact-test"),
	})
	statNext, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(prevStat, statNext) {
		t.Fatalf("append immediately after compaction unexpectedly rewrote the file (sizes: %d -> %d)", prevStat.Size(), statNext.Size())
	}
}

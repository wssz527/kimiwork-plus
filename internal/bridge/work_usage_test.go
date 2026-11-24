package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func TestWorkUsageCollectorIncrementalDirectOfficialAndGatewayDedup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	kimiHome := filepath.Join(t.TempDir(), "kimi-home")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(rev, func(d core.Document) error {
		d["integrationMode"] = "direct"
		d["connections"] = []any{
			core.Document{"id": "magpie", "name": "Magpie", "providerPresetId": "custom-magpie", "baseURL": "http://127.0.0.1:3425/v1"},
		}
		d["models"] = []any{
			core.Document{"id": "magpie/kimi-group%2Fdeepseek-v4-1-flash", "connectionId": "magpie", "upstreamModelId": "group/deepseek-v4-1-flash", "protocol": "chat", "contextWindow": float64(300000), "tools": true},
			core.Document{"id": "magpie/kimi-group%2Fglm-5-3-flash", "connectionId": "magpie", "upstreamModelId": "group/glm-5-3-flash", "protocol": "chat", "contextWindow": float64(300000), "tools": true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	directAlias := "byok-91f8064f-6f7a-4bb6-b795-29d716493c30/group/deepseek-v4-1-flash"
	rawIntent, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"sourceID":      "direct",
		"ownerID":       "owner-1",
		"kimiHome":      kimiHome,
		"aliases":       map[string]string{"magpie/kimi-group%2Fdeepseek-v4-1-flash": directAlias},
		"active":        true,
		"mode":          "direct",
	})
	if err := os.WriteFile(filepath.Join(dir, "native-work-intent.json"), rawIntent, 0o600); err != nil {
		t.Fatal(err)
	}

	nowMS := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC).UnixMilli()
	usageLedger, err := ledger.New(ledger.Options{
		File: filepath.Join(dir, "usage", "ledger.jsonl"),
		Now:  func() int64 { return nowMS },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pre-seed one gateway record that overlaps with one wire.jsonl line (must be deduplicated).
	gwIn, gwOut, gwTot := int64(1500), int64(200), int64(1700)
	gwLat := int64(420)
	usageLedger.Record(ledger.Entry{
		OperationID:  "gw-op-1",
		Source:       "gateway",
		At:           nowMS - 60000,
		Status:       "completed",
		ModelID:      optional("magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		ModelAlias:   optional("byok-kth-kimi/magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		ConnectionID: optional("magpie"),
		InputTokens:  &gwIn,
		OutputTokens: &gwOut,
		TotalTokens:  &gwTot,
		LatencyMS:    &gwLat,
	})

	wireFile := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "sessions", "wd_test", "conv-abc123", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wireFile), 0o700); err != nil {
		t.Fatal(err)
	}

	// Include: a huge non-usage line (>64KB), a direct-mode usage.record, an official model usage.record,
	// a duplicate matching the gateway record, a session summary (must be skipped), and a partial line without newline.
	hugeContent := `{"type":"context.append_message","payload":{"role":"user","content":"` + strings.Repeat("SECRET_PROMPT_", 5000) + `"}}` + "\n"
	directLine := `{"type":"usage.record","agentId":"main","model":"` + directAlias + `","usage":{"inputOther":25372,"output":153,"inputCacheRead":26368,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-30000) + `}` + "\n"
	officialLine := `{"type":"usage.record","agentId":"main","model":"k3-agent","usage":{"inputOther":1000,"output":250,"inputCacheRead":500,"inputCacheCreation":100},"usageScope":"turn","time":` + int64Str(nowMS-20000) + `}` + "\n"
	dupGatewayLine := `{"type":"usage.record","agentId":"main","model":"byok-kth-kimi/magpie/kimi-group%2Fdeepseek-v4-1-flash","usage":{"inputOther":1000,"output":200,"inputCacheRead":500,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-58000) + `}` + "\n"
	summaryLine := `{"type":"usage.record","agentId":"main","model":"` + directAlias + `","usage":{"inputOther":99999,"output":9999,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"session","time":` + int64Str(nowMS-15000) + `}` + "\n"
	partialTail := `{"type":"usage.record","agentId":"main","model":"byok-603d1804-6650-453a-9e1c-a475a1050d27/group/glm-5-3-flash","usage":{"inputOther":400,"output":50,"inputCacheRead":100,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-10000) + `}`

	if err := os.WriteFile(wireFile, []byte(hugeContent+directLine+officialLine+dupGatewayLine+summaryLine+partialTail), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := newWorkUsageCollector(dir, kimiHome, usageLedger, store, nil)
	collector.now = func() int64 { return nowMS }

	// First sync: should import directLine and officialLine (2 records), skip dupGatewayLine, summaryLine, hugeContent, partialTail.
	if got := collector.Sync(); got != 2 {
		t.Fatalf("first Sync() imported %d, want 2", got)
	}
	// Second sync without file change: 0 imported.
	if got := collector.Sync(); got != 0 {
		t.Fatalf("second Sync() imported %d, want 0", got)
	}

	// Finish writing the partial line (plus newline) -> incremental scan picks up historical UUID model mapped via upstreamModel!
	f, err := os.OpenFile(wireFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if got := collector.Sync(); got != 1 {
		t.Fatalf("third Sync() after completing partial line imported %d, want 1", got)
	}

	q, err := usageLedger.Query(ledger.Query{Preset: "today", TimeZone: "UTC", Now: &nowMS})
	if err != nil {
		t.Fatal(err)
	}
	// Total records = 1 gateway + 3 wire = 4.
	if q.Summary.Requests != 4 {
		t.Fatalf("summary.Requests = %d, want 4 (records=%#v)", q.Summary.Requests, q.Records)
	}

	byModel := map[string]map[string]any{}
	for _, rec := range q.Records {
		if rec["source"] == workUsageSource {
			byModel[rec["modelId"].(string)] = rec
		}
		rawBytes, _ := json.Marshal(rec)
		if strings.Contains(string(rawBytes), "SECRET_PROMPT") {
			t.Fatal("conversation text leaked into ledger record")
		}
	}
	ds := byModel["magpie/kimi-group%2Fdeepseek-v4-1-flash"]
	if ds == nil || ds["inputTokens"].(int64) != 51740 || ds["outputTokens"].(int64) != 153 || ds["cacheReadTokens"].(int64) != 26368 || ds["connectionId"] != "magpie" || ds["providerId"] != "custom-magpie" || ds["conversationId"] != "conv-abc123" || ds["latencyMs"] != nil {
		t.Fatalf("unexpected direct deepseek record: %#v", ds)
	}
	glm := byModel["magpie/kimi-group%2Fglm-5-3-flash"]
	if glm == nil || glm["inputTokens"].(int64) != 500 || glm["outputTokens"].(int64) != 50 || glm["connectionId"] != "magpie" {
		t.Fatalf("unexpected historical UUID glm record: %#v", glm)
	}
	k3 := byModel["k3-agent"]
	if k3 == nil || k3["inputTokens"].(int64) != 1600 || k3["outputTokens"].(int64) != 250 || k3["cacheWriteTokens"].(int64) != 100 || k3["providerId"] != "kimi-official" {
		t.Fatalf("unexpected official k3-agent record: %#v", k3)
	}

	// Clearing usage must not resurrect historical wire records even after fresh collector restart.
	usageLedger.Clear(nil, "UTC")
	collector.MarkCleared(nil, "UTC")
	restarted := newWorkUsageCollector(dir, kimiHome, usageLedger, store, nil)
	restarted.now = func() int64 { return nowMS }
	if got := restarted.Sync(); got != 0 {
		t.Fatalf("restarted Sync() after clear resurrected %d records", got)
	}
}

func TestWorkUsageRealGatewayDedupBudgetIsolationAndHomeCursors(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state")
	kimiHome1 := filepath.Join(base, "kimi-home-1")
	kimiHome2 := filepath.Join(base, "kimi-home-2")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(rev, func(d core.Document) error {
		d["integrationMode"] = "gateway"
		d["connections"] = []any{
			core.Document{"id": "magpie", "name": "Magpie", "baseURL": "http://127.0.0.1:3425/v1"},
		}
		d["models"] = []any{
			core.Document{
				"id":              "magpie/kimi-group%2Fdeepseek-v4-1-flash",
				"connectionId":    "magpie",
				"upstreamModelId": "group/deepseek-v4-1-flash",
				"protocol":        "chat",
				"contextWindow":   float64(300000),
				"tools":           true,
			},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	nowMS := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC).UnixMilli()
	usageLedger, err := ledger.New(ledger.Options{
		File: filepath.Join(dir, "usage", "ledger.jsonl"),
		Now:  func() int64 { return nowMS },
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Real protocol.Gateway ledger shape: ModelID = upstreamModelId, ModelAlias = logical ID,
	// At = start time, LatencyMS = 45000ms, wire.jsonl written at start + 46000ms.
	gwStart := nowMS - 60000
	gwLatency := int64(45000)
	gwIn, gwOut, gwTot := int64(2000), int64(400), int64(2400)
	usageLedger.Record(ledger.Entry{
		OperationID:  "gw-long-1",
		Source:       "gateway",
		At:           gwStart,
		Status:       "completed",
		ModelID:      optional("group/deepseek-v4-1-flash"),
		ModelAlias:   optional("magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		ConnectionID: optional("magpie"),
		LatencyMS:    &gwLatency,
		InputTokens:  &gwIn,
		OutputTokens: &gwOut,
		TotalTokens:  &gwTot,
	})

	// 2. In-flight gateway record ("started") for same model while wire.jsonl lands 1s before nowMS.
	usageLedger.Record(ledger.Entry{
		OperationID:  "gw-inflight-2",
		Source:       "gateway",
		At:           nowMS - 4000,
		Status:       "started",
		ModelID:      optional("group/deepseek-v4-1-flash"),
		ModelAlias:   optional("magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		ConnectionID: optional("magpie"),
	})

	wireFile1 := filepath.Join(kimiHome1, "runtime", "kimi-code", "home", "sessions", "wd_1", "conv-1", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wireFile1), 0o700); err != nil {
		t.Fatal(err)
	}
	wireDupLong := `{"type":"usage.record","agentId":"main","model":"byok-kth-kimi/magpie/kimi-group%2Fdeepseek-v4-1-flash","usage":{"inputOther":2000,"output":400,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(gwStart+46000) + `}`
	wireOfficial := `{"type":"usage.record","agentId":"main","model":"k3-agent","usage":{"inputOther":500,"output":100,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-20000) + `}`
	wireInflight := `{"type":"usage.record","agentId":"main","model":"byok-kth-kimi/magpie/kimi-group%2Fdeepseek-v4-1-flash","usage":{"inputOther":900,"output":120,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-1000) + `}`
	if err := os.WriteFile(wireFile1, []byte(strings.Join([]string{wireDupLong, wireOfficial, wireInflight}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	budget := newBudgetGate(usageLedger, func() time.Time { return time.UnixMilli(nowMS) })
	collector := newWorkUsageCollector(dir, kimiHome1, usageLedger, store, budget)
	collector.now = func() int64 { return nowMS }

	// First Sync: skips wireDupLong (matched real gateway format + 45s latency), imports wireOfficial (1),
	// and defers wireInflight without advancing past it!
	if got := collector.Sync(); got != 1 {
		t.Fatalf("first Sync() imported %d, want 1 (dup skipped, inflight deferred)", got)
	}

	// Gateway completes gw-inflight-2 -> next Sync deduplicates wireInflight cleanly!
	gw2Lat := int64(3200)
	gw2In, gw2Out, gw2Tot := int64(900), int64(120), int64(1020)
	usageLedger.Record(ledger.Entry{
		OperationID:  "gw-inflight-2",
		Source:       "gateway",
		At:           nowMS - 4000,
		Status:       "completed",
		ModelID:      optional("group/deepseek-v4-1-flash"),
		ModelAlias:   optional("magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		ConnectionID: optional("magpie"),
		LatencyMS:    &gw2Lat,
		InputTokens:  &gw2In,
		OutputTokens: &gw2Out,
		TotalTokens:  &gw2Tot,
	})
	if got := collector.Sync(); got != 0 {
		t.Fatalf("second Sync() after gateway completion imported %d, want 0", got)
	}

	// Invalid clear boundary must NOT advance ClearedBeforeMS.
	collector.MarkCleared("not-a-valid-date", "UTC")
	collector.MarkCleared(nowMS, "Invalid/Timezone")
	if collector.cursor.ClearedBeforeMS != 0 {
		t.Fatalf("invalid MarkCleared mutated ClearedBeforeMS to %d", collector.cursor.ClearedBeforeMS)
	}

	// Budget gate must ignore kimi_work_session records (both unpriced official k3-agent and priced direct records).
	directCost := 9.5
	priceSrc := "local"
	usageLedger.Record(ledger.Entry{
		OperationID:   "wire-priced-direct",
		Source:        workUsageSource,
		At:            nowMS - 10000,
		Status:        "completed",
		ModelID:       optional("magpie/kimi-group%2Fdeepseek-v4-1-flash"),
		EstimatedCost: &directCost,
		PriceSource:   &priceSrc,
	})
	day, month := budget.spend(time.UnixMilli(nowMS), time.UTC, "local")
	if day.Unpriced != 2 || month.Unpriced != 2 || day.Cost != 0 || month.Cost != 0 {
		t.Fatalf("expected only the 2 unpriced gateway records and 0 cost in budget spend, got day=%+v month=%+v", day, month)
	}

	// Switching to a second home with the identical relative path & offset must NOT collide opID or wipe home1 cursor.
	wireFile2 := filepath.Join(kimiHome2, "runtime", "kimi-code", "home", "sessions", "wd_1", "conv-1", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wireFile2), 0o700); err != nil {
		t.Fatal(err)
	}
	wireHome2 := `{"type":"usage.record","agentId":"main","model":"k3-agent","usage":{"inputOther":300,"output":80,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-15000) + `}`
	if err := os.WriteFile(wireFile2, []byte(wireHome2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector.SetHome(kimiHome2)
	if got := collector.Sync(); got != 1 {
		t.Fatalf("home2 Sync() imported %d, want 1", got)
	}
	if len(collector.cursor.Files) != 2 {
		t.Fatalf("expected cursors for both homes preserved, got %d (%v)", len(collector.cursor.Files), collector.cursor.Files)
	}
	collector.SetHome(kimiHome1)
	if got := collector.Sync(); got != 0 {
		t.Fatalf("returning to home1 re-imported %d records", got)
	}
}

func TestWorkModelResolverMapsCompatibleK3Aliases(t *testing.T) {
	const logicalID = "magpie/kimi-group%2Fdeepseek-v4-1-flash"
	const responsesID = "magpie/%2Fgroup%2Fgpt-5"
	compatibleID := protocol.KimiCompatibleModelID(logicalID)
	responsesCompatible := protocol.KimiCompatibleModelID(responsesID)
	d := core.Document{
		"connections": []any{
			core.Document{"id": "magpie", "name": "Magpie", "providerPresetId": "custom-magpie", "baseURL": "http://127.0.0.1:3425/v1"},
		},
		"models": []any{
			core.Document{"id": logicalID, "connectionId": "magpie", "upstreamModelId": "group/deepseek-v4-1-flash", "protocol": "chat", "contextWindow": float64(300000), "tools": true},
			core.Document{"id": responsesID, "connectionId": "magpie", "upstreamModelId": "group/gpt-5", "protocol": "responses", "contextWindow": float64(400000), "tools": true},
		},
	}
	resolver := buildWorkModelResolver(t.TempDir(), t.TempDir(), d)
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"byok-kth-kimi/" + compatibleID, logicalID},
		{"byok-603d1804-6650-453a-9e1c-a475a1050d27/" + compatibleID, logicalID},
		{compatibleID, logicalID},
		// A Responses row is offered under the same generated id, so the wire log
		// must resolve it as the saved model, not as an unpriced name.
		{"byok-kth-kimi/" + responsesCompatible, responsesID},
		{"byok-603d1804-6650-453a-9e1c-a475a1050d27/" + responsesCompatible, responsesID},
		{responsesCompatible, responsesID},
		{"byok-kth-resp/" + responsesID, responsesID},
		{"byok-kth-kimi/" + responsesID, responsesID},
	} {
		meta, alias := resolver.resolve(tc.raw)
		if meta.ModelID != tc.want || meta.ConnectionID != "magpie" || meta.ProviderID != "custom-magpie" || alias != tc.raw {
			t.Fatalf("resolve(%q) = %+v, %q", tc.raw, meta, alias)
		}
	}
}

func int64Str(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

package protocol_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kimiworkplus.local/migration/internal/ledger"
	"kimiworkplus.local/migration/internal/protocol"
)

// This file is the wiring the daemon does: one gateway whose sink writes a real
// ledger, exercised over loopback and then read back through a rebuilt ledger.
// It is the shape a caller can copy verbatim.

type sink struct {
	instance *ledger.Ledger
}

func (s sink) Record(entry protocol.UsageEntry) error {
	converted := ledger.Entry{OperationID: entry.OperationID, Source: entry.Source, At: entry.At,
		Status: entry.Status, ModelID: optional(entry.ModelID), ModelAlias: optional(entry.ModelAlias),
		ConnectionID: entry.ConnectionID, ProviderID: entry.ProviderID, Account: entry.Account,
		APIKeyID: entry.APIKeyID, ConversationID: entry.ConversationID,
		Protocol: optional(entry.Protocol), UpstreamProtocol: optional(entry.UpstreamProtocol),
		LatencyMS: entry.LatencyMS, ErrorCode: optional(entry.ErrorCode), PriceSource: entry.PriceSource,
		EstimatedCost: entry.EstimatedCost}
	if entry.HTTPStatus != nil {
		status := int64(*entry.HTTPStatus)
		converted.HTTPStatus = &status
	}
	if entry.Usage != nil {
		// The ledger's scanner shape is keyed by the same names this package
		// already serialises, so the JSON tags are the single source of truth.
		// A hand-written map here silently dropped the cache counts once
		// (`cacheReadTokens` instead of `cachedInputTokens`), which is exactly
		// the kind of mismatch a copied example must not contain.
		encoded, err := json.Marshal(entry.Usage)
		if err != nil {
			return err
		}
		converted.Usage = map[string]any{}
		if err := json.Unmarshal(encoded, &converted.Usage); err != nil {
			return err
		}
	}
	s.instance.Record(converted)
	return nil
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func TestEndToEndRequestLandsInTheLedgerAndSurvivesARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		// A cached conversation: the cached prompt is a subset of the input, and
		// the vendor reports it in `prompt_tokens_details`.
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,
			         "prompt_tokens_details":{"cached_tokens":4}}}`))
	})
	connectionID := "work"
	model := chatModel("kimi-probe")
	model.ConnectionID = &connectionID
	provider := "openai"
	account := "synthetic-account"
	conversation := "conv-9"
	// A model that names a connection is only served when the live process
	// really has that connection, so the fixture registers one.
	backend, err := protocol.NewBackend([]protocol.Endpoint{{ID: connectionID, BaseURL: fake.server.URL,
		APIKey: "vendor-key"}}, []protocol.Model{model})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	gateway, err := protocol.NewGateway(protocol.Config{Backend: backend, Key: gatewayKey, RecordRequests: true,
		UsageSink: sink{instance: instance},
		ResolveUsageContext: func(request protocol.UsageContextRequest) (protocol.UsageContext, error) {
			if _, leaked := request.Headers["authorization"]; leaked {
				t.Errorf("the resolver was handed the gateway credential")
			}
			if request.PromptCacheKey != conversation {
				t.Errorf("prompt cache key = %q", request.PromptCacheKey)
			}
			return protocol.UsageContext{ProviderID: &provider, Account: &account,
				ConversationID: &conversation}, nil
		}})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	server, url, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "prompt_cache_key": conversation, "messages": []any{map[string]any{"role": "user",
			"content": "synthetic prompt"}}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	_ = gateway

	records := instance.Records()
	if len(records) != 1 {
		t.Fatalf("records %d", len(records))
	}
	record := records[0]
	if record.Status != "completed" || record.ModelID == nil || *record.ModelID != "kimi-probe" {
		t.Fatalf("record %+v", record)
	}
	if record.ProviderID == nil || *record.ProviderID != provider {
		t.Fatalf("provider %+v", record.ProviderID)
	}
	if record.ConversationID == nil || *record.ConversationID != conversation {
		t.Fatalf("conversation %+v", record.ConversationID)
	}
	if record.InputTokens == nil || *record.InputTokens != 12 || *record.OutputTokens != 3 ||
		record.TotalTokens == nil || *record.TotalTokens != 15 {
		t.Fatalf("tokens %+v", record)
	}
	// The cache read is a subset of the input and must survive the sink: it is
	// the field a hand-written key map silently dropped.
	if record.CacheReadTokens == nil || *record.CacheReadTokens != 4 {
		t.Fatalf("cache read %+v", record.CacheReadTokens)
	}
	if record.CacheWriteTokens != nil {
		t.Fatalf("an OpenAI reply has no cache write, got %+v", record.CacheWriteTokens)
	}
	// The account is a digest, not the value the resolver returned.
	if record.Account == nil || *record.Account != ledger.RedactIdentifier(account) {
		t.Fatalf("account %+v", record.Account)
	}
	if record.ConnectionID == nil || *record.ConnectionID != connectionID {
		t.Fatalf("connection %+v", record.ConnectionID)
	}

	// Nothing about the conversation reached the file.
	stored, err := io.ReadAll(mustOpen(t, file))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, secret := range []string{"synthetic prompt", account, gatewayKey} {
		if strings.Contains(string(stored), secret) {
			t.Fatalf("the ledger holds %q", secret)
		}
	}

	// A rebuilt ledger sees the same history, which is what the panel reads
	// after a restart.
	rebuilt, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	result, err := rebuilt.Query(ledger.Query{Preset: "30d", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Summary.Requests != 1 || *result.Summary.InputTokens != 12 || result.Summary.Errors != 0 {
		t.Fatalf("summary %+v", result.Summary)
	}
	if *result.Summary.OutputTokens != 3 || *result.Summary.TotalTokens != 15 {
		t.Fatalf("summary output/total %+v", result.Summary)
	}
	if result.Summary.CacheReadTokens == nil || *result.Summary.CacheReadTokens != 4 {
		t.Fatalf("summary cache read %+v", result.Summary.CacheReadTokens)
	}
	if result.Summary.CacheWriteTokens != nil {
		t.Fatalf("summary invented a cache write: %+v", result.Summary.CacheWriteTokens)
	}
	if result.Records[0]["cacheReadTokens"] != int64(4) {
		t.Fatalf("row cache read %v", result.Records[0]["cacheReadTokens"])
	}
	populated := 0
	for _, bucket := range result.Buckets {
		if bucket.Requests == 0 {
			continue
		}
		populated++
		if bucket.CacheReadTokens == nil || *bucket.CacheReadTokens != 4 {
			t.Fatalf("bucket cache read %+v", bucket.CacheReadTokens)
		}
	}
	if populated != 1 {
		t.Fatalf("populated buckets %d", populated)
	}
	if len(result.Filters.Providers) != 1 || result.Filters.Providers[0].Value != provider {
		t.Fatalf("providers %+v", result.Filters.Providers)
	}
	if result.Coverage.Pricing != "not_provided" || result.Coverage.Scope != "gateway_observed" {
		t.Fatalf("coverage %+v", result.Coverage)
	}
}

func TestEndToEndFailedRequestIsCountedWithoutInventingTokens(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(config *protocol.Config) {
		config.UsageSink = sink{instance: instance}
	})
	reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 429 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	records := instance.Records()
	if len(records) != 1 {
		t.Fatalf("records %d", len(records))
	}
	record := records[0]
	if record.Status != "failed" || record.HTTPStatus == nil || *record.HTTPStatus != 429 {
		t.Fatalf("record %+v", record)
	}
	if record.InputTokens != nil || record.TotalTokens != nil {
		t.Fatalf("a failed request invented tokens: %+v", record)
	}
	if record.DataSource != "request_only" {
		t.Fatalf("data source %q", record.DataSource)
	}
	result, err := instance.Query(ledger.Query{Preset: "today", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Summary.Requests != 1 || result.Summary.RequestsWithUsage != 0 || result.Summary.Errors != 1 {
		t.Fatalf("summary %+v", result.Summary)
	}
	if result.Summary.InputTokens != nil || result.Summary.OutputTokens != nil {
		t.Fatalf("an aggregate invented tokens: %+v", result.Summary)
	}
}

func TestASinkThatFailsNeverCostsTheReply(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"still served"}}]}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(config *protocol.Config) {
		config.UsageSink = protocol.UsageSinkFunc(func(protocol.UsageEntry) error {
			panic("storage unavailable")
		})
	})
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	if !strings.Contains(body, "still served") {
		t.Fatalf("body %s", body)
	}
}

func TestAThrowingResolverLeavesTheIdentityUnknown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(config *protocol.Config) {
		config.UsageSink = sink{instance: instance}
		config.ResolveUsageContext = func(protocol.UsageContextRequest) (protocol.UsageContext, error) {
			panic("resolver unavailable")
		}
	})
	reply, _ := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	records := instance.Records()
	if len(records) != 1 {
		t.Fatalf("records %d", len(records))
	}
	record := records[0]
	// No provider, account or conversation was invented to fill the gap.
	if record.ProviderID != nil || record.Account != nil || record.ConversationID != nil {
		t.Fatalf("an identity was invented: %+v", record)
	}
	if record.InputTokens == nil || *record.InputTokens != 2 {
		t.Fatalf("the measured tokens were lost: %+v", record)
	}
}

func TestARebuiltGatewayServesTheNewCatalogImmediately(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	_, first := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-old")}, nil)
	if reply, _ := post(t, first, "/chat/completions",
		map[string]any{"model": "kimi-old", "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("old catalog: status %d", reply.StatusCode)
	}

	// A configuration change is a new gateway, not a mutation of a live one: a
	// request in flight never observes a half-applied catalog.
	_, second := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-new")}, nil)
	reply, body := post(t, second, "/chat/completions",
		map[string]any{"model": "kimi-old", "messages": []any{}}, nil)
	if reply.StatusCode != 409 || errorCode(t, body) != "model_not_enabled_for_kimi" {
		t.Fatalf("the replaced catalog still served the old model: %d %s", reply.StatusCode, errorCode(t, body))
	}
	if reply, _ := post(t, second, "/chat/completions",
		map[string]any{"model": "kimi-new", "messages": []any{}}, nil); reply.StatusCode != 200 {
		t.Fatalf("new catalog: status %d", reply.StatusCode)
	}
}

func mustOpen(t *testing.T, path string) io.Reader {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// TestSinkCarriesEveryTokenClassThroughToTheQuery pins the exact numbers a real
// bridge run produced: three requests, 250 total, 200 input, 50 output and 30
// cache read. It exists because a hand-written key map in the sink dropped the
// cache count while every other assertion still passed.
func TestSinkCarriesEveryTokenClassThroughToTheQuery(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage", "ledger.jsonl")
	instance, err := ledger.New(ledger.Options{File: file})
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	// Three replies whose usage adds up to the observed totals. The first two
	// report a cached prompt, the third does not, which is what makes the cache
	// column meaningful rather than constant.
	replies := []string{
		`{"choices":[{"index":0,"message":{"content":"a"}}],
		  "usage":{"prompt_tokens":100,"completion_tokens":25,"total_tokens":125,
		           "prompt_tokens_details":{"cached_tokens":20}}}`,
		`{"choices":[{"index":0,"message":{"content":"b"}}],
		  "usage":{"prompt_tokens":60,"completion_tokens":15,"total_tokens":75,
		           "prompt_tokens_details":{"cached_tokens":10}}}`,
		`{"choices":[{"index":0,"message":{"content":"c"}}],
		  "usage":{"prompt_tokens":40,"completion_tokens":10,"total_tokens":50}}`,
	}
	index := 0
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replies[index%len(replies)]))
		index++
	})
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")},
		func(config *protocol.Config) { config.UsageSink = sink{instance: instance} })
	for i := 0; i < len(replies); i++ {
		reply, body := post(t, url, "/chat/completions",
			map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
		if reply.StatusCode != 200 {
			t.Fatalf("request %d: status %d body %s", i, reply.StatusCode, body)
		}
	}
	if records := instance.Records(); len(records) != 3 {
		t.Fatalf("records %d", len(records))
	}

	result, err := instance.Query(ledger.Query{Preset: "90d", TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	summary := result.Summary
	if summary.Requests != 3 || summary.RequestsWithUsage != 3 {
		t.Fatalf("counts %+v", summary)
	}
	for _, check := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"inputTokens", summary.InputTokens, 200},
		{"outputTokens", summary.OutputTokens, 50},
		{"totalTokens", summary.TotalTokens, 250},
		{"cacheReadTokens", summary.CacheReadTokens, 30},
	} {
		if check.got == nil {
			t.Fatalf("%s is unknown, want %d", check.name, check.want)
		}
		if *check.got != check.want {
			t.Fatalf("%s = %d want %d", check.name, *check.got, check.want)
		}
	}
	// The cache write was never reported, so it stays unknown rather than 0.
	if summary.CacheWriteTokens != nil {
		t.Fatalf("cache write invented: %+v", summary.CacheWriteTokens)
	}
	// A per-model filter over the same history reports the same totals.
	filtered, err := instance.Query(ledger.Query{Preset: "90d", TimeZone: "Asia/Shanghai",
		Filters: map[string]string{"modelId": "kimi-probe"}})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if filtered.Summary.Requests != 3 || *filtered.Summary.CacheReadTokens != 30 {
		t.Fatalf("filtered summary %+v", filtered.Summary)
	}
}

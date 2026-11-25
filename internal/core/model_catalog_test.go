package core

import (
	"slices"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func TestProviderPresetsAndCatalogHints(t *testing.T) {
	ids := []string{"kimi-code-cn", "kimi-code-global", "deepseek", "zhipu-coding-cn-chat", "zhipu-coding-cn-responses", "zhipu-api-cn", "zai-coding-global-chat", "zai-coding-global-responses", "zai-api-global", "qwen-coding", "qwen-api", "openai", "openrouter", "siliconflow", "volcengine", "moonshot-cn", "gemini-openai", "xai", "anthropic"}
	providers := Providers()
	got := []string{}
	for _, value := range Rows(providers["presets"]) {
		row := Object(value)
		got = append(got, String(row["id"]))
		if _, err := ValidateBaseURL(String(row["baseURL"])); err != nil || len(Rows(row["models"])) == 0 {
			t.Fatalf("invalid preset %s: %v", row["id"], err)
		}
	}
	if !slices.Equal(ids, got) {
		t.Fatalf("provider ids = %v", got)
	}
	if len(Rows(providers["modelCatalogHints"])) != len(modelCatalogHints) {
		t.Fatal("providers did not expose catalog hints")
	}
	candidate := Object(Rows(SearchModels("anthropic", "claude-sonnet-4-6")["models"])[0])
	if candidate["upstreamProtocol"] != "anthropic" || !slices.Contains(Rows(candidate["protocols"]), any("chat")) {
		t.Fatalf("missing Anthropic protocol in search: %v", candidate)
	}
}

func TestMergeModelsUsesKnownAndUnknownDefaults(t *testing.T) {
	d := Document{"connections": []any{}, "models": []any{}}
	if err := MergeModels(d, []any{
		Document{"id": "alias", "upstreamModelId": "openai/gpt-5-mini-2025-08-07", "protocol": "responses", "contextWindow": float64(0)},
		Document{"id": "model-a", "protocol": "responses"},
		Document{"id": "gpt-5", "protocol": "responses", "contextWindow": 16000, "tools": false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	models, err := protocol.ValidateModelsAny(Rows(d["models"]))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		context int64
		tools   bool
	}{{400000, true}, {131072, true}, {16000, false}} {
		if models[i].ContextWindow != want.context || models[i].Tools == nil || *models[i].Tools != want.tools {
			t.Fatalf("model = %+v; want context %d and tools %v", models[i], want.context, want.tools)
		}
	}
	if _, err := protocol.ValidateModelsAny([]any{Document{"id": "model-a", "protocol": "responses", "contextWindow": 0}}); err == nil {
		t.Fatal("underlying protocol parser accepted a zero context window")
	}
}

func TestBudgetCatalogPricingRespectsCurrencyAndManualPrices(t *testing.T) {
	d := Document{"models": []any{
		Document{"id": "aliased", "upstreamModelId": "openai/gpt-4.1-mini"},
		Document{"id": "model-a"}, Document{"id": "gpt-test"}, Document{"id": "gpt-5-test"},
		Document{"id": "gpt-4o", "pricing": map[string]any{"inputPerMillion": 9, "outputPerMillion": 10}},
		Document{"id": "qwen-plus"},
	}}
	if err := MergeBudget(d, map[string]any{"currency": "USD", "enabled": true, "serialize": true}); err != nil {
		t.Fatal(err)
	}
	budget := Object(d["budget"])
	if budget["timeZone"] != time.Local.String() || budget["serialize"] != true {
		t.Fatal(budget)
	}
	prices, err := PriceTable(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"model-a", "gpt-test", "gpt-5-test", "qwen-plus"} {
		if prices.Has(id) {
			t.Fatalf("price was assigned to %s", id)
		}
	}
	if !prices.Has("aliased") || !prices.Has("gpt-4o") {
		t.Fatal("price table missing an upstream alias or inline price")
	}
	collected, err := modelPrices(Rows(d["models"]), "USD")
	if err != nil || *collected["aliased"].InputPerMillion != 0.40 || *collected["gpt-4o"].InputPerMillion != 9 {
		t.Fatalf("prices = %v (%v)", collected, err)
	}
	if err := MergeBudget(d, map[string]any{"currency": "CNY", "enabled": true}); err != nil {
		t.Fatal(err)
	}
	prices, err = PriceTable(d)
	if err != nil || !prices.Has("qwen-plus") || prices.Has("aliased") || !prices.Has("gpt-4o") {
		t.Fatalf("CNY price table = %v (%v)", prices, err)
	}
}

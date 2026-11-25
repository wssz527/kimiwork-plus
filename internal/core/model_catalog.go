package core

import "strings"

type ReferencePricing struct {
	Currency  string   `json:"currency"`
	Input     float64  `json:"input"`
	Output    float64  `json:"output"`
	CacheRead *float64 `json:"cacheRead,omitempty"`
}

type ModelCatalogHint struct {
	ID               string            `json:"id"`
	Patterns         []string          `json:"patterns"`
	Name             string            `json:"name"`
	ContextWindow    int               `json:"contextWindow"`
	Tools            bool              `json:"tools"`
	Reasoning        bool              `json:"reasoning"`
	InputModalities  []string          `json:"inputModalities"`
	ReasoningEfforts []string          `json:"reasoningEfforts"`
	ReferencePricing *ReferencePricing `json:"referencePricing,omitempty"`
}

func referencePrice(currency string, input, output float64, cacheRead ...float64) *ReferencePricing {
	price := &ReferencePricing{Currency: currency, Input: input, Output: output}
	if len(cacheRead) != 0 {
		price.CacheRead = &cacheRead[0]
	}
	return price
}

func catalogHint(id, name string, context int, reasoning, image bool, efforts []string, price *ReferencePricing, aliases ...string) ModelCatalogHint {
	patterns := append([]string{id}, aliases...)
	modalities := []string{"text"}
	if image {
		modalities = append(modalities, "image")
	}
	if efforts == nil {
		efforts = []string{}
	}
	return ModelCatalogHint{id, patterns, name, context, true, reasoning, modalities, efforts, price}
}

var (
	standardEfforts = []string{"low", "medium", "high"}
	extendedEfforts = []string{"low", "medium", "high", "xhigh"}
	glmEfforts      = []string{"low", "high", "max"}
)

var modelCatalogHints = []ModelCatalogHint{
	catalogHint("deepseek-chat", "DeepSeek V3", 131072, false, false, nil, referencePrice("USD", 0.28, 0.42, 0.028), "deepseek-v3", "deepseek-v3-0324", "deepseek-v3.1", "deepseek-v3.2"),
	catalogHint("deepseek-reasoner", "DeepSeek R1", 131072, true, false, nil, referencePrice("USD", 0.55, 2.19, 0.14), "deepseek-r1", "deepseek-r1-0528"),
	catalogHint("deepseek-v4-pro", "DeepSeek V4 Pro", 1000000, true, false, nil, referencePrice("USD", 1.74, 3.48, 0.145)),
	catalogHint("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, true, false, nil, referencePrice("USD", 0.14, 0.28, 0.0028), "deepseek-flash"),
	catalogHint("glm-4.5-air", "GLM-4.5 Air", 131072, true, false, nil, referencePrice("USD", 0.20, 1.10)),
	catalogHint("glm-4.5", "GLM-4.5", 131072, true, false, nil, referencePrice("USD", 0.60, 2.20)),
	catalogHint("glm-4.6", "GLM-4.6", 204800, true, false, nil, referencePrice("USD", 0.60, 2.20, 0.11)),
	catalogHint("glm-4.7", "GLM-4.7", 204800, true, false, nil, referencePrice("USD", 0.60, 2.20, 0.11)),
	catalogHint("glm-5", "GLM-5", 204800, true, false, nil, referencePrice("USD", 1, 3.20, 0.20)),
	catalogHint("glm-5.3-flash", "GLM-5.3 Flash", 1048576, true, false, glmEfforts, nil),
	catalogHint("glm-5.3", "GLM-5.3", 1048576, true, false, glmEfforts, nil),
	catalogHint("qwen-max", "Qwen Max", 32768, false, false, nil, referencePrice("CNY", 2.40, 9.60), "qwen-max-latest", "qwen-max-20*"),
	catalogHint("qwen-plus", "Qwen Plus", 131072, true, false, nil, referencePrice("CNY", 0.80, 2), "qwen-plus-latest", "qwen-plus-20*"),
	catalogHint("qwen3-coder-plus", "Qwen3 Coder Plus", 1048576, false, false, nil, referencePrice("CNY", 4, 16), "qwen3-coder-plus-20*", "qwen3-coder-480b-a35b-instruct"),
	catalogHint("qwen-coder-plus", "Qwen Coder Plus", 131072, false, false, nil, referencePrice("CNY", 3.50, 7), "qwen2.5-coder-32b-instruct"),
	catalogHint("qwen3-max", "Qwen3 Max", 262144, true, false, nil, referencePrice("CNY", 6, 24), "qwen3-max-20*"),
	catalogHint("qwen3-235b-a22b", "Qwen3 235B", 131072, true, false, nil, nil, "qwen3-235b-a22b-*"),
	catalogHint("qwen3.8-max", "Qwen3.8 Max", 1000000, true, false, []string{"none", "low", "medium", "xhigh"}, nil),
	catalogHint("qwen3.7-plus", "Qwen3.7 Plus", 1000000, true, false, nil, nil),
	catalogHint("gpt-4o-mini", "GPT-4o mini", 128000, false, true, nil, referencePrice("USD", 0.15, 0.60, 0.075), "gpt-4o-mini-20*"),
	catalogHint("gpt-4o", "GPT-4o", 128000, false, true, nil, referencePrice("USD", 2.50, 10, 1.25), "gpt-4o-20*"),
	catalogHint("gpt-4.1-nano", "GPT-4.1 nano", 1047576, false, true, nil, referencePrice("USD", 0.10, 0.40, 0.025), "gpt-4.1-nano-20*"),
	catalogHint("gpt-4.1-mini", "GPT-4.1 mini", 1047576, false, true, nil, referencePrice("USD", 0.40, 1.60, 0.10), "gpt-4.1-mini-20*"),
	catalogHint("gpt-4.1", "GPT-4.1", 1047576, false, true, nil, referencePrice("USD", 2, 8, 0.50), "gpt-4.1-20*"),
	catalogHint("gpt-5-mini", "GPT-5 mini", 400000, true, true, standardEfforts, referencePrice("USD", 0.25, 2, 0.025), "gpt-5-mini-20*"),
	catalogHint("gpt-5-nano", "GPT-5 nano", 400000, true, true, standardEfforts, referencePrice("USD", 0.05, 0.40, 0.005), "gpt-5-nano-20*"),
	catalogHint("gpt-5", "GPT-5", 400000, true, true, standardEfforts, referencePrice("USD", 1.25, 10, 0.125), "gpt-5-20*"),
	catalogHint("gpt-5.1-codex", "GPT-5.1 Codex", 400000, true, true, extendedEfforts, referencePrice("USD", 1.25, 10, 0.125)),
	catalogHint("gpt-5.2", "GPT-5.2", 400000, true, true, extendedEfforts, referencePrice("USD", 1.75, 14, 0.175), "gpt-5.2-20*", "gpt-5.2-codex"),
	catalogHint("o3-mini", "OpenAI o3-mini", 200000, true, false, standardEfforts, referencePrice("USD", 1.10, 4.40, 0.55), "o3-mini-20*"),
	catalogHint("o3", "OpenAI o3", 200000, true, true, standardEfforts, referencePrice("USD", 2, 8, 0.50), "o3-20*"),
	catalogHint("o4-mini", "OpenAI o4-mini", 200000, true, true, standardEfforts, referencePrice("USD", 1.10, 4.40, 0.275), "o4-mini-20*"),
	catalogHint("claude-3-5-sonnet", "Claude 3.5 Sonnet", 200000, false, true, nil, referencePrice("USD", 3, 15, 0.30), "claude-3-5-sonnet-*", "claude-3.5-sonnet*"),
	catalogHint("claude-3-5-haiku", "Claude 3.5 Haiku", 200000, false, true, nil, referencePrice("USD", 0.80, 4, 0.08), "claude-3-5-haiku-*", "claude-3.5-haiku*"),
	catalogHint("claude-3-7-sonnet", "Claude 3.7 Sonnet", 200000, true, true, nil, referencePrice("USD", 3, 15, 0.30), "claude-3-7-sonnet-*", "claude-3.7-sonnet*"),
	catalogHint("claude-sonnet-4-5", "Claude Sonnet 4.5", 200000, true, true, nil, referencePrice("USD", 3, 15, 0.30), "claude-sonnet-4-5-*", "claude-sonnet-4.5*"),
	catalogHint("claude-sonnet-4-6", "Claude Sonnet 4.6", 1000000, true, true, standardEfforts, referencePrice("USD", 3, 15, 0.30), "claude-sonnet-4.6*"),
	catalogHint("claude-sonnet-4", "Claude Sonnet 4", 200000, true, true, nil, referencePrice("USD", 3, 15, 0.30), "claude-sonnet-4-20*"),
	catalogHint("claude-opus-4-6", "Claude Opus 4.6", 1000000, true, true, extendedEfforts, referencePrice("USD", 5, 25, 0.50), "claude-opus-4.6*"),
	catalogHint("claude-opus-4-5", "Claude Opus 4.5", 200000, true, true, standardEfforts, referencePrice("USD", 5, 25, 0.50), "claude-opus-4-5-*", "claude-opus-4.5*"),
	catalogHint("claude-opus-4", "Claude Opus 4", 200000, true, true, nil, referencePrice("USD", 15, 75, 1.50), "claude-opus-4-20*", "claude-opus-4-1*", "claude-opus-4.1*"),
	catalogHint("gemini-2.5-pro", "Gemini 2.5 Pro", 1048576, true, true, nil, referencePrice("USD", 1.25, 10, 0.125), "gemini-2.5-pro-preview-*"),
	catalogHint("gemini-2.5-flash-lite", "Gemini 2.5 Flash-Lite", 1048576, true, true, nil, referencePrice("USD", 0.10, 0.40, 0.01), "gemini-2.5-flash-lite-*"),
	catalogHint("gemini-2.5-flash", "Gemini 2.5 Flash", 1048576, true, true, nil, referencePrice("USD", 0.30, 2.50, 0.03), "gemini-2.5-flash-preview-*"),
	catalogHint("gemini-3-pro-preview", "Gemini 3 Pro", 1048576, true, true, []string{"low", "high"}, referencePrice("USD", 2, 12, 0.20), "gemini-3-pro"),
	catalogHint("gemini-3-flash-preview", "Gemini 3 Flash", 1048576, true, true, []string{"minimal", "low", "medium", "high"}, referencePrice("USD", 0.50, 3, 0.05), "gemini-3-flash"),
	catalogHint("gemini-3.1-pro-preview", "Gemini 3.1 Pro", 1048576, true, true, standardEfforts, referencePrice("USD", 2, 12, 0.20), "gemini-3.1-pro"),
	catalogHint("grok-3-mini", "Grok 3 Mini", 131072, true, false, []string{"low", "high"}, referencePrice("USD", 0.30, 0.50, 0.075), "grok-3-mini-latest"),
	catalogHint("grok-3", "Grok 3", 131072, false, false, nil, referencePrice("USD", 3, 15, 0.75), "grok-3-latest"),
	catalogHint("grok-4", "Grok 4", 256000, true, true, nil, referencePrice("USD", 3, 15, 0.75), "grok-4-0709", "grok-4-latest"),
	catalogHint("grok-4-fast-reasoning", "Grok 4 Fast", 2000000, true, true, nil, referencePrice("USD", 0.20, 0.50, 0.05), "grok-4.1-fast-reasoning", "grok-4-1-fast-reasoning"),
	catalogHint("grok-4-fast-non-reasoning", "Grok 4 Fast Non-Reasoning", 2000000, false, true, nil, referencePrice("USD", 0.20, 0.50, 0.05), "grok-4.1-fast-non-reasoning", "grok-4-1-fast-non-reasoning"),
	catalogHint("kimi-k2.5", "Kimi K2.5", 262144, true, true, nil, referencePrice("CNY", 4, 21, 0.70)),
	catalogHint("kimi-k2-thinking", "Kimi K2 Thinking", 262144, true, false, nil, referencePrice("CNY", 4, 16, 1)),
	catalogHint("kimi-k2-0905-preview", "Kimi K2", 262144, false, false, nil, referencePrice("CNY", 4, 16, 1), "kimi-k2"),
	catalogHint("moonshot-v1-128k", "Moonshot V1 128K", 131072, false, false, nil, referencePrice("CNY", 60, 60)),
	catalogHint("moonshot-v1-32k", "Moonshot V1 32K", 32768, false, false, nil, referencePrice("CNY", 24, 24)),
	catalogHint("moonshot-v1-8k", "Moonshot V1 8K", 8192, false, false, nil, referencePrice("CNY", 12, 12)),
	catalogHint("k3", "Kimi K3", 1048576, true, true, glmEfforts, nil, "kimi-k3"),
	catalogHint("k3-256k", "Kimi K3 256K", 262144, true, true, glmEfforts, nil, "kimi-k3-256k"),
}

func modelCatalogHint(row map[string]any) *ModelCatalogHint {
	for _, value := range []any{row["upstreamModelId"], row["id"]} {
		id := strings.ToLower(strings.TrimSpace(String(value)))
		if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
			id = id[slash+1:]
		}
		for i := range modelCatalogHints {
			for _, pattern := range modelCatalogHints[i].Patterns {
				prefix, wildcard := strings.CutSuffix(pattern, "*")
				if id == pattern || (wildcard && strings.HasPrefix(id, prefix)) {
					return &modelCatalogHints[i]
				}
			}
		}
	}
	return nil
}

func applyModelDefaults(row map[string]any, toolsCleared bool) {
	window, hasWindow := row["contextWindow"]
	_, hasTools := row["tools"]
	hasTools = hasTools || toolsCleared
	needWindow := !hasWindow || window == 0 || window == int64(0) || window == float64(0)
	if !needWindow && hasTools {
		return
	}
	hint := modelCatalogHint(row)
	if needWindow {
		row["contextWindow"] = 131072
		if hint != nil {
			row["contextWindow"] = hint.ContextWindow
		}
	}
	if !hasTools {
		row["tools"] = hint == nil || hint.Tools
	}
}

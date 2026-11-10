package protocol

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The published Anthropic facts this file encodes, rather than guesses:
//
//   - `output_config.effort` accepts low|medium|high|xhigh|max only. There is
//     no `off` and no `none`: disabling thinking is `thinking.type:disabled`.
//   - `thinking` and `effort` are separate switches, and `budget_tokens` must be
//     at least 1024 and strictly below `max_tokens`.
//   - Images are `image` blocks with a `base64` or `url` source.
const (
	AnthropicVersion   = "2023-06-01"
	AnthropicMaxTokens = 8192
	minBudgetTokens    = 1024
)

// AnthropicEfforts is the whole documented set, ascending. `off` is absent on
// purpose: it is not a level, it is the absence of thinking.
var AnthropicEfforts = []string{"low", "medium", "high", "xhigh", "max"}

var offEfforts = map[string]bool{"off": true, "none": true, "disabled": true, "false": true, "no": true, "0": true}

// ReasoningControls is the reasoning ask one request makes, in Anthropic terms.
type ReasoningControls struct {
	Effort   string
	Thinking map[string]any
}

// ReasoningControlsFor maps a caller's level onto what the Messages API accepts.
// An unrecognised level is omitted rather than guessed into a real one.
func ReasoningControlsFor(effort string, budgetTokens, maxTokens *int64, summary bool) (ReasoningControls, error) {
	name := strings.ToLower(strings.TrimSpace(effort))
	if name != "" && offEfforts[name] {
		return ReasoningControls{Thinking: map[string]any{"type": "disabled"}}, nil
	}
	level := ""
	for _, candidate := range AnthropicEfforts {
		if candidate == name {
			level = name
			break
		}
	}
	limit := int64(AnthropicMaxTokens)
	if maxTokens != nil {
		limit = *maxTokens
	}
	controls := ReasoningControls{Effort: level}
	if budgetTokens != nil {
		if *budgetTokens < minBudgetTokens || limit <= minBudgetTokens {
			return ReasoningControls{}, ErrInvalidThinkingBudget
		}
		budget := *budgetTokens
		if budget > limit-1 {
			budget = limit - 1
		}
		thinking := map[string]any{"type": "enabled", "budget_tokens": budget}
		if summary {
			thinking["display"] = "summarized"
		}
		controls.Thinking = thinking
	} else if level != "" && summary {
		if limit <= minBudgetTokens {
			return ReasoningControls{}, ErrInvalidThinkingBudget
		}
		share := map[string]float64{"low": 0.2, "medium": 0.35, "high": 0.5, "xhigh": 0.7, "max": 0.85}[level]
		ceiling := limit - 1
		if ceiling < minBudgetTokens {
			ceiling = minBudgetTokens
		}
		budget := int64(math.Floor(float64(limit) * share))
		if budget < minBudgetTokens {
			budget = minBudgetTokens
		}
		if budget > ceiling {
			budget = ceiling
		}
		controls.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget, "display": "summarized"}
	}
	return controls, nil
}

var dataURLPattern = regexp.MustCompile(`(?i)^data:(image/[a-z0-9.+-]+);base64,([A-Za-z0-9+/=]+)$`)
var httpURLPattern = regexp.MustCompile(`(?i)^https?://`)

// imageBlock converts one image reference. Nothing is fetched and nothing is
// re-encoded: a data URL becomes a base64 source, an http(s) URL stays a URL
// source so the upstream fetches what the user pointed at.
func imageBlock(url string) map[string]any {
	if url == "" {
		return nil
	}
	if match := dataURLPattern.FindStringSubmatch(url); match != nil {
		return map[string]any{"type": "image",
			"source": map[string]any{"type": "base64", "media_type": strings.ToLower(match[1]), "data": match[2]}}
	}
	if httpURLPattern.MatchString(url) {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
	}
	return nil
}

// chatParts normalises Chat and Responses content parts into Messages blocks.
// `input_text`/`output_text` are the Responses spelling of the same idea.
func chatParts(content any) []map[string]any {
	blocks := []map[string]any{}
	switch value := content.(type) {
	case string:
		if value != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": value})
		}
		return blocks
	case []any:
		for _, part := range value {
			switch item := part.(type) {
			case string:
				if item != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": item})
				}
			case map[string]any:
				switch item["type"] {
				case "text", "input_text", "output_text":
					if text, ok := item["text"].(string); ok && text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": text})
					}
				case "image_url", "input_image":
					if block := imageBlock(imageURLOf(item)); block != nil {
						blocks = append(blocks, block)
					}
				}
			}
		}
	}
	return blocks
}

func imageURLOf(part map[string]any) string {
	for _, key := range []string{"image_url", "url"} {
		switch value := part[key].(type) {
		case string:
			return value
		case map[string]any:
			for _, nested := range []string{"url", "data_url"} {
				if text, ok := value[nested].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func textOfBlocks(blocks []map[string]any) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// toolInput normalises tool arguments. OpenAI carries a JSON string, Anthropic
// a JSON object; a string that is not an object is wrapped rather than dropped,
// because losing a call the model made is worse than re-sending an empty one.
func toolInput(value any) map[string]any {
	switch item := value.(type) {
	case map[string]any:
		return item
	case string:
		if strings.TrimSpace(item) == "" {
			return map[string]any{}
		}
		var parsed any
		if json.Unmarshal([]byte(item), &parsed) != nil {
			return map[string]any{}
		}
		if object, ok := parsed.(map[string]any); ok {
			return object
		}
		return map[string]any{"_": parsed}
	}
	return map[string]any{}
}

type messageBuilder struct {
	system   []string
	messages []map[string]any
	results  []map[string]any
}

func (m *messageBuilder) append(role string, blocks []map[string]any) {
	if len(blocks) == 0 {
		return
	}
	if n := len(m.messages); n > 0 && m.messages[n-1]["role"] == role {
		existing, _ := m.messages[n-1]["content"].([]map[string]any)
		m.messages[n-1]["content"] = append(existing, blocks...)
		return
	}
	m.messages = append(m.messages, map[string]any{"role": role, "content": blocks})
}

func (m *messageBuilder) flushResults() {
	if len(m.results) == 0 {
		return
	}
	pending := m.results
	m.results = nil
	m.append("user", pending)
}

func isThinkingBlock(part any) bool {
	object, ok := part.(map[string]any)
	if !ok {
		return false
	}
	return object["type"] == "thinking" || object["type"] == "redacted_thinking"
}

// fromChat builds one ordered Messages list from a Chat payload. Order is what
// makes a tool round trip legible: an assistant turn that called tools and the
// user turn that supplies the results are one exchange.
func fromChat(payload map[string]any) (*messageBuilder, error) {
	builder := &messageBuilder{}
	rows, _ := payload["messages"].([]any)
	for _, row := range rows {
		message, ok := row.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if text := textOfBlocks(chatParts(message["content"])); text != "" {
				builder.system = append(builder.system, text)
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			builder.results = append(builder.results, map[string]any{"type": "tool_result",
				"tool_use_id": id, "content": textOfBlocks(chatParts(message["content"]))})
		case "assistant":
			builder.flushResults()
			raw, _ := message["content"].([]any)
			calls, _ := message["tool_calls"].([]any)
			if len(calls) > 0 {
				for _, part := range raw {
					if isThinkingBlock(part) {
						return nil, ErrThinkingToolRoundtrip
					}
				}
			}
			blocks := chatParts(message["content"])
			for _, call := range calls {
				entry, ok := call.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := entry["function"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := fn["name"].(string)
				if name == "" {
					continue
				}
				id, _ := entry["id"].(string)
				if id == "" {
					id = name
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name,
					"input": toolInput(fn["arguments"])})
			}
			builder.append("assistant", blocks)
		case "user":
			builder.flushResults()
			builder.append("user", chatParts(message["content"]))
		}
	}
	builder.flushResults()
	return builder, nil
}

// fromResponses builds the same list from a Responses payload. Responses
// interleaves function calls and their outputs as separate items.
func fromResponses(payload map[string]any) (*messageBuilder, error) {
	builder := &messageBuilder{}
	if instructions, ok := payload["instructions"].(string); ok && instructions != "" {
		builder.system = append(builder.system, instructions)
	}
	var input []any
	switch value := payload["input"].(type) {
	case string:
		input = []any{value}
	case []any:
		input = value
	}
	for _, row := range input {
		switch item := row.(type) {
		case string:
			builder.flushResults()
			if item != "" {
				builder.append("user", []map[string]any{{"type": "text", "text": item}})
			}
			continue
		case map[string]any:
			switch item["type"] {
			case "function_call":
				builder.flushResults()
				id := firstString(item["call_id"], item["id"], item["name"])
				name, _ := item["name"].(string)
				builder.append("assistant", []map[string]any{{"type": "tool_use", "id": id,
					"name": name, "input": toolInput(item["arguments"])}})
				continue
			case "function_call_output":
				output := ""
				switch value := item["output"].(type) {
				case string:
					output = value
				case nil:
					output = `""`
				default:
					encoded, _ := json.Marshal(value)
					output = string(encoded)
				}
				id, _ := item["call_id"].(string)
				builder.results = append(builder.results, map[string]any{"type": "tool_result",
					"tool_use_id": id, "content": output})
				continue
			}
			role, _ := item["role"].(string)
			switch role {
			case "system", "developer":
				if text := textOfBlocks(chatParts(item["content"])); text != "" {
					builder.system = append(builder.system, text)
				}
			case "assistant":
				builder.flushResults()
				builder.append("assistant", chatParts(item["content"]))
			default:
				builder.flushResults()
				builder.append("user", chatParts(item["content"]))
			}
		}
	}
	builder.flushResults()
	return builder, nil
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func toolsOf(payload map[string]any) []map[string]any {
	rows, _ := payload["tools"].([]any)
	tools := []map[string]any{}
	for _, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok {
			continue
		}
		// Chat nests the definition under `function`; Responses flattens it.
		fn, ok := entry["function"].(map[string]any)
		if !ok {
			fn = entry
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		// Anthropic has no hosted-tool equivalent, so those definitions are
		// skipped rather than sent as an invalid client tool.
		if typ, ok := entry["type"].(string); ok && typ != "" && typ != "function" {
			continue
		}
		tool := map[string]any{"name": name}
		if description, ok := fn["description"].(string); ok && description != "" {
			tool["description"] = description
		}
		if schema, ok := fn["parameters"].(map[string]any); ok {
			tool["input_schema"] = schema
		} else if schema, ok := fn["input_schema"].(map[string]any); ok {
			tool["input_schema"] = schema
		} else {
			tool["input_schema"] = map[string]any{"type": "object"}
		}
		tools = append(tools, tool)
	}
	return tools
}

func toolChoiceOf(payload map[string]any, tools []map[string]any) map[string]any {
	if len(tools) == 0 {
		return nil
	}
	switch choice := payload["tool_choice"].(type) {
	case string:
		switch choice {
		case "auto", "none":
			return map[string]any{"type": choice}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		name := ""
		if fn, ok := choice["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			name, _ = choice["name"].(string)
		}
		if name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
	}
	return nil
}

func finiteOf(value any) (float64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func maxTokensOf(payload map[string]any) int64 {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value, ok := finiteOf(payload[key]); ok && value > 0 {
			return int64(math.Floor(value))
		}
	}
	return AnthropicMaxTokens
}

func stopOf(payload map[string]any) []string {
	value := payload["stop"]
	if value == nil {
		value = payload["stop_sequences"]
	}
	switch item := value.(type) {
	case string:
		if item != "" {
			return []string{item}
		}
	case []any:
		out := []string{}
		for _, entry := range item {
			if text, ok := entry.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func effortOf(payload map[string]any) string {
	if effort, ok := payload["reasoning_effort"].(string); ok && effort != "" {
		return effort
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			return effort
		}
		if effort, ok := reasoning["effort_level"].(string); ok && effort != "" {
			return effort
		}
	}
	return ""
}

func budgetOf(payload map[string]any) *int64 {
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		if value, ok := finiteOf(thinking["budget_tokens"]); ok {
			return intPtr(int64(value))
		}
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if value, ok := finiteOf(reasoning["max_tokens"]); ok {
			return intPtr(int64(value))
		}
	}
	return nil
}

// ToAnthropicRequest builds the finished Messages body for one Kimi request.
// `max_tokens` is required by the Messages API and has no default there, so it
// is always sent.
func ToAnthropicRequest(route string, payload map[string]any) (map[string]any, error) {
	if route != ProtocolChat && route != ProtocolResponses {
		return nil, ErrInvalidProtocol
	}
	if payload == nil {
		return nil, ErrInvalidRequest
	}
	var builder *messageBuilder
	var err error
	if route == ProtocolChat {
		builder, err = fromChat(payload)
	} else {
		builder, err = fromResponses(payload)
	}
	if err != nil {
		return nil, err
	}
	if len(builder.messages) == 0 {
		return nil, ErrInvalidRequest
	}
	maxTokens := maxTokensOf(payload)
	body := map[string]any{"model": payload["model"], "max_tokens": maxTokens, "messages": builder.messages}
	if len(builder.system) > 0 {
		body["system"] = strings.Join(builder.system, "\n\n")
	}
	tools := toolsOf(payload)
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := toolChoiceOf(payload, tools); choice != nil {
		body["tool_choice"] = choice
	}
	if stop := stopOf(payload); len(stop) > 0 {
		body["stop_sequences"] = stop
	}
	if temperature, ok := finiteOf(payload["temperature"]); ok {
		body["temperature"] = math.Min(1, math.Max(0, temperature))
	}
	if topP, ok := finiteOf(payload["top_p"]); ok {
		body["top_p"] = math.Min(1, math.Max(0, topP))
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok {
		if user, ok := metadata["user_id"].(string); ok {
			body["metadata"] = map[string]any{"user_id": user}
		}
	}
	if stream, ok := payload["stream"].(bool); ok && stream {
		body["stream"] = true
	}
	// `summary` is asked for only when the request names it; an omitted field
	// leaves the model's own default in place.
	summaryPresent := false
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		_, summaryPresent = reasoning["summary"]
	}
	controls, err := ReasoningControlsFor(effortOf(payload), budgetOf(payload), intPtr(maxTokens), summaryPresent)
	if err != nil {
		return nil, err
	}
	if controls.Effort != "" {
		body["output_config"] = map[string]any{"effort": controls.Effort}
	}
	if controls.Thinking != nil {
		body["thinking"] = controls.Thinking
	}
	return body, nil
}

// chatFinish maps Anthropic's stop_reason onto the finish_reason Kimi reads.
var chatFinish = map[string]string{
	"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
	"tool_use": "tool_calls", "refusal": "content_filter", "pause_turn": "stop",
}

// anthropicUsage splits ordinary, cache-read and cache-created input. The
// OpenAI input total is their sum; an absent count stays absent, never zero.
type anthropicUsageValue struct {
	input, output, cached, cacheWrite *int64
}

func readAnthropicUsage(value map[string]any) *anthropicUsageValue {
	if value == nil {
		return nil
	}
	nested := (*int64)(nil)
	if breakdown, ok := value["cache_creation"].(map[string]any); ok {
		var sum int64
		found := false
		for _, key := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
			if number, ok := finiteOf(breakdown[key]); ok {
				sum += int64(number)
				found = true
			}
		}
		if found {
			nested = intPtr(sum)
		}
	}
	cacheWrite := nested
	if number, ok := finiteOf(value["cache_creation_input_tokens"]); ok {
		cacheWrite = intPtr(int64(number))
	}
	var total int64
	known := false
	for _, candidate := range []*int64{
		numberPtr(value["input_tokens"]),
		numberPtr(value["cache_read_input_tokens"]),
		cacheWrite,
	} {
		if candidate != nil {
			total += *candidate
			known = true
		}
	}
	var input *int64
	if known {
		input = intPtr(total)
	}
	output := numberPtr(value["output_tokens"])
	cached := numberPtr(value["cache_read_input_tokens"])
	if input == nil && output == nil {
		return nil
	}
	return &anthropicUsageValue{input: input, output: output, cached: cached, cacheWrite: cacheWrite}
}

func numberPtr(value any) *int64 {
	if number, ok := finiteOf(value); ok {
		return intPtr(int64(number))
	}
	return nil
}

func shapedUsage(value *anthropicUsageValue, inputName, outputName, detailsName string) map[string]any {
	if value == nil {
		return nil
	}
	out := map[string]any{}
	if value.input != nil {
		out[inputName] = *value.input
	}
	if value.output != nil {
		out[outputName] = *value.output
	}
	if value.input != nil && value.output != nil {
		out["total_tokens"] = *value.input + *value.output
	}
	if value.cached != nil {
		out[detailsName] = map[string]any{"cached_tokens": *value.cached}
	}
	if value.cacheWrite != nil {
		out["cache_write_tokens"] = *value.cacheWrite
	}
	return out
}

func chatUsage(value map[string]any) map[string]any {
	return shapedUsage(readAnthropicUsage(value), "prompt_tokens", "completion_tokens", "prompt_tokens_details")
}

func responsesUsage(value map[string]any) map[string]any {
	return shapedUsage(readAnthropicUsage(value), "input_tokens", "output_tokens", "input_tokens_details")
}

func thinkingText(blocks []map[string]any) string {
	parts := []string{}
	for _, block := range blocks {
		if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
			if text, ok := block["thinking"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func assertRoundtripSupported(blocks []map[string]any) error {
	thinking, tool := false, false
	for _, block := range blocks {
		switch block["type"] {
		case "thinking", "redacted_thinking":
			thinking = true
		case "tool_use":
			tool = true
		}
	}
	if thinking && tool {
		return ErrThinkingToolRoundtrip
	}
	return nil
}

func blocksOf(message map[string]any) []map[string]any {
	raw, ok := message["content"].([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, part := range raw {
		if object, ok := part.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

// FromAnthropicChat renders one finished Messages reply as a Chat completion.
func FromAnthropicChat(message map[string]any, now int64) (map[string]any, error) {
	blocks := blocksOf(message)
	if err := assertRoundtripSupported(blocks); err != nil {
		return nil, err
	}
	calls := []map[string]any{}
	for _, block := range blocks {
		if block["type"] != "tool_use" {
			continue
		}
		arguments, _ := json.Marshal(toolInput(block["input"]))
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		calls = append(calls, map[string]any{"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(arguments)}})
	}
	messageOut := map[string]any{"role": "assistant"}
	if text := textOfBlocks(blocks); text != "" {
		messageOut["content"] = text
	} else {
		messageOut["content"] = nil
	}
	if reasoning := thinkingText(blocks); reasoning != "" {
		messageOut["reasoning_content"] = reasoning
	}
	if len(calls) > 0 {
		messageOut["tool_calls"] = calls
	}
	stop, _ := message["stop_reason"].(string)
	finish, ok := chatFinish[stop]
	if !ok {
		finish = "stop"
	}
	id, _ := message["id"].(string)
	model, _ := message["model"].(string)
	out := map[string]any{"id": id, "object": "chat.completion", "created": now,
		"model": model, "choices": []any{map[string]any{"index": 0, "message": messageOut, "finish_reason": finish}}}
	if usage, ok := message["usage"].(map[string]any); ok {
		if shaped := chatUsage(usage); shaped != nil {
			out["usage"] = shaped
		}
	}
	return out, nil
}

// FromAnthropicResponses renders the same reply as a Responses object. The
// `output` list mirrors the content blocks one-for-one, so a client reading
// either the text or the reasoning sees the whole reply.
func FromAnthropicResponses(message map[string]any, now int64) (map[string]any, error) {
	blocks := blocksOf(message)
	if err := assertRoundtripSupported(blocks); err != nil {
		return nil, err
	}
	id, _ := message["id"].(string)
	base := id
	if base == "" {
		base = "anthropic"
	}
	output := []any{}
	for index, block := range blocks {
		itemID := base + "_item_" + itoa(index)
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			output = append(output, map[string]any{"id": itemID, "type": "message", "role": "assistant",
				"status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}})
		case "tool_use":
			arguments, _ := json.Marshal(toolInput(block["input"]))
			callID, _ := block["id"].(string)
			name, _ := block["name"].(string)
			output = append(output, map[string]any{"id": itemID, "type": "function_call", "call_id": callID,
				"name": name, "arguments": string(arguments)})
		case "thinking", "redacted_thinking":
			if text, ok := block["thinking"].(string); ok && text != "" {
				output = append(output, map[string]any{"id": itemID, "type": "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": text}}})
			}
		}
	}
	stop, _ := message["stop_reason"].(string)
	model, _ := message["model"].(string)
	out := map[string]any{"id": id, "object": "response", "created_at": now, "model": model,
		"status": "completed", "output": output}
	if stop == "max_tokens" {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage, ok := message["usage"].(map[string]any); ok {
		if shaped := responsesUsage(usage); shaped != nil {
			out["usage"] = shaped
		}
	}
	return out, nil
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

// AnthropicErrorBody rewrites a vendor error into the envelope Kimi already
// parses, keeping the vendor's own `type` and a bounded message.
func AnthropicErrorBody(raw []byte, status int) []byte {
	typ := ""
	message := ""
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if inner, ok := payload["error"].(map[string]any); ok {
			if value, ok := inner["type"].(string); ok {
				if vendorTypePattern.MatchString(value) {
					typ = value
				}
			}
			if value, ok := inner["message"].(string); ok {
				message = value
				if len(message) > 512 {
					message = message[:512]
				}
			}
		}
	}
	if typ == "" {
		if status == 429 {
			typ = "rate_limit_error"
		} else {
			typ = "api_error"
		}
	}
	if message == "" {
		message = "upstream_http_" + itoa(status)
	}
	body, _ := json.Marshal(map[string]any{"type": "error",
		"error": map[string]any{"type": typ, "message": message}})
	return body
}

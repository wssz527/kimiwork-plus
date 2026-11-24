package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// This file bridges the OpenAI Chat shape Kimi's client speaks and the OpenAI
// Responses shape some upstreams only offer. Both directions are total: every
// message, tool call, image and reasoning summary the Chat payload carries has a
// Responses spelling, and the reply is re-shaped into the chunks and envelopes
// the gateway already accounts for.

// ToResponsesFromChat builds the Responses body for one already-normalised Chat
// request. `model` supplies the catalog reasoning capabilities; a nil model still
// converts the request with the reasoning ask the payload itself states.
func ToResponsesFromChat(payload map[string]any, model *Model) (map[string]any, error) {
	if payload == nil {
		return nil, ErrInvalidRequest
	}
	body := map[string]any{"model": payload["model"]}
	instructions, _ := payload["instructions"].(string)
	input := []any{}
	pending := []string{}
	leading := true
	rows, _ := payload["messages"].([]any)
	for _, row := range rows {
		message, ok := row.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			text := strings.Join(responsesTextParts(message["content"]), "\n")
			if text == "" {
				continue
			}
			if leading {
				if instructions == "" {
					instructions = text
				} else {
					instructions += "\n\n" + text
				}
				continue
			}
			input = append(input, responsesMessage("developer",
				[]any{map[string]any{"type": "input_text", "text": text}}))
			continue
		}
		leading = false
		switch role {
		case "user":
			if content := responsesUserContent(message["content"]); len(content) > 0 {
				input = append(input, responsesMessage("user", content))
			}
		case "assistant":
			if content := responsesAssistantContent(message["content"]); len(content) > 0 {
				input = append(input, responsesMessage("assistant", content))
			}
			calls := responsesCalls(message["tool_calls"])
			input = append(input, calls...)
			for _, call := range calls {
				if row, ok := call.(map[string]any); ok {
					if id, _ := row["call_id"].(string); id != "" {
						pending = append(pending, id)
					}
				}
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			callID = strings.TrimSpace(callID)
			if callID == "" {
				// Responses refuses an empty call_id, so an output that names no
				// call is paired with the oldest call still waiting for a result.
				if len(pending) > 0 {
					callID, pending = pending[0], pending[1:]
				}
			} else {
				for i, id := range pending {
					if id == callID {
						pending = append(pending[:i], pending[i+1:]...)
						break
					}
				}
			}
			if callID == "" {
				continue
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID,
				"output": responsesToolOutput(message["content"])})
		}
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if len(input) > 0 {
		body["input"] = input
	}
	if value, ok := finiteOf(payload["max_completion_tokens"]); ok && value > 0 {
		body["max_output_tokens"] = int64(value)
	} else if value, ok := finiteOf(payload["max_tokens"]); ok && value > 0 {
		body["max_output_tokens"] = int64(value)
	}
	if value, ok := finiteOf(payload["temperature"]); ok {
		body["temperature"] = value
	}
	if value, ok := finiteOf(payload["top_p"]); ok {
		body["top_p"] = value
	}
	if stream, ok := payload["stream"].(bool); ok {
		body["stream"] = stream
	}
	if key, ok := payload["prompt_cache_key"].(string); ok && key != "" {
		body["prompt_cache_key"] = key
	}
	if parallel, ok := payload["parallel_tool_calls"].(bool); ok {
		body["parallel_tool_calls"] = parallel
	}
	if tools := responsesTools(payload["tools"]); len(tools) > 0 {
		body["tools"] = tools
		if choice := responsesToolChoice(payload["tool_choice"]); choice != nil {
			body["tool_choice"] = choice
		}
	}
	if reasoning := responsesReasoning(payload, model); reasoning != nil {
		body["reasoning"] = reasoning
	}
	return body, nil
}

func responsesMessage(role string, content []any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": content}
}

// responsesTextParts collects the plain text of a Chat content value. The
// Responses API accepts text only where a message is text, so every other part
// is dropped instead of being forwarded as something it is not.
func responsesTextParts(content any) []string {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case []any:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			switch item := part.(type) {
			case string:
				if item != "" {
					parts = append(parts, item)
				}
			case map[string]any:
				switch item["type"] {
				case "text", "input_text", "output_text":
					if text, ok := item["text"].(string); ok && text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return parts
	}
	return nil
}

// responsesToolOutput keeps a tool result the model can use. Chat allows any
// JSON value in that slot while Responses takes a string, so a structured value
// is encoded rather than dropped.
func responsesToolOutput(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		texts := make([]string, 0, len(value))
		for _, part := range value {
			switch item := part.(type) {
			case string:
				texts = append(texts, item)
			case map[string]any:
				if typ, _ := item["type"].(string); typ != "text" && typ != "input_text" && typ != "output_text" {
					return responsesJSONText(value)
				}
				text, ok := item["text"].(string)
				if !ok {
					return responsesJSONText(value)
				}
				texts = append(texts, text)
			default:
				return responsesJSONText(value)
			}
		}
		return strings.Join(texts, "\n")
	}
	return responsesJSONText(content)
}

func responsesJSONText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func responsesUserContent(content any) []any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": value}}
	case []any:
		parts := []any{}
		for _, part := range value {
			switch item := part.(type) {
			case string:
				if item != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": item})
				}
			case map[string]any:
				switch item["type"] {
				case "text", "input_text":
					if text, ok := item["text"].(string); ok && text != "" {
						parts = append(parts, map[string]any{"type": "input_text", "text": text})
					}
				case "image_url", "input_image":
					url, detail := responsesImageRef(item)
					if url == "" {
						continue
					}
					image := map[string]any{"type": "input_image", "image_url": url}
					if detail != "" {
						image["detail"] = detail
					}
					parts = append(parts, image)
				}
			}
		}
		return parts
	}
	return nil
}

// responsesImageRef accepts both the Chat object form (`image_url` naming a
// `{url, detail}` object) and the flattened Responses form.
func responsesImageRef(part map[string]any) (string, string) {
	detail, _ := part["detail"].(string)
	for _, key := range []string{"image_url", "url", "data_url"} {
		switch value := part[key].(type) {
		case string:
			if value != "" {
				return value, detail
			}
		case map[string]any:
			if url, ok := value["url"].(string); ok && url != "" {
				if nested, ok := value["detail"].(string); ok && nested != "" {
					detail = nested
				}
				return url, detail
			}
		}
	}
	return "", ""
}

func responsesAssistantContent(content any) []any {
	parts := []any{}
	for _, text := range responsesTextParts(content) {
		parts = append(parts, map[string]any{"type": "output_text", "text": text})
	}
	return parts
}

func responsesCalls(toolCalls any) []any {
	rows, _ := toolCalls.([]any)
	out := make([]any, 0, len(rows))
	for i, row := range rows {
		call, _ := row.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		callID := firstString(call["id"])
		if callID == "" {
			// Responses refuses an empty call_id, so a call without an id gets a
			// stable one its output can be paired with.
			callID = "call_" + itoa(i)
		}
		out = append(out, map[string]any{"type": "function_call", "call_id": callID, "name": name,
			"arguments": responsesArguments(fn["arguments"])})
	}
	return out
}

func responsesArguments(value any) string {
	text, ok := responsesArgumentsText(value)
	if !ok || strings.TrimSpace(text) == "" {
		return "{}"
	}
	return text
}

// responsesArgumentsText keeps the exact argument string Chat carries. A
// non-string value is encoded rather than dropped, because losing a call the
// model made is worse than re-sending an empty one.
func responsesArgumentsText(value any) (string, bool) {
	switch item := value.(type) {
	case nil:
		return "", false
	case string:
		if strings.TrimSpace(item) == "" {
			return "", false
		}
		return item, true
	default:
		encoded, err := json.Marshal(item)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
}

func responsesTools(tools any) []map[string]any {
	rows, _ := tools.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if typ, ok := entry["type"].(string); ok && typ != "" && typ != "function" {
			continue
		}
		fn, ok := entry["function"].(map[string]any)
		if !ok {
			fn = entry
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		tool := map[string]any{"type": "function", "name": name}
		if description, ok := fn["description"].(string); ok && description != "" {
			tool["description"] = description
		}
		if schema, ok := fn["parameters"].(map[string]any); ok {
			tool["parameters"] = schema
		} else {
			tool["parameters"] = map[string]any{"type": "object"}
		}
		if strict, ok := fn["strict"].(bool); ok {
			tool["strict"] = strict
		}
		out = append(out, tool)
	}
	return out
}

func responsesToolChoice(choice any) any {
	switch value := choice.(type) {
	case string:
		switch value {
		case "none", "auto", "required":
			return value
		}
	case map[string]any:
		name := ""
		if fn, ok := value["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			name, _ = value["name"].(string)
		}
		if name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

// responsesReasoning maps the Chat thinking controls onto the Responses
// `reasoning` object. OpenAI has no separate budget field there; only an effort
// level and a summary mode, so a request that cannot name a level falls back to
// the model's default effort and otherwise asks for a summary alone.
func responsesReasoning(payload map[string]any, model *Model) map[string]any {
	if model != nil && model.Reasoning != nil && !*model.Reasoning {
		// A model declared as non-reasoning rejects the object outright, even
		// when the request still carries controls from another model.
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(effortOf(payload)))
	disabled, enabled := false, false
	switch value := payload["thinking"].(type) {
	case map[string]any:
		switch typ, _ := value["type"].(string); strings.ToLower(typ) {
		case "disabled":
			disabled = true
		case "enabled":
			enabled = true
		}
	case bool:
		if value {
			enabled = true
		} else {
			disabled = true
		}
	}
	if effort != "" && offEfforts[effort] {
		disabled = true
	}
	_, reasoningPresent := payload["reasoning"].(map[string]any)
	modelReasoning := model != nil && model.Reasoning != nil && *model.Reasoning
	if disabled || (!enabled && !reasoningPresent && effort == "" && !modelReasoning) {
		if disabled && modelSupportsEffort(model, "none") {
			return map[string]any{"effort": "none"}
		}
		return nil
	}
	level := effort
	if offEfforts[level] || !vendorTypePattern.MatchString(level) {
		level = ""
	}
	if level == "" && model != nil && model.DefaultEffort != nil {
		level = strings.ToLower(strings.TrimSpace(*model.DefaultEffort))
	}
	if offEfforts[level] {
		// A default effort can be a switch-off value; it must not be sent as a
		// level, and the summary alone would be malformed without one.
		if modelSupportsEffort(model, "none") {
			return map[string]any{"effort": "none"}
		}
		return nil
	}
	out := map[string]any{"summary": "auto"}
	if level != "" && vendorTypePattern.MatchString(level) {
		out["effort"] = level
	}
	return out
}

func modelSupportsEffort(model *Model, effort string) bool {
	if model == nil || model.ReasoningEfforts == nil {
		return false
	}
	return contains(*model.ReasoningEfforts, effort)
}

// FromResponsesToChat folds one finished Responses object into the single Chat
// completion the client asked for.
func FromResponsesToChat(response map[string]any, now int64) (map[string]any, error) {
	if response == nil {
		return nil, ErrInvalidUpstreamResponse
	}
	status, _ := response["status"].(string)
	if status == "failed" {
		return nil, ErrInvalidUpstreamResponse
	}
	text := strings.Builder{}
	reasoning := strings.Builder{}
	calls := []map[string]any{}
	contentFilter := false
	if details, ok := response["incomplete_details"].(map[string]any); ok {
		if reason, _ := details["reason"].(string); reason == "content_filter" {
			contentFilter = true
		}
	}
	items, _ := response["output"].([]any)
	for _, row := range items {
		item, ok := row.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			writeOutputText(&text, item["content"])
		case "reasoning":
			if summary := responsesSummaryText(item); summary != "" {
				reasoning.WriteString(summary)
			} else {
				reasoning.WriteString(responsesReasoningContent(item))
			}
		case "function_call":
			callID := firstString(item["call_id"], item["id"], item["name"])
			name, _ := item["name"].(string)
			arguments := responsesArguments(item["arguments"])
			calls = append(calls, map[string]any{"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": arguments}})
		}
	}
	message := map[string]any{"role": "assistant"}
	switch {
	case text.Len() > 0:
		message["content"] = text.String()
	case len(calls) > 0:
		message["content"] = nil
	default:
		message["content"] = ""
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := "stop"
	switch {
	case len(calls) > 0:
		finish = "tool_calls"
	case contentFilter:
		finish = "content_filter"
	case status == "incomplete":
		finish = "length"
	}
	id, _ := response["id"].(string)
	model, _ := response["model"].(string)
	out := map[string]any{"id": id, "object": "chat.completion", "created": now, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if usage, ok := response["usage"].(map[string]any); ok {
		if shaped := responsesChatUsage(usage); shaped != nil {
			out["usage"] = shaped
		}
	}
	return out, nil
}

func writeOutputText(builder *strings.Builder, content any) {
	parts, _ := content.([]any)
	for _, part := range parts {
		item, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := item["text"].(string); ok {
			builder.WriteString(text)
		}
	}
}

func responsesSummaryText(item map[string]any) string {
	summary, _ := item["summary"].([]any)
	builder := strings.Builder{}
	for _, part := range summary {
		entry, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func responsesReasoningContent(item map[string]any) string {
	content, _ := item["content"].([]any)
	builder := strings.Builder{}
	for _, part := range content {
		entry, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := entry["type"].(string); typ != "reasoning_text" {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

// responsesChatUsage rewrites the Responses usage object into the Chat one,
// keeping cache reads and reasoning output distinguishable so the panel can
// price them.
func responsesChatUsage(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := map[string]any{}
	input := numberPtr(value["input_tokens"])
	output := numberPtr(value["output_tokens"])
	if input != nil {
		out["prompt_tokens"] = *input
	}
	if output != nil {
		out["completion_tokens"] = *output
	}
	if total := numberPtr(value["total_tokens"]); total != nil {
		out["total_tokens"] = *total
	} else if input != nil && output != nil {
		out["total_tokens"] = *input + *output
	}
	if details, ok := value["input_tokens_details"].(map[string]any); ok {
		if cached := numberPtr(details["cached_tokens"]); cached != nil {
			out["prompt_tokens_details"] = map[string]any{"cached_tokens": *cached}
		}
	}
	if details, ok := value["output_tokens_details"].(map[string]any); ok {
		if reasoning := numberPtr(details["reasoning_tokens"]); reasoning != nil {
			out["completion_tokens_details"] = map[string]any{"reasoning_tokens": *reasoning}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResponsesErrorBody rewrites a failed Responses reply into the envelope Kimi
// already parses, keeping the vendor's status and vocabulary.
func ResponsesErrorBody(raw []byte, status int) []byte {
	typ, code, message := "", "", ""
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if inner, ok := payload["error"].(map[string]any); ok {
			typ, _ = inner["type"].(string)
			code, _ = inner["code"].(string)
			message, _ = inner["message"].(string)
		} else {
			typ, _ = payload["type"].(string)
			code, _ = payload["code"].(string)
			message, _ = payload["message"].(string)
		}
	}
	if !vendorTypePattern.MatchString(typ) {
		typ = ""
	}
	if !vendorTypePattern.MatchString(code) {
		code = ""
	}
	if typ == "" {
		if status == 429 {
			typ = "rate_limit_error"
		} else {
			typ = "api_error"
		}
	}
	if len(message) > 512 {
		cut := 512
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut]
	}
	if message == "" {
		message = "upstream_http_" + itoa(status)
	}
	inner := map[string]any{"type": typ, "message": message}
	if code != "" {
		inner["code"] = code
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": inner})
	return body
}

// ResponsesChatStreamTranslator rewrites one Responses SSE stream into the
// `chat.completion.chunk` stream Kimi reads. The terminal state is decided only
// by the upstream's own terminal event: a stream that breaks or ends without one
// is reported as incomplete, and a failed stream never fabricates `[DONE]`.
type ResponsesChatStreamTranslator struct {
	maxBuffered int
	buffer      string
	eventName   string
	eventData   []string
	partial     []byte
	responseID  string
	model       string
	started     bool
	closed      bool
	outcome     Outcome
	items       map[string]*responsesStreamItem
	order       []string
	toolCount   int
	hasToolCall bool
}

type responsesStreamItem struct {
	id               string
	kind             string
	outputIndex      int
	toolIndex        int
	callID           string
	name             string
	text             string
	textSent         int
	reasoning        string
	reasoningSent    int
	reasoningChannel string
	arguments        string
	argsSent         int
	announced        bool
}

// NewResponsesChatTranslator returns a translator for one streamed reply.
func NewResponsesChatTranslator(maxBufferedBytes int) (*ResponsesChatStreamTranslator, error) {
	if maxBufferedBytes < 1 {
		return nil, ErrInvalidProtocol
	}
	return &ResponsesChatStreamTranslator{maxBuffered: maxBufferedBytes,
		items: map[string]*responsesStreamItem{}}, nil
}

// Outcome reports the terminal state, or "" while the stream is still open.
func (t *ResponsesChatStreamTranslator) Outcome() Outcome {
	if t == nil {
		return ""
	}
	return t.outcome
}

// Closed reports whether a terminal frame has been emitted.
func (t *ResponsesChatStreamTranslator) Closed() bool { return t != nil && t.closed }

// Push feeds one upstream chunk and returns the bytes to forward.
func (t *ResponsesChatStreamTranslator) Push(chunk []byte) []byte {
	if t == nil || t.closed {
		return nil
	}
	t.buffer += string(decodeSSEChunk(&t.partial, chunk))
	if len(t.buffer) > t.maxBuffered {
		out := t.fail("upstream_event_too_large", "upstream_sse_event_too_large")
		t.buffer = ""
		return out
	}
	var out bytes.Buffer
	for {
		line, ok := takeSSELine(&t.buffer)
		if !ok {
			break
		}
		if text := t.line(line); len(text) > 0 {
			out.Write(text)
		}
		if t.closed {
			t.buffer = ""
			break
		}
	}
	return out.Bytes()
}

// End flushes whatever the upstream left behind. A stream without its terminal
// event is incomplete, never completed.
func (t *ResponsesChatStreamTranslator) End() []byte {
	if t == nil || t.closed {
		return nil
	}
	t.buffer += string(decodeSSEChunk(&t.partial, nil))
	var out bytes.Buffer
	if strings.HasSuffix(t.buffer, "\r") {
		t.buffer = t.buffer[:len(t.buffer)-1]
	}
	if t.buffer != "" {
		out.Write(t.line(t.buffer))
	}
	t.buffer = ""
	if !t.closed {
		out.Write(t.line(""))
	}
	if !t.closed {
		out.Write(t.fail("incomplete_stream", "upstream_message_stop_missing"))
	}
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) line(line string) []byte {
	if line != "" {
		switch {
		case strings.HasPrefix(line, "event:"):
			t.eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			t.eventData = append(t.eventData, strings.TrimSpace(line[len("data:"):]))
		}
		return nil
	}
	raw := strings.Join(t.eventData, "\n")
	event := t.eventName
	t.eventName = ""
	t.eventData = nil
	if raw == "" || raw == "[DONE]" {
		return nil
	}
	var value map[string]any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return t.fail("invalid_upstream_event", "upstream_sse_json_invalid")
	}
	typ, _ := value["type"].(string)
	if typ == "" {
		typ = event
	}
	return t.event(typ, value)
}

func (t *ResponsesChatStreamTranslator) event(typ string, value map[string]any) []byte {
	if t.closed {
		return nil
	}
	switch typ {
	case "response.created", "response.in_progress":
		return t.created(value)
	case "response.output_item.added":
		return t.itemAdded(value)
	case "response.output_item.done":
		return t.itemDone(value)
	case "response.output_text.delta":
		return t.textDelta(value)
	case "response.output_text.done":
		return t.textDone(value)
	case "response.reasoning_summary_text.delta":
		return t.reasoningDelta(value, "summary")
	case "response.reasoning_text.delta":
		return t.reasoningDelta(value, "text")
	case "response.reasoning_summary_text.done":
		return t.reasoningDone(value, "summary")
	case "response.reasoning_text.done":
		return t.reasoningDone(value, "text")
	case "response.function_call_arguments.delta":
		return t.argumentsDelta(value)
	case "response.function_call_arguments.done":
		return t.argumentsDone(value)
	case "response.completed":
		return t.complete(value, false)
	case "response.incomplete":
		return t.complete(value, true)
	case "response.failed":
		return t.failed(value)
	case "error":
		return t.eventError(value)
	default:
		return nil
	}
}

func (t *ResponsesChatStreamTranslator) created(value map[string]any) []byte {
	response, _ := value["response"].(map[string]any)
	if t.responseID == "" {
		t.responseID, _ = response["id"].(string)
	}
	if t.model == "" {
		t.model, _ = response["model"].(string)
	}
	return t.ensureStarted()
}

func (t *ResponsesChatStreamTranslator) ensureStarted() []byte {
	if t.started {
		return nil
	}
	t.started = true
	return t.chunk(map[string]any{"role": "assistant"})
}

func (t *ResponsesChatStreamTranslator) chunk(delta map[string]any) []byte {
	payload := map[string]any{"object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta}}}
	if t.responseID != "" {
		payload["id"] = t.responseID
	}
	if t.model != "" {
		payload["model"] = t.model
	}
	return []byte("data: " + rawJSON(payload) + "\n\n")
}

func (t *ResponsesChatStreamTranslator) finishChunk(reason string) []byte {
	payload := map[string]any{"object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}}
	if t.responseID != "" {
		payload["id"] = t.responseID
	}
	if t.model != "" {
		payload["model"] = t.model
	}
	return []byte("data: " + rawJSON(payload) + "\n\n")
}

func (t *ResponsesChatStreamTranslator) item(id string, index int, kind string) *responsesStreamItem {
	if id == "" {
		for _, key := range t.order {
			existing, ok := t.items[key]
			if !ok || existing.outputIndex != index || !kindCompatible(existing.kind, kind) {
				continue
			}
			if kind != "" && existing.kind == "" {
				existing.kind = kind
			}
			return existing
		}
		key := "index:" + itoa(index) + ":" + kind
		created := &responsesStreamItem{kind: kind, outputIndex: index}
		t.items[key] = created
		t.order = append(t.order, key)
		return created
	}
	if existing, ok := t.items[id]; ok {
		if kind != "" && existing.kind == "" {
			existing.kind = kind
		}
		return existing
	}
	// A placeholder opened while the upstream omitted `item_id` is claimed here,
	// instead of opening a second item that would repeat its text or arguments.
	for _, key := range t.order {
		existing, ok := t.items[key]
		if !ok || existing.id != "" || existing.outputIndex != index || !kindCompatible(existing.kind, kind) {
			continue
		}
		existing.id = id
		if kind != "" {
			existing.kind = kind
		}
		delete(t.items, key)
		t.items[id] = existing
		for i, other := range t.order {
			if other == key {
				t.order[i] = id
				break
			}
		}
		return existing
	}
	created := &responsesStreamItem{id: id, kind: kind, outputIndex: index}
	t.items[id] = created
	t.order = append(t.order, id)
	return created
}

// kindCompatible reports whether two item kinds can be the same item. An empty
// kind means the upstream has not said yet, so it can still become anything.
func kindCompatible(a, b string) bool {
	return a == "" || b == "" || a == b
}

func (t *ResponsesChatStreamTranslator) itemFor(value map[string]any, kind string) *responsesStreamItem {
	id, _ := value["item_id"].(string)
	index := 0
	if number, ok := finiteOf(value["output_index"]); ok {
		index = int(number)
	}
	return t.item(id, index, kind)
}

func (t *ResponsesChatStreamTranslator) itemAdded(value map[string]any) []byte {
	source, _ := value["item"].(map[string]any)
	if source == nil {
		return nil
	}
	kind, _ := source["type"].(string)
	id, _ := source["id"].(string)
	index := 0
	if number, ok := finiteOf(value["output_index"]); ok {
		index = int(number)
	}
	item := t.item(id, index, kind)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	switch kind {
	case "function_call":
		item.kind = "function_call"
		item.callID = firstString(source["call_id"], source["id"], item.callID)
		if name, ok := source["name"].(string); ok && name != "" {
			item.name = name
		}
		out.Write(t.announceTool(item))
		if arguments, ok := responsesArgumentsText(source["arguments"]); ok {
			out.Write(t.fillArguments(item, arguments))
		}
	}
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) itemDone(value map[string]any) []byte {
	source, _ := value["item"].(map[string]any)
	if source == nil {
		return nil
	}
	kind, _ := source["type"].(string)
	id, _ := source["id"].(string)
	index := 0
	if number, ok := finiteOf(value["output_index"]); ok {
		index = int(number)
	}
	item := t.item(id, index, kind)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	switch kind {
	case "function_call":
		item.kind = "function_call"
		item.callID = firstString(source["call_id"], source["id"], item.callID)
		if name, ok := source["name"].(string); ok && name != "" {
			item.name = name
		}
		out.Write(t.announceTool(item))
		if arguments, ok := responsesArgumentsText(source["arguments"]); ok {
			out.Write(t.fillArguments(item, arguments))
		}
	case "message":
		item.kind = "message"
		text := strings.Builder{}
		writeOutputText(&text, source["content"])
		out.Write(t.fillText(item, text.String()))
	case "reasoning":
		item.kind = "reasoning"
		out.Write(t.fillReasoning(item, responsesSummaryText(source), responsesReasoningContent(source)))
	}
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) announceTool(item *responsesStreamItem) []byte {
	if item.announced {
		return nil
	}
	item.announced = true
	item.toolIndex = t.toolCount
	t.toolCount++
	t.hasToolCall = true
	return t.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": item.toolIndex,
		"id": firstString(item.callID, item.id), "type": "function",
		"function": map[string]any{"name": item.name, "arguments": ""}}}})
}

func (t *ResponsesChatStreamTranslator) fillArguments(item *responsesStreamItem, full string) []byte {
	if full == "" || len(full) <= item.argsSent {
		return nil
	}
	emitted := item.argsSent
	if emitted > len(item.arguments) {
		emitted = len(item.arguments)
	}
	if !strings.HasPrefix(full, item.arguments[:emitted]) {
		// The full form cannot be reconciled with what the client already has,
		// so nothing is invented.
		return nil
	}
	tail := full[emitted:]
	item.arguments = full
	item.argsSent = len(full)
	if !item.announced {
		return nil
	}
	return t.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": item.toolIndex,
		"function": map[string]any{"arguments": tail}}}})
}

func (t *ResponsesChatStreamTranslator) textDelta(value map[string]any) []byte {
	delta, _ := value["delta"].(string)
	if delta == "" {
		return nil
	}
	item := t.itemFor(value, "message")
	item.kind = "message"
	item.text += delta
	item.textSent = len(item.text)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	out.Write(t.chunk(map[string]any{"content": delta}))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) textDone(value map[string]any) []byte {
	text, _ := value["text"].(string)
	if text == "" {
		return nil
	}
	item := t.itemFor(value, "message")
	var out bytes.Buffer
	out.Write(t.fillText(item, text))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) fillText(item *responsesStreamItem, full string) []byte {
	if item.kind == "" {
		item.kind = "message"
	}
	if full == "" || len(full) <= item.textSent {
		return nil
	}
	emitted := item.textSent
	if emitted > len(item.text) {
		emitted = len(item.text)
	}
	if !strings.HasPrefix(full, item.text[:emitted]) {
		return nil
	}
	tail := full[emitted:]
	item.text = full
	item.textSent = len(full)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	out.Write(t.chunk(map[string]any{"content": tail}))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) reasoningDelta(value map[string]any, channel string) []byte {
	delta, _ := value["delta"].(string)
	if delta == "" {
		return nil
	}
	item := t.itemFor(value, "reasoning")
	item.kind = "reasoning"
	if item.reasoningChannel != "" && item.reasoningChannel != channel {
		// Upstreams that emit both a summary and the raw reasoning for the same
		// item would otherwise be stitched into the text twice. The first channel
		// seen wins.
		return nil
	}
	item.reasoningChannel = channel
	item.reasoning += delta
	item.reasoningSent = len(item.reasoning)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	out.Write(t.chunk(map[string]any{"reasoning_content": delta}))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) reasoningDone(value map[string]any, channel string) []byte {
	text, _ := value["text"].(string)
	if text == "" {
		return nil
	}
	item := t.itemFor(value, "reasoning")
	if channel == "text" {
		return t.fillReasoning(item, "", text)
	}
	return t.fillReasoning(item, text, "")
}

func (t *ResponsesChatStreamTranslator) fillReasoning(item *responsesStreamItem, summary, text string) []byte {
	if item.kind == "" {
		item.kind = "reasoning"
	}
	full, channel := summary, "summary"
	switch item.reasoningChannel {
	case "summary":
		full = summary
	case "text":
		full, channel = text, "text"
	default:
		if full == "" {
			full, channel = text, "text"
		}
	}
	if full == "" || len(full) <= item.reasoningSent {
		return nil
	}
	emitted := item.reasoningSent
	if emitted > len(item.reasoning) {
		emitted = len(item.reasoning)
	}
	if item.reasoningChannel != "" && !strings.HasPrefix(full, item.reasoning[:emitted]) {
		return nil
	}
	tail := full[emitted:]
	item.reasoningChannel = channel
	item.reasoning = full
	item.reasoningSent = len(full)
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	out.Write(t.chunk(map[string]any{"reasoning_content": tail}))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) argumentsDelta(value map[string]any) []byte {
	delta, _ := value["delta"].(string)
	if delta == "" {
		return nil
	}
	item := t.itemFor(value, "function_call")
	if item.kind != "function_call" {
		item.kind = "function_call"
	}
	item.arguments += delta
	if !item.announced {
		// Without the item's name the first tool frame cannot be formed; the full
		// arguments are sent on output_item.done or the terminal event.
		return nil
	}
	item.argsSent = len(item.arguments)
	return t.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": item.toolIndex,
		"function": map[string]any{"arguments": delta}}}})
}

func (t *ResponsesChatStreamTranslator) argumentsDone(value map[string]any) []byte {
	item := t.itemFor(value, "function_call")
	if name, ok := value["name"].(string); ok && name != "" {
		item.name = name
	}
	if callID, ok := value["call_id"].(string); ok && callID != "" {
		item.callID = callID
	}
	arguments, ok := responsesArgumentsText(value["arguments"])
	if !ok {
		return nil
	}
	if item.name == "" && !item.announced {
		// Without a name the first tool frame cannot be formed; the arguments wait
		// for output_item.done or the terminal event, which carry it.
		item.arguments = arguments
		return nil
	}
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	out.Write(t.announceTool(item))
	out.Write(t.fillArguments(item, arguments))
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) complete(value map[string]any, incomplete bool) []byte {
	if t.closed {
		return nil
	}
	if incomplete {
		t.outcome = OutcomeIncomplete
	} else {
		t.outcome = OutcomeCompleted
	}
	response, _ := value["response"].(map[string]any)
	if t.responseID == "" {
		t.responseID, _ = response["id"].(string)
	}
	if t.model == "" {
		t.model, _ = response["model"].(string)
	}
	var out bytes.Buffer
	out.Write(t.ensureStarted())
	items, _ := response["output"].([]any)
	for index, row := range items {
		source, ok := row.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := source["type"].(string)
		id, _ := source["id"].(string)
		switch kind {
		case "function_call":
			item := t.item(id, index, kind)
			item.kind = "function_call"
			item.callID = firstString(source["call_id"], source["id"], item.callID)
			if name, ok := source["name"].(string); ok && name != "" {
				item.name = name
			}
			out.Write(t.announceTool(item))
			if arguments, ok := responsesArgumentsText(source["arguments"]); ok {
				out.Write(t.fillArguments(item, arguments))
			}
		case "message":
			text := strings.Builder{}
			writeOutputText(&text, source["content"])
			out.Write(t.fillText(t.item(id, index, kind), text.String()))
		case "reasoning":
			out.Write(t.fillReasoning(t.item(id, index, kind), responsesSummaryText(source), responsesReasoningContent(source)))
		}
	}
	finish := "stop"
	switch {
	case t.hasToolCall:
		finish = "tool_calls"
	case incomplete:
		finish = "length"
		if details, ok := response["incomplete_details"].(map[string]any); ok {
			if reason, _ := details["reason"].(string); reason == "content_filter" {
				finish = "content_filter"
			}
		}
	}
	out.Write(t.finishChunk(finish))
	if usage, ok := response["usage"].(map[string]any); ok {
		if shaped := responsesChatUsage(usage); shaped != nil {
			payload := map[string]any{"object": "chat.completion.chunk", "choices": []any{}, "usage": shaped}
			if t.responseID != "" {
				payload["id"] = t.responseID
			}
			if t.model != "" {
				payload["model"] = t.model
			}
			out.Write([]byte("data: " + rawJSON(payload) + "\n\n"))
		}
	}
	out.Write([]byte("data: [DONE]\n\n"))
	t.closed = true
	return out.Bytes()
}

func (t *ResponsesChatStreamTranslator) failed(value map[string]any) []byte {
	response, _ := value["response"].(map[string]any)
	if t.responseID == "" {
		t.responseID, _ = response["id"].(string)
	}
	failure, _ := response["error"].(map[string]any)
	if failure == nil {
		failure, _ = value["error"].(map[string]any)
	}
	code, _ := failure["code"].(string)
	if code == "" {
		code, _ = failure["type"].(string)
	}
	message, _ := failure["message"].(string)
	return t.fail(firstString(code, "upstream_stream_failed"), message)
}

func (t *ResponsesChatStreamTranslator) eventError(value map[string]any) []byte {
	failure, _ := value["error"].(map[string]any)
	if failure == nil {
		failure = value
	}
	code, _ := failure["code"].(string)
	if code == "" {
		code, _ = failure["type"].(string)
	}
	message, _ := failure["message"].(string)
	return t.fail(firstString(code, "upstream_stream_failed"), message)
}

func (t *ResponsesChatStreamTranslator) fail(code, message string) []byte {
	if t.closed {
		return nil
	}
	t.closed = true
	t.outcome = OutcomeFailed
	payload := map[string]any{"object": "chat.completion.chunk", "choices": []any{},
		"error": map[string]any{"type": code, "message": message}}
	if t.responseID != "" {
		payload["id"] = t.responseID
	}
	if t.model != "" {
		payload["model"] = t.model
	}
	return []byte("data: " + rawJSON(payload) + "\n\n")
}

package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// StreamTranslator rewrites an Anthropic SSE stream into the event names Kimi's
// own paths consume: `data: {...}` lines plus `[DONE]` for Chat, or the
// `response.*` event sequence for Responses.
//
// The Anthropic wire format is never passed through. Kimi reads OpenAI deltas,
// so forwarding `content_block_delta` would leave the client with an empty reply
// that still reported 200. The translator is stateful only to the extent the
// upstream protocol is: the open block index, the accumulated text and the
// usage.
//
// The terminal state is the whole point. A stream that breaks, that ends
// without `message_stop`, or that carries a block this cannot represent is
// reported as failed or incomplete — never completed.
type StreamTranslator struct {
	route       string
	maxBuffered int
	buffer      string
	eventName   string
	eventData   []string
	partial     []byte
	messageID   string
	model       string
	usage       map[string]any
	closed      bool
	started     bool
	blocks      map[int]*streamBlock
	blockOrder  []int
	output      []*streamBlock
	toolCount   int
	finish      string
	outcome     Outcome
}

type streamBlock struct {
	source      map[string]any
	index       int
	id          string
	text        string
	arguments   string
	thinking    string
	signature   string
	done        bool
	toolIndex   int
	outputIndex int
	item        map[string]any
}

// NewStreamTranslator returns a translator for one reply.
func NewStreamTranslator(route string, maxBufferedBytes int) (*StreamTranslator, error) {
	if route != ProtocolChat && route != ProtocolResponses {
		return nil, ErrInvalidProtocol
	}
	if maxBufferedBytes < 1 {
		return nil, ErrInvalidProtocol
	}
	return &StreamTranslator{route: route, maxBuffered: maxBufferedBytes, blocks: map[int]*streamBlock{}}, nil
}

// Outcome reports the terminal state, or "" while the stream is still open.
func (t *StreamTranslator) Outcome() Outcome {
	if t == nil {
		return ""
	}
	return t.outcome
}

// Closed reports whether a terminal event has been emitted.
func (t *StreamTranslator) Closed() bool { return t != nil && t.closed }

// Push feeds one upstream chunk and returns the bytes to forward.
func (t *StreamTranslator) Push(chunk []byte) []byte {
	if t == nil || t.closed {
		return nil
	}
	text := string(t.decode(chunk))
	t.buffer += text
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

// takeSSELine removes one complete line from an SSE buffer. A trailing CR might
// be the first half of a CRLF that has not arrived yet; waiting for the next
// chunk keeps the line intact.
func takeSSELine(buffer *string) (string, bool) {
	value := *buffer
	index := strings.IndexAny(value, "\r\n")
	if index < 0 {
		return "", false
	}
	if value[index] == '\r' && index == len(value)-1 {
		return "", false
	}
	line := value[:index]
	width := 1
	if value[index] == '\r' && index+1 < len(value) && value[index+1] == '\n' {
		width = 2
	}
	*buffer = value[index+width:]
	return line, true
}

func (t *StreamTranslator) decode(chunk []byte) []byte {
	return decodeSSEChunk(&t.partial, chunk)
}

// decodeSSEChunk decodes a chunk while holding back an incomplete trailing rune,
// so a multi-byte character split across two chunks is decoded once, not twice.
func decodeSSEChunk(partial *[]byte, chunk []byte) []byte {
	if len(chunk) == 0 {
		return nil
	}
	buf := append(*partial, chunk...)
	*partial = nil
	if utf8.Valid(buf) {
		return buf
	}
	cut := len(buf)
	for i := 0; i < utf8.UTFMax && i < len(buf); i++ {
		start := len(buf) - 1 - i
		if utf8.RuneStart(buf[start]) {
			if !utf8.FullRune(buf[start:]) {
				cut = start
			}
			break
		}
	}
	*partial = append([]byte(nil), buf[cut:]...)
	return buf[:cut]
}

// End flushes whatever the stream left behind. A stream that never announced a
// terminal state is reported as incomplete, which is the honest answer: the
// answer may be cut off.
func (t *StreamTranslator) End() []byte {
	if t == nil || t.closed {
		return nil
	}
	tail := t.decode(nil)
	t.buffer += string(tail)
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

func (t *StreamTranslator) line(line string) []byte {
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

func (t *StreamTranslator) event(typ string, value map[string]any) []byte {
	if t.closed {
		return nil
	}
	switch typ {
	case "message_start":
		return t.start(value)
	case "content_block_start":
		return t.blockStart(value)
	case "content_block_delta":
		return t.delta(value)
	case "content_block_stop":
		return t.blockStop(value)
	case "message_delta":
		if delta, ok := value["delta"].(map[string]any); ok {
			if stop, ok := delta["stop_reason"].(string); ok && stop != "" {
				t.finish = stop
			}
		}
		if usage, ok := value["usage"].(map[string]any); ok {
			if t.usage == nil {
				t.usage = map[string]any{}
			}
			for key, entry := range usage {
				t.usage[key] = entry
			}
		}
		return nil
	case "message_stop":
		return t.complete()
	case "error":
		body := AnthropicErrorBody([]byte(rawJSON(value)), 200)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		inner, _ := parsed["error"].(map[string]any)
		code, _ := inner["type"].(string)
		message, _ := inner["message"].(string)
		return t.fail(code, message)
	default:
		return nil
	}
}

func rawJSON(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (t *StreamTranslator) start(value map[string]any) []byte {
	if t.started {
		return t.fail("invalid_upstream_event", "upstream_message_start_repeated")
	}
	t.started = true
	message, _ := value["message"].(map[string]any)
	t.messageID, _ = message["id"].(string)
	t.model, _ = message["model"].(string)
	if usage, ok := message["usage"].(map[string]any); ok {
		t.usage = map[string]any{}
		for key, entry := range usage {
			t.usage[key] = entry
		}
	}
	if t.route == ProtocolChat {
		return nil
	}
	return t.sse("response.created", map[string]any{"response": t.response("in_progress", nil)})
}

func (t *StreamTranslator) blockStart(value map[string]any) []byte {
	index := len(t.blocks)
	if number, ok := finiteOf(value["index"]); ok {
		index = int(number)
	}
	source, ok := value["content_block"].(map[string]any)
	if !ok {
		source = map[string]any{}
	}
	kind, _ := source["type"].(string)
	if kind != "text" && kind != "tool_use" && kind != "thinking" {
		return t.fail("unsupported_content_block", "upstream_content_block_unsupported")
	}
	base := t.messageID
	if base == "" {
		base = "anthropic"
	}
	id, _ := source["id"].(string)
	if id == "" {
		id = base + "_item_" + itoa(index)
	}
	block := &streamBlock{source: source, index: index, id: id}
	switch kind {
	case "tool_use":
		block.toolIndex = t.toolCount
		t.toolCount++
		block.outputIndex = len(t.output)
		if input, ok := source["input"].(map[string]any); ok && len(input) > 0 {
			block.arguments = rawJSON(input)
		}
		callID, _ := source["id"].(string)
		if callID == "" {
			callID = id
		}
		name, _ := source["name"].(string)
		block.item = map[string]any{"id": id, "type": "function_call", "call_id": callID,
			"name": name, "arguments": block.arguments, "status": "in_progress"}
		t.output = append(t.output, block)
	case "text":
		block.outputIndex = len(t.output)
		block.text, _ = source["text"].(string)
		block.item = map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress",
			"content": []any{map[string]any{"type": "output_text", "text": block.text}}}
		t.output = append(t.output, block)
	case "thinking":
		block.outputIndex = len(t.output)
		block.thinking, _ = source["thinking"].(string)
		block.signature, _ = source["signature"].(string)
		block.item = map[string]any{"id": id, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": block.thinking}}}
		t.output = append(t.output, block)
	}
	if _, exists := t.blocks[index]; !exists {
		t.blockOrder = append(t.blockOrder, index)
	}
	t.blocks[index] = block
	if t.route == ProtocolChat {
		if kind == "tool_use" {
			name, _ := source["name"].(string)
			return chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": block.toolIndex,
				"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": block.arguments}}}})
		}
		return nil
	}
	if block.item == nil {
		return nil
	}
	added := t.sse("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": block.item})
	if kind == "text" {
		return append(added, t.sse("response.content_part.added", map[string]any{"item_id": id,
			"output_index": block.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": block.text}})...)
	}
	return added
}

func (t *StreamTranslator) delta(value map[string]any) []byte {
	index := -1
	if number, ok := finiteOf(value["index"]); ok {
		index = int(number)
	}
	block, ok := t.blocks[index]
	if !ok || block.done {
		return t.fail("invalid_upstream_event", "upstream_content_block_unknown")
	}
	delta, ok := value["delta"].(map[string]any)
	if !ok {
		delta = map[string]any{}
	}
	kind, _ := delta["type"].(string)
	switch kind {
	case "signature_delta":
		if signature, ok := delta["signature"].(string); ok {
			block.signature += signature
			if block.source != nil {
				block.source["signature"] = block.signature
			}
		}
		return nil
	case "text_delta":
		text, ok := delta["text"].(string)
		if !ok || block.source["type"] != "text" {
			return nil
		}
		block.text += text
		if block.item != nil {
			block.item["content"] = []any{map[string]any{"type": "output_text", "text": block.text}}
		}
		if t.route == ProtocolChat {
			return chatDelta(map[string]any{"content": text})
		}
		return t.sse("response.output_text.delta", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "content_index": 0, "delta": text})
	case "input_json_delta":
		partial, ok := delta["partial_json"].(string)
		if !ok || block.source["type"] != "tool_use" {
			return nil
		}
		block.arguments += partial
		block.item["arguments"] = block.arguments
		if t.route == ProtocolChat {
			return chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": block.toolIndex,
				"function": map[string]any{"arguments": partial}}}})
		}
		return t.sse("response.function_call_arguments.delta", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "delta": partial})
	case "thinking_delta":
		thinking, ok := delta["thinking"].(string)
		if !ok || block.source["type"] != "thinking" {
			return nil
		}
		block.thinking += thinking
		if block.item != nil {
			block.item["summary"] = []any{map[string]any{"type": "summary_text", "text": block.thinking}}
		}
		if t.route == ProtocolChat {
			return chatDelta(map[string]any{"reasoning_content": thinking})
		}
		return t.sse("response.reasoning_summary_text.delta", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "summary_index": 0, "delta": thinking})
	}
	return nil
}

func (t *StreamTranslator) sources() []map[string]any {
	out := make([]map[string]any, 0, len(t.blockOrder))
	for _, index := range t.blockOrder {
		if block, ok := t.blocks[index]; ok {
			out = append(out, block.source)
		}
	}
	return out
}

func (t *StreamTranslator) blockStop(value map[string]any) []byte {
	index := -1
	if number, ok := finiteOf(value["index"]); ok {
		index = int(number)
	}
	block, ok := t.blocks[index]
	if !ok || block.done {
		return t.fail("invalid_upstream_event", "upstream_content_block_unknown")
	}
	sources := t.sources()
	signed := false
	for _, entry := range t.blocks {
		if kind, _ := entry.source["type"].(string); kind == "thinking" && entry.signature != "" {
			signed = true
			break
		}
	}
	if signed {
		if err := assertRoundtripSupported(sources); err != nil {
			return t.fail("unsupported_thinking_tool_roundtrip", ErrThinkingToolRoundtrip.Error())
		}
	}
	if t.route == ProtocolResponses && signed {
		for _, other := range t.blocks {
			if kind, _ := other.source["type"].(string); kind == "tool_use" && !other.done {
				return t.fail("unsupported_thinking_tool_roundtrip", ErrThinkingToolRoundtrip.Error())
			}
		}
	}
	block.done = true
	var chatOut []byte
	if kind, _ := block.source["type"].(string); kind == "tool_use" {
		if block.arguments == "" {
			block.arguments = "{}"
			if block.item != nil {
				block.item["arguments"] = "{}"
			}
			if t.route == ProtocolChat {
				chatOut = chatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": block.toolIndex,
					"function": map[string]any{"arguments": "{}"}}}})
			}
		}
		var parsed any
		if json.Unmarshal([]byte(block.arguments), &parsed) != nil {
			return t.fail("invalid_tool_arguments", "upstream_tool_json_incomplete")
		}
	}
	if t.route == ProtocolChat || block.item == nil {
		return chatOut
	}
	block.item["status"] = "completed"
	var out bytes.Buffer
	switch block.source["type"] {
	case "text":
		out.Write(t.sse("response.output_text.done", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "content_index": 0, "text": block.text}))
		out.Write(t.sse("response.content_part.done", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": block.text}}))
	case "tool_use":
		out.Write(t.sse("response.function_call_arguments.done", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "arguments": block.arguments}))
	case "thinking":
		out.Write(t.sse("response.reasoning_summary_text.done", map[string]any{"item_id": block.id,
			"output_index": block.outputIndex, "summary_index": 0, "text": block.thinking}))
	}
	out.Write(t.sse("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": block.item}))
	return out.Bytes()
}

func (t *StreamTranslator) complete() []byte {
	if t.closed {
		return nil
	}
	if !t.started {
		return t.fail("incomplete_stream", "upstream_message_start_missing")
	}
	finish, known := chatFinish[t.finish]
	if !known {
		return t.fail("incomplete_stream", "upstream_stop_reason_missing")
	}
	for _, block := range t.output {
		if !block.done {
			return t.fail("incomplete_stream", "upstream_content_block_incomplete")
		}
	}
	if err := assertRoundtripSupported(t.sources()); err != nil {
		return t.fail("unsupported_thinking_tool_roundtrip", ErrThinkingToolRoundtrip.Error())
	}
	t.closed = true
	if t.finish == "max_tokens" {
		t.outcome = OutcomeIncomplete
	} else {
		t.outcome = OutcomeCompleted
	}
	if t.route == ProtocolChat {
		payload := map[string]any{"id": t.messageID, "object": "chat.completion.chunk", "model": t.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}}
		if usage := chatUsage(t.usage); usage != nil {
			payload["usage"] = usage
		}
		return []byte("data: " + rawJSON(payload) + "\n\ndata: [DONE]\n\n")
	}
	status := "completed"
	event := "response.completed"
	if t.outcome == OutcomeIncomplete {
		status = "incomplete"
		event = "response.incomplete"
	}
	return t.sse(event, map[string]any{"response": t.response(status, nil)})
}

func (t *StreamTranslator) response(status string, failure map[string]any) map[string]any {
	output := make([]any, 0, len(t.output))
	for _, block := range t.output {
		output = append(output, block.item)
	}
	out := map[string]any{"id": t.messageID, "object": "response", "model": t.model, "status": status, "output": output}
	if failure != nil {
		out["error"] = failure
	}
	if t.finish == "max_tokens" {
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage := responsesUsage(t.usage); usage != nil {
		out["usage"] = usage
	}
	return out
}

// fail emits the one terminal event this stream is allowed, and closes it.
func (t *StreamTranslator) fail(code, message string) []byte {
	if t.closed {
		return nil
	}
	t.closed = true
	t.outcome = OutcomeFailed
	if t.route == ProtocolChat {
		payload := map[string]any{"id": t.messageID, "object": "chat.completion.chunk", "model": t.model,
			"choices": []any{}, "error": map[string]any{"type": code, "message": message}}
		return []byte("data: " + rawJSON(payload) + "\n\n")
	}
	return t.sse("response.failed", map[string]any{"response": t.response("failed",
		map[string]any{"code": code, "message": message})})
}

func (t *StreamTranslator) sse(event string, fields map[string]any) []byte {
	payload := map[string]any{"type": event}
	for key, value := range fields {
		payload[key] = value
	}
	return []byte("event: " + event + "\ndata: " + rawJSON(payload) + "\n\n")
}

func chatDelta(delta map[string]any) []byte {
	payload := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
	return []byte("data: " + rawJSON(payload) + "\n\n")
}

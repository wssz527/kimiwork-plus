package protocol_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func event(name string, fields string) string {
	return "event: " + name + "\ndata: {\"type\":\"" + name + "\"" + fields + "}\n\n"
}

const (
	anthropicStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_golden\"," +
		"\"model\":\"claude-golden\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":90," +
		"\"cache_creation_input_tokens\":20}}}\n\n"
	textStart = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0," +
		"\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	textDelta = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0," +
		"\"delta\":{\"type\":\"text_delta\",\"text\":\"你好🙂\"}}\n\n"
	textStop     = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	messageDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}," +
		"\"usage\":{\"output_tokens\":5}}\n\n"
	messageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func sseEvents(output string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) == nil {
			out = append(out, payload)
		}
	}
	return out
}

func typesOf(events []map[string]any) []string {
	out := []string{}
	for _, event := range events {
		if value, ok := event["type"].(string); ok {
			out = append(out, value)
		}
	}
	return out
}

func containsType(events []map[string]any, want string) bool {
	for _, value := range typesOf(events) {
		if value == want {
			return true
		}
	}
	return false
}

// TestGoldenAnthropicTextSurvivesEveryChunkBoundary feeds the same stream at
// every split width, which is what a real socket does.
func TestGoldenAnthropicTextSurvivesEveryChunkBoundary(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		wire := anthropicStart + textStart + textDelta + textStop + messageDelta + messageStop
		wire = strings.ReplaceAll(wire, "\n", newline)
		for width := 1; width <= 7; width++ {
			for _, route := range []string{protocol.ProtocolChat, protocol.ProtocolResponses} {
				translator, err := protocol.NewStreamTranslator(route, 1<<20)
				if err != nil {
					t.Fatalf("translator: %v", err)
				}
				var out strings.Builder
				for at := 0; at < len(wire); at += width {
					end := at + width
					if end > len(wire) {
						end = len(wire)
					}
					out.Write(translator.Push([]byte(wire[at:end])))
				}
				out.Write(translator.End())
				text := out.String()
				if !strings.Contains(text, "你好🙂") {
					t.Fatalf("newline %q width %d %s: text lost:\n%s", newline, width, route, text)
				}
				if translator.Outcome() != protocol.OutcomeCompleted {
					t.Fatalf("newline %q width %d %s: outcome %q", newline, width, route, translator.Outcome())
				}
				events := sseEvents(text)
				if route == protocol.ProtocolChat {
					if !strings.Contains(text, "data: [DONE]") {
						t.Fatalf("chat stream has no [DONE]")
					}
					last := events[len(events)-1]
					usage, _ := last["usage"].(map[string]any)
					if usage["prompt_tokens"] != float64(120) || usage["completion_tokens"] != float64(5) ||
						usage["total_tokens"] != float64(125) || usage["cache_write_tokens"] != float64(20) {
						t.Fatalf("chat usage %v", usage)
					}
					continue
				}
				completed := 0
				var final map[string]any
				for _, event := range events {
					if event["type"] == "response.completed" {
						completed++
						final, _ = event["response"].(map[string]any)
					}
				}
				if completed != 1 || final == nil {
					t.Fatalf("responses terminal count %d", completed)
				}
				if final["status"] != "completed" {
					t.Fatalf("final status %v", final["status"])
				}
				output, _ := final["output"].([]any)
				if len(output) != 1 {
					t.Fatalf("output %v", output)
				}
				item, _ := output[0].(map[string]any)
				if item["id"] != "msg_golden_item_0" {
					t.Fatalf("item id %v", item["id"])
				}
				usage, _ := final["usage"].(map[string]any)
				if usage["input_tokens"] != float64(120) || usage["cache_write_tokens"] != float64(20) {
					t.Fatalf("responses usage %v", usage)
				}
			}
		}
	}
}

// TestStreamErrorIsNeverSynthesizedAsSuccess is the single most important
// property: a broken stream must not end in a completion.
func TestStreamErrorIsNeverSynthesizedAsSuccess(t *testing.T) {
	errorEvent := event("error", `,"error":{"type":"overloaded_error","message":"synthetic overload"}`)
	for _, route := range []string{protocol.ProtocolChat, protocol.ProtocolResponses} {
		for _, prefix := range []string{anthropicStart, anthropicStart + textStart + textDelta} {
			translator, _ := protocol.NewStreamTranslator(route, 1<<20)
			out := string(translator.Push([]byte(prefix + errorEvent)))
			out += string(translator.End())
			if !strings.Contains(out, "overloaded_error") || !strings.Contains(out, "synthetic overload") {
				t.Fatalf("%s: error lost:\n%s", route, out)
			}
			if strings.Contains(out, "response.completed") || strings.Contains(out, `"finish_reason":"stop"`) ||
				strings.Contains(out, "[DONE]") {
				t.Fatalf("%s: a failure was reported as a completion:\n%s", route, out)
			}
			if translator.Outcome() != protocol.OutcomeFailed {
				t.Fatalf("%s: outcome %q", route, translator.Outcome())
			}
			if tail := translator.End(); tail != nil {
				t.Fatalf("%s: a closed stream emitted more: %q", route, tail)
			}
		}
	}
}

func TestEOFWithoutMessageStopIsIncomplete(t *testing.T) {
	partialTool := anthropicStart +
		event("content_block_start", `,"index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"read"}`) +
		event("content_block_delta", `,"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}`)
	for _, wire := range []string{anthropicStart + textStart + textDelta, partialTool} {
		translator, _ := protocol.NewStreamTranslator(protocol.ProtocolResponses, 1<<20)
		out := string(translator.Push([]byte(wire))) + string(translator.End())
		if !strings.Contains(out, "response.failed") {
			t.Fatalf("no failure terminal:\n%s", out)
		}
		if strings.Contains(out, "response.completed") {
			t.Fatalf("a truncated stream completed:\n%s", out)
		}
		if translator.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("outcome %q", translator.Outcome())
		}
	}
}

func TestToolCallDeltasRetainIndexesIDsAndArguments(t *testing.T) {
	wire := anthropicStart
	for index := 0; index < 2; index++ {
		wire += event("content_block_start", `,"index":`+itoa(index)+`,"content_block":{"type":"tool_use","id":"tool_`+
			itoa(index)+`","name":"read_`+itoa(index)+`"}`)
		wire += event("content_block_delta", `,"index":`+itoa(index)+`,"delta":{"type":"input_json_delta","partial_json":"{\"index\":`+
			itoa(index)+`}"}`)
		wire += event("content_block_stop", `,"index":`+itoa(index))
	}
	wire += event("message_delta", `,"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}`) + messageStop

	chat, _ := protocol.NewStreamTranslator(protocol.ProtocolChat, 1<<20)
	chatOut := string(chat.Push([]byte(wire))) + string(chat.End())
	// Each call carries its own argument stream, keyed by the numeric index the
	// client uses to tell two concurrent calls apart.
	byIndex := map[float64]*strings.Builder{}
	for _, event := range sseEvents(chatOut) {
		choices, _ := event["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, call := range calls {
			entry, _ := call.(map[string]any)
			index, _ := entry["index"].(float64)
			if byIndex[index] == nil {
				byIndex[index] = &strings.Builder{}
			}
			fn, _ := entry["function"].(map[string]any)
			if text, ok := fn["arguments"].(string); ok {
				byIndex[index].WriteString(text)
			}
		}
	}
	if len(byIndex) != 2 {
		t.Fatalf("chat tool indexes %v", byIndex)
	}
	for index, builder := range byIndex {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(builder.String()), &parsed); err != nil {
			t.Fatalf("call %v arguments are not one object: %q", index, builder.String())
		}
		if parsed["index"] != index {
			t.Fatalf("call %v carried arguments %v", index, parsed)
		}
	}
	if chat.Outcome() != protocol.OutcomeCompleted {
		t.Fatalf("chat outcome %q", chat.Outcome())
	}

	responses, _ := protocol.NewStreamTranslator(protocol.ProtocolResponses, 1<<20)
	out := string(responses.Push([]byte(wire))) + string(responses.End())
	events := sseEvents(out)
	var final map[string]any
	for _, event := range events {
		if event["type"] == "response.completed" {
			final, _ = event["response"].(map[string]any)
		}
	}
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output %v", output)
	}
	ids := []string{}
	for _, row := range output {
		item, _ := row.(map[string]any)
		ids = append(ids, item["id"].(string))
	}
	if ids[0] != "tool_0" || ids[1] != "tool_1" {
		t.Fatalf("ids %v", ids)
	}
	// The done event must carry the same item the final response does.
	done := 0
	for _, event := range events {
		if event["type"] != "response.output_item.done" {
			continue
		}
		done++
	}
	if done != 2 {
		t.Fatalf("output_item.done count %d", done)
	}
}

func TestEmptyToolInputIsNotPrependedToStreamedArguments(t *testing.T) {
	wire := anthropicStart +
		event("content_block_start", `,"index":0,"content_block":{"type":"tool_use","id":"tool_weather","name":"weather","input":{}}`) +
		event("content_block_delta", `,"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}`) +
		event("content_block_delta", `,"index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}`) +
		event("content_block_stop", `,"index":0`) +
		event("message_delta", `,"delta":{"stop_reason":"tool_use"}`) + messageStop
	for _, route := range []string{protocol.ProtocolChat, protocol.ProtocolResponses} {
		translator, _ := protocol.NewStreamTranslator(route, 1<<20)
		out := string(translator.Push([]byte(wire))) + string(translator.End())
		if translator.Outcome() != protocol.OutcomeCompleted {
			t.Fatalf("%s outcome %q:\n%s", route, translator.Outcome(), out)
		}
		if route == protocol.ProtocolChat {
			arguments := strings.Builder{}
			for _, event := range sseEvents(out) {
				choices, _ := event["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				choice, _ := choices[0].(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				calls, _ := delta["tool_calls"].([]any)
				for _, call := range calls {
					entry, _ := call.(map[string]any)
					fn, _ := entry["function"].(map[string]any)
					if text, ok := fn["arguments"].(string); ok {
						arguments.WriteString(text)
					}
				}
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(arguments.String()), &parsed); err != nil {
				t.Fatalf("arguments %q", arguments.String())
			}
			if parsed["city"] != "Paris" {
				t.Fatalf("arguments %v", parsed)
			}
			continue
		}
		if !strings.Contains(out, `"arguments":"{\"city\":\"Paris\"}"`) {
			t.Fatalf("responses arguments lost:\n%s", out)
		}
	}
}

func TestSignedThinkingWithToolsIsRejected(t *testing.T) {
	wire := anthropicStart +
		event("content_block_start", `,"index":0,"content_block":{"type":"thinking","thinking":"","signature":"synthetic-signature"}`) +
		event("content_block_delta", `,"index":0,"delta":{"type":"thinking_delta","thinking":"synthetic"}`) +
		event("content_block_stop", `,"index":0`) +
		event("content_block_start", `,"index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"read"}`) +
		event("content_block_delta", `,"index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}`) +
		event("content_block_stop", `,"index":1`) +
		event("message_delta", `,"delta":{"stop_reason":"tool_use"}`) + messageStop
	translator, _ := protocol.NewStreamTranslator(protocol.ProtocolResponses, 1<<20)
	out := string(translator.Push([]byte(wire))) + string(translator.End())
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, "unsupported_thinking_tool_roundtrip") {
		t.Fatalf("signed thinking plus tools was not refused:\n%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("refused stream completed:\n%s", out)
	}
	// The request direction must refuse it too.
	_, err := protocol.ToAnthropicRequest(protocol.ProtocolChat, map[string]any{"model": "m",
		"messages": []any{map[string]any{"role": "assistant",
			"content":    []any{map[string]any{"type": "thinking", "thinking": "synthetic", "signature": "sig"}},
			"tool_calls": []any{map[string]any{"id": "tool_1", "function": map[string]any{"name": "read", "arguments": "{}"}}}}}})
	if err != protocol.ErrThinkingToolRoundtrip {
		t.Fatalf("request direction accepted a signed thinking round trip: %v", err)
	}
}

func TestUnknownContentBlockCannotComplete(t *testing.T) {
	for _, block := range []string{
		`{"type":"redacted_thinking","data":"synthetic-redacted-signature"}`,
		`{"type":"server_tool_use","id":"synthetic-tool"}`,
	} {
		wire := anthropicStart + event("content_block_start", `,"index":0,"content_block":`+block) +
			event("content_block_stop", `,"index":0`) + messageDelta + messageStop
		translator, _ := protocol.NewStreamTranslator(protocol.ProtocolResponses, 1<<20)
		out := string(translator.Push([]byte(wire))) + string(translator.End())
		if !strings.Contains(out, "unsupported_content_block") {
			t.Fatalf("block %s: %s", block, out)
		}
		if strings.Contains(out, "response.completed") {
			t.Fatalf("block %s completed", block)
		}
	}
}

func TestOversizedEventIsBoundedAndFailsExplicitly(t *testing.T) {
	for _, route := range []string{protocol.ProtocolChat, protocol.ProtocolResponses} {
		translator, _ := protocol.NewStreamTranslator(route, 64)
		out := string(translator.Push([]byte("data: " + strings.Repeat("synthetic", 20))))
		out += string(translator.End())
		if !strings.Contains(out, "upstream_event_too_large") {
			t.Fatalf("%s: %s", route, out)
		}
		if translator.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("%s outcome %q", route, translator.Outcome())
		}
		empty, _ := protocol.NewStreamTranslator(route, 64)
		if terminal := string(empty.End()); !strings.Contains(terminal, "incomplete_stream") {
			t.Fatalf("%s empty stream: %s", route, terminal)
		}
	}
}

func TestMaxTokensYieldsIncompleteNotCompleted(t *testing.T) {
	wire := anthropicStart + textStart + textDelta + textStop +
		event("message_delta", `,"delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}`) + messageStop
	translator, _ := protocol.NewStreamTranslator(protocol.ProtocolResponses, 1<<20)
	out := string(translator.Push([]byte(wire))) + string(translator.End())
	if strings.Contains(out, "response.completed") {
		t.Fatalf("a truncated reply completed:\n%s", out)
	}
	if !strings.Contains(out, "response.incomplete") {
		t.Fatalf("no incomplete terminal:\n%s", out)
	}
	if translator.Outcome() != protocol.OutcomeIncomplete {
		t.Fatalf("outcome %q", translator.Outcome())
	}
}

// --- the backend path ------------------------------------------------------

func TestAnthropicModelIsDispatchedToMessagesWithVendorAuth(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-probe",
			"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":20,"output_tokens":7,"cache_read_input_tokens":4}}`))
	})
	upstreamProtocol := protocol.ProtocolAnthropic
	model := chatModel("kimi-claude-probe")
	model.UpstreamProtocol = &upstreamProtocol
	plain := chatModel("kimi-openai-probe")
	_, url := startGateway(t, fake.server.URL, []protocol.Model{model, plain}, nil)

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-claude-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	var shaped map[string]any
	if err := json.Unmarshal([]byte(body), &shaped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices, _ := shaped["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("content %v", message["content"])
	}
	requests := fake.captured()
	if requests[0].URL != "/messages" {
		t.Fatalf("upstream path %q", requests[0].URL)
	}
	if requests[0].Headers.Get("X-Api-Key") != "vendor-key" {
		t.Fatalf("x-api-key %q", requests[0].Headers.Get("X-Api-Key"))
	}
	if requests[0].Headers.Get("Anthropic-Version") != protocol.AnthropicVersion {
		t.Fatalf("anthropic-version %q", requests[0].Headers.Get("Anthropic-Version"))
	}
	if requests[0].Headers.Get("Authorization") != "" {
		t.Fatalf("a Bearer header was sent to Anthropic: %v", requests[0].Headers)
	}

	// The same gateway still speaks OpenAI for a model that did not opt in.
	post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-openai-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	requests = fake.captured()
	if requests[1].URL != "/chat/completions" {
		t.Fatalf("second upstream path %q", requests[1].URL)
	}
	if requests[1].Headers.Get("Authorization") != "Bearer vendor-key" {
		t.Fatalf("second auth %q", requests[1].Headers.Get("Authorization"))
	}
	if requests[1].Headers.Get("Anthropic-Version") != "" {
		t.Fatalf("Anthropic header leaked to an OpenAI upstream")
	}
}

func TestAnthropicRefusalKeepsStatusAndVendorType(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"no access to this model"}}`))
	})
	upstreamProtocol := protocol.ProtocolAnthropic
	model := chatModel("kimi-claude-probe")
	model.UpstreamProtocol = &upstreamProtocol
	_, url := startGateway(t, fake.server.URL, []protocol.Model{model}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-claude-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != 403 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	var shaped map[string]any
	_ = json.Unmarshal([]byte(body), &shaped)
	inner, _ := shaped["error"].(map[string]any)
	if inner["type"] != "permission_error" || inner["message"] != "no access to this model" {
		t.Fatalf("error envelope %v", shaped)
	}
}

func TestAnthropicStreamReachesTheClientAsOpenAIDeltas(t *testing.T) {
	wire := anthropicStart + textStart + textDelta + textStop + messageDelta + messageStop
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(wire))
	})
	upstreamProtocol := protocol.ProtocolAnthropic
	model := chatModel("kimi-claude-probe")
	model.UpstreamProtocol = &upstreamProtocol
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{model}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-claude-probe", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d", reply.StatusCode)
	}
	if !strings.Contains(body, `"content":"你好🙂"`) {
		t.Fatalf("translated text lost:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no [DONE]:\n%s", body)
	}
	if strings.Contains(body, "content_block_delta") || strings.Contains(body, "message_start") {
		t.Fatalf("Anthropic event names reached the client:\n%s", body)
	}
	record := gateway.Metrics().Recent[0]
	if record.UpstreamProtocol == nil || *record.UpstreamProtocol != "anthropic" {
		t.Fatalf("upstream protocol not recorded: %+v", record.UpstreamProtocol)
	}
	if record.Usage == nil || *record.Usage.InputTokens != 120 || *record.Usage.CachedInputTokens != 90 {
		t.Fatalf("usage %+v", record.Usage)
	}
}

func TestAnthropicUnparseableReplyIsAFailedRequest(t *testing.T) {
	fake := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	})
	upstreamProtocol := protocol.ProtocolAnthropic
	model := chatModel("kimi-claude-probe")
	model.UpstreamProtocol = &upstreamProtocol
	_, url := startGateway(t, fake.server.URL, []protocol.Model{model}, nil)
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-claude-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != 503 || errorCode(t, body) != "upstream_invalid_response" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
}

// --- request conversion ----------------------------------------------------

func TestChatRequestBecomesAMessagesBody(t *testing.T) {
	body, err := protocol.ToAnthropicRequest(protocol.ProtocolChat, map[string]any{
		"model": "claude-probe", "max_tokens": float64(512), "temperature": 0.25, "stop": "END",
		"reasoning_effort": "xhigh",
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.jpg"}},
			}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": `{"temperature":20}`},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "weather", "description": "lookup", "parameters": map[string]any{"type": "object"}}}},
		"tool_choice": "auto",
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if body["system"] != "be terse" || body["max_tokens"] != int64(512) {
		t.Fatalf("body %v", body)
	}
	outputConfig, _ := body["output_config"].(map[string]any)
	if outputConfig["effort"] != "xhigh" {
		t.Fatalf("effort %v", outputConfig)
	}
	stop, _ := body["stop_sequences"].([]string)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop %v", body["stop_sequences"])
	}
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages %v", messages)
	}
	roles := []string{}
	for _, message := range messages {
		roles = append(roles, message["role"].(string))
	}
	if roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Fatalf("roles %v", roles)
	}
	first, _ := messages[0]["content"].([]map[string]any)
	if first[1]["type"] != "image" {
		t.Fatalf("image block %v", first[1])
	}
	source, _ := first[1]["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("image source %v", source)
	}
	second, _ := messages[1]["content"].([]map[string]any)
	input, _ := second[0]["input"].(map[string]any)
	if second[0]["type"] != "tool_use" || input["city"] != "Paris" {
		t.Fatalf("tool_use %v", second[0])
	}
	third, _ := messages[2]["content"].([]map[string]any)
	if third[0]["type"] != "tool_result" || third[0]["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result %v", third[0])
	}
	tools, _ := body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "weather" || tools[0]["description"] != "lookup" {
		t.Fatalf("tools %v", tools)
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "auto" {
		t.Fatalf("tool_choice %v", choice)
	}
}

func TestResponsesRequestMergesToolResultsWithFollowingUserText(t *testing.T) {
	body, err := protocol.ToAnthropicRequest(protocol.ProtocolResponses, map[string]any{
		"model": "claude-probe", "instructions": "stay in scope", "max_output_tokens": float64(256),
		"stream": true, "reasoning": map[string]any{"effort": "low"},
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "look it up"}}},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "lookup", "arguments": `{"id":7}`},
			map[string]any{"type": "function_call_output", "call_id": "call_2", "output": `{"value":"ok"}`},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "thanks"}}},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if body["system"] != "stay in scope" || body["stream"] != true {
		t.Fatalf("body %v", body)
	}
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages %v", messages)
	}
	// Splitting the result from the text that follows it is the shape the
	// upstream rejects.
	last, _ := messages[2]["content"].([]map[string]any)
	if len(last) != 2 || last[0]["type"] != "tool_result" || last[1]["type"] != "text" {
		t.Fatalf("merged turn %v", last)
	}
}

func TestReasoningControlsRejectAnImpossibleBudget(t *testing.T) {
	if _, err := protocol.ReasoningControlsFor("high", int64Ptr(500), int64Ptr(8192), false); err != protocol.ErrInvalidThinkingBudget {
		t.Fatalf("small budget accepted: %v", err)
	}
	controls, err := protocol.ReasoningControlsFor("off", nil, nil, false)
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	thinking, _ := controls.Thinking["type"]
	if thinking != "disabled" {
		t.Fatalf("off did not disable thinking: %v", controls)
	}
	controls, err = protocol.ReasoningControlsFor("high", int64Ptr(999999), int64Ptr(4096), false)
	if err != nil {
		t.Fatalf("clamp: %v", err)
	}
	if controls.Thinking["budget_tokens"] != int64(4095) {
		t.Fatalf("budget not clamped: %v", controls.Thinking)
	}
	if controls, _ = protocol.ReasoningControlsFor("ultra", nil, nil, false); controls.Effort != "" {
		t.Fatalf("an unknown level became a real one: %v", controls)
	}
}

func int64Ptr(value int64) *int64 { return &value }

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

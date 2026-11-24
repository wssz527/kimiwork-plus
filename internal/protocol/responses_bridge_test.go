package protocol_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

// --- request conversion ----------------------------------------------------

func responsesRow(id string, extra ...func(*protocol.Model)) protocol.Model {
	model := protocol.Model{ID: id, Protocol: protocol.ProtocolResponses, ContextWindow: 1000}
	for _, apply := range extra {
		apply(&model)
	}
	return model
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body JSON: %v\n%s", err, body)
	}
	return decoded
}

func objectAt(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	current, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", strings.Join(path, "."), value)
	}
	for _, key := range path {
		if key == "" {
			continue
		}
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object: %v", strings.Join(path, "."), current[key])
		}
		current = next
	}
	return current
}

func listAt(t *testing.T, value any, path ...string) []any {
	t.Helper()
	current := value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", strings.Join(path, "."))
		}
		current = object[key]
	}
	switch rows := current.(type) {
	case []any:
		return rows
	case []map[string]any:
		converted := make([]any, len(rows))
		for i, row := range rows {
			converted[i] = row
		}
		return converted
	default:
		t.Fatalf("%s is not a list: %v", strings.Join(path, "."), current)
		return nil
	}
}

func TestToResponsesFromChatMapsMessagesAndTools(t *testing.T) {
	reasoning := true
	efforts := []string{"low", "medium", "high"}
	model := responsesRow("vendor/gpt", func(m *protocol.Model) {
		m.Reasoning = &reasoning
		m.ReasoningEfforts = &efforts
	})
	payload := map[string]any{
		"model":        "vendor/gpt",
		"instructions": "base rules",
		"messages": []any{
			map[string]any{"role": "system", "content": "first rules"},
			map[string]any{"role": "developer", "content": []any{
				map[string]any{"type": "text", "text": "second rules"}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "https://example.com/a.png", "detail": "high"}},
				map[string]any{"type": "image_url", "image_url": "https://example.com/b.png"},
			}},
			map[string]any{"role": "assistant", "content": "done",
				"reasoning_content": "secret thinking", "reasoning_details": []any{}, "thinking": "hidden",
				"tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":1}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": []any{
				map[string]any{"type": "text", "text": "result"}}},
			map[string]any{"role": "system", "content": "late rule"},
			map[string]any{"role": "user", "content": "next"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "description": "d", "parameters": map[string]any{"type": "object"}, "strict": true}}},
		"tool_choice":           map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
		"max_completion_tokens": float64(64),
		"temperature":           float64(0.5),
		"top_p":                 float64(0.9),
		"stream":                true,
		"prompt_cache_key":      "cache-1",
		"parallel_tool_calls":   false,
		"reasoning_effort":      "high",
	}
	body, err := protocol.ToResponsesFromChat(payload, &model)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "secret thinking") || strings.Contains(string(encoded), "hidden") {
		t.Fatalf("history reasoning leaked upstream: %s", encoded)
	}
	if got := body["instructions"]; got != "base rules\n\nfirst rules\n\nsecond rules" {
		t.Fatalf("instructions = %q", got)
	}
	input := listAt(t, body, "input")
	if len(input) != 6 {
		t.Fatalf("input items = %d: %v", len(input), input)
	}
	user := objectAt(t, input[0], "")
	if user["role"] != "user" || user["type"] != "message" {
		t.Fatalf("user item = %v", user)
	}
	parts := listAt(t, user, "content")
	if len(parts) != 3 {
		t.Fatalf("user parts = %v", parts)
	}
	first := objectAt(t, parts[0], "")
	if first["type"] != "input_text" || first["text"] != "look" {
		t.Fatalf("text part = %v", first)
	}
	image := objectAt(t, parts[1], "")
	if image["type"] != "input_image" || image["image_url"] != "https://example.com/a.png" || image["detail"] != "high" {
		t.Fatalf("image part = %v", image)
	}
	plainImage := objectAt(t, parts[2], "")
	if plainImage["image_url"] != "https://example.com/b.png" {
		t.Fatalf("plain image part = %v", plainImage)
	}
	if _, hasDetail := plainImage["detail"]; hasDetail {
		t.Fatalf("empty detail was forwarded: %v", plainImage)
	}
	assistant := objectAt(t, input[1], "")
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant item = %v", assistant)
	}
	outputText := objectAt(t, listAt(t, assistant, "content")[0], "")
	if outputText["type"] != "output_text" || outputText["text"] != "done" {
		t.Fatalf("assistant content = %v", outputText)
	}
	call := objectAt(t, input[2], "")
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" ||
		call["arguments"] != `{"q":1}` {
		t.Fatalf("function_call item = %v", call)
	}
	output := objectAt(t, input[3], "")
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != "result" {
		t.Fatalf("function_call_output item = %v", output)
	}
	late := objectAt(t, input[4], "")
	if late["type"] != "message" || late["role"] != "developer" {
		t.Fatalf("mid-conversation system item = %v", late)
	}
	if objectAt(t, listAt(t, late, "content")[0], "")["type"] != "input_text" {
		t.Fatalf("mid-conversation part = %v", late)
	}
	if objectAt(t, input[5], "")["role"] != "user" {
		t.Fatalf("trailing user item = %v", input[5])
	}
	tool := objectAt(t, listAt(t, body, "tools")[0], "")
	if tool["type"] != "function" || tool["name"] != "lookup" || tool["description"] != "d" || tool["strict"] != true {
		t.Fatalf("flattened tool = %v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Fatalf("tool kept the Chat nesting: %v", tool)
	}
	choice := objectAt(t, body, "tool_choice")
	if choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %v", choice)
	}
	if body["max_output_tokens"] != int64(64) || body["temperature"] != float64(0.5) ||
		body["top_p"] != float64(0.9) || body["stream"] != true || body["prompt_cache_key"] != "cache-1" ||
		body["parallel_tool_calls"] != false {
		t.Fatalf("sampling fields = %v", body)
	}
	if _, ok := body["messages"]; ok {
		t.Fatalf("Chat messages survived conversion: %v", body["messages"])
	}
	reasoningBody := objectAt(t, body, "reasoning")
	if reasoningBody["effort"] != "high" || reasoningBody["summary"] != "auto" {
		t.Fatalf("reasoning = %v", reasoningBody)
	}
}

func TestToResponsesFromChatStringChoiceAndTokenFallback(t *testing.T) {
	payload := map[string]any{
		"model":       "vendor/gpt",
		"max_tokens":  float64(32),
		"tool_choice": "required",
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1",
				"function": map[string]any{"name": "lookup"}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "raw"},
		},
	}
	body, err := protocol.ToResponsesFromChat(payload, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if body["max_output_tokens"] != int64(32) {
		t.Fatalf("max_output_tokens = %v", body["max_output_tokens"])
	}
	if body["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}
	if _, ok := body["instructions"]; ok {
		t.Fatalf("empty instructions were sent: %v", body["instructions"])
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning sent for a model without it: %v", body["reasoning"])
	}
	tool := objectAt(t, listAt(t, body, "tools")[0], "")
	if objectAt(t, tool, "parameters")["type"] != "object" {
		t.Fatalf("missing parameters were not defaulted: %v", tool)
	}
	input := listAt(t, body, "input")
	if objectAt(t, input[1], "")["arguments"] != "{}" {
		t.Fatalf("empty tool arguments = %v", input[1])
	}
	if objectAt(t, input[2], "")["output"] != "raw" {
		t.Fatalf("string tool output = %v", input[2])
	}
}

func TestToResponsesFromChatReasoningControls(t *testing.T) {
	enabled := true
	disabled := false
	effortsWithNone := []string{"none", "low", "medium", "high"}
	efforts := []string{"low", "medium", "high"}
	low := "low"
	cases := []struct {
		name    string
		model   *protocol.Model
		payload map[string]any
		want    map[string]any
	}{
		{
			name:    "off without none support sends nothing",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &efforts},
			payload: map[string]any{"reasoning_effort": "off"},
			want:    nil,
		},
		{
			name:    "off with none support asks for none",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &effortsWithNone},
			payload: map[string]any{"reasoning_effort": "none"},
			want:    map[string]any{"effort": "none"},
		},
		{
			name:    "disabled thinking without none support sends nothing",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &efforts},
			payload: map[string]any{"thinking": map[string]any{"type": "disabled"}},
			want:    nil,
		},
		{
			name:    "model reasoning without a level uses the default effort",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &efforts, DefaultEffort: &low},
			payload: map[string]any{},
			want:    map[string]any{"effort": "low", "summary": "auto"},
		},
		{
			name:    "model reasoning without a level asks for a summary only",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &efforts},
			payload: map[string]any{},
			want:    map[string]any{"summary": "auto"},
		},
		{
			name:    "request level wins",
			model:   &protocol.Model{Reasoning: &enabled, ReasoningEfforts: &efforts},
			payload: map[string]any{"reasoning_effort": "high"},
			want:    map[string]any{"effort": "high", "summary": "auto"},
		},
		{
			name:    "no reasoning anywhere sends nothing",
			model:   &protocol.Model{Reasoning: &disabled},
			payload: map[string]any{},
			want:    nil,
		},
		{
			name:    "reasoning budget enables the summary",
			model:   &protocol.Model{},
			payload: map[string]any{"reasoning": map[string]any{"max_tokens": float64(2048)}},
			want:    map[string]any{"summary": "auto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.payload["model"] = "vendor/gpt"
			tc.payload["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
			body, err := protocol.ToResponsesFromChat(tc.payload, tc.model)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			got, ok := body["reasoning"].(map[string]any)
			if tc.want == nil {
				if ok {
					t.Fatalf("reasoning = %v, want none", got)
				}
				return
			}
			if !ok {
				t.Fatalf("reasoning missing, want %v", tc.want)
			}
			encodedGot, _ := json.Marshal(got)
			encodedWant, _ := json.Marshal(tc.want)
			if string(encodedGot) != string(encodedWant) {
				t.Fatalf("reasoning = %s, want %s", encodedGot, encodedWant)
			}
		})
	}
}

// --- non-stream response conversion ---------------------------------------

func TestFromResponsesToChatAggregatesOutput(t *testing.T) {
	response := map[string]any{
		"id": "resp_1", "model": "vendor/gpt", "status": "completed",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "thought "},
				map[string]any{"type": "summary_text", "text": "more"}}},
			map[string]any{"type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": "hello "},
				map[string]any{"type": "output_text", "text": "world"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":1}`},
		},
		"usage": map[string]any{"input_tokens": float64(12), "output_tokens": float64(7),
			"total_tokens":          float64(19),
			"input_tokens_details":  map[string]any{"cached_tokens": float64(3)},
			"output_tokens_details": map[string]any{"reasoning_tokens": float64(2)}},
	}
	shaped, err := protocol.FromResponsesToChat(response, 1700000000)
	if err != nil {
		t.Fatalf("shape: %v", err)
	}
	choice := objectAt(t, listAt(t, shaped, "choices")[0], "")
	message := objectAt(t, choice, "message")
	if message["content"] != "hello world" {
		t.Fatalf("content = %v", message["content"])
	}
	if message["reasoning_content"] != "thought more" {
		t.Fatalf("reasoning_content = %v", message["reasoning_content"])
	}
	calls := listAt(t, message, "tool_calls")
	call := objectAt(t, calls[0], "")
	if call["id"] != "call_1" || objectAt(t, call, "function")["name"] != "lookup" ||
		objectAt(t, call, "function")["arguments"] != `{"q":1}` {
		t.Fatalf("tool call = %v", call)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := objectAt(t, shaped, "usage")
	if usage["prompt_tokens"] != int64(12) || usage["completion_tokens"] != int64(7) ||
		usage["total_tokens"] != int64(19) {
		t.Fatalf("usage = %v", usage)
	}
	if objectAt(t, usage, "prompt_tokens_details")["cached_tokens"] != int64(3) {
		t.Fatalf("cached tokens = %v", usage)
	}
	if objectAt(t, usage, "completion_tokens_details")["reasoning_tokens"] != int64(2) {
		t.Fatalf("reasoning tokens = %v", usage)
	}
	if shaped["object"] != "chat.completion" || shaped["id"] != "resp_1" || shaped["created"] != int64(1700000000) {
		t.Fatalf("envelope = %v", shaped)
	}
}

func TestFromResponsesToChatContentShapes(t *testing.T) {
	// Tools without text: content is JSON null, which is what a Chat client
	// expects before it reads the calls.
	onlyCalls := map[string]any{"status": "completed", "output": []any{
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"}}}
	shaped, err := protocol.FromResponsesToChat(onlyCalls, 1)
	if err != nil {
		t.Fatal(err)
	}
	message := objectAt(t, objectAt(t, listAt(t, shaped, "choices")[0], ""), "message")
	encoded, _ := json.Marshal(message)
	if !strings.Contains(string(encoded), `"content":null`) {
		t.Fatalf("content was not null: %s", encoded)
	}
	// Nothing at all: an empty string rather than null, so a client that prints
	// the field shows an empty answer instead of "null".
	empty := map[string]any{"status": "completed", "output": []any{}}
	shaped, err = protocol.FromResponsesToChat(empty, 1)
	if err != nil {
		t.Fatal(err)
	}
	message = objectAt(t, objectAt(t, listAt(t, shaped, "choices")[0], ""), "message")
	if message["content"] != "" {
		t.Fatalf("empty content = %v", message["content"])
	}
	if _, ok := message["reasoning_content"]; ok {
		t.Fatalf("empty reply carried reasoning: %v", message)
	}
	if _, ok := shaped["usage"]; ok {
		t.Fatalf("empty reply carried usage: %v", shaped["usage"])
	}
}

func TestFromResponsesToChatFinishReasons(t *testing.T) {
	cases := []struct {
		name     string
		response map[string]any
		want     string
		wantErr  bool
	}{
		{"stop", map[string]any{"status": "completed"}, "stop", false},
		{"length", map[string]any{"status": "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"}}, "length", false},
		{"content filter", map[string]any{"status": "incomplete",
			"incomplete_details": map[string]any{"reason": "content_filter"}}, "content_filter", false},
		{"failed", map[string]any{"status": "failed"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shaped, err := protocol.FromResponsesToChat(tc.response, 1)
			if tc.wantErr {
				if !errors.Is(err, protocol.ErrInvalidUpstreamResponse) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			choice := objectAt(t, listAt(t, shaped, "choices")[0], "")
			if choice["finish_reason"] != tc.want {
				t.Fatalf("finish_reason = %v, want %s", choice["finish_reason"], tc.want)
			}
		})
	}
}

// --- stream conversion -----------------------------------------------------

func sseFrame(t *testing.T, event string, fields map[string]any) string {
	t.Helper()
	payload := map[string]any{"type": event}
	for key, value := range fields {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "event: " + event + "\ndata: " + string(encoded) + "\n\n"
}

func chatFrames(t *testing.T, body string) ([]map[string]any, bool) {
	t.Helper()
	frames := []map[string]any{}
	done := false
	for _, block := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line, "\n") {
			if !strings.HasPrefix(part, "data: ") {
				continue
			}
			data := strings.TrimSpace(part[len("data: "):])
			if data == "[DONE]" {
				done = true
				continue
			}
			decoded := decodeBody(t, []byte(data))
			frames = append(frames, decoded)
		}
	}
	return frames, done
}

func frameDelta(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	choices, _ := frame["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	return objectAt(t, choices[0], "delta")
}

func TestResponsesChatStreamTranslatorStreamsDeltas(t *testing.T) {
	translator, err := protocol.NewResponsesChatTranslator(1024 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	feed := func(value string) { out.Write(translator.Push([]byte(value))) }
	feed(sseFrame(t, "response.created", map[string]any{"response": map[string]any{
		"id": "resp_1", "model": "vendor/gpt"}}))
	feed(sseFrame(t, "response.output_item.added", map[string]any{"output_index": 0,
		"item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant"}}))
	feed(sseFrame(t, "response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "delta": "hel"}))
	feed(sseFrame(t, "response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "delta": "lo"}))
	feed(sseFrame(t, "response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "output_index": 1, "delta": "why"}))
	// A raw reasoning text for the same item must not be stitched in twice.
	feed(sseFrame(t, "response.reasoning_text.delta", map[string]any{"item_id": "rs_1", "output_index": 1, "delta": "why"}))
	feed(sseFrame(t, "response.output_item.added", map[string]any{"output_index": 2,
		"item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup"}}))
	feed(sseFrame(t, "response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 2, "delta": `{"q"`}))
	feed(sseFrame(t, "response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 2, "delta": `:1}`}))
	feed(sseFrame(t, "response.output_item.added", map[string]any{"output_index": 3,
		"item": map[string]any{"id": "fc_2", "type": "function_call", "call_id": "call_2", "name": "store"}}))
	feed(sseFrame(t, "response.function_call_arguments.delta", map[string]any{"item_id": "fc_2", "output_index": 3, "delta": `{"r":2}`}))
	feed(sseFrame(t, "response.completed", map[string]any{"response": map[string]any{
		"id": "resp_1", "model": "vendor/gpt", "status": "completed",
		"output": []any{
			map[string]any{"id": "msg_1", "type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": "hello"}}},
			map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "why"}}},
			map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1",
				"name": "lookup", "arguments": `{"q":1}`},
			map[string]any{"id": "fc_2", "type": "function_call", "call_id": "call_2",
				"name": "store", "arguments": `{"r":2}`},
		},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(5),
			"total_tokens":          float64(15),
			"input_tokens_details":  map[string]any{"cached_tokens": float64(4)},
			"output_tokens_details": map[string]any{"reasoning_tokens": float64(2)}},
	}}))
	out.Write(translator.End())

	body := out.String()
	if translator.Outcome() != protocol.OutcomeCompleted || !translator.Closed() {
		t.Fatalf("outcome = %q closed %v", translator.Outcome(), translator.Closed())
	}
	frames, done := chatFrames(t, body)
	if !done {
		t.Fatalf("stream lacks [DONE]: %s", body)
	}
	if strings.Count(body, `"role":"assistant"`) != 1 {
		t.Fatalf("role frame count = %d", strings.Count(body, `"role":"assistant"`))
	}
	var content, reasoning strings.Builder
	toolCalls := 0
	toolArgs := map[float64]*strings.Builder{}
	toolNames := map[float64]string{}
	var finish any
	var usage map[string]any
	for _, frame := range frames {
		delta := frameDelta(t, frame)
		if delta != nil {
			if text, ok := delta["content"].(string); ok {
				content.WriteString(text)
			}
			if text, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(text)
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, row := range calls {
					call := objectAt(t, row, "")
					toolCalls++
					index := call["index"].(float64)
					if index != float64(0) && index != float64(1) {
						t.Fatalf("tool call frame = %v", call)
					}
					if typ, ok := call["type"].(string); ok && typ != "function" {
						t.Fatalf("tool call type = %q", typ)
					}
					if id, ok := call["id"].(string); ok && id != "call_1" && id != "call_2" {
						t.Fatalf("tool call id = %q", id)
					}
					fn := objectAt(t, call, "function")
					if name, ok := fn["name"].(string); ok && name != "" {
						toolNames[index] = name
					}
					if value, ok := fn["arguments"].(string); ok {
						if toolArgs[index] == nil {
							toolArgs[index] = &strings.Builder{}
						}
						toolArgs[index].WriteString(value)
					}
				}
			}
			if reason, ok := delta["finish_reason"]; ok {
				finish = reason
			}
		}
		if choices, ok := frame["choices"].([]any); ok && len(choices) > 0 {
			if reason, ok := objectAt(t, choices[0], "")["finish_reason"]; ok {
				finish = reason
			}
		}
		if raw, ok := frame["usage"].(map[string]any); ok {
			usage = raw
		}
	}
	if content.String() != "hello" {
		t.Fatalf("content = %q", content.String())
	}
	if reasoning.String() != "why" {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	if toolArgs[0].String() != `{"q":1}` || toolArgs[1].String() != `{"r":2}` {
		t.Fatalf("arguments = %q / %q", toolArgs[0].String(), toolArgs[1].String())
	}
	if toolNames[0] != "lookup" || toolNames[1] != "store" {
		t.Fatalf("tool names = %v", toolNames)
	}
	if toolCalls != 5 {
		t.Fatalf("tool frames = %d", toolCalls)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %v", finish)
	}
	if usage == nil {
		t.Fatal("stream lacks a usage chunk")
	}
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) ||
		usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
	if objectAt(t, usage, "prompt_tokens_details")["cached_tokens"] != float64(4) {
		t.Fatalf("usage cache = %v", usage)
	}
	if objectAt(t, usage, "completion_tokens_details")["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage reasoning = %v", usage)
	}

	// The gateway's own scanner is what books the tokens, so it must see the
	// same numbers and the same terminal state.
	scanner := protocol.NewUsageScanner(protocol.ScanSSE)
	scanner.Push([]byte(body))
	scanned := scanner.Finish()
	if scanner.Outcome() != protocol.OutcomeCompleted || scanned == nil {
		t.Fatalf("scanner outcome = %q usage %+v", scanner.Outcome(), scanned)
	}
	if scanned.InputTokens == nil || *scanned.InputTokens != 10 || scanned.OutputTokens == nil || *scanned.OutputTokens != 5 {
		t.Fatalf("scanned usage = %+v", scanned)
	}
	if scanned.CachedInputTokens == nil || *scanned.CachedInputTokens != 4 {
		t.Fatalf("scanned cache = %+v", scanned)
	}
}

func TestResponsesChatStreamTranslatorFillsAggregatedItems(t *testing.T) {
	translator, err := protocol.NewResponsesChatTranslator(1024 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	feed := func(value string) { out.Write(translator.Push([]byte(value))) }
	feed(sseFrame(t, "response.created", map[string]any{"response": map[string]any{
		"id": "resp_2", "model": "vendor/gpt"}}))
	feed(sseFrame(t, "response.output_item.added", map[string]any{"output_index": 0,
		"item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant"}}))
	feed(sseFrame(t, "response.output_item.done", map[string]any{"output_index": 0,
		"item": map[string]any{"id": "msg_1", "type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "aggregated"}}}}))
	feed(sseFrame(t, "response.output_item.done", map[string]any{"output_index": 1,
		"item": map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "because"}}}}))
	feed(sseFrame(t, "response.output_item.done", map[string]any{"output_index": 2,
		"item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_9",
			"name": "lookup", "arguments": `{"q":1}`}}))
	feed(sseFrame(t, "response.completed", map[string]any{"response": map[string]any{
		"id": "resp_2", "model": "vendor/gpt", "status": "completed",
		"output": []any{
			map[string]any{"id": "msg_1", "type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": "aggregated"}}},
			map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "because"}}},
			map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_9",
				"name": "lookup", "arguments": `{"q":1}`},
		},
		"usage": map[string]any{"input_tokens": float64(3), "output_tokens": float64(2)},
	}}))
	out.Write(translator.End())
	body := out.String()
	frames, done := chatFrames(t, body)
	if !done {
		t.Fatalf("stream lacks [DONE]: %s", body)
	}
	var content, reasoning, arguments strings.Builder
	var callID any
	for _, frame := range frames {
		delta := frameDelta(t, frame)
		if delta == nil {
			continue
		}
		if text, ok := delta["content"].(string); ok {
			content.WriteString(text)
		}
		if text, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(text)
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, row := range calls {
			call := objectAt(t, row, "")
			if id, ok := call["id"].(string); ok {
				callID = id
			}
			if value, ok := objectAt(t, call, "function")["arguments"].(string); ok {
				arguments.WriteString(value)
			}
		}
	}
	if content.String() != "aggregated" {
		t.Fatalf("aggregated text appeared as %q: %s", content.String(), body)
	}
	if reasoning.String() != "because" {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	if arguments.String() != `{"q":1}` {
		t.Fatalf("arguments = %q", arguments.String())
	}
	if callID != "call_9" {
		t.Fatalf("tool call id = %v", callID)
	}
	if translator.Outcome() != protocol.OutcomeCompleted {
		t.Fatalf("outcome = %q", translator.Outcome())
	}
}

func TestResponsesChatStreamTranslatorTerminalStates(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		translator, _ := protocol.NewResponsesChatTranslator(1024 * 1024)
		out := translator.Push([]byte(sseFrame(t, "response.output_text.delta", map[string]any{
			"item_id": "msg_1", "output_index": 0, "delta": "cut"})))
		out = append(out, translator.Push([]byte(sseFrame(t, "response.incomplete", map[string]any{
			"response": map[string]any{"id": "resp_3", "status": "incomplete",
				"incomplete_details": map[string]any{"reason": "max_output_tokens"},
				"usage":              map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)}}})))...)
		out = append(out, translator.End()...)
		body := string(out)
		if translator.Outcome() != protocol.OutcomeIncomplete {
			t.Fatalf("outcome = %q", translator.Outcome())
		}
		if !strings.Contains(body, `"finish_reason":"length"`) || !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("incomplete stream = %s", body)
		}
		scanner := protocol.NewUsageScanner(protocol.ScanSSE)
		scanner.Push([]byte(body))
		scanner.Finish()
		if scanner.Outcome() != protocol.OutcomeIncomplete {
			t.Fatalf("scanner outcome = %q", scanner.Outcome())
		}
	})

	t.Run("failed event", func(t *testing.T) {
		translator, _ := protocol.NewResponsesChatTranslator(1024 * 1024)
		out := translator.Push([]byte(sseFrame(t, "response.failed", map[string]any{
			"response": map[string]any{"id": "resp_4", "status": "failed",
				"error": map[string]any{"code": "upstream_overloaded", "message": "busy"}}})))
		out = append(out, translator.End()...)
		body := string(out)
		if translator.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("outcome = %q", translator.Outcome())
		}
		if strings.Contains(body, "[DONE]") {
			t.Fatalf("failed stream faked completion: %s", body)
		}
		if !strings.Contains(body, `"type":"upstream_overloaded"`) || !strings.Contains(body, `"message":"busy"`) {
			t.Fatalf("failure frame = %s", body)
		}
		scanner := protocol.NewUsageScanner(protocol.ScanSSE)
		scanner.Push([]byte(body))
		scanner.Finish()
		if scanner.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("scanner outcome = %q", scanner.Outcome())
		}
	})

	t.Run("disconnect without terminal event", func(t *testing.T) {
		translator, _ := protocol.NewResponsesChatTranslator(1024 * 1024)
		out := translator.Push([]byte(sseFrame(t, "response.output_text.delta", map[string]any{
			"item_id": "msg_1", "output_index": 0, "delta": "partial"})))
		out = append(out, translator.End()...)
		body := string(out)
		if translator.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("outcome = %q", translator.Outcome())
		}
		if strings.Contains(body, "[DONE]") {
			t.Fatalf("broken stream faked completion: %s", body)
		}
		if !strings.Contains(body, `"type":"incomplete_stream"`) {
			t.Fatalf("missing incomplete error: %s", body)
		}
		scanner := protocol.NewUsageScanner(protocol.ScanSSE)
		scanner.Push([]byte(body))
		scanner.Finish()
		if scanner.Outcome() != protocol.OutcomeFailed {
			t.Fatalf("scanner outcome = %q", scanner.Outcome())
		}
	})

	t.Run("error event", func(t *testing.T) {
		translator, _ := protocol.NewResponsesChatTranslator(1024 * 1024)
		out := translator.Push([]byte(sseFrame(t, "error", map[string]any{
			"error": map[string]any{"type": "server_error", "message": "boom"}})))
		out = append(out, translator.End()...)
		if translator.Outcome() != protocol.OutcomeFailed || strings.Contains(string(out), "[DONE]") {
			t.Fatalf("error event outcome = %q body %s", translator.Outcome(), out)
		}
	})
}

func TestResponsesChatStreamTranslatorHandlesSplitUTF8AndCRLF(t *testing.T) {
	translator, _ := protocol.NewResponsesChatTranslator(1024 * 1024)
	frame := "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\"," +
		"\"item_id\":\"msg_1\",\"output_index\":0,\"delta\":\"你好世界\"}\r\n\r\n"
	// Split inside the first rune and right after a CR that still needs its LF.
	at := strings.Index(frame, "你") + 1
	cr := strings.Index(frame, "\r\n\r\n")
	var out strings.Builder
	out.Write(translator.Push([]byte(frame[:at])))
	out.Write(translator.Push([]byte(frame[at : cr+1])))
	out.Write(translator.Push([]byte(frame[cr+1:])))
	out.Write(translator.Push([]byte(sseFrame(t, "response.completed", map[string]any{
		"response": map[string]any{"id": "resp_5", "status": "completed"}}))))
	out.Write(translator.End())
	body := out.String()
	if !strings.Contains(body, `"content":"你好世界"`) {
		t.Fatalf("split rune was not reassembled: %s", body)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("output is not valid UTF-8: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream did not complete: %s", body)
	}
}

func TestResponsesChatStreamTranslatorBoundsBufferedEvent(t *testing.T) {
	translator, err := protocol.NewResponsesChatTranslator(32)
	if err != nil {
		t.Fatal(err)
	}
	out := translator.Push([]byte(`data: {"type":"response.output_text.delta","item_id":"m","delta":"` +
		strings.Repeat("x", 64)))
	body := string(out) + string(translator.End())
	if translator.Outcome() != protocol.OutcomeFailed {
		t.Fatalf("outcome = %q", translator.Outcome())
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("overflowed stream faked completion: %s", body)
	}
}

// --- gateway end to end ----------------------------------------------------

func startGatewayWithEndpoints(t *testing.T, endpoints []protocol.Endpoint, models []protocol.Model,
	config func(*protocol.Config)) (*protocol.Gateway, string) {
	t.Helper()
	backend, err := protocol.NewBackend(endpoints, models)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	cfg := protocol.Config{Backend: backend, Key: gatewayKey, RecordRequests: true}
	if config != nil {
		config(&cfg)
	}
	gateway, err := protocol.NewGateway(cfg)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	server, url, err := gateway.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return gateway, url
}

func responsesJSONReply(body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestGatewayChatRouteConvertsResponsesModel(t *testing.T) {
	reasoning := true
	efforts := []string{"low", "medium", "high"}
	model := responsesRow("vendor/gpt", func(m *protocol.Model) {
		m.Reasoning = &reasoning
		m.ReasoningEfforts = &efforts
	})
	upstream := newUpstream(t, responsesJSONReply(`{
		"id":"resp_1","object":"response","model":"vendor/gpt","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}
		],
		"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19,
			"input_tokens_details":{"cached_tokens":3},
			"output_tokens_details":{"reasoning_tokens":2}}
	}`))
	gate := &fakeGate{}
	var mu sync.Mutex
	var entries []protocol.UsageEntry
	sink := protocol.UsageSinkFunc(func(entry protocol.UsageEntry) error {
		mu.Lock()
		entries = append(entries, entry)
		mu.Unlock()
		return nil
	})
	gateway, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{BaseURL: upstream.server.URL, APIKey: "synthetic-upstream"},
	}, []protocol.Model{model}, func(c *protocol.Config) {
		c.KimiK3Compatibility = true
		c.Budget = gate
		c.UsageSink = sink
	})
	compatible := protocol.KimiCompatibleModelID("vendor/gpt")

	_, catalog := fetchCatalog(t, url)
	if _, ok := catalog.Kimi.Models[compatible]; !ok {
		t.Fatalf("catalog lacks the generated id: %v", catalog.Kimi.Models)
	}
	if len(catalog.Responses.Models) != 0 {
		t.Fatalf("catalog still has a responses group: %v", catalog.Responses.Models)
	}

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model": compatible,
		"messages": []any{
			map[string]any{"role": "system", "content": "rules"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "https://example.com/a.png", "detail": "low"}},
			}},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
		"max_tokens":       float64(128),
		"reasoning_effort": "medium",
	}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	captured := upstream.captured()
	if len(captured) != 1 || captured[0].URL != "/responses" {
		t.Fatalf("upstream captured = %+v", captured)
	}
	sent := decodeBody(t, []byte(captured[0].Body))
	if sent["model"] != "vendor/gpt" {
		t.Fatalf("upstream model = %v", sent["model"])
	}
	if sent["instructions"] != "rules" {
		t.Fatalf("upstream instructions = %v", sent["instructions"])
	}
	if sent["max_output_tokens"] != float64(128) {
		t.Fatalf("upstream max_output_tokens = %v", sent["max_output_tokens"])
	}
	if !strings.Contains(captured[0].Body, `"type":"input_image"`) {
		t.Fatalf("image was not converted: %s", captured[0].Body)
	}
	tool := objectAt(t, listAt(t, sent, "tools")[0], "")
	if tool["type"] != "function" || tool["name"] != "lookup" {
		t.Fatalf("tools were not flattened: %v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Fatalf("tool kept the Chat nesting: %v", tool)
	}
	if objectAt(t, sent, "reasoning")["effort"] != "medium" {
		t.Fatalf("reasoning = %v", sent["reasoning"])
	}

	shaped := decodeBody(t, []byte(body))
	choice := objectAt(t, listAt(t, shaped, "choices")[0], "")
	message := objectAt(t, choice, "message")
	if message["content"] != "answer" || message["reasoning_content"] != "thinking" {
		t.Fatalf("message = %v", message)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := objectAt(t, shaped, "usage")
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) {
		t.Fatalf("usage = %v", usage)
	}

	record := waitForRecentRecord(t, gateway)
	if record.Model != "vendor/gpt" || record.Protocol != protocol.ProtocolChat || record.Status != http.StatusOK {
		t.Fatalf("record = %+v", record)
	}
	if record.Usage == nil || record.Usage.CachedInputTokens == nil || *record.Usage.CachedInputTokens != 3 {
		t.Fatalf("record usage = %+v", record.Usage)
	}
	if record.Outcome != string(protocol.OutcomeCompleted) {
		t.Fatalf("record outcome = %q", record.Outcome)
	}
	if len(gate.requests) != 1 || gate.requests[0].Model != "vendor/gpt" {
		t.Fatalf("budget admissions = %+v", gate.requests)
	}
	if len(gate.settled) != 1 || gate.settled[0].Model != "vendor/gpt" {
		t.Fatalf("budget settlements = %+v", gate.settled)
	}
	terminal := waitForUsageEntry(t, &mu, &entries)
	if terminal.ModelID != "vendor/gpt" || terminal.Usage == nil ||
		terminal.Usage.CachedInputTokens == nil || *terminal.Usage.CachedInputTokens != 3 {
		t.Fatalf("usage entry = %+v", terminal)
	}
}

func TestGatewayChatRouteStreamsResponsesModel(t *testing.T) {
	upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("stream flag was not converted: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			sseFrame(t, "response.created", map[string]any{"response": map[string]any{
				"id": "resp_2", "model": "vendor/gpt"}}),
			sseFrame(t, "response.output_item.added", map[string]any{"output_index": 0,
				"item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant"}}),
			sseFrame(t, "response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "delta": "streamed"}),
			sseFrame(t, "response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "output_index": 1, "delta": "why"}),
			sseFrame(t, "response.completed", map[string]any{"response": map[string]any{
				"id": "resp_2", "model": "vendor/gpt", "status": "completed",
				"output": []any{
					map[string]any{"id": "msg_1", "type": "message", "content": []any{
						map[string]any{"type": "output_text", "text": "streamed"}}},
					map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{
						map[string]any{"type": "summary_text", "text": "why"}}},
				},
				"usage": map[string]any{"input_tokens": float64(9), "output_tokens": float64(4),
					"total_tokens":         float64(13),
					"input_tokens_details": map[string]any{"cached_tokens": float64(2)}},
			}}),
		}
		for index, event := range events {
			_, _ = w.Write([]byte(event))
			if index == 0 && flusher != nil {
				flusher.Flush()
			}
		}
	})
	gateway, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{BaseURL: upstream.server.URL, APIKey: "synthetic-upstream"},
	}, []protocol.Model{responsesRow("vendor/gpt")}, func(c *protocol.Config) {
		c.KimiK3Compatibility = true
	})
	compatible := protocol.KimiCompatibleModelID("vendor/gpt")
	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    compatible,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if !strings.Contains(reply.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", reply.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, `"content":"streamed"`) || !strings.Contains(body, `"reasoning_content":"why"`) {
		t.Fatalf("stream = %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream lacks [DONE]: %s", body)
	}
	record := waitForRecentRecord(t, gateway)
	if record.Outcome != string(protocol.OutcomeCompleted) {
		t.Fatalf("record outcome = %q", record.Outcome)
	}
	if record.Usage == nil || record.Usage.InputTokens == nil || *record.Usage.InputTokens != 9 ||
		record.Usage.OutputTokens == nil || *record.Usage.OutputTokens != 4 {
		t.Fatalf("record usage = %+v", record.Usage)
	}
	if record.Usage.CachedInputTokens == nil || *record.Usage.CachedInputTokens != 2 {
		t.Fatalf("record cache = %+v", record.Usage)
	}
}

func TestGatewayChatRoutePassesResponsesUpstreamErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		typ    string
		want   string
	}{
		{"rate limited", http.StatusTooManyRequests,
			`{"error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"slow down"}}`,
			"rate_limit_exceeded", "rate_limit_exceeded"},
		{"server error", http.StatusInternalServerError,
			`{"error":{"type":"server_error","message":"upstream broke"}}`,
			"server_error", "server_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			gateway, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
				{BaseURL: upstream.server.URL, APIKey: "synthetic-upstream"},
			}, []protocol.Model{responsesRow("vendor/gpt")}, func(c *protocol.Config) {
				c.KimiK3Compatibility = true
			})
			reply, body := post(t, url, "/chat/completions", map[string]any{
				"model":    protocol.KimiCompatibleModelID("vendor/gpt"),
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, nil)
			if reply.StatusCode != tc.status {
				t.Fatalf("status %d body %s", reply.StatusCode, body)
			}
			shaped := decodeBody(t, []byte(body))
			inner := objectAt(t, shaped, "error")
			if inner["type"] != tc.typ {
				t.Fatalf("error = %v", inner)
			}
			record := waitForRecentRecord(t, gateway)
			if record.Status != tc.status || record.Side != protocol.SideUpstream {
				t.Fatalf("record = %+v", record)
			}
			if record.UpstreamErrorType != tc.want {
				t.Fatalf("upstream error type = %q", record.UpstreamErrorType)
			}
			if tc.status == http.StatusTooManyRequests && !strings.Contains(body, "slow down") {
				t.Fatalf("vendor message was dropped: %s", body)
			}
		})
	}
}

func TestGatewayChatRouteFailsOverFromResponsesToAnthropic(t *testing.T) {
	models, err := protocol.ValidateModels([]json.RawMessage{[]byte(
		`{"id":"vendor/gpt","protocol":"responses","contextWindow":8192,"connectionId":"primary",` +
			`"failover":[{"connectionId":"standby","upstreamProtocol":"anthropic"}]}`)})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	standby := newUpstream(t, anthropicReply("from-anthropic-standby"))
	gateway, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{ID: "primary", BaseURL: primary.server.URL, APIKey: "synthetic-primary"},
		{ID: "standby", BaseURL: standby.server.URL, APIKey: "synthetic-standby"},
	}, models, func(c *protocol.Config) { c.KimiK3Compatibility = true })

	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    protocol.KimiCompatibleModelID("vendor/gpt"),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if reply.StatusCode != http.StatusOK || !strings.Contains(body, "from-anthropic-standby") {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if len(primary.captured()) != 1 || primary.captured()[0].URL != "/responses" {
		t.Fatalf("primary captured = %+v", primary.captured())
	}
	if len(standby.captured()) != 1 || standby.captured()[0].URL != "/messages" {
		t.Fatalf("standby captured = %+v", standby.captured())
	}
	record := waitForRecentRecord(t, gateway)
	if record.Attempts != 2 || record.ConnectionID == nil || *record.ConnectionID != "standby" {
		t.Fatalf("record = %+v", record)
	}
	if record.UpstreamProtocol == nil || *record.UpstreamProtocol != protocol.ProtocolAnthropic {
		t.Fatalf("record upstream wire = %v", record.UpstreamProtocol)
	}
}

func TestGatewayChatRouteFailsOverToChatStandby(t *testing.T) {
	cases := []struct {
		name        string
		failover    string
		protocols   []string
		wantStandby string
	}{
		{
			name:        "explicit chat override",
			failover:    `"failover":[{"connectionId":"standby","upstreamProtocol":"chat"}]`,
			wantStandby: "/chat/completions",
		},
		{
			name:        "chat-only standby endpoint",
			failover:    `"failover":[{"connectionId":"standby"}]`,
			protocols:   []string{"chat"},
			wantStandby: "/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models, err := protocol.ValidateModels([]json.RawMessage{[]byte(
				`{"id":"vendor/gpt","protocol":"responses","contextWindow":8192,"connectionId":"primary",` +
					tc.failover + `}`)})
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			primary := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			standby := newUpstream(t, jsonReply("from-chat-standby"))
			gateway, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
				{ID: "primary", BaseURL: primary.server.URL, APIKey: "synthetic-primary"},
				{ID: "standby", BaseURL: standby.server.URL, APIKey: "synthetic-standby", Protocols: tc.protocols},
			}, models, func(c *protocol.Config) { c.KimiK3Compatibility = true })

			reply, body := post(t, url, "/chat/completions", map[string]any{
				"model":    protocol.KimiCompatibleModelID("vendor/gpt"),
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			if reply.StatusCode != http.StatusOK || !strings.Contains(body, "from-chat-standby") {
				t.Fatalf("status %d body %s", reply.StatusCode, body)
			}
			if len(primary.captured()) != 1 || primary.captured()[0].URL != "/responses" {
				t.Fatalf("primary captured = %+v", primary.captured())
			}
			if len(standby.captured()) != 1 || standby.captured()[0].URL != tc.wantStandby {
				t.Fatalf("standby captured = %+v", standby.captured())
			}
			if !strings.Contains(standby.captured()[0].Body, `"messages"`) {
				t.Fatalf("standby did not receive the Chat body: %s", standby.captured()[0].Body)
			}
			record := waitForRecentRecord(t, gateway)
			if record.Attempts != 2 || record.ConnectionID == nil || *record.ConnectionID != "standby" {
				t.Fatalf("record = %+v", record)
			}
		})
	}
}

func TestResponsesModelStaysRefusedWithoutCompatibility(t *testing.T) {
	upstream := newUpstream(t, jsonReply("must not be used"))
	_, url := startGatewayWithEndpoints(t, []protocol.Endpoint{
		{BaseURL: upstream.server.URL, APIKey: "synthetic-upstream"},
	}, []protocol.Model{responsesRow("vendor/gpt")}, func(c *protocol.Config) {
		c.KimiK3Compatibility = false
	})
	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model":    "vendor/gpt",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if reply.StatusCode != http.StatusConflict || errorCode(t, body) != "model_protocol_mismatch" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if len(upstream.captured()) != 0 {
		t.Fatal("a refused request reached the upstream")
	}
}

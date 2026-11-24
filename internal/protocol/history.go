package protocol

import (
	"encoding/json"
	"strings"
)

func cleanOpenAIHistory(payload []byte, route string) []byte {
	field := "messages"
	if route == ProtocolResponses {
		field = "input"
	}
	document, err := ParseDocument(payload)
	if err != nil {
		return payload
	}
	raw, _ := document.Get(field)
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil {
		return payload
	}
	changed := false
	cleanReasoning := route == ProtocolResponses
	start := len(rows)
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if item, err := Object(row); err == nil {
			if route == ProtocolResponses && isForeignResponsesThinking(item) {
				changed = true
				continue
			}
			role, _ := rawString(item.values["role"])
			switch role {
			case "user":
				cleanReasoning = true
			case "assistant":
				if edited, keep := cleanOpenAIAssistant(item, route, cleanReasoning); edited {
					changed = true
					if !keep {
						continue
					}
					row = item.Bytes()
				}
			}
		}
		start--
		rows[start] = row
	}
	if !changed {
		return payload
	}
	cleaned, _ := json.Marshal(rows[start:])
	document.SetRaw(field, cleaned)
	return document.Bytes()
}

func isForeignResponsesThinking(item *Document) bool {
	typ, _ := rawString(item.values["type"])
	switch typ {
	case "thinking", "redacted_thinking":
		return true
	case "reasoning":
		id, _ := rawString(item.values["id"])
		encrypted, _ := rawString(item.values["encrypted_content"])
		if encrypted != "" {
			return false
		}
		return id == "" || isSyntheticItemID(id)
	default:
		return false
	}
}

func isSyntheticItemID(id string) bool {
	index := strings.LastIndex(id, "_item_")
	if index <= 0 {
		return false
	}
	suffix := id[index+len("_item_"):]
	if suffix == "" {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return false
		}
	}
	return true
}

func cleanOpenAIAssistant(item *Document, route string, cleanReasoning bool) (edited, keep bool) {
	for _, key := range []string{"reasoning_content", "reasoning_details", "reasoning"} {
		if _, exists := item.Get(key); cleanReasoning && exists {
			item.Delete(key)
			edited = true
		}
	}
	content, _ := item.Get("content")
	var parts []json.RawMessage
	removedParts := false
	if json.Unmarshal(content, &parts) == nil {
		kept := parts[:0]
		for _, part := range parts {
			if object, err := Object(part); err == nil {
				typ, _ := rawString(object.values["type"])
				if typ == "thinking" || typ == "redacted_thinking" || typ == "reasoning" {
					removedParts = true
					continue
				}
			}
			kept = append(kept, part)
		}
		if removedParts && len(kept) > 0 {
			content, _ = json.Marshal(kept)
			item.SetRaw("content", content)
			return true, true
		}
	}
	if !edited && !removedParts {
		return false, true
	}
	text, _ := rawString(content)
	if !removedParts && (len(parts) > 0 || text != "") {
		return true, true
	}
	if route == ProtocolChat && hasChatAssistantPayload(item) {
		if removedParts {
			item.SetRaw("content", json.RawMessage("null"))
		}
		return true, true
	}
	return true, false
}

func hasChatAssistantPayload(item *Document) bool {
	var calls []json.RawMessage
	if json.Unmarshal(item.values["tool_calls"], &calls) == nil && len(calls) > 0 {
		return true
	}
	for _, key := range []string{"function_call", "refusal", "audio"} {
		if value, ok := item.Get(key); ok && string(value) != "null" {
			return true
		}
	}
	return false
}

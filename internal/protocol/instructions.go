package protocol

import (
	"encoding/json"
	"strings"
)

// Shared-instruction tags. The tag travels upstream with the prompt, so it
// names neither the app nor a vendor. Histories written by older builds hold
// the previous tag; both are taken out, so a history never carries two copies.
const (
	InstructionsBegin = "<shared-instructions>"
	InstructionsEnd   = "</shared-instructions>"
)

var legacyInstructionTags = [][2]string{
	{InstructionsBegin, InstructionsEnd},
	{"<magpie-kimi-shared-instructions>", "</magpie-kimi-shared-instructions>"},
}

func isManagedMessage(message map[string]any) bool {
	if role, _ := message["role"].(string); role != "system" {
		return false
	}
	content := ""
	switch value := message["content"].(type) {
	case string:
		content = value
	case []any:
		if len(value) == 1 {
			if part, ok := value[0].(map[string]any); ok {
				if typ, _ := part["type"].(string); typ == "input_text" {
					content, _ = part["text"].(string)
				}
			}
		}
	}
	if content == "" {
		return false
	}
	for _, pair := range legacyInstructionTags {
		if strings.HasPrefix(content, pair[0]+"\n") && strings.HasSuffix(content, "\n"+pair[1]) {
			return true
		}
	}
	return false
}

// ApplyInstructions inserts the shared rules as one managed system message and
// removes any previous copy, so a history never accumulates them. An empty
// `text` only strips the managed message. This is a pure transformation: the
// caller reads the file.
func ApplyInstructions(payload *Document, text, route string) error {
	if payload == nil {
		return ErrInvalidRequest
	}
	text = strings.TrimSpace(text)
	switch route {
	case ProtocolChat:
		raw, ok := payload.Get("messages")
		if !ok {
			return ErrInvalidMessages
		}
		var rows []any
		if json.Unmarshal(raw, &rows) != nil {
			return ErrInvalidMessages
		}
		kept := make([]any, 0, len(rows)+1)
		for _, row := range rows {
			message, ok := row.(map[string]any)
			if !ok {
				kept = append(kept, row)
				continue
			}
			if isManagedMessage(message) {
				continue
			}
			kept = append(kept, row)
		}
		if text != "" {
			at := 0
			for at < len(kept) {
				message, ok := kept[at].(map[string]any)
				if !ok {
					break
				}
				role, _ := message["role"].(string)
				if role != "system" && role != "developer" {
					break
				}
				at++
			}
			managed := map[string]any{"role": "system",
				"content": InstructionsBegin + "\n" + text + "\n" + InstructionsEnd}
			kept = append(kept[:at], append([]any{managed}, kept[at:]...)...)
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return ErrInvalidMessages
		}
		payload.SetRaw("messages", encoded)
		return nil
	case ProtocolResponses:
		raw, has := payload.Get("input")
		if !has {
			return ErrInvalidInput
		}
		var single string
		isSingle := json.Unmarshal(raw, &single) == nil
		var rows []any
		if !isSingle && json.Unmarshal(raw, &rows) != nil {
			return ErrInvalidInput
		}
		if text == "" && isSingle {
			return nil
		}
		if isSingle {
			rows = []any{map[string]any{"role": "user", "content": single}}
		}
		kept := make([]any, 0, len(rows)+1)
		for _, row := range rows {
			if message, ok := row.(map[string]any); ok && isManagedMessage(message) {
				continue
			}
			kept = append(kept, row)
		}
		if text != "" {
			at := 0
			for at < len(kept) {
				message, ok := kept[at].(map[string]any)
				if !ok {
					break
				}
				role, _ := message["role"].(string)
				if role != "system" && role != "developer" {
					break
				}
				at++
			}
			managed := map[string]any{"role": "system", "content": []any{map[string]any{
				"type": "input_text", "text": InstructionsBegin + "\n" + text + "\n" + InstructionsEnd}}}
			kept = append(kept[:at], append([]any{managed}, kept[at:]...)...)
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return ErrInvalidInput
		}
		payload.SetRaw("input", encoded)
		return nil
	}
	return ErrInvalidProtocol
}

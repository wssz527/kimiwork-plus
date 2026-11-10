package core

import (
	"errors"
	"kimiworkplus.local/migration/internal/protocol"
	"net/url"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
var envReplace = regexp.MustCompile(`[^A-Z0-9]+`)
var modelFields = map[string]bool{}

func init() {
	for _, k := range strings.Fields("id name connectionId upstreamModelId protocol upstreamProtocol contextWindow contextLimit maxOutputTokens tools reasoning inputModalities reasoningEfforts defaultEffort metadataSource") {
		modelFields[k] = true
	}
}

func String(v any) string { s, _ := v.(string); return s }
func Rows(v any) []any    { r, _ := v.([]any); return r }
func Object(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Document:
		return map[string]any(m)
	}
	return nil
}
func KeyEnv(id string) string {
	return "KIMI_WORK_" + strings.Trim(envReplace.ReplaceAllString(strings.ToUpper(id), "_"), "_") + "_KEY"
}
func BaseURL(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid_base_url")
	}
	loop := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return "", errors.New("invalid_base_url")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func Validate(d Document) error {
	connections := map[string]map[string]any{}
	envs := map[string]bool{}
	if _, ok := d["connections"].([]any); !ok {
		return errors.New("invalid_connections")
	}
	if len(Rows(d["connections"])) > 32 {
		return errors.New("invalid_connections")
	}
	for _, v := range Rows(d["connections"]) {
		c := Object(v)
		id := String(c["id"])
		if !idPattern.MatchString(id) || connections[id] != nil || strings.TrimSpace(String(c["name"])) == "" {
			return errors.New("invalid_connections")
		}
		base, e := BaseURL(String(c["baseURL"]))
		if e != nil {
			return e
		}
		c["baseURL"] = base
		env := String(c["apiKeyEnv"])
		if env == "" {
			env = KeyEnv(id)
			c["apiKeyEnv"] = env
		}
		if !envPattern.MatchString(env) || envs[env] {
			return errors.New("invalid_connections")
		}
		for _, p := range Rows(c["protocols"]) {
			if p != "chat" && p != "responses" {
				return errors.New("invalid_connections")
			}
		}
		connections[id] = c
		envs[env] = true
	}
	if _, ok := d["models"].([]any); !ok {
		return errors.New("invalid_models")
	}
	models, err := protocol.ValidateModelsAny(Rows(d["models"]))
	if err != nil {
		return err
	}
	for _, raw := range Rows(d["models"]) {
		m := Object(raw)
		if id := String(m["id"]); id != strings.TrimSpace(id) {
			return errors.New("invalid_models")
		}
		for _, effort := range Rows(m["reasoningEfforts"]) {
			if !effortPattern.MatchString(String(effort)) {
				return errors.New("invalid_models")
			}
		}
		_, hasDefault := m["defaultEffort"]
		if m["reasoning"] != true && (len(Rows(m["reasoningEfforts"])) > 0 || hasDefault) {
			return errors.New("invalid_models")
		}
		if modalities, exists := m["inputModalities"]; exists {
			hasText := false
			for _, modality := range Rows(modalities) {
				hasText = hasText || modality == "text"
			}
			if !hasText {
				return errors.New("invalid_models")
			}
		}
	}
	for _, m := range models {
		if m.ConnectionID != nil && connections[*m.ConnectionID] == nil {
			return errors.New("connection_unavailable")
		}
	}
	return nil
}

func MergeModels(d Document, input []any) error {
	old := map[string]map[string]any{}
	for _, v := range Rows(d["models"]) {
		m := Object(v)
		old[String(m["id"])] = m
	}
	out := make([]any, 0, len(input))
	originals := map[string]bool{}
	for _, v := range input {
		row := Object(v)
		id := String(row["id"])
		original := String(row["originalId"])
		if original == "" {
			original = id
		} else if old[original] == nil {
			return errors.New("model_original_unavailable")
		}
		if originals[original] {
			return errors.New("invalid_models")
		}
		originals[original] = true
		merged := map[string]any{}
		for k, v := range old[original] {
			merged[k] = v
		}
		for k, v := range row {
			if modelFields[k] {
				if v == nil {
					delete(merged, k)
				} else {
					merged[k] = v
				}
			}
		}
		out = append(out, merged)
	}
	if _, err := NormalizeSubagents(d["subagents"], out); err != nil {
		return errors.New("subagent_model_in_use")
	}
	d["models"] = out
	return nil
}

func MergeConnections(d Document, input []any) (map[string]*string, error) {
	out := make([]any, 0, len(input))
	keys := map[string]*string{}
	ids := map[string]bool{}
	allowed := map[string]bool{"id": true, "name": true, "providerPresetId": true, "baseURL": true, "apiKeyEnv": true, "protocols": true}
	for _, v := range input {
		row := Object(v)
		c := map[string]any{}
		for k, v := range row {
			if allowed[k] {
				c[k] = v
			} else if k != "apiKey" {
				return nil, errors.New("invalid_connections")
			}
		}
		id := String(c["id"])
		env := String(c["apiKeyEnv"])
		if env == "" {
			env = KeyEnv(id)
			c["apiKeyEnv"] = env
		}
		for _, previous := range Rows(d["connections"]) {
			old := Object(previous)
			if old["id"] == id && old["apiKeyEnv"] == env {
				if ref, exists := old["apiKeyRef"]; exists {
					c["apiKeyRef"] = ref
				}
				break
			}
		}
		if v, ok := row["apiKey"]; ok {
			if v == nil {
				keys[env] = nil
			} else {
				s, ok := v.(string)
				if !ok || strings.ContainsAny(s, "\r\n\x00") {
					return nil, errors.New("invalid_connections")
				}
				keys[env] = &s
			}
		}
		ids[id] = true
		out = append(out, c)
	}
	for _, v := range Rows(d["models"]) {
		if id := String(Object(v)["connectionId"]); id != "" && !ids[id] {
			return nil, errors.New("connection_in_use")
		}
	}
	d["connections"] = out
	return keys, nil
}

var errorPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,100}$`)

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if errorPattern.MatchString(s) {
		return s
	}
	return "operation_failed"
}

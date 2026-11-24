package core

import (
	"errors"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
	"net/url"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
var envReplace = regexp.MustCompile(`[^A-Z0-9]+`)
var modelFields = map[string]bool{}

// budgetFields is the writable set of the budget document. `prices` is not
// here because a price belongs to the model it prices, so it round-trips through
// the model row instead of being duplicated.
var budgetFields = map[string]bool{"enabled": true, "currency": true, "timeZone": true,
	"daily": true, "monthly": true, "serialize": true}

func init() {
	for _, k := range strings.Fields("id name connectionId upstreamModelId protocol upstreamProtocol contextWindow contextLimit maxOutputTokens tools reasoning inputModalities reasoningEfforts defaultEffort metadataSource failover pricing") {
		modelFields[k] = true
	}
}

// Integration modes. The gateway is what every release before Direct Mode ran,
// so a document that does not mention a mode keeps that behaviour.
const (
	IntegrationModeGateway = "gateway"
	IntegrationModeDirect  = "direct"
)

func String(v any) string { s, _ := v.(string); return s }

// IntegrationMode reads the configured mode. A missing, empty or unrecognised
// value is the gateway: an older document or a hand-edited file must not be
// able to opt into a different data path by accident.
func IntegrationMode(d Document) string {
	if String(d["integrationMode"]) == IntegrationModeDirect {
		return IntegrationModeDirect
	}
	return IntegrationModeGateway
}

// SetIntegrationMode writes the mode. Only the two known values are accepted;
// anything else is a caller mistake rather than a value to normalise away.
func SetIntegrationMode(d Document, mode string) error {
	if mode != IntegrationModeGateway && mode != IntegrationModeDirect {
		return errors.New("invalid_integration_mode")
	}
	d["integrationMode"] = mode
	return nil
}

// KimiK3CompatibilityKey is the stored opt-in that offers Chat models under a
// Kimi-compatible id, so an existing K3 conversation can be switched to them.
const KimiK3CompatibilityKey = "kimiK3Compatibility"

// KimiK3Compatibility reads the effective opt-in. It applies only in gateway
// mode: Direct Mode has no local catalog to rename, and a stored `true` left
// behind by a mode change must not leak into a path that cannot honour it. The
// gateway default is on, because every saved model is offered from the one
// `kimi` group; only a document that explicitly stores `false` turns it off,
// which is what the compatibility regression tests use.
func KimiK3Compatibility(d Document) bool {
	if IntegrationMode(d) != IntegrationModeGateway {
		return false
	}
	enabled, ok := d[KimiK3CompatibilityKey].(bool)
	if ok {
		return enabled
	}
	return true
}
func Rows(v any) []any { r, _ := v.([]any); return r }
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
	if raw, exists := d["integrationMode"]; exists && raw != nil {
		if mode := String(raw); mode != IntegrationModeGateway && mode != IntegrationModeDirect {
			return errors.New("invalid_integration_mode")
		}
	}
	if raw, exists := d[KimiK3CompatibilityKey]; exists && raw != nil {
		if _, ok := raw.(bool); !ok {
			return errors.New("invalid_kimi_k3_compatibility")
		}
	}
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
		for _, standby := range m.Failover {
			if connections[standby.ConnectionID] == nil {
				return errors.New("connection_unavailable")
			}
		}
	}
	if err := validateBudgetDocument(d["budget"]); err != nil {
		return err
	}
	if err := validateCompatibleModelIDs(models); err != nil {
		return err
	}
	return nil
}

// validateCompatibleModelIDs refuses a catalog where a saved model id is
// exactly the generated compatible id of a different model. The generated id is
// a fixed function of the logical id, so such a pair would make the Kimi
// catalog emit one key twice and would let one model shadow the other on the
// inference route. Every model is covered, because the compatible catalog keys
// all of them by that id. It is refused whether or not the opt-in is on,
// because turning it off must not silently reinterpret a name a saved alias
// already used.
func validateCompatibleModelIDs(models []protocol.Model) error {
	owner := map[string]string{}
	for _, model := range models {
		owner[protocol.KimiCompatibleModelID(model.ID)] = model.ID
	}
	for _, model := range models {
		if generatedBy, taken := owner[model.ID]; taken && generatedBy != model.ID {
			return errors.New("kimi_k3_model_id_conflict")
		}
	}
	return nil
}

// validateBudgetDocument checks the stored policy. The panel refuses an
// unenforceable policy before saving; a hand-edited file is refused here so the
// gateway never starts with a document it cannot read.
func validateBudgetDocument(raw any) error {
	if raw == nil {
		return nil
	}
	if _, err := protocol.ParseBudget(raw); err != nil {
		return errors.New("invalid_budget")
	}
	return nil
}

// MergeBudget replaces the budget document with the validated input. A nil or
// empty input removes it, which is how the panel turns budgeting off again.
func MergeBudget(d Document, input any) error {
	if input == nil {
		delete(d, "budget")
		return nil
	}
	if object, ok := input.(map[string]any); ok && len(object) > 0 {
		for key := range object {
			if !budgetFields[key] {
				return errors.New("invalid_budget")
			}
		}
	}
	normalized, err := NormalizeBudget(input)
	if err != nil {
		return err
	}
	if normalized == nil {
		delete(d, "budget")
		return nil
	}
	d["budget"] = normalized
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
		for _, id := range modelConnections(Object(v)) {
			if !ids[id] {
				return nil, errors.New("connection_in_use")
			}
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

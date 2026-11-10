package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"kimiworkplus.local/migration/internal/core"
)

func translationRevision(c core.TranslationConfig) string {
	b, _ := json.Marshal(c)
	var d core.Document
	_ = json.Unmarshal(b, &d)
	return core.Revision(d)
}
func (s *Service) translationKey(d core.Document, c core.TranslationConfig) string {
	for _, v := range core.Rows(d["connections"]) {
		row := core.Object(v)
		if row["apiKeyEnv"] == c.APIKeyEnv {
			key, _ := credentialFor(row, s.Keys)
			return key
		}
	}
	if s.Keys != nil {
		key, _ := s.Keys.Get(c.APIKeyEnv)
		return key
	}
	return ""
}
func (s *Service) saveTranslation(in core.Document) (any, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	d, rev, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	current, e := core.TranslationSettings(d["translation"])
	if e != nil {
		return nil, e
	}
	token, hasToken := in["translationRevision"]
	document, hasDocument := in["documentRevision"]
	if !hasDocument {
		document, hasDocument = in["revision"]
	}
	if hasDocument && document != rev {
		return nil, core.ErrConflict
	}
	if !hasDocument && !hasToken {
		return nil, errors.New("translation_settings_conflict")
	}
	if hasToken && token != translationRevision(current) {
		return nil, errors.New("translation_settings_conflict")
	}
	value := core.Document{}
	for k, v := range in {
		if k != "revision" && k != "documentRevision" && k != "translationRevision" {
			value[k] = v
		}
	}
	settings, e := core.TranslationSettings(value)
	if e != nil {
		return nil, e
	}
	b, _ := json.Marshal(settings)
	var normalized map[string]any
	_ = json.Unmarshal(b, &normalized)
	d, _, e = s.Store.Update(rev, func(d core.Document) error { d["translation"] = normalized; return nil })
	if e != nil {
		return nil, e
	}
	if s.OnConfig != nil {
		if e = s.OnConfig(d); e != nil {
			return nil, e
		}
	}
	state, e := s.State()
	return core.Document{"ok": true, "state": state}, e
}
func (s *Service) translationAction(ctx context.Context, path string, in core.Document) (any, error) {
	d, _, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	if path == "/api/translation/preview" {
		c, e := core.TranslationSettings(in["settings"])
		if e != nil {
			return nil, e
		}
		const original = "Open `src/app.js`, read https://example.test/docs and update config.json before the next step."
		translated, e := core.NewTranslator().Translate(ctx, original, c, s.translationKey(d, c), []string{"src/app.js", "https://example.test/docs", "config.json"})
		if e != nil {
			return nil, e
		}
		return core.Document{"preview": core.Document{"original": original, "translated": translated, "provider": c.Provider, "targetLanguage": c.TargetLanguage}}, nil
	}
	id := core.String(in["modelId"])
	var model, connection map[string]any
	for _, v := range core.Rows(d["models"]) {
		m := core.Object(v)
		if m["id"] == id {
			model = m
			break
		}
	}
	if model == nil {
		return nil, errors.New("translation_model_required")
	}
	for _, v := range core.Rows(d["connections"]) {
		c := core.Object(v)
		if c["id"] == model["connectionId"] {
			connection = c
			break
		}
	}
	if connection == nil {
		return nil, errors.New("translation_connection_required")
	}
	if model["upstreamProtocol"] == "anthropic" {
		return nil, errors.New("translation_connection_anthropic_only")
	}
	support := "model-protocol"
	if ps, ok := connection["protocols"].([]any); ok {
		support = "declared"
		chat := false
		for _, p := range ps {
			if p == "chat" {
				chat = true
			}
		}
		if !chat {
			return nil, errors.New("translation_connection_responses_only")
		}
	} else if model["protocol"] != "chat" {
		return nil, errors.New("translation_connection_chat_undeclared")
	}
	key, e := credentialFor(connection, s.Keys)
	if e != nil || key == "" {
		return nil, errors.New("translation_connection_key_missing")
	}
	base := core.String(connection["baseURL"])
	if strings.HasSuffix(strings.ToLower(base), "/chat/completions") || strings.HasSuffix(strings.ToLower(base), "/responses") {
		return nil, errors.New("translation_connection_endpoint_not_base")
	}
	settings, e := core.TranslationSettings(d["translation"])
	if e != nil {
		return nil, e
	}
	settings.Provider = "custom"
	settings.CustomEndpoint = base + "/chat/completions"
	settings.CustomModel = core.String(model["upstreamModelId"])
	if settings.CustomModel == "" {
		settings.CustomModel = id
	}
	settings.APIKeyEnv = core.String(connection["apiKeyEnv"])
	return core.Document{"settings": settings, "source": core.Document{"connectionId": connection["id"], "connectionName": connection["name"], "modelId": id, "upstreamModelId": settings.CustomModel, "baseURL": base, "chatSupport": support}, "keyAvailable": true}, nil
}

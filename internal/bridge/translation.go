package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func translationRevision(c core.TranslationConfig) string {
	b, _ := json.Marshal(c)
	var d core.Document
	_ = json.Unmarshal(b, &d)
	return core.Revision(d)
}

type publicTranslationConfig struct {
	core.TranslationConfig
	HasAPIKey    bool   `json:"hasApiKey"`
	APIKeySource string `json:"apiKeySource"`
}

func translationPublicConfig(c core.TranslationConfig, source string) publicTranslationConfig {
	c.APIKeyRef = ""
	return publicTranslationConfig{TranslationConfig: c, HasAPIKey: source != "", APIKeySource: source}
}

func (s *Service) translationState(d core.Document, c core.TranslationConfig, runtime any) core.Document {
	_, source := s.translationCredential(d, c)
	configured := translationPublicConfig(c, source)
	return core.Document{"configured": configured, "revision": translationRevision(c),
		"environment": core.Document{"keySet": configured.HasAPIKey}, "runtime": runtime}
}

func (s *Service) translationKey(d core.Document, c core.TranslationConfig) string {
	key, _ := s.translationCredential(d, c)
	return key
}

func (s *Service) translationCredential(d core.Document, c core.TranslationConfig) (string, string) {
	if s.Keys == nil {
		return "", ""
	}
	if c.APIKeyRef != "" {
		key, _ := s.Keys.Get(c.APIKeyRef)
		if key != "" {
			return key, "managed"
		}
		return "", ""
	}
	for _, v := range core.Rows(d["connections"]) {
		row := core.Object(v)
		if row["apiKeyEnv"] == c.APIKeyEnv {
			key, _ := credentialFor(row, s.Keys)
			if key != "" {
				return key, "connection"
			}
			return "", ""
		}
	}
	key, _ := s.Keys.Get(c.APIKeyEnv)
	if key != "" {
		return key, "environment"
	}
	return "", ""
}

func translationInput(in map[string]any) core.Document {
	value := core.Document{}
	for k, v := range in {
		switch k {
		case "revision", "documentRevision", "translationRevision", "apiKey", "apiKeyRef", "clearApiKey", "hasApiKey", "apiKeySource", "apiKeyMasked":
		default:
			value[k] = v
		}
	}
	return value
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
	clear, validClear := in["clearApiKey"].(bool)
	_, hasClear := in["clearApiKey"]
	supplied, _ := in["apiKey"].(string)
	if hasClear && !validClear || clear && supplied != "" {
		return nil, errors.New("invalid_translation_config")
	}
	value := translationInput(in)
	if env, exists := value["apiKeyEnv"]; !exists || env == "" {
		value["apiKeyEnv"] = current.APIKeyEnv
	}
	settings, e := core.TranslationSettings(value)
	if e != nil {
		return nil, e
	}
	if settings.APIKeyRef != "" {
		return nil, errors.New("invalid_translation_config")
	}
	if !clear && current.APIKeyEnv == settings.APIKeyEnv {
		settings.APIKeyRef = current.APIKeyRef
	}
	staged := ""
	if supplied != "" {
		if s.Keys == nil {
			return nil, errors.New("credentials_unavailable")
		}
		staged = "translation-" + RandomKey()
		if err := stageCredential(s.Keys, staged, supplied); err != nil {
			if deleteErr := s.Keys.Delete(staged); deleteErr != nil {
				return nil, errors.New("credentials_cleanup_failed")
			}
			return nil, err
		}
		settings.APIKeyRef = staged
	}
	b, _ := json.Marshal(settings)
	var normalized map[string]any
	_ = json.Unmarshal(b, &normalized)
	d["translation"] = normalized
	d, _, e = s.Store.Commit(d, rev)
	if e != nil {
		if staged != "" {
			s.discardStagedCredentials([]string{staged})
		}
		return nil, e
	}
	obsolete := []string{}
	if current.APIKeyRef != "" && current.APIKeyRef != settings.APIKeyRef {
		obsolete = append(obsolete, current.APIKeyRef)
	}
	clearedEnv := ""
	if clear {
		clearedEnv = current.APIKeyEnv
		if clearedEnv != "" {
			obsolete = append(obsolete, clearedEnv)
		}
	}
	s.discardUnreferencedCredentials(obsolete, clearedEnv)
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
		value := in["settings"]
		if settings := core.Object(value); settings != nil {
			value = translationInput(settings)
		}
		c, e := core.TranslationSettings(value)
		if e != nil {
			return nil, e
		}
		saved, e := core.TranslationSettings(d["translation"])
		if e != nil {
			return nil, e
		}
		c.APIKeyRef = ""
		ref := saved.APIKeyRef
		saved.APIKeyRef = ""
		if c == saved {
			c.APIKeyRef = ref
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
	// A connection that names a protocol decides whether it can answer a Chat
	// request at all. An empty declaration is a legacy row and falls back to the
	// model's own route, which is what it always did; claiming "declared" for an
	// empty list would report a Chat capability nobody stated.
	if ps, ok := connection["protocols"].([]any); ok && len(ps) > 0 {
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
	return core.Document{"settings": translationPublicConfig(settings, "connection"), "hasApiKey": true, "source": core.Document{"connectionId": connection["id"], "connectionName": connection["name"], "modelId": id, "upstreamModelId": settings.CustomModel, "baseURL": base, "chatSupport": support}, "keyAvailable": true}, nil
}

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

const translationKeyEnvDisabled = "translationKeyEnvDisabled"

func savedTranslationSettings(d core.Document) (core.TranslationConfig, error) {
	settings, err := core.TranslationSettings(d["translation"])
	settings.APIKeyEnvDisabled = d[translationKeyEnvDisabled] == true
	return settings, err
}

func translationRevision(c core.TranslationConfig) string {
	b, _ := json.Marshal(c)
	var d core.Document
	_ = json.Unmarshal(b, &d)
	if c.APIKeyEnvDisabled {
		d[translationKeyEnvDisabled] = true
	}
	return core.Revision(d)
}

type publicTranslationConfig struct {
	core.TranslationConfig
	HasAPIKey    bool   `json:"hasApiKey"`
	APIKeySource string `json:"apiKeySource"`
}

func translationPublicConfig(c core.TranslationConfig, source string) publicTranslationConfig {
	c.APIKeyRef = ""
	c.APIKeyEnvDisabled = false
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
	if c.APIKeyEnvDisabled {
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
	current, e := savedTranslationSettings(d)
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
	supplied, validKey := in["apiKey"].(string)
	if hasClear && !validClear || in["apiKey"] != nil && !validKey ||
		strings.ContainsAny(supplied, "\r\n\x00") || clear && supplied != "" {
		return nil, errors.New("invalid_translation_config")
	}
	value := translationInput(in)
	env, validEnv := value["apiKeyEnv"].(string)
	if value["apiKeyEnv"] != nil && !validEnv {
		return nil, errors.New("invalid_translation_config")
	}
	if env == "" && !clear {
		value["apiKeyEnv"] = current.APIKeyEnv
	}
	settings, e := core.TranslationSettings(value)
	if e != nil {
		return nil, e
	}
	if settings.APIKeyRef != "" {
		return nil, errors.New("invalid_translation_config")
	}
	if clear && env == "" {
		settings.APIKeyEnvDisabled = true
	} else if !clear && current.APIKeyEnv == settings.APIKeyEnv {
		settings.APIKeyRef = current.APIKeyRef
		settings.APIKeyEnvDisabled = current.APIKeyEnvDisabled
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
	if settings.APIKeyEnvDisabled {
		d[translationKeyEnvDisabled] = true
	} else {
		delete(d, translationKeyEnvDisabled)
	}
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
		obsolete = append(obsolete, current.APIKeyEnv)
		if env == "" && settings.APIKeyEnv != current.APIKeyEnv {
			obsolete = append(obsolete, settings.APIKeyEnv)
		}
		if env != "" && settings.APIKeyEnv == current.APIKeyEnv {
			clearedEnv = current.APIKeyEnv
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
func translationPreviewSettings(value any, d core.Document) (core.TranslationConfig, error) {
	saved, err := savedTranslationSettings(d)
	if err != nil || value == nil {
		return saved, err
	}
	draft := core.Object(value)
	if draft == nil {
		return core.TranslationSettings(value)
	}
	c, err := core.TranslationSettings(translationInput(draft))
	if err != nil {
		return c, err
	}
	c.APIKeyRef = ""
	sameService := c.Provider == saved.Provider && c.CustomEndpoint == saved.CustomEndpoint && c.CustomModel == saved.CustomModel
	if core.String(draft["apiKeyEnv"]) == "" {
		if !sameService {
			c.APIKeyEnvDisabled = true
			return c, nil
		}
		c.APIKeyEnv = saved.APIKeyEnv
	}
	if sameService && c.APIKeyEnv == saved.APIKeyEnv {
		c.APIKeyEnvDisabled = saved.APIKeyEnvDisabled
	}
	ref := saved.APIKeyRef
	saved.APIKeyRef = ""
	if c == saved {
		c.APIKeyRef = ref
	}
	return c, nil
}

func (s *Service) translationAction(ctx context.Context, path string, in core.Document) (any, error) {
	d, _, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	if path == "/api/translation/preview" {
		c, e := translationPreviewSettings(in["settings"], d)
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
	settings, e := savedTranslationSettings(d)
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

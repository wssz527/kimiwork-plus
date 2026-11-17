package kimi

import (
	"context"
	"path/filepath"
	"time"
)

type accountModelSurface interface {
	accountModelAliases(context.Context, accountPage) ([]string, error)
}

func (c *Controller) syncAccountModels(ctx context.Context, surface accountSurface, home, user string) (string, string) {
	c.mu.Lock()
	settings := c.settings
	c.mu.Unlock()
	if settings.GatewayURL == "" || len(settings.Models) == 0 {
		return "", ""
	}
	native, err := NewNativeIntegration(settings.StateDir, home, "gateway")
	if err != nil {
		return "failed", "invalid_integration_path"
	}
	intent, err := native.readIntent()
	if err != nil || intent == nil || !intent.Active || normalizeNativeURL(intent.URL) != normalizeNativeURL(settings.GatewayURL) {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	scope := sha256Hex(user)
	imported := false
	reason := "account_models_runtime_unverified"
	for attempt := 0; attempt < accountRuntimeRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "pending", reason
			case <-time.After(accountVerifyStep):
			}
		}
		if ctx.Err() != nil {
			return "pending", reason
		}
		if current, verified := accountPageUser(ctx, surface); !verified || current != user || !accountConfigMatches(home, user) {
			return "failed", NativeAccountChanged
		}
		aliases, pending, needsImport := native.accountCatalog(settings, scope)
		if needsImport && !imported {
			imported = true
			result, err := c.ImportModelsAuthorized(ctx)
			if err != nil || !result.OK {
				return "failed", firstNonEmpty(result.Reason, "account_models_import_failed")
			}
			if result.State == ImportStateAccountChanged {
				return "failed", NativeAccountChanged
			}
			reason = "native_profile_pending"
			continue
		}
		if pending != "" {
			reason = pending
			if !needsImport && pending != "native_profile_pending" && pending != "config_not_generated" {
				return "failed", pending
			}
			continue
		}
		reason = "account_models_runtime_unverified"
		if !accountRuntimeHasModels(ctx, surface, aliases) {
			continue
		}
		if current, verified := accountPageUser(ctx, surface); !verified || current != user || !accountConfigMatches(home, user) {
			return "failed", NativeAccountChanged
		}
		integration := c.reconcile(true, scope)
		if !integration.Applied || integration.Changed {
			reason = firstNonEmpty(integration.Reason, "account_models_not_verified")
			continue
		}
		if current, verified := accountPageUser(ctx, surface); !verified || current != user || !accountConfigMatches(home, user) {
			return "failed", NativeAccountChanged
		}
		c.mu.Lock()
		c.loadedKnown, c.loadedGeneration = true, integration.Readback.Generation
		integration.Adopted = true
		c.integration = &integration
		c.mu.Unlock()
		return "ready", ""
	}
	return "pending", reason
}

func accountRuntimeHasModels(ctx context.Context, surface accountSurface, want []string) bool {
	models, ok := surface.(accountModelSurface)
	if !ok {
		return false
	}
	pages, err := surface.accountPages(ctx)
	if err != nil {
		return false
	}
	for _, page := range pages {
		if page.credentials {
			continue
		}
		aliases, err := models.accountModelAliases(ctx, page)
		if err != nil {
			continue
		}
		all := true
		for _, alias := range want {
			if !containsString(aliases, alias) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// Only the target account's exact profile can establish that an import is reusable.
func (n *NativeIntegration) accountCatalog(settings ControllerSettings, scope string) ([]string, string, bool) {
	store, storeErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr != "" {
		return nil, "profile_store_" + storeErr, storeErr == "absent"
	}
	var profile map[string]any
	for _, candidate := range kthProfiles(store) {
		if candidate.key == scope {
			profile = candidate.profile
			break
		}
	}
	if profile == nil {
		return nil, "profile_missing", true
	}
	if pending, present := profile["pending"]; present && pending != nil {
		return nil, "native_profile_pending", false
	}
	if delisted, _ := profile["delisted"].(bool); delisted {
		return nil, "profile_delisted", true
	}
	target := normalizeNativeURL(settings.GatewayURL)
	if normalizeNativeURL(stringField(profile, "baseUrl")) != target {
		return nil, "foreign_url", false
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr != "" {
		return nil, "config_" + configErr, false
	}
	credentialRef := stringField(profile, "credentialRef")
	credential, _ := section(config, "credentials")[credentialRef].(map[string]any)
	if credentialRef == "" || settings.GatewayToken == "" || stringField(credential, "apiKey") != settings.GatewayToken {
		return nil, "credential_changed", true
	}
	catalog, ok := normalizeCatalog(settings.Models)
	if !ok {
		return nil, "invalid_request", false
	}
	providers := section(section(config, "model"), "providers")
	models := section(section(config, "model"), "models")
	rows := profileRows(profile)
	aliases := make([]string, 0, len(catalog))
	for _, entry := range catalog {
		family := "responses"
		if entry.protocol == "chat" {
			family = "kimi"
		}
		row, found := findRow(rows, entry.id, family)
		if !found || rowFamily(row) != family {
			return nil, "models_missing", true
		}
		alias, providerID := stringField(row, "alias"), stringField(row, "providerId")
		provider, _ := providers[providerID].(map[string]any)
		model, _ := models[alias].(map[string]any)
		if provider == nil || model == nil {
			return nil, "config_not_generated", false
		}
		if !providerMatches(provider, map[string]any{"type": providerType(row), "baseUrl": target, "credential": credentialRef}) {
			return nil, "foreign_provider", false
		}
		if !modelMatches(model, modelEntry(row)) {
			return nil, "foreign_model", false
		}
		if containsString(aliases, alias) {
			return nil, "profile_invalid", false
		}
		aliases = append(aliases, alias)
	}
	return aliases, "", false
}

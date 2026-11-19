package kimi

import (
	"context"
	"path/filepath"
	"time"
)

type accountModelSurface interface {
	accountModelConfig(context.Context, accountPage) (accountModelConfig, error)
}

// accountModelConfig is what one page reports about the loaded catalog. The two
// store generations answer in the same two sections; `credentialPublished` says
// whether the providers carry a credential reference at all, because the 3.2.28
// surface reports only whether a key is configured.
type accountModelConfig struct {
	providers           map[string]any
	models              map[string]any
	credentialPublished bool
}

type accountCatalogEntry struct {
	alias    string
	provider map[string]any
	model    map[string]any
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
		catalog, pending, needsImport := native.accountCatalog(settings, scope)
		if needsImport && !imported {
			imported = true
			runtime, page, watching := watchAccountRuntime(ctx, surface)
			if watching {
				defer runtime.accountStopRuntimeWatch(context.WithoutCancel(ctx), page)
			}
			result, err := c.ImportModelsAuthorized(ctx)
			if err != nil || !result.OK {
				return "failed", firstNonEmpty(result.Reason, "account_models_import_failed")
			}
			if result.State == ImportStateAccountChanged {
				return "failed", NativeAccountChanged
			}
			if result.RequiresRestart && (!watching || !waitAccountRuntime(ctx, surface, runtime, page, home, user, true)) {
				return "pending", "account_models_runtime_unverified"
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
		if !accountPageHasModels(ctx, surface, catalog) {
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

func (c *Controller) confirmRunningCatalog(ctx context.Context, result IntegrationResult) IntegrationResult {
	c.mu.Lock()
	child, bridge, settings := c.child, c.bridge, c.settings
	c.mu.Unlock()
	if child == nil || bridge == nil || result.Readback.Generation <= 0 {
		return result
	}
	surface, err := c.accountSurface()
	if err != nil {
		return result
	}
	home := c.kimiHome()
	user, verified := accountPageUser(ctx, surface)
	if !verified || sha256Hex(user) != result.Readback.AccountDigest || !accountConfigMatches(home, user) {
		return result
	}
	native, err := NewNativeIntegration(settings.StateDir, home, "gateway")
	if err != nil {
		return result
	}
	catalog, reason, _ := native.accountCatalog(settings, result.Readback.AccountDigest)
	if reason != "" || len(catalog) == 0 || !accountPageHasModels(ctx, surface, catalog) {
		return result
	}
	runtime, page, ok := accountRuntimeTarget(ctx, surface)
	if !ok {
		return result
	}
	ready, err := runtime.accountRuntimeReady(ctx, page, false)
	_ = runtime.accountStopRuntimeWatch(ctx, page)
	if err != nil || !ready {
		return result
	}
	current, verified := accountPageUser(ctx, surface)
	if !verified || current != user || !accountConfigMatches(home, user) || c.currentGeneration() != result.Readback.Generation {
		return result
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.child != child || c.bridge != bridge || c.integration == nil || c.integration.Readback != result.Readback {
		return result
	}
	select {
	case <-child.Exit():
		return result
	default:
	}
	c.loadedKnown, c.loadedGeneration = true, result.Readback.Generation
	result.Adopted = true
	c.integration = &result
	return result
}

func accountPageHasModels(ctx context.Context, surface accountSurface, want []accountCatalogEntry) bool {
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
		config, err := models.accountModelConfig(ctx, page)
		if err != nil {
			continue
		}
		providers, entries := config.providers, config.models
		matches := providerMatches
		if !config.credentialPublished {
			matches = providerMatchesPublished
		}
		all := true
		for _, entry := range want {
			provider, _ := providers[stringField(entry.model, "provider")].(map[string]any)
			model, _ := entries[entry.alias].(map[string]any)
			if !matches(provider, entry.provider) || !modelMatches(model, entry.model) {
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
func (n *NativeIntegration) accountCatalog(settings ControllerSettings, scope string) ([]accountCatalogEntry, string, bool) {
	store, storeErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr != "" {
		return nil, "profile_store_" + storeErr, storeErr == "absent"
	}
	// Both store generations are keyed by the account key, which is already what
	// `scope` answers; comparing them directly is what makes a v3 `accounts` key
	// and a legacy `profiles` key select the same way.
	var profile map[string]any
	var account *nativeAccountV3
	for _, candidate := range kthProfiles(store) {
		if candidate.key == scope {
			profile, account = candidate.profile, candidate.v3
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
	if account != nil {
		// A v3 account states a base URL per provider, so every provider this
		// account holds must already point at the gateway.
		for _, value := range account.baseURLs {
			if value != target {
				return nil, "foreign_url", false
			}
		}
	} else if normalizeNativeURL(stringField(profile, "baseUrl")) != target {
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
	entries := make([]accountCatalogEntry, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
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
		wantProvider := desiredProviderTerms(target, credentialRef, providerID, providerType(row), account)
		wantModel := modelEntry(row)
		if !providerMatches(provider, wantProvider) {
			return nil, "foreign_provider", false
		}
		if !modelMatches(model, wantModel) {
			return nil, "foreign_model", false
		}
		if seen[alias] {
			return nil, "profile_invalid", false
		}
		seen[alias] = true
		entries = append(entries, accountCatalogEntry{alias: alias, provider: wantProvider, model: wantModel})
	}
	return entries, "", false
}

package kimi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Installations upgraded from the earlier Node prototype carry providers named
// `byok-kwp-*` in the generated config.json. They point at loopback ports of a
// gateway that no longer runs, and because the official store never lists them
// in `ownedResources.providerIds`, Kimi's own generator has no reason to remove
// them. They stay behind as dead providers and dead models.
//
// This cleanup removes exactly those entries and nothing else: a provider must
// be named `byok-kwp-*`, must not be owned by the official store, and must point
// at a loopback /v1 endpoint. Every write is a compare-and-swap against the
// bytes that were read, so a concurrent generator wins and this call reports it
// instead of overwriting.

// CleanupOrphanedLegacyEntries removes the orphaned legacy providers, their
// models and their now-unreferenced credentials from `config.json`.
//
// It acts only on a 3.2.28 store: a missing store, a legacy `profiles` store or
// a store without readable `ownedResources` is left alone, because without
// ownership data there is no safe way to decide what is orphaned. It answers
// true only when it actually removed something.
func CleanupOrphanedLegacyEntries(kimiDir string) (bool, error) {
	if kimiDir == "" || !filepath.IsAbs(kimiDir) {
		return false, errors.New("invalid_integration_path")
	}
	store, storeErr := readJSONFile(filepath.Join(kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr != "" {
		return false, nil
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		return false, nil
	}
	owned, ok := store["ownedResources"].(map[string]any)
	if !ok {
		return false, nil
	}
	ownedProviders := stringSet(owned["providerIds"])
	ownedCredentials := stringSet(owned["credentialRefs"])

	file := filepath.Join(kimiDir, NativeWorkConfig)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maxConfigBytes {
		return false, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, nil
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil || config == nil {
		return false, errors.New("config_corrupt")
	}
	model, ok := config["model"].(map[string]any)
	if !ok {
		return false, nil
	}
	providers, ok := model["providers"].(map[string]any)
	if !ok || len(providers) == 0 {
		return false, nil
	}

	removed := map[string]bool{}
	orphanedCredentials := map[string]bool{}
	for providerID, row := range providers {
		if !strings.HasPrefix(providerID, "byok-kwp-") || ownedProviders[providerID] {
			continue
		}
		provider, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if !isLoopbackGateway(providerBaseURL(provider)) {
			continue
		}
		removed[providerID] = true
		for _, ref := range providerCredentialRefs(provider) {
			orphanedCredentials[ref] = true
		}
	}
	if len(removed) == 0 {
		return false, nil
	}

	for providerID := range removed {
		delete(providers, providerID)
	}
	if models, ok := model["models"].(map[string]any); ok {
		for alias, row := range models {
			entry, ok := row.(map[string]any)
			if ok && removed[stringField(entry, "provider")] {
				delete(models, alias)
			}
		}
	}
	if credentials, ok := config["credentials"].(map[string]any); ok {
		referenced := map[string]bool{}
		for _, row := range providers {
			if provider, ok := row.(map[string]any); ok {
				for _, ref := range providerCredentialRefs(provider) {
					referenced[ref] = true
				}
			}
		}
		for ref := range orphanedCredentials {
			if ownedCredentials[ref] || referenced[ref] {
				continue
			}
			if !strings.HasPrefix(ref, "byok-") && !strings.HasPrefix(ref, "kwp-") {
				continue
			}
			if _, present := credentials[ref]; !present {
				continue
			}
			delete(credentials, ref)
		}
	}
	if err := commitConfigFile(kimiDir, string(raw), config); err != nil {
		return false, err
	}
	return true, nil
}

func providerBaseURL(provider map[string]any) string {
	return firstNonEmpty(stringField(provider, "baseUrl"), stringField(provider, "base_url"))
}

// providerCredentialRefs reads the reference a generated provider declares. Both
// spellings are accepted because the generated shape moved between builds.
func providerCredentialRefs(provider map[string]any) []string {
	var refs []string
	for _, name := range []string{"credential", "credentialRef"} {
		if ref := stringField(provider, name); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func stringSet(value any) map[string]bool {
	rows, ok := value.([]any)
	if !ok {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(rows))
	for _, row := range rows {
		if text, ok := row.(string); ok && text != "" {
			set[text] = true
		}
	}
	return set
}

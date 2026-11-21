package kimi

import (
	"os"
	"path/filepath"
	"sort"
)

// Readback is the evidence half of an import. It answers what the profile and
// the generated config hold right now, which is the only thing that may turn a
// page's `ok` into "applied".
//
// It carries no token and no private path: the profile and the Kimi home are
// reported as digests, so a renderer or a log can compare them across calls
// without ever learning where the user's data lives.

// ImportReadback is what the readback found.
type ImportReadback struct {
	// ProfileDigest identifies the Kimi home the readback used, so two calls can
	// be proven to be about the same directory without naming it.
	ProfileDigest string `json:"profileDigest,omitempty"`
	// ProfilePresent is true when that directory exists.
	ProfilePresent bool `json:"profilePresent"`
	// AccountDigest is the account identity read from the profile, or empty when
	// the profile has not signed in.
	AccountDigest string `json:"accountDigest,omitempty"`
	// AccountVerified is true only when an identity was actually read. An empty
	// digest with this false is `waiting-login`, never "no account needed".
	AccountVerified bool `json:"accountVerified"`
	// StorePresent and ConfigPresent report the two files Kimi owns.
	StorePresent  bool `json:"storePresent"`
	ConfigPresent bool `json:"configPresent"`
	// URL is the profile's own base URL, normalised. It is a loopback gateway
	// address, not a secret.
	URL string `json:"url,omitempty"`
	// CredentialRef is the name of the credential the profile points at. It is a
	// reference, never the key.
	CredentialRef string `json:"credentialRef,omitempty"`
	// Models is how many model rows the profile holds for this tool.
	Models int `json:"models,omitempty"`
	// Aliases are those model aliases, sorted.
	Aliases []string `json:"aliases,omitempty"`
	// Generation is the intent generation, i.e. how many times this tool has
	// written the generated config for this profile.
	Generation int `json:"generation,omitempty"`
	// Active is true while the intent still owns the entries in the config.
	Active bool `json:"active"`
	// Mode is the integration mode this evidence belongs to. Empty means the
	// gateway, which is what a readback from an older caller means.
	Mode string `json:"mode,omitempty"`
	// DirectSourceID is the managed custom source Direct Mode verified.
	DirectSourceID string `json:"directSourceId,omitempty"`
	// ModelAliases maps a Direct Mode logical model id to the alias Kimi gave it.
	// The aliases themselves are what `Aliases` lists; this is the mapping a
	// caller needs to recognise its own models in the runtime.
	ModelAliases map[string]string `json:"modelAliases,omitempty"`
	// Reason explains an unreadable profile; empty when the read succeeded.
	Reason string `json:"reason,omitempty"`
}

// Readback reads the profile and the generated config without writing anything.
//
// `account` is the account scope as `AccountScope` answers it, not a raw
// identity. Profiles in Kimi's store are keyed by that same value, one hash
// below the scope digest `Reconcile` compares, so this call derives the digest
// exactly the way `Reconcile` does and both agree on which profile is whose.
//
// It is deliberately a separate call from Reconcile: reading is safe at any
// time, while reconciling may write. A caller that only wants evidence — a
// status panel, a verification step after an import — uses this.
func (n *NativeIntegration) Readback(account string) ImportReadback {
	out := ImportReadback{ProfileDigest: sha256Hex(n.kimiDir)}
	if account != "" {
		out.AccountDigest = account
		out.AccountVerified = true
	}
	if info, err := os.Lstat(n.kimiDir); err != nil || !info.IsDir() {
		out.Reason = "profile_missing"
		return out
	}
	out.ProfilePresent = true

	if _, err := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes); err == "" {
		out.StorePresent = true
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "" && config != nil {
		out.ConfigPresent = true
	}

	intent, intentErr := n.readIntent()
	if intentErr == nil && intent != nil {
		out.Generation = intent.Generation
		out.Active = intent.Active
		if out.URL == "" {
			out.URL = normalizeNativeURL(intent.URL)
		}
	}
	if !out.ConfigPresent {
		out.Reason = firstNonEmpty(out.Reason, "config_"+configErr)
	}

	// The profile's own row is the authority for the URL, the credential
	// reference and the model set; the intent only supplies the generation.
	if out.StorePresent {
		store, err := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
		if err == "" {
			// `account` is the scope, and the store keys profiles one hash below
			// it: the selector wants the same level `Reconcile` computes, or it
			// can never match the caller's own profile.
			scopeDigest := ""
			if out.AccountDigest != "" {
				scopeDigest = sha256Hex(out.AccountDigest)
			}
			selected, ambiguous := selectProfile(kthProfiles(store), scopeDigest, intent)
			if !ambiguous && selected.profile != nil {
				if value := normalizeNativeURL(stringField(selected.profile, "baseUrl")); value != "" {
					out.URL = value
				}
				out.CredentialRef = stringField(selected.profile, "credentialRef")
				aliases := make([]string, 0, 8)
				for _, row := range profileRows(selected.profile) {
					aliases = append(aliases, stringField(row, "alias"))
				}
				sort.Strings(aliases)
				out.Aliases = aliases
				out.Models = len(aliases)
			}
		}
	}
	return out
}

// ReadbackMode reads the evidence for one integration mode. Direct Mode reports
// the managed custom source instead of the kth profile: it names the source, its
// usable models, their aliases and the credential references they point at, and
// still carries no key.
func (n *NativeIntegration) ReadbackMode(account, mode, directSourceID string) ImportReadback {
	if mode != IntegrationModeDirect {
		return n.Readback(account)
	}
	out := ImportReadback{ProfileDigest: sha256Hex(n.kimiDir), Mode: IntegrationModeDirect}
	if account != "" {
		out.AccountDigest = account
		out.AccountVerified = true
	}
	if info, err := os.Lstat(n.kimiDir); err != nil || !info.IsDir() {
		out.Reason = "profile_missing"
		return out
	}
	out.ProfilePresent = true
	store, storeErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
	if storeErr == "" {
		out.StorePresent = true
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "" && config != nil {
		out.ConfigPresent = true
	}
	intent, intentErr := n.readIntent()
	if intentErr == nil && intent != nil {
		out.Generation = intent.Generation
		out.Active = intent.Active
		if directSourceID == "" {
			directSourceID = intent.DirectSourceID
		}
		if intent.Mode != IntegrationModeDirect {
			out.ModelAliases = nil
		}
	}
	if !out.ConfigPresent {
		out.Reason = firstNonEmpty(out.Reason, "config_"+configErr)
	}
	if storeErr != "" || store == nil {
		return out
	}
	if _, ok := store["accounts"].(map[string]any); !ok {
		out.Reason = firstNonEmpty(out.Reason, "direct_mode_unsupported")
		return out
	}
	scopeDigest := ""
	if account != "" {
		scopeDigest = sha256Hex(account)
	}
	chosen, ambiguous := selectDirectAccount(directAccountCandidates(store), scopeDigest, intent)
	if ambiguous {
		out.Reason = firstNonEmpty(out.Reason, "account_ambiguous")
		return out
	}
	if chosen.account == nil {
		out.Reason = firstNonEmpty(out.Reason, "profile_missing")
		return out
	}
	source, found := directSourceOf(chosen.sources, firstNonEmpty(directSourceID, intentDirectSource(intent)))
	if !found {
		out.Reason = firstNonEmpty(out.Reason, "profile_missing")
		return out
	}
	out.DirectSourceID = stringField(source, "id")
	if source["quarantined"] == true {
		out.Active = false
		out.Reason = firstNonEmpty(out.Reason, "profile_delisted")
		return out
	}
	facts, rows := directActiveRows(chosen.account, out.DirectSourceID)
	aliases := make([]string, 0, len(rows))
	modelAliases := map[string]string{}
	credentialRef := ""
	for _, row := range rows {
		alias := stringField(row, "alias")
		aliases = append(aliases, alias)
		if provider, ok := facts[stringField(row, "providerId")]; ok {
			if credentialRef == "" {
				credentialRef = provider.credential
			}
			if intent != nil && intent.Mode == IntegrationModeDirect {
				for logical, recorded := range intent.Aliases {
					if recorded == alias {
						modelAliases[logical] = alias
					}
				}
			}
		}
	}
	sort.Strings(aliases)
	out.Aliases = aliases
	out.Models = len(aliases)
	out.CredentialRef = credentialRef
	if len(modelAliases) > 0 {
		out.ModelAliases = modelAliases
	}
	return out
}

// HoldsDirect reports whether a Direct Mode readback found the managed source
// with every model the spec declares. As with Holds, it says nothing about
// whether the running application has loaded it.
func (r ImportReadback) HoldsDirect(spec *DirectSpec) bool {
	if r.Mode != IntegrationModeDirect || r.DirectSourceID == "" ||
		!r.ProfilePresent || !r.StorePresent || !r.ConfigPresent ||
		r.CredentialRef == "" || r.Models == 0 {
		return false
	}
	if spec == nil || len(spec.Models) == 0 {
		return false
	}
	for _, model := range spec.Models {
		if r.ModelAliases[model.ID] == "" {
			return false
		}
	}
	return true
}

// Holds reports whether the readback found a usable model set at `url` with a
// generated credential. It is the applied condition, and it says nothing about
// whether the running application has loaded it.
func (r ImportReadback) Holds(url string) bool {
	if !r.ProfilePresent || !r.StorePresent || !r.ConfigPresent || r.CredentialRef == "" {
		return false
	}
	if r.Models == 0 {
		return false
	}
	wanted := normalizeNativeURL(url)
	return wanted != "" && r.URL == wanted
}

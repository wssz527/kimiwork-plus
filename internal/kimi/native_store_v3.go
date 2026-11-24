package kimi

import (
	"path/filepath"
	"sort"
	"time"
)

// The 3.2.28 store keeps BYOK state per account under `accounts`, while the
// store this package was written against keeps it under `profiles`. Both are
// keyed by the same account key (sha256 of the signed-in user id), so selection
// is unchanged; only the shape below the key differs.
//
// A v3 account is
//
//	revision, sources[], active{rev,providers[],models[]},
//	pending{rev,providers[],models[],applyState}, blockedAliases{alias:status}
//
// `active` is the applied set. `pending` survives until the running application
// confirms the apply, so its presence alone does not mean the models are
// unusable: only a pending model that is not active yet is a transaction in
// flight. There is no account-wide `baseUrl` or `credentialRef`; both live on
// each provider, and one source may hold several providers with different
// credential references.

type nativeProviderV3 struct {
	sourceID   string
	protocol   string
	baseURL    string
	credential string
}

type nativeAccountV3 struct {
	providers     map[string]nativeProviderV3
	refs          []string
	baseURLs      []string
	pending       bool
	quarantined   bool
	directCleanup bool
}

// accountProfiles reads every account the v3 store holds. Only accounts that
// carry a kth source are candidates: another source kind is somebody else's
// profile and is never substituted.
func accountProfiles(store map[string]any) []profileCandidate {
	accounts, ok := store["accounts"].(map[string]any)
	if !ok || len(accounts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(accounts))
	for key := range accounts {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var found []profileCandidate
	for _, key := range keys {
		account, ok := accounts[key].(map[string]any)
		if !ok {
			continue
		}
		source, ok := kthSourceOf(account)
		if !ok {
			continue
		}
		if candidate, ok := accountProfile(key, account, source); ok {
			found = append(found, candidate)
		}
	}
	return found
}

func kthSourceOf(account map[string]any) (map[string]any, bool) {
	sources, _ := account["sources"].([]any)
	var fallback map[string]any
	for _, row := range sources {
		source, ok := row.(map[string]any)
		if !ok {
			continue
		}
		id, kind := stringField(source, "id"), stringField(source, "kind")
		if id != "byok-kth" && kind != "kth" {
			continue
		}
		if id == "byok-kth" {
			return source, true
		}
		if fallback == nil {
			fallback = source
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

func accountProfile(key string, account, source map[string]any) (profileCandidate, bool) {
	active, _ := account["active"].(map[string]any)
	pending, _ := account["pending"].(map[string]any)
	sourceID := stringField(source, "id")
	facts := providerFacts(active, pending)
	rows := usableAccountRows(account, active, sourceID)
	if len(facts) == 0 || len(rows) == 0 {
		return profileCandidate{}, false
	}
	usedFacts := map[string]nativeProviderV3{}
	normalized := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		providerID := stringField(row, "providerId")
		fact, ok := facts[providerID]
		if !ok {
			continue
		}
		usedFacts[providerID] = fact
		normalized = append(normalized, normalizeAccountRow(row, fact))
	}
	if len(usedFacts) == 0 || len(normalized) == 0 {
		return profileCandidate{}, false
	}
	// A pending model that is not applied yet is the transaction in flight the
	// legacy shape reports through `profile.pending`.
	applied := map[string]bool{}
	for _, row := range normalized {
		applied[stringField(row, "alias")] = true
	}
	blocking := false
	for _, row := range modelRows(pending) {
		if rowSource := stringField(row, "sourceId"); sourceID != "" && rowSource != "" && rowSource != sourceID {
			continue
		}
		if alias := stringField(row, "alias"); alias != "" && !applied[alias] {
			blocking = true
			break
		}
	}
	account3 := &nativeAccountV3{providers: usedFacts, pending: blocking,
		quarantined:   source["quarantined"] == true || stringField(source, "status") == "disconnected",
		directCleanup: directSourceRemainsInAccount(account, "") != ""}
	seen := map[string]bool{}
	for _, fact := range usedFacts {
		if fact.credential != "" && !seen[fact.credential] {
			seen[fact.credential] = true
			account3.refs = append(account3.refs, fact.credential)
		}
		if value := normalizeNativeURL(fact.baseURL); value != "" {
			account3.baseURLs = append(account3.baseURLs, value)
		}
	}
	sort.Strings(account3.refs)
	sort.Strings(account3.baseURLs)

	profile := map[string]any{"kind": "kth", "id": "byok-kth", "models": rowsAsAny(normalized)}
	if value := singleValue(account3.baseURLs); value != "" {
		profile["baseUrl"] = value
	}
	if len(account3.refs) > 0 {
		profile["credentialRef"] = account3.refs[0]
	}
	if blocking {
		profile["pending"] = map[string]any{"rev": stringField(pending, "rev")}
	}
	if account3.quarantined {
		profile["delisted"] = true
	}
	return profileCandidate{key: key, profile: profile, v3: account3}, true
}

func normalizeAccountRow(row map[string]any, fact nativeProviderV3) map[string]any {
	family := ""
	switch fact.protocol {
	case "kimi":
		family = "kimi"
	case "openai_responses":
		family = "responses"
	default:
		return row
	}
	if stringField(row, "family") == family {
		return row
	}
	copyRow := make(map[string]any, len(row)+1)
	for k, v := range row {
		copyRow[k] = v
	}
	copyRow["family"] = family
	return copyRow
}

// usableAccountRows is the applied model set minus the aliases the account
// blocks. `active` alone is not enough: a blocked alias keeps its status even
// while an entry for it is still present.
func usableAccountRows(account, active map[string]any, sourceID string) []map[string]any {
	blocked, _ := account["blockedAliases"].(map[string]any)
	seen := map[string]bool{}
	var out []map[string]any
	for _, row := range modelRows(active) {
		if rowSource := stringField(row, "sourceId"); sourceID != "" && rowSource != "" && rowSource != sourceID {
			continue
		}
		alias := stringField(row, "alias")
		if alias == "" || seen[alias] {
			continue
		}
		if status := stringField(blocked, alias); status != "" && status != "available" {
			continue
		}
		seen[alias] = true
		out = append(out, row)
	}
	return out
}

func modelRows(section map[string]any) []map[string]any {
	if section == nil {
		return nil
	}
	rows, _ := section["models"].([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if entry, ok := row.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

func rowsAsAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func providerFacts(active, pending map[string]any) map[string]nativeProviderV3 {
	facts := map[string]nativeProviderV3{}
	for _, section := range []map[string]any{active, pending} {
		if section == nil {
			continue
		}
		rows, _ := section["providers"].([]any)
		for _, row := range rows {
			provider, ok := row.(map[string]any)
			if !ok {
				continue
			}
			id := stringField(provider, "id")
			if id == "" {
				continue
			}
			if _, seen := facts[id]; seen {
				continue
			}
			facts[id] = nativeProviderV3{sourceID: stringField(provider, "sourceId"), protocol: stringField(provider, "protocol"),
				baseURL: stringField(provider, "baseUrl"), credential: stringField(provider, "credentialRef")}
		}
	}
	return facts
}

func singleValue(values []string) string {
	var unique []string
	for _, value := range values {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	if len(unique) == 1 {
		return unique[0]
	}
	return ""
}

// desiredProviderTerms is the declaration this package expects for one provider
// of the selected profile. A v3 account states it per provider; the legacy
// profile states it once for the whole profile.
func desiredProviderTerms(target, credentialRef, providerID, kind string, account *nativeAccountV3) map[string]any {
	declaration := map[string]any{"type": kind, "baseUrl": target, "credential": credentialRef}
	if account == nil {
		return declaration
	}
	provider, ok := account.providers[providerID]
	if !ok {
		return declaration
	}
	if provider.protocol != "" {
		declaration["type"] = provider.protocol
	}
	if value := normalizeNativeURL(provider.baseURL); value != "" {
		declaration["baseUrl"] = value
	}
	if provider.credential != "" {
		declaration["credential"] = provider.credential
	}
	return declaration
}

// verifyAccount checks a 3.2.28 account against the generated config.
//
// The official application generates `config.json` from the store it owns, so
// this package verifies what it generated instead of writing over it: an entry
// that is missing is still being generated, and one that differs belongs to
// someone else. The intent is still recorded, because it is this tool's own
// record of what it verified for which account.
func (n *NativeIntegration) verifyAccount(account profileCandidate, target string,
	catalog []catalogEntry, intent *nativeIntent, scopeDigest string) IntegrationResult {
	if len(account.v3.baseURLs) > 0 {
		foreign := false
		for _, value := range account.v3.baseURLs {
			if value != target {
				foreign = true
				break
			}
		}
		if foreign {
			// A profile this tool last imported that now points elsewhere is drift
			// it can repair; one it never imported belongs to someone else.
			if intent != nil && intent.Active && normalizeNativeURL(intent.URL) != "" {
				return IntegrationResult{State: IntegrationNeedsImport, Reason: "base_url_changed"}
			}
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "foreign_url"}
		}
	}
	if len(account.v3.refs) == 0 {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "credential_missing"}
	}

	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "absent" {
		return IntegrationResult{State: IntegrationWaitingConfig, Reason: "config_missing"}
	}
	if configErr != "" {
		return IntegrationResult{State: IntegrationConflict, Reason: "config_" + configErr}
	}
	modelSection, ok := config["model"].(map[string]any)
	if !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_config"}
	}
	credentials, ok := config["credentials"].(map[string]any)
	if !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_config"}
	}
	credentialRef := ""
	for _, ref := range account.v3.refs {
		credential, ok := credentials[ref].(map[string]any)
		if !ok || stringField(credential, "apiKey") == "" {
			return IntegrationResult{State: IntegrationWaitingConfig, Reason: "credential_not_generated",
				CredentialRef: ref}
		}
		if credentialRef == "" {
			credentialRef = ref
		}
	}

	rows := profileRows(account.profile)
	matched := make([]matchedRow, 0, len(catalog))
	missing := make([]string, 0)
	for _, entry := range catalog {
		wanted := "responses"
		if entry.protocol == "chat" {
			wanted = "kimi"
		}
		row, found := findRow(rows, entry.modelID, wanted)
		if found {
			matched = append(matched, matchedRow{catalog: entry, row: row})
		} else {
			missing = append(missing, entry.id)
		}
	}
	if len(missing) > 0 {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "models_missing", MissingModels: missing}
	}
	aliases := map[string]string{}
	providerTypes := map[string]string{}
	for _, entry := range matched {
		providerID, kind := stringField(entry.row, "providerId"), providerType(entry.row)
		if existing, seen := providerTypes[providerID]; seen && existing != kind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_invalid"}
		}
		providerTypes[providerID] = kind
		aliases[entry.catalog.id] = stringField(entry.row, "alias")
	}
	if hasDuplicateValues(aliases) {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_invalid"}
	}

	existingProviders := section(modelSection, "providers")
	existingModels := section(modelSection, "models")
	entries := ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}}
	for providerID, kind := range providerTypes {
		desired := desiredProviderTerms(target, credentialRef, providerID, kind, account.v3)
		current, present := existingProviders[providerID]
		object, ok := current.(map[string]any)
		if !present || !ok {
			return IntegrationResult{State: IntegrationWaitingConfig, Reason: "config_not_generated"}
		}
		if !providerMatches(object, desired) {
			return IntegrationResult{State: IntegrationConflict, Reason: "foreign_provider"}
		}
		entries.Providers[providerID] = ownedEntry{Digest: digestOf(current)}
	}
	for _, entry := range matched {
		alias := stringField(entry.row, "alias")
		current, present := existingModels[alias]
		object, ok := current.(map[string]any)
		if !present || !ok {
			return IntegrationResult{State: IntegrationWaitingConfig, Reason: "config_not_generated"}
		}
		if !modelMatches(object, modelEntry(entry.row)) {
			return IntegrationResult{State: IntegrationConflict, Reason: "foreign_model"}
		}
		entries.Models[alias] = ownedEntry{Digest: digestOf(current)}
	}

	sameAccount := scopeDigest != "" && intent != nil && intent.AccountDigest == scopeDigest
	next := &nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: n.sourceID, OwnerID: n.ownerID,
		KimiHome: n.kimiDir, URL: target,
		AccountDigest:    firstNonEmpty(scopeDigest, intentAccount(intent)),
		ProfileKeyDigest: firstNonEmpty(sha256If(account.key), sameAccountDigest(sameAccount, intent)),
		CredentialRef:    credentialRef, Aliases: aliases,
		Entries: entries, Active: true,
		Generation: intentGeneration(intent) + 1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if intentSignature(intent) != intentSignature(next) {
		if err := n.writeIntent(next); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
	}
	return IntegrationResult{State: IntegrationReady, Imported: true, Aliases: aliases,
		AccountDigest: scopeDigest, ProfileKeyDigest: sha256If(account.key), CredentialRef: credentialRef}
}

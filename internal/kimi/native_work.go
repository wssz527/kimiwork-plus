package kimi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The native integration reconciles Kimi's generated `config.json` outwards from
// the profile Kimi itself owns. Three boundaries shape everything here:
//
//   - The profile is per account. A profile that cannot be tied to the caller's
//     account is never substituted for another account's.
//   - A missing or changed profile cannot be repaired from a local snapshot.
//     Only the native import, with the user's own token, can create one, so this
//     reports `needs-native-import` instead of guessing.
//   - `config.json` may be rewritten by Kimi at any moment. Every write is
//     preceded by a byte comparison against what was parsed, and a foreign entry
//     is a conflict rather than an overwrite.

// Native file names inside the Kimi home and the state directory.
const (
	NativeWorkStore   = "byok-providers.json"
	NativeWorkConfig  = "config.json"
	NativeWorkIntent  = "native-work-intent.json"
	NativeWorkLock    = ".kimi-work-gateway-owner.json"
	NativeWorkReceipt = "native-work-catalog.json"
	NativeWorkSchema  = 1
)

const (
	maxConfigBytes = 8 << 20
	maxStoreBytes  = 4 << 20
	maxIntentBytes = 1 << 20
	staleLockAge   = 60 * time.Second
)

// Integration states. Only `ready` and `repaired` mean the models are in place.
const (
	IntegrationIdle           = "idle"
	IntegrationReady          = "ready"
	IntegrationRepaired       = "repaired"
	IntegrationConflict       = "conflict"
	IntegrationNeedsImport    = "needs-native-import"
	IntegrationWaitingConfig  = "waiting-config"
	IntegrationPending        = "pending"
	IntegrationDisabled       = "disabled"
	IntegrationAccountUnknown = "account-unverified"
)

// ImportableReasons are the failures a native import may repair. An account
// change or a foreign URL is a user decision instead.
var ImportableReasons = map[string]bool{
	"profile_missing": true, "profile_store_missing": true, "models_missing": true,
	"base_url_changed": true, "credential_missing": true, "profile_delisted": true,
}

// IntegrationResult is the reconcile answer.
//
// `Imported` says the profile holds the catalog this tool owns; it does not say
// Kimi has loaded it. `Applied` and `Adopted` are the graded answers and are set
// only from readback evidence, so a caller cannot turn a page's `ok` into
// "in use" by mistake.
type IntegrationResult struct {
	State            string            `json:"state"`
	Reason           string            `json:"reason,omitempty"`
	Imported         bool              `json:"imported"`
	Changed          bool              `json:"changed"`
	Aliases          map[string]string `json:"aliases,omitempty"`
	MissingModels    []string          `json:"missingModels,omitempty"`
	MissingProviders []string          `json:"missingProviders,omitempty"`
	AccountDigest    string            `json:"accountDigest,omitempty"`
	ProfileKeyDigest string            `json:"profileKeyDigest,omitempty"`
	CredentialRef    string            `json:"credentialRef,omitempty"`
	// Applied means the readback found the models in the profile and in the
	// generated config. It is never set from a request that was merely accepted.
	Applied bool `json:"applied"`
	// Adopted means the running application has the import loaded. It needs the
	// official application to have started after the import, so it is only ever
	// reported from a caller that observed the runtime, never inferred here.
	Adopted bool `json:"adopted"`
	// Readback is what the profile and the generated config hold right now.
	Readback *ImportReadback `json:"readback,omitempty"`
	// Mode is the integration mode this result belongs to. Empty means the
	// gateway, which is what every older caller means.
	Mode string `json:"mode,omitempty"`
	// DirectSourceID is the managed custom source a Direct Mode result verified.
	DirectSourceID string `json:"directSourceId,omitempty"`
	// RouteCleanupPending means a verified gateway replacement still shares the
	// account with a connected managed Direct Mode source.
	RouteCleanupPending bool `json:"routeCleanupPending,omitempty"`
}

// NativeIntegration reconciles one Kimi home.
type NativeIntegration struct {
	stateDir string
	kimiDir  string
	sourceID string
	ownerID  string
	locked   bool
}

// NewNativeIntegration prepares an integration handle. No filesystem access
// happens here: an instance may be built before the caller knows whether the
// integration is wanted at all.
func NewNativeIntegration(stateDir, kimiDir, sourceID string) (*NativeIntegration, error) {
	if stateDir == "" || kimiDir == "" || !filepath.IsAbs(stateDir) || !filepath.IsAbs(kimiDir) {
		return nil, errors.New("invalid_integration_path")
	}
	if sourceID == "" {
		sourceID = "gateway"
	}
	owner, err := randomUUID()
	if err != nil {
		return nil, err
	}
	return &NativeIntegration{stateDir: stateDir, kimiDir: kimiDir, sourceID: sourceID, ownerID: owner}, nil
}

// ReconcileOptions is one reconcile request.
type ReconcileOptions struct {
	Enabled            bool
	URL                string
	Models             []Model
	AccountScope       string
	ConnectionRevision string
	NativeProfile      map[string]any
	// AllowAccountRebind follows a confirmed import or a switch whose target
	// catalog and runtime were verified. Periodic callers leave it false.
	AllowAccountRebind bool
	// Mode selects the integration mode. Empty means the gateway.
	Mode string
	// DirectSpec is the non-secret declaration Direct Mode verifies. It replaces
	// URL and Models, which describe the gateway import.
	DirectSpec *DirectSpec
	// DirectSourceID names the managed custom source to verify. Empty means the
	// intent's own record, then the display name.
	DirectSourceID string
}

// Reconcile compares the profile, the intent and the generated config, and
// writes only what it can prove it owns.
func (n *NativeIntegration) Reconcile(options ReconcileOptions) IntegrationResult {
	if options.Mode == IntegrationModeDirect {
		return n.verifyDirectAccount(options)
	}
	target := normalizeNativeURL(options.URL)
	catalog, ok := normalizeCatalog(options.Models)
	if target == "" || !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_request"}
	}
	if !options.Enabled {
		return n.disconnect(target)
	}

	lock, reason := n.acquireLock()
	if !lock {
		return IntegrationResult{State: IntegrationConflict, Reason: reason}
	}
	defer n.releaseLock()

	intent, intentErr := n.readIntent()
	if intentErr == errUnsafeFile {
		return IntegrationResult{State: IntegrationConflict, Reason: "intent_unavailable"}
	}

	scopeDigest := ""
	if options.AccountScope != "" {
		scopeDigest = sha256Hex(options.AccountScope)
	}

	profile := options.NativeProfile
	selectedKey := ""
	var selected profileCandidate
	storeKind := "injected"
	if profile == nil {
		store, err := readJSONFile(filepath.Join(n.kimiDir, NativeWorkStore), maxStoreBytes)
		storeKind = err
		if err == "absent" {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_store_missing"}
		}
		if err != "" {
			return IntegrationResult{State: IntegrationConflict, Reason: "profile_store_" + err}
		}
		chosen, ambiguous := selectProfile(kthProfiles(store), scopeDigest, intent)
		if ambiguous {
			return IntegrationResult{State: IntegrationAccountUnknown, Reason: "account_ambiguous"}
		}
		profile = chosen.profile
		selectedKey = chosen.key
		selected = chosen
	}
	if profile == nil {
		if intent != nil && intent.AccountDigest != "" && scopeDigest != "" && intent.AccountDigest != scopeDigest && !options.AllowAccountRebind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed"}
		}
		reason := "profile_missing"
		if storeKind == "absent" {
			reason = "profile_store_missing"
		}
		return IntegrationResult{State: IntegrationNeedsImport, Reason: reason}
	}

	// Account changes require a confirmed import or a verified switch. Both
	// require the caller's exact profile and preserve every ownership gate.
	//
	// The gate is the account, not the address: a confirmed import arrives after
	// the gateway moved (a new port is the usual case), so requiring the old
	// intent to already name the new URL would refuse exactly the import that
	// proves the new address works. The profile's own URL is still checked
	// against the target below, and every credential/store gate still applies.
	rebind := false
	if intent != nil && intent.AccountDigest != "" && scopeDigest != "" && intent.AccountDigest != scopeDigest {
		rebind = options.AllowAccountRebind && selectedKey != "" && sha256Hex(selectedKey) == scopeDigest
		if !rebind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed"}
		}
	}
	if intent != nil && intent.ProfileKeyDigest != "" && selectedKey != "" && !rebind &&
		sha256Hex(selectedKey) != intent.ProfileKeyDigest {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "account_changed"}
	}

	if pending, present := profile["pending"]; present && pending != nil {
		// An unfinished native import is a transaction in flight: its models are
		// not active yet, so nothing is generated and nothing is cleared.
		return IntegrationResult{State: IntegrationPending, Reason: "native_profile_pending"}
	}
	if delisted, _ := profile["delisted"].(bool); delisted {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_delisted"}
	}
	// The 3.2.28 account shape declares everything per provider and is generated
	// by the official application, so it is verified on its own terms: this
	// package never writes a v3 store's `config.json`.
	if selected.v3 != nil {
		result := n.verifyAccount(selected, target, catalog, intent, scopeDigest)
		if result.State == IntegrationReady || result.State == IntegrationRepaired {
			result.RouteCleanupPending = selected.v3.directCleanup
		}
		return result
	}
	profileURL := normalizeNativeURL(stringField(profile, "baseUrl"))
	if profileURL == "" {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_invalid"}
	}
	if profileURL != target {
		// A profile this tool last imported that now points elsewhere is drift
		// it can repair. A profile it never imported belongs to someone else.
		previous := ""
		if intent != nil && intent.Active {
			previous = normalizeNativeURL(intent.URL)
		}
		if previous != "" {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "base_url_changed"}
		}
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "foreign_url"}
	}
	credentialRef := stringField(profile, "credentialRef")
	if credentialRef == "" {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "credential_missing"}
	}

	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "absent" {
		// A file that is momentarily gone keeps the intent: it comes back with
		// the generator, and only an explicit disconnect clears what we know.
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
	credential, ok := credentials[credentialRef].(map[string]any)
	if !ok || stringField(credential, "apiKey") == "" {
		// The profile is intact and the reference still exists; only the
		// generated copy of the key is missing. Kimi's own generator repairs
		// that, never this package.
		return IntegrationResult{State: IntegrationWaitingConfig, Reason: "credential_not_generated",
			CredentialRef: credentialRef}
	}

	rows := profileRows(profile)
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
	aliases := map[string]string{}
	for _, entry := range matched {
		aliases[entry.catalog.id] = stringField(entry.row, "alias")
	}
	if hasDuplicateValues(aliases) {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_invalid"}
	}
	if len(missing) > 0 {
		return IntegrationResult{State: IntegrationNeedsImport, Reason: "models_missing", MissingModels: missing}
	}

	providerTypes := map[string]string{}
	for _, entry := range matched {
		providerID := stringField(entry.row, "providerId")
		kind := providerType(entry.row)
		if existing, seen := providerTypes[providerID]; seen && existing != kind {
			return IntegrationResult{State: IntegrationNeedsImport, Reason: "profile_invalid"}
		}
		providerTypes[providerID] = kind
	}

	existingProviders := section(modelSection, "providers")
	existingModels := section(modelSection, "models")
	owned := ownedEntries(intent)

	// An entry is repairable while it still equals the digest recorded for it.
	// Once the bytes differ, the change belongs to the generator, another source
	// or the user, and it is reported instead of replaced.
	for providerID, kind := range providerTypes {
		desired := map[string]any{"type": kind, "baseUrl": target, "credential": credentialRef}
		current, present := existingProviders[providerID]
		if !present {
			continue
		}
		if object, ok := current.(map[string]any); ok && providerMatches(object, desired) {
			continue
		}
		if owned.Providers[providerID].Digest == digestOf(current) {
			continue
		}
		return IntegrationResult{State: IntegrationConflict, Reason: "foreign_provider"}
	}
	for _, entry := range matched {
		alias := stringField(entry.row, "alias")
		desired := modelEntry(entry.row)
		current, present := existingModels[alias]
		if !present {
			continue
		}
		if object, ok := current.(map[string]any); ok && modelMatches(object, desired) {
			continue
		}
		if owned.Models[alias].Digest == digestOf(current) {
			continue
		}
		return IntegrationResult{State: IntegrationConflict, Reason: "foreign_model"}
	}

	// Build the next document. Unknown fields the generator or the user added
	// are preserved: only the entries this tool owns are touched.
	next, err := deepCopy(config)
	if err != nil {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_config"}
	}
	nextModel, _ := next["model"].(map[string]any)
	nextProviders := section(nextModel, "providers")
	nextModels := section(nextModel, "models")
	entries := ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}}
	changed := false
	for providerID, kind := range providerTypes {
		desired := map[string]any{"type": kind, "baseUrl": target, "credential": credentialRef}
		if current, present := existingProviders[providerID]; present {
			if object, ok := current.(map[string]any); ok && providerMatches(object, desired) {
				entries.Providers[providerID] = ownedEntry{Digest: digestOf(current),
					Written: owned.Providers[providerID].Written}
				continue
			}
		}
		nextProviders[providerID] = desired
		entries.Providers[providerID] = ownedEntry{Digest: digestOf(desired), Written: true}
		changed = true
	}
	for _, entry := range matched {
		alias := stringField(entry.row, "alias")
		desired := modelEntry(entry.row)
		if current, present := existingModels[alias]; present {
			if object, ok := current.(map[string]any); ok && modelMatches(object, desired) {
				entries.Models[alias] = ownedEntry{Digest: digestOf(current),
					Written: owned.Models[alias].Written}
				continue
			}
		}
		nextModels[alias] = desired
		entries.Models[alias] = ownedEntry{Digest: digestOf(desired), Written: true}
		changed = true
	}
	nextModel["providers"] = nextProviders
	nextModel["models"] = nextModels

	sameAccount := scopeDigest != "" && intent != nil && intent.AccountDigest == scopeDigest
	nextIntent := &nativeIntent{
		SchemaVersion: NativeWorkSchema, SourceID: n.sourceID, OwnerID: n.ownerID,
		KimiHome: n.kimiDir, URL: target,
		AccountDigest:    firstNonEmpty(scopeDigest, intentAccount(intent)),
		ProfileKeyDigest: firstNonEmpty(sha256If(selectedKey), sameAccountDigest(sameAccount, intent)),
		CredentialRef:    credentialRef, Aliases: aliases,
		Entries: entries, Active: true,
		Generation: intentGeneration(intent) + 1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	intentChanged := intentSignature(intent) != intentSignature(nextIntent)
	if intentChanged {
		if err := n.writeIntent(nextIntent); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
	}
	if changed {
		if err := n.commitConfig(configRawFor(n.kimiDir), next); err != nil {
			if intentChanged {
				n.restoreIntent(intent)
			}
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
	}
	state := IntegrationReady
	if changed {
		state = IntegrationRepaired
	}
	return IntegrationResult{State: state, Imported: true, Changed: changed, Aliases: aliases,
		AccountDigest: scopeDigest, ProfileKeyDigest: sha256If(selectedKey), CredentialRef: credentialRef}
}

// NeedsNativeImport reports whether this result is one a native import can fix.
func (r IntegrationResult) NeedsNativeImport() bool {
	return r.State == IntegrationNeedsImport && ImportableReasons[r.Reason]
}

// ---------------------------------------------------------------------------
// helpers

type matchedRow struct {
	catalog catalogEntry
	row     map[string]any
}

type catalogEntry struct {
	id       string
	modelID  string
	protocol string
}

type ownedEntry struct {
	Digest  string `json:"digest"`
	Written bool   `json:"written"`
}

type ownership struct {
	Providers map[string]ownedEntry `json:"providers"`
	Models    map[string]ownedEntry `json:"models"`
}

type nativeIntent struct {
	SchemaVersion    int               `json:"schemaVersion"`
	SourceID         string            `json:"sourceID"`
	OwnerID          string            `json:"ownerID"`
	KimiHome         string            `json:"kimiHome"`
	URL              string            `json:"url"`
	AccountDigest    string            `json:"accountDigest"`
	ProfileKeyDigest string            `json:"profileKeyDigest"`
	CredentialRef    string            `json:"credentialRef"`
	Aliases          map[string]string `json:"aliases"`
	Entries          ownership         `json:"entries"`
	Active           bool              `json:"active"`
	Generation       int               `json:"generation"`
	UpdatedAt        string            `json:"updatedAt"`
	// Mode is empty for the gateway, so an intent written by an earlier version
	// keeps meaning the gateway.
	Mode string `json:"mode,omitempty"`
	// DirectSourceID is the managed custom source a Direct Mode intent verified.
	// No provider key is ever recorded here.
	DirectSourceID string `json:"directSourceId,omitempty"`
}

var errUnsafeFile = errors.New("unsafe")

// readJSONFile answers one of "", "absent", "unsafe", "corrupt".
func readJSONFile(file string, limit int64) (map[string]any, string) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, "absent"
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit || info.Size() <= 0 {
		return nil, "unsafe"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "unsafe"
	}
	var document map[string]any
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, "corrupt"
	}
	return document, ""
}

func configRawFor(kimiDir string) string {
	data, err := os.ReadFile(filepath.Join(kimiDir, NativeWorkConfig))
	if err != nil {
		return ""
	}
	return string(data)
}

// commitConfig replaces the generated file atomically, and only while the bytes
// that were parsed are still the bytes on disk: a generator or an editor racing
// this call wins, and this call reports the conflict.
func (n *NativeIntegration) commitConfig(raw string, document map[string]any) error {
	return commitConfigFile(n.kimiDir, raw, document)
}

// commitConfigFile is the same guard for a caller that holds a Kimi home but no
// integration instance, such as the legacy-entry cleanup.
func commitConfigFile(kimiDir, raw string, document map[string]any) error {
	current, err := os.ReadFile(filepath.Join(kimiDir, NativeWorkConfig))
	if err != nil {
		return errors.New("config_missing")
	}
	if string(current) != raw {
		return errors.New("concurrent_modification")
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return errors.New("invalid_config")
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(filepath.Join(kimiDir, NativeWorkConfig)); err == nil {
		mode = info.Mode().Perm()
	}
	return replaceFile(filepath.Join(kimiDir, NativeWorkConfig), append(encoded, '\n'), mode)
}

func replaceFile(file string, content []byte, mode os.FileMode) error {
	temporary := file + "." + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tmp"
	if err := os.WriteFile(temporary, content, mode); err != nil {
		return errors.New("write_failed")
	}
	if err := os.Rename(temporary, file); err != nil {
		_ = os.Remove(temporary)
		return errors.New("write_failed")
	}
	return nil
}

func (n *NativeIntegration) readIntent() (*nativeIntent, error) {
	return readIntentFile(n.stateDir)
}

func readIntentFile(stateDir string) (*nativeIntent, error) {
	if stateDir == "" {
		return nil, nil
	}
	file := filepath.Join(stateDir, NativeWorkIntent)
	info, err := os.Lstat(file)
	if err != nil {
		return nil, nil
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxIntentBytes {
		return nil, errUnsafeFile
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, errUnsafeFile
	}
	var intent nativeIntent
	if json.Unmarshal(data, &intent) != nil || intent.SchemaVersion != NativeWorkSchema {
		return nil, nil
	}
	return &intent, nil
}

// ReadIntentAliases returns the active logical-model-id -> runtime-alias map
// recorded in native-work-intent.json for the given state directory, Kimi home,
// integration mode, and account scope.
func ReadIntentAliases(stateDir, kimiHome, mode, accountScope string) map[string]string {
	intent, err := readIntentFile(stateDir)
	if err != nil || intent == nil || !intent.Active || len(intent.Aliases) == 0 {
		return nil
	}
	if kimiHome != "" && intent.KimiHome != "" && filepath.Clean(intent.KimiHome) != filepath.Clean(kimiHome) {
		return nil
	}
	intentMode := intent.Mode
	if intentMode == "" {
		intentMode = IntegrationModeGateway
	}
	if mode != "" && intentMode != mode {
		return nil
	}
	if accountScope != "" && intent.AccountDigest != "" && sha256Hex(accountScope) != intent.AccountDigest {
		return nil
	}
	out := make(map[string]string, len(intent.Aliases))
	for modelID, alias := range intent.Aliases {
		if modelID != "" && alias != "" {
			out[modelID] = alias
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (n *NativeIntegration) writeIntent(intent *nativeIntent) error {
	if err := os.MkdirAll(n.stateDir, 0o700); err != nil {
		return errors.New("intent_unavailable")
	}
	encoded, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return errors.New("intent_unavailable")
	}
	if err := replaceFile(filepath.Join(n.stateDir, NativeWorkIntent), append(encoded, '\n'), 0o600); err != nil {
		return errors.New("intent_unavailable")
	}
	return nil
}

func (n *NativeIntegration) restoreIntent(intent *nativeIntent) {
	file := filepath.Join(n.stateDir, NativeWorkIntent)
	if intent == nil {
		_ = os.Remove(file)
		return
	}
	encoded, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return
	}
	_ = replaceFile(file, append(encoded, '\n'), 0o600)
}

// acquireLock takes the shared owner lock beside config.json. The lock lives in
// the Kimi home, so every installation sharing that home shares it wherever its
// own state directory is.
func (n *NativeIntegration) acquireLock() (bool, string) {
	info, err := os.Lstat(n.kimiDir)
	if err != nil {
		return false, "path_unavailable"
	}
	if !info.IsDir() {
		return false, "path_unavailable"
	}
	path := filepath.Join(n.kimiDir, NativeWorkLock)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, "lock_unavailable"
	}
	if err == nil {
		var owner struct {
			OwnerID  string `json:"ownerID"`
			SourceID string `json:"sourceID"`
			KimiHome string `json:"kimiHome"`
			PID      int    `json:"pid"`
		}
		if json.Unmarshal(data, &owner) != nil || owner.OwnerID == "" {
			if age, ok := lockAge(path); !ok || age < staleLockAge {
				return false, "lock_corrupt"
			}
		} else if owner.OwnerID == n.ownerID {
			n.locked = true
			return true, ""
		} else if owner.KimiHome != "" && filepath.Clean(owner.KimiHome) != n.kimiDir {
			return false, "owner_conflict"
		} else if processAlive(owner.PID) {
			return false, "owner_conflict"
		}
		current, readErr := os.ReadFile(path)
		if readErr != nil || string(current) != string(data) {
			return false, "owner_conflict"
		}
		_ = os.Remove(path)
	}
	record, err := json.Marshal(map[string]any{"ownerID": n.ownerID, "sourceID": n.sourceID,
		"kimiHome": n.kimiDir, "pid": os.Getpid(), "at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return false, "lock_unavailable"
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, "owner_conflict"
		}
		return false, "lock_unavailable"
	}
	defer handle.Close()
	if _, err := handle.Write(append(record, '\n')); err != nil {
		return false, "lock_unavailable"
	}
	n.locked = true
	return true, ""
}

func (n *NativeIntegration) releaseLock() bool {
	if !n.locked {
		return false
	}
	n.locked = false
	path := filepath.Join(n.kimiDir, NativeWorkLock)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var owner struct {
		OwnerID string `json:"ownerID"`
	}
	if json.Unmarshal(data, &owner) != nil || owner.OwnerID != n.ownerID {
		return false
	}
	return os.Remove(path) == nil
}

func lockAge(path string) (time.Duration, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// Disconnect clears this tool's ownership record after the official source API
// has disconnected its sources. Legacy generated entries are cleaned here too;
// v3 entries are owned and regenerated by Kimi itself.
func (n *NativeIntegration) Disconnect() IntegrationResult {
	return n.disconnectForAccount("", "")
}

// DisconnectForAccount clears this tool's ownership record only when the local
// intent belongs to accountScope (or has no recorded account). Disconnecting a
// second account must not deactivate another account's active intent.
func (n *NativeIntegration) DisconnectForAccount(accountScope string) IntegrationResult {
	return n.disconnectForAccount("", accountScope)
}

func (n *NativeIntegration) disconnect(target string) IntegrationResult {
	return n.disconnectForAccount(target, "")
}

// disconnectForAccount removes only entries this tool wrote and still owns byte
// for byte, keeps a model that is still the current one, and keeps every
// provider another model still references. The intent stays, marked inactive
// and stripped of its ownership digests, so a later run cannot revive the old
// import.
func (n *NativeIntegration) disconnectForAccount(target, accountScope string) IntegrationResult {
	if ok, reason := n.acquireLock(); !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: reason}
	}
	defer n.releaseLock()
	intent, err := n.readIntent()
	if err == errUnsafeFile {
		return IntegrationResult{State: IntegrationConflict, Reason: "intent_unavailable"}
	}
	result := IntegrationResult{State: IntegrationDisabled, Reason: "disconnected"}
	if intent == nil {
		return result
	}
	if accountScope != "" && intent.AccountDigest != "" && intent.AccountDigest != sha256Hex(accountScope) {
		return result
	}
	owned := ownedEntries(intent)
	if len(owned.Models) == 0 && len(owned.Providers) == 0 {
		if err := n.markInactive(intent, "disconnected"); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
		return result
	}
	config, configErr := readJSONFile(filepath.Join(n.kimiDir, NativeWorkConfig), maxConfigBytes)
	if configErr == "absent" {
		if err := n.markInactive(intent, "config_missing"); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
		result.Reason = "config_missing"
		return result
	}
	if configErr != "" {
		return IntegrationResult{State: IntegrationConflict, Reason: "config_" + configErr}
	}
	if _, ok := config["model"].(map[string]any); !ok {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_config"}
	}
	next, err := deepCopy(config)
	if err != nil {
		return IntegrationResult{State: IntegrationConflict, Reason: "invalid_config"}
	}
	nextModel, _ := next["model"].(map[string]any)
	models := section(nextModel, "models")
	providers := section(nextModel, "providers")
	changed := false
	for alias, entry := range owned.Models {
		if !entry.Written {
			continue
		}
		current, present := models[alias]
		if !present {
			continue
		}
		if currentAlias, _ := nextModel["current"].(string); currentAlias == alias {
			continue
		}
		if digestOf(current) != entry.Digest {
			continue
		}
		delete(models, alias)
		changed = true
	}
	for providerID, entry := range owned.Providers {
		if !entry.Written {
			continue
		}
		current, present := providers[providerID]
		if !present {
			continue
		}
		if providerReferenced(models, providerID) {
			continue
		}
		if digestOf(current) != entry.Digest {
			continue
		}
		delete(providers, providerID)
		changed = true
	}
	if changed {
		if err := n.commitConfig(configRawFor(n.kimiDir), next); err != nil {
			return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
		}
	}
	if err := n.markInactive(intent, "disconnected"); err != nil {
		return IntegrationResult{State: IntegrationConflict, Reason: err.Error()}
	}
	result.Changed = changed
	return result
}

func (n *NativeIntegration) markInactive(intent *nativeIntent, reason string) error {
	if intent == nil {
		return nil
	}
	next := *intent
	next.Active = false
	next.Entries = ownership{}
	next.Generation = intent.Generation + 1
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return n.writeIntent(&next)
}

func providerReferenced(models map[string]any, providerID string) bool {
	for _, row := range models {
		entry, ok := row.(map[string]any)
		if ok && stringField(entry, "provider") == providerID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// profile shapes

type profileCandidate struct {
	key     string
	profile map[string]any
	// v3 is set when the profile came from the 3.2.28 `accounts` shape. Its
	// declarations are per provider and it is verified, never written.
	v3 *nativeAccountV3
}

// kthProfiles reads the profile entries Kimi's own store holds. Both store
// generations are accepted, because the released build moved from `profiles` to
// `accounts` and an installation upgraded in place may hold either. Profiles are
// found by shape, not by version: the store's own `v` field is tolerated.
func kthProfiles(store map[string]any) []profileCandidate {
	if found := legacyProfiles(store); len(found) > 0 {
		return found
	}
	return accountProfiles(store)
}

func legacyProfiles(store map[string]any) []profileCandidate {
	return legacyProfilesOfKind(store, "kth")
}

func legacyProfilesOfKind(store map[string]any, wantedKind string) []profileCandidate {
	profiles, ok := store["profiles"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(profiles))
	for key := range profiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var found []profileCandidate
	for _, key := range keys {
		entry, ok := profiles[key].(map[string]any)
		if !ok {
			continue
		}
		for slot, candidate := range entry {
			profile, ok := candidate.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := profile["kind"].(string)
			id, _ := profile["id"].(string)
			_, hasBaseURL := profile["baseUrl"].(string)
			if kind == wantedKind || (wantedKind == "kth" && (id == "byok-kth" || (slot == "byok-kth" && hasBaseURL))) {
				found = append(found, profileCandidate{key: key, profile: profile})
				if wantedKind == "kth" {
					break
				}
			}
		}
	}
	return found
}

// Exact account scope wins over legacy intent and single-profile fallbacks.
//
// Both store generations key a profile by sha256 of the signed-in user id,
// which is what `AccountScope` answers, so the digest comparison is the same for
// each. A v3 account is selected by its key alone: its profile map carries no
// identity of its own.
func selectProfile(candidates []profileCandidate, scopeDigest string, intent *nativeIntent) (profileCandidate, bool) {
	if len(candidates) == 0 {
		return profileCandidate{}, false
	}
	if scopeDigest != "" {
		for _, candidate := range candidates {
			if sha256Hex(candidate.key) == scopeDigest {
				return candidate, false
			}
		}
		return profileCandidate{}, false
	}
	if intent != nil && intent.ProfileKeyDigest != "" {
		for _, candidate := range candidates {
			if sha256Hex(candidate.key) == intent.ProfileKeyDigest {
				return candidate, false
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0], false
	}
	return profileCandidate{}, true
}

func profileRows(profile map[string]any) []map[string]any {
	rows, ok := profile["models"].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if stringField(entry, "alias") == "" || stringField(entry, "providerId") == "" ||
			stringField(entry, "model") == "" {
			continue
		}
		if size, ok := entry["maxContextSize"].(float64); !ok || size <= 0 {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func findRow(rows []map[string]any, id, wanted string) (map[string]any, bool) {
	var fallback map[string]any
	for _, row := range rows {
		if stringField(row, "model") != id {
			continue
		}
		if rowFamily(row) == wanted {
			return row, true
		}
		if fallback == nil {
			fallback = row
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

func rowFamily(row map[string]any) string {
	if family, _ := row["family"].(string); family == "kimi" || family == "responses" {
		return family
	}
	alias := stringField(row, "alias")
	if strings.HasPrefix(alias, "byok-kth-kimi/") {
		return "kimi"
	}
	if strings.HasPrefix(alias, "byok-kth-resp/") {
		return "responses"
	}
	model := stringField(row, "model")
	last := model
	if at := strings.LastIndex(model, "/"); at >= 0 {
		last = model[at+1:]
	}
	if strings.HasPrefix(strings.ToLower(last), "kimi-") {
		return "kimi"
	}
	return "responses"
}

func providerType(row map[string]any) string {
	if rowFamily(row) == "kimi" {
		return "kimi"
	}
	return "openai_responses"
}

func modelEntry(row map[string]any) map[string]any {
	entry := map[string]any{
		"provider":       stringField(row, "providerId"),
		"model":          stringField(row, "model"),
		"maxContextSize": row["maxContextSize"],
	}
	if size, ok := row["maxOutputSize"].(float64); ok && size > 0 {
		entry["maxOutputSize"] = row["maxOutputSize"]
	}
	if capabilities := uniqueSorted(row["capabilities"]); len(capabilities) > 0 {
		entry["capabilities"] = capabilities
	}
	if efforts := uniqueSorted(row["supportEfforts"]); len(efforts) > 0 {
		entry["supportEfforts"] = efforts
		if def, _ := row["defaultEffort"].(string); containsString(efforts, def) {
			entry["defaultEffort"] = def
		}
	}
	return entry
}

func providerMatches(current, desired map[string]any) bool {
	return stringField(current, "type") == stringField(desired, "type") &&
		normalizeNativeURL(stringField(current, "baseUrl")) == stringField(desired, "baseUrl") &&
		stringField(current, "credential") == stringField(desired, "credential")
}

// providerMatchesPublished is providerMatches for an answer that publishes no
// credential reference: the 3.2.28 surface reports whether a key is configured,
// not which reference holds it, so only the terms it does publish are compared.
// The store on disk is what proves the reference.
func providerMatchesPublished(current, desired map[string]any) bool {
	return stringField(current, "type") == stringField(desired, "type") &&
		normalizeNativeURL(stringField(current, "baseUrl")) == stringField(desired, "baseUrl")
}

// modelMatches compares declaration terms: "absent" and "an empty list" both
// mean "nothing declared", because Kimi's generator writes `[]` where this
// package omits the field. Scalars keep exact comparison.
func modelMatches(current, desired map[string]any) bool {
	if digestOf(current["provider"]) != digestOf(desired["provider"]) ||
		digestOf(current["model"]) != digestOf(desired["model"]) ||
		digestOf(current["maxContextSize"]) != digestOf(desired["maxContextSize"]) ||
		digestOf(current["maxOutputSize"]) != digestOf(desired["maxOutputSize"]) ||
		digestOf(current["defaultEffort"]) != digestOf(desired["defaultEffort"]) {
		return false
	}
	return sameList(current["capabilities"], desired["capabilities"]) &&
		sameList(current["supportEfforts"], desired["supportEfforts"])
}

func sameList(current, desired any) bool {
	left := uniqueSorted(current)
	right := uniqueSorted(desired)
	return digestOf(left) == digestOf(right)
}

func uniqueSorted(value any) []string {
	rows, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			rows = make([]any, len(typed))
			for i, item := range typed {
				rows[i] = item
			}
		} else {
			return nil
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		text, ok := row.(string)
		if !ok || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

func normalizeCatalog(models []Model) ([]catalogEntry, bool) {
	if len(models) == 0 || len(models) > 256 {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]catalogEntry, 0, len(models))
	for _, model := range models {
		if model.ID == "" || seen[model.ID] {
			return nil, false
		}
		protocol := model.Protocol
		if protocol == "" {
			protocol = ModelFamily(model.ID)
		}
		if protocol != "chat" && protocol != "responses" {
			return nil, false
		}
		seen[model.ID] = true
		out = append(out, catalogEntry{id: model.ID, modelID: firstNonEmpty(model.CatalogID, model.ID), protocol: protocol})
	}
	return out, true
}

func normalizeNativeURL(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := parseURL(value)
	if err != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.TrimRight(parsed.String(), "/")
}

func section(parent map[string]any, name string) map[string]any {
	if existing, ok := parent[name].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	parent[name] = created
	return created
}

func deepCopy(document map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	var copy map[string]any
	if json.Unmarshal(encoded, &copy) != nil {
		return nil, errors.New("invalid_config")
	}
	return copy, nil
}

// digestOf canonicalises before hashing, because key order differs between
// Kimi's generator, a hand edit and this package.
func digestOf(value any) string {
	return sha256Hex(stableJSON(value))
}

func stableJSON(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = stableJSON(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case []string:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = strconv.Quote(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, strconv.Quote(key)+":"+stableJSON(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "undefined"
	}
	return string(encoded)
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func sha256If(value string) string {
	if value == "" {
		return ""
	}
	return sha256Hex(value)
}

func ownedEntries(intent *nativeIntent) ownership {
	if intent == nil {
		return ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}}
	}
	out := ownership{Providers: map[string]ownedEntry{}, Models: map[string]ownedEntry{}}
	for key, entry := range intent.Entries.Providers {
		if entry.Digest != "" {
			out.Providers[key] = entry
		}
	}
	for key, entry := range intent.Entries.Models {
		if entry.Digest != "" {
			out.Models[key] = entry
		}
	}
	return out
}

// intentSignature is the content of the intent, excluding its generation and
// timestamp. It decides whether a write is needed, so the generation only moves
// when the intent's content actually changes: a generation that incremented on
// every call would make "the child loaded generation N" meaningless.
func intentSignature(intent *nativeIntent) string {
	if intent == nil {
		return ""
	}
	return stableJSON(map[string]any{
		"sourceID": intent.SourceID, "kimiHome": intent.KimiHome, "url": intent.URL,
		"accountDigest": intent.AccountDigest, "profileKeyDigest": intent.ProfileKeyDigest,
		"credentialRef": intent.CredentialRef, "aliases": intent.Aliases,
		"active": intent.Active, "mode": intent.Mode, "directSourceId": intent.DirectSourceID,
	})
}

func intentAccount(intent *nativeIntent) string {
	if intent == nil {
		return ""
	}
	return intent.AccountDigest
}

func intentGeneration(intent *nativeIntent) int {
	if intent == nil {
		return 0
	}
	return intent.Generation
}

func sameAccountDigest(same bool, intent *nativeIntent) string {
	if same && intent != nil {
		return intent.ProfileKeyDigest
	}
	return ""
}

func hasDuplicateValues(aliases map[string]string) bool {
	seen := map[string]bool{}
	for _, alias := range aliases {
		if alias == "" {
			return true
		}
		if seen[alias] {
			return true
		}
		seen[alias] = true
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func processAlive(pid int) bool {
	return processAlivePlatform(pid)
}

package kimi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CompatStatusVerified       = "verified"
	CompatStatusNewVersionPass = "new_version_ok"
	CompatStatusDegraded       = "degraded"
	CompatStatusUnavailable    = "unavailable"
	CompatStatusChecking       = "checking"

	CheckStatusPass        = "pass"
	CheckStatusFail        = "fail"
	CheckStatusUnavailable = "unavailable"
	CheckStatusAbsent      = "absent"

	StoreFormatProfiles = "profiles"
	StoreFormatAccounts = "accounts"
	StoreFormatAbsent   = "absent"
	StoreFormatUnknown  = "unknown"
	StoreFormatInvalid  = "invalid"

	FeatureModelSync     = "model_sync"
	FeatureAccountSwitch = "account_switch"

	CompatErrModelSyncBlocked     = "compat_model_sync_blocked"
	CompatErrAccountSwitchBlocked = "compat_account_switch_blocked"

	maxPreloadInspectBytes = 2 << 20
)

// VerifiedOfficialVersions lists the official Kimi desktop versions that have
// been verified end-to-end with KimiWork +.
var VerifiedOfficialVersions = []string{"3.2.27", "3.2.28"}

func IsVerifiedOfficialVersion(version string) bool {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		return false
	}
	for _, known := range VerifiedOfficialVersions {
		if trimmed == known {
			return true
		}
	}
	return false
}

type CompatibilityReport struct {
	Status               string              `json:"status"`
	Summary              string              `json:"summary"`
	OfficialVersion      string              `json:"officialVersion,omitempty"`
	VerifiedVersion      bool                `json:"verifiedVersion"`
	VerifiedVersions     []string            `json:"verifiedVersions"`
	KernelVersion        string              `json:"kernelVersion,omitempty"`
	KernelCommit         string              `json:"kernelCommit,omitempty"`
	ArchiveSHA256        string              `json:"archiveSha256,omitempty"`
	VersionChanged       bool                `json:"versionChanged,omitempty"`
	PreviousVersion      string              `json:"previousVersion,omitempty"`
	PreviousKernelCommit string              `json:"previousKernelCommit,omitempty"`
	CheckedAt            int64               `json:"checkedAt"`
	Checks               CompatibilityChecks `json:"checks"`
	AffectedFeatures     []AffectedFeature   `json:"affectedFeatures"`
	Guards               CompatibilityGuards `json:"guards"`
}

type CompatibilityChecks struct {
	Catalog     CatalogCompatCheck     `json:"catalog"`
	AccountAPI  AccountAPICompatCheck  `json:"accountApi"`
	StoreFormat StoreFormatCompatCheck `json:"storeFormat"`
}

type CatalogCompatCheck struct {
	Status        string `json:"status"`
	ContractState string `json:"contractState,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Parser        string `json:"parser,omitempty"`
	ModelCount    int    `json:"modelCount,omitempty"`
	AcceptedCount int    `json:"acceptedCount,omitempty"`
}

type AccountAPICompatCheck struct {
	Status          string `json:"status"`
	Source          string `json:"source,omitempty"`
	GetUserProfile  bool   `json:"getUserProfile"`
	GetByokProfile  bool   `json:"getByokProfile"`
	ByokSourcesList bool   `json:"byokSourcesList"`
	ModelImportAPI  bool   `json:"modelImportApi"`
	Reason          string `json:"reason,omitempty"`
}

type StoreFormatCompatCheck struct {
	Status string `json:"status"`
	Format string `json:"format"`
	Reason string `json:"reason,omitempty"`
}

type AffectedFeature struct {
	Feature      string `json:"feature"`
	Label        string `json:"label"`
	Reason       string `json:"reason"`
	WriteBlocked bool   `json:"writeBlocked"`
}

type CompatibilityGuards struct {
	ModelSyncWriteBlocked     bool `json:"modelSyncWriteBlocked"`
	AccountSwitchWriteBlocked bool `json:"accountSwitchWriteBlocked"`
}

// CompatibilityOptions configures one read-only compatibility self-check.
type CompatibilityOptions struct {
	Installation Installation
	WorkHome     string
	Models       []Model
	Contract     *ContractResult
	Capabilities *WorkCapabilities
	LiveAPIs     *AccountAPICompatCheck
	Previous     *OfficialObservation
	Platform     string
	Env          map[string]string
	Executor     ExecutorConfig
	SkipProcess  bool
}

// OfficialObservation records the last observed official Kimi fingerprint on
// disk so restarts and background updates detect version or kernel shifts.
type OfficialObservation struct {
	OfficialVersion string `json:"officialVersion,omitempty"`
	KernelVersion   string `json:"kernelVersion,omitempty"`
	KernelCommit    string `json:"kernelCommit,omitempty"`
	ArchiveSHA256   string `json:"archiveSha256,omitempty"`
	StoreFormat     string `json:"storeFormat,omitempty"`
	Status          string `json:"status,omitempty"`
	CheckedAt       int64  `json:"checkedAt,omitempty"`
}

// OfficialFingerprint captures cheap stat/metadata fields so a periodic watcher
// can tell when the official Kimi install, kernel bundle or BYOK store changed
// without hashing the 200MB archive on every tick.
type OfficialFingerprint struct {
	ArchivePath     string
	ArchiveModTime  int64
	ArchiveSize     int64
	OfficialVersion string
	KernelVersion   string
	KernelCommit    string
	StoreExists     bool
	StoreModTime    int64
	StoreSize       int64
}

func ReadOfficialFingerprint(installation Installation, workHome string) OfficialFingerprint {
	fp := OfficialFingerprint{
		ArchivePath:     installation.Archive,
		OfficialVersion: installation.Version,
	}
	if installation.Archive != "" {
		if info, err := os.Stat(installation.Archive); err == nil {
			fp.ArchiveModTime = info.ModTime().UnixNano()
			fp.ArchiveSize = info.Size()
		}
		if version := InstalledVersion(installation.Archive); version != "" {
			fp.OfficialVersion = version
		}
		fp.KernelVersion, fp.KernelCommit = readKernelMetadata(installation.Archive)
	}
	if workHome != "" {
		if info, err := os.Stat(filepath.Join(workHome, NativeWorkStore)); err == nil {
			fp.StoreExists = true
			fp.StoreModTime = info.ModTime().UnixNano()
			fp.StoreSize = info.Size()
		}
	}
	return fp
}

func readKernelMetadata(archive string) (version, commit string) {
	if archive == "" {
		return "", ""
	}
	bundleManifest := filepath.Join(filepath.Dir(archive), "resources", "daimon-bundle", "bundle.json")
	data, err := readCapabilityFile(bundleManifest, 64<<10)
	if err != nil {
		return "", ""
	}
	var metadata struct {
		Version      string `json:"version"`
		KernelCommit string `json:"kernelCommit"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return "", ""
	}
	if len(metadata.Version) <= 128 {
		version = metadata.Version
	}
	if len(metadata.KernelCommit) <= 128 {
		commit = metadata.KernelCommit
	}
	return version, commit
}

// CheckCompatibility runs the full read-only self-check suite:
//  1. Model catalog parser probe (Inspect)
//  2. Account & model bridge API presence (getUserProfile, getByokProfile / byokSources.list)
//  3. BYOK store format (profiles vs accounts)
//
// Nothing here starts, stops or writes to the official application.
func CheckCompatibility(ctx context.Context, opts CompatibilityOptions) (CompatibilityReport, ContractResult) {
	archive := opts.Installation.Archive
	var contract ContractResult
	if opts.Contract != nil {
		contract = *opts.Contract
	} else {
		contract = Inspect(ctx, ProbeOptions{
			Archive:     archive,
			Models:      opts.Models,
			Platform:    opts.Platform,
			Env:         opts.Env,
			Executor:    opts.Executor,
			SkipProcess: opts.SkipProcess,
		})
	}
	if archive == "" {
		archive = contract.Archive
	}

	version := firstNonEmpty(contract.Version, opts.Installation.Version)
	if version == "" && archive != "" {
		version = InstalledVersion(archive)
	}

	var kernelVersion, kernelCommit string
	if opts.Capabilities != nil {
		kernelVersion = opts.Capabilities.KernelVersion
		kernelCommit = opts.Capabilities.KernelCommit
	}
	if kernelVersion == "" && kernelCommit == "" && archive != "" {
		kernelVersion, kernelCommit = readKernelMetadata(archive)
	}

	catalogCheck := buildCatalogCheck(contract)
	apiCheck := InspectPreloadAPIs(archive)
	if opts.LiveAPIs != nil && opts.LiveAPIs.Source == "live" {
		apiCheck = mergeLiveAPICheck(apiCheck, *opts.LiveAPIs)
	}
	storeCheck := InspectStoreFormat(opts.WorkHome)

	report := CompatibilityReport{
		OfficialVersion:  version,
		VerifiedVersion:  IsVerifiedOfficialVersion(version),
		VerifiedVersions: append([]string(nil), VerifiedOfficialVersions...),
		KernelVersion:    kernelVersion,
		KernelCommit:     kernelCommit,
		ArchiveSHA256:    contract.SHA256,
		CheckedAt:        time.Now().UnixMilli(),
		Checks: CompatibilityChecks{
			Catalog:     catalogCheck,
			AccountAPI:  apiCheck,
			StoreFormat: storeCheck,
		},
		AffectedFeatures: []AffectedFeature{},
	}

	if opts.Previous != nil {
		prevVer := strings.TrimSpace(opts.Previous.OfficialVersion)
		prevCommit := strings.TrimSpace(opts.Previous.KernelCommit)
		verChanged := prevVer != "" && version != "" && prevVer != version
		commitChanged := prevCommit != "" && kernelCommit != "" && prevCommit != kernelCommit
		if verChanged || commitChanged {
			report.VersionChanged = true
			report.PreviousVersion = prevVer
			report.PreviousKernelCommit = prevCommit
		}
	}

	if catalogCheck.Status == CheckStatusUnavailable {
		report.Status = CompatStatusUnavailable
		report.Summary = "未检测到官方 Kimi"
		report.Guards = CompatibilityGuards{
			ModelSyncWriteBlocked:     true,
			AccountSwitchWriteBlocked: true,
		}
		return report, contract
	}

	modelSyncReasons := []string{}
	if catalogCheck.Status != CheckStatusPass {
		modelSyncReasons = append(modelSyncReasons, firstNonEmpty(catalogCheck.Reason, "catalog_probe_failed"))
	}
	if apiCheck.Status != CheckStatusUnavailable && !apiCheck.GetByokProfile && !apiCheck.ByokSourcesList {
		modelSyncReasons = append(modelSyncReasons, "model_readback_api_missing")
	}
	if apiCheck.Status != CheckStatusUnavailable && !apiCheck.ModelImportAPI {
		modelSyncReasons = append(modelSyncReasons, "model_import_api_missing")
	}
	if storeCheck.Status == CheckStatusFail {
		modelSyncReasons = append(modelSyncReasons, firstNonEmpty(storeCheck.Reason, "store_format_unsupported"))
	}

	accountSwitchReasons := []string{}
	if apiCheck.Status != CheckStatusUnavailable && !apiCheck.GetUserProfile {
		accountSwitchReasons = append(accountSwitchReasons, "account_profile_api_missing")
	}

	if len(modelSyncReasons) > 0 {
		report.Guards.ModelSyncWriteBlocked = true
		report.AffectedFeatures = append(report.AffectedFeatures, AffectedFeature{
			Feature:      FeatureModelSync,
			Label:        "模型同步不可用",
			Reason:       strings.Join(modelSyncReasons, ","),
			WriteBlocked: true,
		})
	}
	if len(accountSwitchReasons) > 0 {
		report.Guards.AccountSwitchWriteBlocked = true
		report.AffectedFeatures = append(report.AffectedFeatures, AffectedFeature{
			Feature:      FeatureAccountSwitch,
			Label:        "账号切换不可用",
			Reason:       strings.Join(accountSwitchReasons, ","),
			WriteBlocked: true,
		})
	}

	if len(report.AffectedFeatures) > 0 {
		report.Status = CompatStatusDegraded
		labels := make([]string, 0, len(report.AffectedFeatures))
		for _, item := range report.AffectedFeatures {
			labels = append(labels, item.Label)
		}
		report.Summary = strings.Join(labels, "、")
		return report, contract
	}

	if report.VerifiedVersion {
		report.Status = CompatStatusVerified
		report.Summary = "兼容"
	} else {
		report.Status = CompatStatusNewVersionPass
		report.Summary = "新版本，基础检查通过"
	}
	return report, contract
}

func buildCatalogCheck(contract ContractResult) CatalogCompatCheck {
	check := CatalogCompatCheck{
		ContractState: contract.State,
		Reason:        contract.Reason,
		Parser:        contract.Parser,
		ModelCount:    contract.ModelCount,
		AcceptedCount: len(contract.Accepted),
	}
	switch contract.State {
	case StateVerified:
		check.Status = CheckStatusPass
	case StateUnavailable:
		check.Status = CheckStatusUnavailable
	default:
		check.Status = CheckStatusFail
	}
	return check
}

// InspectPreloadAPIs performs a strictly read-only check of the preload scripts
// packaged inside app.asar to verify that account and model bridge APIs exist.
func InspectPreloadAPIs(archive string) AccountAPICompatCheck {
	if archive == "" {
		return AccountAPICompatCheck{Status: CheckStatusUnavailable, Source: "none", Reason: "archive_missing"}
	}
	if _, err := os.Stat(archive); err != nil {
		return AccountAPICompatCheck{Status: CheckStatusUnavailable, Source: "none", Reason: "archive_missing"}
	}
	entries, err := ListAsarEntries(archive, "out/preload")
	if err != nil || len(entries) == 0 {
		return AccountAPICompatCheck{Status: CheckStatusFail, Source: "preload", Reason: "preload_entries_missing"}
	}

	var agentText, clawText, sharedText strings.Builder
	for _, entry := range entries {
		base := strings.ToLower(filepath.Base(entry))
		if !strings.HasSuffix(base, ".mjs") && !strings.HasSuffix(base, ".js") && !strings.HasSuffix(base, ".cjs") {
			continue
		}
		var target *strings.Builder
		switch {
		case strings.HasPrefix(base, "preload-kimi-agent."):
			target = &agentText
		case strings.HasPrefix(base, "preload-claw."):
			target = &clawText
		case strings.HasPrefix(base, "ipc-subscribe"):
			target = &sharedText
		default:
			continue
		}
		content, err := ReadAsarEntry(archive, entry)
		if err != nil || len(content) > maxPreloadInspectBytes {
			continue
		}
		target.WriteString(content)
		target.WriteByte('\n')
	}

	agent := agentText.String()
	claw := clawText.String()
	shared := sharedText.String()
	if agent == "" && claw == "" {
		return AccountAPICompatCheck{Status: CheckStatusFail, Source: "preload", Reason: "preload_scripts_not_found"}
	}

	hasAgentAPI := strings.Contains(agent, "kimiAgentAPI")
	hasClawAPI := strings.Contains(claw, "clawBridgeAPI")
	getUserProfile := hasAgentAPI && strings.Contains(agent, "getUserProfile")
	getByokProfile := hasAgentAPI && strings.Contains(agent, "getByokProfile")

	agentByokSources := hasAgentAPI && strings.Contains(agent, "byokSources") && (strings.Contains(agent, "list") || strings.Contains(shared, "BYOK_SOURCES_LIST"))
	clawByokSources := hasClawAPI && strings.Contains(claw, "byokSources") && (strings.Contains(claw, "list") || strings.Contains(shared, "list") || strings.Contains(shared, "BYOK_SOURCES_LIST"))
	byokSourcesList := agentByokSources || clawByokSources

	legacyImport := hasClawAPI && strings.Contains(claw, "importByokModels")
	v3Import := clawByokSources &&
		(strings.Contains(claw, "preview") || strings.Contains(shared, "preview")) &&
		(strings.Contains(claw, "save") || strings.Contains(shared, "save"))
	modelImportAPI := legacyImport || v3Import

	out := AccountAPICompatCheck{
		Status:          CheckStatusPass,
		Source:          "preload",
		GetUserProfile:  getUserProfile,
		GetByokProfile:  getByokProfile,
		ByokSourcesList: byokSourcesList,
		ModelImportAPI:  modelImportAPI,
	}
	var missing []string
	if !getUserProfile {
		missing = append(missing, "getUserProfile")
	}
	if !getByokProfile && !byokSourcesList {
		missing = append(missing, "getByokProfile_or_byokSources.list")
	}
	if !modelImportAPI {
		missing = append(missing, "modelImportAPI")
	}
	if len(missing) > 0 {
		out.Status = CheckStatusFail
		out.Reason = "missing:" + strings.Join(missing, ",")
	}
	return out
}

func mergeLiveAPICheck(staticCheck, liveCheck AccountAPICompatCheck) AccountAPICompatCheck {
	out := staticCheck
	out.Source = "live"
	out.GetUserProfile = liveCheck.GetUserProfile
	out.GetByokProfile = liveCheck.GetByokProfile
	out.ByokSourcesList = liveCheck.ByokSourcesList
	if liveCheck.ModelImportAPI {
		out.ModelImportAPI = true
	} else if liveCheck.Reason != "claw_page_not_open" {
		out.ModelImportAPI = false
	}
	var missing []string
	if !out.GetUserProfile {
		missing = append(missing, "getUserProfile")
	}
	if !out.GetByokProfile && !out.ByokSourcesList {
		missing = append(missing, "getByokProfile_or_byokSources.list")
	}
	if !out.ModelImportAPI {
		missing = append(missing, "modelImportAPI")
	}
	if len(missing) > 0 {
		out.Status = CheckStatusFail
		out.Reason = "missing:" + strings.Join(missing, ",")
	} else {
		out.Status = CheckStatusPass
		out.Reason = ""
	}
	return out
}

// InspectStoreFormat inspects `<workHome>/byok-providers.json` in read-only mode
// to identify whether the official store uses `profiles` (3.2.27) or `accounts`
// (3.2.28+), or an unrecognised future layout.
func InspectStoreFormat(workHome string) StoreFormatCompatCheck {
	if workHome == "" {
		return StoreFormatCompatCheck{Status: CheckStatusAbsent, Format: StoreFormatAbsent, Reason: "work_home_unresolved"}
	}
	store, errCode := readJSONFile(filepath.Join(workHome, NativeWorkStore), maxStoreBytes)
	if errCode == "absent" {
		return StoreFormatCompatCheck{Status: CheckStatusAbsent, Format: StoreFormatAbsent, Reason: "store_not_created"}
	}
	if errCode != "" {
		return StoreFormatCompatCheck{Status: CheckStatusFail, Format: StoreFormatInvalid, Reason: "store_" + errCode}
	}
	if _, ok := store["accounts"].(map[string]any); ok {
		return StoreFormatCompatCheck{Status: CheckStatusPass, Format: StoreFormatAccounts}
	}
	if _, ok := store["profiles"].(map[string]any); ok {
		return StoreFormatCompatCheck{Status: CheckStatusPass, Format: StoreFormatProfiles}
	}
	return StoreFormatCompatCheck{Status: CheckStatusFail, Format: StoreFormatUnknown, Reason: "store_format_unknown"}
}

// LoadOfficialObservation reads the persisted last-seen official Kimi
// fingerprint from stateDir. Missing or invalid files return nil without error.
func LoadOfficialObservation(stateDir string) *OfficialObservation {
	if stateDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "official-compat.json"))
	if err != nil || len(data) == 0 || len(data) > 64<<10 {
		return nil
	}
	var obs OfficialObservation
	if json.Unmarshal(data, &obs) != nil {
		return nil
	}
	return &obs
}

// SaveOfficialObservation atomically writes the latest official Kimi
// fingerprint to stateDir/official-compat.json (no credentials or secrets).
func SaveOfficialObservation(stateDir string, report CompatibilityReport) error {
	if stateDir == "" || report.Status == CompatStatusUnavailable || report.Status == CompatStatusChecking {
		return nil
	}
	obs := OfficialObservation{
		OfficialVersion: report.OfficialVersion,
		KernelVersion:   report.KernelVersion,
		KernelCommit:    report.KernelCommit,
		ArchiveSHA256:   report.ArchiveSHA256,
		StoreFormat:     report.Checks.StoreFormat.Format,
		Status:          report.Status,
		CheckedAt:       report.CheckedAt,
	}
	encoded, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(stateDir, "official-compat.json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// ProbeAccountAPIsLive runs a strictly read-only check against the managed
// Work shell page (and claw page if open) to confirm the runtime page APIs.
func (c *Controller) ProbeAccountAPIsLive(ctx context.Context) (*AccountAPICompatCheck, bool) {
	if !accountSessionMu.TryLock() {
		return nil, false
	}
	defer accountSessionMu.Unlock()
	surface, err := c.accountSurface()
	if err != nil {
		return nil, false
	}
	actions, ok := surface.(*cdpActions)
	if !ok {
		return nil, false
	}
	pages, err := actions.accountPages(ctx)
	if err != nil {
		return nil, false
	}
	var shell accountPage
	for _, page := range pages {
		if !page.credentials && page.sessionID != "" {
			shell = page
			break
		}
	}
	if shell.sessionID == "" {
		return nil, false
	}
	value, err := actions.evaluate(ctx, shell.sessionID, liveAccountAPIProbeExpression)
	if err != nil {
		return nil, false
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	check := &AccountAPICompatCheck{
		Source:          "live",
		GetUserProfile:  raw["getUserProfile"] == true,
		GetByokProfile:  raw["getByokProfile"] == true,
		ByokSourcesList: raw["byokSourcesList"] == true,
		ModelImportAPI:  raw["modelImportApi"] == true,
		Reason:          "claw_page_not_open",
	}
	if targets, err := actions.targets(ctx); err == nil {
		if claw, found := findPath(targets, "/desktopclaw.html"); found && claw.sessionID != "" {
			if cv, err := actions.evaluate(ctx, claw.sessionID, liveClawAPIProbeExpression); err == nil {
				if cm, ok := cv.(map[string]any); ok {
					check.Reason = ""
					if cm["byokSourcesList"] == true {
						check.ByokSourcesList = true
					}
					check.ModelImportAPI = cm["modelImportApi"] == true
				}
			}
		}
	}
	return check, true
}

const (
	liveAccountAPIProbeExpression = `(function(){
const api = globalThis.kimiAgentAPI;
const claw = globalThis.clawBridgeAPI;
const sources = api?.byokSources ?? claw?.byokSources;
return {
  getUserProfile: typeof api?.getUserProfile === "function",
  getByokProfile: typeof api?.getByokProfile === "function",
  byokSourcesList: typeof sources?.list === "function",
  modelImportApi: typeof claw?.importByokModels === "function" || (typeof claw?.byokSources?.list === "function" && typeof claw?.byokSources?.preview === "function" && typeof claw?.byokSources?.save === "function")
};
})()`

	liveClawAPIProbeExpression = `(function(){
const api = globalThis.clawBridgeAPI;
const sources = api?.byokSources;
return {
  byokSourcesList: typeof sources?.list === "function",
  modelImportApi: typeof api?.importByokModels === "function" || (typeof sources?.list === "function" && typeof sources?.preview === "function" && typeof sources?.save === "function")
};
})()`
)

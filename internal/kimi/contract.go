package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Contract states. They are the vocabulary the panel and the feature matrix
// use, and they are deliberately not interchangeable:
//
//	verified      the installed parser accepted every configured model
//	incompatible  the parser ran and refused or skipped something
//	unverified    the bundle could not be read, or the probe could not run
//	unavailable   there is no installation to probe
//
// Only `verified` may be reported as compatible. A probe that cannot run says
// nothing about whether the integration still works, and must never be shown as
// success.
const (
	StateVerified     = "verified"
	StateIncompatible = "incompatible"
	StateUnverified   = "unverified"
	StateUnavailable  = "unavailable"
)

// ContractResult is what the panel renders and what the feature matrix records.
type ContractResult struct {
	State      string            `json:"state"`
	Reason     string            `json:"reason,omitempty"`
	Archive    string            `json:"archive,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	ModelCount int               `json:"modelCount"`
	Version    string            `json:"version,omitempty"`
	SHA256     string            `json:"sha256,omitempty"`
	Contracts  map[string]bool   `json:"contracts,omitempty"`
	Parser     string            `json:"parser,omitempty"`
	Functions  int               `json:"functions,omitempty"`
	Pending    []string          `json:"pending,omitempty"`
	Accepted   []string          `json:"accepted,omitempty"`
	Skipped    []string          `json:"skipped,omitempty"`
	Families   map[string]string `json:"families,omitempty"`
}

// Supported reports whether the integration may be treated as compatible.
func (r ContractResult) Supported() bool { return r.State == StateVerified }

// ProbeOptions selects what to inspect. Zero values use the fixed sample
// catalog and the platform's default archive locations.
type ProbeOptions struct {
	// Archive overrides discovery. An explicit path is probed even when it does
	// not exist, so the caller learns `unavailable` rather than a silent
	// fallback to another installation.
	Archive string
	// Models overrides the sample catalog. A non-nil empty slice is a real
	// catalog with no models and is refused as such.
	Models []Model
	// Platform and Env exist so Windows discovery can be exercised on macOS.
	Platform string
	Env      map[string]string
	// Executor bounds the child process. Zero values take the defaults.
	Executor ExecutorConfig
	// SkipProcess runs the pipeline in this process. It exists for tests that
	// assert equivalence; production paths must not set it.
	SkipProcess bool
}

func (o ProbeOptions) platform() string {
	if o.Platform != "" {
		return o.Platform
	}
	return runtime.GOOS
}

func (o ProbeOptions) env() map[string]string {
	if o.Env != nil {
		return o.Env
	}
	out := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		out[name] = value
	}
	return out
}

// ArchiveCandidates lists the locations an official Kimi install may occupy.
// Known folders come first; anything discovered from a shortcut or a running
// process is appended so an existing installation keeps winning.
func ArchiveCandidates(platform string, env map[string]string) []string {
	switch platform {
	case "darwin":
		home := env["HOME"]
		out := []string{"/Applications/Kimi.app/Contents/Resources/app.asar"}
		if home != "" {
			out = append(out, filepath.Join(home, "Applications", "Kimi.app", "Contents", "Resources", "app.asar"))
		}
		return out
	case "win32", "windows":
		return windowsCandidates(env)
	}
	return nil
}

// Inspect runs one contract probe against the selected archive.
func Inspect(ctx context.Context, options ProbeOptions) ContractResult {
	archive := options.Archive
	if archive == "" {
		archive = firstExisting(ArchiveCandidates(options.platform(), options.env()))
	}
	models := options.Models
	scope := "configured-models"
	if models == nil {
		models = SampleModels()
		scope = "sample-models"
	}
	validated, err := ValidateModels(models)
	base := ContractResult{Archive: archive, Scope: scope, ModelCount: len(validated)}
	if err != nil {
		return withState(base, StateUnverified, "models_invalid")
	}
	// An empty catalog is not a probe: the parser would accept it by returning
	// nothing, which says nothing about whether the configured models import.
	if len(validated) == 0 {
		return withState(base, StateUnverified, "models_empty")
	}
	if archive == "" {
		return withState(base, StateUnavailable, "unsupported_platform")
	}
	if _, err := os.Stat(archive); err != nil {
		return withState(base, StateUnavailable, "archive_missing")
	}
	text, err := ReadAsarEntry(archive, EntryPath)
	if err != nil {
		return withState(base, StateUnavailable, "archive_unreadable")
	}

	base.Version = InstalledVersion(archive)
	base.SHA256, _ = HashFile(archive)
	base.Contracts = RuntimeContracts(text)

	catalog, err := json.Marshal(Catalog(validated))
	if err != nil {
		return withState(base, StateUnverified, "models_invalid")
	}
	outcome, err := executeJob(ctx, Job{Kind: "contract", Source: text, Catalog: catalog}, options)
	if err != nil {
		return withState(base, StateUnverified, reasonForExecution(err))
	}
	if outcome.Error != "" {
		base.Parser = stringField(outcome.Result, "parser")
		return withState(base, StateUnverified, outcome.Error)
	}
	return verdict(base, outcome.Result, validated)
}

// verdict turns the worker's report into the contract state. It refuses to
// certify anything it cannot recognise: a parser whose result shape changed is
// `unverified`, never an empty success.
func verdict(base ContractResult, result map[string]any, models []Model) ContractResult {
	if result == nil {
		return withState(base, StateUnverified, "parser_result_unrecognized")
	}
	base.Parser = stringField(result, "parser")
	base.Functions = intField(result, "functions")
	base.Pending = stringSlice(result["pending"])

	rows, rowsOK := result["models"].([]any)
	skipped, skippedOK := result["skippedModels"].([]any)
	if !rowsOK || !skippedOK {
		return withState(base, StateUnverified, "parser_result_unrecognized")
	}
	accepted := make([]string, 0, len(rows))
	families := map[string]string{}
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			return withState(base, StateUnverified, "parser_result_unrecognized")
		}
		model := stringField(object, "model")
		family := stringField(object, "family")
		if model == "" || family == "" {
			return withState(base, StateUnverified, "parser_result_unrecognized")
		}
		accepted = append(accepted, model)
		families[model] = family
	}
	skippedIDs := make([]string, 0, len(skipped))
	for _, row := range skipped {
		id, ok := row.(string)
		if !ok {
			return withState(base, StateUnverified, "parser_result_unrecognized")
		}
		skippedIDs = append(skippedIDs, id)
	}
	base.Accepted = SortedIDs(accepted)
	base.Skipped = skippedIDs
	base.Families = families

	if len(skippedIDs) > 0 {
		return withState(base, StateIncompatible, "models_skipped")
	}
	expected := make([]string, 0, len(models))
	for _, model := range models {
		expected = append(expected, model.ID)
	}
	sort.Strings(expected)
	if strings.Join(base.Accepted, "\x00") != strings.Join(expected, "\x00") {
		return withState(base, StateIncompatible, "model_list_mismatch")
	}
	if base.Contracts != nil && !base.Contracts["catalog_parser"] {
		return withState(base, StateUnverified, "parser_marker_missing")
	}
	return withState(base, StateVerified, "models_accepted")
}

// LaunchResult reports whether the packaged build still reads
// DAIMON_BUNDLE_DIR. Only `supported` means the bundle override can be used.
type LaunchResult struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// InspectLaunchContract probes the bundle-override reader of an archive.
func InspectLaunchContract(ctx context.Context, options ProbeOptions) LaunchResult {
	archive := options.Archive
	if archive == "" {
		archive = firstExisting(ArchiveCandidates(options.platform(), options.env()))
	}
	if archive == "" {
		return LaunchResult{State: StateUnavailable, Reason: "bundle_override_archive_unavailable"}
	}
	text, err := ReadAsarEntry(archive, EntryPath)
	if err != nil {
		return LaunchResult{State: StateUnavailable, Reason: "bundle_override_archive_unavailable"}
	}
	outcome, err := executeJob(ctx, Job{Kind: "launch", Source: text}, options)
	if err != nil {
		return LaunchResult{State: StateUnverified, Reason: "bundle_override_probe_failed"}
	}
	if outcome.Error != "" {
		return LaunchResult{State: StateUnverified, Reason: "bundle_override_probe_failed"}
	}
	return LaunchResult{State: stringField(outcome.Result, "state"), Reason: stringField(outcome.Result, "reason")}
}

func executeJob(ctx context.Context, job Job, options ProbeOptions) (Outcome, error) {
	if options.SkipProcess {
		return runInProcess(job, options.Executor)
	}
	return RunJob(ctx, job, options.Executor)
}

func reasonForExecution(err error) string {
	switch {
	case errors.Is(err, ErrProbeTimeout):
		return "contract_probe_timeout"
	case errors.Is(err, ErrProbeResource):
		return "contract_probe_resource_exceeded"
	case errors.Is(err, context.Canceled):
		return "contract_probe_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "contract_probe_timeout"
	}
	message := err.Error()
	if strings.HasPrefix(message, "contract_") {
		name, _, _ := strings.Cut(message, ":")
		return name
	}
	return "contract_probe_failed"
}

func withState(result ContractResult, state, reason string) ContractResult {
	result.State = state
	result.Reason = reason
	return result
}

func firstExisting(candidates []string) string {
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func stringField(object map[string]any, name string) string {
	if object == nil {
		return ""
	}
	value, _ := object[name].(string)
	return value
}

func intField(object map[string]any, name string) int {
	if object == nil {
		return 0
	}
	switch typed := object[name].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	}
	return 0
}

func stringSlice(value any) []string {
	rows, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if text, ok := row.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// Summarize renders the one-line verdict the panel shows.
func Summarize(result ContractResult) string {
	var out strings.Builder
	fmt.Fprintf(&out, "kimi: %s", orUnknown(result.Version))
	if result.SHA256 != "" {
		fmt.Fprintf(&out, " (app.asar %s…)", result.SHA256[:min(12, len(result.SHA256))])
	}
	switch result.State {
	case StateVerified:
		fmt.Fprintf(&out, "\nparser: verified — %d models accepted, 0 skipped", len(result.Accepted))
	case StateIncompatible:
		fmt.Fprintf(&out, "\nparser: INCOMPATIBLE — %s", result.Reason)
		if len(result.Skipped) > 0 {
			fmt.Fprintf(&out, ", skipped: %s", strings.Join(result.Skipped, ", "))
		}
	case StateUnverified:
		fmt.Fprintf(&out, "\nparser: unverified — %s (compatibility could not be determined)", result.Reason)
	default:
		fmt.Fprintf(&out, "\nparser: unavailable — %s", result.Reason)
	}
	return out.String()
}

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

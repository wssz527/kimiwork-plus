package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

const (
	diagnosticsSchemaVersion = 1
	diagnosticsEventLimit    = 200
	diagnosticsLogFilename   = "operations.json"
	diagnosticsFileLimit     = 2 << 20
	// diagnosticsRetryInterval bounds how often a failed history file is
	// retried, so an unreadable file is not re-read on every state poll.
	diagnosticsRetryInterval = 5 * time.Second

	diagnosticPhaseStart    = "start"
	diagnosticPhaseRejected = "rejected"
	diagnosticPhaseFinish   = "finish"
	diagnosticPhaseUnknown  = "unknown"

	diagnosticStateRunning = "running"

	diagnosticStagePrepare          = "prepare-byok"
	diagnosticStageImportModels     = "import-models"
	diagnosticStageImportDirect     = "import-direct"
	diagnosticStageDisconnectModels = "disconnect-models"
)

// diagnosticsFailure is a stored failure code. It is an error so the helpers
// below can return it, but its text is already the sanitized code the state
// document publishes: no path, no system error message.
type diagnosticsFailure string

func (f diagnosticsFailure) Error() string { return string(f) }

const (
	diagnosticsReadFailed       = diagnosticsFailure("diagnostics_read_failed")
	diagnosticsFileInvalid      = diagnosticsFailure("diagnostics_file_invalid")
	diagnosticsDirectoryInvalid = diagnosticsFailure("diagnostics_directory_invalid")
	diagnosticsWriteFailed      = diagnosticsFailure("diagnostics_write_failed")
)

// diagnosticImport is the only part of an import result that may be stored.
// It is a copy of the fields that are already published to the page; the
// source id, the readback, the aliases and the provider keys stay out.
type diagnosticImport struct {
	OK                  bool   `json:"ok"`
	State               string `json:"state,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Models              int    `json:"models,omitempty"`
	RequiresRestart     bool   `json:"requiresRestart,omitempty"`
	RestartDeferred     bool   `json:"restartDeferred,omitempty"`
	Applied             bool   `json:"applied,omitempty"`
	Adopted             bool   `json:"adopted,omitempty"`
	Opened              bool   `json:"opened,omitempty"`
	Closed              *bool  `json:"closed,omitempty"`
	RouteCleanupPending bool   `json:"routeCleanupPending,omitempty"`
}

type diagnosticEvent struct {
	Time       int64             `json:"time"`
	ID         string            `json:"id,omitempty"`
	Version    string            `json:"version"`
	BuildID    string            `json:"buildId"`
	Action     string            `json:"action"`
	Phase      string            `json:"phase"`
	State      string            `json:"state"`
	Reason     string            `json:"reason"`
	Stage      string            `json:"stage"`
	DurationMs int64             `json:"durationMs"`
	Import     *diagnosticImport `json:"import,omitempty"`
}

type diagnosticsFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Events        []diagnosticEvent `json:"events"`
}

// diagnosticsLog keeps a small, bounded, on-disk history of native actions.
// It owns its own lock: no caller holds Native.mu while this does file I/O.
//
// The in-memory state is split so an unreadable history file is never
// destroyed: `disk` holds what was last read successfully, `pending` holds the
// events recorded since, and nothing is written while the file's contents are
// still unknown. A later successful read merges both.
type diagnosticsLog struct {
	dir  string
	path string
	now  func() time.Time

	mu          sync.Mutex
	loaded      bool
	disk        []diagnosticEvent
	pending     []diagnosticEvent
	loadReason  string
	writeReason string
	retryAt     time.Time
}

func newDiagnosticsLog(stateDir string) *diagnosticsLog {
	if stateDir == "" {
		return nil
	}
	dir := filepath.Join(stateDir, "diagnostics")
	return &diagnosticsLog{dir: dir, path: filepath.Join(dir, diagnosticsLogFilename)}
}

func (l *diagnosticsLog) clock() time.Time {
	if l != nil && l.now != nil {
		return l.now()
	}
	return time.Now()
}

// refresh stamps the fields the log owns and sanitizes everything else. It is
// only used for events this process just recorded; loaded events keep the
// version and build that wrote them.
func (l *diagnosticsLog) refresh(event diagnosticEvent) diagnosticEvent {
	if event.Time == 0 {
		event.Time = l.clock().UnixMilli()
	}
	event.Version = Version
	event.BuildID = BuildID
	return sanitizeDiagnosticEvent(event)
}

func (l *diagnosticsLog) record(event diagnosticEvent) {
	if l == nil {
		return
	}
	event = l.refresh(event)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, event)
	if len(l.pending) > diagnosticsEventLimit {
		l.pending = append([]diagnosticEvent(nil), l.pending[len(l.pending)-diagnosticsEventLimit:]...)
	}
	l.ensureLoadedLocked()
	if !l.loaded {
		// The history on disk is unknown or unreadable. Overwriting it now would
		// replace whatever is there with only this session's events, so the new
		// events stay in memory until a read succeeds.
		return
	}
	if len(l.pending) == 0 {
		// Recovery already merged and wrote these events.
		return
	}
	if err := l.flushLocked(); err != nil {
		l.writeReason = diagnosticsWriteFailed.Error()
		return
	}
	l.writeReason = ""
}

// summary is the public shape /api/state publishes. It carries no path and no
// error text: the front end only needs to know whether history is usable.
func (l *diagnosticsLog) summary() core.Document {
	if l == nil {
		return core.Document{"available": false, "retained": 0, "limit": diagnosticsEventLimit, "lastAt": int64(0), "reason": "diagnostics_unconfigured"}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLoadedLocked()
	merged := l.mergedLocked()
	lastAt := int64(0)
	if len(merged) > 0 {
		lastAt = merged[len(merged)-1].Time
	}
	reason := l.loadReason
	if reason == "" {
		reason = l.writeReason
	}
	return core.Document{"available": l.loaded && reason == "", "retained": len(merged), "limit": diagnosticsEventLimit, "lastAt": lastAt, "reason": reason}
}

// history returns the retained events as a deep copy: a caller can not reach
// the log's own slice or the pointers inside it.
func (l *diagnosticsLog) history() []diagnosticEvent {
	if l == nil {
		return []diagnosticEvent{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensureLoadedLocked()
	merged := l.mergedLocked()
	out := make([]diagnosticEvent, len(merged))
	for i, event := range merged {
		out[i] = cloneDiagnosticEvent(event)
	}
	return out
}

// mergedLocked is the view of the retained history. While the file can not be
// read it is just this session's bounded events.
func (l *diagnosticsLog) mergedLocked() []diagnosticEvent {
	if len(l.pending) == 0 {
		return l.disk
	}
	merged := make([]diagnosticEvent, 0, len(l.disk)+len(l.pending))
	merged = append(merged, l.disk...)
	merged = append(merged, l.pending...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Time < merged[j].Time })
	if len(merged) > diagnosticsEventLimit {
		merged = merged[len(merged)-diagnosticsEventLimit:]
	}
	return merged
}

func (l *diagnosticsLog) ensureLoadedLocked() {
	if l.loaded {
		return
	}
	now := l.clock()
	if !l.retryAt.IsZero() && now.Before(l.retryAt) {
		return
	}
	l.retryAt = time.Time{}
	if err := l.loadLocked(); err != nil {
		l.loadReason = err.Error()
		l.retryAt = now.Add(diagnosticsRetryInterval)
		return
	}
	l.loaded = true
	l.loadReason = ""
	if len(l.pending) == 0 {
		return
	}
	// The events recorded while the file was unreadable are merged into what
	// was read, then written back once. Only the successful read may trigger
	// this: an unreadable file is never replaced by this session's events.
	if err := l.flushLocked(); err != nil {
		l.writeReason = diagnosticsWriteFailed.Error()
		return
	}
	l.writeReason = ""
}

// loadLocked reads the history once. A missing file is an empty history; a
// symlinked, irregular or world-readable parent, and any file core.ReadPrivateFile
// refuses, are reported as failures and leave `loaded` false so the next call
// retries instead of writing over contents that were never read.
func (l *diagnosticsLog) loadLocked() error {
	if info, err := os.Lstat(l.dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return diagnosticsDirectoryInvalid
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return diagnosticsDirectoryInvalid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return diagnosticsReadFailed
	}
	data, err := core.ReadPrivateFile(l.path, diagnosticsFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		l.disk = nil
		return nil
	}
	if err != nil {
		return diagnosticsReadFailed
	}
	var file diagnosticsFile
	if json.Unmarshal(data, &file) != nil || file.SchemaVersion != diagnosticsSchemaVersion {
		return diagnosticsFileInvalid
	}
	events := file.Events
	if len(events) > diagnosticsEventLimit {
		events = events[len(events)-diagnosticsEventLimit:]
	}
	loaded := make([]diagnosticEvent, 0, len(events))
	for _, event := range events {
		loaded = append(loaded, sanitizeDiagnosticEvent(event))
	}
	l.disk = loaded
	return nil
}

func (l *diagnosticsLog) flushLocked() error {
	merged := l.mergedLocked()
	if err := l.persistLocked(merged); err != nil {
		return err
	}
	if merged == nil {
		merged = []diagnosticEvent{}
	}
	l.disk = merged
	l.pending = nil
	return nil
}

// persistLocked writes the history with the same directory guarantees
// core.PrivateDirectory enforces for other private state: 0700, a real
// directory rather than a link, and no group or other access on Unix.
func (l *diagnosticsLog) persistLocked(events []diagnosticEvent) error {
	if err := core.PrivateDirectory(l.dir); err != nil {
		return err
	}
	if events == nil {
		events = []diagnosticEvent{}
	}
	return core.AtomicJSON(l.path, diagnosticsFile{SchemaVersion: diagnosticsSchemaVersion, Events: events})
}

// diagnosticText keeps short machine-readable values and replaces anything
// that could be a URL, a path or free text. Callers only pass codes, states,
// stages and build identifiers through it.
func diagnosticText(value string, max int) string {
	if value == "" {
		return ""
	}
	if len(value) > max {
		return "redacted"
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '+':
		default:
			return "redacted"
		}
	}
	return value
}

// diagnosticCodeList accepts the two list shapes the compatibility report
// builds: `missing:a,b` from the account API check and `a,b` from the guard
// features. Each element is a code, so a URL or a path is refused instead of
// being let through by a wider character set.
func diagnosticCodeList(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 256 {
		return "redacted"
	}
	prefix := ""
	if rest, ok := strings.CutPrefix(value, "missing:"); ok {
		prefix, value = "missing:", rest
	}
	parts := strings.Split(value, ",")
	if len(parts) > 12 {
		return "redacted"
	}
	codes := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if !diagnosticCode(part) {
			return "redacted"
		}
		codes = append(codes, part)
	}
	return prefix + strings.Join(codes, ",")
}

func diagnosticCode(value string) bool {
	if value == "" || len(value) > 64 || strings.Contains(value, "..") {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		case r == '.' || r == '-':
			if i == 0 || i == len(value)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// diagnosticVersion and diagnosticBuildID keep a historical build identity but
// refuse anything shaped like a credential. A real version ("1.1.5",
// "1.1.6-preview.1") and a real build id ("development", "1.1.5-a26ef4a") are
// short and separated; a token or key is one long unbroken run.
func diagnosticVersion(value string) string {
	if value == "" {
		return ""
	}
	if !diagnosticBuildText(value, 32) || value[0] < '0' || value[0] > '9' {
		return "redacted"
	}
	return value
}

func diagnosticBuildID(value string) string {
	if value == "" {
		return ""
	}
	if value == "development" || diagnosticCommitBuildID(value) {
		return value
	}
	return "redacted"
}

// diagnosticCommitBuildID matches the shapes this project stamps: a bare short
// commit (`a26ef4a`) and the versioned form `1.1.5-a26ef4a` /
// `1.1.6-preview.1-b791b81`. A word or a long unbroken token matches neither.
func diagnosticCommitBuildID(value string) bool {
	if len(value) > 40 {
		return false
	}
	index := strings.LastIndexByte(value, '-')
	if index < 0 {
		return diagnosticCommit(value)
	}
	if index == 0 || index == len(value)-1 {
		return false
	}
	return diagnosticCommit(value[index+1:]) && diagnosticBuildText(value[:index], 32) && value[0] >= '0' && value[0] <= '9'
}

func diagnosticCommit(value string) bool {
	if len(value) < 7 || len(value) > 12 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func diagnosticBuildText(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	run := 0
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			run++
			if run >= 24 {
				return false
			}
		case r == '.', r == '-', r == '+':
			run = 0
		default:
			return false
		}
	}
	return true
}

func diagnosticAction(action string) string {
	if _, ok := diagnosticActions[action]; ok {
		return action
	}
	return "unknown"
}

// diagnosticReason keeps a short machine code. Every reason the log records is
// an error code (`[a-z][a-z0-9_]*`) or an import reason of the same shape, so
// anything else — including a hyphenated token — is refused.
func diagnosticReason(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 100 || value[0] < 'a' || value[0] > 'z' {
		return "redacted"
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return "redacted"
		}
	}
	return value
}

// diagnosticImportState accepts only the documented ImportState values, so a
// free-text or hand-edited value can not reach the export.
func diagnosticImportState(value string) string {
	switch value {
	case "":
		return ""
	case kimi.ImportStateWaitingLogin, kimi.ImportStateWaitingWork, kimi.ImportStateWaitingSettings,
		kimi.ImportStateContractChanged, kimi.ImportStateAccountChanged, kimi.ImportStateRequiresRestart,
		kimi.ImportStateApplied, kimi.ImportStateAccepted, kimi.ImportStateFailed:
		return value
	}
	return "redacted"
}

// diagnosticStage accepts only the stages this product sets.
func diagnosticStage(value string) string {
	switch value {
	case "", diagnosticStagePrepare, diagnosticStageImportModels, diagnosticStageImportDirect, diagnosticStageDisconnectModels:
		return value
	}
	return "redacted"
}

func diagnosticPhase(phase string) string {
	switch phase {
	case diagnosticPhaseStart, diagnosticPhaseRejected, diagnosticPhaseFinish:
		return phase
	}
	return diagnosticPhaseUnknown
}

var diagnosticActions = map[string]struct{}{
	"launch-kimi": {}, "open-work": {}, "restart-kimi": {}, "connect-kimi": {},
	"disconnect-models": {}, "prepare-native": {}, "remove-dns": {}, "check-translation": {},
	"check-compatibility": {}, "apply-connections": {}, "account-capture": {},
	"account-begin-add": {}, "account-complete-add": {}, "account-cancel-add": {},
	"account-switch": {}, "account-remove": {}, "set-swarm": {}, "check-upstream": {},
}

// sanitizeDiagnosticEvent rebuilds an event from known fields only. It is used
// for events this process records and for events read back from disk, so a
// hand-edited or older file can not inject anything the writer never stores.
// The version and build id are sanitized, never replaced: a historical event
// must keep the build that produced it.
func sanitizeDiagnosticEvent(event diagnosticEvent) diagnosticEvent {
	out := diagnosticEvent{
		Time:       event.Time,
		ID:         diagnosticText(event.ID, 32),
		Version:    diagnosticVersion(event.Version),
		BuildID:    diagnosticBuildID(event.BuildID),
		Action:     diagnosticAction(event.Action),
		Phase:      diagnosticPhase(event.Phase),
		State:      diagnosticText(event.State, 40),
		Reason:     diagnosticReason(event.Reason),
		Stage:      diagnosticStage(event.Stage),
		DurationMs: event.DurationMs,
		Import:     sanitizeDiagnosticImport(event.Import),
	}
	if out.ID != "" && out.ID == "redacted" {
		out.ID = ""
	}
	if out.Time < 0 {
		out.Time = 0
	}
	if out.DurationMs < 0 {
		out.DurationMs = 0
	}
	return out
}

func cloneDiagnosticEvent(event diagnosticEvent) diagnosticEvent {
	out := event
	out.Import = sanitizeDiagnosticImport(event.Import)
	return out
}

func sanitizeDiagnosticImport(in *diagnosticImport) *diagnosticImport {
	if in == nil {
		return nil
	}
	out := &diagnosticImport{
		OK:                  in.OK,
		State:               diagnosticImportState(in.State),
		Reason:              diagnosticReason(in.Reason),
		Models:              in.Models,
		RequiresRestart:     in.RequiresRestart,
		RestartDeferred:     in.RestartDeferred,
		Applied:             in.Applied,
		Adopted:             in.Adopted,
		Opened:              in.Opened,
		RouteCleanupPending: in.RouteCleanupPending,
	}
	if in.Models < 0 {
		out.Models = 0
	}
	if in.Closed != nil {
		closed := *in.Closed
		out.Closed = &closed
	}
	if diagnosticImportEmpty(out) {
		return nil
	}
	return out
}

func diagnosticImportEmpty(imported *diagnosticImport) bool {
	return !imported.OK && imported.State == "" && imported.Reason == "" && imported.Models == 0 &&
		!imported.RequiresRestart && !imported.RestartDeferred && !imported.Opened &&
		imported.Closed == nil && !imported.Applied && !imported.Adopted && !imported.RouteCleanupPending
}

func diagnosticImportOf(value any) *diagnosticImport {
	var result kimi.ImportResult
	switch v := value.(type) {
	case kimi.ImportResult:
		result = v
	case *kimi.ImportResult:
		if v == nil {
			return nil
		}
		result = *v
	default:
		return nil
	}
	out := &diagnosticImport{
		OK:                  result.OK,
		State:               result.State,
		Reason:              result.Reason,
		Models:              result.Models,
		RequiresRestart:     result.RequiresRestart,
		RestartDeferred:     result.RestartDeferred,
		Applied:             result.Applied,
		Adopted:             result.Adopted,
		Opened:              result.Opened,
		RouteCleanupPending: result.RouteCleanupPending,
	}
	if result.Closed != nil {
		closed := *result.Closed
		out.Closed = &closed
	}
	return sanitizeDiagnosticImport(out)
}

func documentInt64(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func (n *Native) logOperationStarted(id, action string) {
	n.diagnostics.record(diagnosticEvent{ID: id, Action: action, Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
}

func (n *Native) logOperationRejected(action, reason string) {
	n.diagnostics.record(diagnosticEvent{Action: action, Phase: diagnosticPhaseRejected, State: diagnosticPhaseRejected, Reason: reason})
}

func (n *Native) logOperationFinished(id, action, stage, state, reason string, durationMs int64, result any) {
	n.diagnostics.record(diagnosticEvent{ID: id, Action: action, Phase: diagnosticPhaseFinish, State: state, Reason: reason, Stage: stage, DurationMs: durationMs, Import: diagnosticImportOf(result)})
}

// setOperationStage publishes a phase marker on the in-memory operation. It
// never touches the filesystem, so holding Native.mu here stays cheap.
func (n *Native) setOperationStage(id, stage string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.operation == nil || n.operation["id"] != id {
		return
	}
	if stage == "" {
		delete(n.operation, "stage")
		return
	}
	n.operation["stage"] = stage
}

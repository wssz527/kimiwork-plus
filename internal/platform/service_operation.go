package platform

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// A service operation is the durable record of one lifecycle change: which
// action ran, who ran it, whether the previous daemon was drained, what was read
// back, and why it failed.
//
// The journal exists because a lifecycle change cannot be answered by the
// process it unloads. A caller gets an operation id immediately, and the answer
// is the journal entry — never an optimistic "started".
const (
	OperationRegister     = "register-service"
	OperationLoginStart   = "login-start"
	OperationStartService = "start-service"
	OperationRestart      = "restart-service"
	OperationStopService  = "stop-service"
	OperationUninstall    = "uninstall-service"
)

// Operation phases. Only the first two are non-terminal.
const (
	PhasePending   = "pending"
	PhaseRunning   = "running"
	PhaseSucceeded = "succeeded"
	PhaseFailed    = "failed"
)

// Executors. "helper" is a short-lived detached process that outlives the
// daemon it replaces; "inline" runs inside the caller and is only used for
// changes that do not unload, kill or restart the caller.
const (
	ExecutorInline = "inline"
	ExecutorHelper = "helper"
)

// Termination names the mechanism that stopped the previous daemon. Only
// "http_drain" is graceful; the others are reported as forced, never as a drain.
const (
	TerminationDrain   = "http_drain"
	TerminationLaunchd = "launchd_bootout"
	// TerminationWindows is the daemon's own --terminate-daemon path. It is not a
	// platform kill: the process verifies the recorded pid still belongs to it
	// before ending it, so a reused pid cannot take an unrelated program with it.
	TerminationWindows = "daemon_terminate"
	TerminationNone    = "none"
)

// ErrOperationBusy means another lifecycle change owns the state directory.
// There is deliberately no queue: two concurrent reloads of one job cannot both
// be right, and a queue would hide the second click behind the first.
var ErrOperationBusy = errors.New("service_operation_busy")

// ErrOperationNotFound means no journal entry has that id.
var ErrOperationNotFound = errors.New("service_operation_not_found")

// OperationStep is one observable step. Output is a short tail of a command's
// own output; the helper only runs launchctl, reg and its own HTTP client,
// so this never carries a credential.
type OperationStep struct {
	Name       string    `json:"name"`
	OK         bool      `json:"ok"`
	Reason     string    `json:"reason,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Output     string    `json:"output,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

// ServiceOperation is what a panel polls.
type ServiceOperation struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Executor is ExecutorInline or ExecutorHelper.
	Executor string `json:"executor"`
	// HelperPID is the detached executor's pid, for diagnostics only. It is
	// never used to decide whether an operation is alive.
	HelperPID int `json:"helperPid,omitempty"`
	// ExpectedBuildID is the build that requested the change. ServingBuildID is
	// what the daemon reported after it; a mismatch fails the operation.
	ExpectedBuildID string `json:"expectedBuildId,omitempty"`
	ServingBuildID  string `json:"servingBuildId,omitempty"`
	// LoginStart is the requested preference, when the action sets it.
	LoginStart *bool `json:"loginStart,omitempty"`
	// Drained is true only when the previous daemon accepted the authenticated
	// stop and stopped answering before the change.
	Drained bool `json:"drained"`
	// Forced is true when the operation had to fall back to a platform
	// termination. A forced stop is never reported as a graceful one.
	Forced bool `json:"forced"`
	// Termination is one of the Termination* constants.
	Termination string `json:"termination,omitempty"`
	// Sequence orders records created in the same instant, which a clock with a
	// coarse resolution does. It is a monotonic counter, never a timestamp, so
	// ordering cannot depend on the wall clock going backwards.
	Sequence  int64     `json:"sequence,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Deadline  time.Time `json:"deadline,omitempty"`
	// State is the ManagerState read back after the operation, success or not.
	State *ManagerState   `json:"state,omitempty"`
	Steps []OperationStep `json:"steps,omitempty"`
}

// Terminal reports whether the operation can no longer change.
func (o ServiceOperation) Terminal() bool {
	return o.Phase == PhaseSucceeded || o.Phase == PhaseFailed
}

// OperationSpec is what a caller asks for.
type OperationSpec struct {
	// Action is one of the Operation* constants.
	Action string
	// LoginStart is the requested preference for OperationLoginStart.
	LoginStart bool
	// Force permits a platform termination after a failed drain. It never turns
	// a forced termination into a reported graceful stop.
	Force bool
	// Executor overrides the default: helper for a destructive action, inline
	// otherwise.
	Executor string
}

// OperationForAction maps a panel action name to an operation. The names are the
// ones the bridge and the panel already send, so wiring the controller does not
// rename anything the front end knows.
func OperationForAction(action string) (OperationSpec, bool) {
	switch action {
	case "register-service":
		return OperationSpec{Action: OperationRegister}, true
	case "enable-login-service":
		return OperationSpec{Action: OperationLoginStart, LoginStart: true}, true
	case "disable-login-service":
		return OperationSpec{Action: OperationLoginStart, LoginStart: false}, true
	case "start-service":
		return OperationSpec{Action: OperationStartService}, true
	case "restart-service":
		return OperationSpec{Action: OperationRestart}, true
	case "stop-service":
		return OperationSpec{Action: OperationStopService}, true
	case "uninstall-service":
		return OperationSpec{Action: OperationUninstall}, true
	}
	return OperationSpec{}, false
}

// Destruction means "unloads, kills or replaces the running daemon". Those
// changes must be completed by a process the daemon does not own.
func (s OperationSpec) Destructive() bool {
	switch s.Action {
	case OperationStartService, OperationRestart, OperationStopService, OperationUninstall:
		return true
	}
	return false
}

// Drains reports whether the action requires the running daemon to be stopped
// gracefully before the platform step.
func (s OperationSpec) Drains() bool {
	switch s.Action {
	case OperationRestart, OperationStopService, OperationUninstall:
		return true
	}
	return false
}

func knownAction(action string) bool {
	switch action {
	case OperationRegister, OperationLoginStart, OperationStartService,
		OperationRestart, OperationStopService, OperationUninstall:
		return true
	}
	return false
}

const (
	operationDirectoryName = "service-operations"
	operationLockName      = "operator.lock"
	// staleGrace is how long past its deadline an operation may stay unfinished
	// before a reader declares the executor lost. It is a deadline, not a pid
	// probe: a reused pid would otherwise keep a dead operation alive forever.
	staleGrace = 2 * time.Minute
	// keepOperations bounds journal growth without a background janitor.
	keepOperations = 20
	// generationDirectoryName holds the short-lived helper executable that the
	// controller copies out of the application bundle before starting it.
	generationDirectoryName = "service-helper"
)

func operationDirectory(stateDir string) string {
	return filepath.Join(stateDir, operationDirectoryName)
}

func ensureOperationDirectory(stateDir string) (string, error) {
	if stateDir == "" {
		return "", errors.New("service_state_dir_missing")
	}
	if !filepath.IsAbs(stateDir) {
		return "", errors.New("service_state_dir_not_absolute")
	}
	dir := operationDirectory(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("service_operation_dir_invalid")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func newOperationID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func validOperationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

func operationPath(dir, id string) string { return filepath.Join(dir, id+".json") }
func requestPath(dir, id string) string   { return filepath.Join(dir, id+".request.json") }
func helperLogPath(dir, id string) string { return filepath.Join(dir, id+".helper.log") }

// writePrivateJSON writes a record atomically. A half-written journal entry
// would be indistinguishable from a failed operation.
func writePrivateJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(0o600); err == nil {
		if _, err = temp.Write(encoded); err == nil {
			err = temp.Sync()
		}
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readPrivateJSON(path string, limit int64, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("service_operation_invalid")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("service_operation_insecure")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("service_operation_invalid")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(content)) > limit {
		return errors.New("service_operation_invalid")
	}
	return json.Unmarshal(content, value)
}

func writeOperation(dir string, operation ServiceOperation) error {
	return writePrivateJSON(operationPath(dir, operation.ID), operation)
}

func readOperationRecord(dir, id string) (ServiceOperation, error) {
	if !validOperationID(id) {
		return ServiceOperation{}, errors.New("invalid_operation_id")
	}
	var operation ServiceOperation
	if err := readPrivateJSON(operationPath(dir, id), 64*1024, &operation); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ServiceOperation{}, ErrOperationNotFound
		}
		return ServiceOperation{}, err
	}
	if operation.ID != id {
		return ServiceOperation{}, errors.New("service_operation_identity_mismatch")
	}
	return operation, nil
}

func readOperationRequest(dir, id string) (OperationRequest, error) {
	if !validOperationID(id) {
		return OperationRequest{}, errors.New("invalid_operation_id")
	}
	var request OperationRequest
	if err := readPrivateJSON(requestPath(dir, id), 64*1024, &request); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OperationRequest{}, ErrOperationNotFound
		}
		return OperationRequest{}, err
	}
	if request.ID != id {
		return OperationRequest{}, errors.New("service_operation_identity_mismatch")
	}
	return request, nil
}

// listOperations returns the newest records first. One unreadable entry is
// skipped so the panel still renders the rest.
func listOperations(dir string, limit int) []ServiceOperation {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	operations := make([]ServiceOperation, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" || len(name) != 32+len(".json") {
			continue
		}
		operation, err := readOperationRecord(dir, name[:32])
		if err != nil {
			continue
		}
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		left, right := operations[i], operations[j]
		if left.Sequence != right.Sequence {
			return left.Sequence > right.Sequence
		}
		if !left.StartedAt.Equal(right.StartedAt) {
			return left.StartedAt.After(right.StartedAt)
		}
		return left.ID > right.ID
	})
	if limit > 0 && len(operations) > limit {
		operations = operations[:limit]
	}
	return operations
}

// pruneOperations keeps the journal bounded. It runs after an operation
// finishes, never on a timer, so there is no janitor process.
func pruneOperations(dir string) {
	keep := map[string]bool{}
	for index, operation := range listOperations(dir, 0) {
		if index < keepOperations {
			keep[operation.ID] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) <= 32 {
			continue
		}
		id := name[:32]
		if !validOperationID(id) || keep[id] {
			continue
		}
		switch name[32:] {
		case ".json", ".helper.log", ".request.json":
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

type operationLockRecord struct {
	OperationID string    `json:"operationId"`
	PID         int       `json:"pid"`
	At          time.Time `json:"at"`
	Deadline    time.Time `json:"deadline"`
}

type operationLock struct {
	path string
	id   string
}

// acquireOperationLock grants the single destructive-change slot. Ownership is
// handed to the helper: the controller writes the lock, the helper keeps it, and
// only the finish path removes it.
func acquireOperationLock(dir, id string, now, deadline time.Time) (*operationLock, error) {
	path := filepath.Join(dir, operationLockName)
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			record := operationLockRecord{OperationID: id, PID: os.Getpid(), At: now, Deadline: deadline}
			encoded, _ := json.Marshal(record)
			_, writeErr := file.Write(append(encoded, '\n'))
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(path)
				if writeErr != nil {
					return nil, writeErr
				}
				return nil, closeErr
			}
			return &operationLock{path: path, id: id}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		existing, readErr := readLockRecord(path)
		if readErr != nil || now.After(existing.Deadline.Add(staleGrace)) {
			// A lock whose owner is past its deadline cannot be doing useful
			// work any more. Taking it over is what keeps a crashed executor
			// from blocking the panel forever.
			_ = os.Remove(path)
			continue
		}
		return nil, ErrOperationBusy
	}
	return nil, ErrOperationBusy
}

func readLockRecord(path string) (operationLockRecord, error) {
	var record operationLockRecord
	content, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if len(content) > 4096 || json.Unmarshal(content, &record) != nil || !validOperationID(record.OperationID) {
		return record, errors.New("service_operation_lock_invalid")
	}
	return record, nil
}

func (l *operationLock) Release() {
	if l == nil || l.path == "" {
		return
	}
	record, err := readLockRecord(l.path)
	if err == nil && record.OperationID != l.id {
		// Someone else owns the slot now; removing their lock would let two
		// executors run at once.
		return
	}
	_ = os.Remove(l.path)
	l.path = ""
}

// lockHolder reports which operation owns the slot, or "" when it is free.
func lockHolder(dir string) string {
	record, err := readLockRecord(filepath.Join(dir, operationLockName))
	if err != nil {
		return ""
	}
	return record.OperationID
}

// OperationRequest is the helper's entire input. It is written next to the
// journal entry, so the helper never depends on the caller's memory or argv.
type OperationRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`

	LoginStart bool `json:"loginStart,omitempty"`
	Force      bool `json:"force,omitempty"`

	Binary   string `json:"binary"`
	StateDir string `json:"stateDir"`
	LogPath  string `json:"logPath,omitempty"`
	Port     string `json:"port,omitempty"`
	BuildID  string `json:"buildId"`
	Home     string `json:"home,omitempty"`
	UID      string `json:"uid,omitempty"`

	// HelperPath is the executable the caller spawned. The helper refuses to run
	// the request when its own path differs, which catches a replaced binary.
	HelperPath string `json:"helperPath,omitempty"`

	DrainTimeoutMS int64 `json:"drainTimeoutMs,omitempty"`
	StepTimeoutMS  int64 `json:"stepTimeoutMs,omitempty"`
	DeadlineMS     int64 `json:"deadlineMs,omitempty"`
	// StartDelayMS lets the caller's HTTP response reach the client before the
	// drain stops the daemon that is writing it.
	StartDelayMS int64 `json:"startDelayMs,omitempty"`
}

func (r OperationRequest) spec() OperationSpec {
	return OperationSpec{Action: r.Action, LoginStart: r.LoginStart, Force: r.Force}
}

func (r OperationRequest) managerOptions(host ManagerHost) ManagerOptions {
	return ManagerOptions{Binary: r.Binary, StateDir: r.StateDir, LogPath: r.LogPath,
		Port: r.Port, BuildID: r.BuildID, Home: r.Home, UID: r.UID, Host: host}
}

func requestDuration(milliseconds int64, fallback time.Duration) time.Duration {
	if milliseconds <= 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func (r OperationRequest) drainTimeout() time.Duration {
	return requestDuration(r.DrainTimeoutMS, 15*time.Second)
}
func (r OperationRequest) stepTimeout() time.Duration {
	return requestDuration(r.StepTimeoutMS, 20*time.Second)
}
func (r OperationRequest) deadline() time.Duration {
	return requestDuration(r.DeadlineMS, 90*time.Second)
}
func (r OperationRequest) startDelay() time.Duration {
	return requestDuration(r.StartDelayMS, 250*time.Millisecond)
}

func validateRequest(request OperationRequest) error {
	if !validOperationID(request.ID) {
		return errors.New("invalid_operation_id")
	}
	if !knownAction(request.Action) {
		return errors.New("invalid_service_action")
	}
	if !filepath.IsAbs(request.Binary) {
		return errors.New("service_binary_not_absolute")
	}
	if !filepath.IsAbs(request.StateDir) {
		return errors.New("service_state_dir_not_absolute")
	}
	if request.BuildID == "" {
		return errors.New("service_build_id_missing")
	}
	return nil
}

// reasonOf turns an error into a short machine-readable code. The panel shows a
// sentence of its own; the code is what a log or a bug report quotes.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	code := err.Error()
	if index := strings.IndexAny(code, ": "); index > 0 {
		code = code[:index]
	}
	if code == "" || len(code) > 100 || code[0] < 'a' || code[0] > 'z' {
		return "service_operation_failed"
	}
	for _, character := range code {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return "service_operation_failed"
		}
	}
	return code
}

func shortOutput(output string) string {
	const limit = 400
	lines := strings.Split(strings.TrimSpace(output), "\n")
	kept := make([]string, 0, len(lines))
	size := 0
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if size+len(line) > limit && len(kept) > 0 {
			break
		}
		kept = append(kept, line)
		size += len(line) + 1
	}
	joined := strings.Join(kept, "\n")
	if len(joined) > limit {
		return joined[:limit]
	}
	return joined
}

func boolPointer(value bool) *bool { return &value }

// sequenceSource is the process-wide monotonic counter behind ServiceOperation
// .Sequence. It starts from the wall clock so records from an earlier process
// still sort sensibly, and never repeats.
var sequenceSource atomic.Int64

func nextSequence(now time.Time) int64 {
	candidate := now.UnixNano()
	for {
		current := sequenceSource.Load()
		next := candidate
		if next <= current {
			next = current + 1
		}
		if sequenceSource.CompareAndSwap(current, next) {
			return next
		}
	}
}

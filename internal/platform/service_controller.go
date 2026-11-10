package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Controller owns every lifecycle change for one state directory.
//
// The rule it exists to enforce: a change that unloads, kills or replaces the
// running daemon must not be completed by that daemon. Begin records the intent,
// takes the single change slot, and either finishes an operation that cannot be
// interrupted (writing a login-start preference) or spawns a short-lived helper
// that outlives the daemon it is replacing. There is no long-lived supervisor:
// the helper exits, and the journal is the answer.
type Controller interface {
	// Begin starts one operation. A non-destructive operation runs inline and
	// returns its terminal record; a destructive one returns a pending record
	// with the helper's pid, and the caller polls Read.
	Begin(ctx context.Context, spec OperationSpec) (ServiceOperation, error)
	// Read returns one journal entry. A record whose helper died is reported as
	// failed without a separate recovery step.
	Read(ctx context.Context, id string) (ServiceOperation, error)
	// List returns the newest entries first.
	List(ctx context.Context, limit int) ([]ServiceOperation, error)
	// Active returns the operation holding the change slot, if any.
	Active(ctx context.Context) (ServiceOperation, bool, error)
	// Status reads the service state without changing it.
	Status(ctx context.Context) (ManagerState, error)
}

// ControllerOptions configures the controller. ManagerOptions carries the service
// identity; the extra fields control how the helper is launched.
type ControllerOptions struct {
	ManagerOptions
	// HelperPath is an executable that understands HelperFlag. Empty means the
	// current executable, which is correct when the host binary delegates the
	// flag to RunHelperFromArgs.
	HelperPath string
	// Spawn replaces the real process launch. A test uses it to run the helper
	// in-process; production leaves it nil.
	Spawn func(HelperStart) (int, error)
	// StartDelay is how long the helper waits before touching the daemon, so the
	// caller's HTTP response reaches the panel. Zero means 250ms.
	StartDelay time.Duration
	// Deadline bounds one operation. Zero means 90 seconds.
	Deadline time.Duration
}

type controller struct {
	options ControllerOptions
	manager Manager
	goos    string
}

// OpenController builds a controller for this host.
func OpenController(options ControllerOptions) (Controller, error) {
	manager, err := OpenManager(options.ManagerOptions)
	if err != nil {
		return nil, err
	}
	goos := options.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	return &controller{options: options, manager: manager, goos: goos}, nil
}

func (c *controller) Status(ctx context.Context) (ManagerState, error) {
	return c.manager.Status(ctx)
}

func (c *controller) operationDir() (string, error) {
	return ensureOperationDirectory(c.options.StateDir)
}

func (c *controller) startDelay() time.Duration {
	if c.options.StartDelay > 0 {
		return c.options.StartDelay
	}
	return 250 * time.Millisecond
}

func (c *controller) deadline() time.Duration {
	if c.options.Deadline > 0 {
		return c.options.Deadline
	}
	return 90 * time.Second
}

// drainTimeout is the service's own budget. It lives on ManagerOptions because
// the inline Stop uses it too; keeping one field means a caller cannot set a
// value the helper silently ignores.
func (c *controller) drainTimeout() time.Duration {
	if c.options.ManagerOptions.DrainTimeout > 0 {
		return c.options.ManagerOptions.DrainTimeout
	}
	return 15 * time.Second
}

// Begin starts one lifecycle change.
func (c *controller) Begin(ctx context.Context, spec OperationSpec) (ServiceOperation, error) {
	if !knownAction(spec.Action) {
		return ServiceOperation{}, errors.New("invalid_service_action")
	}
	dir, err := c.operationDir()
	if err != nil {
		return ServiceOperation{}, err
	}
	id, err := newOperationID()
	if err != nil {
		return ServiceOperation{}, err
	}
	now := c.now()
	deadline := now.Add(c.deadline())
	operation := ServiceOperation{
		ID: id, Action: spec.Action, Phase: PhasePending, Executor: ExecutorInline,
		ExpectedBuildID: c.options.BuildID, Sequence: nextSequence(now),
		StartedAt: now, UpdatedAt: now, Deadline: deadline,
		Termination: TerminationNone,
	}
	if spec.Action == OperationLoginStart {
		operation.LoginStart = boolPointer(spec.LoginStart)
	}
	if spec.Executor != "" {
		operation.Executor = spec.Executor
	} else if spec.Destructive() {
		operation.Executor = ExecutorHelper
	}
	if operation.Executor == ExecutorHelper && !spec.Destructive() {
		return ServiceOperation{}, errors.New("service_helper_not_required")
	}

	lock, err := acquireOperationLock(dir, id, now, deadline)
	if err != nil {
		return ServiceOperation{}, err
	}
	// The lock is written before the record so a reader never sees a pending
	// operation without an owner.
	if err := writeOperation(dir, operation); err != nil {
		lock.Release()
		return ServiceOperation{}, err
	}
	if operation.Executor == ExecutorInline {
		return c.runInline(ctx, dir, operation, spec, lock)
	}
	started, err := c.startHelper(dir, operation, spec, lock)
	if err != nil {
		lock.Release()
		failed := c.finish(operation, TerminationNone, errorReasonOf(err), err, ManagerState{})
		failed.Detail = detailOf(err)
		if writeErr := writeOperation(dir, failed); writeErr != nil {
			return failed, writeErr
		}
		return failed, err
	}
	return started, nil
}

// runInline performs an operation that cannot interrupt the caller: writing a
// definition or a login-start preference. It never drains, unloads or kills.
func (c *controller) runInline(ctx context.Context, dir string, operation ServiceOperation,
	spec OperationSpec, lock *operationLock) (ServiceOperation, error) {
	defer lock.Release()
	operation.Phase = PhaseRunning
	operation.UpdatedAt = c.now()
	if err := writeOperation(dir, operation); err != nil {
		return operation, err
	}
	stepCtx, cancel := context.WithTimeout(ctx, c.deadline())
	defer cancel()
	var (
		state ManagerState
		err   error
		name  string
	)
	switch spec.Action {
	case OperationRegister:
		name = "register"
		state, err = c.manager.Register(stepCtx)
	case OperationLoginStart:
		if spec.LoginStart {
			name = "enable_login_start"
			state, err = c.manager.Enable(stepCtx)
		} else {
			name = "disable_login_start"
			state, err = c.manager.Disable(stepCtx)
		}
	default:
		err = errors.New("service_action_not_inline")
	}
	operation.Steps = append(operation.Steps, OperationStep{Name: name, OK: err == nil,
		Reason: reasonOf(err), Detail: detailOf(err), StartedAt: operation.StartedAt, FinishedAt: c.now()})
	if err == nil && spec.Action == OperationLoginStart && state.Enabled != spec.LoginStart {
		err = errors.New("login_start_readback_mismatch")
		operation.Steps = append(operation.Steps, OperationStep{Name: "readback_login_start", OK: false,
			Reason: "login_start_readback_mismatch",
			Detail: "The definition did not read back with the requested login-start value."})
	}
	finished := c.finish(operation, TerminationNone, reasonOf(err), err, state)
	if writeErr := writeOperation(dir, finished); writeErr != nil {
		return finished, writeErr
	}
	return finished, err
}

// startHelper writes the request and launches the short-lived executor. The
// helper inherits the change slot: the lock file already names this operation, so
// a second click is refused until the helper finishes or its deadline passes.
func (c *controller) startHelper(dir string, operation ServiceOperation,
	spec OperationSpec, lock *operationLock) (ServiceOperation, error) {
	helperPath := c.options.HelperPath
	if helperPath == "" {
		executable, err := os.Executable()
		if err != nil {
			return ServiceOperation{}, err
		}
		helperPath = executable
	}
	if !filepath.IsAbs(helperPath) {
		return ServiceOperation{}, errors.New("service_helper_not_absolute")
	}
	request := OperationRequest{
		ID: operation.ID, Action: operation.Action, LoginStart: spec.LoginStart, Force: spec.Force,
		Binary: c.options.Binary, StateDir: c.options.StateDir, LogPath: c.options.LogPath,
		Port: c.options.Port, BuildID: c.options.BuildID, Home: c.options.Home, UID: c.options.UID,
		HelperPath:     helperPath,
		DrainTimeoutMS: c.drainTimeout().Milliseconds(),
		DeadlineMS:     c.deadline().Milliseconds(),
		StartDelayMS:   c.startDelay().Milliseconds(),
	}
	if err := validateRequest(request); err != nil {
		return ServiceOperation{}, err
	}
	if err := writePrivateJSON(requestPath(dir, operation.ID), request); err != nil {
		return ServiceOperation{}, err
	}
	operation.Phase = PhaseRunning
	operation.UpdatedAt = c.now()
	if err := writeOperation(dir, operation); err != nil {
		return ServiceOperation{}, err
	}
	spawn := c.options.Spawn
	if spawn == nil {
		spawn = spawnHelper
	}
	pid, err := spawn(HelperStart{Binary: helperPath, Arg: HelperFlag,
		Args: []string{c.options.StateDir, operation.ID}, LogPath: helperLogPath(dir, operation.ID),
		Env: helperEnvironment()})
	if err != nil {
		return ServiceOperation{}, err
	}
	// The lock now belongs to the helper. Dropping this handle must not remove
	// the file, so the controller forgets it without unlinking.
	lock.path = ""
	operation.HelperPID = pid
	operation.UpdatedAt = c.now()
	if err := writeOperation(dir, operation); err != nil {
		return operation, err
	}
	return operation, nil
}

// HelperStart is one detached launch. The seam exists so a test can run the
// helper in-process without starting a second executable.
type HelperStart struct {
	Binary  string
	Arg     string
	Args    []string
	LogPath string
	Env     []string
}

func helperEnvironment() []string {
	return append(os.Environ(), "KIMI_WORK_PLUS_SERVICE_HELPER=1")
}

// finish builds the terminal record. The caller persists it; keeping the write
// out of here lets the inline path write once.
func (c *controller) finish(operation ServiceOperation, termination string,
	reason string, cause error, state ManagerState) ServiceOperation {
	operation.UpdatedAt = c.now()
	operation.Termination = termination
	if operation.Termination == "" {
		operation.Termination = TerminationNone
	}
	if reason == "" && cause != nil {
		reason = errorReasonOf(cause)
	}
	if reason == "" {
		operation.Phase = PhaseSucceeded
	} else {
		operation.Phase = PhaseFailed
		operation.Reason = reason
		if operation.Detail == "" {
			operation.Detail = detailOf(cause)
		}
	}
	if state.Kind != "" {
		copied := state
		operation.State = &copied
	}
	return operation
}

// Read returns one journal entry. A record whose executor died after its
// deadline is reported as failed: a pending change is never shown as success.
func (c *controller) Read(ctx context.Context, id string) (ServiceOperation, error) {
	dir, err := c.operationDir()
	if err != nil {
		return ServiceOperation{}, err
	}
	operation, err := readOperationRecord(dir, id)
	if err != nil {
		return ServiceOperation{}, err
	}
	if operation.Terminal() || c.now().Before(operation.Deadline.Add(staleGrace)) {
		return operation, nil
	}
	// The helper is past every deadline it was given. Report the loss instead of
	// waiting forever; a later read does not resurrect it.
	if current, err := readOperationRecord(dir, id); err == nil && current.Terminal() {
		return current, nil
	}
	operation.Phase = PhaseFailed
	operation.Reason = "service_operation_lost"
	operation.Detail = "The lifecycle helper stopped reporting before it finished."
	operation.UpdatedAt = c.now()
	_ = writeOperation(dir, operation)
	_ = os.Remove(filepath.Join(dir, operationLockName))
	return operation, nil
}

func (c *controller) List(ctx context.Context, limit int) ([]ServiceOperation, error) {
	dir, err := c.operationDir()
	if err != nil {
		return nil, err
	}
	return listOperations(dir, limit), nil
}

func (c *controller) Active(ctx context.Context) (ServiceOperation, bool, error) {
	dir, err := c.operationDir()
	if err != nil {
		return ServiceOperation{}, false, err
	}
	id := lockHolder(dir)
	if id == "" {
		return ServiceOperation{}, false, nil
	}
	operation, err := readOperationRecord(dir, id)
	if errors.Is(err, ErrOperationNotFound) {
		return ServiceOperation{}, false, nil
	}
	if err != nil {
		return ServiceOperation{}, false, err
	}
	return operation, true, nil
}

func (c *controller) now() time.Time {
	if c.options.Now != nil {
		return c.options.Now()
	}
	return time.Now()
}

func errorReasonOf(err error) string {
	if err == nil {
		return ""
	}
	return reasonOf(err)
}

func detailOf(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// HelperFlag is the argv word that makes a host binary act as the short-lived
// lifecycle helper. The host passes the state directory and the operation id.
const HelperFlag = "--kimi-service-helper"

// RunHelperFromArgs is the whole contract a host binary needs: when argv[1] is
// HelperFlag, run the helper and return its exit code. It returns (0, false) for
// every other command line, so the host can call it before installing its own
// flag set.
//
// The helper is deliberately a separate process. A daemon cannot unload itself,
// reload the job that owns it, and then report the result: the code that would
// send the answer is gone. The helper is started detached, is not a child the
// daemon waits on, and exits when its operation ends.
func RunHelperFromArgs(args []string) (int, bool) {
	if len(args) < 4 || args[1] != HelperFlag {
		return 0, false
	}
	// The helper must never write to the terminal of whoever launched the app.
	// Its output goes to the per-operation log the controller created.
	if err := RunHelper(args[2], args[3], HTTPTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, reasonOf(err))
		return 1, true
	}
	return 0, true
}

// RunHelper executes one recorded operation. It is exported so a test can drive
// it in-process, and so a host can wrap it with its own logging.
func RunHelper(stateDir, id string, transport DaemonTransport) error {
	return RunHelperWith(stateDir, id, transport, nil)
}

// RunHelperWith is RunHelper with an explicit host seam. A nil host is the real
// machine; tests pass a recording host so no command or file outside the test's
// temporary directory is touched.
func RunHelperWith(stateDir, id string, transport DaemonTransport, host ManagerHost) error {
	dir, err := ensureOperationDirectory(stateDir)
	if err != nil {
		return err
	}
	request, err := readOperationRequest(dir, id)
	if err != nil {
		return err
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	// A helper must be the binary the caller started. If the file was replaced
	// between write and exec, refusing is the only honest answer.
	if request.HelperPath != "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if !samePath(executable, request.HelperPath) {
			return errors.New("service_helper_identity_mismatch")
		}
	}
	operation, err := readOperationRecord(dir, id)
	if err != nil {
		return err
	}
	if operation.Action != request.Action {
		return errors.New("service_operation_identity_mismatch")
	}
	lock, err := acquireOperationLock(dir, id, time.Now(), operation.Deadline)
	if err != nil && !errors.Is(err, ErrOperationBusy) {
		return err
	}
	// A busy slot here means the controller's own lock is still present, which is
	// the normal hand-off: the controller wrote it before spawning. Ownership is
	// taken over by rewriting it under this pid.
	if errors.Is(err, ErrOperationBusy) {
		lock, err = takeOverOperationLock(dir, id, operation.Deadline, time.Now())
		if err != nil {
			return err
		}
	}
	defer lock.Release()

	if delay := request.startDelay(); delay > 0 {
		// The caller's HTTP response is still being written. Let it reach the
		// panel before the drain stops the daemon that is writing it.
		time.Sleep(delay)
	}
	if host == nil {
		host = localHost{}
	}
	executor := helperExecutor{dir: dir, id: id, request: request,
		transport: transport, manager: request.managerOptions(host)}
	executor.run(operation)
	pruneOperations(dir)
	return nil
}

func samePath(left, right string) bool {
	if left == right {
		return true
	}
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// Path comparison on Windows is case-insensitive.
		return equalFold(leftResolved, rightResolved)
	}
	return leftResolved == rightResolved
}

func equalFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := 0; index < len(left); index++ {
		a, b := left[index], right[index]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// takeOverOperationLock rewrites the slot for this helper. It is only called when
// the slot already names this operation.
func takeOverOperationLock(dir, id string, deadline, now time.Time) (*operationLock, error) {
	path := filepath.Join(dir, operationLockName)
	existing, err := readLockRecord(path)
	if err != nil {
		return nil, err
	}
	if existing.OperationID != id {
		return nil, ErrOperationBusy
	}
	record := operationLockRecord{OperationID: id, PID: os.Getpid(), At: now, Deadline: deadline}
	if err := writePrivateJSON(path, record); err != nil {
		return nil, err
	}
	return &operationLock{path: path, id: id}, nil
}

// helperExecutor performs the operation. Every observable step is appended to the
// journal as it finishes, so a panel can show where a slow or failed change is.
type helperExecutor struct {
	dir       string
	id        string
	request   OperationRequest
	transport DaemonTransport
	manager   ManagerOptions
	// drained is set once the graceful stop is confirmed, so the platform step
	// can tell "force because the drain failed" from "force anyway".
	drained bool
}

func (e *helperExecutor) step(name string, started time.Time, err error, output string) OperationStep {
	step := OperationStep{Name: name, OK: err == nil, Reason: reasonOf(err), Detail: detailOf(err),
		Output: shortOutput(output), StartedAt: started, FinishedAt: time.Now()}
	return step
}

// update persists the in-progress record. A failure to write is fatal for the
// operation: a change nobody can read back is not a completed change.
func (e *helperExecutor) update(operation *ServiceOperation) bool {
	operation.UpdatedAt = time.Now()
	return writeOperation(e.dir, *operation) == nil
}

func (e *helperExecutor) run(operation ServiceOperation) {
	operation.Phase = PhaseRunning
	operation.HelperPID = os.Getpid()
	operation.Termination = TerminationNone
	if !e.update(&operation) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestDuration(e.request.DeadlineMS, 90*time.Second))
	defer cancel()
	closed := false
	defer func() {
		if !closed {
			e.fail(&operation, "service_operation_lost", "The helper exited before the operation finished.", ManagerState{})
		}
	}()

	manager, err := OpenManager(e.manager)
	if err != nil {
		e.fail(&operation, reasonOf(err), detailOf(err), ManagerState{})
		closed = true
		return
	}

	// A destructive action stops the old daemon through its own authenticated
	// endpoint before the platform step. The helper is not that daemon, so it is
	// still alive to write the result.
	if e.request.spec().Drains() {
		started := time.Now()
		result := drain(ctx, e.transport, e.request.StateDir,
			requestDuration(e.request.DrainTimeoutMS, 15*time.Second), time.Now, time.Sleep)
		var stepErr error
		if result.Replaced {
			stepErr = errors.New(result.Reason)
		} else if !result.Drained {
			stepErr = errors.New(result.Reason)
		}
		operation.Steps = append(operation.Steps, e.step("drain", started, stepErr, ""))
		if result.Drained {
			operation.Drained = true
			e.drained = true
			operation.Termination = TerminationDrain
		}
		if !result.Drained && !e.request.Force {
			e.keepState(&operation, manager)
			if stepErr == nil {
				stepErr = errors.New("daemon_drain_unconfirmed")
			}
			e.fail(&operation, reasonOf(stepErr), detailOf(stepErr), stateOf(operation))
			closed = true
			return
		}
	}

	action, err := e.platformAction()
	if err != nil {
		e.fail(&operation, reasonOf(err), detailOf(err), ManagerState{})
		closed = true
		return
	}
	if action == "" {
		// The drain was the whole change: nothing was loaded, killed or
		// re-registered, so there is no platform step to run and no forced
		// termination to report.
		closed = true
		e.succeed(&operation, manager)
		return
	}
	started := time.Now()
	output, actionErr := e.runPlatform(ctx, manager, action)
	if actionErr != nil {
		operation.Steps = append(operation.Steps, e.step(action, started, actionErr, output))
		e.keepState(&operation, manager)
		e.fail(&operation, reasonOf(actionErr), detailOf(actionErr), stateOf(operation))
		closed = true
		return
	}
	if action == actionTerminate || (action == actionUninstall && !operation.Drained) {
		// Two ways to reach a forced termination: an explicit force, or an
		// uninstall whose drain did not confirm. Both must say so, or the record
		// would read as a clean shutdown that never happened.
		operation.Forced = true
		if runtime.GOOS == "darwin" {
			operation.Termination = TerminationLaunchd
		} else {
			operation.Termination = TerminationSchtasks
		}
	}
	operation.Steps = append(operation.Steps, e.step(action, started, nil, output))
	closed = true
	e.succeed(&operation, manager)
}

// platformAction chooses the platform step for the requested action.
func (e *helperExecutor) platformAction() (string, error) {
	switch e.request.Action {
	case OperationStartService:
		return actionStart, nil
	case OperationRestart:
		return actionRestart, nil
	case OperationStopService:
		if e.request.Force && !e.drained {
			return actionTerminate, nil
		}
		// A stop that drained successfully needs no platform step: the daemon is
		// gone, and the job stays loaded with RunAtLoad false.
		return "", nil
	case OperationUninstall:
		return actionUninstall, nil
	case OperationRegister:
		return actionRegister, nil
	case OperationLoginStart:
		if e.request.LoginStart {
			return actionEnable, nil
		}
		return actionDisable, nil
	}
	return "", errors.New("invalid_service_action")
}

func (e *helperExecutor) runPlatform(ctx context.Context, manager Manager, action string) (string, error) {
	service, ok := manager.(*serviceManager)
	if !ok {
		// A substituted Manager in a test still runs its own method.
		switch action {
		case actionStart:
			_, err := manager.Start(ctx)
			return "", err
		case actionRestart:
			_, err := manager.Start(ctx)
			return "", err
		case actionTerminate, actionUninstall:
			_, err := manager.Uninstall(ctx)
			return "", err
		}
		return "", errors.New("service_action_not_supported")
	}
	outputs, err := service.run(ctx, action)
	return joinOutputs(outputs), err
}

func joinOutputs(outputs []string) string {
	combined := ""
	for _, output := range outputs {
		if combined != "" {
			combined += "\n"
		}
		combined += output
	}
	return combined
}

func (e *helperExecutor) keepState(operation *ServiceOperation, manager Manager) {
	ctx, cancel := contextWithTimeout(5 * time.Second)
	defer cancel()
	state, err := manager.Status(ctx)
	if err != nil {
		return
	}
	operation.State = &state
	operation.ServingBuildID = state.CurrentBuildID
}

func (e *helperExecutor) succeed(operation *ServiceOperation, manager Manager) {
	state := e.readBack(operation, manager)
	if state.Kind != "" {
		operation.State = &state
		operation.ServingBuildID = state.CurrentBuildID
	}
	if reason, detail := verifyOutcome(e.request.spec(), state); reason != "" {
		operation.Phase = PhaseFailed
		operation.Reason = reason
		operation.Detail = detail
		e.update(operation)
		return
	}
	operation.Phase = PhaseSucceeded
	operation.Reason = ""
	operation.Detail = ""
	e.update(operation)
}

// readBack waits briefly for the platform to settle, then reads the state. A
// change is not complete until the platform agrees, so the read-back is not
// optional.
func (e *helperExecutor) readBack(operation *ServiceOperation, manager Manager) ManagerState {
	timeout := requestDuration(e.request.StepTimeoutMS, 20*time.Second)
	deadline := time.Now().Add(timeout)
	var state ManagerState
	for {
		ctx, cancel := contextWithTimeout(minDuration(5*time.Second, timeout))
		read, err := manager.Status(ctx)
		cancel()
		if err == nil {
			state = read
			if !needsSettlement(e.request.spec(), read) {
				return read
			}
		}
		if !time.Now().Before(deadline) {
			return state
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

// needsSettlement reports whether a read-back is still consistent with the
// operation being in progress. It only waits for states the platform can
// legitimately reach; it never waits for a state the action does not cause.
func needsSettlement(spec OperationSpec, state ManagerState) bool {
	switch spec.Action {
	case OperationStartService, OperationRestart:
		return !state.Running
	}
	return false
}

// verifyOutcome checks the read-back against what the action promised. It is
// what stops a "succeeded" that only means "the command exited zero".
func verifyOutcome(spec OperationSpec, state ManagerState) (reason, detail string) {
	if state.Kind == "" {
		return "service_status_unavailable", "The service state could not be read after the change."
	}
	if spec.Action == OperationLoginStart {
		if state.Enabled != spec.LoginStart {
			if spec.LoginStart {
				return "login_start_not_enabled", "The definition was written but does not start at login."
			}
			return "login_start_not_disabled", "The definition still starts at login."
		}
		if !state.Registered {
			return "service_not_registered", "No definition exists for this service."
		}
		if spec.LoginStart && !state.Running {
			// Enabling login start does not start anything now, so a stopped
			// daemon is a correct result, not a failure.
		}
		return "", ""
	}
	if spec.Action == OperationUninstall {
		if state.Registered {
			return "service_still_registered", "The definition is still present after uninstall."
		}
		return "", ""
	}
	if spec.Action == OperationStopService {
		if state.Running {
			return "service_still_running", "The daemon is still running after stop."
		}
		return "", ""
	}
	if !state.Registered {
		return "service_not_registered", "No definition exists for this service."
	}
	if state.Stale {
		return "service_registered_for_another_build", "The definition belongs to a different build."
	}
	return "", ""
}

func (e *helperExecutor) fail(operation *ServiceOperation, reason, detail string, state ManagerState) {
	if reason == "" {
		reason = "service_operation_failed"
	}
	operation.Phase = PhaseFailed
	operation.Reason = reason
	operation.Detail = detail
	if state.Kind != "" {
		operation.State = &state
		operation.ServingBuildID = state.CurrentBuildID
	}
	e.update(operation)
}

func stateOf(operation ServiceOperation) ManagerState {
	if operation.State == nil {
		return ManagerState{}
	}
	return *operation.State
}

// contextWithTimeout is context.WithTimeout with the cancel function discarded
// on purpose: these calls are short, and the timer is bounded.
func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

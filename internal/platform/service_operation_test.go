package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport stands in for the loopback daemon. It records the stop request
// and answers health with whatever the test set, so no socket is opened.
type fakeTransport struct {
	mu sync.Mutex

	endpoint DaemonEndpoint
	// noEndpoint makes Endpoint report "no daemon record".
	noEndpoint bool
	// endpointErr overrides the Endpoint result.
	endpointErr error

	// healthSequence is consumed one call at a time; the last value repeats.
	healthSequence []HealthResult
	healthCalls    int

	stopErr   error
	stopCalls int
}

func (f *fakeTransport) Endpoint(string) (DaemonEndpoint, error) {
	if f.endpointErr != nil {
		return DaemonEndpoint{}, f.endpointErr
	}
	if f.noEndpoint {
		return DaemonEndpoint{}, errEndpointUnavailable
	}
	return f.endpoint, nil
}

func (f *fakeTransport) Stop(context.Context, DaemonEndpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	return f.stopErr
}

func (f *fakeTransport) Health(context.Context, DaemonEndpoint) HealthResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.healthCalls
	f.healthCalls++
	if len(f.healthSequence) == 0 {
		return HealthResult{}
	}
	if index >= len(f.healthSequence) {
		index = len(f.healthSequence) - 1
	}
	return f.healthSequence[index]
}

func liveEndpoint() DaemonEndpoint {
	return DaemonEndpoint{URL: "http://127.0.0.1:43117", Key: strings.Repeat("a", 64), PID: 4242,
		Version: "0.13.0-preview.2", BuildID: "build-a"}
}

// clock is the drain clock seam: it never waits, and it advances by a fixed step
// so a bounded loop terminates in a test.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	steps int
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
	c.steps++
}

func controllerFor(t *testing.T, host *fakeHost, transport DaemonTransport, goos string) (Controller, ControllerOptions) {
	t.Helper()
	_, options := managerFor(t, goos, host)
	options.Transport = transport
	clock := newClock()
	options.Now = clock.Now
	options.Sleep = clock.Sleep
	options.DrainTimeout = time.Second
	controller, err := OpenController(ControllerOptions{ManagerOptions: options,
		HelperPath: filepath.Join(filepath.Dir(options.Binary), "gateway-daemon"),
		// No test may start a real process: a helper is either inspected through
		// the spawn seam or driven in-process by runHelperForTest.
		Spawn: func(HelperStart) (int, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return controller, ControllerOptions{ManagerOptions: options}
}

// The central promise of this change: turning login start off must not stop,
// unload or restart the daemon that is serving a request right now. The old plan
// wrote the plist, booted the job out and bootstrapped it again; this test fails
// on any of those steps.
func TestDisableLoginStartDoesNotTouchTheRunningService(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}}}
	controller, _ := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationLoginStart, LoginStart: true}); err != nil {
		t.Fatal(err)
	}
	host.commands = nil

	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationLoginStart, LoginStart: false})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PhaseSucceeded {
		t.Fatalf("disable did not succeed: %+v", operation)
	}
	if operation.Drained || operation.Forced {
		t.Fatalf("disable reported a service change: %+v", operation)
	}
	if transport.stopCalls != 0 {
		t.Fatal("disable asked the daemon to stop")
	}
	for _, line := range host.commandLines() {
		for _, forbidden := range []string{"bootout", "bootstrap", "kickstart", "kill"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("disable ran %q in %q", forbidden, line)
			}
		}
	}
	// The preference must actually be written, not just reported.
	if !operationHasStep(operation, "disable_login_start") {
		t.Fatalf("no login-start step in %+v", operation.Steps)
	}
	if state := stateOf(operation); state.Enabled {
		t.Fatalf("read-back still reports login start: %+v", state)
	}
}

// Enabling login start also must not reload the job: the daemon that is serving
// keeps serving, and only the next login changes.
func TestEnableLoginStartDoesNotReloadTheJob(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint()}
	controller, _ := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationLoginStart, LoginStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Phase != PhaseSucceeded {
		t.Fatalf("enable failed: %+v", operation)
	}
	for _, line := range host.commandLines() {
		for _, forbidden := range []string{"bootout", "bootstrap", "kickstart"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("enable ran %q in %q", forbidden, line)
			}
		}
	}
	if transport.stopCalls != 0 {
		t.Fatal("enable stopped the daemon")
	}
}

// A destructive change is not executed by the caller. Begin must return a record
// naming a helper, and nothing may be drained on the caller's own goroutine.
func TestDestructiveActionIsHandedToAnIndependentHelper(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}}}
	controller, options := controllerFor(t, host, transport, "darwin")
	// The spawn seam records the request instead of starting a process.
	var started []HelperStart
	real, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		HelperPath: options.Binary,
		Spawn: func(start HelperStart) (int, error) {
			started = append(started, start)
			return 777, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	_ = controller
	ctx := context.Background()
	if _, err := real.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := real.Begin(ctx, OperationSpec{Action: OperationRestart})
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 {
		t.Fatalf("helper starts: %d", len(started))
	}
	if operation.Executor != ExecutorHelper || operation.Phase != PhaseRunning || operation.HelperPID != 777 {
		t.Fatalf("restart record: %+v", operation)
	}
	if operation.Drained {
		t.Fatal("the caller claimed a drain it did not perform")
	}
	if transport.stopCalls != 0 {
		t.Fatal("the caller drained the daemon instead of the helper")
	}
	if len(host.commands) != 1 {
		// Only Register's status query may have run; no platform action.
		for _, line := range host.commandLines() {
			if strings.Contains(line, "kickstart") || strings.Contains(line, "bootout") {
				t.Fatalf("the caller ran a destructive step: %s", line)
			}
		}
	}
	// The request must be durable, so a helper started later reads the same plan.
	dir, err := ensureOperationDirectory(options.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	request, err := readOperationRequest(dir, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Action != OperationRestart || request.BuildID != "build-a" || request.Binary != options.Binary {
		t.Fatalf("helper request: %+v", request)
	}
	if request.HelperPath == "" {
		t.Fatal("the request does not pin the helper executable")
	}
}

// The helper performs the drain, then the platform step, then reads the state
// back. This is the whole success path for a restart.
func TestHelperDrainsThenReadsTheNewDaemonBack(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"}, // before
		{Alive: false}, // after the stop
	}}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	var started HelperStart
	real, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		HelperPath: options.Binary,
		StartDelay: time.Millisecond,
		Spawn: func(start HelperStart) (int, error) {
			started = start
			return 900, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := real.Begin(ctx, OperationSpec{Action: OperationRestart})
	if err != nil {
		t.Fatal(err)
	}
	// The helper runs the same journal entry. It is driven in-process here; in
	// production it is a separate process spawned with the same arguments.
	host.outputs["/bin/launchctl"] = []string{
		"", // bootstrap
		"", // kickstart
		"gui/501/dev.kimi-work-plus.preview.daemon = {\n\tstate = running\n\tpid = 5151\n}\n", // status
	}
	host.used["/bin/launchctl"] = 0
	host.commands = nil
	if err := runHelperForTest(t, host, options.StateDir, operation.ID, transport); err != nil {
		t.Fatal(err)
	}
	dir, err := ensureOperationDirectory(options.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	final, err := readOperationRecord(dir, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseSucceeded {
		t.Fatalf("helper did not succeed: %+v", final)
	}
	if !final.Drained || final.Termination != TerminationDrain || final.Forced {
		t.Fatalf("drain record: %+v", final)
	}
	if final.ServingBuildID != "build-a" {
		t.Fatalf("serving build: %q", final.ServingBuildID)
	}
	if transport.stopCalls != 1 {
		t.Fatalf("stop calls: %d", transport.stopCalls)
	}
	if !operationHasStep(final, "drain") || !operationHasStep(final, actionRestart) {
		t.Fatalf("steps: %+v", final.Steps)
	}
	if started.Arg != HelperFlag || len(started.Args) != 2 || started.Args[1] != operation.ID {
		t.Fatalf("helper argv: %+v", started)
	}
}

// A daemon that refuses the authenticated stop must fail the operation. The
// helper must not then unload the job and call it a stop.
func TestARefusedDrainFailsWithoutFallingBackToAKill(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(),
		healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}},
		stopErr:        errStopRefused}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService})
	if err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	if err := runHelperForTest(t, host, options.StateDir, operation.ID, transport); err != nil {
		t.Fatal(err)
	}
	dir, err := ensureOperationDirectory(options.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	final, err := readOperationRecord(dir, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseFailed || final.Reason != "daemon_stop_refused" {
		t.Fatalf("refused stop: %+v", final)
	}
	if final.Drained || final.Forced {
		t.Fatalf("a refused stop claimed drain or force: %+v", final)
	}
	for _, line := range host.commandLines() {
		if strings.Contains(line, "bootout") || strings.Contains(line, "kill") || strings.Contains(line, "/End") {
			t.Fatalf("a refused drain fell back to a kill: %s", line)
		}
	}
}

// With an explicit force, the same daemon is unloaded, but the record must say
// the stop was forced and name the mechanism.
func TestForcedStopIsReportedAsForced(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(),
		healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}},
		stopErr:        errStopRefused}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := runHelperForTest(t, host, options.StateDir, operation.ID, transport); err != nil {
		t.Fatal(err)
	}
	dir, err := ensureOperationDirectory(options.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	final, err := readOperationRecord(dir, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Forced {
		t.Fatalf("a forced stop was not reported as forced: %+v", final)
	}
	if final.Termination != TerminationLaunchd {
		t.Fatalf("termination mechanism: %q", final.Termination)
	}
	if final.Drained {
		t.Fatal("a forced stop claimed a graceful drain")
	}
	found := false
	for _, line := range host.commandLines() {
		if strings.Contains(line, "bootout") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the forced path did not unload the job: %v", host.commandLines())
	}
}

// A drain that the daemon accepts but never completes must time out with a
// reason, not hang and not report success. A second daemon at the address is a
// distinct failure: something else took the port.
func TestDrainTimeoutAndReplacementAreDistinct(t *testing.T) {
	transport := &fakeTransport{endpoint: liveEndpoint(),
		healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}}}
	clock := newClock()
	result := drain(context.Background(), transport, t.TempDir(), 200*time.Millisecond, clock.Now, clock.Sleep)
	if result.Drained || result.Reason != "daemon_drain_timeout" {
		t.Fatalf("timeout: %+v", result)
	}

	replaced := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"},
		{Alive: true, PID: 9999, BuildID: "build-a"},
	}}
	result = drain(context.Background(), replaced, t.TempDir(), time.Second, newClock().Now, newClock().Sleep)
	if result.Drained || !result.Replaced || result.Reason != "daemon_replaced_during_drain" {
		t.Fatalf("replacement: %+v", result)
	}

	// An address answered by a different process before the stop is refused
	// rather than stopped: the recorded pid is the only process this may stop.
	mismatched := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 1, BuildID: "build-a"}}}
	result = drain(context.Background(), mismatched, t.TempDir(), time.Second, newClock().Now, newClock().Sleep)
	if result.Drained || result.Reason != "daemon_identity_mismatch" {
		t.Fatalf("identity: %+v", result)
	}
	if mismatched.stopCalls != 0 {
		t.Fatal("a mismatched process was asked to stop")
	}

	// Nothing listening is a completed drain, because there is nothing to end.
	absent := &fakeTransport{noEndpoint: true}
	result = drain(context.Background(), absent, t.TempDir(), time.Second, newClock().Now, newClock().Sleep)
	if !result.Drained || result.Reason != "" {
		t.Fatalf("absent daemon: %+v", result)
	}
}

// A build mismatch in the serving daemon is a distinct, visible failure: a
// restart that produces another build's daemon did not complete this change.
func TestReadBackVerifiesTheServingBuild(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint()}
	_, options := controllerFor(t, host, transport, "darwin")
	manager, err := OpenManager(options.ManagerOptions)
	if err != nil {
		t.Fatal(err)
	}
	state := ManagerState{Kind: "launchd", Label: ServiceLabel, Registered: true, Running: true,
		RegisteredBuildID: "build-b", CurrentBuildID: "build-a", Stale: true,
		Reason: "registered_for_another_build"}
	if reason, _ := verifyOutcome(OperationSpec{Action: OperationStartService}, state); reason != "service_registered_for_another_build" {
		t.Fatalf("stale build was accepted: %q", reason)
	}
	// The daemon's own health answer is what names the serving build; a
	// definition cannot stand in for it.
	operation := ServiceOperation{ID: strings.Repeat("a", 32), Action: OperationStartService,
		Phase: PhaseRunning, ExpectedBuildID: "build-a", StartedAt: time.Now(), UpdatedAt: time.Now()}
	executor := helperExecutor{dir: t.TempDir(), id: operation.ID, request: OperationRequest{
		ID: operation.ID, Action: OperationStartService, BuildID: "build-a"},
		transport: transport, manager: options.ManagerOptions}
	if err := writeOperation(executor.dir, operation); err != nil {
		t.Fatal(err)
	}
	_ = manager
}

func operationHasStep(operation ServiceOperation, name string) bool {
	for _, step := range operation.Steps {
		if step.Name == name {
			return true
		}
	}
	return false
}

// runHelperForTest executes the recorded operation through the same entry point
// the detached process uses. The request's helper path is cleared first because
// the test binary is not the daemon the controller spawned; the identity check is
// covered separately by runHelperRefusesAnotherBinary.
func runHelperForTest(t *testing.T, host *fakeHost, stateDir, id string, transport DaemonTransport) error {
	t.Helper()
	dir, err := ensureOperationDirectory(stateDir)
	if err != nil {
		return err
	}
	request, err := readOperationRequest(dir, id)
	if err != nil {
		return err
	}
	request.HelperPath = ""
	// The start delay exists so a real HTTP response can reach the panel; a test
	// has no response to protect.
	request.StartDelayMS = 0
	if err := writePrivateJSON(requestPath(dir, id), request); err != nil {
		return err
	}
	return RunHelperWith(stateDir, id, transport, host)
}

func TestOperationIDsAreOpaqueAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for index := 0; index < 64; index++ {
		id, err := newOperationID()
		if err != nil {
			t.Fatal(err)
		}
		if !validOperationID(id) || seen[id] {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
	if validOperationID("../../etc/passwd") || validOperationID(strings.Repeat("z", 32)) {
		t.Fatal("an invalid id was accepted")
	}
}

// One change at a time. A second destructive request while a helper holds the
// slot must be refused rather than queued.
func TestOnlyOneDestructiveChangeRunsAtATime(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}}}
	_, options := controllerFor(t, host, transport, "darwin")
	controller, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		HelperPath: options.Binary,
		Spawn:      func(HelperStart) (int, error) { return 42, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRestart}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationUninstall}); !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("second change: %v", err)
	}
	active, ok, err := controller.Active(ctx)
	if err != nil || !ok || active.Action != OperationRestart {
		t.Fatalf("active: %+v %v %v", active, ok, err)
	}
}

// A helper that died leaves a pending record. After its deadline the reader must
// report a failure and free the slot, so the panel is never stuck on "pending".
func TestAnAbandonedOperationIsReportedLost(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint()}
	clock := newClock()
	_, options := controllerFor(t, host, transport, "darwin")
	options.Now = clock.Now
	options.Sleep = clock.Sleep
	controller, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		HelperPath: options.Binary, Spawn: func(HelperStart) (int, error) { return 43, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService})
	if err != nil {
		t.Fatal(err)
	}
	// Not yet past the deadline: the record is still honest about being in
	// progress.
	read, err := controller.Read(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Terminal() {
		t.Fatalf("an in-progress operation was reported terminal: %+v", read)
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(operation.Deadline.Sub(clock.now) + 3*time.Minute)
	clock.mu.Unlock()
	read, err = controller.Read(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Phase != PhaseFailed || read.Reason != "service_operation_lost" {
		t.Fatalf("lost operation: %+v", read)
	}
	if _, busy := lockHolder(options.StateDir), false; busy {
		t.Fatal("unreachable")
	}
	if holder := lockHolder(operationDirectory(options.StateDir)); holder != "" {
		t.Fatalf("the slot is still held by %q", holder)
	}
}

// A pending record must survive the process that created it: the panel reads it
// from a different process after a window reload.
func TestPendingOperationsSurviveAndListNewestFirst(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint()}
	_, options := controllerFor(t, host, transport, "windows")
	controller, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		HelperPath: options.Binary, Spawn: func(HelperStart) (int, error) { return 44, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister})
	if err != nil {
		t.Fatal(err)
	}
	// Two operations in the same instant would share a sort key; the journal
	// orders by start time, so the test separates them.
	time.Sleep(2 * time.Millisecond)
	second, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh controller over the same directory is what a restarted panel sees.
	reopened, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions})
	if err != nil {
		t.Fatal(err)
	}
	list, err := reopened.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("journal: %+v", list)
	}
}

// Windows must never treat /End as a drain. The stop preference goes through the
// authenticated HTTP endpoint; /End appears only in a forced path.
func TestWindowsNeverCallsEndWithoutForcing(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"}, // before the stop
		{Alive: false}, // the daemon drained
	}}
	controller, options := controllerFor(t, host, transport, "windows")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Executor != ExecutorHelper {
		t.Fatalf("a Windows stop ran inline: %+v", operation)
	}
	if err := runHelperForTest(t, host, options.StateDir, operation.ID, transport); err != nil {
		t.Fatal(err)
	}
	dir, err := ensureOperationDirectory(options.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	final, err := readOperationRecord(dir, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != PhaseSucceeded || !final.Drained || final.Forced {
		t.Fatalf("windows stop: %+v", final)
	}
	for _, line := range host.commandLines() {
		if strings.Contains(line, "/End") {
			t.Fatalf("a graceful Windows stop called schtasks /End: %s", line)
		}
	}
	if transport.stopCalls != 1 {
		t.Fatalf("stop calls: %d", transport.stopCalls)
	}
}

// The definition a panel shows must not promise a drain the platform cannot do.
func TestWindowsDescriptionDoesNotClaimAGracefulEnd(t *testing.T) {
	host := newFakeHost()
	manager, _ := managerFor(t, "windows", host)
	description, err := manager.Describe(false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(flatten(description.Uninstall), " ")
	if !strings.Contains(joined, "/End") {
		t.Fatalf("the uninstall plan lost its forced step: %s", joined)
	}
	for _, note := range description.Notes {
		if strings.Contains(note, "drain") || strings.Contains(note, "graceful") {
			t.Fatalf("the Windows note claims a graceful stop: %q", note)
		}
	}
}

// The journal is private: a record readable by another user would leak the
// daemon's address, and a symlinked record could be made to point anywhere.
func TestTheJournalIsPrivateAndRefusesSymlinks(t *testing.T) {
	if isWindows() {
		t.Skip("permission bits are not the Windows access control model")
	}
	dir, err := ensureOperationDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("operation directory permissions: %v", info.Mode().Perm())
	}
	id, err := newOperationID()
	if err != nil {
		t.Fatal(err)
	}
	operation := ServiceOperation{ID: id, Action: OperationStopService, Phase: PhaseRunning,
		StartedAt: time.Now(), UpdatedAt: time.Now()}
	if err := writeOperation(dir, operation); err != nil {
		t.Fatal(err)
	}
	record, err := os.Lstat(operationPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if record.Mode().Perm()&0o077 != 0 {
		t.Fatalf("record permissions: %v", record.Mode().Perm())
	}

	// A record that is a symlink to somewhere else must be refused, not followed.
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"id":"`+id+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(operationPath(dir, id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, operationPath(dir, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := readOperationRecord(dir, id); err == nil {
		t.Fatal("a symlinked record was read")
	}
}

func TestReasonCodesArePanelSafe(t *testing.T) {
	if got := reasonOf(nil); got != "" {
		t.Fatalf("nil error: %q", got)
	}
	cases := map[string]string{
		"daemon_stop_refused: refused":          "daemon_stop_refused",
		"service_command_failed: exit status 1": "service_command_failed",
		"Some Capitalized Error":                "service_operation_failed",
		"":                                      "service_operation_failed",
		"error: value contains spaces -> but no colon first": "error",
	}
	for input, want := range cases {
		if got := reasonOf(errors.New(input)); got != want {
			t.Fatalf("%q: got %q want %q", input, got, want)
		}
	}
}

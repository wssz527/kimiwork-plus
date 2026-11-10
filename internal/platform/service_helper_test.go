package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The host binary dispatches the helper before it parses its own flags. A helper
// that is not recognised would be started as a daemon, so the argv contract is
// checked here rather than only in an integration run.
func TestHelperArgvContract(t *testing.T) {
	if _, handled := RunHelperFromArgs([]string{"gateway-daemon"}); handled {
		t.Fatal("a plain launch was treated as a helper")
	}
	if _, handled := RunHelperFromArgs([]string{"gateway-daemon", "--state-dir", "/tmp/x"}); handled {
		t.Fatal("a daemon flag was treated as a helper")
	}
	if _, handled := RunHelperFromArgs([]string{"gateway-daemon", HelperFlag, "/tmp/x"}); handled {
		t.Fatal("a helper launch without an operation id was handled")
	}
	// A truncated helper launch must be handled and must fail, not fall through
	// to the daemon path where it would bind a port.
	if _, handled := RunHelperFromArgs([]string{"gateway-daemon", HelperFlag, "", ""}); !handled {
		t.Fatal("a malformed helper launch fell through to the daemon")
	}
}

// A helper must refuse a request written for a different executable. Without this
// check a replaced binary could be asked to unload a job on behalf of an
// operation it never planned.
func TestHelperRefusesAnotherBinary(t *testing.T) {
	stateDir := t.TempDir()
	dir, err := ensureOperationDirectory(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newOperationID()
	if err != nil {
		t.Fatal(err)
	}
	request := OperationRequest{ID: id, Action: OperationStopService,
		Binary:   filepath.Join(stateDir, "gateway-daemon"),
		StateDir: stateDir, BuildID: "build-a",
		HelperPath: filepath.Join(stateDir, "some-other-binary")}
	if err := writePrivateJSON(requestPath(dir, id), request); err != nil {
		t.Fatal(err)
	}
	if err := writeOperation(dir, ServiceOperation{ID: id, Action: OperationStopService,
		Phase: PhaseRunning, StartedAt: time.Now(), UpdatedAt: time.Now(),
		Deadline: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	err = RunHelperWith(stateDir, id, &fakeTransport{}, newFakeHost())
	if err == nil || err.Error() != "service_helper_identity_mismatch" {
		t.Fatalf("other binary: %v", err)
	}
}

// The private endpoint record is the only address the operation may talk to. A
// non-loopback host, an embedded user, a query string or a short token must all be
// refused before any request is sent.
func TestEndpointRecordIsValidated(t *testing.T) {
	transport := HTTPTransport{}
	valid := DaemonEndpoint{URL: "http://127.0.0.1:43117", Key: strings.Repeat("k", 64),
		PID: 100, Version: "0.13.0-preview.2", BuildID: "build-a"}
	cases := map[string]DaemonEndpoint{
		"remote host":   {URL: "http://10.0.0.5:43117"},
		"hostname":      {URL: "http://localhost:43117"},
		"embedded user": {URL: "http://u:p@127.0.0.1:43117"},
		"query":         {URL: "http://127.0.0.1:43117/?x=1"},
		"path":          {URL: "http://127.0.0.1:43117/api"},
		"no port":       {URL: "http://127.0.0.1"},
		"https":         {URL: "https://127.0.0.1:43117"},
	}
	for name, endpoint := range cases {
		endpoint.Key, endpoint.PID, endpoint.BuildID = valid.Key, valid.PID, valid.BuildID
		dir := t.TempDir()
		encoded, _ := json.Marshal(endpoint)
		if err := os.WriteFile(filepath.Join(dir, "desktop-endpoint.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := transport.Endpoint(dir); !errors.Is(err, errEndpointInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	shortKey := valid
	shortKey.Key = "short"
	dir := t.TempDir()
	encoded, _ := json.Marshal(shortKey)
	if err := os.WriteFile(filepath.Join(dir, "desktop-endpoint.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Endpoint(dir); !errors.Is(err, errEndpointInvalid) {
		t.Fatalf("short token: %v", err)
	}

	// A missing record is "nothing to stop", not a malformed record.
	if _, err := transport.Endpoint(t.TempDir()); !errors.Is(err, errEndpointUnavailable) {
		t.Fatalf("missing record: %v", err)
	}

	// A record another user can read must not be trusted with the token.
	if !isWindows() {
		loose := t.TempDir()
		path := filepath.Join(loose, "desktop-endpoint.json")
		encoded, _ := json.Marshal(valid)
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := transport.Endpoint(loose); err == nil {
			t.Fatal("a world-readable endpoint record was accepted")
		}
	}
}

// The stop request is only accepted when the daemon answers 202. Any other status
// is a refusal, including a redirect, so a proxy or a different service cannot
// stand in for a drain.
func TestStopRequiresTheAcceptedStatus(t *testing.T) {
	for _, status := range []int{200, 204, 401, 409, 503} {
		endpoint := liveEndpoint()
		server := newFakeHTTPServer(t, status)
		endpoint.URL = server
		if err := (HTTPTransport{}).Stop(context.Background(), endpoint); !errors.Is(err, errStopRefused) {
			t.Fatalf("%d: %v", status, err)
		}
	}
	endpoint := liveEndpoint()
	server := newFakeHTTPServer(t, 202)
	endpoint.URL = server
	if err := (HTTPTransport{}).Stop(context.Background(), endpoint); err != nil {
		t.Fatalf("202: %v", err)
	}
}

// Once the operation is done the slot must be free, or the next change would be
// refused until the deadline passed.
func TestTheChangeSlotIsFreedAfterAHelperFinishes(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"},
		{Alive: false},
	}}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService})
	if err != nil {
		t.Fatal(err)
	}
	if holder := lockHolder(operationDirectory(options.StateDir)); holder != operation.ID {
		t.Fatalf("the helper does not own the slot: %q", holder)
	}
	if err := runHelperForTest(t, host, options.StateDir, operation.ID, transport); err != nil {
		t.Fatal(err)
	}
	if holder := lockHolder(operationDirectory(options.StateDir)); holder != "" {
		t.Fatalf("the slot was left held by %q", holder)
	}
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationStartService}); err != nil {
		t.Fatalf("the next change was refused: %v", err)
	}
}

// A lost helper must not leave the slot held forever: a later change has to be
// able to recover without a restart.
func TestALostHelperIsRecoveredAfterItsDeadline(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint()}
	controller, options := controllerFor(t, host, transport, "darwin")
	clock := newClock()
	options.Now = clock.Now
	options.Sleep = clock.Sleep
	reopened, err := OpenController(ControllerOptions{ManagerOptions: options.ManagerOptions,
		Spawn: func(HelperStart) (int, error) { return 11, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	operation, err := reopened.Begin(ctx, OperationSpec{Action: OperationStopService})
	if err != nil {
		t.Fatal(err)
	}
	// The "helper" never runs. Before the deadline a second change is refused.
	if _, err := reopened.Begin(ctx, OperationSpec{Action: OperationStopService}); !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("second change before deadline: %v", err)
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(operation.Deadline.Sub(clock.now) + 3*time.Minute)
	clock.mu.Unlock()
	if _, err := reopened.Read(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	// Read reports the loss and frees the slot, so the user can try again.
	if _, err := reopened.Begin(ctx, OperationSpec{Action: OperationStopService}); err != nil {
		t.Fatalf("a change after a lost helper was refused: %v", err)
	}
	_ = controller
}

// Uninstall must end with no definition, no record and nothing claiming the
// service exists, and the preview identity must never touch the official app.
func TestUninstallLeavesNothingBehind(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"},
		{Alive: false},
	}}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationUninstall})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Executor != ExecutorHelper {
		t.Fatalf("uninstall ran inline: %+v", operation)
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
	if final.Phase != PhaseSucceeded {
		t.Fatalf("uninstall: %+v", final)
	}
	if _, still := host.files[definitionPathFor("darwin", options.Home)]; still {
		t.Fatal("the plist survived uninstall")
	}
	if final.State == nil || final.State.Registered {
		t.Fatalf("read-back still registered: %+v", final.State)
	}
	for _, line := range host.commandLines() {
		if strings.Contains(line, "Kimi.app") || strings.Contains(line, "Kimi.exe") ||
			strings.Contains(line, "com.moonshot") {
			t.Fatalf("a lifecycle step touches the official application: %s", line)
		}
	}
}

// newFakeHTTPServer answers every request with one status and returns its base
// URL. It is the transport's other end, so the stop status rule is exercised over
// a real HTTP round trip rather than a stub.
func newFakeHTTPServer(t *testing.T, status int) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A force request is permission, not an instruction: when the drain succeeds the
// operation must not unload the job afterwards. Unloading a stopped job would
// throw away the definition the user still wants.
func TestForceDoesNotUnloadAfterASuccessfulDrain(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(), healthSequence: []HealthResult{
		{Alive: true, PID: 4242, BuildID: "build-a"},
		{Alive: false},
	}}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationStopService, Force: true})
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
	if final.Phase != PhaseSucceeded || !final.Drained || final.Forced {
		t.Fatalf("clean stop with force permission: %+v", final)
	}
	if final.Termination != TerminationDrain {
		t.Fatalf("termination: %q", final.Termination)
	}
	for _, line := range host.commandLines() {
		if strings.Contains(line, "bootout") || strings.Contains(line, "kill") || strings.Contains(line, "/End") {
			t.Fatalf("a successful drain was followed by a platform termination: %s", line)
		}
	}
}

// Uninstall removes the definition either way, but when the daemon never drained
// the record has to say the stop was forced. Reporting a clean shutdown here
// would hide that a request was cut off.
func TestUninstallWithoutADrainIsReportedAsForced(t *testing.T) {
	host := newFakeHost()
	transport := &fakeTransport{endpoint: liveEndpoint(),
		healthSequence: []HealthResult{{Alive: true, PID: 4242, BuildID: "build-a"}},
		stopErr:        errStopRefused}
	controller, options := controllerFor(t, host, transport, "darwin")
	ctx := context.Background()
	if _, err := controller.Begin(ctx, OperationSpec{Action: OperationRegister}); err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Begin(ctx, OperationSpec{Action: OperationUninstall, Force: true})
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
	if final.Phase != PhaseSucceeded {
		t.Fatalf("uninstall: %+v", final)
	}
	if !final.Forced || final.Drained {
		t.Fatalf("an uninstall without a drain did not report force: %+v", final)
	}
	if final.Termination != TerminationLaunchd {
		t.Fatalf("termination: %q", final.Termination)
	}
	if _, still := host.files[definitionPathFor("darwin", options.Home)]; still {
		t.Fatal("the definition survived uninstall")
	}
}

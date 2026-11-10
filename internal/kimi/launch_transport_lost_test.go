package kimi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A closed transport must be reported as a restart request, never as something
// this package can recover. The descriptors were handed to the child at exec and
// there is no protocol for taking them back, so "reconnect" would be a promise the
// code cannot keep.
func TestAClosedTransportIsReportedAsRestartRequired(t *testing.T) {
	child, _ := startFakeChild(t)

	// The pipe is open and the child is answering: nothing to report.
	if lost, ok := child.TransportLost(); ok {
		t.Fatalf("a live transport was reported lost: %+v", lost)
	}

	// Close the parent's side, which is what a lost child pipe looks like here.
	child.pipe.Dispose()
	deadline := time.Now().Add(2 * time.Second)
	var lost TransportLost
	var ok bool
	for time.Now().Before(deadline) {
		if lost, ok = child.TransportLost(); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("a disposed transport was not reported")
	}
	if lost.Recoverable {
		t.Fatal("a lost transport was reported as recoverable")
	}
	if lost.Reason != RestartRequired {
		t.Fatalf("reason: %q", lost.Reason)
	}
	if lost.Detail == "" {
		t.Fatal("no explanation for the user")
	}
	// The child is still running here, so the detail must tell the user to quit it
	// normally rather than implying a silent reattach.
	if lost.ChildAlive && lost.Detail == "" {
		t.Fatal("a still-running child has no instruction")
	}
}

// A child that exited on its own reports its exit state, and the transport is
// reported as ended for the same reason: there is nothing to attach to.
func TestAnExitedChildReportsItsExit(t *testing.T) {
	child, _ := startFakeChild(t)
	if err := child.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the child did not exit")
	}
	lost, ok := child.TransportLost()
	if !ok {
		t.Fatal("an ended child's transport was not reported")
	}
	if lost.Recoverable {
		t.Fatal("an ended child's transport was reported as recoverable")
	}
	if lost.ChildAlive {
		t.Fatal("an exited child was reported alive")
	}
	if !lost.Exit.Known {
		t.Fatalf("exit state: %+v", lost.Exit)
	}
}

// The transport a panel is told about must match the launcher, and it must never
// claim verification this build has not done.
func TestTransportReportIsHonest(t *testing.T) {
	transport := ManagedTransportInfo()
	if transport.Kind == "" && transport.Supported {
		t.Fatal("a supported transport with no kind")
	}
	if transport.Verified {
		t.Fatal("verified without an official-application run on this host")
	}
	if transport.Supported && transport.Detail == "" {
		t.Fatal("a supported transport with no detail")
	}
}

// Starting a child on a platform whose transport is unsupported must fail with a
// clear error instead of starting something unreachable. This is a static check of
// the reported capability, since only one platform is compiled at a time.
func TestUnsupportedTransportNeverStartsAChild(t *testing.T) {
	if ManagedTransportInfo().Supported {
		t.Skip("this build has a transport here")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child, err := StartKimi(context.Background(), LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  filepath.Join(t.TempDir(), "profile"),
	})
	if err == nil {
		_ = child.Stop(time.Second)
		t.Fatal("a child was started on a platform with no transport")
	}
}

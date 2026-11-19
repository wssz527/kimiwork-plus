package kimi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Windows transport is named by a switch whose format Chromium parses as two
// unsigned 32-bit values separated by a comma. This test pins the wire format on
// every platform, because the value is what the child reads and a second,
// unchecked copy elsewhere would drift.
func TestIOPipesArgumentIsTheDocumentedFormat(t *testing.T) {
	cases := []struct {
		read, write uintptr
		want        string
	}{
		{3, 4, "3,4"},
		{0x1a4, 0x1a8, "420,424"},
		{0xffffffff, 1, "4294967295,1"},
	}
	for _, item := range cases {
		if got := ioPipesArgument(item.read, item.write); got != item.want {
			t.Fatalf("ioPipesArgument(%d, %d) = %q, want %q", item.read, item.write, got, item.want)
		}
		read, write, ok := parseIOPipes(item.want)
		if !ok || read != item.read || write != item.write {
			t.Fatalf("round trip of %q = (%d, %d, %v)", item.want, read, write, ok)
		}
	}
}

// A malformed value must be rejected rather than read as handle 0, which would
// point the child at a handle it does not own.
func TestParseIOPipesRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "3", "3,", ",4", "3,4,5", "a,4", "3,b", "-3,4", "3,4294967296"} {
		if _, _, ok := parseIOPipes(value); ok {
			t.Fatalf("%q was accepted as a handle pair", value)
		}
	}
}

// The transport this build reports must match what it can actually start. A
// platform without a transport reports Supported=false, so the panel disables the
// action instead of failing at start time.
func TestManagedTransportMatchesTheLauncher(t *testing.T) {
	transport := ManagedTransportInfo()
	if !transport.Supported {
		if ManagedChildConfigured() {
			t.Fatal("the panel would offer a launch this platform cannot start")
		}
		return
	}
	if !ManagedChildConfigured() {
		t.Fatal("a supported transport is not reported as configured")
	}
	if transport.Kind != "fd-pipe" && transport.Kind != "handle-pipe" {
		t.Fatalf("unknown transport kind %q", transport.Kind)
	}
	// Supported is not the same claim as verified, and this build has never run
	// the official application on any host it ships a transport for.
	if transport.Verified {
		t.Fatal("the transport is reported verified without an official-application run")
	}
	if transport.Detail == "" {
		t.Fatal("a supported transport with no explanation")
	}
}

// A started child must be reachable over the transport this platform prepared,
// and Stop must end it. This is the same assertion for both transports: the
// difference lives in prepareManagedChild, not in the protocol.
func TestChildStartsOnThePreparedTransport(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child, err := StartKimi(context.Background(), LaunchOptions{
		Installation: Installation{Application: executable},
		UserDataDir:  filepath.Join(t.TempDir(), "profile"),
		ExtraArgs:    []string{fakeCDPFlag},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := child.Pipe().Send(ctx, "Target.getTargets", nil, "", 5*time.Second); err != nil {
		_ = child.Stop(3 * time.Second)
		t.Fatalf("no answer over the prepared transport: %v", err)
	}
	if err := child.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// The switch must reach the child only when the platform's transport needs it.
// Passing --remote-debugging-io-pipes on Unix would be harmless but misleading:
// the value would name descriptors the child never received.
func TestChildArgumentsNameOnlyTheTransportInUse(t *testing.T) {
	transport := ManagedTransportInfo()
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pipes := launchPipes{commandReader: commandReader, commandWriter: commandWriter,
		responseReader: responseReader, responseWriter: responseWriter}
	t.Cleanup(pipes.closeParentEnds)

	command := exec.Command("daemon")
	args, err := prepareManagedChild(command, pipes)
	if err != nil {
		if transport.Supported {
			t.Fatalf("supported transport refused to prepare: %v", err)
		}
		return
	}
	joined := strings.Join(args, " ")
	switch transport.Kind {
	case "handle-pipe":
		if !strings.Contains(joined, "--remote-debugging-io-pipes=") {
			t.Fatalf("the handle transport does not name its handles: %v", args)
		}
		// Exactly the two ends the child needs, and nothing else: a wider
		// inheritance would hand the child whatever else this process holds.
		if got := inheritedHandles(command); got != 2 {
			t.Fatalf("%d inherited handles, want 2", got)
		}
		// The official application is the managed child, so starting it hidden
		// would leave the user with a running Kimi and no window.
		if managedChildWindowHidden(command) {
			t.Fatal("the managed child would start with its main window hidden")
		}
		if len(command.ExtraFiles) != 0 {
			t.Fatalf("the handle transport also set ExtraFiles: %v", command.ExtraFiles)
		}
	case "fd-pipe":
		if strings.Contains(joined, "--remote-debugging-io-pipes=") {
			t.Fatalf("the descriptor transport passed a handle switch: %v", args)
		}
		if got := len(command.ExtraFiles); got != 2 {
			t.Fatalf("%d extra files, want 2", got)
		}
		if got := inheritedHandles(command); got != 0 {
			t.Fatalf("the descriptor transport also inherited %d handles", got)
		}
	}
}

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost records every side effect instead of performing it, so these tests
// never register, load, start or stop a real service and never touch the user's
// own LaunchAgents directory or task database.
type fakeHost struct {
	commands []fakeCommand
	files    map[string][]byte
	// failOn makes one command name fail, so a plan's error path is exercised.
	failOn map[string]error
	// outputs are returned per command name, in order of use.
	outputs map[string][]string
	used    map[string]int
}

type fakeCommand struct {
	name string
	args []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{files: map[string][]byte{}, failOn: map[string]error{},
		outputs: map[string][]string{}, used: map[string]int{}}
}

func (f *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	f.commands = append(f.commands, fakeCommand{name: name, args: args})
	if err, ok := f.failOn[name]; ok {
		return "", err
	}
	queued := f.outputs[name]
	index := f.used[name]
	f.used[name] = index + 1
	if index < len(queued) {
		return queued[index], nil
	}
	return "", nil
}

func (f *fakeHost) WriteFile(path string, data []byte, _ os.FileMode) error {
	f.files[path] = append([]byte(nil), data...)
	return nil
}

func (f *fakeHost) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (f *fakeHost) Remove(path string) error {
	delete(f.files, path)
	return nil
}

// commandLines renders every recorded command for a substring check.
func (f *fakeHost) commandLines() []string {
	out := make([]string, 0, len(f.commands))
	for _, command := range f.commands {
		out = append(out, command.name+" "+strings.Join(command.args, " "))
	}
	return out
}

// managerFor builds a Manager on a temporary home with the fake host. The state
// directory is real and temporary, so the provenance record is written and read
// through the same code the daemon uses.
func managerFor(t *testing.T, goos string, host *fakeHost) (Manager, ManagerOptions) {
	t.Helper()
	home := t.TempDir()
	options := ManagerOptions{
		Binary:   filepath.Join(home, "bin", "gateway-daemon"),
		StateDir: filepath.Join(home, "state"),
		LogPath:  filepath.Join(home, "daemon.log"),
		Port:     "43117",
		BuildID:  "build-a",
		Home:     home,
		UID:      "501",
		Host:     host,
		GOOS:     goos,
	}
	manager, err := OpenManager(options)
	if err != nil {
		t.Fatal(err)
	}
	return manager, options
}

// The central product rule: registering a service must not make it start at
// login. If this ever fails, first launch quietly adds a login item.
func TestRegisterDoesNotEnableLoginStart(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			host := newFakeHost()
			manager, options := managerFor(t, goos, host)
			if _, err := manager.Register(context.Background()); err != nil {
				t.Fatal(err)
			}
			lines := strings.Join(host.commandLines(), "\n")
			switch goos {
			case "darwin":
				content := string(host.files[definitionPathFor(goos, options.Home)])
				if !strings.Contains(content, "<key>RunAtLoad</key>\n    <false/>") {
					t.Fatalf("RunAtLoad is not false after Register:\n%s", content)
				}
				if strings.Contains(content, "<true/>\n    <key>KeepAlive</key>") {
					t.Fatalf("the definition starts at login:\n%s", content)
				}
			case "windows":
				// schtasks cannot create a disabled task, so the plan must create
				// it and then disable it in the same action.
				if !strings.Contains(lines, "/Create") {
					t.Fatalf("the task was not created:\n%s", lines)
				}
				if !strings.Contains(lines, "/Change /TN "+ServiceLabel+" /DISABLE") {
					t.Fatalf("the task was left enabled after Register:\n%s", lines)
				}
			}
			// KeepAlive must never be true: a crash loop would hide the failure.
			if goos == "darwin" {
				content := string(host.files[definitionPathFor(goos, options.Home)])
				if !strings.Contains(content, "<key>KeepAlive</key>\n    <false/>") {
					t.Fatalf("KeepAlive is not false:\n%s", content)
				}
			}
		})
	}
}

// Enable is the only action that turns login start on, and Disable turns it back
// off without removing the definition.
func TestOnlyEnableTurnsLoginStartOn(t *testing.T) {
	host := newFakeHost()
	manager, options := managerFor(t, "darwin", host)
	ctx := context.Background()
	if _, err := manager.Register(ctx); err != nil {
		t.Fatal(err)
	}
	path := definitionPathFor("darwin", options.Home)
	if strings.Contains(string(host.files[path]), "<true/>") {
		t.Fatal("Register enabled login start")
	}

	state, err := manager.Enable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(host.files[path]), "<key>RunAtLoad</key>\n    <true/>") {
		t.Fatal("Enable did not set RunAtLoad true")
	}
	if !state.Enabled {
		t.Fatalf("Enable reported enabled=false: %+v", state)
	}

	state, err = manager.Disable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(host.files[path]), "<key>RunAtLoad</key>\n    <true/>") {
		t.Fatal("Disable left RunAtLoad true")
	}
	if state.Enabled {
		t.Fatalf("Disable reported enabled=true: %+v", state)
	}
	// The definition must still exist: disabling is not uninstalling.
	if state.Registered == false {
		t.Fatal("Disable removed the registration")
	}
}

// Uninstall must remove the definition and leave nothing claiming the service
// exists. It must also never mention the official application.
func TestUninstallRemovesOnlyItsOwnDefinition(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			host := newFakeHost()
			manager, options := managerFor(t, goos, host)
			ctx := context.Background()
			if _, err := manager.Register(ctx); err != nil {
				t.Fatal(err)
			}
			state, err := manager.Uninstall(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if state.Registered {
				t.Fatalf("still registered after Uninstall: %+v", state)
			}
			if goos == "darwin" {
				if _, still := host.files[definitionPathFor(goos, options.Home)]; still {
					t.Fatal("the plist survived Uninstall")
				}
			}
			for _, line := range host.commandLines() {
				if strings.Contains(line, "Kimi.app") || strings.Contains(line, "Kimi.exe") {
					t.Fatalf("a service command touches the official application: %s", line)
				}
			}
			// The provenance record goes with the definition, or a later Status
			// would attribute a service that no longer exists.
			if _, still := host.files[filepath.Join(options.StateDir, "service-definition.json")]; still {
				t.Fatal("the provenance record survived Uninstall")
			}
		})
	}
}

// A definition written by a different build must be reported as stale rather than
// silently reused or silently rewritten.
func TestStatusReportsADefinitionFromAnotherBuild(t *testing.T) {
	host := newFakeHost()
	manager, options := managerFor(t, "darwin", host)
	ctx := context.Background()
	if _, err := manager.Register(ctx); err != nil {
		t.Fatal(err)
	}
	// A newer build asks about the same state directory.
	options.BuildID = "build-b"
	newer, err := OpenManager(options)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newer.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Stale || state.Reason != "registered_for_another_build" {
		t.Fatalf("a stale definition was not reported: %+v", state)
	}
	if state.RegisteredBuildID != "build-a" || state.CurrentBuildID != "build-b" {
		t.Fatalf("build identities: %+v", state)
	}
	// Reporting must not have rewritten anything: the user decides when to
	// re-register.
	if !strings.Contains(string(host.files[definitionPathFor("darwin", options.Home)]), "build-a") {
		t.Fatal("Status rewrote the definition")
	}
}

// Status on a machine with no service is a normal answer, not an error, and it
// must not claim a running service.
func TestStatusWithoutAServiceIsNotAnError(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			host := newFakeHost()
			// Both platforms' status queries fail when nothing is registered.
			host.failOn["/bin/launchctl"] = errors.New("exit status 113")
			host.failOn["schtasks"] = errors.New("ERROR: The system cannot find the file specified.")
			manager, _ := managerFor(t, goos, host)
			state, err := manager.Status(context.Background())
			if err != nil {
				t.Fatalf("Status returned an error for a missing service: %v", err)
			}
			if state.Registered || state.Running || state.Enabled {
				t.Fatalf("a missing service reported as present: %+v", state)
			}
			if state.Reason != "not_registered" {
				t.Fatalf("reason: %q", state.Reason)
			}
		})
	}
}

// The state a panel renders must come from the platform's own query, not from the
// provenance record. A record without a definition is not a service.
func TestStatusTrustsThePlatformOverItsOwnRecord(t *testing.T) {
	host := newFakeHost()
	manager, options := managerFor(t, "windows", host)
	ctx := context.Background()
	// A record exists but schtasks knows nothing: the definition was removed by
	// hand.
	host.files[filepath.Join(options.StateDir, "service-definition.json")] =
		[]byte(`{"buildId":"build-a","binary":"x","loginStart":true}`)
	host.failOn["schtasks"] = errors.New("not found")
	state, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Registered {
		t.Fatalf("a leftover record was reported as a registered service: %+v", state)
	}
}

// A disabled task must report enabled=false even though it exists, because that
// is the difference between "installed" and "starts at login".
func TestStatusReadsTheWindowsEnabledFlag(t *testing.T) {
	host := newFakeHost()
	manager, _ := managerFor(t, "windows", host)
	host.outputs["schtasks"] = []string{
		`<?xml version="1.0"?><Task><Settings><Enabled>false</Enabled></Settings></Task>`,
		"TaskName: \\" + ServiceLabel + "\nStatus:        Ready\n",
	}
	state, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Registered {
		t.Fatalf("a task that exists was reported missing: %+v", state)
	}
	if state.Enabled {
		t.Fatalf("a disabled task reported enabled: %+v", state)
	}
	if state.Running {
		t.Fatalf("a Ready task reported running: %+v", state)
	}

	host2 := newFakeHost()
	manager2, _ := managerFor(t, "windows", host2)
	host2.outputs["schtasks"] = []string{
		`<Task><Settings><Enabled>true</Enabled></Settings></Task>`,
		"Status:        Running\n",
	}
	state, err = manager2.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || !state.Running {
		t.Fatalf("an enabled, running task: %+v", state)
	}
}

// launchctl print is not a stable API, so only two unambiguous lines are read and
// an unparsable output must never be read as "running".
func TestLaunchdStatusParsingIsConservative(t *testing.T) {
	running, pid, ok := parseLaunchdPrint("gui/501/dev.kimi-work-plus.preview.daemon = {\n\tstate = running\n\tpid = 4321\n}\n")
	if !ok || !running || pid != 4321 {
		t.Fatalf("running job: %v %d %v", running, pid, ok)
	}
	// A stopped job still prints a state line, with no pid.
	running, pid, ok = parseLaunchdPrint("gui/501/x = {\n\tstate = not running\n}\n")
	if !ok || running || pid != 0 {
		t.Fatalf("stopped job: %v %d %v", running, pid, ok)
	}
	// Garbage means unknown, which the caller renders as not running.
	if _, _, ok := parseLaunchdPrint("Could not find service"); ok {
		t.Fatal("an unparsable output was treated as a loaded job")
	}
}

// A failing command in the middle of a plan must abort it, so a half-registered
// service is never reported as complete.
func TestAFailedStepAbortsThePlan(t *testing.T) {
	host := newFakeHost()
	manager, _ := managerFor(t, "windows", host)
	host.failOn["schtasks"] = errors.New("access denied")
	if _, err := manager.Register(context.Background()); err == nil {
		t.Fatal("a failed schtasks call reported success")
	}
}

// Registering is a file operation. It must not bootstrap the job: a launchd call
// here would make the daemon start before the user asked, which is exactly the
// "installed means running" mistake the product avoids.
func TestRegisterDoesNotCallLaunchd(t *testing.T) {
	host := newFakeHost()
	manager, _ := managerFor(t, "darwin", host)
	if _, err := manager.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range host.commandLines() {
		if strings.Contains(line, "/bin/launchctl bootstrap") {
			t.Fatalf("Register bootstrapped the job: %s", line)
		}
	}
}

// A step marked best-effort must not abort the plan: booting out a job that is
// not loaded is normal.
func TestABestEffortStepDoesNotAbortThePlan(t *testing.T) {
	host := newFakeHost()
	manager, options := managerFor(t, "darwin", host)
	// Every launchctl call fails, which is what an unloaded job looks like.
	host.failOn["/bin/launchctl"] = errors.New("Could not find service")
	// Start is not best-effort, so a failing kickstart must be reported.
	if _, err := manager.Start(context.Background()); err == nil {
		t.Fatal("Start ignored a failing kickstart")
	}
	// Uninstall marks every step best-effort, so it completes and removes the file.
	host.files[definitionPathFor("darwin", options.Home)] = []byte("plist")
	if _, err := manager.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall failed on a best-effort step: %v", err)
	}
	if _, still := host.files[definitionPathFor("darwin", options.Home)]; still {
		t.Fatal("Uninstall did not remove the definition")
	}
}

// The description a panel shows must match the plan the Manager would run.
func TestDescribeMatchesTheRenderedDefinition(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			host := newFakeHost()
			manager, _ := managerFor(t, goos, host)
			off, err := manager.Describe(false)
			if err != nil {
				t.Fatal(err)
			}
			on, err := manager.Describe(true)
			if err != nil {
				t.Fatal(err)
			}
			if off.LoginStart || !on.LoginStart {
				t.Fatalf("LoginStart flags: %v %v", off.LoginStart, on.LoginStart)
			}
			if off.Label != ServiceLabel || !strings.Contains(off.Label, "preview") {
				t.Fatalf("label: %q", off.Label)
			}
			switch goos {
			case "darwin":
				if !strings.Contains(off.Content, "<false/>") || !strings.Contains(on.Content, "<true/>") {
					t.Fatal("the rendered plists do not differ on RunAtLoad")
				}
			case "windows":
				offLines := strings.Join(flatten(off.Install), " ")
				onLines := strings.Join(flatten(on.Install), " ")
				if !strings.Contains(offLines, "/DISABLE") {
					t.Fatalf("the disabled rendering does not disable: %s", offLines)
				}
				if strings.Contains(onLines, "/DISABLE") {
					t.Fatalf("the enabled rendering still disables: %s", onLines)
				}
			}
			// Describing must not touch the machine.
			if len(host.commands) != 0 || len(host.files) != 0 {
				t.Fatal("Describe performed a side effect")
			}
		})
	}
}

// The daemon's own port must reach the definition, because a fixed port is what
// keeps an already-imported official URL working across a restart.
func TestTheConfiguredPortReachesTheDefinition(t *testing.T) {
	host := newFakeHost()
	manager, _ := managerFor(t, "darwin", host)
	description, err := manager.Describe(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description.Content, "<string>--port</string>") ||
		!strings.Contains(description.Content, "<string>43117</string>") {
		t.Fatalf("the port is missing from the definition:\n%s", description.Content)
	}
}

// A relative path or an unsupported platform must be refused before anything is
// written, not discovered halfway through.
func TestOpenManagerRefusesWhatItCannotOperate(t *testing.T) {
	host := newFakeHost()
	if _, err := OpenManager(ManagerOptions{Binary: "daemon", GOOS: "darwin", Host: host}); err == nil {
		t.Fatal("a relative binary was accepted")
	}
	if _, err := OpenManager(ManagerOptions{Binary: filepath.Join(t.TempDir(), "d"),
		StateDir: "state", GOOS: "darwin", Host: host}); err == nil {
		t.Fatal("a relative state directory was accepted")
	}
	if _, err := OpenManager(ManagerOptions{Binary: filepath.Join(t.TempDir(), "d"),
		GOOS: "linux", Host: host}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("linux: %v", err)
	}
}

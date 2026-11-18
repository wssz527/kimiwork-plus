package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ManagerState is what a panel renders about the user-level background service.
//
// The three booleans are separate on purpose, because they answer three
// different questions a user asks:
//
//	Registered  is there a definition on this machine at all
//	Enabled     will it start when the user logs in
//	Running     is the daemon up right now
//
// "Registered and not enabled" is the normal resting state: the user installed
// the app, nothing starts by itself. Collapsing these into one "installed" flag
// is how a product ends up quietly adding a login item.
type ManagerState struct {
	// Kind is "launchd" or "schtasks".
	Kind string `json:"kind"`
	// Label is the service identity.
	Label string `json:"label"`
	// DefinitionPath is the plist path, empty for a scheduled task.
	DefinitionPath string `json:"definitionPath,omitempty"`
	// Registered is true when a definition for this label exists.
	Registered bool `json:"registered"`
	// Enabled is true when the definition starts the daemon at login.
	Enabled bool `json:"enabled"`
	// Running is true when the service is up now.
	Running bool `json:"running"`
	// PID is the daemon pid when the platform reports one, else 0.
	PID int `json:"pid,omitempty"`
	// RegisteredBuildID is the build the definition was written for. Empty when
	// the platform cannot report it or no definition exists.
	RegisteredBuildID string `json:"registeredBuildId,omitempty"`
	// CurrentBuildID is the build asking, so a caller can see a stale definition
	// without trusting the daemon's own report.
	CurrentBuildID string `json:"currentBuildId,omitempty"`
	// Stale is true when a definition exists for a different build. The panel
	// says "restart to pick up the new version"; nothing is re-registered behind
	// the user's back.
	Stale bool `json:"stale,omitempty"`
	// Reason is a short machine-readable code when something is not available.
	Reason string `json:"reason,omitempty"`
	// Detail is one sentence for the panel.
	Detail string `json:"detail,omitempty"`
}

// ManagerOptions describes the service a Manager operates on.
type ManagerOptions struct {
	// Binary is the absolute path of the daemon executable.
	Binary string
	// StateDir is passed to the daemon as --state-dir.
	StateDir string
	// LogPath receives the daemon's stdout and stderr.
	LogPath string
	// Port is passed as --port when set. An empty value keeps the daemon's own
	// default. A fixed port is what keeps an already-imported official URL
	// working across a restart; port "0" means a new port each start.
	Port string
	// BuildID identifies the build writing the definition.
	BuildID string
	// Home overrides the home directory used for the definition path. It exists
	// so a test can render and inspect a definition without touching the real
	// one.
	Home string
	// UID overrides the launchd gui/<uid> domain. Same reason as Home.
	UID string
	// Host is the command and file seam. Nil means the real machine.
	Host ManagerHost
	// GOOS overrides the target platform. Nil means runtime.GOOS. It exists so
	// both plans can be built and inspected on one host; the manager never uses
	// it to pretend a platform is present.
	GOOS string
	// Transport is how a drain reaches the running daemon. Nil means the real
	// loopback HTTP client.
	Transport DaemonTransport
	// DrainTimeout bounds the graceful stop. Zero means fifteen seconds.
	DrainTimeout time.Duration
	// Now and Sleep are the clock seam, so a drain test does not wait.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// ManagerHost is the seam every side effect goes through.
//
// A Manager test must never register a real service, so the whole surface it
// touches is behind this interface: run a command, write the definition, read it
// back, remove it.
type ManagerHost interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	ReadFile(path string) ([]byte, error)
	Remove(path string) error
}

// localHost is the real machine.
type localHost struct{}

func (localHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	hideCommandConsole(command)
	var out strings.Builder
	command.Stdout = &out
	command.Stderr = &out
	err := command.Run()
	return out.String(), err
}

func (localHost) WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func (localHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (localHost) Remove(path string) error {
	err := os.Remove(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Manager operates the user-level background service.
//
// Every method is an explicit user action. Nothing here is called by first
// launch, by a window opening, or by an upgrade: Register writes a definition
// that does not start at login, and only Enable changes that. The daemon the
// service runs is the same binary the panel runs; this type never touches the
// official Kimi application.
//
// The methods also split what used to be one step. Enable and Disable change the
// login-start preference and deliberately do not unload, kill or restart a
// running daemon: turning a login item off must not interrupt a model request
// that is in flight. Stop is the only graceful way to end the daemon, and it
// uses the daemon's own authenticated HTTP endpoint rather than a platform
// termination.
type Manager interface {
	// Register writes or replaces the definition without enabling login start.
	Register(ctx context.Context) (ManagerState, error)
	// Enable turns login start on. It rewrites the definition and never unloads
	// a running job.
	Enable(ctx context.Context) (ManagerState, error)
	// Disable turns login start off, leaves the definition and the running
	// daemon in place.
	Disable(ctx context.Context) (ManagerState, error)
	// Start starts the daemon now, without changing the login-start setting and
	// without restarting a daemon that is already serving.
	Start(ctx context.Context) (ManagerState, error)
	// Stop asks the daemon to drain through its authenticated endpoint, then
	// confirms it stopped answering.
	Stop(ctx context.Context) (ManagerState, error)
	// Status reads the current state without changing anything.
	Status(ctx context.Context) (ManagerState, error)
	// Uninstall stops the daemon if it is running and removes the definition.
	Uninstall(ctx context.Context) (ManagerState, error)
	// Describe returns the definition text and the commands, for a panel that
	// wants to show what would change.
	Describe(loginStart bool) (ServiceDescription, error)
}

// serviceManager is the only implementation; the platform difference is in the
// plan it builds.
type serviceManager struct {
	options ManagerOptions
	goos    string
	host    ManagerHost
}

// OpenManager returns a Manager for this host.
func OpenManager(options ManagerOptions) (Manager, error) {
	if !filepath.IsAbs(options.Binary) {
		return nil, fmt.Errorf("service_binary_not_absolute")
	}
	if options.StateDir != "" && !filepath.IsAbs(options.StateDir) {
		return nil, fmt.Errorf("service_state_dir_not_absolute")
	}
	target := options.GOOS
	if target == "" {
		target = runtime.GOOS
	}
	if target != "darwin" && target != "windows" {
		return nil, ErrUnsupported
	}
	host := options.Host
	if host == nil {
		host = localHost{}
	}
	return &serviceManager{options: options, goos: target, host: host}, nil
}

// Describe renders the definition and commands without touching the machine.
func (m *serviceManager) Describe(loginStart bool) (ServiceDescription, error) {
	return describeServiceFor(m.goos, m.serviceOptions(), loginStart)
}

func (m *serviceManager) serviceOptions() ServiceOptions {
	return ServiceOptions{Binary: m.options.Binary, StateDir: m.options.StateDir,
		LogPath: m.options.LogPath, Port: m.options.Port, BuildID: m.options.BuildID}
}

func (m *serviceManager) transport() DaemonTransport {
	if m.options.Transport != nil {
		return m.options.Transport
	}
	return HTTPTransport{}
}

func (m *serviceManager) now() time.Time {
	if m.options.Now != nil {
		return m.options.Now()
	}
	return time.Now()
}

func (m *serviceManager) sleep(duration time.Duration) {
	if m.options.Sleep != nil {
		m.options.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func (m *serviceManager) drainTimeout() time.Duration {
	if m.options.DrainTimeout > 0 {
		return m.options.DrainTimeout
	}
	return 15 * time.Second
}

// definitionPath is where the definition lives for this platform and home.
func (m *serviceManager) definitionPath() string {
	return definitionPathFor(m.goos, m.home())
}

func (m *serviceManager) home() string {
	if m.options.Home != "" {
		return m.options.Home
	}
	return homeDirectory()
}

func (m *serviceManager) uid() string {
	if m.options.UID != "" {
		return m.options.UID
	}
	return currentUID()
}

// target is the launchd service target, or the task name on Windows.
func (m *serviceManager) target() string {
	if m.goos == "darwin" {
		return "gui/" + m.uid() + "/" + ServiceLabel
	}
	return ServiceLabel
}

// step is one side effect in a plan.
type step struct {
	// WriteFile, when set, writes Content to Path with Mode.
	WriteFile *fileStep
	// RemovePath, when set, removes that path.
	RemovePath string
	// Command, when set, runs Name with Args.
	Command *commandStep
	// IgnoreFailure keeps a best-effort step (an unload of something that is not
	// loaded) from failing the whole action.
	IgnoreFailure bool
}

type fileStep struct {
	Path    string
	Content string
	Mode    os.FileMode
}

type commandStep struct {
	Name string
	Args []string
}

// plan builds the ordered steps for one action.
func (m *serviceManager) plan(action string) ([]step, error) {
	options := m.serviceOptions()
	definition := m.definitionPath()
	switch m.goos {
	case "darwin":
		return launchdPlan(action, options, definition, m.uid())
	case "windows":
		return schtasksPlan(action, options, definition)
	}
	return nil, ErrUnsupported
}

// launchdPlan is the macOS action plan.
//
// The definition is a LaunchAgent in the user's own LaunchAgents directory, so
// the daemon runs as the signed-in user in the gui/<uid> domain and needs no
// elevation. RunAtLoad is the login-start switch: false for Register and
// Disable, true only for Enable.
//
// Enable and Disable only rewrite the definition. launchd reads it at login (and
// at bootstrap), so rewriting is enough to change what happens at the next login
// while leaving the daemon that is serving right now untouched. A reload
// (bootout + bootstrap) is a separate, destructive action and is never part of a
// preference change.
func launchdPlan(action string, options ServiceOptions, definition, uid string) ([]step, error) {
	target := "gui/" + uid + "/" + ServiceLabel
	domain := "gui/" + uid
	write := func(loginStart bool) []step {
		description, err := describeServiceFor("darwin", options, loginStart)
		if err != nil {
			return nil
		}
		return []step{{WriteFile: &fileStep{Path: definition, Content: description.Content, Mode: 0o600}}}
	}
	switch action {
	case actionRegister:
		return write(false), nil
	case actionEnable:
		return write(true), nil
	case actionDisable:
		return write(false), nil
	case actionStart:
		// bootstrap loads the job when it is not loaded (a disabled job does not
		// start by itself now that RunAtLoad is false), then kickstart starts it
		// without -k so a daemon that is already serving is left alone.
		return []step{
			{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"bootstrap", domain, definition}}, IgnoreFailure: true},
			{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"kickstart", target}}},
		}, nil
	case actionRestart:
		// The caller has already drained the old daemon; kickstart starts the
		// one this definition describes.
		return []step{
			{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"bootstrap", domain, definition}}, IgnoreFailure: true},
			{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"kickstart", target}}},
		}, nil
	case actionTerminate:
		// Last resort only. bootout unloads the job; the daemon receives a
		// termination signal and does not drain. It is reported as forced.
		return []step{{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"bootout", target}}, IgnoreFailure: true}}, nil
	case actionStatus:
		return []step{{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"print", target}}}}, nil
	case actionUninstall:
		return []step{
			{Command: &commandStep{Name: "/bin/launchctl", Args: []string{"bootout", target}}, IgnoreFailure: true},
			{RemovePath: definition, IgnoreFailure: true},
		}, nil
	}
	return nil, fmt.Errorf("service_action_unknown")
}

// schtasksPlan is the Windows action plan.
//
// The task is created ONLOGON with /RL LIMITED, which is the documented way to
// run as the signed-in user without elevation, and then immediately disabled.
// That combination is deliberate: schtasks has no "create without a trigger"
// form, and a task that exists but is disabled cannot start anything at login.
// Enable is the only step that makes it active.
//
// /End is not a graceful stop: it terminates the task's process and does not ask
// it to drain. It appears only in a forced path, and every caller that uses it
// reports a forced termination.
func schtasksPlan(action string, options ServiceOptions, definition string) ([]step, error) {
	commandLine := taskCommandLine(options)
	task := func(args ...string) step {
		return step{Command: &commandStep{Name: "schtasks", Args: append([]string{args[0], "/TN", ServiceLabel}, args[1:]...)}}
	}
	switch action {
	case actionRegister:
		return []step{
			{Command: &commandStep{Name: "schtasks", Args: []string{"/Create", "/F", "/SC", "ONLOGON",
				"/RL", "LIMITED", "/TN", ServiceLabel, "/TR", commandLine}}},
			task("/Change", "/DISABLE"),
		}, nil
	case actionEnable:
		return []step{task("/Change", "/ENABLE")}, nil
	case actionDisable:
		return []step{task("/Change", "/DISABLE")}, nil
	case actionStart, actionRestart:
		return []step{task("/Run")}, nil
	case actionTerminate:
		// /End kills the task's process. It is a forced termination and is never
		// described as a drain.
		return []step{{Command: &commandStep{Name: "schtasks", Args: []string{"/End", "/TN", ServiceLabel}}, IgnoreFailure: true}}, nil
	case actionStatus:
		return []step{
			task("/Query", "/XML"),
			task("/Query", "/FO", "LIST"),
		}, nil
	case actionUninstall:
		return []step{
			{Command: &commandStep{Name: "schtasks", Args: []string{"/End", "/TN", ServiceLabel}}, IgnoreFailure: true},
			{Command: &commandStep{Name: "schtasks", Args: []string{"/Delete", "/F", "/TN", ServiceLabel}}},
		}, nil
	}
	return nil, fmt.Errorf("service_action_unknown")
}

// taskCommandLine is what the task runs: the daemon with its arguments, quoted
// the way schtasks stores and CreateProcess parses.
func taskCommandLine(options ServiceOptions) string {
	commandLine := quoteWindows(options.Binary)
	for _, argument := range serviceArguments(options) {
		commandLine += " " + quoteWindows(argument)
	}
	return commandLine
}

const (
	actionRegister  = "register"
	actionEnable    = "enable"
	actionDisable   = "disable"
	actionStart     = "start"
	actionRestart   = "restart"
	actionTerminate = "terminate"
	actionStatus    = "status"
	actionUninstall = "uninstall"
)

// run executes a plan and returns the output of every command step, in order.
func (m *serviceManager) run(ctx context.Context, action string) ([]string, error) {
	steps, err := m.plan(action)
	if err != nil {
		return nil, err
	}
	return m.runSteps(ctx, steps)
}

func (m *serviceManager) runSteps(ctx context.Context, steps []step) ([]string, error) {
	outputs := make([]string, 0, len(steps))
	for _, item := range steps {
		switch {
		case item.WriteFile != nil:
			if err := m.host.WriteFile(item.WriteFile.Path, []byte(item.WriteFile.Content), item.WriteFile.Mode); err != nil {
				if item.IgnoreFailure {
					continue
				}
				return outputs, fmt.Errorf("service_write_failed: %w", err)
			}
		case item.RemovePath != "":
			if err := m.host.Remove(item.RemovePath); err != nil && !item.IgnoreFailure {
				return outputs, fmt.Errorf("service_remove_failed: %w", err)
			}
		case item.Command != nil:
			out, err := m.host.Run(ctx, item.Command.Name, item.Command.Args...)
			outputs = append(outputs, out)
			if err != nil && !item.IgnoreFailure {
				return outputs, fmt.Errorf("service_command_failed: %w", err)
			}
		}
	}
	return outputs, nil
}

// Register writes the definition without enabling login start. It does not
// bootstrap the job either: registration is a file the user asked for, and
// touching launchd here would make "registered but not started" false.
func (m *serviceManager) Register(ctx context.Context) (ManagerState, error) {
	if _, err := m.run(ctx, actionRegister); err != nil {
		return ManagerState{}, err
	}
	m.writeRecord(false)
	return m.Status(ctx)
}

// Enable is the only path that makes the daemon start at login. It rewrites the
// definition and does not reload the job, so a request in flight is unaffected.
func (m *serviceManager) Enable(ctx context.Context) (ManagerState, error) {
	if _, err := m.run(ctx, actionEnable); err != nil {
		return ManagerState{}, err
	}
	m.writeRecord(true)
	return m.Status(ctx)
}

// Disable turns login start off without removing the definition and without
// stopping the daemon that is running now.
func (m *serviceManager) Disable(ctx context.Context) (ManagerState, error) {
	if _, err := m.run(ctx, actionDisable); err != nil {
		return ManagerState{}, err
	}
	m.writeRecord(false)
	return m.Status(ctx)
}

// Start starts the daemon now.
func (m *serviceManager) Start(ctx context.Context) (ManagerState, error) {
	if _, err := m.run(ctx, actionStart); err != nil {
		return ManagerState{}, err
	}
	return m.Status(ctx)
}

// Stop asks the daemon to drain and exit through its own authenticated endpoint.
// A daemon that refuses or does not stop is reported, not killed.
func (m *serviceManager) Stop(ctx context.Context) (ManagerState, error) {
	result := m.drain(ctx)
	if result.Replaced {
		return ManagerState{}, errors.New(result.Reason)
	}
	if !result.Drained {
		return ManagerState{}, errors.New(result.Reason)
	}
	return m.Status(ctx)
}

// drain is the shared graceful stop.
func (m *serviceManager) drain(ctx context.Context) DrainResult {
	return drain(ctx, m.transport(), m.options.StateDir, m.drainTimeout(), m.now, m.sleep)
}

// terminate is the forced platform stop. It is used only after a failed drain
// and an explicit force.
func (m *serviceManager) terminate(ctx context.Context) ([]string, error) {
	return m.run(ctx, actionTerminate)
}

// Uninstall removes the definition. It drains first so an in-flight request can
// finish; a daemon that survives the drain is unloaded anyway, and the caller
// records that as forced.
func (m *serviceManager) Uninstall(ctx context.Context) (ManagerState, error) {
	_ = m.drain(ctx)
	if _, err := m.run(ctx, actionUninstall); err != nil {
		return ManagerState{}, err
	}
	if path := m.recordPath(); path != "" {
		_ = m.host.Remove(path)
	}
	return m.Status(ctx)
}

// Status reads the state. It never changes anything, and a missing service is a
// normal answer rather than an error.
//
// Registration and login start are read from two independent sources, because
// each alone can lie: the platform's own query says whether a definition exists
// and is active, and the record this build wrote says which build wrote it. When
// the platform query succeeds it wins on existence; when it fails, a readable
// definition file still counts, and a record without either does not.
func (m *serviceManager) Status(ctx context.Context) (ManagerState, error) {
	state := ManagerState{Kind: kindFor(m.goos), Label: ServiceLabel,
		DefinitionPath: m.definitionPath(), CurrentBuildID: m.options.BuildID}
	outputs, err := m.run(ctx, actionStatus)
	// A failing status command means "not registered" far more often than it
	// means "broken"; the platform parsers below decide, so the error is not
	// returned. The state carries the reason instead.
	_ = err
	switch m.goos {
	case "darwin":
		registered := false
		if content, readErr := m.host.ReadFile(state.DefinitionPath); readErr == nil {
			registered = true
			state.Enabled = plistRunAtLoad(string(content))
			// The plist carries the build that wrote it, so a definition
			// registered from a state directory this process cannot see is still
			// attributable. The record read below takes precedence.
			state.RegisteredBuildID = plistBuildID(string(content))
		}
		if len(outputs) > 0 {
			if running, pid, ok := parseLaunchdPrint(outputs[0]); ok {
				state.Running, state.PID = running, pid
				// A loaded job proves registration even when the definition file
				// is somewhere this process cannot read.
				registered = registered || ok
			}
		}
		state.Registered = registered
	case "windows":
		if len(outputs) > 0 {
			if enabled, ok := parseSchtasksEnabled(outputs[0]); ok {
				state.Registered, state.Enabled = true, enabled
			}
		}
		if len(outputs) > 1 {
			if running, ok := parseSchtasksStatus(outputs[1]); ok {
				state.Running = running
			}
		}
	}
	if record, ok := m.readRecord(); ok {
		// The record is the more specific evidence: it says which build wrote
		// this registration, and it survives on platforms whose definition has
		// no metadata field.
		state.RegisteredBuildID = record.BuildID
	}
	if !state.Registered {
		state.Reason = "not_registered"
		state.Detail = "No background service is registered for this user."
		return state, nil
	}
	if state.RegisteredBuildID != "" && state.CurrentBuildID != "" &&
		state.RegisteredBuildID != state.CurrentBuildID {
		state.Stale = true
		state.Reason = "registered_for_another_build"
		state.Detail = "The background service was registered by a different build. " +
			"Restart it from this build to update the definition."
	}
	return state, nil
}

func kindFor(goos string) string {
	if goos == "windows" {
		return "schtasks"
	}
	return "launchd"
}

// definitionPathFor is the plist location. A scheduled task has none, because
// the definition lives in the task database.
func definitionPathFor(goos, home string) string {
	if goos != "darwin" {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist")
}

// plistRunAtLoad reads the login-start flag back out of a rendered definition.
// The rendered plist puts the key on its own line with a <true/> or <false/>
// value, so this is a read of our own output rather than a general plist parser.
func plistRunAtLoad(content string) bool {
	return plistBool(content, "RunAtLoad")
}

func plistBool(content, key string) bool {
	index := strings.Index(content, "<key>"+key+"</key>")
	if index < 0 {
		return false
	}
	rest := content[index:]
	end := strings.Index(rest, "</key>")
	if end < 0 {
		return false
	}
	rest = rest[end:]
	return strings.Contains(rest[:min(len(rest), 80)], "<true/>")
}

// plistBuildID reads the build identity recorded next to the label.
func plistBuildID(content string) string {
	return plistString(content, "BuildID")
}

// plistString reads one <string> value by key from a rendered plist.
func plistString(content, key string) string {
	index := strings.Index(content, "<key>"+key+"</key>")
	if index < 0 {
		return ""
	}
	rest := content[index:]
	start := strings.Index(rest, "<string>")
	if start < 0 {
		return ""
	}
	rest = rest[start+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// definitionRecord is what this build wrote when it registered the service.
//
// schtasks offers no free-form metadata that survives a round trip, so the build
// identity cannot live in the task itself. Recording it in the state directory is
// the honest alternative: it says what *this build* wrote, and a definition
// written by another build simply has no record here, which Status reports as
// "unknown" rather than as a match.
type definitionRecord struct {
	BuildID    string `json:"buildId"`
	Binary     string `json:"binary"`
	StateDir   string `json:"stateDir,omitempty"`
	Port       string `json:"port,omitempty"`
	LoginStart bool   `json:"loginStart"`
}

// recordPath is where the record lives, or "" when there is no state directory to
// put it in.
func (m *serviceManager) recordPath() string {
	if m.options.StateDir == "" {
		return ""
	}
	return filepath.Join(m.options.StateDir, "service-definition.json")
}

// writeRecord records what was just registered. A failure here is not fatal: the
// service is registered and usable, only the provenance line is missing.
func (m *serviceManager) writeRecord(loginStart bool) {
	path := m.recordPath()
	if path == "" {
		return
	}
	record := definitionRecord{BuildID: m.options.BuildID, Binary: m.options.Binary,
		StateDir: m.options.StateDir, Port: m.options.Port, LoginStart: loginStart}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return
	}
	_ = m.host.WriteFile(path, append(encoded, '\n'), 0o600)
}

// readRecord reads the provenance line, if this build wrote one.
func (m *serviceManager) readRecord() (definitionRecord, bool) {
	path := m.recordPath()
	if path == "" {
		return definitionRecord{}, false
	}
	content, err := m.host.ReadFile(path)
	if err != nil {
		return definitionRecord{}, false
	}
	var record definitionRecord
	if json.Unmarshal(content, &record) != nil {
		return definitionRecord{}, false
	}
	return record, record.BuildID != ""
}

// parseLaunchdPrint reads the state line of `launchctl print`. The output is not
// a stable API, so only two unambiguous lines are read: the state word and the
// pid. A missing line means "not loaded", never "assume running".
func parseLaunchdPrint(output string) (running bool, pid int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if value, found := strings.CutPrefix(trimmed, "state = "); found {
			ok = true
			running = strings.TrimSpace(value) == "running"
		}
		if value, found := strings.CutPrefix(trimmed, "pid = "); found {
			if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
				pid = parsed
			}
		}
	}
	if ok && pid > 0 {
		running = true
	}
	return running, pid, ok
}

// parseSchtasksEnabled reads <Settings><Enabled> from `schtasks /Query /XML`.
func parseSchtasksEnabled(output string) (enabled bool, ok bool) {
	index := strings.Index(output, "<Enabled>")
	if index < 0 {
		return false, false
	}
	rest := output[index+len("<Enabled>"):]
	end := strings.Index(rest, "</Enabled>")
	if end < 0 {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(rest[:end]), "true"), true
}

// parseSchtasksStatus reads the Status line of `schtasks /Query /FO LIST`.
func parseSchtasksStatus(output string) (running bool, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		value, found := strings.CutPrefix(trimmed, "Status:")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		return strings.EqualFold(value, "Running"), true
	}
	return false, false
}

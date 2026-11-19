package platform

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// BackgroundFlag starts the daemon detached from the caller's console. The Run
// value and the panel's Start button both use it: a login start must not open a
// window, and a start from the panel must not make the app wait on the daemon.
const BackgroundFlag = "--start-background"

// TerminateFlag ends a daemon this binary started, after verifying the recorded
// pid is still this executable.
const TerminateFlag = "--terminate-daemon"

// RunBackgroundFromArgs is the whole contract a host binary needs for the two
// launch and stop flags: when argv[1] is one of them, run it and return its exit
// code. It answers (0, false) for every other command line, so the host can call
// it before installing its own flag set.
func RunBackgroundFromArgs(args []string) (int, bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case BackgroundFlag:
		if err := StartBackground(args[2:]); err != nil {
			return 1, true
		}
		return 0, true
	case TerminateFlag:
		if err := TerminateDaemon(args[2:]); err != nil {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// StartBackground starts the same executable with the daemon's own arguments and
// returns without waiting for it.
//
// A daemon that is already healthy under this state directory is left alone: a
// login start and a panel start can both arrive, and spawning a second process
// would leave two candidates for one lock. The child is detached so it outlives
// the shell or the app that started it, and its output goes to the log the caller
// named, never to a console that does not exist.
func StartBackground(args []string) error {
	options, err := parseDaemonArguments(args, true)
	if err != nil {
		return err
	}
	if options.StateDir == "" {
		return errors.New("state_dir_required")
	}
	if healthyDaemon(context.Background(), options.StateDir) {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	childArgs := []string{"--state-dir", options.StateDir}
	if options.Port != "" {
		childArgs = append(childArgs, "--port", options.Port)
	}
	command := exec.Command(executable, childArgs...)
	command.Stdin = nil
	if options.LogPath != "" {
		log, err := openHelperLog(options.LogPath)
		if err != nil {
			return err
		}
		defer log.Close()
		command.Stdout = log
		command.Stderr = log
	}
	detach(command)
	if err = command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// TerminateDaemon ends the daemon recorded for a state directory.
//
// Three checks stand between the record and a kill, because a pid is not an
// identity: the pid must be positive and not this process, the process must still
// be alive, and its executable must be this binary. A stale record therefore
// cannot take an unrelated program with it, and the official application is never
// a candidate because its path never equals this executable's.
//
// A missing record or an already-exited process is a success: the goal is that no
// daemon is left running.
func TerminateDaemon(args []string) error {
	options, err := parseDaemonArguments(args, false)
	if err != nil {
		return err
	}
	if options.StateDir == "" {
		return errors.New("state_dir_required")
	}
	endpoint, err := (HTTPTransport{}).Endpoint(options.StateDir)
	if err != nil {
		if errors.Is(err, errEndpointUnavailable) {
			return nil
		}
		return err
	}
	if endpoint.PID <= 0 || endpoint.PID == os.Getpid() {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if !terminablePID(endpoint.PID, executable) {
		return nil
	}
	process, err := os.FindProcess(endpoint.PID)
	if err != nil {
		return nil
	}
	return process.Kill()
}

// terminablePID reports whether the pid is alive and runs this executable.
func terminablePID(pid int, executable string) bool {
	if pid <= 0 || !processAlive(pid) {
		return false
	}
	path, ok := processExecutable(pid)
	if !ok {
		return false
	}
	return samePath(filepath.Clean(path), filepath.Clean(executable))
}

// healthTimeout bounds the probe StartBackground uses to avoid a second daemon.
const healthTimeout = 2 * time.Second

// healthyDaemon reports whether the recorded daemon still answers as itself.
func healthyDaemon(ctx context.Context, stateDir string) bool {
	transport := HTTPTransport{}
	endpoint, err := transport.Endpoint(stateDir)
	if err != nil {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	health := transport.Health(probe, endpoint)
	return health.Alive && health.PID == endpoint.PID
}

// daemonOptions is the subset of the daemon's own flags these two entry points
// read. The rest of the command line is the daemon's, and it parses it itself.
type daemonOptions struct {
	StateDir string
	Port     string
	LogPath  string
}

func parseDaemonArguments(args []string, withLog bool) (daemonOptions, error) {
	var options daemonOptions
	set := flag.NewFlagSet("kimi-work-plus", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&options.StateDir, "state-dir", "", "")
	set.StringVar(&options.Port, "port", "", "")
	if withLog {
		set.StringVar(&options.LogPath, "log-path", "", "")
	}
	if err := set.Parse(args); err != nil {
		return daemonOptions{}, err
	}
	if options.StateDir != "" && !filepath.IsAbs(options.StateDir) {
		return daemonOptions{}, errors.New("service_state_dir_not_absolute")
	}
	if options.LogPath != "" && !filepath.IsAbs(options.LogPath) {
		return daemonOptions{}, errors.New("service_log_path_not_absolute")
	}
	return options, nil
}

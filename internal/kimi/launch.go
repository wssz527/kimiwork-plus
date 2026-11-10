package kimi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// LaunchOptions describes one managed Kimi process.
//
// This package never launches the user's own application: the caller must pass
// a user-data directory it owns, and the child is started with
// -remote-debugging-pipe so no listening port exists. Starting Kimi is a
// product decision the panel makes; this type only carries out an explicit
// request.
type LaunchOptions struct {
	// Installation is the discovered install to run.
	Installation Installation
	// UserDataDir is the isolated profile directory for this child.
	UserDataDir string
	// BundleDir, when set, is exported as DAIMON_BUNDLE_DIR. Callers must have
	// confirmed the packaged build still honours it (LaunchResult "supported").
	BundleDir string
	// Env is the base environment. Empty means os.Environ().
	Env []string
	// ProtectedEnvNames are removed from the child environment. A key that must
	// not reach Kimi is listed here rather than trusted to be absent.
	ProtectedEnvNames []string
	// ExtraArgs are appended after the fixed arguments. Only a test sets this;
	// production callers leave it empty so the child always sees exactly
	// -remote-debugging-pipe plus its isolated profile.
	ExtraArgs []string
}

// Child is a running managed Kimi process and its CDP pipe.
type Child struct {
	command *exec.Cmd
	pipe    *Pipe

	readFrom io.ReadCloser
	writeTo  io.WriteCloser

	exitOnce sync.Once
	exit     ExitState
	exitWait chan struct{}

	mu      sync.Mutex
	stopped bool
}

// ExitState records how the child ended.
type ExitState struct {
	Kind   string `json:"kind"`
	Code   int    `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
	Known  bool   `json:"known"`
}

// PID is the child's process id, or 0 before it started.
func (c *Child) PID() int {
	if c.command == nil || c.command.Process == nil {
		return 0
	}
	return c.command.Process.Pid
}

// Pipe is the CDP transport for this child.
func (c *Child) Pipe() *Pipe { return c.pipe }

// Exit is closed when the child exits.
func (c *Child) Exit() <-chan struct{} { return c.exitWait }

// ExitState returns the recorded exit, valid once Exit() is closed.
func (c *Child) ExitState() ExitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exit
}

// StartKimi launches the installation with a private CDP pipe.
//
// The pipe uses the descriptors Chromium documents for -remote-debugging-pipe:
// the child reads commands from fd3 and writes responses to fd4. Go's ExtraFiles
// maps straight onto that: ExtraFiles[0] becomes fd3, ExtraFiles[1] becomes fd4.
//
// `ctx` bounds the launch request only, never the child. A managed Kimi is meant
// to outlive the HTTP call that asked for it, so the process is started with
// exec.Command and cancelled only through Child.Stop. Binding it to `ctx` would
// have `defer cancel()` in the caller kill a healthy application the moment
// Start returned.
func StartKimi(ctx context.Context, options LaunchOptions) (*Child, error) {
	if options.Installation.Application == "" {
		return nil, errors.New("kimi_application_missing")
	}
	if options.UserDataDir == "" || !filepath.IsAbs(options.UserDataDir) {
		return nil, errors.New("kimi_user_data_dir_invalid")
	}
	// A request that was already abandoned must not start a process.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kimi_start_cancelled: %w", err)
	}
	if err := os.MkdirAll(options.UserDataDir, 0o700); err != nil {
		return nil, fmt.Errorf("kimi_user_data_dir_unavailable: %w", err)
	}

	// Two OS pipes: commands flow parent -> child, responses child -> parent.
	commandReader, commandWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		commandReader.Close()
		commandWriter.Close()
		return nil, err
	}

	args := []string{"-remote-debugging-pipe", "--user-data-dir=" + options.UserDataDir}
	args = append(args, options.ExtraArgs...)
	command := exec.Command(options.Installation.Application, args...)
	command.Env = childEnvironment(options)
	// ExtraFiles[0] is fd3 (child reads commands), ExtraFiles[1] is fd4 (child
	// writes responses). stdio stays detached: this process has no terminal.
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.ExtraFiles = []*os.File{commandReader, responseWriter}
	configureManagedChild(command)

	if err := command.Start(); err != nil {
		commandReader.Close()
		commandWriter.Close()
		responseReader.Close()
		responseWriter.Close()
		return nil, fmt.Errorf("kimi_start_failed: %w", err)
	}
	// The child holds these ends now; keeping them open here would stop the
	// pipe from ever reporting EOF.
	commandReader.Close()
	responseWriter.Close()

	child := &Child{command: command, readFrom: responseReader, writeTo: commandWriter,
		exitWait: make(chan struct{})}
	child.pipe = NewPipe(responseReader, commandWriter)
	go child.await()
	return child, nil
}

// await records how the child ended and releases the pipe.
func (c *Child) await() {
	err := c.command.Wait()
	c.exitOnce.Do(func() {
		state := ExitState{Known: true, Kind: "application-exit"}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				state.Code = exitErr.ExitCode()
			} else {
				state.Kind = "start-failed"
			}
		}
		c.mu.Lock()
		c.exit = state
		c.mu.Unlock()
		c.pipe.Dispose()
		close(c.exitWait)
	})
}

// childEnvironment builds the child's environment. Every protected name is
// removed, and DAIMON_BUNDLE_DIR is set only when the caller asked for it: a
// leftover value from this process must never leak into Kimi.
func childEnvironment(options LaunchOptions) []string {
	base := options.Env
	if base == nil {
		base = os.Environ()
	}
	protected := map[string]bool{"ELECTRON_RUN_AS_NODE": true, "DAIMON_BUNDLE_DIR": true}
	for _, name := range options.ProtectedEnvNames {
		if name != "" {
			protected[name] = true
		}
	}
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if protected[name] {
			continue
		}
		out = append(out, entry)
	}
	if options.BundleDir != "" {
		out = append(out, "DAIMON_BUNDLE_DIR="+options.BundleDir)
	}
	return out
}

// Stop ends the child. It never targets any other process: only the pid this
// handle started is signalled.
func (c *Child) Stop(timeout time.Duration) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	if c.command.Process == nil {
		return nil
	}
	_ = c.command.Process.Signal(os.Interrupt)
	select {
	case <-c.exitWait:
		return nil
	case <-time.After(timeout):
	}
	if err := c.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-c.exitWait
	return nil
}

// ManagedChildConfigured reports whether this platform can start a child with
// extra descriptors. It exists so the panel can say "unsupported here" instead
// of failing at start time on a platform without it.
func ManagedChildConfigured() bool {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return true
	}
	return false
}

//go:build !windows

package kimi

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareManagedChild attaches the child's two pipe ends as file descriptors 3
// and 4 and puts it in its own process group so a stop request reaches it and
// not this process's group.
//
// ExtraFiles[0] becomes fd3 (the child reads commands) and ExtraFiles[1]
// becomes fd4 (the child writes responses). That is the mapping Chromium
// documents for -remote-debugging-pipe on POSIX, and it is the only argument
// this transport needs.
func prepareManagedChild(command *exec.Cmd, pipes launchPipes) ([]string, error) {
	command.ExtraFiles = []*os.File{pipes.commandReader, pipes.responseWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil, nil
}

// requestChildStop asks the child to end. SIGINT is the polite request a POSIX
// Chromium understands; the escalation to SIGKILL is Stop's job, after the
// caller's timeout.
func requestChildStop(child *Child) {
	if child.command.Process == nil {
		return
	}
	_ = child.command.Process.Signal(os.Interrupt)
}

// ManagedTransportInfo reports the Unix transport: descriptors 3 and 4, which is
// what Chromium reads for -remote-debugging-pipe on POSIX.
//
// Verified stays false because this build has only exercised the transport with
// a stand-in child that speaks the same protocol, not with the official
// application. The descriptor mapping itself is not in doubt; the claim "the
// official app answers over it" has not been made on a real launch.
func ManagedTransportInfo() ManagedTransport {
	return ManagedTransport{
		Kind: "fd-pipe", Supported: true,
		Reason: "stand_in_child_only",
		Detail: "Commands and responses use file descriptors 3 and 4 of the managed child; " +
			"exercised against a stand-in child, not the official application.",
	}
}

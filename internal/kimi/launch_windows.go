//go:build windows

package kimi

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// The Windows transport is not fd3/fd4. os/exec documents ExtraFiles as
// unsupported on Windows, and Chromium does not read CRT descriptors there in
// the way it does on POSIX. What it does read is
// --remote-debugging-io-pipes=<read handle>,<write handle>, where both values are
// real pipe HANDLEs that the child inherited *by value*.
//
// See launch_iopipes.go for the source citations. The three requirements that
// follow from them, and that this file implements, are:
//
//  1. The handles must be pipe handles: Chromium rejects anything whose
//     GetFileType is not FILE_TYPE_PIPE.
//  2. They must be inheritable and the child must inherit them by value, so the
//     numbers stay valid in the child. Go's SysProcAttr.AdditionalInheritedHandles
//     is exactly that: it puts the handles in PROC_THREAD_ATTRIBUTE_HANDLE_LIST
//     and CreateProcess is called with bInheritHandles=TRUE, so only the listed
//     handles cross the boundary.
//  3. The handle values must survive Chromium's parser, which reads them as
//     unsigned 32-bit integers. A value above 2^32-1 is refused here instead of
//     being silently truncated into a wrong handle.

const (
	// createNoWindow keeps a console from flashing up for the managed child.
	createNoWindow = 0x08000000
)

// prepareManagedChild makes the child's two pipe ends inheritable, lists exactly
// those two handles for inheritance, and returns the switch that names them.
//
// Nothing else is added to the list. The child inherits no other handle from this
// process, which is the point of the handle list: a broad inherit would hand a
// managed Kimi whatever this process happens to hold.
func prepareManagedChild(command *exec.Cmd, pipes launchPipes) ([]string, error) {
	readHandle, err := inheritablePipeHandle(pipes.commandReader, "commands")
	if err != nil {
		return nil, err
	}
	writeHandle, err := inheritablePipeHandle(pipes.responseWriter, "responses")
	if err != nil {
		return nil, err
	}
	if uint64(readHandle) > 0xffffffff || uint64(writeHandle) > 0xffffffff {
		// Chromium parses these as uint32. A larger value cannot be named
		// correctly, so refuse rather than pass a truncated handle.
		return nil, fmt.Errorf("%w: handle value does not fit the 32-bit switch", errManagedChildUnsupported)
	}

	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              createNoWindow,
		AdditionalInheritedHandles: []syscall.Handle{readHandle, writeHandle},
	}
	return []string{"--remote-debugging-io-pipes=" + ioPipesArgument(uintptr(readHandle), uintptr(writeHandle))}, nil
}

// inheritablePipeHandle returns the raw handle behind one pipe end and makes sure
// it is marked inheritable. os.Pipe already creates inheritable handles, and
// setting the flag again is idempotent; checking is cheap and a pipe end that
// silently is not a pipe would fail deep inside the child instead.
func inheritablePipeHandle(file *os.File, name string) (syscall.Handle, error) {
	if file == nil {
		return 0, fmt.Errorf("%w: %s pipe missing", errManagedChildUnsupported, name)
	}
	handle := syscall.Handle(file.Fd())
	if handle == syscall.InvalidHandle || handle == 0 {
		return 0, fmt.Errorf("%w: %s pipe has no handle", errManagedChildUnsupported, name)
	}
	if kind, err := syscall.GetFileType(handle); err != nil || kind != syscall.FILE_TYPE_PIPE {
		// Chromium's AdoptHandle() refuses a non-pipe handle too, so this is the
		// same check made where the failure can still be reported to a caller.
		return 0, fmt.Errorf("%w: %s end is not a pipe", errManagedChildUnsupported, name)
	}
	if err := syscall.SetHandleInformation(handle, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		return 0, fmt.Errorf("%w: %s end is not inheritable", errManagedChildUnsupported, name)
	}
	return handle, nil
}

// requestChildStop asks the child to end. Windows has no signal a Chromium
// browser honours, but closing the parent's command-pipe end is the documented
// CDP disconnect: DevToolsPipeHandler's reader sees EOF and posts its
// on_disconnect closure, which in Electron quits the browser.
//
// This is a disconnect request, not a kill. Production Stop reports a child that
// does not exit so the user can close it normally; only test stand-ins may opt
// into forced cleanup after the caller's timeout.
func requestChildStop(child *Child) {
	if child.writeTo == nil {
		return
	}
	_ = child.writeTo.Close()
}

// ManagedTransportInfo reports the Windows transport using inherited pipe
// handles passed via --remote-debugging-io-pipes.
func ManagedTransportInfo() ManagedTransport {
	return ManagedTransport{
		Kind:      "handle-pipe",
		Supported: true,
		Verified:  true,
		Detail:    "Commands and responses use inherited pipe handles named in --remote-debugging-io-pipes.",
	}
}

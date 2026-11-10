package kimi

import (
	"errors"
	"strconv"
	"strings"
)

// Chromium accepts its two Windows debug pipe handles as a comma-separated pair
// of unsigned integers on --remote-debugging-io-pipes. The values are HANDLE
// values, not CRT file descriptors, and they must be inherited by value: the
// child's copy keeps the parent's numeric value, which is what lets the child
// name them at all.
//
// Evidence (Chromium and Electron source, checked 2026-11-10):
//
//   - content/public/common/content_switches.cc: kRemoteDebuggingIoPipes is
//     "remote-debugging-io-pipes", documented as "a comma separated list of two
//     pipe handles serialized as unsigned integers, e.g.
//     --remote-debugging-io-pipes=3,4". Present in 123, 140 and 150.
//   - content/browser/devtools/devtools_agent_host_impl.cc: StartRemoteDebugging
//     PipeHandler defaults to kReadFD=3 / kWriteFD=4 and, on Windows only,
//     replaces them with AdoptPipes(...) when the switch is non-empty.
//     AdoptHandle() requires GetFileType(handle)==FILE_TYPE_PIPE and then calls
//     _open_osfhandle(handle, flags), so the value is a real pipe HANDLE.
//   - electron/shell/browser/electron_browser_main_parts.cc (v43.7.7, which is
//     the framework inside the installed Kimi 3.2.25) calls
//     StartRemoteDebuggingPipeHandler when --remote-debugging-pipe is present,
//     with no Electron-specific override of the fd/handle logic.
//
// That is why the Windows launcher passes inherited handles plus this switch
// instead of ExtraFiles: os/exec documents ExtraFiles as unsupported on Windows,
// and even if it worked it would assign CRT descriptors, which is the Unix
// convention and not what this switch reads.

// ioPipesArgument renders the switch value for two handle values. It is pure so
// a test can check the exact wire format on any platform.
func ioPipesArgument(read, write uintptr) string {
	return strconv.FormatUint(uint64(read), 10) + "," + strconv.FormatUint(uint64(write), 10)
}

// parseIOPipes reads back an --remote-debugging-io-pipes value. It exists so the
// format has one definition instead of a second, unchecked copy in a test.
func parseIOPipes(value string) (read, write uintptr, ok bool) {
	first, second, found := strings.Cut(value, ",")
	if !found {
		return 0, 0, false
	}
	readValue, err := strconv.ParseUint(strings.TrimSpace(first), 10, 32)
	if err != nil {
		return 0, 0, false
	}
	writeValue, err := strconv.ParseUint(strings.TrimSpace(second), 10, 32)
	if err != nil {
		return 0, 0, false
	}
	return uintptr(readValue), uintptr(writeValue), true
}

// errManagedChildUnsupported is returned when this platform has no CDP transport
// for a managed child. It is a refusal, not a degraded launch: a child started
// without a transport would run with no way to talk to it.
var errManagedChildUnsupported = errors.New("managed_child_transport_unsupported")

//go:build darwin

package appupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestMatchingBinaryPIDsHandlesLiteralPlusAndSpaces(t *testing.T) {
	target := "/Applications/Kimi Work+.app/Contents/MacOS/KimiWorkPlus"
	psOut := strings.Join([]string{
		"  101 /Applications/Kimi Work+.app/Contents/MacOS/KimiWorkPlus",
		"  102 /Applications/Kimi Work+.app/Contents/MacOS/KimiWorkPlus --state-dir /tmp/test",
		"  103 /Applications/Kimi Work+ Preview.app/Contents/MacOS/KimiWorkPlus",
		"  104 /Applications/Kimi Work+app/Contents/MacOS/KimiWorkPlus",
		"  105 /Applications/Kimi Work+.app/Contents/MacOS/KimiWorkPlusHelper",
		"  106 /Applications/Kimi Work+.app/Contents/MacOS/gateway-daemon --port 0",
		"  107 /Applications/Kimi Work+.app/Contents/MacOS/KimiWorkPlus --self",
	}, "\n")

	got := matchingBinaryPIDs(psOut, target, 107)
	want := []int{101, 102}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matchingBinaryPIDs() = %v, want %v", got, want)
	}
}

func TestStopTargetDesktopTerminatesAppBundleWithPlusInPath(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "Kimi Work+.app")
	binDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sleepSrc, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not available")
	}
	fakeDesktop := filepath.Join(binDir, "KimiWorkPlus")
	data, err := os.ReadFile(sleepSrc)
	if err != nil {
		t.Fatalf("read sleep: %v", err)
	}
	if err := os.WriteFile(fakeDesktop, data, 0o755); err != nil {
		t.Fatalf("write fake desktop: %v", err)
	}
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", fakeDesktop).CombinedOutput(); err != nil {
		t.Fatalf("codesign fake desktop: %v (%s)", err, string(out))
	}

	cmd := exec.Command(fakeDesktop, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake desktop: %v", err)
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("fake desktop exited before stopTargetDesktop ran: %v", err)
	}

	var logs []string
	stopTargetDesktop(appDir, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	<-done
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("expected process %d to have exited; logs=%v", pid, logs)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "stopped desktop processes cleanly via SIGTERM") {
		t.Fatalf("expected clean SIGTERM diagnostic log, got: %s", joined)
	}
}

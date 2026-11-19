//go:build windows

package appupdate

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/wssz527/kimiwork-plus/internal/platform"
)

const (
	createNoWindow                 = 0x08000000
	detachedProcess                = 0x00000008
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	hkeyCurrentUser                = uintptr(0x80000001)
	keyWrite                       = 0x20006
	regSZ                          = 1
	uninstallRegSubKey             = `Software\Microsoft\Windows\CurrentVersion\Uninstall\dev.kimi-work-plus`
)

var (
	advapi32UpdateDLL  = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW  = advapi32UpdateDLL.NewProc("RegOpenKeyExW")
	procRegSetValueExW = advapi32UpdateDLL.NewProc("RegSetValueExW")
	procRegCloseKey    = advapi32UpdateDLL.NewProc("RegCloseKey")
)

func configureDetachedHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
		if err != nil {
			return
		}
		var code uint32
		err = syscall.GetExitCodeProcess(h, &code)
		_ = syscall.CloseHandle(h)
		if err != nil || code != stillActive {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func stopTargetDesktop(targetDir string, logf func(string, ...any)) {
	script := fmt.Sprintf(`$dir = '%s'; Get-Process -Name 'KimiWorkPlus','gateway-daemon' -ErrorAction SilentlyContinue | Where-Object { try { $_.Path -and $_.Path.StartsWith($dir, [System.StringComparison]::OrdinalIgnoreCase) } catch { $false } } | Stop-Process -Force -ErrorAction SilentlyContinue`,
		strings.ReplaceAll(targetDir, "'", "''"))
	u16 := utf16.Encode([]rune(script))
	raw := make([]byte, len(u16)*2)
	for i, code := range u16 {
		binary.LittleEndian.PutUint16(raw[i*2:], code)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	psExe := "powershell.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		psExe = filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	cmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-InputFormat", "None", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = cmd.Run()
	time.Sleep(400 * time.Millisecond)
	logf("stopped Windows processes under %s", targetDir)
}

func validateStagedArtifact(stagedDir string) error {
	return validateWindowsDir(stagedDir)
}

func validateInstalledArtifact(installedDir string) error {
	return validateWindowsDir(installedDir)
}

func validateWindowsDir(dir string) error {
	for _, name := range []string{"KimiWorkPlus.exe", "gateway-daemon.exe"} {
		p := filepath.Join(dir, name)
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("missing %s: %w", name, err)
		}
		var magic [2]byte
		n, readErr := f.Read(magic[:])
		_ = f.Close()
		if readErr != nil || n < 2 || magic[0] != 'M' || magic[1] != 'Z' {
			return fmt.Errorf("invalid PE binary %s", name)
		}
	}
	return nil
}

func swapInstallation(targetDir, stagedDir, backupDir string, logf func(string, ...any)) error {
	required := []string{"KimiWorkPlus.exe", "gateway-daemon.exe"}
	optional := []string{"LICENSE", "THIRD_PARTY_NOTICES.txt"}

	swapped := make([]string, 0, len(required)+len(optional))
	rollbackPartial := func() {
		if backupDir == "" {
			return
		}
		for _, name := range swapped {
			bakFile := filepath.Join(backupDir, name)
			if _, err := os.Stat(bakFile); err == nil {
				_ = replaceWindowsBinary(filepath.Join(targetDir, name), bakFile)
			}
		}
	}

	for _, name := range required {
		src := filepath.Join(stagedDir, name)
		dst := filepath.Join(targetDir, name)
		if err := replaceWindowsBinary(dst, src); err != nil {
			rollbackPartial()
			return fmt.Errorf("replace %s: %w", name, err)
		}
		swapped = append(swapped, name)
	}
	for _, name := range optional {
		src := filepath.Join(stagedDir, name)
		if _, err := os.Stat(src); err == nil {
			_ = replaceWindowsBinary(filepath.Join(targetDir, name), src)
		}
	}
	logf("Windows dual executables swapped cleanly in %s", targetDir)
	return nil
}

func restoreInstallation(targetDir, backupDir string, logf func(string, ...any)) error {
	for _, name := range []string{"KimiWorkPlus.exe", "gateway-daemon.exe"} {
		src := filepath.Join(backupDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("backup missing %s: %w", name, err)
		}
		if err := replaceWindowsBinary(filepath.Join(targetDir, name), src); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}
	logf("Windows dual executables restored from %s", backupDir)
	return nil
}

func replaceWindowsBinary(target, src string) error {
	sweepWindowsAsides(target)
	tmp := fmt.Sprintf("%s.next.%d.tmp", target, time.Now().UnixNano())
	if err := copyFileExact(src, tmp, 0o755); err != nil {
		return err
	}
	for attempt := 0; attempt < 15; attempt++ {
		if err := os.Remove(target); err == nil || errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(tmp, target); err == nil {
				return nil
			}
		}
		aside := fmt.Sprintf("%s.old.%d", target, time.Now().UnixNano())
		if err := os.Rename(target, aside); err == nil {
			if err := os.Rename(tmp, target); err == nil {
				_ = os.Remove(aside)
				return nil
			}
			_ = os.Rename(aside, target)
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return fmt.Errorf("timed out replacing %s", filepath.Base(target))
}

func sweepWindowsAsides(target string) {
	matches, err := filepath.Glob(target + ".old.*")
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func launchSysProcAttr(launchMode string) *syscall.SysProcAttr {
	if launchMode == "daemon" {
		return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	}
	return &syscall.SysProcAttr{CreationFlags: detachedProcess}
}

func launchInstallation(targetDir, stateDir, launchMode string, logf func(string, ...any)) error {
	defaultDir, _ := platform.DefaultStateDir()
	customState := stateDir != "" && stateDir != defaultDir

	var bin string
	args := []string{}
	if launchMode == "daemon" {
		bin = filepath.Join(targetDir, "gateway-daemon.exe")
		if stateDir != "" {
			args = append(args, "--state-dir", stateDir)
		}
	} else {
		bin = filepath.Join(targetDir, "KimiWorkPlus.exe")
		if customState {
			args = append(args, "--state-dir", stateDir)
		}
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = targetDir
	cmd.SysProcAttr = launchSysProcAttr(launchMode)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	logf("launched Windows binary: %s", bin)
	return nil
}

func postInstallUpdateMetadata(plan ApplyPlan, health daemonHealth) {
	if health.Version == "" {
		return
	}
	subKey, err := syscall.UTF16PtrFromString(uninstallRegSubKey)
	if err != nil {
		return
	}
	var hKey uintptr
	r1, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(subKey)), 0, keyWrite, uintptr(unsafe.Pointer(&hKey)))
	if r1 != 0 {
		return
	}
	defer procRegCloseKey.Call(hKey)
	kPtr, err := syscall.UTF16PtrFromString("DisplayVersion")
	if err != nil {
		return
	}
	vSlice, err := syscall.UTF16FromString(health.Version)
	if err != nil {
		return
	}
	_, _, _ = procRegSetValueExW.Call(hKey, uintptr(unsafe.Pointer(kPtr)), 0, regSZ, uintptr(unsafe.Pointer(&vSlice[0])), uintptr(len(vSlice)*2))
}

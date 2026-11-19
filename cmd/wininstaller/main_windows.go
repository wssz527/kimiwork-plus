//go:build windows

package main

import (
	"context"
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
	uninstallRegSubKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\dev.kimi-work-plus`
	shortcutName       = "Kimi Work+.lnk"
	createNoWindow     = 0x08000000
	detachedProcess    = 0x00000008
)

var (
	user32DLL           = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW     = user32DLL.NewProc("MessageBoxW")
	advapi32DLL         = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW = advapi32DLL.NewProc("RegCreateKeyExW")
	procRegSetValueExW  = advapi32DLL.NewProc("RegSetValueExW")
	procRegCloseKey     = advapi32DLL.NewProc("RegCloseKey")
	procRegDeleteTreeW  = advapi32DLL.NewProc("RegDeleteTreeW")
)

const (
	hkeyCurrentUser = uintptr(0x80000001)
	keyWrite        = 0x20006
	regSZ           = 1
	regDWord        = 4

	mbOK              = 0x00000000
	mbOKCancel        = 0x00000001
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconQuestion    = 0x00000020
	mbIconInformation = 0x00000040
	idOK              = 1
	idYes             = 6
)

type options struct {
	silent    bool
	uninstall bool
	noLaunch  bool
	launch    bool
	dir       string
}

func parseArgs(args []string) options {
	var opt options
	for i := 0; i < len(args); i++ {
		arg := args[i]
		lower := strings.ToLower(arg)
		switch {
		case lower == "/s" || lower == "--silent" || lower == "-s":
			opt.silent = true
		case lower == "--uninstall" || lower == "/uninstall":
			opt.uninstall = true
		case lower == "--no-launch":
			opt.noLaunch = true
		case lower == "--launch":
			opt.launch = true
		case (lower == "--dir" || lower == "/d") && i+1 < len(args):
			i++
			opt.dir = args[i]
		case strings.HasPrefix(lower, "--dir="):
			opt.dir = arg[len("--dir="):]
		case strings.HasPrefix(lower, "/d="):
			opt.dir = arg[len("/d="):]
		}
	}
	return opt
}

func defaultInstallDir() (string, error) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(localAppData, "Programs", "Kimi Work+"), nil
}

func startMenuShortcutPath() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", shortcutName), nil
}

func main() {
	opt := parseArgs(os.Args[1:])
	var err error
	if opt.uninstall {
		err = runUninstall(opt)
	} else {
		err = runInstall(opt)
	}
	if err != nil {
		if !opt.silent {
			messageBox("KimiWork + 安装程序", err.Error(), mbOK|mbIconError)
		} else {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(1)
	}
}

func runInstall(opt options) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位安装程序路径：%w", err)
	}
	installDir := opt.dir
	if installDir == "" {
		installDir, err = defaultInstallDir()
		if err != nil {
			return fmt.Errorf("无法确定用户安装目录：%w", err)
		}
	}
	installDir, err = filepath.Abs(installDir)
	if err != nil {
		return err
	}

	meta, _, err := inspectPayload(exePath)
	if err != nil {
		return fmt.Errorf("安装包数据不完整：%w", err)
	}
	version := meta.Version
	if version == "" {
		version = "1.0.0"
	}
	buildID := meta.BuildID
	if buildID == "" {
		buildID = "development"
	}

	if !opt.silent {
		prompt := fmt.Sprintf("即将安装 KimiWork + %s 到当前用户目录：\n\n%s\n\n安装过程会创建开始菜单快捷方式并注册用户级后台服务，不需要管理员权限。是否继续？", version, installDir)
		if messageBox("安装 KimiWork +", prompt, mbOKCancel|mbIconQuestion) != idOK {
			return nil
		}
	}

	stopExistingDaemons()
	stopDesktopInDir(installDir)

	// Remember whether the formal service already had login-start enabled before upgrading.
	wasLoginEnabled := false
	stateDir, _ := platform.DefaultStateDir()
	daemonPath := filepath.Join(installDir, "gateway-daemon.exe")
	if stateDir != "" {
		if mgr, mErr := platform.OpenManager(platform.ManagerOptions{
			Binary:   daemonPath,
			StateDir: stateDir,
			BuildID:  buildID,
		}); mErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if st, sErr := mgr.Status(ctx); sErr == nil && st.Enabled {
				wasLoginEnabled = true
			}
			cancel()
		}
	}

	if _, _, err := extractPayload(exePath, installDir); err != nil {
		return fmt.Errorf("写入程序文件失败：%w", err)
	}

	desktopPath := filepath.Join(installDir, "KimiWorkPlus.exe")
	if _, err := os.Stat(desktopPath); err != nil {
		return fmt.Errorf("安装后缺少 KimiWorkPlus.exe：%w", err)
	}
	if _, err := os.Stat(daemonPath); err != nil {
		return fmt.Errorf("安装后缺少 gateway-daemon.exe：%w", err)
	}

	uninstallPath := filepath.Join(installDir, "uninstall.exe")
	if err := copyUninstallerStub(exePath, uninstallPath, meta.StubSize); err != nil {
		return fmt.Errorf("写入卸载程序失败：%w", err)
	}

	if err := createStartMenuShortcut(desktopPath, installDir); err != nil {
		return fmt.Errorf("创建开始菜单快捷方式失败：%w", err)
	}
	if err := writeUninstallRegistry(installDir, desktopPath, uninstallPath, version); err != nil {
		return fmt.Errorf("写入系统卸载信息失败：%w", err)
	}

	if stateDir != "" {
		if mgr, mErr := platform.OpenManager(platform.ManagerOptions{
			Binary:   daemonPath,
			StateDir: stateDir,
			BuildID:  buildID,
		}); mErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = mgr.Register(ctx)
			if wasLoginEnabled {
				_, _ = mgr.Enable(ctx)
			}
			cancel()
		}
	}

	shouldLaunch := false
	if opt.silent {
		shouldLaunch = opt.launch && !opt.noLaunch
	} else if !opt.noLaunch {
		ans := messageBox("KimiWork + 安装完成", "KimiWork + 已安装完成。\n\n是否现在启动 KimiWork +？", mbYesNo|mbIconInformation)
		shouldLaunch = ans == idYes
	}
	if shouldLaunch {
		cmd := exec.Command(desktopPath)
		cmd.Dir = installDir
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
		_ = cmd.Start()
	}
	return nil
}

func runUninstall(opt options) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	installDir := opt.dir
	if installDir == "" {
		installDir = filepath.Dir(exePath)
	}
	installDir, err = filepath.Abs(installDir)
	if err != nil {
		return err
	}
	// Safety check: refuse to uninstall from a directory that does not look like KimiWork +.
	if _, err := os.Stat(filepath.Join(installDir, "KimiWorkPlus.exe")); err != nil {
		if _, err2 := os.Stat(filepath.Join(installDir, "uninstall.exe")); err2 != nil {
			return errors.New("目标目录不包含 KimiWork + 安装文件，已取消卸载。")
		}
	}

	if !opt.silent {
		prompt := fmt.Sprintf("确定卸载位于以下位置的 KimiWork +？\n\n%s\n\n后台服务和开始菜单快捷方式会被移除；您的账号、配置和用量账本保留在用户配置目录中。", installDir)
		if messageBox("卸载 KimiWork +", prompt, mbYesNo|mbIconQuestion) != idYes {
			return nil
		}
	}

	stateDir, _ := platform.DefaultStateDir()
	daemonPath := filepath.Join(installDir, "gateway-daemon.exe")
	if stateDir != "" {
		if mgr, mErr := platform.OpenManager(platform.ManagerOptions{
			Binary:   daemonPath,
			StateDir: stateDir,
		}); mErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = mgr.Uninstall(ctx)
			cancel()
		}
	}
	stopExistingDaemons()
	stopDesktopInDir(installDir)

	if scPath, err := startMenuShortcutPath(); err == nil {
		_ = os.Remove(scPath)
	}
	deleteUninstallRegistry()

	for _, name := range []string{"KimiWorkPlus.exe", "gateway-daemon.exe", "LICENSE", "THIRD_PARTY_NOTICES.txt"} {
		p := filepath.Join(installDir, name)
		_ = os.Remove(p)
		sweepStaleAsides(p)
	}
	_ = os.RemoveAll(filepath.Join(installDir, ".rollback"))

	scheduleSelfDelete(installDir)
	if !opt.silent {
		messageBox("KimiWork + 卸载完成", "KimiWork + 及其后台服务已移除。", mbOK|mbIconInformation)
	}
	return nil
}

func stopDaemonInDir(dir string) {
	if dir == "" {
		return
	}
	transport := platform.HTTPTransport{Timeout: 2 * time.Second}
	ep, err := transport.Endpoint(dir)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = transport.Stop(ctx, ep)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
		health := transport.Health(probeCtx, ep)
		probeCancel()
		if !health.Alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func stopExistingDaemons() {
	if formalDir, err := platform.DefaultStateDir(); err == nil {
		stopDaemonInDir(formalDir)
	}
	if legacyDir, err := platform.LegacyPreviewStateDir(); err == nil {
		stopDaemonInDir(legacyDir)
	}
}

func runPowerShellHidden(script string) ([]byte, error) {
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
	return cmd.CombinedOutput()
}

func stopDesktopInDir(installDir string) {
	ps := fmt.Sprintf(`$dir = %s; Get-Process -Name 'KimiWorkPlus','gateway-daemon' -ErrorAction SilentlyContinue | Where-Object { try { $_.Path -and $_.Path.StartsWith($dir, [System.StringComparison]::OrdinalIgnoreCase) } catch { $false } } | Stop-Process -Force -ErrorAction SilentlyContinue`, psQuote(installDir))
	_, _ = runPowerShellHidden(ps)
	time.Sleep(300 * time.Millisecond)
}

func createStartMenuShortcut(targetExe, workDir string) error {
	scPath, err := startMenuShortcutPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(scPath), 0o755); err != nil {
		return err
	}
	ps := fmt.Sprintf(`$s = (New-Object -ComObject WScript.Shell).CreateShortcut(%s); $s.TargetPath = %s; $s.WorkingDirectory = %s; $s.IconLocation = %s; $s.Description = 'KimiWork +'; $s.Save()`,
		psQuote(scPath), psQuote(targetExe), psQuote(workDir), psQuote(targetExe+",0"))
	out, err := runPowerShellHidden(ps)
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func writeUninstallRegistry(installDir, desktopPath, uninstallPath, version string) error {
	subKey, err := syscall.UTF16PtrFromString(uninstallRegSubKey)
	if err != nil {
		return err
	}
	var hKey uintptr
	r1, _, _ := procRegCreateKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(subKey)),
		0,
		0,
		0,
		keyWrite,
		0,
		uintptr(unsafe.Pointer(&hKey)),
		0,
	)
	if r1 != 0 {
		return fmt.Errorf("RegCreateKeyExW failed: %d", r1)
	}
	defer procRegCloseKey.Call(hKey)

	stringsToSet := map[string]string{
		"DisplayName":          "KimiWork +",
		"DisplayVersion":       version,
		"Publisher":            "KimiWork +",
		"InstallLocation":      installDir,
		"DisplayIcon":          desktopPath + ",0",
		"UninstallString":      fmt.Sprintf(`"%s" --uninstall`, uninstallPath),
		"QuietUninstallString": fmt.Sprintf(`"%s" --uninstall --silent`, uninstallPath),
	}
	for k, v := range stringsToSet {
		if err := regSetSZ(hKey, k, v); err != nil {
			return err
		}
	}
	if err := regSetDWord(hKey, "NoModify", 1); err != nil {
		return err
	}
	if err := regSetDWord(hKey, "NoRepair", 1); err != nil {
		return err
	}
	return nil
}

func regSetSZ(hKey uintptr, name, value string) error {
	kPtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	vSlice, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	r1, _, _ := procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(kPtr)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&vSlice[0])),
		uintptr(len(vSlice)*2),
	)
	if r1 != 0 {
		return fmt.Errorf("RegSetValueExW(%s) failed: %d", name, r1)
	}
	return nil
}

func regSetDWord(hKey uintptr, name string, value uint32) error {
	kPtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r1, _, _ := procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(kPtr)),
		0,
		regDWord,
		uintptr(unsafe.Pointer(&value)),
		4,
	)
	if r1 != 0 {
		return fmt.Errorf("RegSetValueExW(%s) failed: %d", name, r1)
	}
	return nil
}

func deleteUninstallRegistry() {
	subKey, err := syscall.UTF16PtrFromString(uninstallRegSubKey)
	if err != nil {
		return
	}
	_, _, _ = procRegDeleteTreeW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(subKey)))
}

func scheduleSelfDelete(installDir string) {
	script := fmt.Sprintf(`for ($i = 0; $i -lt 20; $i++) { Start-Sleep -Milliseconds 300; Remove-Item -LiteralPath %s -Recurse -Force -ErrorAction SilentlyContinue; if (-not (Test-Path -LiteralPath %s)) { break } }`, psQuote(installDir), psQuote(installDir))
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
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = cmd.Start()
}

func messageBox(title, body string, flags uint32) int {
	tPtr, _ := syscall.UTF16PtrFromString(title)
	bPtr, _ := syscall.UTF16PtrFromString(body)
	r1, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(bPtr)),
		uintptr(unsafe.Pointer(tPtr)),
		uintptr(flags),
	)
	return int(r1)
}

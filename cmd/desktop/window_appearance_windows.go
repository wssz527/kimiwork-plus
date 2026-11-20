//go:build windows

package main

import (
	"sync/atomic"
	"syscall"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/w32"
	"github.com/wssz527/kimiwork-plus/assets/icon"
)

const (
	winLightTitleBarBGR uint32 = 0x00e8f0f5
	winLightTitleTxtBGR uint32 = 0x00181b1d
	winDarkTitleBarBGR  uint32 = 0x00121517
	winDarkTitleTxtBGR  uint32 = 0x00e5eff5

	winImageIcon = 1
	winLRShared  = 0x00008000
)

var (
	desktopUser32DLL = syscall.NewLazyDLL("user32.dll")
	procLoadImageW   = desktopUser32DLL.NewProc("LoadImageW")
	appliedIconHWND  atomic.Uintptr
)

func platformAppIcon() []byte {
	return icon.WindowsICO
}

func configurePlatformTray(tray *application.SystemTray) {
	tray.SetIcon(icon.WindowsICO)
}

func loadWindowIcon(cx, cy int) w32.HICON {
	h, _, _ := procLoadImageW.Call(
		uintptr(w32.GetModuleHandle("")),
		uintptr(icon.AppIconResourceID),
		winImageIcon,
		uintptr(cx),
		uintptr(cy),
		winLRShared,
	)
	return w32.HICON(h)
}

func applyPlatformWindowIcon(hwnd uintptr) {
	if hwnd == 0 || appliedIconHWND.Swap(hwnd) == hwnd {
		return
	}
	small := loadWindowIcon(w32.GetSystemMetrics(w32.SM_CXSMICON), w32.GetSystemMetrics(w32.SM_CYSMICON))
	if small == 0 {
		small, _ = w32.CreateSmallHIconFromImage(icon.WindowsICO)
	}
	if small != 0 {
		w32.SendMessage(w32.HWND(hwnd), w32.WM_SETICON, w32.ICON_SMALL, uintptr(small))
	}
	big := loadWindowIcon(w32.GetSystemMetrics(w32.SM_CXICON), w32.GetSystemMetrics(w32.SM_CYICON))
	if big == 0 {
		big, _ = w32.CreateLargeHIconFromImage(icon.WindowsICO)
	}
	if big != 0 {
		w32.SendMessage(w32.HWND(hwnd), w32.WM_SETICON, w32.ICON_BIG, uintptr(big))
	}
}

func platformWindowOptions(opts application.WebviewWindowOptions) application.WebviewWindowOptions {
	opts.BackgroundColour = themeLightBG
	opts.Windows = application.WindowsWindow{
		Theme: application.SystemDefault,
	}
	return opts
}

func applyPlatformWindowTheme(window *application.WebviewWindow, mode string) {
	if window == nil {
		return
	}
	dark := false
	switch mode {
	case themeModeDark:
		dark = true
	case themeModeLight:
		dark = false
	default:
		dark = w32.IsCurrentlyDarkMode()
	}
	if dark {
		window.SetBackgroundColour(themeDarkBG)
	} else {
		window.SetBackgroundColour(themeLightBG)
	}
	hwnd := uintptr(window.NativeWindow())
	if hwnd == 0 {
		return
	}
	applyPlatformWindowIcon(hwnd)
	if !w32.SupportsThemes() {
		return
	}
	w32.SetTheme(hwnd, dark)
	if w32.SupportsCustomThemes() {
		if dark {
			w32.SetTitleBarColour(hwnd, winDarkTitleBarBGR)
			w32.SetTitleTextColour(hwnd, winDarkTitleTxtBGR)
		} else {
			w32.SetTitleBarColour(hwnd, winLightTitleBarBGR)
			w32.SetTitleTextColour(hwnd, winLightTitleTxtBGR)
		}
	}
}

func attachPlatformWindowHooks(app *application.App, window *application.WebviewWindow, ctrl *windowAppearanceController) {
	app.Event.OnApplicationEvent(events.Windows.SystemThemeChanged, func(*application.ApplicationEvent) {
		ctrl.onSystemThemeChanged()
	})
	window.OnWindowEvent(events.Windows.WebViewNavigationCompleted, func(*application.WindowEvent) {
		window.ExecJS(`document.documentElement.dataset.platform = "windows"; if (window.__kwpSyncTheme) window.__kwpSyncTheme();`)
	})
}

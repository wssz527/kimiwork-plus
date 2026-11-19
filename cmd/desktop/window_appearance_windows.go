//go:build windows

package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

const (
	winLightTitleBarBGR uint32 = 0x00e8f0f5
	winLightTitleTxtBGR uint32 = 0x00181b1d
	winDarkTitleBarBGR  uint32 = 0x00121517
	winDarkTitleTxtBGR  uint32 = 0x00e5eff5
)

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
	if hwnd == 0 || !w32.SupportsThemes() {
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

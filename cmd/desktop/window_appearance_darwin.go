//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

static void kwpSetWindowAppearance(void *winPtr, int mode) {
	if (winPtr == NULL) {
		return;
	}
	NSWindow *win = (NSWindow *)winPtr;
	if (mode == 1) {
		[win setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameAqua]];
	} else if (mode == 2) {
		[win setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameDarkAqua]];
	} else {
		[win setAppearance:nil];
	}
}

static int kwpIsSystemDarkMode(void) {
	NSAppearance *appearance = [NSApp effectiveAppearance];
	if (appearance == nil) {
		return 0;
	}
	NSAppearanceName match = [appearance bestMatchFromAppearancesWithNames:@[
		NSAppearanceNameAqua,
		NSAppearanceNameDarkAqua
	]];
	return [match isEqualToString:NSAppearanceNameDarkAqua] ? 1 : 0;
}
*/
import "C"

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func platformAppIcon() []byte {
	return nil
}

func configurePlatformTray(tray *application.SystemTray) {
	tray.SetLabel("K+")
}

func platformWindowOptions(opts application.WebviewWindowOptions) application.WebviewWindowOptions {
	opts.BackgroundColour = themeLightBG
	opts.Mac = application.MacWindow{
		TitleBar: application.MacTitleBarHiddenInset,
	}
	return opts
}

func applyPlatformWindowTheme(window *application.WebviewWindow, mode string) {
	if window == nil {
		return
	}
	native := window.NativeWindow()
	switch mode {
	case themeModeLight:
		C.kwpSetWindowAppearance(native, 1)
		window.SetBackgroundColour(themeLightBG)
	case themeModeDark:
		C.kwpSetWindowAppearance(native, 2)
		window.SetBackgroundColour(themeDarkBG)
	default:
		C.kwpSetWindowAppearance(native, 0)
		if C.kwpIsSystemDarkMode() != 0 {
			window.SetBackgroundColour(themeDarkBG)
		} else {
			window.SetBackgroundColour(themeLightBG)
		}
	}
}

func attachPlatformWindowHooks(app *application.App, window *application.WebviewWindow, ctrl *windowAppearanceController) {
	setFullscreenAttr := func(full bool) {
		val := "false"
		if full {
			val = "true"
		}
		application.InvokeAsync(func() {
			window.ExecJS(`document.documentElement.dataset.fullscreen = "` + val + `";`)
		})
	}
	window.OnWindowEvent(events.Common.WindowFullscreen, func(*application.WindowEvent) {
		setFullscreenAttr(true)
	})
	window.OnWindowEvent(events.Common.WindowUnFullscreen, func(*application.WindowEvent) {
		setFullscreenAttr(false)
	})
	window.OnWindowEvent(events.Mac.WindowWillEnterFullScreen, func(*application.WindowEvent) {
		setFullscreenAttr(true)
	})
	window.OnWindowEvent(events.Mac.WindowDidEnterFullScreen, func(*application.WindowEvent) {
		setFullscreenAttr(true)
	})
	window.OnWindowEvent(events.Mac.WindowWillExitFullScreen, func(*application.WindowEvent) {
		setFullscreenAttr(false)
	})
	window.OnWindowEvent(events.Mac.WindowDidExitFullScreen, func(*application.WindowEvent) {
		setFullscreenAttr(false)
	})
	window.OnWindowEvent(events.Mac.WebViewDidFinishNavigation, func(*application.WindowEvent) {
		full := "false"
		if window.IsFullscreen() {
			full = "true"
		}
		window.ExecJS(`document.documentElement.dataset.platform = "mac"; document.documentElement.dataset.fullscreen = "` + full + `"; if (window.__kwpSyncTheme) window.__kwpSyncTheme();`)
	})
	app.Event.OnApplicationEvent(events.Mac.ApplicationDidChangeEffectiveAppearance, func(*application.ApplicationEvent) {
		ctrl.onSystemThemeChanged()
	})
}

//go:build !darwin && !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

func platformWindowOptions(opts application.WebviewWindowOptions) application.WebviewWindowOptions {
	opts.BackgroundColour = themeLightBG
	return opts
}

func applyPlatformWindowTheme(window *application.WebviewWindow, mode string) {
	if window == nil {
		return
	}
	if mode == themeModeDark {
		window.SetBackgroundColour(themeDarkBG)
	} else {
		window.SetBackgroundColour(themeLightBG)
	}
}

func attachPlatformWindowHooks(_ *application.App, _ *application.WebviewWindow, _ *windowAppearanceController) {}

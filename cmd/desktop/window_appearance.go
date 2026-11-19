package main

import (
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	themeModeLight = "light"
	themeModeDark  = "dark"
	themeModeAuto  = "auto"
)

var (
	themeLightBG = application.NewRGB(0xf5, 0xf0, 0xe8)
	themeDarkBG  = application.NewRGB(0x17, 0x15, 0x12)
)

func normalizeThemeMode(raw string) string {
	switch raw {
	case themeModeLight:
		return themeModeLight
	case themeModeDark:
		return themeModeDark
	default:
		return themeModeAuto
	}
}

type windowAppearanceController struct {
	mu     sync.Mutex
	mode   string
	window *application.WebviewWindow
}

func attachWindowAppearance(app *application.App, window *application.WebviewWindow) *windowAppearanceController {
	ctrl := &windowAppearanceController{
		mode:   themeModeAuto,
		window: window,
	}
	ctrl.apply(themeModeAuto)

	app.Event.On("kwp:theme:light", func(*application.CustomEvent) {
		ctrl.setMode(themeModeLight)
	})
	app.Event.On("kwp:theme:dark", func(*application.CustomEvent) {
		ctrl.setMode(themeModeDark)
	})
	app.Event.On("kwp:theme:auto", func(*application.CustomEvent) {
		ctrl.setMode(themeModeAuto)
	})
	app.Event.OnApplicationEvent(events.Common.ThemeChanged, func(*application.ApplicationEvent) {
		ctrl.onSystemThemeChanged()
	})
	attachPlatformWindowHooks(app, window, ctrl)
	return ctrl
}

func (c *windowAppearanceController) setMode(raw string) {
	mode := normalizeThemeMode(raw)
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
	c.apply(mode)
}

func (c *windowAppearanceController) onSystemThemeChanged() {
	c.mu.Lock()
	mode := c.mode
	c.mu.Unlock()
	if mode == themeModeAuto {
		c.apply(themeModeAuto)
	}
}

func (c *windowAppearanceController) apply(mode string) {
	if c.window == nil {
		return
	}
	application.InvokeAsync(func() {
		applyPlatformWindowTheme(c.window, mode)
	})
}

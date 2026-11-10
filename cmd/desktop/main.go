package main

import (
	"crypto/sha256"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"kimiworkplus.local/migration/internal/bridge"
)

func main() {
	defaultDir, err := bridge.DefaultStateDir()
	if err != nil {
		log.Fatal("preview_state_unavailable")
	}
	dir := flag.String("state-dir", defaultDir, "isolated preview state directory")
	width := flag.Int("width", 1180, "window width")
	height := flag.Int("height", 820, "window height")
	flag.Parse()
	exe, err := os.Executable()
	if err != nil {
		log.Fatal("preview_executable_unavailable")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	ep, err := bridge.Ensure(*dir, filepath.Join(filepath.Dir(exe), "gateway-daemon"+suffix))
	if err != nil {
		log.Fatal("preview_daemon_unavailable")
	}
	var window *application.WebviewWindow
	app := application.New(application.Options{Name: "Kimi Work+", Description: "Go/Wails experimental preview", SingleInstance: &application.SingleInstanceOptions{UniqueID: "dev.kimi-work-plus.preview", EncryptionKey: sha256.Sum256([]byte(ep.Key)), OnSecondInstanceLaunch: func(application.SecondInstanceData) {
		if window != nil {
			window.Show()
			window.Restore()
			window.Focus()
		}
	}}, Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false}})
	window = app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Kimi Work+", Width: *width, Height: *height, MinWidth: 1024, MinHeight: 700, URL: ep.URL + "/#key=" + ep.Key})
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) { window.Hide(); e.Cancel() })
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { window.Show(); window.Restore(); window.Focus() })
	menu := app.NewMenu()
	menu.AddRole(application.AppMenu)
	menu.AddRole(application.EditMenu)
	app.Menu.Set(menu)
	tray := app.SystemTray.New()
	tray.SetLabel("K+")
	trayMenu := app.NewMenu()
	trayMenu.Add("打开 Kimi Work+").OnClick(func(*application.Context) { window.Show(); window.Focus() })
	trayMenu.AddSeparator()
	trayMenu.Add("退出窗口（网关继续运行）").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(trayMenu)
	if err = app.Run(); err != nil {
		log.Fatal("preview_window_failed")
	}
}

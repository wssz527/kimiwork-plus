package main

import (
	"crypto/sha256"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"kimiworkplus.local/migration/internal/bridge"
)

func main() {
	defaultDir, startupErr := bridge.DefaultStateDir()
	dir := flag.String("state-dir", defaultDir, "application state directory")
	width := flag.Int("width", 1180, "window width")
	height := flag.Int("height", 820, "window height")
	flag.Parse()
	var ep bridge.Endpoint
	var executable string
	if startupErr == nil {
		executable, startupErr = os.Executable()
	}
	if startupErr == nil {
		suffix := ""
		if runtime.GOOS == "windows" {
			suffix = ".exe"
		}
		ep, startupErr = bridge.Ensure(*dir, filepath.Join(filepath.Dir(executable), "gateway-daemon"+suffix))
	}

	var window *application.WebviewWindow
	var openWindow func()
	encryptionKey := sha256.Sum256([]byte("startup"))
	if startupErr == nil {
		encryptionKey = sha256.Sum256([]byte(ep.Key))
	}
	app := application.New(application.Options{Name: "Kimi Work+", Description: "Kimi Work local gateway and enhancement suite", SingleInstance: &application.SingleInstanceOptions{UniqueID: bridge.AppIdentifier, EncryptionKey: encryptionKey, OnSecondInstanceLaunch: func(application.SecondInstanceData) {
		if openWindow != nil {
			openWindow()
		}
	}}, Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false}})
	if startupErr != nil {
		app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			app.Dialog.Error().SetTitle("Kimi Work+ 无法启动").SetMessage("本机后台或窗口初始化未完成。请检查状态目录权限和网关日志后重试。\n\n诊断信息：" + startupErr.Error()).Show()
			app.Quit()
		})
		if err := app.Run(); err != nil {
			log.Printf("desktop_window_failed: %v", err)
		}
		return
	}

	window = app.Window.NewWithOptions(platformWindowOptions(application.WebviewWindowOptions{
		Title:                "Kimi Work+",
		Width:                *width,
		Height:               *height,
		MinWidth:             1024,
		MinHeight:            700,
		URL:                  ep.URL + "/#key=" + ep.Key,
		AllowSimpleEventEmit: true,
	}))
	attachWindowAppearance(app, window)
	var epMu sync.Mutex
	currentEndpoint := func() bridge.Endpoint {
		epMu.Lock()
		defer epMu.Unlock()
		return ep
	}
	syncEndpoint := func() (bridge.Endpoint, bool) {
		latest, err := bridge.ReadEndpoint(*dir)
		if err != nil {
			return bridge.Endpoint{}, false
		}
		observed := currentEndpoint()
		if latest == observed || !bridge.Healthy(latest) {
			return bridge.Endpoint{}, false
		}
		confirmed, err := bridge.ReadEndpoint(*dir)
		if err != nil || confirmed != latest {
			return bridge.Endpoint{}, false
		}
		epMu.Lock()
		defer epMu.Unlock()
		if ep != observed {
			return bridge.Endpoint{}, false
		}
		ep = confirmed
		return confirmed, true
	}
	applyEndpoint := func(next bridge.Endpoint) {
		application.InvokeAsync(func() {
			epMu.Lock()
			defer epMu.Unlock()
			if ep != next {
				return
			}
			window.SetURL(next.URL + "/#key=" + next.Key)
		})
	}
	openWindow = func() {
		if next, changed := syncEndpoint(); changed {
			applyEndpoint(next)
		}
		window.Show()
		window.Restore()
		window.Focus()
	}
	stopSync := make(chan struct{})
	defer close(stopSync)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopSync:
				return
			case <-ticker.C:
				if next, changed := syncEndpoint(); changed {
					applyEndpoint(next)
				}
			}
		}
	}()
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) { window.Hide(); e.Cancel() })
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { openWindow() })
	menu := app.NewMenu()
	menu.AddRole(application.AppMenu)
	menu.AddRole(application.EditMenu)
	app.Menu.Set(menu)
	tray := app.SystemTray.New()
	tray.SetLabel("K+")
	tray.SetTooltip("Kimi Work+")
	diagnostics := &windowDiagnostics{}
	window.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) {
		diagnostics.Failed()
		tray.SetLabel("K+ · 页面加载失败")
		tray.SetTooltip("Kimi Work+ · 页面加载失败")
	})
	trayMenu := app.NewMenu()
	trayMenu.Add("打开 Kimi Work+").OnClick(func(*application.Context) { openWindow() })
	trayMenu.Add("窗口恢复与诊断").OnClick(func(*application.Context) {
		message := "窗口已创建。如果内容没有显示，请保持 macOS 用户目录可用，并从托盘重新打开窗口。"
		if diagnostics.Status() == "web_content_process_terminated" {
			message = "系统页面进程已终止，页面脚本无法提供诊断。请从原生菜单导出窗口诊断，保持 macOS 用户目录可用后重新打开应用；不要反复刷新。"
		}
		app.Dialog.Info().SetTitle("Kimi Work+ 窗口诊断").SetMessage(message).Show()
	})
	trayMenu.Add("导出窗口诊断").OnClick(func(*application.Context) {
		path, err := diagnostics.Export(*dir)
		if err != nil {
			app.Dialog.Error().SetTitle("无法导出窗口诊断").SetMessage("请检查应用状态目录是否可写，然后重试。").Show()
			return
		}
		app.Dialog.Info().SetTitle("窗口诊断已导出").SetMessage("诊断不含面板令牌、密钥或页面内容。文件位置：\n" + path).Show()
	})
	installAccountTray(app, tray, trayMenu, *dir, func() {
		openWindow()
		window.ExecJS("document.getElementById('nav-maintenance')?.click()")
	})
	trayMenu.AddSeparator()
	trayMenu.Add("退出窗口（网关继续运行）").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(trayMenu)
	if err := app.Run(); err != nil {
		app.Dialog.Error().SetTitle("Kimi Work+ 窗口启动失败").SetMessage("系统未能创建应用窗口。请重新打开应用；若仍失败，请导出窗口诊断。\n\n诊断信息：" + err.Error()).Show()
		log.Printf("desktop_window_failed: %v", err)
	}
}

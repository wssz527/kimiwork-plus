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
	defaultDir, startupErr := bridge.DefaultStateDir()
	dir := flag.String("state-dir", defaultDir, "preview state directory")
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
	app := application.New(application.Options{Name: "Kimi Work+", Description: "Go/Wails experimental preview", SingleInstance: &application.SingleInstanceOptions{UniqueID: "dev.kimi-work-plus.preview", EncryptionKey: encryptionKey, OnSecondInstanceLaunch: func(application.SecondInstanceData) {
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
			log.Printf("preview_window_failed: %v", err)
		}
		return
	}

	window = app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Kimi Work+", Width: *width, Height: *height, MinWidth: 1024, MinHeight: 700, URL: ep.URL + "/#key=" + ep.Key})
	openWindow = func() {
		if latest, err := bridge.ReadEndpoint(*dir); err == nil && bridge.Healthy(latest) && latest != ep {
			ep = latest
			window.SetURL(ep.URL + "/#key=" + ep.Key)
		}
		window.Show()
		window.Restore()
		window.Focus()
	}
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) { window.Hide(); e.Cancel() })
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { openWindow() })
	menu := app.NewMenu()
	menu.AddRole(application.AppMenu)
	menu.AddRole(application.EditMenu)
	app.Menu.Set(menu)
	tray := app.SystemTray.New()
	tray.SetLabel("K+")
	diagnostics := &windowDiagnostics{}
	window.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) {
		diagnostics.Failed()
		tray.SetLabel("K+ · 页面加载失败")
	})
	trayMenu := app.NewMenu()
	trayMenu.Add("打开 Kimi Work+").OnClick(func(*application.Context) { openWindow() })
	trayMenu.Add("窗口恢复与诊断").OnClick(func(*application.Context) {
		message := "窗口已创建。如果内容没有显示，请保持 macOS 用户目录可用，并从托盘重新打开窗口。"
		if diagnostics.Status() == "web_content_process_terminated" {
			message = "系统页面进程已终止，页面脚本无法提供诊断。请从原生菜单导出窗口诊断，保持 macOS 用户目录可用后重新打开预览；不要反复刷新。"
		}
		app.Dialog.Info().SetTitle("Kimi Work+ 窗口诊断").SetMessage(message).Show()
	})
	trayMenu.Add("导出窗口诊断").OnClick(func(*application.Context) {
		path, err := diagnostics.Export(*dir)
		if err != nil {
			app.Dialog.Error().SetTitle("无法导出窗口诊断").SetMessage("请检查预览状态目录是否可写，然后重试。").Show()
			return
		}
		app.Dialog.Info().SetTitle("窗口诊断已导出").SetMessage("诊断不含面板令牌、密钥或页面内容。文件位置：\n" + path).Show()
	})
	trayMenu.AddSeparator()
	trayMenu.Add("退出窗口（网关继续运行）").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(trayMenu)
	if err := app.Run(); err != nil {
		app.Dialog.Error().SetTitle("Kimi Work+ 窗口启动失败").SetMessage("系统未能创建应用窗口。请重新打开应用；若仍失败，请导出窗口诊断。\n\n诊断信息：" + err.Error()).Show()
		log.Printf("preview_window_failed: %v", err)
	}
}

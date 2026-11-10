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
	diagnostics := &windowDiagnostics{}
	window.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) {
		diagnostics.Failed()
		tray.SetLabel("K+ · 页面加载失败")
	})
	trayMenu := app.NewMenu()
	trayMenu.Add("打开 Kimi Work+").OnClick(func(*application.Context) { window.Show(); window.Focus() })
	trayMenu.Add("窗口恢复与诊断").OnClick(func(*application.Context) {
		message := "窗口已创建。如果内容没有显示，请保持 macOS 用户目录可用，并重新打开独立预览应用。预览业务数据仍保存在独立状态目录。"
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
	if err = app.Run(); err != nil {
		log.Fatal("preview_window_failed")
	}
}

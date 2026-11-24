package main

import (
	"crypto/sha256"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wssz527/kimiwork-plus/internal/bridge"
)

const openAccountsJS = `(() => {
  const open = () => {
    if (typeof window.__kwpOpenAccounts === 'function') {
      window.__kwpOpenAccounts();
      return;
    }
    document.getElementById('nav-maintenance')?.click();
    document.getElementById('settings-tab-accounts')?.click();
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', open, { once: true });
  } else {
    open();
  }
})();`

const openConversationsJS = `(() => {
  const open = () => {
    if (typeof window.__kwpOpenConversations === 'function') {
      window.__kwpOpenConversations();
      return;
    }
    document.getElementById('nav-conversations')?.click();
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', open, { once: true });
  } else {
    open();
  }
})();`

const openUpdatesJS = `(() => {
  const open = () => {
    if (typeof window.__kwpOpenUpdates === 'function') {
      window.__kwpOpenUpdates();
      return;
    }
    document.getElementById('nav-maintenance')?.click();
    document.getElementById('settings-tab-connection')?.click();
    const panel = document.getElementById('update-panel');
    if (panel) {
      panel.scrollIntoView({ block: 'start' });
      document.getElementById('upd-check')?.focus({ preventScroll: true });
    }
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', open, { once: true });
  } else {
    open();
  }
})();`

var windowNavScripts = map[string]string{
	"accounts":      openAccountsJS,
	"conversations": openConversationsJS,
	"updates":       openUpdatesJS,
}

func singleInstanceEncryptionKey(stateDir string) [32]byte {
	clean := filepath.Clean(strings.TrimSpace(stateDir))
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return sha256.Sum256([]byte(bridge.AppIdentifier + "\x00single-instance\x00" + clean))
}

type windowNavController struct {
	mu               sync.Mutex
	waitForNavFinish bool
	navLoaded        bool
	loadedOriginURL  string
	loadedKey        string
	pendingNavTarget string
	setURL           func(string)
	execJS           func(string)
}

func newWindowNavController(waitForNavFinish bool, initialOriginURL, initialKey string, setURL, execJS func(string)) *windowNavController {
	return &windowNavController{
		waitForNavFinish: waitForNavFinish,
		loadedOriginURL:  initialOriginURL,
		loadedKey:        initialKey,
		setURL:           setURL,
		execJS:           execJS,
	}
}

func (c *windowNavController) OnNavigationFinished() {
	c.mu.Lock()
	c.navLoaded = true
	target := c.pendingNavTarget
	c.pendingNavTarget = ""
	execJS := c.execJS
	c.mu.Unlock()
	if target != "" && execJS != nil {
		execJS(windowNavScripts[target])
	}
}

func (c *windowNavController) prepareApplyEndpoint(next bridge.Endpoint) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	crossOrigin := next.URL != c.loadedOriginURL
	keyChanged := next.Key != c.loadedKey
	if crossOrigin {
		c.navLoaded = false
		c.loadedOriginURL = next.URL
		c.loadedKey = next.Key
		return next.URL + "/#key=" + next.Key
	}
	if keyChanged {
		c.loadedKey = next.Key
		return next.URL + "/#key=" + next.Key
	}
	return ""
}

func (c *windowNavController) ApplyEndpoint(next bridge.Endpoint) {
	if targetURL := c.prepareApplyEndpoint(next); targetURL != "" && c.setURL != nil {
		c.setURL(targetURL)
	}
}

func (c *windowNavController) RequestNavigate(target string) {
	if _, ok := windowNavScripts[target]; !ok {
		return
	}
	c.mu.Lock()
	if c.waitForNavFinish && !c.navLoaded {
		c.pendingNavTarget = target
		c.mu.Unlock()
		return
	}
	c.pendingNavTarget = ""
	execJS := c.execJS
	c.mu.Unlock()
	if execJS != nil {
		execJS(windowNavScripts[target])
	}
}

func (c *windowNavController) RequestOpenAccounts() {
	c.RequestNavigate("accounts")
}

func (c *windowNavController) RequestOpenConversations() {
	c.RequestNavigate("conversations")
}

func (c *windowNavController) RequestOpenUpdates() {
	c.RequestNavigate("updates")
}

func windowDiagnosticMessage(goos, status string) string {
	terminated := status == "web_content_process_terminated"
	switch goos {
	case "windows":
		if terminated {
			return "系统页面进程已终止，页面脚本无法提供诊断。请从托盘菜单导出窗口诊断，确认当前 Windows 用户目录和 WebView2 运行时可用后重新打开应用；不要反复刷新。"
		}
		return "窗口已创建。如果内容没有显示，请确认当前 Windows 用户目录和 WebView2 运行时可用，并从托盘重新打开窗口。"
	case "darwin":
		if terminated {
			return "系统页面进程已终止，页面脚本无法提供诊断。请从原生菜单导出窗口诊断，保持 macOS 用户目录可用后重新打开应用；不要反复刷新。"
		}
		return "窗口已创建。如果内容没有显示，请保持 macOS 用户目录可用，并从托盘重新打开窗口。"
	default:
		if terminated {
			return "系统页面进程已终止，页面脚本无法提供诊断。请从托盘菜单导出窗口诊断，确认当前用户目录可用后重新打开应用；不要反复刷新。"
		}
		return "窗口已创建。如果内容没有显示，请确认当前用户目录可用，并从托盘重新打开窗口。"
	}
}

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
	encryptionKey := singleInstanceEncryptionKey(*dir)
	app := application.New(application.Options{Name: "KimiWork +", Description: "Kimi Work local gateway and enhancement suite", Icon: platformAppIcon(), SingleInstance: &application.SingleInstanceOptions{UniqueID: bridge.AppIdentifier, EncryptionKey: encryptionKey, OnSecondInstanceLaunch: func(application.SecondInstanceData) {
		if openWindow != nil {
			openWindow()
		}
	}}, Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false}})
	if startupErr != nil {
		app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			app.Dialog.Error().SetTitle("KimiWork + 无法启动").SetMessage("本机后台或窗口初始化未完成。请检查状态目录权限和网关日志后重试。\n\n诊断信息：" + startupErr.Error()).Show()
			app.Quit()
		})
		if err := app.Run(); err != nil {
			log.Printf("desktop_window_failed: %v", err)
		}
		return
	}

	window = app.Window.NewWithOptions(platformWindowOptions(application.WebviewWindowOptions{
		Title:                "KimiWork +",
		Width:                *width,
		Height:               *height,
		MinWidth:             1024,
		MinHeight:            700,
		URL:                  ep.URL + "/#key=" + ep.Key,
		AllowSimpleEventEmit: true,
	}))
	nav := newWindowNavController(
		runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		ep.URL,
		ep.Key,
		func(target string) { window.SetURL(target) },
		func(js string) { window.ExecJS(js) },
	)
	attachWindowAppearance(app, window, nav.OnNavigationFinished)
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
			if ep != next {
				epMu.Unlock()
				return
			}
			targetURL := nav.prepareApplyEndpoint(next)
			epMu.Unlock()
			if targetURL != "" {
				window.SetURL(targetURL)
			}
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
	configurePlatformTray(tray)
	tray.SetTooltip("KimiWork +")
	diagnostics := &windowDiagnostics{}
	window.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) {
		diagnostics.Failed()
		tray.SetLabel("K+ · 页面加载失败")
		tray.SetTooltip("KimiWork + · 页面加载失败")
	})
	installTray(app, tray, *dir, diagnostics, openWindow, nav)
	if err := app.Run(); err != nil {
		app.Dialog.Error().SetTitle("KimiWork + 窗口启动失败").SetMessage("系统未能创建应用窗口。请重新打开应用；若仍失败，请导出窗口诊断。\n\n诊断信息：" + err.Error()).Show()
		log.Printf("desktop_window_failed: %v", err)
	}
}

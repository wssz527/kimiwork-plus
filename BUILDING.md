# Go/Wails 实验实现

本目录是 Kimi Work+ 0.12.1 的独立 Go/Wails 迁移源码。它可以构建和运行，但尚未通过完整原生功能等价验收。稳定安装继续保留。

## 工具链

- Go 1.26.10（go.mod 的 toolchain 锁定）。
- Wails v3.1.0 稳定版，由 go.mod/go.sum 锁定，不需要用全局 Wails CLI 构建。
- macOS arm64：Xcode Command Line Tools、系统 WebKit、codesign。
- Windows amd64：运行时使用系统 WebView2。本轮在 macOS 交叉构建，无 Windows 真机记录。

```sh
go test ./...
go test -race ./...
go vet ./...
bash build/macos.sh
```

可用 `GO=/absolute/path/to/go OUTPUT_DIR=/absolute/path/to/output bash build/macos.sh` 指定本地工具链与输出。脚本不安装、不启动、不覆盖稳定版。当前签名为 ad-hoc，仅适合开发实验，没有公证。

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -H windowsgui' -o out/KimiWorkPlus.exe ./cmd/desktop
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o out/gateway-daemon.exe ./cmd/gateway-daemon
```

`KimiWorkPlus` 与 `gateway-daemon` 必须放在同一目录。Mac 构建脚本会生成正确 app 结构。独立 Gateway：

```sh
./gateway-daemon --state-dir /absolute/private/preview-state --port 0
```

只允许 loopback。自动生成的面板与模型令牌是私有运行数据，不提交或转发。桌面 `--state-dir` 必须指向同一实验目录，不能指向 0.12.1 生产数据目录。

后台地址来自私有 `desktop-endpoint.json`。首次启动可用 `--port 0` 选择空闲端口；正常停止、重开与同构建重启默认沿用已保存的 loopback 地址和面板令牌，避免已导入的官方地址失效。健康读回同时核对 PID、版本和 buildId。`--stop-daemon` 使用鉴权接口排空本服务请求，不会把另一个端口响应者当作本服务停止成功。

维护页可以显式注册、启停、允许登录启动和卸载预览自己的用户级服务。注册不等于允许登录启动，关闭窗口也不等于服务恢复。默认不注册开机启动。连接、翻译和 Tavily 密钥写入系统 Keychain/DPAPI，配置只保存新引用；写入后从系统存储读回再提交，不解密旧版 safeStorage。ad-hoc 更新导致 ACL 拒绝时，需要在界面重新输入密钥。

`build/install-preview.sh` 只安装到当前用户的 `Applications/Kimi Work+ Preview.app`。升级前验证新包并正常停止旧预览，保留 `Kimi Work+ Preview.previous.app`；停止或校验失败不会替换旧预览。稳定版不在脚本目标中。

Windows 使用 `build/windows.ps1 -BuildId <build-id>` 或以下等价命令生成同身份 PE 文件：

```powershell
$flags = "-s -w -X kimiworkplus.local/migration/internal/bridge.BuildID=<build-id>"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$flags -H windowsgui" -o out/KimiWorkPlus.exe ./cmd/desktop
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags $flags -o out/gateway-daemon.exe ./cmd/gateway-daemon
```

私有 CDP 在 Windows 上通过继承的两条管道 HANDLE 传递，不使用 `ExtraFiles`，也不回退到开放 TCP 调试端口。交叉构建只证明编译成功，不证明官方 CDP、DPAPI 或 WebView2 真机行为。

## 测试和边界

协议、账本、额度、Tavily 测试包含旧实现的冻结观察值，不需要 Node/Bun 即可运行这些对照测试。`DUAL_RUN_REGEN=1` 是开发者重新执行旧 JS 的可选模式，需要原 0.12.1 源码和 Bun；独立回传源码没有捆绑完整旧仓库，普通测试不依赖它。

`internal/kimi/INTERFACE.md` 记录原生契约与未支持能力。Go 写入配置文件、CDP fixture 或 JS 解析成功都不代表官方登录会话已经采用。MCP 原生测试、remote companion/updater、旧 safeStorage 解密、配置重协调的官方会话采用和生产账号导入仍有缺口，详见回传根目录 FEATURE_MATRIX.csv。

前端保留现有 HTML/CSS/JavaScript，没有为迁移重新设计 UI。翻译注入脚本仍在官方 Kimi 的渲染上下文运行。新版包不携带 Node、Electron 或 Chromium。

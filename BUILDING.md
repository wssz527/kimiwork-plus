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

## 测试和边界

协议、账本、额度、Tavily 测试包含旧实现的冻结观察值，不需要 Node/Bun 即可运行这些对照测试。`DUAL_RUN_REGEN=1` 是开发者重新执行旧 JS 的可选模式，需要原 0.12.1 源码和 Bun；独立回传源码没有捆绑完整旧仓库，普通测试不依赖它。

`internal/kimi/INTERFACE.md` 记录原生契约与未支持能力。Go 写入配置文件、CDP fixture 或 JS 解析成功都不代表官方登录会话已经采用。MCP 原生测试、remote companion/updater、旧 safeStorage 解密、配置重协调的官方会话采用和生产账号导入仍有缺口，详见回传根目录 FEATURE_MATRIX.csv。

前端保留现有 HTML/CSS/JavaScript，没有为迁移重新设计 UI。翻译注入脚本仍在官方 Kimi 的渲染上下文运行。新版包不携带 Node、Electron 或 Chromium。

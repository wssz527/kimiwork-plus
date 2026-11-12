# Go/Wails 实验实现

本目录是 Kimi Work+ 当前唯一开发的 Go/Wails 增强套件源码，从 0.12.1 迁移而来。Node 网关与 Magpie 专用插件停止开发，保留历史候选。当前构建仍是独立预览，尚未通过完整官方采用验收，稳定安装继续保留。

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

### 生命周期操作由独立执行者完成

`platform.Controller` 是唯一的生命周期入口。每个动作生成一个 `operationId`，写入 `service-operations/` 下的私有日志，并返回可轮询的记录；调用方不能把「命令已启动」当作「操作已完成」。

- `register-service` 与登录启动开关（`enable-login-service` / `disable-login-service`）是内联操作：只重写 plist 或计划任务，不 bootout、不 kickstart、不 `/End`。关闭登录启动不会中断正在进行的模型请求。
- `start-service`、`restart-service`、`stop-service`、`uninstall-service` 交给短命 helper 进程执行。helper 用 `setsid`（macOS）或 `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`（Windows）脱离调用者，因此不会被它要替换的 daemon 带走；它执行完后退出，不留下长期 supervisor。
- 停止优先走已有鉴权 HTTP `/api/daemon/stop`，并核对 `desktop-endpoint.json` 里的 PID 与 buildId。只有在调用方显式要求 `Force` 且排空失败时才回退到平台终止；此时记录 `forced=true` 并注明机制（`launchd_bootout` / `schtasks_end`），不会把 `/End` 描述为优雅排空。
- 结果以平台读回为准：`enabled`、`registered`、`running` 与 `buildId` 都要和请求一致，否则操作标为失败。start/restart 还要求私有 endpoint 与实际健康响应的 PID/buildId 符合预期，`ServingBuildID` 只取自该健康响应。健康状态不可确认不等于已排空，非 Force 操作不会继续破坏性步骤。同一时间只允许一个破坏性操作，锁记录在 `service-operations/operator.lock`，超过 deadline 的锁可被接管。

宿主进程必须在解析自己的命令行参数之前处理 helper 调用，否则 helper 分支永远不会执行：

```go
if code, handled := platform.RunHelperFromArgs(os.Args); handled {
    os.Exit(code)
}
```

`bridge` 侧建议用 `platform.OpenController(platform.ControllerOptions{ManagerOptions: ..., HelperPath: daemonExecutable})`，`Begin` 后对 `PhasePending`/`PhaseRunning` 立即返回 `202` 与 `operationId`，由 `Read`/`Active` 提供状态；不要在同一次 HTTP 响应里等待破坏性操作完成。

本机 Security SDK 提供 Keychain 的函数声明、CoreFoundation 回调和引用所有权；普通后台读写不显示隐藏授权，显式界面保存可通过 `VisibleSaveStore` 请求一次系统交互。缺失、锁定、取消、拒绝、会话不可用与读回失败分别返回可诊断状态。系统实测与最终 GUI 的完成级别以回传证据为准，不用单元测试替代。

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

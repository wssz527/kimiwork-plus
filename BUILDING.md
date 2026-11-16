# Go/Wails 实验实现

本目录是 Kimi Work+ 当前唯一开发的 Go/Wails 增强套件源码，从 0.12.1 迁移而来。Node 网关与 Magpie 专用插件停止开发，保留历史候选。当前构建是独立预览，稳定安装继续保留。阶段 1 基线 `00c62b8` 已完成核心模型链路、子代理与 MCP 真实验收；阶段 2–6 后续实现仍在集成验收，双账号与 Windows 真机尚未完成。

## 工具链

- Go 1.26.10（go.mod 的 toolchain 锁定）。
- Wails v3.1.0 稳定版，由 go.mod/go.sum 锁定，不需要用全局 Wails CLI 构建。
- macOS arm64：Xcode Command Line Tools、系统 WebKit、codesign。
- Windows amd64：运行时使用系统 WebView2。本轮在 macOS 交叉构建，无 Windows 真机记录。

```sh
go test -count=1 ./...
go test -race -count=1 ./...
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

维护页可以显式注册、启停、允许登录启动和卸载预览自己的用户级服务。注册不等于允许登录启动，关闭窗口也不等于服务恢复。默认不注册开机启动。连接、翻译和 Tavily 密钥写入系统 Keychain/DPAPI，配置只保存新引用；写入后从系统存储读回再提交，不解密旧版 safeStorage。ad-hoc 更新导致 ACL 拒绝时，连接可通过界面的“重新授权并应用连接”读取原有凭据，不显示或改写 Key；凭据确实缺失时仍需要重新保存。

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

## 阶段 2–6 集成候选

导航为总览、模型、用量、Work 能力、扩展、设置。服务商与模型共用一次保存事务；保存本机目录与同步到 Work 是不同操作，新增/删除模型后按同步状态重新导入，必要时正常重启 Kimi。

- 用量页包含真实请求、Kimi Code 三来源、Magpie 余额和预算。Work 来源随当前官方凭据重新读取。本机 Kimi Code 登录只有显式采用后才使用，手填 Key 保存在本产品的系统凭据存储中。读取失败保留错误和过期标记，不能解释为额度为 0。
- 账号库保存可恢复会话到系统钥匙串，窗口顶部、菜单栏和设置共用后台状态。先捕获当前最新凭据，目标 Cookie 和 localStorage 必须完整写入，才会更新内核配置并正常重启官方程序；写入失败不会被重启后的身份读回掩盖。页面与内核配置身份不一致时不报告成功，失败尝试恢复原账号。任务在跑或忙状态未知时拒绝切换。当前没有已验收的免重启切换路径。
- Swarm 通过当前官方会话的 `app.swarmMode.get/set` 接口控制，不直接修改会话文件。当前内核的 Agent/AgentSwarm 共享 `secondary_model`；没有独立 Swarm 模型池。Tower 在当前官方桌面版本没有入口，显示不支持，不通过注入伪造入口。
- 预算依赖模型定价和本机账本估算，按所选时区计算日/月提醒或拒绝。删除模型定价不清除已经记录的同币种历史费用，不同币种不相加。缺少定价的记录明确列为未计价，供应商的真实账单与硬限额不由它保证。可选串行模式减少在途请求超额窗口，但会降低吞吐。
- 备用连接按配置顺序尝试。只在拨号失败或指定限流/服务错误状态且尚未转发响应时切换，不在流开始后重发。账本归属实际回答的连接。主凭据不可读属于配置错误，不静默使用备用连接掩盖。

候选构建使用单独输出目录并设置 `BUILD_ID`。验收时核对桌面、daemon 的 buildinfo 与 `/api/health` 的 buildId/PID，未提交工作区不能冒充干净提交构建。更换 ad-hoc 签名可能使旧钥匙串引用不可读。显式应用连接可通过系统可见授权读取原引用，全部连接可读后才应用配置；拒绝或取消保留原配置和重试入口。账号切换/取消也可通过系统可见授权读取已有快照，后台读取仍禁止弹窗。等待系统授权时，缓存凭据保持可读，未缓存的后台读取立即返回需授权状态，不阻塞面板轮询。取消授权不会缓存失败结果，可重新尝试。这不等于真实账号登录凭据已失效。

当前已有三来源额度真实查询、预算提醒/拒绝与真实备用模型账本证据，以及预算界面保存/清除。2026-11-17 的连接授权候选已在官方界面选择 DeepSeek、GLM 并取得真实回复；Swarm 启用后两个并行助手完成任务，当前网关账本记录对应请求，结束后开关恢复关闭。两真实账号五次往返、部分界面输入边界和 Windows 真机仍未完成。未提交候选的证据也不能代替最终提交绑定包的验收。

## 测试和边界

协议、账本、额度、Tavily 测试包含旧实现的冻结观察值，不需要 Node/Bun 即可运行这些对照测试。`DUAL_RUN_REGEN=1` 是开发者重新执行旧 JS 的可选模式，需要原 0.12.1 源码和 Bun；独立回传源码没有捆绑完整旧仓库，普通测试不依赖它。

`internal/kimi/INTERFACE.md` 记录原生契约、当前默认 profile 与未支持能力。阶段 1 已在真实官方账号和 Work 数据上验收模型选择、DeepSeek/GLM 回复、固定/继承子代理及 MCP 工具往返；账本记录真实 Token、耗时和 `conversationId`。真实请求没有可靠的主对话/子代理角色字段，不能据此声称账本已区分调用方。MCP 面板内的连接探测仍需要 kernel runtime；remote companion/updater、旧 safeStorage 解密、Windows 真机和生产级共存仍有缺口。功能分级以回传根目录 `FEATURE_MATRIX.md` 为准。

前端保留现有 HTML/CSS/JavaScript，没有为迁移重新设计 UI。翻译注入脚本仍在官方 Kimi 的渲染上下文运行。新版包不携带 Node、Electron 或 Chromium。

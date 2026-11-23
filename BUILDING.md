# KimiWork + 构建与开发说明（Go / Wails）

本目录是 KimiWork +（Go + Wails v3）的完整源码。产品通过官方配置文件、官方页面接口、受管 CDP 管道与本地回环网关接入官方 Kimi 桌面版 Work，不修改官方程序包体，也不代理官方更新。对外使用说明见 [README.md](README.md)，许可与第三方声明见 [LICENSE](LICENSE) 与 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)。

## 工具链与打包

- Go 1.26.10（由 `go.mod` 的 `toolchain` 锁定，模块路径 `github.com/wssz527/kimiwork-plus`）。
- Wails v3.1.0 稳定版（由 `go.mod` / `go.sum` 锁定，无需安装全局 Wails CLI）。
- macOS（Universal：arm64 + amd64）：Xcode Command Line Tools、系统 WebKit、`codesign`、`lipo`、`hdiutil`。
- Windows（amd64）：纯 Go 交叉编译（`CGO_ENABLED=0`），运行时使用系统 WebView2。

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
node --test frontend/app.test.mjs frontend/plus.test.mjs frontend/usage.test.mjs

# 构建 macOS Universal .app + .zip + .dmg（默认 ad-hoc；发布使用固定自签名证书 CODESIGN_IDENTITY）
VERSION=1.1.5 BUILD_ID=1.1.5-local bash build/macos-dmg.sh

# 交叉构建 Windows amd64 二进制、便携 .zip 与用户级 setup.exe 安装程序
VERSION=1.1.5 BUILD_ID=1.1.5-local bash build/windows.sh

# 一键构建、签名双端发布包并生成跨源相对路径 update-manifest.json
VERSION=1.1.5 BUILD_ID=1.1.5-local NOTES_FILE=/path/to/notes.txt bash scripts/package_release.sh

# 同步发布包到国内 CNB 镜像
python3 scripts/sync_cnb_release.py --tag v1.1.5 --notes-file /path/to/notes.txt /path/to/release-assets/*
```

可用 `GO=/absolute/path/to/go OUTPUT_DIR=/absolute/path/to/output` 指定本地工具链与独立输出目录，并用 `CODESIGN_IDENTITY="<证书名或 SHA-1>"`、`UPDATE_PUBLIC_KEY="<hex>"`、`UPDATE_GITHUB_REPO="<owner/repo>"` 绑定发布证书与自动更新源。固定自签名证书可用 `build/create-signing-cert.sh` 在仓库外生成；自动更新清单位与 Ed25519 签名由 `cmd/signrelease` 生成（签名私钥必须保存在仓库之外）。脚本不安装、不启动、不覆盖本机既有安装，并会自动把根目录的 `LICENSE` 与 `THIRD_PARTY_NOTICES.txt` 打入应用包、DMG 与 Windows 安装器。

`KimiWorkPlus` 与 `gateway-daemon` 必须位于同一目录。独立启动网关（隔离状态目录）：

```sh
./gateway-daemon --state-dir /absolute/private/dev-state --port 0
```

只允许 loopback。自动生成的面板与模型令牌是私有运行数据，不提交或转发。桌面 `--state-dir` 必须指向同一实验目录，不能指向 0.12.1 生产数据目录。

后台地址来自私有 `desktop-endpoint.json`。首次启动可用 `--port 0` 选择空闲端口；正常停止、重开与同构建重启默认沿用已保存的 loopback 地址和面板令牌，避免已导入的官方地址失效。健康读回同时核对 PID、版本和 buildId。`--stop-daemon` 使用鉴权接口排空本服务请求，不会把另一个端口响应者当作本服务停止成功。

守护进程日志默认写在状态目录下的 `launchd-daemon.log`；没有状态目录时落到 `~/Library/Logs/KimiWorkPlus/`。旧版本把日志放在 `~/Library/LaunchAgents` 与 plist 并列，卸载时会一并删除那些遗留文件。排障时先看状态目录里的日志与 `service-operations/<operationId>.helper.log`，不要假设 `LaunchAgents` 里还有当前日志。

维护页可以显式注册、启停、允许登录启动和卸载本产品注册的用户级服务。注册不等于允许登录启动，关闭窗口也不等于服务恢复。默认不注册开机启动。macOS 的连接、翻译、Tavily 与账号凭据使用应用目录内的 AES-GCM 加密文件，Windows 使用 DPAPI；配置只保存引用，写入后读回再提交，不解密旧版 safeStorage。v1.1.2 之前的 macOS 钥匙串条目会在本地缺失时迁移，新值写入并读回一致后才清理旧条目；只有旧条目需要系统确认时，显式重授权操作才可能显示一次系统提示。

### 生命周期操作由独立执行者完成

`platform.Controller` 是唯一的生命周期入口。每个动作生成一个 `operationId`，写入 `service-operations/` 下的私有日志，并返回可轮询的记录；调用方不能把「命令已启动」当作「操作已完成」。

- `register-service` 与登录启动开关（`enable-login-service` / `disable-login-service`）是内联操作：只重写 plist 或计划任务，不 bootout、不 kickstart、不 `/End`。关闭登录启动不会中断正在进行的模型请求。
- `start-service`、`restart-service`、`stop-service`、`uninstall-service` 交给短命 helper 进程执行。helper 用 `setsid`（macOS）或 `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`（Windows）脱离调用者，因此不会被它要替换的 daemon 带走；它执行完后退出，不留下长期 supervisor。
- 停止优先走已有鉴权 HTTP `/api/daemon/stop`，并核对 `desktop-endpoint.json` 里的 PID 与 buildId。只有在调用方显式要求 `Force` 且排空失败时才回退到平台终止；此时记录 `forced=true` 并注明机制（`launchd_bootout` / `daemon_terminate`），不会把强制终止描述为优雅排空。Windows 的 `daemon_terminate` 由 `gateway-daemon --terminate-daemon --state-dir <dir>` 执行，先校验记录里的 PID 仍在运行且可执行文件路径就是自己，绝不触碰官方 Kimi。
- Windows 的登录自启用免提权的 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，不用 `schtasks`：中等完整性进程创建 `ONLOGON` 任务会被系统以 `ERROR_ACCESS_DENIED` 拒绝，而本产品不能依赖手工提权。`gateway-daemon.exe` 以 `-H windowsgui` 构建，从 `Run` 键直接拉起时不出现控制台窗口；`--start-background` 负责去重与脱离子进程，`--terminate-daemon` 负责受控终止。
- 结果以平台读回为准：`enabled`、`registered`、`running` 与 `buildId` 都要和请求一致，否则操作标为失败。start/restart 还要求私有 endpoint 与实际健康响应的 PID/buildId 符合预期，`ServingBuildID` 只取自该健康响应。健康状态不可确认不等于已排空，非 Force 操作不会继续破坏性步骤。同一时间只允许一个破坏性操作，锁记录在 `service-operations/operator.lock`，超过 deadline 的锁可被接管。

宿主进程必须在解析自己的命令行参数之前处理 helper 调用，否则 helper 分支永远不会执行：

```go
if code, handled := platform.RunHelperFromArgs(os.Args); handled {
    os.Exit(code)
}
```

`bridge` 侧建议用 `platform.OpenController(platform.ControllerOptions{ManagerOptions: ..., HelperPath: daemonExecutable})`，`Begin` 后对 `PhasePending`/`PhaseRunning` 立即返回 `202` 与 `operationId`，由 `Read`/`Active` 提供状态；不要在同一次 HTTP 响应里等待破坏性操作完成。

本机 Security SDK 仅用于读取和清理旧版本 Keychain 条目；普通后台迁移不显示隐藏授权，显式重授权可通过 `VisibleSaveStore` 请求一次系统交互。新凭据使用应用内 AES-GCM 存储，不经过该 SDK。缺失、锁定、取消、拒绝、会话不可用与读回失败分别返回可诊断状态。系统实测与最终 GUI 的完成级别以回传证据为准，不用单元测试替代。

`build/install-preview.sh` 只安装到当前用户的 `Applications/Kimi Work+ Preview.app`。升级前验证新包并正常停止旧预览，保留 `Kimi Work+ Preview.previous.app`；停止或校验失败不会替换旧预览。稳定版不在脚本目标中。

Windows 使用 `build/windows.ps1 -BuildId <build-id>` 或以下等价命令生成同身份 PE 文件：

```powershell
$flags = "-s -w -X github.com/wssz527/kimiwork-plus/internal/bridge.BuildID=<build-id>"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$flags -H windowsgui" -o out/KimiWorkPlus.exe ./cmd/desktop
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$flags -H windowsgui" -o out/gateway-daemon.exe ./cmd/gateway-daemon
```

两个可执行文件都以 `-H windowsgui` 构建。`gateway-daemon.exe` 也需要它：登录自启由 `HKCU\...\Run` 直接拉起进程，没有 GUI 子系统标志时会附带一个隐藏的 `conhost.exe`，并在某些路径上闪出控制台窗口。`windowsgui` 只改变控制台分配，不影响标准输入输出重定向——helper 契约和 `kimi.RunContractWorker(os.Stdin, os.Stdout)` 的管道读写照常工作，日志仍可通过 `--log-path` 落到文件。

私有 CDP 在 Windows 上通过继承的两条管道 HANDLE 传递，不使用 `ExtraFiles`，也不回退到开放 TCP 调试端口。交叉构建只证明编译成功，不证明官方 CDP、DPAPI 或 WebView2 真机行为。

### 零网关直连模式

接入模式保存在私有配置的 `integrationMode` 中，缺失或非法值一律按 `gateway` 处理，因此旧配置和手改文件不会意外进入直连数据路径。`bridge.buildDirectSpec` 只在直连下产出非密钥目录（服务商、协议、地址、模型）。

- 直连导入先准备静态解析：macOS 走 `byokdns.Setup{RequireStaticHosts: true}`，把 `dev.msh.team` 的带标记条目写进 `/etc/hosts`，并在同一次提权步骤里删除旧版本安装的 `/etc/resolver/dev.msh.team`；Windows 只写带标记的 hosts 对。两种平台都保留文件原有 BOM、行尾和无关条目，遇到同域名的其他写法报 `resolver_conflict` 而不是覆盖。
- 提权通过 `/usr/bin/osascript ... with administrator privileges`（macOS）或 PowerShell `Start-Process -Verb RunAs`（Windows）完成。macOS 在已有本产品条目且解析探测通过时不弹授权；`RequireStaticHosts` 只决定 macOS 是否愿意复用旧版本安装的 `/etc/resolver` 条目，网关模式仍可复用它，直连模式必须换成 hosts 条目。Windows 的 `prepare` 每次都经提权脚本写入，不做无提权的复用判断。
- 导入走 `clawBridgeAPI.byokSources` 的 `list` / `preview` / `save` / `applyPending` / `discard` / `disconnect`。页面答案只是候选，`gradeDirectImport` 仍以官方 store 与生成的 `config.json` 读回为准确认 `applied`。
- 直连判定 `ready` 同时要求模式为直连、官方 store 与生成配置校验通过、以及静态 hosts 就绪（Windows 还要一次通过的解析探测）。只缺 hosts 时是「待补齐静态 hosts」，不是已就绪。
- 双向切换都先预览、保存、应用并读回新来源，再断开旧来源。预览或保存失败不触碰旧路由；若官方后台目录发现与首次直连清理竞争，且没有其他来源待确认，同一次操作会按精确手工草稿自动收尾一次。未请求的目录 pending 不阻塞读回，手工模型 pending 仍会阻塞；剩余清理失败用 `routeCleanupPending` / `route_cleanup_pending` 报告，可通过再次同步重试。两个方向的导入各自拒绝错误的模式（`native_import_direct_mode` / `native_import_gateway_mode`）。
- 排障：`/api/state` 的 `directMode` 给出 `supported`、`staticHostsReady`、`routeCleanupPending`、`eligibleModelIds`、`eligibleModels`、`skippedModels` 与 `activeFeaturesBypassed`。跳过原因是固定的代码值（缺连接、地址不合规、协议不支持、未声明工具调用、上下文长度无效、端点重复、缺密钥），不要按文案反推。macOS 可用 `grep kimi-work-gateway /etc/hosts` 确认条目是否在位；`{"action":"remove-dns"}` 会移除本产品写入的那一条并停止 DNS 监听。

以上是代码路径与实际写入规则。直连模式的真机验收结论不写在本文档里，以对应版本的交接记录和发布说明为准。

## 阶段 2–6 集成候选

导航为总览、模型、用量、Work 能力、扩展、设置。服务商与模型共用一次保存事务；保存本机目录与同步到 Work 是不同操作，新增/删除模型后按同步状态重新导入，必要时正常重启 Kimi。

- 用量页包含真实请求、Kimi Code 三来源、Magpie 余额和预算。Work 来源随当前官方凭据重新读取。本机 Kimi Code 登录只有显式采用后才使用，手填 Key 保存在本产品的加密凭据存储中。读取失败保留错误和过期标记，不能解释为额度为 0。
- 账号库把可恢复会话保存在本产品的加密凭据存储（macOS 应用内 AES-GCM，Windows DPAPI），窗口顶部、菜单栏和设置共用后台状态。先捕获当前最新凭据，再通过官方 TokenStore 完整写入目标 Cookie 和令牌，优先由官方协调器切换账号与内核。免桌面重启成功要求切换前安装的监听观察到非就绪态，再读回协调器就绪、内核只读模型查询成功、官方 Work shell 和配置身份一致。成功时保留官方轮换后的令牌与 Code Key。内核核验总等待最多 12 秒，无法核实时最多正常重启桌面一次；写入不完整不重启，失败尝试恢复原账号，上层不会重复自动切换。任务在跑或忙状态未知时拒绝切换。开发候选已完成两真实账号五次往返、额度跟随及添加/取消恢复，全程桌面 PID 不变。最终产物与 GUI 验收状态以工作区交接记录为准。
- Swarm 通过当前官方会话的 `app.swarmMode.get/set` 接口控制，不直接修改会话文件。当前内核的 Agent/AgentSwarm 共享 `secondary_model`；没有独立 Swarm 模型池。Tower 在当前官方桌面版本没有入口，显示不支持，不通过注入伪造入口。
- 预算依赖模型定价和本机账本估算，按所选时区计算日/月提醒或拒绝。删除模型定价不清除已经记录的同币种历史费用，不同币种不相加。可能产生费用但缺少价格或用量的记录列为未计价。网关在转发前明确拒绝的请求不增加未计价数，在途请求单独统计。供应商的真实账单与硬限额不由它保证。可选串行模式在上一笔入账后再判断下一笔，最后一笔已放行请求仍可能越过上限。达到上限后，Work+ 跨页提示当前预算状态，可跳到用量页查看预算。
- 备用连接按配置顺序尝试。只在拨号失败或指定限流/服务错误状态且尚未转发响应时切换，不在流开始后重发。账本归属实际回答的连接。主凭据不可读属于配置错误，不静默使用备用连接掩盖。

候选构建使用单独输出目录并设置 `BUILD_ID`。验收时核对桌面、daemon 的 buildinfo 与 `/api/health` 的 buildId/PID，未提交工作区不能冒充干净提交构建。v1.1.2 起 macOS 已迁移凭据不再受签名 ACL 影响，但升级验收仍必须核对连接 `apiKeySet`、真实模型请求以及全过程没有钥匙串授权，不能只看健康接口。旧版本首次升级优先后台迁移；旧条目需确认时，模型页的「重新授权并应用连接」沿用原引用完成一次可见迁移。新值读回失败会清理未验证的本地值并保留旧条目。预览迁移只读旧 Keychain，正式存储验证完成前不得删除预览凭据。本地主密钥或密文异常不得回退到旧值、静默重建主密钥或报告成未配置。

当前已有三来源额度真实查询、预算提醒/拒绝与真实备用模型账本证据，以及预算界面保存/清除。2026-11-17 的连接授权候选已在官方界面选择 DeepSeek、GLM 并取得真实回复；Swarm 启用后两个并行助手完成任务，当前网关账本记录对应请求，结束后开关恢复关闭。e6d2ac1 最终包已完成两真实账号五次往返、添加/取消、托盘切换、双账号各 DeepSeek/GLM 实际回复和 Work 额度跟随。本轮继续验收前端备用行、预算和凭据补修，最终运行包及证据以工作区交接记录为准。Windows 真机与长期令牌寿命尚未验证。未提交候选的证据不能代替提交绑定包的验收。

## 测试和边界

协议、账本、额度、Tavily 测试包含旧实现的冻结观察值，不需要 Node/Bun 即可运行这些对照测试。`DUAL_RUN_REGEN=1` 是开发者重新执行旧 JS 的可选模式，需要原 0.12.1 源码和 Bun；独立回传源码没有捆绑完整旧仓库，普通测试不依赖它。

`internal/kimi/INTERFACE.md` 记录原生契约、当前默认 profile 与未支持能力。阶段 1 已在真实官方账号和 Work 数据上验收模型选择、DeepSeek/GLM 回复、固定/继承子代理及 MCP 工具往返；账本记录真实 Token、耗时和 `conversationId`。真实请求没有可靠的主对话/子代理角色字段，不能据此声称账本已区分调用方。MCP 面板内的连接探测仍需要 kernel runtime；remote companion/updater、旧 safeStorage 解密、Windows 真机和生产级共存仍有缺口。

平台边界按实际验证分级记录，不互相顶替：

| 能力 | 状态 |
| --- | --- |
| Mac 模型链路、子代理、MCP、账号切换、预算、Swarm、3.2.28 兼容自检、自动更新与回滚 | 真机已验证（见工作区交接记录） |
| Windows 3.2.28 历史基线（`4ddaa3b`） | 已真机验证受管窗口、双账号切换与模型读回 |
| Windows 1.0.0 全链路（安装/卸载、预览升级迁移、三主题外观、双 exe 自动更新/回滚、3.2.28 兼容自检与双账号实测） | 真机已验证（见工作区交接记录） |
| 登录时启动 | 注册/启停与读回已验证；真实注销再登录后是否自启未测 |
| 长期闲置令牌寿命 | 未验证 |
| 零网关直连模式（1.1.0） | 新增功能；真机验收状态以工作区交接记录与发布说明为准，表中其他行的结论不适用于它 |

系统直接启动的官方 Kimi 不受本产品托管。此时已同步的模型配置仍保留在磁盘，但面板不会声称受管进程已加载配置代次，需点「正常退出并重启 Kimi」恢复完整受管能力。

前端使用原生 HTML/CSS/JavaScript（按页面拆分模块，无第三方前端依赖与打包步骤）。翻译注入脚本仍在官方 Kimi 的渲染上下文运行。正式包不携带 Node、Electron 或 Chromium。

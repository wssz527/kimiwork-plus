# 参与贡献（Contributing）

感谢你对 KimiWork + 的关注与支持。提交 Issue、代码改动或文档改进前，请先阅读本说明中的许可条款与工程边界。

## 开源许可

本项目采用 [MIT License](LICENSE) 开源。

在你向本仓库提交 Pull Request、补丁、文档或其他形式的贡献时，即表示你确认对该贡献拥有合法授权、不侵犯第三方权利，并同意按本仓库的 **MIT License** 向项目和公众授权。

## 产品与工程边界

- **不修改官方程序包体**：所有接入只通过官方配置文件、官方页面接口、受管 CDP 管道与网络协议完成，不修改、不重签、不替换官方 Kimi 安装包，也不代理或拦截官方 Kimi 自身的更新。
- **不提交敏感数据**：严禁将签名私钥（包括自动更新 Ed25519 私钥和代码签名 `.p12`/`.pem`）、API Key、Cookie、令牌、个人配置备份或包含真实账号信息的截图提交进仓库。
- **扩展与主链路隔离**：页面增强（如翻译）必须保持可单独关闭、单独失效，不得影响网关转发或官方 Kimi 启动。
- **官方变化保护**：涉及官方 Kimi 契约或存储格式（`profiles` / `accounts`）的改动，需同步更新只读兼容自检（`internal/kimi/compatibility.go`）与相应单测。
- **第三方依赖许可**：新增 Go 或前端依赖前须确认其许可证为宽松许可（MIT / BSD / ISC / Apache-2.0），并同步更新 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)；不要引入 GPL/AGPL 等强传染性依赖。

## 开发环境与提交前检查

本项目使用 Go 1.26.10（见 `go.mod`）与 Wails v3.1.0，前端采用原生 HTML/CSS/JavaScript，无额外 Node 构建步骤（前端单测使用 Node 内置测试运行器）。详细构建说明见 [BUILDING.md](BUILDING.md)。

提交 Pull Request 前，请在仓库根目录执行以下检查并确保全部通过：

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
node --test frontend/app.test.mjs frontend/plus.test.mjs frontend/usage.test.mjs
```

如果改动了 `build/` 下的构建脚本，请同时检查脚本语法（`bash -n build/*.sh`）并在对应平台实测构建产物。

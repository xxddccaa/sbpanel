# System Architecture — sbpanel

## 1. 场景与目标
`sbpanel` 面向无桌面环境的 Linux 服务器、容器与开发机（如云主机、DSW），通过单文件 Go 二进制提供浏览器面板与 CLI 工具，用于管理 `sing-box` 订阅、节点切换/测速、绕过大陆分流、内核升级以及面板自升级。

## 2. 系统架构与进程模型

```text
+-------------------------------------------------------------------+
| CLI / Service Layer (main.go, install.go, service.go, manage.go)  |
|   systemd unit (sandbox -> plain)  OR  direct detached process    |
+---------------------------------+---------------------------------+
                                  | launches `sbpanel serve`
                                  v
+-------------------------------------------------------------------+
| Supervisor Process (`sbpanel serve`, panel.go)                    |
|   - picks newer binary: <data>/panel/sbpanel vs system binary     |
|   - spawns worker with fd 3 ready pipe + Pdeathsig(SIGTERM)       |
|   - trial mode (pending marker, 90s timeout) -> auto revert       |
|   - restarts worker on exit code 75                               |
+---------------------------------+---------------------------------+
                                  | forks with SBPANEL_PANEL_WORKER=1
                                  v
+-------------------------------------------------------------------+
| Panel Worker Process (`main.go:runServe`)                         |
|   - Flock(<data>/.lock) & killStaleCores()                        |
|   - HTTP Server (server.go + embed web/)                          |
|       checkHost -> securityHeaders -> protectRequest -> handlers  |
|   - App Orchestrator (app.go, serialized by applyMu)              |
|       Store (store.go) | Config (config.go) | Rules (rules.go)    |
|       Sub (subscription.go) | Transfer (transfer.go)              |
|       Kernel (kernel.go) | PanelUpdater (panel.go)                |
|   - Core Supervisor (core.go)                                     |
|       manages child `sing-box run`, ring log buffer, Clash API    |
+---------------------------------+---------------------------------+
                                  | spawns child (Setpgid=true)
                                  v
+-------------------------------------------------------------------+
| sing-box Process                                                  |
|   - mixed inbound (HTTP + SOCKS5, default 127.0.0.1:10826)        |
|   - loopback Clash API (random free port + random 24-char secret) |
+-------------------------------------------------------------------+
```

## 3. 目录与文件职责表
仓库当前采用扁平单包结构（根目录 `package main`）加嵌入式前端 `web/`，每个文件对应单一职责，后续增量开发必须按职责归位，禁止把不相关逻辑塞入已有大文件。

| 路径 | 职责边界 | 说明 / 演进约束 |
|------|----------|-----------------|
| `main.go` | CLI 路由、`serve` Worker 装配与优雅停机、数据目录 `Flock` | 不写业务逻辑，仅做依赖注入与生命周期串联 |
| `app.go` | 核心业务编排（订阅 CRUD、选节点、测速、设置、事务 `mutate`、定时调度、`View` 聚合） | 已达 ~800 行；新增独立能力（如连接监控、规则编辑）应拆新文件 |
| `store.go` | 内存状态快照与 `state.json` 原子落盘、历史字段迁移 | 唯一负责状态持久化与 `writeFileAtomic` |
| `config.go` | `sing-box` 配置生成、唯一 tag 分配、依赖剔除、`sing-box check` 逐节点校验 | 与协议解析解耦，只消费 `State` 与 `Node.Raw` |
| `subscription.go` | 订阅拉取、Base64/sing-box JSON 识别解析与节点提取 | 未来新增分享链接 / Clash YAML 解析应拆独立解析器文件 |
| `rules.go` | 绕过大陆 `.srs` 规则集下载、魔数校验、`sing-box check` 验证与原子替换 | 仅处理规则集生命周期 |
| `core.go` | `sing-box` 子进程托管、崩溃指数退避重启、500 行环形日志、Clash API 客户端 | 所有与 `sing-box` 运行时通信统一收口于此 |
| `kernel.go` | `sing-box` 内核版本检测、GitHub 下载与 SHA-256 校验、预检切换与回退 | 负责 `<data>/kernel/` 管理 |
| `panel.go` | Supervisor-Worker 守护协议、网页自升级检查（无 GitHub API）、试运行与自动回退 | 环境变量、fd 3、退出码 75 为跨版本兼容协议，不可破坏 |
| `panel_linux.go` / `panel_other.go` | 平台差异隔离（Linux `Pdeathsig`） | 仅放平台特定系统调用 |
| `server.go` | HTTP 路由、`//go:embed web` 托管、`checkHost` / CSP / CSRF 防护与 JSON 编解码 | 不承载业务规则，全部委托给 `App` |
| `transfer.go` | 通用 HTTP 下载（代理优先再直连）、瞬态错误重试、URL 脱敏与读取上限 | 复用统一传输策略 |
| `random.go` | `crypto/rand` 随机密码/ID 生成 | 纯工具函数 |
| `service.go` | 安装路径布局（root/user）、`systemd` vs `direct` 服务管理器、Linux `/proc` 进程识别与清理 | 已达 ~800 行；平台/进程检测逻辑高度集中于此 |
| `install.go` | `install` / `update` / `uninstall` 实现、systemd 沙箱降级、CLI 下载与 Semver 比较 | 仅处理安装维护生命周期 |
| `manage.go` | 终端交互菜单、`status` / `start` / `stop` / `logs` / `listen`、终端着色输出 | 仅处理 CLI 管理交互 |
| `web/` | 零构建原生前端（`index.html`、`style.css`、`app.js`） | 通过 DOM helper `h()` 渲染，严禁 `innerHTML` |
| `install.sh` | 一键安装 Shell 脚本（架构识别、`noexec` 临时目录探测、SHA-256 校验） | 需保持 `shellcheck` 通过 |
| `scripts/release-notes.sh` | 从 `CHANGELOG.md` 提取对应版本发布说明 | CI Release 专用 |
| `*_test.go` | 单元测试（`sbpanel_test.go`、`features_test.go`、`panel_test.go`、`manage_test.go`、`service_linux_test.go`） | 覆盖配置生成、回滚、安全头、重试、Supervisor/Worker 与 `/proc` |
| `.github/workflows/` | CI（lint/test/shellcheck/dist）与 Tag 自动 Release 流水线 | 发布 4 个 Linux 架构包及 `sha256sums.txt` |

## 4. 关键运行链路
1. **状态变更事务（`App.mutate`）**：获取 `applyMu` -> 快照旧 `State` 与旧 `config.json` -> `Store.Update` 原子写入新状态 -> `buildChecked`（循环 `sing-box check` 剔除不兼容节点）-> 原子写 `config.json` -> 若运行中且配置有变化则重启 `Core` -> 若启动失败则恢复旧状态与旧配置并重启旧 `Core`。
2. **下载回退链路（`App.fetchWithFallback`）**：若代理运行中先经本地混合代理尝试（最多 3 次瞬态重试），失败后再直连尝试（最多 3 次瞬态重试）；订阅更新额外支持 `auto` / `proxy` / `direct` 三种策略。
3. **Web 自升级链路**：Worker 下载并验证 `sha256sums.txt` -> 解压验证 `sbpanel version` -> 写 `<data>/panel/sbpanel` 与 `pending` 标记 -> 退出码 `75` -> Supervisor 启动新版本并等待 90s 内 fd 3 收到 `"ready\n"` -> 成功则删除 `pending`，失败/超时则恢复 `sbpanel.previous` 并写 `last-error`。

## 5. 当前已识别的结构与工程风险
1. **单包膨胀趋势**：根目录 `package main` 已含 17 个源码文件（约 6,700 行，多个文件接近 800 行），后续实施 `ROADMAP.md` 中的多格式订阅解析、连接监控与访问控制前，建议先规划文件/子包边界。
2. **Clash API 连接池重复创建**：`core.go:clashRequest` 每次调用都新建 `&http.Client{Transport: &http.Transport{...}}` 且未复用或释放，高频轮询（`/api/state`）与并发测速（`TestDelays`）下存在空闲连接与内存堆积风险。
3. **`sing-box` 内核升级仍依赖 GitHub API**：`kernel.go` 与 `install.go` 查 `SagerNet/sing-box` 版本仍请求 `api.github.com`，在共享出口 IP（如云服务器、DSW）上可能遭遇 60 次/小时匿名 HTTP 403 限流（`sbpanel` 自身已在 v0.3.1 规避）。
4. **后台内核升级未绑定退出上下文**：`StartKernelInstall` 使用 `context.Background()` 而非 `App` 生命周期上下文，面板关闭时不会主动取消正在进行的内核安装。
5. **免密与明文敏感配置暴露面**：默认 `0.0.0.0:8091` 免密访问，`/api/state` 与 `/api/config` 明文返回订阅 URL、节点密钥、代理密码和 `clash_api.secret`（已列入 `ROADMAP.md` 安全优先级）。
6. **`App.View` 浅拷贝 `a.excluded`**：`app.go:710` 直接持有 `a.excluded` map 引用；当前 `apply` 每次整块替换 map 引用尚无竞态，但后续维护若改为原地修改该 map 将引发并发 panic。

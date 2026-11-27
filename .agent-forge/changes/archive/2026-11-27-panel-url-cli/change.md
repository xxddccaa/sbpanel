# Change: Panel URL Display & `url` / `open` CLI Subcommands

- **级别**：L1（轻量增量功能）
- **日期**：2026-11-27

## 1. 改动目标与不做范围
### 改动目标
解决用户忘记面板端口时“敲命令就能立刻看到或打开面板 URL”的痛点，保持无参数 `sbpanel` 进管理菜单的原有习惯：
1. 补全 `sbpanel` 交互菜单顶部与 `sbpanel status`（手动运行分支）的面板 URL 展示：列出全部可用 URL 并高亮；支持识别未 `install` 但手动运行中的 `sbpanel serve`；监听环回地址时附带可复制的 `ssh -L` 提示。
2. 新增 `sbpanel url` 子命令：向 `stdout` 逐行输出当前全部面板 URL 后立刻退出（不进入交互菜单）；未运行或环回提示写到 `stderr` 不污染管道输出。
3. 新增 `sbpanel open` 子命令：在有图形桌面的环境调用系统浏览器打开面板；无桌面/SSH 环境退化为直接打印面板 URL 与访问提示（退出码 0）。

### 不做范围
- 不改变无参数 `sbpanel` 进入交互菜单的默认行为。
- 不引入 Local Daemon / Unix Socket / Named Pipe 新架构，不改变 `sbpanel serve` 监听默认值（`0.0.0.0:8091`）与现有安装/升级流程。
- 不引入任何第三方依赖。

## 2. 行为变化与边界情况
- **目标解析优先级**（供 `url`、`open`、菜单及 `status` 复用）：
  1. 若当前布局已安装（`l.installed()`）：使用 `l.panelListen()`，运行状态取 `managerFor(l).Status().Running`。
  2. 若未安装：优先匹配 `findRunningPanel(l.DataDir)`；若为空且本机当前用户恰好运行 1 个手动 `sbpanel serve` 进程，则兜底展示该进程地址。
  3. 既未安装也无运行进程：`url` / `open` 返回清晰中文错误引导先安装或启动；菜单保持原有“还没有安装”提示。
- **环回地址隧道提示**：监听 IP 为环回地址（`127.0.0.1`、`::1`）时，输出 `ssh -L <port>:<host>:<port> 用户@服务器`。
- **`sbpanel url` 输出边界**：`stdout` 仅输出纯 URL 行（无 ANSI 转义码）；若服务已安装但未运行，在 `stderr` 打印提示，仍输出配置的 URL。
- **`sbpanel open` 桌面探测**：
  - `darwin`：查找 `open`；
  - `linux`：仅当 `DISPLAY` 或 `WAYLAND_DISPLAY` 非空时查找 `xdg-open`；
  - 服务未运行时报错提示先 `sbpanel start`；无可用桌面打开器时打印全部 URL 到 `stdout` 并在 `stderr` 给出手动打开提示，正常返回 `nil`。

## 3. 验证任务清单
- [x] 在 `manage_test.go` 补充单元测试覆盖：面板目标解析（已安装/未安装+手动运行/未安装无进程）、`sshTunnelHint`、`browserCommand`（macOS / Linux 有桌面 / Linux 无桌面 / 无命令）、`writeMenuPanelURLs` 与 `writePanelURLs` 输出行为。
- [x] 在 `main.go`、`manage.go` 实现改动，更新 `usage` 与 `README.md` 子命令说明。
- [x] 运行 `make lint`、`make build` 及全量 `go test ./...` 验证全部通过，复核 `git diff`。
- [x] 同步 `.agent-forge/specs/domains/install-and-cli.md`，归档本变更文档到 `changes/archive/` 并更新 `.agent-forge/STATE.md`。

# Domain: Kernel & Panel Self-Update

覆盖文件：`kernel.go`、`panel.go`、`panel_linux.go`、`panel_other.go`

## 1. `sing-box` 内核管理（`Kernel`）
- **优先级**：`<data>/kernel/sing-box`（面板托管）优先于系统 `sing-box`（与 `sbpanel` 同目录或 `PATH`），无需写 `/usr/local/bin`。
- **版本缓存**：按文件 `ModTime` + `Size` 缓存 `<binary> version` 输出，避免高频重复 fork。
- **架构与产物**：仅支持 Linux `amd64/arm64/armv7/386`，下载官方 `sing-box-<ver>-linux-<arch>-musl.tar.gz` 静态包。
- **检查与安装流程**：
  - 按需手动触发（`POST /api/kernel/check`），请求 `SagerNet/sing-box` GitHub Releases API（`per_page=15`）提取最新稳定版与（发布时间晚于稳定版的）预发布版。
  - 安装为异步任务（`KernelJob` 汇报阶段与字节进度）：要求 Release Asset 必须提供 `sha256:` digest -> 流式下载并边算边校验 SHA-256 -> 解压验证 `sing-box version` 与目标一致 -> `switchKernel` 事务切换。
- **事务切换与回退（`switchKernel`）**：
  1. 若存在可用节点，先用新二进制执行 `buildChecked` 预检，不通过则拒绝切换。
  2. 停止 `Core` -> 原子重命名旧版为 `sing-box.previous`、新版为 `sing-box` -> `apply` 启动。
  3. 若启动失败，执行 `undo` 恢复旧版并重启；此外面板支持随时点击回退（`RollbackKernel`）或切回系统版（`UseSystemKernel`）。

## 2. `sbpanel` 网页自升级（Supervisor-Worker 架构）
为在 `systemd DynamicUser` + `ProtectSystem=strict` 只读环境下完成自身升级，`sbpanel serve` 拆分为两个进程角色：

### 跨版本稳定协议（不可随意变更）
- 环境变量：`SBPANEL_PANEL_WORKER=1`、`SBPANEL_READY_FD=3`、`SBPANEL_BASE_VERSION=<supervisor版本>`
- 就绪管道：Supervisor 通过 `ExtraFiles` 传入 fd 3，Worker 启动 HTTP 与 `app.Startup` 完成后写入 `"ready\n"` 并关闭（启动时设 `CloseOnExec` 防被 `sing-box` 继承）。
- 重启信号：Worker 主动退出码 `75`（`panelRestartExitCode`），Supervisor 收到后重新按版本优先级拉起 Worker。
- 孤儿防护：Linux 下通过 `Pdeathsig: SIGTERM` 确保 Supervisor 异常被杀时 Worker 自动退出并释放 `.lock`。

### 版本选择、无 API 查询与试运行回退
- **版本优先级**：Supervisor 比较 `<data>/panel/sbpanel` 与自身版本（`compareSemver`），仅当托管版本严格更新时才启动它；若后续管理员用 CLI `sbpanel update` 安装了更高或同版本，自动回归系统二进制。
- **绕开 GitHub API 限流**：通过 `GET /releases/latest` 的 302 重定向目标解析最新 tag，并直接下载该 tag 的 `sha256sums.txt` 获取目标架构包校验值，避免共享出口 IP 命中 API 60 次/小时限制。
- **试运行机制**：`swapIn` 替换 `<data>/panel/sbpanel` 并写入 `pending` 标记 -> 延迟 1.5s 让前端感知“重启中”后退出码 `75` -> Supervisor 在 `trial` 模式下给予 90s 等待 fd 3 `"ready\n"` -> 收到则删 `pending` 转正；超时或提前退出则删除失败版本、恢复 `sbpanel.previous` 并写入 `last-error` 供网页展示。

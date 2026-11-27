# ADR-003: Supervisor-Worker Process Split for Web Self-Update

- **Status**: Active
- **Date**: 2026-11-25 (v0.3.0)

## 背景
在 `systemd` 沙箱模式下，服务以 `DynamicUser=yes` 和 `ProtectSystem=strict` 运行，进程无法覆盖 `/usr/local/bin/sbpanel`，因此单进程无法在网页中完成自升级与故障自动回退。

## 决策
将 `sbpanel serve` 拆分为长期驻留的轻量 Supervisor 与可替换的 Worker 子进程：
1. 网页下载的新版二进制写入可写数据目录 `<data>/panel/sbpanel`；Supervisor 比较版本号后优先拉起更新的版本。
2. Worker 启动完毕后向继承的 fd 3 写入 `"ready\n"`；试运行版本（存在 `pending` 标记）在 90s 内未就绪或异常退出时，Supervisor 自动回退到 `sbpanel.previous`（或自身）并记录 `last-error`。
3. 升级或回退触发切换时，Worker 优雅停机并以退出码 `75` 退出，由 Supervisor 立即重拉新 Worker；Linux 下为 Worker 设置 `Pdeathsig: SIGTERM` 防止孤儿进程占锁。

## 影响与约束
- **不可破坏契约**：Supervisor 进程在服务重启前始终运行旧版本代码，因此 `SBPANEL_PANEL_WORKER`、`SBPANEL_READY_FD`、`SBPANEL_BASE_VERSION`、fd 3 `"ready\n"` 语义与退出码 `75` 必须跨版本永久向后兼容。

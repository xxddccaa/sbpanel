# Domain: Core Runtime & App Orchestration

覆盖文件：`core.go`、`app.go`

## 1. `sing-box` 子进程托管（`Core`）
- **动态解析二进制**：`Core.binary` 为函数回调（指向 `Kernel.Active`），每次重启自动拾取最新切换的内核路径。
- **进程组隔离**：启动时设置 `Setpgid: true`，终端 `Ctrl+C` 仅发给 `sbpanel`，由其统一优雅停止 `sing-box`（`SIGTERM` 等待 10s，超时 `SIGKILL`）；systemd 下由 cgroup 兜底回收。
- **启动就绪判定**：拉起后 15s 内每 200ms 轮询 Clash API `GET /version`，连通后仍需满足 `coreStayUpTime`（1s）存活窗口才判定成功；失败即调用 `Stop()` 取消自动重启标记，交由上层回滚。
- **崩溃自动重启**：运行期间若非主动停止（`wanted == true` 且 `restartGen` 未递增），以 2s 起步、最大 60s 指数退避重启；持续稳定运行超过 30s 后重置退避。
- **日志缓冲**：stdout/stderr 写入线程安全 `lineBuffer`（去除 ANSI 颜色码，保留最近 500 行，单行残片超 64 KiB 丢弃）。
- **Clash API 封装**：`SelectNode`（`PUT /proxies/proxy`）、`Group`（`GET /proxies/{tag}`）、`Delay`（`GET /proxies/{tag}/delay`）；测速接口非 401/404 的 API 错误返回 `-1`（超时/不可达）。

## 2. 业务编排与事务回滚（`App`）
- **锁分层**：
  - `applyMu`：串行化所有改写 `config.json` 或启停 `Core` 的操作（`Startup`、`mutate`、`Select`、`StopCore`、`RestartCore`、`switchKernel`、规则集替换）。
  - `mu`：保护内存运行时缓存（`excluded`、`tags`、`delays`、`updating`、`lastApply`）。
  - `ruleSetMu`：防止规则集并发下载覆盖。
- **事务式变更（`mutate`）**：快照旧 `State` 与旧 `config.json` -> 更新并原子落盘 `State` -> `apply`；若触发 `startFailure`，恢复旧 `State`、回写旧 `config.json` 并重启旧 `Core`，返回带恢复提示的错误。
- **节点热切换（`Select`）**：运行中先经 Clash API 即时切换 `proxy` selector，再更新 `State.Selected` 并以 `restart=false` 重写 `config.json` 的 `default` 字段，不中断现有连接。
- **并发测速（`TestDelays`）**：8 个 worker 并发测速全部可用非 Helper 节点，单节点超时 5s，结果缓存在内存 `delays`（不持久化）。
- **定时调度（`RunScheduler`）**：每分钟巡检订阅；仅对 `IntervalHours > 0` 的订阅生效，基于 `sha256(id|attemptedAt)` 生成稳定 0~25% 延迟抖动；若上次更新失败则退避间隔收敛至最多 1 小时。

## 3. 已知运行期注意点
- 面板收到 `SIGTERM`/`SIGINT` 退出时仅调用 `core.terminate()` 停止子进程，保持 `state.CoreEnabled` 不变，确保下次启动面板自动恢复代理。
- 启动阶段持有 `<data>/.lock` 后先执行 `killStaleCores`，清理上一次异常强杀残留的同配置 `sing-box` 进程。

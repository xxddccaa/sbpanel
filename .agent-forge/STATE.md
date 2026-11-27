# STATE — sbpanel 当前状态快照

- **当前阶段**：已上线维护与演进期（最新标签 `v0.3.2`，基于 `main` 分支）
- **架构状态**：已确认（`.agent-forge/` 随项目提交进 Git，不加入 `.gitignore`）
- **当前运行基线**：`make lint`、`make build` 与全量 `go test ./...` 全部通过

## 正在推进的任务
- （暂无进行中的开发任务）

## 近期候选演进方向（来源：`ROADMAP.md`）
1. **安全加固**：IP 网段访问控制、可选面板密码、配置/日志敏感字段打码
2. **体验增强**：Clash API `/connections` 连接列表与实时网速、日志增量刷新、订阅流量/到期解析与关键词过滤
3. **协议与分流扩展**：分享链接订阅、Clash YAML 订阅、自定义分流规则、TUN 透明代理

## 最近有效关键决策
- **ADR-004**（Active）：`sbpanel` 自身更新改用 GitHub `/releases/latest` 重定向 + `sha256sums.txt`，绕开匿名 API 60 次/小时限流（`v0.3.1`）。
- **ADR-003**（Active）：`sbpanel serve` 采用 Supervisor-Worker 双进程 + fd 3 就绪信号，支持只读系统目录下的网页自升级与试运行自动回退（`v0.3.0`）。
- **ADR-002**（Active）：移除面板密码，以 Host 白名单防 DNS 重绑定 + CSRF 校验作为免密可信网络防护边界（`v0.3.0`）。

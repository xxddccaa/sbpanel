# Domain: Web UI, HTTP API & Security Model

覆盖文件：`server.go`、`web/index.html`、`web/style.css`、`web/app.js`

## 1. HTTP API 契约（`/api/*`）
除静态资源 `GET /`（`Cache-Control: no-cache`）外，全部 API 返回 JSON（`Cache-Control: no-store`），请求体上限 `1 MiB`，写操作与耗时长请求使用 `context.WithoutCancel` + 2 分钟超时，避免浏览器标签页关闭导致配置半写入。

| 分类 | 端点 | 作用 |
|------|------|------|
| 健康与状态 | `GET /api/health`、`GET /api/state` | 探活与版本（CLI/升级用）、聚合面板全量视图 |
| 订阅管理 | `POST /api/subscriptions`、`PUT/DELETE /api/subscriptions/{id}`、`POST /api/subscriptions/{id}/update`、`POST /api/subscriptions/update-all` | 增删改与单条/全量拉取更新 |
| 节点与核心 | `POST /api/select`、`POST /api/core/{start\|stop\|restart}`、`POST /api/delay-test` | 切换节点/自动选择、控制 sing-box、全量测速 |
| 设置与规则 | `PUT /api/settings`、`POST /api/rulesets/update`、`GET /api/config`、`GET /api/logs` | 代理/DNS/分流设置、规则集更新、只读配置查看、日志拉取 |
| 内核管理 | `GET /api/kernel`、`POST /api/kernel/{check\|install\|rollback\|use-system}` | sing-box 版本查询、异步安装、回退、切回系统版 |
| 面板自升级 | `GET /api/panel`、`POST /api/panel/{check\|install\|rollback}` | sbpanel 版本查询、异步自升级与回退 |

## 2. 安全防护边界（免密设计）
自 `v0.3.0` 起面板不设密码，明确限定运行于可信局域网、本机 SSH 隧道或前置认证反代之后，服务端强制执行三层防护：
1. **防 DNS 重绑定（`checkHost`）**：仅放行空 Host、合法 IP、`localhost`/`*.localhost` 以及 `SBPANEL_ALLOWED_HOSTS` 白名单（`*` 关闭）；其他域名一律 `403`。
2. **防跨站请求（`protectRequest`）**：
   - `Sec-Fetch-Site: cross-site` 直接拒绝（`403`）。
   - 若存在 `Origin` 头，必须为 `http`/`https` 且 `Host` 与请求 `Host` 完全一致。
   - 非 `GET`/`HEAD` 请求必须携带自定义头 `X-SBPanel: 1`（触发浏览器预检并阻断简单表单/跨站请求）。
3. **响应安全头与防 XSS（`securityHeaders` + `web/app.js`）**：
   - 下发严格 CSP（`default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'`）、`X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`。
   - 前端所有 DOM 节点经 `h()` 以 `textContent` / `createTextNode` 构造，严禁使用 `innerHTML`，防止订阅节点名携带恶意脚本。

## 3. 前端交互机制（`web/app.js`）
- 零构建原生 JS，4 个标签页（节点、订阅、日志、设置），当前页签持久化于 `localStorage`。
- **轮询策略**：页面可见时每 3 秒拉 `/api/state`（有进行中操作 `ui.pending > 0` 时跳过重绘以防冲掉表单/加载态）；日志页每 2 秒拉 `/api/logs`。
- **升级衔接**：面板自升级/回退时先将目标版本写入 `sessionStorage('sbpanel-switch')`，检测到重启断连后轮询 `/api/health` 最长 180 秒，恢复后自动刷新页面并 toast 汇报切换成功或回退原因。

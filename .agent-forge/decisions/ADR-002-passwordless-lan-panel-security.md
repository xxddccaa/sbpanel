# ADR-002: Passwordless LAN Panel with Host & CSRF Defense

- **Status**: Active
- **Date**: 2026-11-25 (v0.3.0)

## 背景
`v0.1.0`–`v0.2.x` 曾内置面板密码登录与 Cookie 会话，但在典型内网/SSH 隧道使用场景下增加了首次使用摩擦，且明文 HTTP 下的密码登录本身不防链路窃听。

## 决策
自 `v0.3.0` 起移除面板密码、`sbpanel passwd` 与 Cookie 会话，改为清晰的网络信任边界 + 浏览器攻击面防护：
1. 默认免密，明确声明面板为明文 HTTP，仅限可信局域网或本机监听 + SSH 隧道 / 前置认证 HTTPS 反代使用。
2. 服务端增加 `checkHost`（仅允许 IP、`localhost` 与 `SBPANEL_ALLOWED_HOSTS`）抵御 DNS 重绑定攻击。
3. 保留并强化 `protectRequest`（校验 `Sec-Fetch-Site`、`Origin` 与自定义 `X-SBPanel: 1` 头）与严格 CSP 抵御 CSRF/XSS。

## 影响与约束
- **收益**：安装即用，去除 Cookie / 密码状态管理复杂度。
- **风险与演进**：同网段任意可访问面板端口者均可读取明文订阅与配置；后续按 `ROADMAP.md` 演进时可增量叠加可选 CIDR 白名单、可选密码及配置敏感字段打码（若恢复认证机制需新增 ADR 说明）。

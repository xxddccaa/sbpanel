# ADR-004: Avoid GitHub API Rate Limits for sbpanel Release Lookups

- **Status**: Active
- **Date**: 2026-11-25 (v0.3.1)

## 背景
GitHub REST API 对未认证请求限制每 IP 每小时 60 次。云服务器与 DSW 等共享出口 IP 环境下，`sbpanel` 升级检查频繁遭遇 HTTP 403。

## 决策
`sbpanel` 自身的最新版本查询与包校验不再调用 `api.github.com`：
1. 请求 `https://github.com/xxddccaa/sbpanel/releases/latest`，从最终跳转 URL 的 `/releases/tag/<tag>` 提取最新版本号。
2. 直接下载对应 tag 的 `sha256sums.txt` 提取目标架构压缩包的 SHA-256 校验和。
3. 网页检查/升级与 CLI `sbpanel update` 统一复用 `latestSbpanelTag` 和 `sbpanelReleaseAsset`。

## 影响与约束
- **约束**：Release CI（`.github/workflows/release.yml` + `Makefile dist`）必须持续随每个版本上传标准格式的 `sha256sums.txt`。
- **遗留观察点**：`SagerNet/sing-box` 内核更新（`kernel.go`、`install.go`）目前仍依赖 GitHub API（因为需要同时区分 stable/prerelease 与读取 asset digest），若后续共享 IP 场景频繁受限可考虑同类去 API 化改造。

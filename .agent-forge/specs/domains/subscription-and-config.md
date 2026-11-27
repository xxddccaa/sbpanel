# Domain: Subscription, Routing Rules & Config Generation

覆盖文件：`subscription.go`、`config.go`、`rules.go`、`transfer.go`

## 1. 订阅拉取与解析
- **默认 User-Agent**：`sbpanel (sing-box; SFA compatible)`，使机场面板及 `fscarmen/sing-box` 按 UA 返回 sing-box JSON。
- **格式支持**：sing-box JSON（支持 UTF-8 BOM）以及宽松 Base64（Std/URL/Raw 编码、忽略空白字符）包装的 sing-box JSON；遇到 HTML、Clash YAML（含 `proxies:`）、分享链接（含 `://`）返回明确中文提示。
- **节点提取**：
  - 过滤 `direct`、`block`、`dns`、`selector`、`urltest` 非代理 outbound 及同订阅重复 tag。
  - 原样压缩保留 `Raw`（使用 `json.Decoder.UseNumber()` 防止重编码改变大整数/精度）。
  - 标记被同订阅其他节点的 `detour` 引用的节点为 `Helper: true`（前置节点不暴露到可选节点列表）。
- **传输策略与安全**：
  - 响应体上限 `16 MiB`，超时 `30s`，支持 `auto`（先代理后直连）、`proxy`（仅代理，未运行直接报错）、`direct`（仅直连）。
  - 传输错误统一 `stripURL` 剥离 URL，避免订阅密钥进入日志、前端和 `state.json`。
  - 失败时保留旧节点，仅更新 `AttemptedAt` 与 `LastError`。

## 2. 配置生成与逐节点兼容校验
- **稳定 Tag 分配（`assignTags`）**：仅依赖全部订阅节点（不依赖排除列表），重名或撞保留字（`auto`/`proxy`/`direct`）时自动加 `<订阅名> · <tag>` 及 `#N` 后缀；同步重写同订阅节点内的 `detour` 引用，并移除节点原带的 `domain_resolver`（统一走 `route.default_domain_resolver`）。
- **依赖级联排除（`excludeDependents`）**：被排除节点若作为其他节点的 `detour`，依赖者自动级联标记不可用。
- **逐节点预检（`buildChecked`）**：调用 `sing-box check -c <temp>`；若报错指向 `outbounds[N]`，将其加入 `excluded` 并转换为友好提示，循环重试直到通过或无可用节点，避免单个新协议/旧协议节点拖垮整份订阅。
- **生成的 sing-box 结构**：
  - `inbounds`：单一 `mixed` 入站（默认 `127.0.0.1:10826`，可选用户名/密码）。
  - `outbounds`：全部可用节点 + `urltest`（`auto`，10m 间隔）+ `selector`（`proxy`，默认 `selectedTag`）+ `direct`。
  - `dns`：按 sing-box 1.12+ 格式生成 `dns-direct`；当服务器填写域名时自动补充 `local` 类型的 `dns-bootstrap` 解析器。
  - `experimental.clash_api`：监听 `127.0.0.1:<ClashPort>` + Bearer `ClashSecret`。

## 3. 绕过大陆规则集
- 规则文件固定来自官方 `sing-geosite`（`geosite-cn.srs`）与 `sing-geoip`（`geoip-cn.srs`），落盘于 `<data>/rules/`。
- 开启 `BypassChina` 时路由规则顺序：`sniff` -> `ip_is_private` 直连 -> `geosite-cn` 直连 -> `geoip-cn` 直连 -> `final: proxy`；绝不为匹配 `geoip-cn` 而在本地提前解析域名，防止境外域名 DNS 泄露。
- 更新流程：通过 `fetchWithFallback`（含最多 3 次瞬态重试）下载 -> 校验 `SRS` 魔数 -> 写入 `.new` 暂存 -> 构造最小配置经 `sing-box check` 校验 -> 加 `applyMu` 原子替换 -> 若启用绕过大陆且代理运行中则重启 `Core`。

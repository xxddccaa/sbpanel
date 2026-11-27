# Data Model & Storage — sbpanel

## 1. 存储选型与理由
- **主状态存储**：单文件 JSON（`<data>/state.json`，权限 `0600`），由内存 `Store` 持有并通过 `sync.Mutex` 串行化更新。
- **选型理由**：单机单实例面板状态体量小（通常几十 KB 到几百 KB），无需引入 SQLite/外部数据库依赖即可维持 Go 纯标准库单二进制目标；写盘采用 `CreateTemp` -> `Chmod(0600)` -> `Write` -> `Sync` -> `Close` -> `os.Rename` 保证崩溃不产生半截文件。
- **并发与进程互斥**：`<data>/.lock` 使用 `syscall.Flock` 排他非阻塞锁，防止多实例同时操作同一数据目录。

## 2. 磁盘目录布局

### 安装路径（`layout`）
| 维度 | root 系统安装 | 非 root / 只读根文件系统用户安装 |
|------|---------------|----------------------------------|
| 二进制 | `/usr/local/bin/sbpanel`（及同目录 `sing-box`） | `~/.local/bin/sbpanel` |
| 环境配置 | `/etc/sbpanel/sbpanel.env` | `~/.config/sbpanel/sbpanel.env` |
| 数据目录 | `/var/lib/sbpanel`（`DynamicUser` 下指向 `/var/lib/private/sbpanel`） | `~/.local/share/sbpanel` |
| 日志 | `journald` 或 `/var/log/sbpanel/sbpanel.log` | `~/.local/share/sbpanel/sbpanel.log` |
| systemd unit | `/etc/systemd/system/sbpanel.service` | 无 |

### 数据目录（`<data>/`）内部结构
```text
<data>/
├── .lock                 # flock 进程排他锁
├── sbpanel.pid           # direct 后台进程模式 PID 记录（配合 /proc 校验）
├── state.json            # 主持久化状态（0600，原子写入）
├── config.json           # 生成的 sing-box 配置（0600，原子写入，无可用节点时删除）
├── run/                  # sing-box 工作目录
│   └── tmp/              # `sing-box check` 临时校验文件目录
├── rules/                # 绕过大陆二进制规则集
│   ├── geosite-cn.srs
│   └── geoip-cn.srs
├── kernel/               # 面板托管的 sing-box 内核
│   ├── sing-box          # 当前生效内核
│   └── sing-box.previous # 回退备份
└── panel/                # 网页自升级的 sbpanel 二进制与试运行状态
    ├── sbpanel           # 新版二进制
    ├── sbpanel.previous  # 上一面板托管版本
    ├── pending           # 试运行标记（新版本发送 ready 后删除）
    └── last-error        # 最近一次试运行回退原因
```

## 3. 核心持久化数据结构（`store.go`）

### `State`（`state.json` 根对象）
| 字段 | JSON 键 | 类型 | 说明 |
|------|---------|------|------|
| `Settings` | `settings` | `Settings` | 代理、DNS、测速、日志与分流配置 |
| `Subscriptions` | `subscriptions` | `[]*Subscription` | 已添加订阅列表 |
| `Selected` | `selected` | `*NodeRef` | 手动选中节点；`nil` 表示走 `auto`（urltest） |
| `CoreEnabled` | `coreEnabled` | `bool` | 用户期望的代理开关状态（面板重启后自动恢复） |
| `ClashPort` | `clashPort` | `int` | 本机 `127.0.0.1` 随机分配的 Clash API 端口 |
| `ClashSecret` | `clashSecret` | `string` | 随机 24 位 Clash API Bearer 密钥 |
| `RuleSetsUpdatedAt` | `ruleSetsUpdatedAt` | `time.Time` | 绕过大陆规则集上次更新时间 |

### `Settings`
| 字段 | JSON 键 | 默认值 | 约束 |
|------|---------|--------|------|
| `ListenAddr` | `listenAddr` | `"127.0.0.1"` | 合法 IP（如 `127.0.0.1` / `0.0.0.0`） |
| `ListenPort` | `listenPort` | `10826` | `1..65535`，且不能等于 `ClashPort` |
| `ProxyUser` / `ProxyPass` | `proxyUser` / `proxyPass` | `""` | 要么全填，要么全空 |
| `DNSServer` | `dnsServer` | `"local"` | `local`、纯 IP 或 `udp/tcp/tls/https/quic/h3://...` |
| `DNSStrategy` | `dnsStrategy` | `""` | `""` / `prefer_ipv4` / `prefer_ipv6` / `ipv4_only` / `ipv6_only` |
| `TestURL` | `testUrl` | `"https://www.gstatic.com/generate_204"` | `http://` 或 `https://` 合法 URL |
| `LogLevel` | `logLevel` | `"info"` | `trace` / `debug` / `info` / `warn` / `error` / `fatal` / `panic` |
| `BypassChina` | `bypassChina` | `false` | 开启前须确保 `geosite-cn.srs` 与 `geoip-cn.srs` 已就绪 |

### `Subscription`、`Node` 与 `NodeRef`
- **`Subscription`**：`id`（10 位随机小写串）、`name`（全局去重）、`url`（唯一校验，含敏感密钥）、`userAgent`、`intervalHours`（`0..720`，默认 `0` 手动）、`updateVia`（`auto` / `proxy` / `direct`，兼容迁移旧字段 `viaProxy`）、`updatedVia`、`updatedAt`、`attemptedAt`、`lastError`、`nodes`。
- **`Node`**：`tag`、`type`、`server`、`port`、`helper`（若被同订阅其他节点的 `detour` 引用则为 `true`，不可单独选择）、`raw`（原样压缩保存的 sing-box outbound JSON）。
- **`NodeRef`**：`{ subId, tag }`，独立于生成配置中去重后的最终 tag，保证跨订阅重名或部分节点被剔除时选中状态不漂移。

## 4. 环境配置文件（`sbpanel.env`）
`EnvironmentFile` 风格纯文本（`0644`），CLI 修改 `SBPANEL_LISTEN` 时保留用户手动添加的其他键：
- `SBPANEL_LISTEN`：面板监听地址，默认 `0.0.0.0:8091`
- `SBPANEL_DATA`：数据目录覆盖
- `SBPANEL_SING_BOX`：系统 `sing-box` 可执行文件路径覆盖
- `SBPANEL_ALLOWED_HOSTS`：允许访问面板的额外域名（逗号或空白分隔，`*` 关闭检查）
- `SBPANEL_SERVICE_MANAGER`：设为 `direct` 时强制使用后台进程模式

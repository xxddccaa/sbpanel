# sbpanel

[![CI](https://github.com/xxddccaa/sbpanel/actions/workflows/ci.yml/badge.svg)](https://github.com/xxddccaa/sbpanel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/xxddccaa/sbpanel)](https://github.com/xxddccaa/sbpanel/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

在**没有桌面的 Linux 服务器**上，用浏览器管理 [sing-box](https://github.com/SagerNet/sing-box) 订阅。

v2rayN 这类客户端需要图形界面。sbpanel 把它们最常用的几项操作做成了网页：添加订阅、选节点、测速、绕过大陆、启动和停止代理。装在服务器上以后，用自己电脑或手机的浏览器操作。

<p align="center">
  <img src="docs/screenshots/nodes.png" width="62%" alt="节点页">
  <img src="docs/screenshots/mobile.png" width="26%" alt="手机上的节点页">
</p>

- **一个可执行文件**：Go 编写，只用标准库，网页打包在程序里，没有其他依赖
- **一行命令安装**：自动注册 systemd 服务、开机自启，没装 sing-box 的话顺便装上
- **终端里也能管**：`sbpanel` 打开管理菜单，可以查看状态、启停服务、看日志、改密码、更新、卸载

## 安装

用 root 运行：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/xxddccaa/sbpanel/main/install.sh)
```

安装完会打印面板地址和登录密码：

```
✓ sbpanel v0.1.0 已安装并启动，已开启开机自启
  网页面板  http://192.168.1.10:8091
  登录密码  Xk7mP2qRtW9a  （请记下来；忘了运行 sbpanel passwd 重设）
  管理命令  sbpanel（打开管理菜单）
```

- 支持 x86_64、arm64、armv7、i386，需要 systemd。
- 服务器访问不了 GitHub 时，可以加一个加速前缀：`GH_PROXY=https://ghfast.top/ bash <(curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/xxddccaa/sbpanel/main/install.sh)`。也可以设置 `HTTPS_PROXY`。
- 换面板端口：在命令末尾加 `--port 8092`。只允许本机访问：加 `--listen 127.0.0.1:8091`。
- 下载的文件都会按 `sha256sums.txt` 校验。sing-box 安装包按 GitHub 提供的 SHA-256 校验。

## 使用

1. 浏览器打开安装时打印的面板地址，用打印出来的密码登录。
2. **订阅** → **添加订阅**，粘贴订阅地址。
3. 点 **启动代理**。
4. **节点** 页：点一下切换节点，或者选“自动选择”，也可以全部测速。
5. 让服务器上的程序走代理。状态卡片上的“复制终端命令”会生成这一行：

   ```bash
   export http_proxy=http://127.0.0.1:7890 https_proxy=http://127.0.0.1:7890 all_proxy=socks5h://127.0.0.1:7890
   ```

代理端口默认 `127.0.0.1:7890`，同时支持 HTTP 和 SOCKS5。在“设置”里可以改端口，也可以开放给局域网，开放时建议设置用户名和密码。

### 绕过大陆

在“设置”里勾上 **绕过大陆**：国内域名（geosite-cn）和国内 IP（geoip-cn）直连，其余走代理，和 v2rayN 的同名选项一样。

- 规则文件来自 sing-box 官方的 [sing-geosite](https://github.com/SagerNet/sing-geosite) 和 [sing-geoip](https://github.com/SagerNet/sing-geoip)，总共约 100 KB。第一次打开开关时下载，之后只在你点“更新规则文件”时更新。最好先启动代理再打开这个开关，这样规则文件可以通过代理下载。
- 判断一个域名是不是国内的，靠的是域名规则，不会先在本地把域名解析一遍，所以国外域名不会泄露给本地 DNS。
- 打开后，`www.gstatic.com` 这类在国内也有服务的域名会走直连。想测试代理是否可用，请换成 `https://cp.cloudflare.com/` 之类的地址。

### 订阅更新

- **默认只手动更新**。按固定时间拉订阅是一种可以被识别的规律，而且订阅地址里带着账号密钥。
- 可以给单个订阅打开自动更新。实际时间会在设定间隔的基础上随机推迟 0～25%。
- 更新时优先走已经启动的代理（请求加密），代理不通再直连。也可以设成“只走代理”或“只直连”。
- 更新失败时保留原来的节点。错误信息里不会出现订阅地址。

### sing-box 内核

“设置 → sing-box 内核”可以检查新版本（只在你点的时候访问 GitHub），一键安装稳定版或测试版：

1. 下载官方安装包，按 SHA-256 校验
2. 先用新版本检查一遍当前配置，通过了才切换
3. 切换后启动失败，自动换回原来的版本

面板安装的内核放在 `/var/lib/sbpanel/kernel/`，不会改动系统里的 sing-box。随时可以回退到上一个版本，或改回系统自带的版本。

<p align="center"><img src="docs/screenshots/settings.png" width="70%" alt="设置页"></p>

## 终端管理

直接运行 `sbpanel` 会打开管理菜单：

```
  sbpanel 管理菜单  v0.1.0
  ─────────────────────────────────
  服务: ● 运行中（已运行 3 小时，内存 45 MB）   开机自启: 已开启
  面板: http://192.168.1.10:8091

   1. 查看详细状态
   2. 启动              3. 停止              4. 重启
   5. 关闭开机自启
   6. 查看最近日志      7. 持续查看日志（Ctrl+C 返回）
   8. 重设面板密码
   9. 修改面板地址/端口
  10. 更新 sbpanel
  11. 卸载
   0. 退出
```

也可以直接用子命令：

| 命令 | 作用 |
|------|------|
| `sbpanel status` | 服务状态、面板地址、sing-box 版本、当前节点，并实际测一次代理是否可用 |
| `sbpanel start` / `stop` / `restart` | 启动、停止、重启（停止时代理也会一起停） |
| `sbpanel enable` / `disable` | 开启、关闭开机自启 |
| `sbpanel logs [-f]` | 查看日志，`-f` 持续输出 |
| `sbpanel passwd` | 重设面板密码（直接回车随机生成） |
| `sbpanel listen 127.0.0.1:8091` | 修改面板监听地址；只写端口也行，比如 `sbpanel listen 8092` |
| `sbpanel update` | 更新到最新版本 |
| `sbpanel uninstall [--purge]` | 卸载；默认保留订阅和设置，加 `--purge` 一起删除 |

`sbpanel status` 的输出示例：

```
sbpanel v0.1.0
  服务      ● 运行中（已运行 3 小时，内存 45 MB）
  开机自启  已开启
  网页面板  http://192.168.1.10:8091
  sing-box  1.14.4 （系统 /usr/local/bin/sing-box）
  代理      127.0.0.1:7890  HTTP 和 SOCKS5 都行
  绕过大陆  开
  订阅      1 个，共 7 个节点
  当前节点  🇭🇰 香港 01 · Reality（手动选择）
  连通性    ✓ 代理可用（cp.cloudflare.com 312 ms）
```

## 安全

- 面板要用密码登录。密码用 PBKDF2 加盐哈希保存；登录凭证放在 HttpOnly、SameSite=Strict 的 Cookie 里；修改类请求有 CSRF 防护；连续输错 5 次密码锁定 1 分钟。
- 服务用 systemd 的临时用户（`DynamicUser`）运行，不需要 root，也不能改动系统文件。
- **面板走的是明文 HTTP。** 默认监听 `0.0.0.0:8091`，方便在局域网里用。在公网服务器上，建议只监听本机，再用 SSH 隧道访问：

  ```bash
  sbpanel listen 127.0.0.1:8091
  # 在你自己的电脑上：
  ssh -L 8091:127.0.0.1:8091 root@服务器
  # 然后浏览器打开 http://127.0.0.1:8091
  ```

  也可以在前面加一层 HTTPS 反向代理（nginx、Caddy），并在 `/etc/sbpanel/sbpanel.env` 里加上 `SBPANEL_SECURE_COOKIE=1`。

## 目前的限制

- **只支持 sing-box JSON 格式的订阅**，base64 编码过的也可以。
  - [fscarmen/sing-box](https://github.com/fscarmen/sing-box) 脚本的订阅：用 `http://服务器:端口/UUID/sing-box` 这个地址。
  - Clash YAML 和 `vless://` 这类分享链接格式的订阅会提示“暂不支持”，在[计划](ROADMAP.md)里。
- 只提供本地代理（HTTP + SOCKS5），没有 TUN 透明代理。
- 分流只有两种：“内网直连，其余走代理”，或者再加上“绕过大陆”。还不能自定义规则。
- 当前 sing-box 不支持的节点（比如官方版本没有的 snell）会单独标成“不可用”，不影响其他节点。

## 文件位置

| 路径 | 内容 |
|------|------|
| `/usr/local/bin/sbpanel` | 程序 |
| `/etc/sbpanel/sbpanel.env` | 服务设置（面板监听地址等） |
| `/etc/systemd/system/sbpanel.service` | systemd 服务 |
| `/var/lib/sbpanel/` | 订阅、设置、面板密码、生成的 sing-box 配置、规则文件、面板安装的内核 |

## 从源码构建

需要 Go 1.22 或更新版本。

```bash
git clone https://github.com/xxddccaa/sbpanel.git
cd sbpanel
make test        # 测试
make build       # 编译当前平台
make run         # 用 ./dev-data 在 127.0.0.1:8091 运行
make install     # 编译并安装成本机服务（需要 sudo）
make dist        # 交叉编译所有架构，输出到 dist/
```

代码结构：

| 文件 | 内容 |
|------|------|
| `main.go` | 命令行入口、`serve` |
| `manage.go` | 终端管理：状态、启停、日志、菜单 |
| `install.go` | 安装、更新、卸载、systemd 服务 |
| `app.go` | 业务逻辑：订阅、选节点、测速、设置，以及失败回滚 |
| `config.go` | 生成 sing-box 配置，用 `sing-box check` 校验并剔除不支持的节点 |
| `subscription.go` | 下载订阅、识别格式、提取节点 |
| `rules.go` | “绕过大陆”的规则文件 |
| `kernel.go` | sing-box 内核：查版本、下载校验、切换、回退 |
| `core.go` | sing-box 子进程管理、日志缓冲、clash API |
| `auth.go` | 密码哈希、会话、登录限速 |
| `server.go` | HTTP 接口 |
| `store.go` | 状态保存（原子写入） |
| `transfer.go` | 下载工具：先走代理再直连 |
| `web/` | 前端（原生 JS，不需要构建） |

发布：推送 `v*` 标签后，GitHub Actions 会自动交叉编译并发布 Release。

## 许可证

[MIT](LICENSE)

sbpanel 会把 sing-box 作为独立程序调用和下载，但不包含 sing-box 的代码。sing-box 使用 GPL-3.0 许可证。

# Domain: Installation, Service Management & CLI

覆盖文件：`install.sh`、`install.go`、`service.go`、`manage.go`、`main.go`

## 1. 一键安装与多环境自适应
- **脚本入口（`install.sh`）**：检查 Linux 与架构（`amd64/arm64/armv7/386`），探测允许执行的临时目录（规避 `/tmp` 挂载 `noexec`），支持 `GH_PROXY` / `HTTPS_PROXY` / `SBPANEL_VERSION`，下载压缩包与 `sha256sums.txt` 校验后解压执行 `sbpanel install "$@"`。
- **路径与权限降级（`layout`）**：
  - `root` 且系统目录可写：系统安装（`/usr/local/bin`、`/etc/sbpanel`、`/var/lib/sbpanel`）。
  - 非 `root` 或系统目录只读（如只读根文件系统容器）：自动降级用户安装（`~/.local/bin`、`~/.config/sbpanel`、`~/.local/share/sbpanel`）。
- **服务管理器三段降级（`startInstalled`）**：
  1. `root` 且 `systemdUsable()`（验证 `/run/systemd/system` 且 PID 1 `/proc/1/comm == systemd`）：先尝试沙箱化 `systemd` unit（`DynamicUser=yes`、`ProtectSystem=strict` 等）。
  2. 若 `systemd` 自身启动报退出码 `>= 200`（旧内核/嵌套容器不支持沙箱）：自动降级为普通 `systemd` unit（`SBPANEL_DATA=/var/lib/sbpanel`）。
  3. 无 `systemd` 或 `systemd` 启动失败（或设置 `SBPANEL_SERVICE_MANAGER=direct`）：自动降级为 `directManager` 独立会话后台进程（`Setsid: true`，日志轮转上限 10 MiB）。
- **进程接管与权限对齐**：
  - 安装/重装时扫描 Linux `/proc` 识别同数据目录的手动运行进程（如 `nohup sbpanel serve &`），保留原监听端口并平滑接管；若是不同数据目录的另一实例占用端口，给出迁移或换端口提示。
  - 从后台进程切换回 `systemd` 时调用 `alignOwnership` 对齐数据目录属主，避免 `DynamicUser` 无权读取 `root` 留下的文件。
- **自动补装 `sing-box`**：若未检测到可用内核且未传 `--skip-sing-box`，自动下载校验并安装官方最新稳定版到 `sbpanel` 同目录，写 `.sing-box-installed-by-sbpanel` 标记（卸载时仅删除由自己安装的 `sing-box`）。

## 2. CLI 子命令与终端管理菜单
- 无参数且标准输入为 TTY 时进入中文交互菜单（`runMenu`），顶部加粗高亮列出全部面板 URL（已安装取配置地址，未安装但检测到手动 `sbpanel serve` 时同样展示 PID 与地址；监听环回地址时附 `ssh -L` 隧道提示）；非 TTY 打印帮助。
- 子命令：
  - `install`、`update`（下载新版并交由新版自身执行 `install`，保证新安装逻辑生效）、`uninstall [--purge] [-y]`（默认保留配置与订阅数据）
  - `status`（展示服务、面板版本、全部面板地址、内核、订阅数、当前节点并实测 `cp.cloudflare.com` 代理连通性；未安装但手动运行中也显示 PID 与面板 URL）
  - `url`（向 `stdout` 逐行输出全部纯文本面板 URL 并立刻退出，不进菜单；未运行与环回隧道提示输出到 `stderr` 不污染管道）
  - `open`（在 macOS 或含 `DISPLAY`/`WAYLAND_DISPLAY` 的 Linux 桌面用系统浏览器打开面板；无桌面/SSH 环境退化为打印全部面板 URL 与访问提示并正常退出；服务未运行时提示先 `sbpanel start`）
  - `start`/`stop`/`restart`、`enable`/`disable`、`logs [-f] [-n]`、`listen <addr|port>`、`menu`、`serve`、`version`
- 进程监控（`service.go`）：`directManager` 不盲信 `sbpanel.pid`，结合 `/proc/<pid>/cmdline`、`/proc/<pid>/stat`、`/proc/<pid>/environ`、`/proc/<pid>/statm` 与 `/proc/uptime` 精确识别存活状态、运行时间与整棵进程树（Supervisor + Worker + sing-box）RSS 内存。
